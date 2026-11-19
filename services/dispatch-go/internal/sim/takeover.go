package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/ledger"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// Take-over mode: a person drives one simulated car from the Driver screen.
//
// While a driver is human-controlled (TakeoverTTL, renewed by every
// POST /v1/drivers/{id}/takeover while the screen is open) the simulator parks
// it instead of wandering, keeps it online in a rain storm, and rides matched
// to it are offered instead of auto-accepted: a ride_offer goes out on the
// driver:<id> stream topic and the driver has OfferTimeout to accept. Accept
// starts the normal ride flow; decline, timeout, or the take-over ending with
// an offer pending re-run matching without that driver
// (dispatch.RematchExcluding). A ride the driver declined is never offered to
// it again. When the take-over is released or expires the car reverts to
// simulator control.
//
// State machine per driver:
//
//	simulated --TakeOver--> human/idle --ride matched--> human/offered
//	human/offered --accept--> human/on ride (normal flow) --ride ends--> human/idle
//	human/offered --decline | timeout--> human/idle (ride re-matched elsewhere)
//	human/* --Release | TTL expiry--> simulated (pending offer re-matched)

// Take-over timings.
const (
	TakeoverTTL  = 10 * time.Minute
	OfferTimeout = 15 * time.Second
)

// Take-over errors.
var (
	ErrUnknownDriver = errors.New("sim: unknown driver")
	ErrNotTakenOver  = errors.New("sim: driver is not in take-over mode")
	ErrNoOffer       = errors.New("sim: no pending offer for this ride")
	ErrDriverBusy    = errors.New("sim: driver already has a ride or a pending offer")
)

// RideOffer is a message on the driver:<id> topic: "ride_offer" when a ride is
// offered, "offer_closed" (with Reason accepted | declined | timeout |
// cancelled | released) when it is no longer open.
type RideOffer struct {
	Type         string    `json:"type"`
	RideID       string    `json:"ride_id"`
	DriverID     string    `json:"driver_id"`
	RiderID      string    `json:"rider_id,omitempty"`
	Pickup       geo.Point `json:"pickup"`
	Dropoff      geo.Point `json:"dropoff"`
	FareINR      float64   `json:"fare_inr"`
	TripKm       float64   `json:"trip_km"`
	PickupETASec int       `json:"pickup_eta_sec"`
	TimeoutS     int       `json:"timeout_s"`
	ExpiresAt    time.Time `json:"expires_at"`
	Reason       string    `json:"reason,omitempty"`
}

// TakeoverView is the take-over state of one driver.
type TakeoverView struct {
	DriverID        string     `json:"driver_id"`
	HumanControlled bool       `json:"human_controlled"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	TTLS            int        `json:"ttl_s"`
	Offer           *RideOffer `json:"offer,omitempty"`
}

// OfferResult is the answer to POST /v1/rides/{id}/offer/{accept|decline}.
type OfferResult struct {
	RideID      string `json:"ride_id"`
	DriverID    string `json:"driver_id"`
	Decision    string `json:"decision"`               // accepted | declined
	Status      string `json:"status"`                 // the ride's status afterwards
	RematchedTo string `json:"rematched_to,omitempty"` // declined: the new driver, if any
}

type takeover struct {
	until    time.Time
	offer    *RideOffer
	ride     store.Ride // the offered ride
	declined map[string]bool
}

type rematchJob struct {
	rideID, driverID, reason string
	created                  time.Time
	offer                    *RideOffer // closed with reason once the re-match ran
}

// takeoverState lives in Sim, guarded by Sim.mu.
type takeoverState struct {
	drivers map[string]*takeover
	queue   []rematchJob
	msgs    []RideOffer
}

func (s *Sim) humanLocked(id string) bool {
	t := s.to.drivers[id]
	return t != nil && s.now().Before(t.until)
}

// HumanControlled reports whether driver id is in take-over mode.
func (s *Sim) HumanControlled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.humanLocked(id)
}

func (s *Sim) viewLocked(id string) TakeoverView {
	v := TakeoverView{DriverID: id}
	if t := s.to.drivers[id]; t != nil {
		until := t.until
		v.HumanControlled, v.ExpiresAt = true, &until
		v.TTLS = int(math.Ceil(until.Sub(s.now()).Seconds()))
		if t.offer != nil {
			o := *t.offer
			v.Offer = &o
		}
	}
	return v
}

// Takeover returns driver id's take-over state.
func (s *Sim) Takeover(id string) (TakeoverView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.drivers[id]; !ok {
		return TakeoverView{}, fmt.Errorf("%w: %s", ErrUnknownDriver, id)
	}
	return s.viewLocked(id), nil
}

// TakeOver puts driver id under human control for TakeoverTTL, or renews it.
func (s *Sim) TakeOver(ctx context.Context, id string) (TakeoverView, error) {
	s.mu.Lock()
	d, ok := s.drivers[id]
	if !ok {
		s.mu.Unlock()
		return TakeoverView{}, fmt.Errorf("%w: %s", ErrUnknownDriver, id)
	}
	if s.to.drivers == nil {
		s.to.drivers = map[string]*takeover{}
	}
	t, renewing := s.to.drivers[id]
	if !renewing {
		t = &takeover{declined: map[string]bool{}}
		s.to.drivers[id] = t
		if d.ride == nil { // stop wandering: the person decides where the car goes
			s.clearRoute(d)
			d.speed = 0
		}
	}
	t.until = s.now().Add(TakeoverTTL)
	v := s.viewLocked(id)
	s.mu.Unlock()
	if !renewing {
		s.announceTakeover(ctx, fmt.Sprintf("driver %s switched to human control (take-over mode)", id),
			"Rides matched to this car are offered to the person driving it (15 s to accept) instead of auto-accepted.")
	}
	return v, nil
}

// Release returns driver id to the simulator; a pending offer is re-matched.
func (s *Sim) Release(ctx context.Context, id string) (TakeoverView, error) {
	s.mu.Lock()
	if _, ok := s.drivers[id]; !ok {
		s.mu.Unlock()
		return TakeoverView{}, fmt.Errorf("%w: %s", ErrUnknownDriver, id)
	}
	t := s.to.drivers[id]
	delete(s.to.drivers, id)
	var job *rematchJob
	if t != nil && t.offer != nil {
		job = &rematchJob{rideID: t.offer.RideID, driverID: id, reason: "released", created: t.ride.CreatedAt, offer: t.offer}
	}
	v := s.viewLocked(id)
	s.mu.Unlock()
	if job != nil {
		s.runRematch(ctx, *job)
	}
	if t != nil {
		s.announceTakeover(ctx, fmt.Sprintf("driver %s back under simulator control (take-over released)", id),
			"The Driver screen was closed; the fleet simulator drives this car again.")
	}
	return v, nil
}

// SendRide (DEMO_MODE) books a ride near a taken-over driver and assigns it
// to that driver, so the offer card can be shown on demand.
func (s *Sim) SendRide(ctx context.Context, id string) (dispatch.RideView, error) {
	s.mu.Lock()
	d, ok := s.drivers[id]
	if !ok {
		s.mu.Unlock()
		return dispatch.RideView{}, fmt.Errorf("%w: %s", ErrUnknownDriver, id)
	}
	t := s.to.drivers[id]
	if t == nil || !s.now().Before(t.until) {
		s.mu.Unlock()
		return dispatch.RideView{}, fmt.Errorf("%w: %s", ErrNotTakenOver, id)
	}
	if d.ride != nil || t.offer != nil {
		s.mu.Unlock()
		return dispatch.RideView{}, fmt.Errorf("%w: %s", ErrDriverBusy, id)
	}
	pickup := s.offset(d.pos, 0.4+s.rng.Float64()*0.4)
	dropoff := s.jitter(s.pickHotspotNear(pickup).at, 1.0)
	if km := geo.HaversineKm(pickup, dropoff); km < 2 || km > 7 {
		dropoff = s.offset(pickup, 3+s.rng.Float64()*3)
	}
	rider := fmt.Sprintf("r_demo_%04d", s.rng.IntN(10000))
	s.mu.Unlock()
	v, err := s.svc.CreateRideForDriver(ctx, dispatch.CreateRideInput{RiderID: rider, Pickup: &pickup, Dropoff: &dropoff}, id)
	if errors.Is(err, dispatch.ErrConflict) {
		return v, fmt.Errorf("%w: %v", ErrDriverBusy, err)
	}
	return v, err
}

// offset is a point km away from p in a random direction. Callers hold s.mu.
func (s *Sim) offset(p geo.Point, km float64) geo.Point {
	a := s.rng.Float64() * 2 * math.Pi
	return geo.Point{
		Lat: round(p.Lat+km*math.Cos(a)/111.32, 6),
		Lng: round(p.Lng+km*math.Sin(a)/(111.32*math.Cos(p.Lat*math.Pi/180)), 6),
	}
}

// RespondOffer accepts or declines the pending offer of ride rideID.
func (s *Sim) RespondOffer(ctx context.Context, rideID string, accept bool) (OfferResult, error) {
	s.mu.Lock()
	id, t := s.offerFor(rideID)
	if t == nil {
		s.mu.Unlock()
		return OfferResult{}, fmt.Errorf("%w: %s", ErrNoOffer, rideID)
	}
	offer, ride := *t.offer, t.ride
	t.offer = nil
	if !accept {
		t.declined[rideID] = true
		s.mu.Unlock()
		v := s.runRematch(ctx, rematchJob{rideID: rideID, driverID: id, reason: "declined", created: ride.CreatedAt, offer: &offer})
		res := OfferResult{RideID: rideID, DriverID: id, Decision: "declined", Status: string(v.Ride.Status)}
		if v.Match != nil {
			res.RematchedTo = v.Ride.DriverID
		}
		return res, nil
	}
	d := s.drivers[id]
	now := s.now()
	u := s.startRide(d, ride, now)
	s.mu.Unlock()
	s.closeOffer(offer, "accepted")
	s.pub.Publish(stream.RideTopic(rideID), u)
	return OfferResult{RideID: rideID, DriverID: id, Decision: "accepted", Status: string(store.RideMatched)}, nil
}

// offerFor finds the taken-over driver holding an offer for rideID. Callers hold s.mu.
func (s *Sim) offerFor(rideID string) (string, *takeover) {
	for id, t := range s.to.drivers {
		if t.offer != nil && t.offer.RideID == rideID {
			return id, t
		}
	}
	return "", nil
}

// offerToHuman turns a ride matched to a taken-over driver into an offer.
// It reports false for a simulated driver (the ride is auto-accepted).
// Callers hold s.mu.
func (s *Sim) offerToHuman(d *simDriver, r store.Ride, now time.Time) bool {
	if !s.humanLocked(d.id) {
		return false
	}
	t := s.to.drivers[d.id]
	if t.declined[r.ID] || t.offer != nil {
		s.to.queue = append(s.to.queue, rematchJob{rideID: r.ID, driverID: d.id, reason: "declined", created: r.CreatedAt})
		return true
	}
	fare := float64(ledger.DefaultFares().Quote(r.Tier, r.DistanceKm)) / 100
	t.ride = r
	t.offer = &RideOffer{
		Type: "ride_offer", RideID: r.ID, DriverID: d.id, RiderID: r.RiderID, Pickup: r.Pickup, Dropoff: r.Dropoff,
		FareINR: fare, TripKm: round(r.DistanceKm, 2), PickupETASec: int(math.Round(r.ETASeconds)),
		TimeoutS: int(OfferTimeout / time.Second), ExpiresAt: now.Add(OfferTimeout),
	}
	s.to.msgs = append(s.to.msgs, *t.offer)
	return true
}

// withdrawOffer closes the offer of a cancelled ride. Callers hold s.mu.
func (s *Sim) withdrawOffer(rideID string) {
	if _, t := s.offerFor(rideID); t != nil {
		closed := *t.offer
		closed.Type, closed.Reason = "offer_closed", "cancelled"
		s.to.msgs = append(s.to.msgs, closed)
		t.offer = nil
	}
}

// tickTakeovers expires take-overs and offers. Callers hold s.mu.
func (s *Sim) tickTakeovers(now time.Time) []string {
	var ended []string
	ids := make([]string, 0, len(s.to.drivers))
	for id := range s.to.drivers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := s.to.drivers[id]
		reason := ""
		switch {
		case !now.Before(t.until):
			delete(s.to.drivers, id)
			ended = append(ended, id)
			reason = "released"
		case t.offer != nil && !now.Before(t.offer.ExpiresAt):
			t.declined[t.offer.RideID] = true
			reason = "timeout"
		}
		if reason != "" && t.offer != nil {
			o := *t.offer
			s.to.queue = append(s.to.queue, rematchJob{rideID: o.RideID, driverID: id, reason: reason, created: t.ride.CreatedAt, offer: &o})
			t.offer = nil
		}
	}
	return ended
}

// flushTakeovers publishes offer messages and runs queued re-matches (s.mu not held).
func (s *Sim) flushTakeovers(ctx context.Context, ended []string) {
	s.mu.Lock()
	msgs, queue := s.to.msgs, s.to.queue
	s.to.msgs, s.to.queue = nil, nil
	s.mu.Unlock()
	for _, m := range msgs {
		s.pub.Publish(stream.DriverTopic(m.DriverID), m)
	}
	for _, j := range queue {
		s.runRematch(ctx, j)
	}
	for _, id := range ended {
		s.announceTakeover(ctx, fmt.Sprintf("driver %s back under simulator control (take-over expired)", id),
			"No renewal from the Driver screen for 10 min; the fleet simulator drives this car again.")
	}
}

// runRematch re-matches a ride without the driver who passed on it; an
// unmatched ride joins the open requests the simulator retries.
func (s *Sim) runRematch(ctx context.Context, j rematchJob) dispatch.RideView {
	v, err := s.svc.RematchExcluding(ctx, j.rideID, j.driverID, j.reason)
	if err != nil && !errors.Is(err, dispatch.ErrConflict) {
		s.log.Printf("WARN sim: re-match ride %s without %s: %v", j.rideID, j.driverID, err)
	}
	if err == nil && v.Match == nil {
		s.mu.Lock()
		s.open = append(s.open, openRequest{id: j.rideID, created: j.created, tried: s.now()})
		s.mu.Unlock()
	}
	if j.offer != nil {
		s.closeOffer(*j.offer, j.reason)
	}
	return v
}

func (s *Sim) closeOffer(o RideOffer, reason string) {
	o.Type, o.Reason = "offer_closed", reason
	s.pub.Publish(stream.DriverTopic(o.DriverID), o)
}

func (s *Sim) announceTakeover(ctx context.Context, decision, reasoning string) {
	s.svc.RecordAgentEvent(ctx, dispatch.AgentEventInput{
		Agent: "fleet_simulator", EventType: "takeover", Kind: "takeover", Decision: decision, Reasoning: reasoning, Confidence: 1,
	})
}

// RouteFor returns a road route from a to b (OSRM when configured, cached and
// rate-limited by the router) and its source; a slow or failed router gives
// the straight line. Used by the Driver screen's "Move to hotspot" card.
func (s *Sim) RouteFor(ctx context.Context, a, b geo.Point) ([]geo.Point, string) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if _, straight := s.router.(StraightLine); !straight {
		if pts, err := s.router.Route(ctx, a, b); err == nil && len(pts) >= 2 {
			return pts, "osrm"
		}
	}
	return []geo.Point{a, b}, "straight_line"
}
