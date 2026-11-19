package safety

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// RedisChannel carries every safety_signal as JSON.
const RedisChannel = "safety"

// ConfirmingAgent is the only actor allowed to lift a suspension.
const ConfirmingAgent = "ops_supervisor"

const (
	recentSignals   = 50
	recentEmergency = 50
	trackTTL        = 2 * time.Hour
	postTimeout     = 5 * time.Second
	postRetryWait   = time.Second
)

// Errors.
var (
	ErrUnknownDriver = errors.New("safety: unknown driver")
	ErrNotSuspended  = errors.New("safety: driver is not suspended")
	ErrForbidden     = errors.New("safety: only the ops_supervisor can lift a suspension")
)

// RedisPublisher publishes a message on a Redis channel.
type RedisPublisher interface {
	Publish(ctx context.Context, channel, message string) error
}

// VehicleController stops and releases vehicles (the fleet simulator).
type VehicleController interface {
	// Suspend stops the vehicle, ends its ride and, for AVs on a trip,
	// dispatches a replacement ride for the rider.
	Suspend(ctx context.Context, driverID, reason string) (SuspendResult, error)
	// Unsuspend returns the vehicle to service.
	Unsuspend(ctx context.Context, driverID string) error
}

// SuspendResult describes what happened to the vehicle.
type SuspendResult struct {
	DriverID          string    `json:"driver_id"`
	Tier              string    `json:"tier,omitempty"`
	VehicleStopped    bool      `json:"vehicle_stopped"`
	InterruptedRideID string    `json:"interrupted_ride_id,omitempty"`
	ReplacementRideID string    `json:"replacement_ride_id,omitempty"`
	Location          geo.Point `json:"location"`
}

// Positions looks up a driver's live position.
type Positions interface {
	Get(id string) (fleet.Driver, bool)
}

// Options configures a Service.
type Options struct {
	Config     Config
	AgentsURL  string       // agents-py base URL; "" = signals are not posted
	HTTPClient *http.Client // nil = default client
	Redis      RedisPublisher
	Stream     stream.Publisher
	Events     func(ctx context.Context, e dispatch.AgentEventInput) // agent_events + ops feed; nil = none
	Positions  Positions
	Telemetry  store.TelemetryStore
	Logger     *log.Logger
	Now        func() time.Time
}

// Suspension is one frozen driver.
type Suspension struct {
	DriverID   string        `json:"driver_id"`
	IncidentID string        `json:"incident_id"`
	Reason     string        `json:"reason"`
	Actor      string        `json:"actor"`
	At         time.Time     `json:"suspended_at"`
	Result     SuspendResult `json:"result"`
}

type trackLink struct {
	Token      string    `json:"token"`
	DriverID   string    `json:"driver_id"`
	RideID     string    `json:"ride_id,omitempty"`
	IncidentID string    `json:"incident_id,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// EmergencyRecord is one simulated emergency dispatch or contact notification.
type EmergencyRecord struct {
	Kind      string         `json:"kind"` // dispatch | contact
	Reference string         `json:"reference"`
	Payload   map[string]any `json:"payload"`
	At        time.Time      `json:"at"`
}

// Service ships signals to the Safety Sentinel and executes its actions.
type Service struct {
	det     *Detector
	agents  string
	client  *http.Client
	redis   RedisPublisher
	stream  stream.Publisher
	events  func(context.Context, dispatch.AgentEventInput)
	pos     Positions
	tel     store.TelemetryStore
	log     *log.Logger
	now     func() time.Time
	pending sync.WaitGroup

	mu          sync.Mutex
	vehicles    VehicleController
	signals     []Signal // newest last
	suspensions map[string]Suspension
	tracks      map[string]trackLink
	emergency   []EmergencyRecord
	posted      int
	postErrors  int

	onSuspend []SuspensionHook // see OnSuspension (hooks.go)
	onRelease []SuspensionHook
	onIncident []func(context.Context, IncidentOpened) // see OnIncidentOpened (hooks.go)
}

// New builds a Service.
func New(o Options) *Service {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: postTimeout}
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{
		det: NewDetector(o.Config), agents: strings.TrimRight(o.AgentsURL, "/"), client: o.HTTPClient,
		redis: o.Redis, stream: o.Stream, events: o.Events, pos: o.Positions, tel: o.Telemetry,
		log: o.Logger, now: func() time.Time { return o.Now().UTC() },
		suspensions: map[string]Suspension{}, tracks: map[string]trackLink{},
	}
}

// Detector returns the detector.
func (s *Service) Detector() *Detector { return s.det }

// SetVehicles attaches the vehicle controller (the simulator is built after the Service).
func (s *Service) SetVehicles(v VehicleController) {
	s.mu.Lock()
	s.vehicles = v
	s.mu.Unlock()
}

// Wait blocks until every in-flight signal delivery finished (tests, shutdown).
func (s *Service) Wait() { s.pending.Wait() }

// ObserveTick runs the detector over one simulator tick and ships new signals.
func (s *Service) ObserveTick(ctx context.Context, obs []Observation) {
	var now time.Time
	for _, o := range obs {
		if sig := s.det.Observe(o); sig != nil {
			s.Emit(ctx, *sig)
		}
		if o.At.After(now) {
			now = o.At
		}
	}
	if !now.IsZero() {
		s.det.Sweep(now)
	}
}

// RideEnded forgets a finished ride.
func (s *Service) RideEnded(rideID string) { s.det.RideEnded(rideID) }

// Emit records, publishes and posts one signal. Delivery to agents-py runs
// in the background; Emit never blocks the simulator tick on the network.
func (s *Service) Emit(ctx context.Context, sig Signal) {
	s.mu.Lock()
	s.signals = append(s.signals, sig)
	if len(s.signals) > recentSignals {
		s.signals = append(s.signals[:0], s.signals[len(s.signals)-recentSignals:]...)
	}
	s.mu.Unlock()

	if s.events != nil {
		s.events(ctx, dispatch.AgentEventInput{
			Agent: "safety_sentinel", EventType: "safety_signal", RideID: sig.RideID, Kind: "incident", DriverID: sig.DriverID,
			Input:      map[string]any{"signal_id": sig.ID, "triggers": sig.Triggers, "lat": sig.Location.Lat, "lng": sig.Location.Lng},
			Decision:   fmt.Sprintf("safety signal on ride %s (%s): %s, severity %.2f", sig.RideID, sig.DriverID, Describe(sig.Triggers), sig.Severity),
			Reasoning:  "Deterministic telemetry detector (dispatch-go): " + Explain(sig.Triggers) + " Handing over to the Safety Sentinel.",
			Confidence: sig.Severity,
		})
	}
	body, err := json.Marshal(sig)
	if err != nil { // cannot happen for this struct
		s.log.Printf("WARN safety: encode signal: %v", err)
		return
	}
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*postTimeout)
		defer cancel()
		if s.redis != nil {
			if err := s.redis.Publish(bg, RedisChannel, string(body)); err != nil {
				s.log.Printf("WARN safety: publish %s on redis: %v", sig.ID, err)
			}
		}
		if out := s.post(bg, sig.ID, body); out != nil {
			s.incidentFromReply(bg, sig, out)
		}
	}()
}

// post delivers the signal to agents-py, retrying once, and returns the
// Safety Sentinel's reply (nil when delivery failed).
func (s *Service) post(ctx context.Context, id string, body []byte) []byte {
	if s.agents == "" {
		return nil
	}
	var err error
	var out []byte
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(postRetryWait):
			}
		}
		if out, err = s.postOnce(ctx, body); err == nil {
			s.mu.Lock()
			s.posted++
			s.mu.Unlock()
			return out
		}
	}
	s.mu.Lock()
	s.postErrors++
	s.mu.Unlock()
	s.log.Printf("WARN safety: post signal %s to agents-py: %v", id, err)
	return nil
}

func (s *Service) postOnce(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.agents+"/v1/safety/signal", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return out, nil
}

// Signals returns the most recent signals, newest first.
func (s *Service) Signals() []Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Signal, 0, len(s.signals))
	for i := len(s.signals) - 1; i >= 0; i-- {
		out = append(out, s.signals[i])
	}
	return out
}

// Delivery reports how many signals reached agents-py and how many failed.
func (s *Service) Delivery() (posted, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posted, s.postErrors
}

// ---- driver suspension ----

// Suspend freezes a driver: the vehicle stops (via the controller, when one is
// attached) and stays out of dispatch until an ops_supervisor-confirmed Unsuspend.
func (s *Service) Suspend(ctx context.Context, driverID, incidentID, reason, actor string) (Suspension, error) {
	driverID = strings.TrimSpace(driverID)
	if driverID == "" {
		return Suspension{}, ErrUnknownDriver
	}
	s.mu.Lock()
	if cur, ok := s.suspensions[driverID]; ok {
		s.mu.Unlock()
		return cur, nil // idempotent
	}
	v := s.vehicles
	s.mu.Unlock()

	res := SuspendResult{DriverID: driverID}
	if v != nil {
		r, err := v.Suspend(ctx, driverID, reason)
		if err != nil {
			return Suspension{}, err
		}
		res = r
	} else if s.pos != nil {
		d, ok := s.pos.Get(driverID)
		if !ok {
			return Suspension{}, ErrUnknownDriver
		}
		res.Tier, res.Location = string(d.Tier), d.Point()
	}
	sp := Suspension{DriverID: driverID, IncidentID: incidentID, Reason: reason, Actor: actor, At: s.now(), Result: res}
	s.mu.Lock()
	s.suspensions[driverID] = sp
	s.mu.Unlock()
	s.fire(ctx, false, sp)
	return sp, nil
}

// Unsuspend lifts a suspension; only the ops_supervisor may confirm it.
func (s *Service) Unsuspend(ctx context.Context, driverID, confirmedBy string) error {
	if confirmedBy != ConfirmingAgent {
		return ErrForbidden
	}
	s.mu.Lock()
	sp, ok := s.suspensions[driverID]
	v := s.vehicles
	s.mu.Unlock()
	if !ok {
		return ErrNotSuspended
	}
	if v != nil {
		if err := v.Unsuspend(ctx, driverID); err != nil && !errors.Is(err, ErrUnknownDriver) {
			return err
		}
	}
	s.mu.Lock()
	delete(s.suspensions, driverID)
	s.mu.Unlock()
	s.fire(ctx, true, sp)
	return nil
}

// Suspended returns the driver's suspension, if any.
func (s *Service) Suspended(driverID string) (Suspension, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.suspensions[driverID]
	return sp, ok
}

// ---- share live location ----

// TrackLink creates a share-live-location token for a driver.
func (s *Service) TrackLink(driverID, rideID, incidentID string) (trackLink, error) {
	if strings.TrimSpace(driverID) == "" {
		return trackLink{}, ErrUnknownDriver
	}
	l := trackLink{Token: newID(""), DriverID: driverID, RideID: rideID, IncidentID: incidentID, ExpiresAt: s.now().Add(trackTTL)}
	s.mu.Lock()
	now := s.now()
	for k, t := range s.tracks {
		if now.After(t.ExpiresAt) {
			delete(s.tracks, k)
		}
	}
	s.tracks[l.Token] = l
	s.mu.Unlock()
	return l, nil
}

// TrackView is the live location behind a share link.
type TrackView struct {
	DriverID   string    `json:"driver_id"`
	RideID     string    `json:"ride_id,omitempty"`
	IncidentID string    `json:"incident_id,omitempty"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	SpeedKmh   float64   `json:"speed_kmh"`
	Status     string    `json:"status"`
	Suspended  bool      `json:"suspended"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Track resolves a share link to the vehicle's live position.
func (s *Service) Track(token string) (TrackView, bool) {
	s.mu.Lock()
	l, ok := s.tracks[token]
	_, susp := s.suspensions[l.DriverID]
	s.mu.Unlock()
	if !ok || s.now().After(l.ExpiresAt) {
		return TrackView{}, false
	}
	v := TrackView{DriverID: l.DriverID, RideID: l.RideID, IncidentID: l.IncidentID, Suspended: susp, ExpiresAt: l.ExpiresAt, Status: "unknown"}
	if s.pos != nil {
		if d, ok := s.pos.Get(l.DriverID); ok {
			v.Lat, v.Lng, v.SpeedKmh, v.Status, v.UpdatedAt = d.Lat, d.Lng, d.SpeedKmh, string(d.Status), d.UpdatedAt
		}
	}
	if susp {
		v.Status = "suspended"
	}
	return v, true
}

// ---- simulated emergency services ----

func (s *Service) recordEmergency(kind, prefix string, payload map[string]any) EmergencyRecord {
	rec := EmergencyRecord{Kind: kind, Reference: strings.ToUpper(newID(prefix))[:len(prefix)+8], Payload: payload, At: s.now()}
	s.mu.Lock()
	s.emergency = append(s.emergency, rec)
	if len(s.emergency) > recentEmergency {
		s.emergency = append(s.emergency[:0], s.emergency[len(s.emergency)-recentEmergency:]...)
	}
	s.mu.Unlock()
	b, _ := json.Marshal(payload)
	s.log.Printf("SAFETY simulated %s %s: %s", kind, rec.Reference, b)
	return rec
}

// EmergencyLog returns the simulated emergency records, newest first.
func (s *Service) EmergencyLog() []EmergencyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EmergencyRecord, 0, len(s.emergency))
	for i := len(s.emergency) - 1; i >= 0; i-- {
		out = append(out, s.emergency[i])
	}
	return out
}

// ---- descriptions ----

// Describe renders triggers as "route deviation 420 m, harsh deceleration 1.4 g".
func Describe(ts []Trigger) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		label := strings.ReplaceAll(t.Type, "_", " ")
		switch t.Unit {
		case "m":
			parts = append(parts, fmt.Sprintf("%s %.0f m", label, t.Value))
		case "g":
			parts = append(parts, fmt.Sprintf("%s %.1f g", label, t.Value))
		case "s":
			parts = append(parts, fmt.Sprintf("%s %.0f s", label, t.Value))
		default:
			parts = append(parts, fmt.Sprintf("%s %.0f %s", label, t.Value, t.Unit))
		}
	}
	if len(parts) == 0 {
		return "no anomaly"
	}
	return strings.Join(parts, ", ")
}

// Explain gives the rule behind each trigger.
func Explain(ts []Trigger) string {
	var b strings.Builder
	for _, t := range ts {
		switch t.Type {
		case TriggerDeviation:
			fmt.Fprintf(&b, "Vehicle %.0f m off the planned route for %.0f s. ", t.Value, t.DurationS)
		case TriggerHarshDecel:
			fmt.Fprintf(&b, "Speed dropped at %.1f g (threshold 0.5 g). ", t.Value)
		case TriggerStop:
			fmt.Fprintf(&b, "Standing still for %.0f s away from pickup and drop-off. ", t.DurationS)
		case TriggerSpeed:
			fmt.Fprintf(&b, "Sustained %.0f km/h for %.0f s. ", t.Value, t.DurationS)
		}
	}
	return strings.TrimSpace(b.String())
}
