package sim

import (
	"context"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

func TestHotspotsAreTheSixBengaluruAreas(t *testing.T) {
	var names []string
	for _, h := range baseHotspots {
		names = append(names, h.name)
	}
	sort.Strings(names)
	want := "Electronic City,Indiranagar,Kempegowda Airport,Koramangala,MG Road,Whitefield"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("hotspots %s, want %s", got, want)
	}
	for _, h := range baseHotspots {
		if km := geo.HaversineKm(center, h.at); km > cityRadiusKm-2 {
			t.Errorf("%s is %.1f km out, beyond the city radius", h.name, km)
		}
	}
	// Palace Grounds, not a stadium, hosts the concert.
	if km := geo.HaversineKm(ConcertVenue, geo.Point{Lat: 12.9980, Lng: 77.5920}); km > 0.5 {
		t.Errorf("concert venue %.2f km from Palace Grounds", km)
	}
}

func TestPickupsConcentrateAroundHotspots(t *testing.T) {
	h := newHarness(t, 0)
	near := 0
	const n = 4000
	for range n {
		p := h.sim.randomPickup(10)
		for _, hs := range h.sim.hotspots {
			if geo.HaversineKm(p, hs.at) <= 3 {
				near++
				break
			}
		}
	}
	if share := float64(near) / n; share < 0.75 {
		t.Fatalf("only %.0f%% of pickups within 3 km of a hotspot", share*100)
	}
}

func TestDemoCurveIsDampedButShaped(t *testing.T) {
	h := newHarness(t, 0)
	ist := time.FixedZone("IST", 19800)
	at := func(hr int) float64 { return h.sim.curve(time.Date(2026, 11, 16, hr, 0, 0, 0, ist)) }
	if at(18) <= at(13) || at(3) >= at(13) {
		t.Fatalf("curve lost its shape: 3h %.2f 13h %.2f 18h %.2f", at(3), at(13), at(18))
	}
	for hr := range 24 {
		if f := at(hr); f < 0.65 || f > 1.35 {
			t.Errorf("%02d:00 factor %.2f outside the demo band [0.65, 1.35]", hr, f)
		}
	}
}

func TestAutoDemandScalesWithFleet(t *testing.T) {
	a, b := AutoDemandPerMin(300), AutoDemandPerMin(600)
	if a < 8 || a > 30 {
		t.Fatalf("auto demand for 300 drivers = %.1f/min", a)
	}
	if math.Abs(b-2*a) > 1e-9 || AutoDemandPerMin(0) != 0 {
		t.Fatalf("auto demand not proportional: %.2f vs %.2f", a, b)
	}
}

// demoStats summarises a simulated stretch of demo time.
type demoStats struct {
	busy     float64 // mean (en_route + on_trip) / active, sampled each minute
	firstTry float64 // share of new requests matched immediately
	eventual float64 // share of new requests matched, including re-dispatch
	requests int
	upgraded int
}

// runDemo spawns the fleet, warms up, then samples for `sample`.
func runDemo(h *harness, warmup, sample time.Duration) demoStats {
	ctx := context.Background()
	h.sim.Spawn(ctx)
	var st demoStats
	counting := false
	matched, retried, first := 0, 0, 0
	tick := func() {
		h.clock.advance(time.Second)
		h.sim.DemandTick(ctx)
		h.sim.Step(ctx)
		if counting {
			for _, ev := range h.pub.agentEvents("dispatch_broker") {
				switch {
				case ev.Kind == "no_match":
					st.requests++
				case ev.Kind == "match" && strings.Contains(ev.Decision, "re-dispatched"):
					retried++
				case ev.Kind == "match":
					st.requests++
					first++
				}
				if ev.Upgraded {
					st.upgraded++
				}
			}
		}
		h.pub.reset() // keep memory flat over an hour of ticks
	}
	for range int(warmup.Seconds()) {
		tick()
	}
	counting = true
	sum, n := 0.0, 0
	for i := range int(sample.Seconds()) {
		tick()
		if i%60 == 0 {
			c := h.svc.Fleet().Counts()
			sum += float64(c.EnRoute+c.OnTrip) / float64(c.Active)
			n++
		}
	}
	matched = first + retried
	req := math.Max(float64(st.requests), 1)
	st.busy, st.firstTry, st.eventual = sum/float64(n), float64(first)/req, math.Min(1, float64(matched)/req)
	return st
}

func rideAt(p geo.Point, tier fleet.Tier) dispatch.CreateRideInput {
	drop := geo.Point{Lat: p.Lat + 0.02, Lng: p.Lng}
	return dispatch.CreateRideInput{RiderID: "r_test", Pickup: &p, Dropoff: &drop, Tier: string(tier)}
}

func TestSteadyStateBusyShareWithAutoDemand(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation skipped in -short mode")
	}
	h := newHarness(t, 300, func(o *Options) { o.DemandPerMin = AutoDemandPerMin(300) })
	st := runDemo(h, 45*time.Minute, 30*time.Minute)
	t.Logf("300 drivers, %.1f req/min: busy %.0f%%, %d requests, matched first try %.1f%%, eventually %.1f%%, %d upgrades",
		AutoDemandPerMin(300), st.busy*100, st.requests, st.firstTry*100, st.eventual*100, st.upgraded)
	if st.busy < 0.40 || st.busy > 0.60 {
		t.Errorf("steady-state busy share %.0f%%, want 40-60%%", st.busy*100)
	}
	if st.eventual < 0.85 {
		t.Errorf("eventual match rate %.1f%%, want >= 85%%", st.eventual*100)
	}
}

func TestRushHourIsDramatic(t *testing.T) {
	h := newHarness(t, 300, func(o *Options) { o.DemandPerMin = AutoDemandPerMin(300) })
	ctx := context.Background()
	h.sim.Spawn(ctx)
	base := h.sim.DemandRate()
	before := len(h.pub.agentEvents("dispatch_broker"))
	if _, err := h.sim.TriggerScenario(ctx, "rush_hour"); err != nil {
		t.Fatal(err)
	}
	if got := h.sim.DemandRate(); math.Abs(got-3*base) > 1e-6 {
		t.Fatalf("rush hour demand %.2f, want 3x %.2f", got, base)
	}
	if n := len(h.pub.agentEvents("dispatch_broker")) - before; n < RushBurst {
		t.Fatalf("rush hour fired %d requests at once, want >= %d", n, RushBurst)
	}
	idle0 := h.svc.Fleet().Counts().Idle
	h.step(ctx, 15)
	if idle := h.svc.Fleet().Counts().Idle; idle0-idle < RushBurst/2 && 300-idle < RushBurst {
		t.Fatalf("idle %d -> %d within 15 s: not visibly busier", idle0, idle)
	}
}

func TestRainStormTakesDriversOffline(t *testing.T) {
	h := newHarness(t, 100)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	base := h.sim.DemandRate()
	v, err := h.sim.TriggerScenario(ctx, "rain_storm")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.sim.DemandRate(); math.Abs(got-2*base) > 1e-6 {
		t.Fatalf("rain demand %.2f, want 2x %.2f", got, base)
	}
	if f := h.sim.SpeedFactor(); math.Abs(f-0.7) > 1e-9 {
		t.Fatalf("rain speed factor %.2f, want 0.70 (30%% slower)", f)
	}
	if !strings.Contains(v.Detail, "offline") {
		t.Fatalf("detail %q should mention drivers going offline", v.Detail)
	}
	h.step(ctx, 2)
	c := h.svc.Fleet().Counts()
	if c.Offline < 15 {
		t.Fatalf("only %d drivers offline during the storm (%+v)", c.Offline, c)
	}
	offline := 0
	for _, d := range h.pub.lastDrivers() {
		if d.Status == string(fleet.StatusOffline) {
			offline++
		}
	}
	if offline != c.Offline {
		t.Fatalf("published %d offline, fleet has %d", offline, c.Offline)
	}
	// Offline drivers are never dispatched.
	for i := 1; i <= 100; i++ {
		if d, _ := h.svc.Fleet().Get(DriverID(i)); d.Status == fleet.StatusOffline {
			p := d.Point()
			view, err := h.svc.CreateRide(ctx, rideAt(p, d.Tier))
			if err == nil && view.Match != nil && view.Match.Driver.ID == d.ID {
				t.Fatalf("offline driver %s was matched", d.ID)
			}
			break
		}
	}
	h.clock.advance(ScenarioDuration + time.Second)
	h.step(ctx, 1)
	if c := h.svc.Fleet().Counts(); c.Offline != 0 {
		t.Fatalf("%d drivers still offline after the storm", c.Offline)
	}
}

func TestConcertExitIsABigSpike(t *testing.T) {
	h := newHarness(t, 0)
	base := h.sim.DemandRate()
	if _, err := h.sim.TriggerScenario(context.Background(), "concert_exit"); err != nil {
		t.Fatal(err)
	}
	if ConcertBurst < 40 || h.sim.DemandRate()-base < 40 {
		t.Fatalf("concert burst %d, extra demand %.1f/min", ConcertBurst, h.sim.DemandRate()-base)
	}
}
