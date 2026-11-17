// Package dispatch is the Spatio-Temporal Dispatch Broker: it wires the fleet
// index, matcher, persistence and metrics together and exposes the ride and
// driver REST API.
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/matching"
	"github.com/hexago/hexago/services/dispatch-go/internal/metrics"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// AgentName identifies this broker in the agent_events audit trail.
const AgentName = "dispatch_broker"

// Errors mapped to HTTP status codes by the handlers.
var (
	ErrValidation = errors.New("validation error")
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
)

// Options configures a Service. Zero values select in-memory defaults.
type Options struct {
	H3Resolution    int       // 0 = geo.DefaultResolution
	Center          geo.Point // city centre (HexGrid projection reference)
	CitySpeedFactor float64   // 0 = matching default
	MaxPickupKm     float64   // 0 = matching default (5 km)
	Store           store.Store
	Telemetry       store.TelemetryStore
	Mirror          fleet.Mirror
	Registry        prometheus.Registerer // nil = metrics not exported to Prometheus
	Logger          *log.Logger
	// Publisher receives an ops agent_event for every dispatch decision (nil = none).
	Publisher stream.Publisher
}

// Service is the dispatch broker.
type Service struct {
	geo       geo.Indexer
	fleet     *fleet.Index
	matcher   *matching.Matcher
	store     store.Store
	telemetry store.TelemetryStore
	rec       *metrics.Recorder
	log       *log.Logger
	now       func() time.Time

	// rideMu serialises ride state transitions (cancel vs. driver status
	// changes). Matching and plain location pings do not take it.
	rideMu sync.Mutex

	publisher stream.Publisher
	obsMu     sync.RWMutex
	observer  func(store.Ride)
}

// New builds a Service.
func New(o Options) (*Service, error) {
	res := o.H3Resolution
	if res == 0 {
		res = geo.DefaultResolution
	}
	if o.Center == (geo.Point{}) {
		o.Center = geo.Point{Lat: 12.9716, Lng: 77.5946}
	}
	g, err := geo.NewIndexer(res, o.Center)
	if err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.Store == nil {
		mem := store.NewMemory()
		o.Store = mem
		if o.Telemetry == nil {
			o.Telemetry = mem
		}
	}
	if o.Telemetry == nil {
		o.Telemetry = store.NewMemory()
	}

	fopts := []fleet.Option{fleet.WithLogger(o.Logger)}
	if o.Mirror != nil {
		fopts = append(fopts, fleet.WithMirror(o.Mirror))
	}
	f := fleet.New(g, fopts...)

	rec := metrics.NewRecorder(o.Registry)
	cfg := matching.DefaultConfig()
	if o.CitySpeedFactor > 0 {
		cfg.CitySpeedFactor = o.CitySpeedFactor
	}
	if o.MaxPickupKm > 0 {
		cfg.MaxPickupKm = o.MaxPickupKm
	}

	s := &Service{
		geo:       g,
		fleet:     f,
		matcher:   matching.New(f, g, cfg, matching.WithObserver(rec)),
		store:     o.Store,
		telemetry: o.Telemetry,
		rec:       rec,
		log:       o.Logger,
		now:       func() time.Time { return time.Now().UTC() },
		publisher: o.Publisher,
	}
	if o.Registry != nil {
		s.registerGauges(o.Registry)
	}
	return s, nil
}

func (s *Service) registerGauges(reg prometheus.Registerer) {
	gauges := []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "dispatch_active_drivers", Help: "Drivers not offline."},
			func() float64 { return float64(s.fleet.Counts().Active) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "dispatch_idle_drivers", Help: "Drivers available for dispatch."},
			func() float64 { return float64(s.fleet.Counts().Idle) }),
	}
	for _, g := range gauges {
		if err := reg.Register(g); err != nil {
			s.log.Printf("WARN dispatch: gauge registration: %v", err)
		}
	}
}

// Fleet returns the live driver index.
func (s *Service) Fleet() *fleet.Index { return s.fleet }

// Geo returns the spatial indexer.
func (s *Service) Geo() geo.Indexer { return s.geo }

// StoreKind names the persistence backend ("postgres" or "memory").
func (s *Service) StoreKind() string { return s.store.Kind() }

// MirrorKind names the fleet mirror ("redis" or "none").
func (s *Service) MirrorKind() string {
	if s.fleet.HasMirror() {
		return "redis"
	}
	return "none"
}

// ---- Rides ----

// CreateRideInput is the POST /v1/rides body.
type CreateRideInput struct {
	RiderID string     `json:"rider_id"`
	Pickup  *geo.Point `json:"pickup"`
	Dropoff *geo.Point `json:"dropoff"`
	Tier    string     `json:"tier"`
}

// Alternative is a runner-up driver in a match.
type Alternative struct {
	DriverID string             `json:"driver_id"`
	Lat      float64            `json:"lat"`
	Lng      float64            `json:"lng"`
	Ring     int                `json:"ring"`
	Score    matching.Breakdown `json:"score"`
}

// MatchView explains how a ride's driver was chosen.
type MatchView struct {
	Driver         fleet.Driver       `json:"driver"`
	Ring           int                `json:"ring"`
	Score          matching.Breakdown `json:"score"`
	Alternatives   []Alternative      `json:"alternatives"`
	Considered     int                `json:"candidates_considered"`
	RingsSearched  int                `json:"rings_searched"`
	SearchRadiusKm float64            `json:"search_radius_km"`
	Tier           string             `json:"tier"`           // tier of the assigned car
	RequestedTier  string             `json:"requested_tier"` // tier the rider asked for
	Upgraded       bool               `json:"upgraded"`       // free upgrade (mini -> sedan -> premium)
	LatencyMs      float64            `json:"latency_ms"`
}

// RideView is the API representation of a ride.
type RideView struct {
	Ride  store.Ride `json:"ride"`
	Match *MatchView `json:"match,omitempty"`
}

func newRideID() string {
	var b [16]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // UUID v4
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// CreateRide validates the request, matches a driver, claims it atomically and
// persists the ride. If no driver is available the ride is stored as an open
// request (status "requested") and Match is nil.
func (s *Service) CreateRide(ctx context.Context, in CreateRideInput) (RideView, error) {
	in.RiderID = strings.TrimSpace(in.RiderID)
	if in.RiderID == "" {
		return RideView{}, fmt.Errorf("%w: rider_id is required", ErrValidation)
	}
	if in.Pickup == nil || in.Dropoff == nil {
		return RideView{}, fmt.Errorf("%w: pickup and dropoff are required", ErrValidation)
	}
	tier, err := fleet.ParseTier(in.Tier)
	if err != nil {
		return RideView{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	req := matching.Request{Pickup: *in.Pickup, Dropoff: *in.Dropoff, Tier: tier}

	res, err := s.matcher.Match(req)
	switch {
	case errors.Is(err, matching.ErrInvalidRequest):
		return RideView{}, fmt.Errorf("%w: %v", ErrValidation, err)
	case err != nil && !errors.Is(err, matching.ErrNoCandidates):
		return RideView{}, err
	}

	now := s.now()
	pickupCell, _ := s.geo.CellOf(req.Pickup)
	tripKm := geo.HaversineKm(req.Pickup, req.Dropoff) * s.matcher.Config().RoadFactor
	ride := store.Ride{
		ID:         newRideID(),
		RiderID:    in.RiderID,
		Tier:       string(tier),
		Status:     store.RideRequested,
		Pickup:     req.Pickup,
		Dropoff:    req.Dropoff,
		PickupH3:   pickupCell.String(),
		DistanceKm: tripKm,
		Legs:       []store.RideLeg{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	var view *MatchView
	if err == nil {
		res, view = s.claimBest(&ride, res, tripKm)
	}

	if err := s.store.CreateRide(ctx, ride); err != nil {
		if view != nil {
			s.release(ride.DriverID)
		}
		return RideView{}, fmt.Errorf("persist ride: %w", err)
	}
	if view != nil {
		s.rec.IncMatched()
	} else {
		s.rec.IncUnmatched()
	}
	s.recordMatchEvent(ctx, ride, req, view, res, "")
	if view != nil {
		s.notify(ride)
	}
	return RideView{Ride: ride, Match: view}, nil
}

// claimBest claims the best driver still idle; another request may have taken
// it between scoring and now, so it falls through the ranked list. On success
// ride is assigned and the returned view is non-nil.
func (s *Service) claimBest(ride *store.Ride, res matching.Result, tripKm float64) (matching.Result, *MatchView) {
	for i, c := range res.Ranked {
		claimed, err := s.fleet.Claim(c.Driver.ID, fleet.StatusIdle, fleet.StatusEnRoute)
		if err != nil {
			continue
		}
		res = res.WithBest(i)
		res.Best.Driver = claimed
		s.assign(ride, res.Best, tripKm)
		return res, buildMatchView(res)
	}
	return res, nil
}

// RetryRide re-runs dispatch for an open request (status "requested"), e.g.
// once drivers have freed up. A retry that still finds nobody changes nothing
// and publishes nothing, so periodic retries never flood the ops feed. Legs
// are immutable in the store, so a retried ride returns them in the view only.
func (s *Service) RetryRide(ctx context.Context, id string) (RideView, error) {
	r, err := s.store.GetRide(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return RideView{}, fmt.Errorf("%w: ride %s", ErrNotFound, id)
	}
	if err != nil {
		return RideView{}, err
	}
	if r.Status != store.RideRequested {
		return RideView{}, fmt.Errorf("%w: ride is %s, not %s", ErrConflict, r.Status, store.RideRequested)
	}
	req := matching.Request{Pickup: r.Pickup, Dropoff: r.Dropoff, Tier: fleet.Tier(r.Tier)}
	res, err := s.matcher.Match(req)
	if errors.Is(err, matching.ErrNoCandidates) {
		return RideView{Ride: r}, nil
	}
	if err != nil {
		return RideView{}, err
	}

	s.rideMu.Lock()
	cur, err := s.store.GetRide(ctx, id) // the rider may have cancelled meanwhile
	if err != nil || cur.Status != store.RideRequested {
		s.rideMu.Unlock()
		if err == nil {
			return RideView{Ride: cur}, fmt.Errorf("%w: ride is %s", ErrConflict, cur.Status)
		}
		return RideView{}, err
	}
	res, view := s.claimBest(&cur, res, cur.DistanceKm)
	if view == nil {
		s.rideMu.Unlock()
		return RideView{Ride: cur}, nil
	}
	cur.UpdatedAt = s.now()
	if err := s.store.UpdateRide(ctx, cur); err != nil {
		s.rideMu.Unlock()
		s.release(cur.DriverID)
		return RideView{}, fmt.Errorf("persist ride: %w", err)
	}
	s.rideMu.Unlock()

	s.rec.IncMatched()
	wait := cur.UpdatedAt.Sub(cur.CreatedAt).Round(time.Second)
	s.recordMatchEvent(ctx, cur, req, view, res, fmt.Sprintf("re-dispatched after %s waiting", wait))
	s.notify(cur)
	return RideView{Ride: cur, Match: view}, nil
}

// SetRideObserver registers fn to be called (synchronously; it must not block
// or call back into rides) whenever a ride is matched or cancelled.
func (s *Service) SetRideObserver(fn func(store.Ride)) {
	s.obsMu.Lock()
	s.observer = fn
	s.obsMu.Unlock()
}

func (s *Service) notify(r store.Ride) {
	s.obsMu.RLock()
	fn := s.observer
	s.obsMu.RUnlock()
	if fn != nil {
		fn(r)
	}
}

func (s *Service) publish(ev stream.AgentEvent) {
	if s.publisher != nil {
		s.publisher.Publish(stream.TopicOps, ev)
	}
}

// AgentEventInput is an agent decision to audit and stream.
type AgentEventInput struct {
	Agent        string
	EventType    string
	RideID       string
	Input        any
	Decision     string
	Reasoning    string
	Confidence   float64
	LatencyMs    float64
	UsedFallback bool
	// Optional stream metadata (see stream.AgentEvent).
	Kind     string
	DriverID string
	Tier     string
}

// RecordAgentEvent writes e to agent_events and publishes it on the ops topic.
func (s *Service) RecordAgentEvent(ctx context.Context, e AgentEventInput) {
	now := s.now()
	input, _ := json.Marshal(e.Input)
	if e.Input == nil {
		input = nil
	}
	decision, _ := json.Marshal(map[string]any{"decision": e.Decision, "reasoning": e.Reasoning})
	if err := s.store.RecordEvent(ctx, store.AgentEvent{
		Agent: e.Agent, EventType: e.EventType, RideID: e.RideID, Input: input, Decision: decision,
		Confidence: e.Confidence, FallbackUsed: e.UsedFallback, LatencyMs: e.LatencyMs, CreatedAt: now,
	}); err != nil {
		s.log.Printf("WARN dispatch: write agent_events: %v", err)
	}
	ev := stream.NewAgentEvent(e.Agent, e.Decision, e.Reasoning, e.Confidence, e.LatencyMs, e.UsedFallback, now)
	ev.Kind, ev.RideID, ev.DriverID, ev.Tier = e.Kind, e.RideID, e.DriverID, e.Tier
	s.publish(ev)
}

// AdvanceRide moves a ride along its trip lifecycle and keeps the driver's
// status in step: matched → in_progress (driver en_route → on_trip) and
// in_progress → completed (driver on_trip → idle).
func (s *Service) AdvanceRide(ctx context.Context, id string, to store.RideStatus) (store.Ride, error) {
	var from store.RideStatus
	var dFrom, dTo fleet.Status
	switch to {
	case store.RideInProgress:
		from, dFrom, dTo = store.RideMatched, fleet.StatusEnRoute, fleet.StatusOnTrip
	case store.RideCompleted:
		from, dFrom, dTo = store.RideInProgress, fleet.StatusOnTrip, fleet.StatusIdle
	default:
		return store.Ride{}, fmt.Errorf("%w: cannot advance a ride to %q", ErrValidation, to)
	}
	s.rideMu.Lock()
	defer s.rideMu.Unlock()
	r, err := s.store.GetRide(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Ride{}, fmt.Errorf("%w: ride %s", ErrNotFound, id)
	}
	if err != nil {
		return store.Ride{}, err
	}
	if r.Status != from {
		return store.Ride{}, fmt.Errorf("%w: ride is %s, not %s", ErrConflict, r.Status, from)
	}
	if r.DriverID != "" {
		if _, err := s.fleet.Claim(r.DriverID, dFrom, dTo); err != nil && !errors.Is(err, fleet.ErrNotFound) {
			return store.Ride{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	r.Status = to
	r.UpdatedAt = s.now()
	if err := s.store.UpdateRide(ctx, r); err != nil {
		return store.Ride{}, err
	}
	return r, nil
}

// CloseStaleRides cancels every non-terminal ride last updated before `before`.
// The simulator calls it at start-up: rides from a previous process reference
// drivers that no longer exist and would otherwise inflate the live metrics.
func (s *Service) CloseStaleRides(ctx context.Context, before time.Time) (int64, error) {
	s.rideMu.Lock()
	defer s.rideMu.Unlock()
	return s.store.CloseStaleRides(ctx, before, "gateway_restart", s.now())
}

func (s *Service) assign(ride *store.Ride, best matching.Candidate, tripKm float64) {
	cfg := s.matcher.Config()
	d := best.Driver
	ride.Status = store.RideMatched
	ride.DriverID = d.ID
	ride.ETASeconds = best.Score.ETASeconds
	ride.MatchScore = best.Score.Total
	tripETA := tripKm / (cfg.BaseSpeedKmh * cfg.CitySpeedFactor) * 3600
	ride.Legs = []store.RideLeg{
		{Seq: 0, Kind: store.LegPickup, From: d.Point(), To: ride.Pickup, DistanceKm: best.Score.DistanceKm, ETASeconds: best.Score.ETASeconds},
		{Seq: 1, Kind: store.LegTrip, From: ride.Pickup, To: ride.Dropoff, DistanceKm: tripKm, ETASeconds: tripETA},
	}
}

func buildMatchView(res matching.Result) *MatchView {
	v := &MatchView{
		Driver:         res.Best.Driver,
		Ring:           res.Best.Ring,
		Score:          res.Best.Score,
		Alternatives:   make([]Alternative, 0, len(res.Alternatives)),
		Considered:     res.Considered,
		RingsSearched:  res.RingsSearched,
		SearchRadiusKm: res.SearchRadiusKm,
		Tier:           string(res.Tier),
		RequestedTier:  string(res.RequestedTier),
		Upgraded:       res.Upgraded,
		LatencyMs:      float64(res.Latency) / float64(time.Millisecond),
	}
	for _, a := range res.Alternatives {
		v.Alternatives = append(v.Alternatives, Alternative{
			DriverID: a.Driver.ID, Lat: a.Driver.Lat, Lng: a.Driver.Lng, Ring: a.Ring, Score: a.Score,
		})
	}
	return v
}

// release returns an en-route driver to idle (best effort).
func (s *Service) release(driverID string) {
	if _, err := s.fleet.Claim(driverID, fleet.StatusEnRoute, fleet.StatusIdle); err != nil && !errors.Is(err, fleet.ErrConflict) {
		s.log.Printf("WARN dispatch: release driver %s: %v", driverID, err)
	}
}

func (s *Service) recordMatchEvent(ctx context.Context, ride store.Ride, req matching.Request, view *MatchView, res matching.Result, note string) {
	input, _ := json.Marshal(map[string]any{"pickup": req.Pickup, "dropoff": req.Dropoff, "tier": req.Tier})
	ev := store.AgentEvent{
		Agent:     AgentName,
		EventType: "no_match",
		RideID:    ride.ID,
		Input:     input,
		LatencyMs: float64(res.Latency) / float64(time.Millisecond),
		CreatedAt: s.now(),
	}
	if view != nil {
		ev.EventType = "match"
		ev.Confidence = view.Score.Total
		ev.Decision, _ = json.Marshal(map[string]any{
			"driver_id": view.Driver.ID, "score": view.Score, "alternatives": view.Alternatives,
			"candidates_considered": view.Considered, "rings_searched": view.RingsSearched,
			"search_radius_km": view.SearchRadiusKm, "requested_tier": view.RequestedTier,
			"matched_tier": view.Tier, "upgraded": view.Upgraded,
		})
	} else {
		ev.Decision, _ = json.Marshal(map[string]any{
			"rings_searched": res.RingsSearched, "search_radius_km": res.SearchRadiusKm,
			"candidates_found": res.Considered, "requested_tier": req.Tier, "tiers_tried": res.TiersTried,
		})
	}
	if err := s.store.RecordEvent(ctx, ev); err != nil {
		s.log.Printf("WARN dispatch: write agent_events: %v", err)
	}
	decision, reasoning := describeMatch(ride, req, view, res)
	if note != "" {
		decision += "; " + note
	}
	pub := stream.NewAgentEvent(AgentName, decision, reasoning, ev.Confidence, ev.LatencyMs, false, ev.CreatedAt)
	pub.Kind, pub.RideID, pub.RequestedTier = ev.EventType, ride.ID, string(req.Tier)
	if view != nil {
		pub.DriverID, pub.Tier, pub.Upgraded = view.Driver.ID, view.Tier, view.Upgraded
	}
	s.publish(pub)
}

func describeMatch(ride store.Ride, req matching.Request, view *MatchView, res matching.Result) (decision, reasoning string) {
	rings := fmt.Sprintf("%d H3 ring", res.RingsSearched)
	if res.RingsSearched != 1 {
		rings += "s"
	}
	if view == nil {
		tiers := make([]string, len(res.TiersTried))
		for i, t := range res.TiersTried {
			tiers[i] = string(t)
		}
		decision = fmt.Sprintf("no driver available for ride %s (%s); request left open", ride.ID, req.Tier)
		if res.Considered > 0 {
			return decision, fmt.Sprintf("Searched %s (~%.1f km) around the pickup and found %d candidates, "+
				"but every one was claimed by a concurrent request.", rings, res.SearchRadiusKm, res.Considered)
		}
		return decision, fmt.Sprintf("Searched %s out to the %.1f km pickup radius and found 0 candidates "+
			"among idle %s drivers; the request stays open until a driver frees up.",
			rings, res.SearchRadiusKm, strings.Join(tiers, "/"))
	}
	sc := view.Score
	decision = fmt.Sprintf("matched ride %s to %s (%s), pickup ETA %.1f min",
		ride.ID, view.Driver.ID, view.Driver.Tier, sc.ETASeconds/60)
	if view.Upgraded {
		decision += fmt.Sprintf(", upgraded from %s", view.RequestedTier)
	}
	reasoning = fmt.Sprintf("Best of %d candidates within %s (~%.1f km): ", view.Considered, rings, view.SearchRadiusKm)
	if view.Upgraded {
		reasoning = fmt.Sprintf("No idle %s within %.1f km, so a free upgrade to %s was offered. ",
			view.RequestedTier, res.SearchRadiusKm, view.Tier) + reasoning
	}
	reasoning += fmt.Sprintf("%.2f km away; scores ETA %.2f, momentum %.2f, rating %.2f, idle %.2f, battery %.2f → %.2f.",
		sc.DistanceKm, sc.ETA, sc.Momentum, sc.Rating, sc.Idle, sc.Battery, sc.Total)
	if len(view.Alternatives) > 0 {
		a := view.Alternatives[0]
		reasoning += fmt.Sprintf(" Runner-up %s scored %.2f.", a.DriverID, a.Score.Total)
	}
	return decision, reasoning
}

// GetRide returns a ride by ID.
func (s *Service) GetRide(ctx context.Context, id string) (RideView, error) {
	r, err := s.store.GetRide(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return RideView{}, fmt.Errorf("%w: ride %s", ErrNotFound, id)
	}
	return RideView{Ride: r}, err
}

// CancelRide cancels an open or matched ride and frees its driver.
func (s *Service) CancelRide(ctx context.Context, id, reason string) (RideView, error) {
	v, err := s.cancelRide(ctx, id, reason)
	if err == nil {
		s.notify(v.Ride)
	}
	return v, err
}

func (s *Service) cancelRide(ctx context.Context, id, reason string) (RideView, error) {
	s.rideMu.Lock()
	defer s.rideMu.Unlock()
	r, err := s.store.GetRide(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return RideView{}, fmt.Errorf("%w: ride %s", ErrNotFound, id)
	}
	if err != nil {
		return RideView{}, err
	}
	if r.Status != store.RideRequested && r.Status != store.RideMatched {
		return RideView{}, fmt.Errorf("%w: ride is %s and can no longer be cancelled", ErrConflict, r.Status)
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "rider_cancelled"
	}
	r.Status = store.RideCancelled
	r.CancelReason = reason
	r.UpdatedAt = s.now()
	if err := s.store.UpdateRide(ctx, r); err != nil {
		return RideView{}, err
	}
	if r.DriverID != "" {
		s.release(r.DriverID)
	}
	return RideView{Ride: r}, nil
}

// ---- Drivers ----

// DriverStatusInput is the POST /v1/drivers/{id}/status body. Omitted fields
// keep their current value; a new driver needs lat, lng and tier.
type DriverStatusInput struct {
	Status     *string  `json:"status"`
	Lat        *float64 `json:"lat"`
	Lng        *float64 `json:"lng"`
	Heading    *float64 `json:"heading"`
	SpeedKmh   *float64 `json:"speed_kmh"`
	Tier       *string  `json:"tier"`
	BatteryPct *float64 `json:"battery_pct"`
	Rating     *float64 `json:"rating"`
}

// DriverView is the API representation of a driver.
type DriverView struct {
	Driver fleet.Driver `json:"driver"`
}

// UpdateDriverStatus merges in into the driver's live state, persists it,
// records telemetry and advances the driver's active ride if the status
// change implies it (on_trip → ride in progress, back to idle → completed).
func (s *Service) UpdateDriverStatus(ctx context.Context, id string, in DriverStatusInput) (DriverView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return DriverView{}, fmt.Errorf("%w: driver id is required", ErrValidation)
	}
	if (in.Lat == nil) != (in.Lng == nil) {
		return DriverView{}, fmt.Errorf("%w: lat and lng must be sent together", ErrValidation)
	}
	var status fleet.Status
	if in.Status != nil {
		st, err := fleet.ParseStatus(*in.Status)
		if err != nil {
			return DriverView{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		status = st
	}
	var tier fleet.Tier
	if in.Tier != nil {
		t, err := fleet.ParseTier(*in.Tier)
		if err != nil {
			return DriverView{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		tier = t
	}

	var prev fleet.Status
	d, err := s.fleet.Apply(id, func(d *fleet.Driver, exists bool) error {
		if !exists && (in.Lat == nil || in.Tier == nil) {
			return fmt.Errorf("%w: new driver needs lat, lng and tier", ErrValidation)
		}
		prev = d.Status
		if !exists {
			prev, d.Status = fleet.StatusOffline, fleet.StatusOffline
		}
		if status != "" {
			d.Status = status
		}
		if tier != "" {
			d.Tier = tier
		}
		if in.Lat != nil {
			d.Lat, d.Lng = *in.Lat, *in.Lng
		}
		setIf(&d.Heading, in.Heading)
		setIf(&d.SpeedKmh, in.SpeedKmh)
		setIf(&d.BatteryPct, in.BatteryPct)
		setIf(&d.Rating, in.Rating)
		return nil
	})
	switch {
	case errors.Is(err, fleet.ErrInvalidDriver):
		return DriverView{}, fmt.Errorf("%w: %v", ErrValidation, err)
	case err != nil:
		return DriverView{}, err
	}

	rec := store.DriverRecord{ID: d.ID, Status: string(d.Status), Tier: string(d.Tier), Rating: d.Rating}
	if d.Tier == fleet.TierAV {
		b := d.BatteryPct
		rec.BatteryPct = &b
	}
	if err := s.store.UpsertDriver(ctx, rec); err != nil {
		s.log.Printf("WARN dispatch: persist driver %s: %v", d.ID, err)
	}
	if err := s.telemetry.AppendTelemetry(ctx, store.TelemetryPoint{
		DriverID: d.ID, Lat: d.Lat, Lng: d.Lng, Heading: d.Heading, SpeedKmh: d.SpeedKmh,
		Status: string(d.Status), H3Cell: d.Cell.String(), RecordedAt: d.UpdatedAt.UTC(),
	}); err != nil {
		s.log.Printf("WARN dispatch: append telemetry %s: %v", d.ID, err)
	}
	if prev != d.Status {
		s.rideMu.Lock()
		err := s.advanceRide(ctx, d.ID, d.Status)
		s.rideMu.Unlock()
		if err != nil {
			s.log.Printf("WARN dispatch: advance ride for driver %s: %v", d.ID, err)
		}
	}
	return DriverView{Driver: d}, nil
}

func setIf(dst *float64, v *float64) {
	if v != nil {
		*dst = *v
	}
}

// advanceRide moves the driver's active ride along with the driver's status.
// Callers hold rideMu.
func (s *Service) advanceRide(ctx context.Context, driverID string, st fleet.Status) error {
	r, err := s.store.ActiveRideForDriver(ctx, driverID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case st == fleet.StatusOnTrip && r.Status == store.RideMatched:
		r.Status = store.RideInProgress
	case st != fleet.StatusOnTrip && r.Status == store.RideInProgress:
		r.Status = store.RideCompleted
	case (st == fleet.StatusIdle || st == fleet.StatusOffline) && r.Status == store.RideMatched:
		// Driver abandoned the pickup: reopen the request.
		r.Status = store.RideRequested
		r.DriverID = ""
		r.ETASeconds = 0
		r.MatchScore = 0
	default:
		return nil
	}
	r.UpdatedAt = s.now()
	return s.store.UpdateRide(ctx, r)
}

// ---- Metrics ----

// DispatchMetrics is the GET /v1/metrics/dispatch body. Field order is part of the contract.
type DispatchMetrics struct {
	P50Ms           metrics.Millis `json:"p50_ms"`
	P95Ms           metrics.Millis `json:"p95_ms"`
	P99Ms           metrics.Millis `json:"p99_ms"`
	MatchesTotal    uint64         `json:"matches_total"`
	ActiveDrivers   int            `json:"active_drivers"`
	IdleDrivers     int            `json:"idle_drivers"`
	OpenRequests    int            `json:"open_requests"`
	RidesInProgress int            `json:"rides_in_progress"`
}

// Metrics returns the live dispatch summary.
func (s *Service) Metrics(ctx context.Context) (DispatchMetrics, error) {
	p50, p95, p99 := s.rec.Quantiles()
	fc := s.fleet.Counts()
	rc, err := s.store.RideCounts(ctx)
	if err != nil {
		return DispatchMetrics{}, err
	}
	return DispatchMetrics{
		P50Ms: metrics.Millis(p50), P95Ms: metrics.Millis(p95), P99Ms: metrics.Millis(p99),
		MatchesTotal:  s.rec.MatchesTotal(),
		ActiveDrivers: fc.Active, IdleDrivers: fc.Idle,
		OpenRequests: rc.Open, RidesInProgress: rc.InProgress,
	}, nil
}
