package sim

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

var center = geo.Point{Lat: 12.9716, Lng: 77.5946}

// ---- harness ----

type published struct {
	topic string
	data  any
}

type recorder struct {
	mu   sync.Mutex
	msgs []published
}

func (r *recorder) Publish(topic string, data any) {
	r.mu.Lock()
	r.msgs = append(r.msgs, published{topic, data})
	r.mu.Unlock()
}

func (r *recorder) all() []published {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]published(nil), r.msgs...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.msgs = nil
	r.mu.Unlock()
}

func (r *recorder) agentEvents(agent string) []stream.AgentEvent {
	var out []stream.AgentEvent
	for _, m := range r.all() {
		if ev, ok := m.data.(stream.AgentEvent); ok && m.topic == stream.TopicOps && (agent == "" || ev.Agent == agent) {
			out = append(out, ev)
		}
	}
	return out
}

// rideStatuses returns the distinct consecutive statuses published on ride:<id>.
func (r *recorder) rideStatuses(id string) []string {
	var out []string
	for _, m := range r.all() {
		if m.topic != stream.RideTopic(id) {
			continue
		}
		u := m.data.(stream.RideUpdate)
		if len(out) == 0 || out[len(out)-1] != u.Status {
			out = append(out, u.Status)
		}
	}
	return out
}

func (r *recorder) lastDrivers() []stream.Driver {
	msgs := r.all()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].topic == stream.TopicDrivers {
			return msgs[i].data.([]stream.Driver)
		}
	}
	return nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	sim   *Sim
	svc   *dispatch.Service
	mem   *store.Memory
	pub   *recorder
	clock *clock
}

// noon IST on a weekday: time-of-day factor is close to 1.
var noonIST = time.Date(2026, 11, 16, 6, 30, 0, 0, time.UTC)

func newHarness(t *testing.T, drivers int, mutate ...func(*Options)) *harness {
	t.Helper()
	mem := store.NewMemory()
	pub := &recorder{}
	svc, err := dispatch.New(dispatch.Options{
		H3Resolution: 9, Center: center, Store: mem, Telemetry: mem,
		Registry: prometheus.NewRegistry(), Logger: log.New(io.Discard, "", 0), Publisher: pub,
		MatchFeedInterval: -1, // the harness counts every dispatch event
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: noonIST}
	o := Options{
		Service: svc, Publisher: pub, Drivers: drivers, DemandPerMin: 6, Center: center,
		Seed: 42, Now: clk.now, Logger: log.New(io.Discard, "", 0), RandomSpikesOff: true,
	}
	for _, m := range mutate {
		m(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{sim: s, svc: svc, mem: mem, pub: pub, clock: clk}
}

func (h *harness) step(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		h.clock.advance(time.Second)
		h.sim.Step(ctx)
	}
}

// requestRideFor books a ride whose pickup is ~200 m from driver d, so d wins the match.
func (h *harness) requestRideFor(t *testing.T, d fleet.Driver, tripKm float64) dispatch.RideView {
	t.Helper()
	pickup := geo.Point{Lat: d.Lat + 0.0018, Lng: d.Lng}
	drop := geo.Point{Lat: pickup.Lat, Lng: pickup.Lng + tripKm/(111.32*math.Cos(pickup.Lat*math.Pi/180))}
	view, err := h.svc.CreateRide(context.Background(), dispatch.CreateRideInput{
		RiderID: "rider-test", Pickup: &pickup, Dropoff: &drop, Tier: string(d.Tier),
	})
	if err != nil || view.Match == nil {
		t.Fatalf("create ride: %v %+v", err, view)
	}
	return view
}

func (h *harness) anyDriver(t *testing.T) fleet.Driver {
	t.Helper()
	d, ok := h.svc.Fleet().Get(DriverID(1))
	if !ok {
		t.Fatal("driver d_001 not spawned")
	}
	return d
}

// runUntil steps until cond holds (max n ticks) and reports whether it did.
func (h *harness) runUntil(ctx context.Context, n int, cond func() bool) bool {
	for i := 0; i < n; i++ {
		if cond() {
			return true
		}
		h.step(ctx, 1)
	}
	return cond()
}

// ---- spawning ----

func TestSpawnFleetMixAndPositions(t *testing.T) {
	h := newHarness(t, 300)
	h.sim.Spawn(context.Background())
	c := h.svc.Fleet().Counts()
	if c.Total != 300 || c.Idle != 300 {
		t.Fatalf("fleet counts %+v", c)
	}
	av := 0
	for i := 1; i <= 300; i++ {
		d, ok := h.svc.Fleet().Get(DriverID(i))
		if !ok {
			t.Fatalf("missing %s", DriverID(i))
		}
		if km := geo.HaversineKm(center, d.Point()); km > cityRadiusKm {
			t.Errorf("%s spawned %.1f km from centre", d.ID, km)
		}
		if d.Tier == fleet.TierAV {
			av++
			if d.BatteryPct <= 0 {
				t.Errorf("AV %s has no battery", d.ID)
			}
		}
	}
	if av != 45 { // exactly 15%
		t.Fatalf("%d AVs, want 45", av)
	}
	if DriverID(123) != "d_123" {
		t.Fatalf("driver id format %q", DriverID(123))
	}
}

func TestStepPublishesDriversInContractShape(t *testing.T) {
	h := newHarness(t, 20)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	h.step(ctx, 1)
	ds := h.pub.lastDrivers()
	if len(ds) != 20 {
		t.Fatalf("published %d drivers", len(ds))
	}
	for _, d := range ds {
		if !strings.HasPrefix(d.ID, "d_") || d.Status == "" || d.Tier == "" {
			t.Fatalf("driver %+v", d)
		}
		if (d.Tier == "av") != (d.Battery != nil) {
			t.Fatalf("battery must be set for AVs only: %+v", d)
		}
	}
}

func TestIdleDriversWander(t *testing.T) {
	h := newHarness(t, 30)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	start := map[string]geo.Point{}
	for i := 1; i <= 30; i++ {
		d, _ := h.svc.Fleet().Get(DriverID(i))
		start[d.ID] = d.Point()
	}
	h.step(ctx, 120)
	moved := 0
	for id, p := range start {
		d, _ := h.svc.Fleet().Get(id)
		if geo.HaversineKm(p, d.Point()) > 0.05 {
			moved++
		}
		if d.Status != fleet.StatusIdle {
			t.Fatalf("%s is %s without a ride", id, d.Status)
		}
	}
	if moved < 10 {
		t.Fatalf("only %d of 30 idle drivers moved in 2 minutes", moved)
	}
}

// ---- ride lifecycle ----

func TestRideLifecycleAcceptedArrivingOnTripCompleted(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	d := h.anyDriver(t)
	view := h.requestRideFor(t, d, 1.5)
	id, driverID := view.Ride.ID, view.Match.Driver.ID

	var etas []int
	done := h.runUntil(ctx, 1200, func() bool {
		for _, m := range h.pub.all() {
			if m.topic == stream.RideTopic(id) && m.data.(stream.RideUpdate).Status == "completed" {
				return true
			}
		}
		return false
	})
	if !done {
		t.Fatalf("ride never completed; statuses %v", h.pub.rideStatuses(id))
	}
	got := h.pub.rideStatuses(id)
	want := []string{"accepted", "arriving", "on_trip", "completed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ride statuses %v, want %v", got, want)
	}
	for _, m := range h.pub.all() {
		if m.topic != stream.RideTopic(id) {
			continue
		}
		u := m.data.(stream.RideUpdate)
		if u.RideID != id || u.Driver.ID != driverID || u.ETASec < 0 {
			t.Fatalf("ride update %+v", u)
		}
		if u.Status == "on_trip" {
			etas = append(etas, u.ETASec)
		}
		if u.Status == "completed" && u.ETASec != 0 {
			t.Fatalf("completed eta %d", u.ETASec)
		}
	}
	if len(etas) < 2 || etas[len(etas)-1] >= etas[0] {
		t.Fatalf("on_trip ETA should count down: %v", etas)
	}
	r, _ := h.mem.GetRide(ctx, id)
	if r.Status != store.RideCompleted {
		t.Fatalf("stored ride %s", r.Status)
	}
	drv, _ := h.svc.Fleet().Get(driverID)
	if drv.Status != fleet.StatusIdle {
		t.Fatalf("driver %s after completion", drv.Status)
	}
	if km := geo.HaversineKm(drv.Point(), r.Dropoff); km > 0.05 {
		t.Fatalf("driver finished %.3f km from dropoff", km)
	}
}

func TestDriverStatusTracksRide(t *testing.T) {
	h := newHarness(t, 3)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	view := h.requestRideFor(t, h.anyDriver(t), 2)
	driverID := view.Match.Driver.ID
	sawOnTrip := h.runUntil(ctx, 600, func() bool {
		d, _ := h.svc.Fleet().Get(driverID)
		return d.Status == fleet.StatusOnTrip
	})
	if !sawOnTrip {
		t.Fatal("driver never went on_trip")
	}
	r, _ := h.mem.GetRide(ctx, view.Ride.ID)
	if r.Status != store.RideInProgress {
		t.Fatalf("ride %s while driver on_trip", r.Status)
	}
	for _, d := range h.pub.lastDrivers() {
		if d.ID == driverID && d.Status != "on_trip" {
			t.Fatalf("published status %s", d.Status)
		}
	}
}

func TestCancelledRideReleasesSimDriver(t *testing.T) {
	h := newHarness(t, 3)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	view := h.requestRideFor(t, h.anyDriver(t), 3)
	h.step(ctx, 2)
	if _, err := h.svc.CancelRide(ctx, view.Ride.ID, "changed mind"); err != nil {
		t.Fatal(err)
	}
	h.step(ctx, 30)
	d, _ := h.svc.Fleet().Get(view.Match.Driver.ID)
	if d.Status != fleet.StatusIdle {
		t.Fatalf("driver %s after cancel", d.Status)
	}
	if h.sim.ActiveRides() != 0 {
		t.Fatalf("%d active sim rides after cancel", h.sim.ActiveRides())
	}
}

// blockingRouter never answers, like an OSRM server that hangs.
type blockingRouter struct{}

func (blockingRouter) Route(ctx context.Context, _, _ geo.Point) ([]geo.Point, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type failingRouter struct{}

func (failingRouter) Route(context.Context, geo.Point, geo.Point) ([]geo.Point, error) {
	return nil, errors.New("osrm down")
}

func TestSlowRouterFallsBackToStraightLine(t *testing.T) {
	for name, r := range map[string]Router{"slow": blockingRouter{}, "failing": failingRouter{}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 2, func(o *Options) { o.Router = r; o.RouteWait = 3 * time.Second })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.sim.Spawn(ctx)
			view := h.requestRideFor(t, h.anyDriver(t), 1)
			ok := h.runUntil(ctx, 8, func() bool {
				s := h.pub.rideStatuses(view.Ride.ID)
				return len(s) > 0 && s[len(s)-1] == "arriving"
			})
			if !ok {
				t.Fatalf("no fallback: statuses %v", h.pub.rideStatuses(view.Ride.ID))
			}
			done := h.runUntil(ctx, 900, func() bool {
				r, _ := h.mem.GetRide(ctx, view.Ride.ID)
				return r.Status == store.RideCompleted
			})
			if !done {
				t.Fatal("ride did not complete on straight-line fallback")
			}
		})
	}
}

// ---- telemetry batching ----

type countingTelemetry struct {
	mu      sync.Mutex
	batches [][]store.TelemetryPoint
}

func (c *countingTelemetry) AppendTelemetry(ctx context.Context, p store.TelemetryPoint) error {
	return c.AppendTelemetryBatch(ctx, []store.TelemetryPoint{p})
}

func (c *countingTelemetry) AppendTelemetryBatch(_ context.Context, ps []store.TelemetryPoint) error {
	c.mu.Lock()
	c.batches = append(c.batches, append([]store.TelemetryPoint(nil), ps...))
	c.mu.Unlock()
	return nil
}

func (c *countingTelemetry) RecentTelemetry(context.Context, string, int) ([]store.TelemetryPoint, error) {
	return nil, nil
}

func TestTelemetryIsBatched(t *testing.T) {
	ct := &countingTelemetry{}
	b := store.NewTelemetryBatcher(ct, store.BatcherOptions{Logger: log.New(io.Discard, "", 0)})
	h := newHarness(t, 10, func(o *Options) { o.Telemetry = b })
	ctx := context.Background()
	h.sim.Spawn(ctx)
	h.step(ctx, 10)
	ct.mu.Lock()
	n := len(ct.batches)
	ct.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d DB writes before flush; telemetry must be batched", n)
	}
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if len(ct.batches) != 1 {
		t.Fatalf("%d batches, want 1", len(ct.batches))
	}
	if got := len(ct.batches[0]); got == 0 || got > 10 {
		t.Fatalf("batch of %d points; want at most one per driver", got)
	}
}

// ---- demand ----

func TestTimeOfDayCurve(t *testing.T) {
	ist := time.FixedZone("IST", 19800)
	at := func(h int) float64 { return TimeOfDayFactor(time.Date(2026, 11, 16, h, 0, 0, 0, ist)) }
	if at(9) <= at(13) || at(18) <= at(13) {
		t.Fatalf("peaks: 9h %.2f, 13h %.2f, 18h %.2f", at(9), at(13), at(18))
	}
	if at(3) >= 0.5 {
		t.Fatalf("night factor %.2f should be low", at(3))
	}
	sum := 0.0
	for hr := 0; hr < 24; hr++ {
		sum += at(hr)
	}
	if mean := sum / 24; mean < 0.9 || mean > 1.1 {
		t.Fatalf("daily mean factor %.2f, want ~1", mean)
	}
}

func TestDemandTickCreatesRequestsAndExpiresUnmatched(t *testing.T) {
	h := newHarness(t, 0, func(o *Options) { o.DemandPerMin = 600 })
	ctx := context.Background()
	h.sim.DemandTick(ctx) // no drivers spawned: every request stays open
	m, _ := h.svc.Metrics(ctx)
	if m.OpenRequests < 3 {
		t.Fatalf("open requests %d after one tick at 600/min", m.OpenRequests)
	}
	h.clock.advance(UnmatchedTimeout + time.Second)
	h.sim.SetDemandPerMin(0)
	h.sim.DemandTick(ctx)
	m, _ = h.svc.Metrics(ctx)
	if m.OpenRequests != 0 {
		t.Fatalf("open requests %d after timeout", m.OpenRequests)
	}
}

func TestPoissonMean(t *testing.T) {
	h := newHarness(t, 0)
	total := 0
	for i := 0; i < 2000; i++ {
		total += h.sim.poisson(2.5)
	}
	if mean := float64(total) / 2000; math.Abs(mean-2.5) > 0.2 {
		t.Fatalf("poisson mean %.2f", mean)
	}
}

// ---- scenarios ----

func TestUnknownScenario(t *testing.T) {
	h := newHarness(t, 1)
	if _, err := h.sim.TriggerScenario(context.Background(), "earthquake"); !errors.Is(err, ErrUnknownScenario) {
		t.Fatalf("err %v", err)
	}
}

func TestRushHourScenario(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()
	base := h.sim.DemandRate()
	v, err := h.sim.TriggerScenario(ctx, "rush_hour")
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "rush_hour" || !v.ActiveUntil.After(h.clock.now()) {
		t.Fatalf("view %+v", v)
	}
	if got := h.sim.DemandRate(); got < 2*base {
		t.Fatalf("rush hour demand %.1f, base %.1f", got, base)
	}
	if h.sim.SpeedFactor() >= 1 {
		t.Fatal("rush hour should slow traffic")
	}
	if len(h.pub.agentEvents("fleet_simulator")) != 1 {
		t.Fatal("scenario start should be announced on ops")
	}
	h.clock.advance(ScenarioDuration + time.Second)
	h.sim.Step(ctx)
	if got := h.sim.DemandRate(); math.Abs(got-base) > 1e-9 {
		t.Fatalf("demand %.2f after expiry, want %.2f", got, base)
	}
	if h.sim.SpeedFactor() != 1 {
		t.Fatalf("speed factor %.2f after expiry", h.sim.SpeedFactor())
	}
}

func TestRainStormScenario(t *testing.T) {
	h := newHarness(t, 5)
	base := h.sim.DemandRate()
	if _, err := h.sim.TriggerScenario(context.Background(), "rain_storm"); err != nil {
		t.Fatal(err)
	}
	if f := h.sim.SpeedFactor(); f > 0.7 {
		t.Fatalf("rain speed factor %.2f, want <= 0.7", f)
	}
	if h.sim.DemandRate() <= base*1.5 {
		t.Fatalf("rain demand %.1f vs base %.1f", h.sim.DemandRate(), base)
	}
}

func TestConcertExitScenario(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	base := h.sim.DemandRate()
	v, err := h.sim.TriggerScenario(ctx, "concert_exit")
	if err != nil {
		t.Fatal(err)
	}
	if geo.HaversineKm(v.Location, ConcertVenue) > 0.01 {
		t.Fatalf("spike at %+v", v.Location)
	}
	if h.sim.DemandRate() <= base+20 {
		t.Fatalf("concert demand %.1f vs base %.1f", h.sim.DemandRate(), base)
	}
	// The exit burst is requested right away, all near the venue.
	m, _ := h.svc.Metrics(ctx)
	if m.OpenRequests < ConcertBurst {
		t.Fatalf("open requests %d, want >= %d", m.OpenRequests, ConcertBurst)
	}
	near := 0
	for _, ev := range h.pub.agentEvents("dispatch_broker") {
		if strings.Contains(ev.Decision, "no driver") {
			near++
		}
	}
	if near < ConcertBurst {
		t.Fatalf("%d dispatch events for the burst", near)
	}
}

func TestSafetyIncidentNeedsOnTripVehicle(t *testing.T) {
	h := newHarness(t, 3)
	h.sim.Spawn(context.Background())
	if _, err := h.sim.TriggerScenario(context.Background(), "safety_incident"); !errors.Is(err, ErrNoOnTripVehicle) {
		t.Fatalf("err %v", err)
	}
}

// offRouteKm is the distance from p to segment a-b (local flat projection).
func offRouteKm(p, a, b geo.Point) float64 {
	k := 111.32 * math.Cos(a.Lat*math.Pi/180)
	px, py := (p.Lng-a.Lng)*k, (p.Lat-a.Lat)*110.57
	bx, by := (b.Lng-a.Lng)*k, (b.Lat-a.Lat)*110.57
	t := (px*bx + py*by) / (bx*bx + by*by)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-t*bx, py-t*by)
}

func TestSafetyIncidentDeviatesAndBrakesHard(t *testing.T) {
	var det *safety.Service
	h := newHarness(t, 3, func(o *Options) {
		det = safety.New(safety.Options{Config: safety.DefaultConfig(true), Events: o.Service.RecordAgentEvent,
			Logger: log.New(io.Discard, "", 0)})
		o.Safety = det
	})
	ctx := context.Background()
	h.sim.Spawn(ctx)
	view := h.requestRideFor(t, h.anyDriver(t), 6)
	driverID := view.Match.Driver.ID
	if !h.runUntil(ctx, 300, func() bool {
		d, _ := h.svc.Fleet().Get(driverID)
		return d.Status == fleet.StatusOnTrip
	}) {
		t.Fatal("never on trip")
	}
	h.step(ctx, 20) // cruising
	h.pub.reset()

	v, err := h.sim.TriggerScenario(ctx, "safety_incident")
	if err != nil {
		t.Fatal(err)
	}
	if v.DriverID != driverID || v.RideID != view.Ride.ID {
		t.Fatalf("incident view %+v", v)
	}

	maxOff, sawStop, sawIncident := 0.0, false, false
	prevSpeed := -1.0
	hardBrake := false
	for i := 0; i < 90; i++ {
		h.step(ctx, 1)
		d, _ := h.svc.Fleet().Get(driverID)
		maxOff = math.Max(maxOff, offRouteKm(d.Point(), view.Ride.Pickup, view.Ride.Dropoff))
		if prevSpeed >= 30 && d.SpeedKmh == 0 {
			hardBrake = true // ≥30 km/h to standstill in one 1 s tick
		}
		if d.SpeedKmh == 0 {
			sawStop = true
		}
		prevSpeed = d.SpeedKmh
		for _, pd := range h.pub.lastDrivers() {
			if pd.ID == driverID && pd.Status == "incident" {
				sawIncident = true
			}
		}
	}
	if maxOff < 0.3 {
		t.Fatalf("max deviation %.0f m, want >= 300 m", maxOff*1000)
	}
	if !sawStop || !hardBrake {
		t.Fatalf("hard brake not observed (stop=%v brake=%v)", sawStop, hardBrake)
	}
	if !sawIncident {
		t.Fatal(`driver never published with status "incident"`)
	}
	evs := h.pub.agentEvents("safety_sentinel")
	if len(evs) != 1 || !strings.Contains(evs[0].Decision, driverID) || !strings.Contains(evs[0].Decision, "route deviation") {
		t.Fatalf("want exactly one de-duplicated safety signal event, got %+v", evs)
	}
	sigs := det.Signals()
	if len(sigs) != 1 || sigs[0].RideID != view.Ride.ID || sigs[0].RiderID != "rider-test" || len(sigs[0].PlannedRoute) < 2 {
		t.Fatalf("signals %+v", sigs)
	}
	stored, _ := h.mem.RecentEvents(ctx, 50)
	found := false
	for _, e := range stored {
		found = found || e.Agent == "safety_sentinel"
	}
	if !found {
		t.Fatal("safety event not written to agent_events")
	}

	// The vehicle recovers and finishes the trip.
	if !h.runUntil(ctx, 1500, func() bool {
		r, _ := h.mem.GetRide(ctx, view.Ride.ID)
		return r.Status == store.RideCompleted
	}) {
		t.Fatal("ride did not complete after the incident")
	}
}

// ---- config and HTTP ----

func TestConfigureResizesFleetAndDemand(t *testing.T) {
	h := newHarness(t, 10)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	n, rate := 25, 12.0
	v, err := h.sim.Configure(ctx, ConfigInput{Drivers: &n, DemandPerMin: &rate})
	if err != nil {
		t.Fatal(err)
	}
	h.step(ctx, 1)
	if h.svc.Fleet().Counts().Total != 25 || v.Drivers != 25 || v.DemandPerMin != 12 {
		t.Fatalf("after grow: fleet %d view %+v", h.svc.Fleet().Counts().Total, v)
	}
	n = 5
	if _, err := h.sim.Configure(ctx, ConfigInput{Drivers: &n}); err != nil {
		t.Fatal(err)
	}
	h.step(ctx, 1)
	if got := h.svc.Fleet().Counts().Total; got != 5 {
		t.Fatalf("after shrink: fleet %d", got)
	}
	lat, lng := 12.93, 77.62
	base := h.sim.DemandRate()
	if _, err := h.sim.Configure(ctx, ConfigInput{SpikeLat: &lat, SpikeLng: &lng}); err != nil {
		t.Fatal(err)
	}
	if h.sim.DemandRate() <= base {
		t.Fatal("spike did not raise demand")
	}

	bad := []ConfigInput{
		{Drivers: ptrInt(-1)},
		{Drivers: ptrInt(MaxDrivers + 1)},
		{DemandPerMin: ptrF(-1)},
		{SpikeLat: &lat},
		{SpikeLat: ptrF(95), SpikeLng: &lng},
	}
	for _, in := range bad {
		if _, err := h.sim.Configure(ctx, in); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%+v: err %v", in, err)
		}
	}
}

func ptrInt(i int) *int       { return &i }
func ptrF(f float64) *float64 { return &f }

func TestHTTPEndpoints(t *testing.T) {
	h := newHarness(t, 3)
	h.sim.Spawn(context.Background())
	r := chi.NewRouter()
	h.sim.Routes(r)

	do := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := do(http.MethodPost, "/v1/sim/scenario/rush_hour", ""); code != http.StatusOK || !strings.Contains(body, `"name":"rush_hour"`) {
		t.Fatalf("rush_hour: %d %s", code, body)
	}
	if code, _ := do(http.MethodPost, "/v1/sim/scenario/earthquake", ""); code != http.StatusNotFound {
		t.Fatalf("unknown scenario: %d", code)
	}
	if code, _ := do(http.MethodPost, "/v1/sim/scenario/safety_incident", ""); code != http.StatusConflict {
		t.Fatalf("safety without trips: %d", code)
	}
	code, body := do(http.MethodPost, "/v1/sim/config", `{"drivers":8,"demand_per_min":3.5,"spike_lat":12.95,"spike_lng":77.6}`)
	if code != http.StatusOK || !strings.Contains(body, `"drivers":8`) || !strings.Contains(body, `"demand_per_min":3.5`) {
		t.Fatalf("config: %d %s", code, body)
	}
	if code, _ := do(http.MethodPost, "/v1/sim/config", `{"drivers":"many"}`); code != http.StatusBadRequest {
		t.Fatalf("bad config: %d", code)
	}
	if code, _ := do(http.MethodPost, "/v1/sim/config", `{"spike_lat":12.9}`); code != http.StatusBadRequest {
		t.Fatalf("half spike: %d", code)
	}
	if code, body := do(http.MethodGet, "/v1/sim/config", ""); code != http.StatusOK || !strings.Contains(body, `"running"`) {
		t.Fatalf("get config: %d %s", code, body)
	}
}

func TestDisabledRoutesReport503(t *testing.T) {
	r := chi.NewRouter()
	DisabledRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/v1/sim/scenario/rush_hour", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(t, 5, func(o *Options) { o.Now = nil; o.Tick = 10 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.sim.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for h.svc.Fleet().Counts().Total != 5 || len(h.pub.lastDrivers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("simulator did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if h.sim.Status().Running {
		t.Fatal("status should report stopped")
	}
}

// ---- cold start ----

func TestWarmupPutsVehiclesOnTripWithinTwoMinutes(t *testing.T) {
	ctx := context.Background()
	onTrip := func(h *harness) int {
		n := 0
		for _, d := range h.pub.lastDrivers() {
			if d.Status == string(fleet.StatusOnTrip) {
				n++
			}
		}
		return n
	}
	run := func(warm bool) (*harness, int) {
		h := newHarness(t, 200, func(o *Options) { o.DemandPerMin = 2 })
		h.sim.Spawn(ctx)
		if warm {
			h.sim.StartWarmup(h.clock.now())
		}
		for i := 0; i < 120; i++ { // two minutes of 1 s ticks
			h.sim.DemandTick(ctx)
			h.step(ctx, 1)
		}
		return h, onTrip(h)
	}
	h, warm := run(true)
	_, cold := run(false)
	t.Logf("on trip after 2 min: %d with the ramp, %d without", warm, cold)
	if warm < 20 {
		t.Fatalf("after 2 min with the ramp only %d of 200 vehicles are on a trip (without it: %d)", warm, cold)
	}
	if warm <= 2*cold {
		t.Errorf("ramp should clearly beat steady demand: %d vs %d on trip", warm, cold)
	}
	if h.sim.Warming(h.clock.now()) {
		t.Error("the ramp must end after WarmupWindow")
	}
	// Bounded: the ramp aims at WarmupBusyShare, never the whole fleet.
	if busy := h.sim.ActiveRides(); busy > int(0.45*200) {
		t.Errorf("ramp overshot: %d active rides", busy)
	}
	// Once the window is over it adds nothing.
	h.sim.mu.Lock()
	extra := h.sim.warmupRequests(h.clock.now())
	h.sim.mu.Unlock()
	if len(extra) != 0 {
		t.Errorf("%d warm-up requests after the window", len(extra))
	}
}
