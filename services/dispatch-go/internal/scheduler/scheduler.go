// Package scheduler stores the legs of confirmed multi-leg booking plans and
// dispatches each one through the normal matching flow at its pickup time.
//
// Real time: a leg is dispatched Lead (10 min) before its pickup time.
// DEMO_MODE compresses time so a whole plan can be watched live: the first
// leg dispatches FirstDelay (20 s) after confirmation and every later leg
// NextDelay (60 s) after the previous leg's ride completes (or is cancelled).
//
// A dispatched leg follows its ride (GET via the dispatch service) until the
// ride is completed or cancelled; a ride still waiting for a driver is
// re-dispatched every RetryEvery.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

// Status is a scheduled leg's state.
type Status string

const (
	StatusScheduled  Status = "scheduled"  // waiting for its dispatch time (or, in demo, the previous leg)
	StatusDispatched Status = "dispatched" // a ride exists; following it
	StatusCompleted  Status = "completed"
	StatusCancelled  Status = "cancelled"
	StatusFailed     Status = "failed" // the ride could not be created
)

// Terminal reports whether the leg needs no more work.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusCancelled || s == StatusFailed
}

// Defaults.
const (
	DefaultFirstDelay = 20 * time.Second
	DefaultNextDelay  = 60 * time.Second
	DefaultLead       = 10 * time.Minute
	DefaultTick       = time.Second
	DefaultRetryEvery = 5 * time.Second
	MaxLegs           = 10
)

// ErrValidation marks a bad request.
var ErrValidation = errors.New("validation")

// Leg is one scheduled ride of a plan.
type Leg struct {
	ID            string     `json:"id"`
	PlanID        string     `json:"plan_id"`
	RiderID       string     `json:"rider_id"`
	Seq           int        `json:"seq"`
	Passenger     string     `json:"passenger"`
	PassengerName string     `json:"passenger_name"`
	Tier          string     `json:"tier"`          // as booked (may be pool)
	DispatchTier  string     `json:"dispatch_tier"` // fleet tier sent to matching
	Pickup        geo.Point  `json:"pickup"`
	Dropoff       geo.Point  `json:"dropoff"`
	PickupName    string     `json:"pickup_name"`
	DropoffName   string     `json:"dropoff_name"`
	PickupAt      time.Time  `json:"pickup_at"`
	ArriveBy      *time.Time `json:"arrive_by,omitempty"`
	DispatchAt    *time.Time `json:"dispatch_at,omitempty"` // nil: waits for the previous leg (demo)
	Fare          float64    `json:"fare"`
	DurationMin   float64    `json:"duration_min"`
	Status        Status     `json:"status"`
	RideID        string     `json:"ride_id,omitempty"`
	RideStatus    string     `json:"ride_status,omitempty"`
	DriverID      string     `json:"driver_id,omitempty"`
	Note          string     `json:"note,omitempty"`
	DemoMode      bool       `json:"demo_mode"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DispatchedAt  *time.Time `json:"dispatched_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`

	lastRetry time.Time
}

// LegInput is one leg of a POST /v1/scheduled body.
type LegInput struct {
	Seq           int        `json:"seq"`
	Passenger     string     `json:"passenger"`
	PassengerName string     `json:"passenger_name"`
	Tier          string     `json:"tier"`
	Pickup        *geo.Point `json:"pickup"`
	Dropoff       *geo.Point `json:"dropoff"`
	PickupName    string     `json:"pickup_name"`
	DropoffName   string     `json:"dropoff_name"`
	PickupAt      time.Time  `json:"pickup_at"`
	ArriveBy      *time.Time `json:"arrive_by"`
	Fare          float64    `json:"fare"`
	DurationMin   float64    `json:"duration_min"`
}

// PlanInput is the POST /v1/scheduled body.
type PlanInput struct {
	PlanID  string     `json:"plan_id"`
	RiderID string     `json:"rider_id"`
	Legs    []LegInput `json:"legs"`
}

// Rides is the part of the dispatch service the scheduler drives.
type Rides interface {
	CreateRide(ctx context.Context, in dispatch.CreateRideInput) (dispatch.RideView, error)
	GetRide(ctx context.Context, id string) (dispatch.RideView, error)
	RetryRide(ctx context.Context, id string) (dispatch.RideView, error)
}

// Repo persists legs (write-through); Load returns the legs to resume at boot.
type Repo interface {
	Save(ctx context.Context, l Leg) error
	Load(ctx context.Context) ([]Leg, error)
}

// Options configures a Scheduler. Zero durations take the defaults.
type Options struct {
	Rides      Rides
	Repo       Repo // nil = memory only
	Demo       bool
	FirstDelay time.Duration
	NextDelay  time.Duration
	Lead       time.Duration
	Tick       time.Duration
	RetryEvery time.Duration
	Now        func() time.Time
	Events     func(ctx context.Context, e dispatch.AgentEventInput) // ops feed + agent_events (optional)
	Logger     *log.Logger

	// Geo and Fleet answer GET /v1/supply (optional).
	Geo   geo.Indexer
	Fleet *fleet.Index
}

// Scheduler holds the scheduled legs.
type Scheduler struct {
	o    Options
	mu   sync.Mutex
	legs map[string]*Leg
}

// New builds a Scheduler.
func New(o Options) *Scheduler {
	if o.FirstDelay <= 0 {
		o.FirstDelay = DefaultFirstDelay
	}
	if o.NextDelay <= 0 {
		o.NextDelay = DefaultNextDelay
	}
	if o.Lead <= 0 {
		o.Lead = DefaultLead
	}
	if o.Tick <= 0 {
		o.Tick = DefaultTick
	}
	if o.RetryEvery <= 0 {
		o.RetryEvery = DefaultRetryEvery
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	return &Scheduler{o: o, legs: map[string]*Leg{}}
}

// Demo reports whether demo time compression is on.
func (s *Scheduler) Demo() bool { return s.o.Demo }

func newID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// DispatchTier maps a booked tier onto a fleet tier (pool rides use minis).
func DispatchTier(t string) (string, error) {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "pool" {
		return string(fleet.TierMini), nil
	}
	ft, err := fleet.ParseTier(t)
	if err != nil {
		return "", err
	}
	return string(ft), nil
}

// Restore reloads unfinished legs from the repo (after a restart).
func (s *Scheduler) Restore(ctx context.Context) (int, error) {
	if s.o.Repo == nil {
		return 0, nil
	}
	legs, err := s.o.Repo.Load(ctx)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range legs {
		l := legs[i]
		s.legs[l.ID] = &l
	}
	return len(legs), nil
}

// Schedule stores a confirmed plan. Scheduling the same plan_id again returns
// the legs already stored.
func (s *Scheduler) Schedule(ctx context.Context, in PlanInput) ([]Leg, error) {
	in.RiderID = strings.TrimSpace(in.RiderID)
	in.PlanID = strings.TrimSpace(in.PlanID)
	if in.RiderID == "" {
		return nil, fmt.Errorf("%w: rider_id is required", ErrValidation)
	}
	if len(in.Legs) == 0 || len(in.Legs) > MaxLegs {
		return nil, fmt.Errorf("%w: a plan needs 1..%d legs", ErrValidation, MaxLegs)
	}
	if in.PlanID == "" {
		in.PlanID = newID("pln")
	}
	if existing := s.Plan(in.PlanID); len(existing) > 0 {
		return existing, nil
	}
	now := s.o.Now()
	legs := make([]*Leg, 0, len(in.Legs))
	for i, li := range in.Legs {
		if li.Pickup == nil || li.Dropoff == nil {
			return nil, fmt.Errorf("%w: leg %d: pickup and dropoff are required", ErrValidation, i+1)
		}
		if li.PickupAt.IsZero() {
			return nil, fmt.Errorf("%w: leg %d: pickup_at is required", ErrValidation, i+1)
		}
		tier, err := DispatchTier(li.Tier)
		if err != nil {
			return nil, fmt.Errorf("%w: leg %d: %v", ErrValidation, i+1, err)
		}
		seq := li.Seq
		if seq <= 0 {
			seq = i + 1
		}
		legs = append(legs, &Leg{
			ID: newID("sch"), PlanID: in.PlanID, RiderID: in.RiderID, Seq: seq,
			Passenger: firstNonEmpty(li.Passenger, "me"), PassengerName: li.PassengerName,
			Tier: strings.ToLower(strings.TrimSpace(li.Tier)), DispatchTier: tier,
			Pickup: *li.Pickup, Dropoff: *li.Dropoff, PickupName: li.PickupName, DropoffName: li.DropoffName,
			PickupAt: li.PickupAt, ArriveBy: li.ArriveBy, Fare: li.Fare, DurationMin: li.DurationMin,
			Status: StatusScheduled, DemoMode: s.o.Demo, CreatedAt: now, UpdatedAt: now,
		})
	}
	sort.SliceStable(legs, func(i, j int) bool {
		if !legs[i].PickupAt.Equal(legs[j].PickupAt) {
			return legs[i].PickupAt.Before(legs[j].PickupAt)
		}
		return legs[i].Seq < legs[j].Seq
	})
	for i, l := range legs {
		l.Seq = i + 1
		switch {
		case s.o.Demo && i == 0:
			at := now.Add(s.o.FirstDelay)
			l.DispatchAt = &at
		case s.o.Demo:
			l.Note = "dispatches 60s after the previous leg completes (demo time)"
		default:
			at := l.PickupAt.Add(-s.o.Lead)
			if at.Before(now) {
				at = now
			}
			l.DispatchAt = &at
		}
	}
	s.mu.Lock()
	for _, l := range legs {
		s.legs[l.ID] = l
	}
	s.mu.Unlock()
	out := make([]Leg, len(legs))
	for i, l := range legs {
		s.save(ctx, *l)
		out[i] = *l
	}
	return out, nil
}

func firstNonEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func (s *Scheduler) save(ctx context.Context, l Leg) {
	if s.o.Repo == nil {
		return
	}
	if err := s.o.Repo.Save(ctx, l); err != nil {
		s.o.Logger.Printf("WARN scheduler: persist leg %s: %v", l.ID, err)
	}
}

func sortLegs(out []Leg) {
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		if out[i].PlanID != out[j].PlanID {
			return out[i].PlanID < out[j].PlanID
		}
		return out[i].Seq < out[j].Seq
	})
}

// List returns a rider's legs (all riders when riderID is empty), oldest plan first.
func (s *Scheduler) List(riderID string) []Leg {
	s.mu.Lock()
	out := make([]Leg, 0, len(s.legs))
	for _, l := range s.legs {
		if riderID == "" || l.RiderID == riderID {
			out = append(out, *l)
		}
	}
	s.mu.Unlock()
	sortLegs(out)
	return out
}

// Plan returns one plan's legs in order.
func (s *Scheduler) Plan(planID string) []Leg {
	s.mu.Lock()
	var out []Leg
	for _, l := range s.legs {
		if l.PlanID == planID {
			out = append(out, *l)
		}
	}
	s.mu.Unlock()
	sortLegs(out)
	return out
}

// Run ticks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.o.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick dispatches due legs and follows dispatched ones.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.o.Now()
	s.mu.Lock()
	var due, live []Leg
	for _, l := range s.legs {
		switch {
		case l.Status == StatusScheduled && l.DispatchAt != nil && !now.Before(*l.DispatchAt):
			due = append(due, *l)
		case l.Status == StatusDispatched:
			live = append(live, *l)
		}
	}
	s.mu.Unlock()
	sortLegs(due)
	for _, l := range due {
		s.dispatch(ctx, l, now)
	}
	for _, l := range live {
		s.follow(ctx, l, now)
	}
}

func (s *Scheduler) update(ctx context.Context, id string, fn func(l *Leg)) (Leg, bool) {
	s.mu.Lock()
	l, ok := s.legs[id]
	if !ok {
		s.mu.Unlock()
		return Leg{}, false
	}
	fn(l)
	l.UpdatedAt = s.o.Now()
	cp := *l
	s.mu.Unlock()
	s.save(ctx, cp)
	return cp, true
}

func (s *Scheduler) dispatch(ctx context.Context, l Leg, now time.Time) {
	pickup, dropoff := l.Pickup, l.Dropoff
	view, err := s.o.Rides.CreateRide(ctx, dispatch.CreateRideInput{
		RiderID: l.RiderID, Pickup: &pickup, Dropoff: &dropoff, Tier: l.DispatchTier,
	})
	if err != nil {
		l, _ = s.update(ctx, l.ID, func(x *Leg) {
			x.Status, x.Note = StatusFailed, "could not create the ride: "+err.Error()
			x.FinishedAt = &now
		})
		s.event(ctx, l, fmt.Sprintf("scheduled leg %d of plan %s failed to dispatch", l.Seq, l.PlanID), l.Note)
		s.releaseNext(ctx, l, now)
		return
	}
	l, _ = s.update(ctx, l.ID, func(x *Leg) {
		x.Status, x.RideID, x.RideStatus, x.DriverID = StatusDispatched, view.Ride.ID, string(view.Ride.Status), view.Ride.DriverID
		x.DispatchedAt, x.Note, x.lastRetry = &now, "", now
	})
	driver := "waiting for a driver"
	if view.Match != nil {
		driver = "driver " + view.Ride.DriverID
	}
	s.event(ctx, l, fmt.Sprintf("dispatched scheduled leg %d of plan %s as ride %s (%s)", l.Seq, l.PlanID, l.RideID, driver),
		fmt.Sprintf("%s: %s -> %s, %s, pickup %s.", l.PassengerName, l.PickupName, l.DropoffName, l.Tier,
			l.PickupAt.Format("15:04 MST")))
}

func (s *Scheduler) follow(ctx context.Context, l Leg, now time.Time) {
	view, err := s.o.Rides.GetRide(ctx, l.RideID)
	if errors.Is(err, dispatch.ErrNotFound) {
		l, _ = s.update(ctx, l.ID, func(x *Leg) {
			x.Status, x.Note, x.FinishedAt = StatusCancelled, "ride disappeared", &now
		})
		s.releaseNext(ctx, l, now)
		return
	}
	if err != nil {
		s.o.Logger.Printf("WARN scheduler: follow ride %s: %v", l.RideID, err)
		return
	}
	r := view.Ride
	switch r.Status {
	case store.RideCompleted, store.RideCancelled:
		st := StatusCompleted
		if r.Status == store.RideCancelled {
			st = StatusCancelled
		}
		l, _ = s.update(ctx, l.ID, func(x *Leg) {
			x.Status, x.RideStatus, x.DriverID, x.FinishedAt = st, string(r.Status), r.DriverID, &now
			if r.CancelReason != "" {
				x.Note = r.CancelReason
			}
		})
		s.event(ctx, l, fmt.Sprintf("scheduled leg %d of plan %s %s (ride %s)", l.Seq, l.PlanID, st, l.RideID), "")
		s.releaseNext(ctx, l, now)
	case store.RideRequested:
		if now.Sub(l.lastRetry) >= s.o.RetryEvery {
			s.update(ctx, l.ID, func(x *Leg) { x.lastRetry = now })
			if v, err := s.o.Rides.RetryRide(ctx, l.RideID); err == nil && v.Match != nil {
				s.update(ctx, l.ID, func(x *Leg) { x.RideStatus, x.DriverID = string(v.Ride.Status), v.Ride.DriverID })
			}
		}
	default:
		if string(r.Status) != l.RideStatus || r.DriverID != l.DriverID {
			s.update(ctx, l.ID, func(x *Leg) { x.RideStatus, x.DriverID = string(r.Status), r.DriverID })
		}
	}
}

// releaseNext starts the demo clock for the plan's next waiting leg.
func (s *Scheduler) releaseNext(ctx context.Context, done Leg, now time.Time) {
	if !s.o.Demo {
		return
	}
	var next *Leg
	s.mu.Lock()
	for _, l := range s.legs {
		if l.PlanID == done.PlanID && l.Status == StatusScheduled && l.DispatchAt == nil && l.Seq > done.Seq &&
			(next == nil || l.Seq < next.Seq) {
			next = l
		}
	}
	var id string
	if next != nil {
		id = next.ID
	}
	s.mu.Unlock()
	if id == "" {
		return
	}
	at := now.Add(s.o.NextDelay)
	s.update(ctx, id, func(x *Leg) { x.DispatchAt, x.Note = &at, "" })
}

func (s *Scheduler) event(ctx context.Context, l Leg, decision, reasoning string) {
	if s.o.Events == nil {
		return
	}
	s.o.Events(ctx, dispatch.AgentEventInput{
		Agent: "dispatch_broker", EventType: "scheduled_dispatch", RideID: l.RideID,
		Input:    map[string]any{"plan_id": l.PlanID, "leg_id": l.ID, "seq": l.Seq, "rider_id": l.RiderID},
		Decision: decision, Reasoning: reasoning, Confidence: 1, Kind: "scheduled", DriverID: l.DriverID, Tier: l.DispatchTier,
	})
}
