package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

var t0 = time.Date(2026, 11, 18, 2, 30, 0, 0, time.UTC) // 8:00 AM IST

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeRides struct {
	mu         sync.Mutex
	rides      map[string]*store.Ride
	created    []dispatch.CreateRideInput
	n          int
	createErr  error
	getErr     error
	noMatch    bool
	retries    int
	retryMatch bool
}

func newRides() *fakeRides { return &fakeRides{rides: map[string]*store.Ride{}} }

func (f *fakeRides) CreateRide(_ context.Context, in dispatch.CreateRideInput) (dispatch.RideView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return dispatch.RideView{}, f.createErr
	}
	f.n++
	f.created = append(f.created, in)
	r := &store.Ride{ID: fmt.Sprintf("ride-%d", f.n), RiderID: in.RiderID, Tier: in.Tier, Status: store.RideMatched,
		DriverID: fmt.Sprintf("d_%d", f.n)}
	if f.noMatch {
		r.Status, r.DriverID = store.RideRequested, ""
	}
	f.rides[r.ID] = r
	v := dispatch.RideView{Ride: *r}
	if !f.noMatch {
		v.Match = &dispatch.MatchView{}
	}
	return v, nil
}

func (f *fakeRides) GetRide(_ context.Context, id string) (dispatch.RideView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return dispatch.RideView{}, f.getErr
	}
	r, ok := f.rides[id]
	if !ok {
		return dispatch.RideView{}, fmt.Errorf("%w: ride %s", dispatch.ErrNotFound, id)
	}
	return dispatch.RideView{Ride: *r}, nil
}

func (f *fakeRides) RetryRide(_ context.Context, id string) (dispatch.RideView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries++
	r := f.rides[id]
	if f.retryMatch {
		r.Status, r.DriverID = store.RideMatched, "d_late"
		return dispatch.RideView{Ride: *r, Match: &dispatch.MatchView{}}, nil
	}
	return dispatch.RideView{Ride: *r}, nil
}

func (f *fakeRides) set(id string, st store.RideStatus, reason string) {
	f.mu.Lock()
	f.rides[id].Status, f.rides[id].CancelReason = st, reason
	f.mu.Unlock()
}

func (f *fakeRides) drop(id string) {
	f.mu.Lock()
	delete(f.rides, id)
	f.mu.Unlock()
}

type memRepo struct {
	mu    sync.Mutex
	saved map[string]Leg
	fail  bool
}

func (m *memRepo) Save(_ context.Context, l Leg) error {
	if m.fail {
		return errors.New("db down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saved == nil {
		m.saved = map[string]Leg{}
	}
	m.saved[l.ID] = l
	return nil
}

func (m *memRepo) Load(context.Context) ([]Leg, error) {
	if m.fail {
		return nil, errors.New("db down")
	}
	var out []Leg
	for _, l := range m.saved {
		if !l.Status.Terminal() {
			out = append(out, l)
		}
	}
	return out, nil
}

type events struct {
	mu  sync.Mutex
	got []dispatch.AgentEventInput
}

func (e *events) record(_ context.Context, in dispatch.AgentEventInput) {
	e.mu.Lock()
	e.got = append(e.got, in)
	e.mu.Unlock()
}

func (e *events) decisions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, g := range e.got {
		out = append(out, g.Decision)
	}
	return out
}

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func newSched(demo bool) (*Scheduler, *fakeRides, *clock, *events) {
	c := &clock{t: t0}
	r := newRides()
	ev := &events{}
	s := New(Options{Rides: r, Demo: demo, Now: c.Now, Events: ev.record, Logger: quiet()})
	return s, r, c, ev
}

func pt(lat, lng float64) *geo.Point { return &geo.Point{Lat: lat, Lng: lng} }

// The golden plan: kid school -> soccer at 15:15 IST, me office -> airport arriving 18:00 IST.
func goldenPlan() PlanInput {
	ist := time.FixedZone("IST", 5*3600+1800)
	arrive := time.Date(2026, 11, 18, 18, 0, 0, 0, ist)
	return PlanInput{PlanID: "pln_golden", RiderID: "rider_priya", Legs: []LegInput{
		{Seq: 2, Passenger: "me", PassengerName: "Priya", Tier: "premium", Pickup: pt(13.0475, 77.62),
			Dropoff: pt(13.1989, 77.7068), PickupName: "Office", DropoffName: "Airport",
			PickupAt: time.Date(2026, 11, 18, 17, 1, 0, 0, ist), ArriveBy: &arrive, Fare: 1170},
		{Seq: 1, Passenger: "kid", PassengerName: "Anaya", Tier: "pool", Pickup: pt(12.9352, 77.6265),
			Dropoff: pt(12.969, 77.6005), PickupName: "School", DropoffName: "Soccer",
			PickupAt: time.Date(2026, 11, 18, 15, 15, 0, 0, ist), Fare: 240},
	}}
}

func status(t *testing.T, s *Scheduler) []string {
	t.Helper()
	var out []string
	for _, l := range s.Plan("pln_golden") {
		out = append(out, fmt.Sprintf("%d:%s:%s", l.Seq, l.Passenger, l.Status))
	}
	return out
}

func TestDemoTimeCompression(t *testing.T) {
	s, rides, c, ev := newSched(true)
	ctx := context.Background()
	legs, err := s.Schedule(ctx, goldenPlan())
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by pickup time and renumbered; pool rides dispatch as minis.
	if legs[0].Passenger != "kid" || legs[0].Seq != 1 || legs[0].DispatchTier != "mini" || legs[0].Tier != "pool" {
		t.Fatalf("first leg = %+v", legs[0])
	}
	if !legs[0].DispatchAt.Equal(t0.Add(20*time.Second)) || legs[1].DispatchAt != nil || !legs[0].DemoMode {
		t.Fatalf("demo dispatch times: %v / %v", legs[0].DispatchAt, legs[1].DispatchAt)
	}
	c.Add(19 * time.Second)
	s.Tick(ctx)
	if got := status(t, s); got[0] != "1:kid:scheduled" {
		t.Fatalf("dispatched early: %v", got)
	}
	c.Add(time.Second) // 20 s after confirm
	s.Tick(ctx)
	if got := status(t, s); strings.Join(got, ",") != "1:kid:dispatched,2:me:scheduled" {
		t.Fatalf("after 20s: %v", got)
	}
	first := s.Plan("pln_golden")[0]
	if first.RideID != "ride-1" || first.DriverID != "d_1" || rides.created[0].Tier != "mini" || rides.created[0].RiderID != "rider_priya" {
		t.Fatalf("first ride: %+v %+v", first, rides.created[0])
	}
	// The ride is under way: the next leg keeps waiting.
	rides.set("ride-1", store.RideInProgress, "")
	c.Add(90 * time.Second)
	s.Tick(ctx)
	if got := s.Plan("pln_golden"); got[0].RideStatus != "in_progress" || got[1].DispatchAt != nil {
		t.Fatalf("in progress: %+v", got)
	}
	rides.set("ride-1", store.RideCompleted, "")
	c.Add(10 * time.Second)
	done := c.Now()
	s.Tick(ctx)
	second := s.Plan("pln_golden")[1]
	if got := status(t, s); strings.Join(got, ",") != "1:kid:completed,2:me:scheduled" {
		t.Fatalf("after completion: %v", got)
	}
	if second.DispatchAt == nil || !second.DispatchAt.Equal(done.Add(60*time.Second)) {
		t.Fatalf("next leg dispatch at %v, want completion + 60s", second.DispatchAt)
	}
	c.Add(59 * time.Second)
	s.Tick(ctx)
	if got := status(t, s); got[1] != "2:me:scheduled" {
		t.Fatalf("second leg early: %v", got)
	}
	c.Add(time.Second)
	s.Tick(ctx)
	if got := status(t, s); got[1] != "2:me:dispatched" || rides.created[1].Tier != "premium" {
		t.Fatalf("second leg: %v %+v", got, rides.created)
	}
	rides.set("ride-2", store.RideCancelled, "rider no-show")
	s.Tick(ctx)
	last := s.Plan("pln_golden")[1]
	if last.Status != StatusCancelled || last.Note != "rider no-show" || last.FinishedAt == nil {
		t.Fatalf("cancelled leg: %+v", last)
	}
	if n := len(ev.decisions()); n != 4 { // dispatched, completed, dispatched, cancelled
		t.Fatalf("events: %v", ev.decisions())
	}
	if !strings.HasPrefix(ev.decisions()[0], "dispatched scheduled leg 1 of plan pln_golden as ride ride-1") {
		t.Fatalf("event: %q", ev.decisions()[0])
	}
}

func TestRealTimeDispatchesLeadBeforePickup(t *testing.T) {
	s, _, c, _ := newSched(false)
	ctx := context.Background()
	legs, err := s.Schedule(ctx, goldenPlan())
	if err != nil {
		t.Fatal(err)
	}
	want := legs[0].PickupAt.Add(-DefaultLead)
	if !legs[0].DispatchAt.Equal(want) || !legs[1].DispatchAt.Equal(legs[1].PickupAt.Add(-DefaultLead)) {
		t.Fatalf("dispatch times %v %v", legs[0].DispatchAt, legs[1].DispatchAt)
	}
	c.Add(want.Sub(t0) - time.Second)
	s.Tick(ctx)
	if s.Plan("pln_golden")[0].Status != StatusScheduled {
		t.Fatal("dispatched early")
	}
	c.Add(time.Second)
	s.Tick(ctx)
	if s.Plan("pln_golden")[0].Status != StatusDispatched {
		t.Fatal("not dispatched at pickup - lead")
	}
	// A pickup in the past dispatches now.
	past := goldenPlan()
	past.PlanID = "pln_past"
	past.Legs = past.Legs[:1]
	past.Legs[0].PickupAt = t0.Add(-time.Hour)
	got, _ := s.Schedule(ctx, past)
	if !got[0].DispatchAt.Equal(c.Now()) {
		t.Fatalf("past pickup dispatch at %v", got[0].DispatchAt)
	}
}

func TestScheduleValidationAndIdempotency(t *testing.T) {
	s, _, _, _ := newSched(true)
	ctx := context.Background()
	bad := []PlanInput{
		{RiderID: "", Legs: goldenPlan().Legs},
		{RiderID: "r", Legs: nil},
		{RiderID: "r", Legs: make([]LegInput, MaxLegs+1)},
		{RiderID: "r", Legs: []LegInput{{Tier: "mini", PickupAt: t0}}},
		{RiderID: "r", Legs: []LegInput{{Tier: "mini", Pickup: pt(1, 1), Dropoff: pt(1, 2)}}},
		{RiderID: "r", Legs: []LegInput{{Tier: "jet", Pickup: pt(1, 1), Dropoff: pt(1, 2), PickupAt: t0}}},
	}
	for i, in := range bad {
		if _, err := s.Schedule(ctx, in); !errors.Is(err, ErrValidation) {
			t.Errorf("case %d: err = %v", i, err)
		}
	}
	one, err := s.Schedule(ctx, goldenPlan())
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.Schedule(ctx, goldenPlan())
	if len(again) != 2 || again[0].ID != one[0].ID {
		t.Fatalf("not idempotent: %v vs %v", again, one)
	}
	anon, err := s.Schedule(ctx, PlanInput{RiderID: "r2", Legs: []LegInput{{Pickup: pt(1, 1), Dropoff: pt(1, 2),
		PickupAt: t0, Tier: "Sedan"}}})
	if err != nil || !strings.HasPrefix(anon[0].PlanID, "pln_") || anon[0].Passenger != "me" || anon[0].Seq != 1 {
		t.Fatalf("anonymous plan: %+v %v", anon, err)
	}
	if n := len(s.List("")); n != 3 {
		t.Fatalf("list all = %d", n)
	}
	if n := len(s.List("rider_priya")); n != 2 {
		t.Fatalf("list rider = %d", n)
	}
	if !s.Demo() {
		t.Fatal("demo flag")
	}
}

func TestFailedDispatchReleasesNextLeg(t *testing.T) {
	s, rides, c, ev := newSched(true)
	ctx := context.Background()
	_, _ = s.Schedule(ctx, goldenPlan())
	rides.createErr = errors.New("no fleet")
	c.Add(20 * time.Second)
	s.Tick(ctx)
	legs := s.Plan("pln_golden")
	if legs[0].Status != StatusFailed || !strings.Contains(legs[0].Note, "no fleet") || legs[1].DispatchAt == nil {
		t.Fatalf("after failure: %+v", legs)
	}
	if !strings.Contains(ev.decisions()[0], "failed to dispatch") {
		t.Fatalf("event %v", ev.decisions())
	}
}

func TestWaitingRideIsRetriedAndVanishedRideCancels(t *testing.T) {
	s, rides, c, _ := newSched(true)
	ctx := context.Background()
	_, _ = s.Schedule(ctx, goldenPlan())
	rides.noMatch = true
	c.Add(20 * time.Second)
	s.Tick(ctx)
	if l := s.Plan("pln_golden")[0]; l.RideStatus != "requested" || l.DriverID != "" {
		t.Fatalf("unmatched: %+v", l)
	}
	c.Add(2 * time.Second)
	s.Tick(ctx)
	if rides.retries != 0 {
		t.Fatal("retried too soon")
	}
	c.Add(3 * time.Second)
	s.Tick(ctx)
	if rides.retries != 1 {
		t.Fatalf("retries = %d", rides.retries)
	}
	rides.retryMatch = true
	c.Add(5 * time.Second)
	s.Tick(ctx)
	if l := s.Plan("pln_golden")[0]; l.RideStatus != "matched" || l.DriverID != "d_late" {
		t.Fatalf("after retry: %+v", l)
	}
	rides.getErr = errors.New("db blip")
	s.Tick(ctx) // logged, nothing changes
	rides.getErr = nil
	rides.drop("ride-1")
	s.Tick(ctx)
	if l := s.Plan("pln_golden")[0]; l.Status != StatusCancelled || l.Note != "ride disappeared" {
		t.Fatalf("vanished: %+v", l)
	}
}

func TestRepoWriteThroughAndRestore(t *testing.T) {
	repo := &memRepo{}
	c := &clock{t: t0}
	rides := newRides()
	s := New(Options{Rides: rides, Repo: repo, Demo: true, Now: c.Now, Logger: quiet()})
	ctx := context.Background()
	_, _ = s.Schedule(ctx, goldenPlan())
	c.Add(20 * time.Second)
	s.Tick(ctx)
	if len(repo.saved) != 2 {
		t.Fatalf("saved %d", len(repo.saved))
	}
	restarted := New(Options{Rides: rides, Repo: repo, Demo: true, Now: c.Now, Logger: quiet()})
	n, err := restarted.Restore(ctx)
	if err != nil || n != 2 || restarted.Plan("pln_golden")[0].RideID != "ride-1" {
		t.Fatalf("restore: %d %v %+v", n, err, restarted.Plan("pln_golden"))
	}
	if n, err := New(Options{Rides: rides}).Restore(ctx); n != 0 || err != nil {
		t.Fatal("restore without repo")
	}
	repo.fail = true
	if _, err := restarted.Restore(ctx); err == nil {
		t.Fatal("restore error swallowed")
	}
	if _, err := restarted.Schedule(ctx, PlanInput{RiderID: "r", Legs: []LegInput{{Pickup: pt(1, 1),
		Dropoff: pt(1, 2), PickupAt: t0, Tier: "mini"}}}); err != nil {
		t.Fatal("a failing repo must not fail scheduling")
	}
}

func TestRunStopsWithContext(t *testing.T) {
	rides := newRides()
	s := New(Options{Rides: rides, Demo: true, Tick: time.Millisecond, FirstDelay: time.Millisecond, Logger: quiet()})
	_, _ = s.Schedule(context.Background(), goldenPlan())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for s.Plan("pln_golden")[0].Status != StatusDispatched {
		select {
		case <-deadline:
			t.Fatal("never dispatched")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if d := New(Options{}); d.o.FirstDelay != DefaultFirstDelay || d.o.NextDelay != DefaultNextDelay || d.o.Now == nil {
		t.Fatal("defaults")
	}
}

func TestDispatchTier(t *testing.T) {
	for in, want := range map[string]string{"pool": "mini", " AV ": "av", "premium": "premium", "xl": "xl"} {
		if got, err := DispatchTier(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v", in, got, err)
		}
	}
	if _, err := DispatchTier("boat"); err == nil {
		t.Fatal("boat accepted")
	}
	if !StatusFailed.Terminal() || StatusDispatched.Terminal() {
		t.Fatal("terminal")
	}
}
