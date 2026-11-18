package safety

import (
	"math"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

var (
	t0     = time.Date(2026, 11, 18, 6, 30, 0, 0, time.UTC)
	origin = geo.Point{Lat: 12.9716, Lng: 77.5946}
)

// east returns the point km east of origin (north for negative-free tests use northKm).
func east(km float64) geo.Point {
	return geo.Point{Lat: origin.Lat, Lng: origin.Lng + km/(111.320*math.Cos(origin.Lat*math.Pi/180))}
}

func at(p geo.Point, northKm float64) geo.Point {
	return geo.Point{Lat: p.Lat + northKm/110.574, Lng: p.Lng}
}

// plan is a straight 5 km route due east; pickup at its start, drop-off at its end.
var plan = []geo.Point{origin, east(2.5), east(5)}

func obs(ride string, pos geo.Point, kmh float64, sec float64) Observation {
	return Observation{RideID: ride, DriverID: "d_001", RiderID: "r_1", Tier: "standard", Pos: pos, SpeedKmh: kmh,
		Plan: plan, Pickup: origin, Dropoff: east(5), At: t0.Add(time.Duration(sec * float64(time.Second)))}
}

func TestDefaultConfigDemoThresholds(t *testing.T) {
	prod, demo := DefaultConfig(false), DefaultConfig(true)
	if prod.DeviationFor != 30*time.Second || prod.StopFor != 3*time.Minute || prod.DeviationKm != 0.3 || prod.HarshDecelG != 0.5 {
		t.Fatalf("prod %+v", prod)
	}
	if demo.DeviationFor != 10*time.Second || demo.StopFor != 45*time.Second || !demo.DemoMode {
		t.Fatalf("demo %+v", demo)
	}
	// Zero thresholds are filled from the defaults.
	if c := NewDetector(Config{DemoMode: true}).Config(); c != demo {
		t.Fatalf("filled %+v", c)
	}
	custom := NewDetector(Config{DeviationKm: 1, DeviationFor: time.Minute}).Config()
	if custom.DeviationKm != 1 || custom.DeviationFor != time.Minute || custom.StopFor != 3*time.Minute {
		t.Fatalf("custom %+v", custom)
	}
}

// driveOff keeps the vehicle `offKm` north of the route for `secs` seconds at 30 km/h.
func driveOff(d *Detector, ride string, offKm float64, secs int) (*Signal, int) {
	for i := 0; i <= secs; i++ {
		if s := d.Observe(obs(ride, at(east(1+float64(i)*0.008), offKm), 30, float64(i))); s != nil {
			return s, i
		}
	}
	return nil, -1
}

func TestDeviationNeedsDistanceAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		demo   bool
		offKm  float64
		wantAt int // seconds after leaving the route; -1 = never
	}{
		{true, 0.29, -1},  // within 300 m: never
		{true, 0.32, 10},  // demo: 10 s
		{false, 0.32, 30}, // production: 30 s
		{false, 0.8, 30},
	} {
		d := NewDetector(DefaultConfig(tc.demo))
		sig, i := driveOff(d, "r", tc.offKm, 60)
		if i != tc.wantAt {
			t.Fatalf("demo=%v off=%.2f: fired at %d, want %d", tc.demo, tc.offKm, i, tc.wantAt)
		}
		if sig == nil {
			continue
		}
		if sig.Triggers[0].Type != TriggerDeviation || math.Abs(sig.Triggers[0].Value-tc.offKm*1000) > 5 {
			t.Fatalf("triggers %+v", sig.Triggers)
		}
		if sig.Severity != DeviationSeverity(tc.offKm) || sig.Severity >= 0.8 {
			t.Fatalf("severity %v", sig.Severity)
		}
		if sig.Type != "safety_signal" || sig.ID == "" || sig.RiderID != "r_1" || len(sig.PlannedRoute) != 3 || len(sig.Trail) != i+1 || sig.DemoMode != tc.demo {
			t.Fatalf("signal %+v", sig)
		}
	}
}

func TestReturningToRouteResetsDeviation(t *testing.T) {
	d := NewDetector(DefaultConfig(true))
	for i := 0; i < 8; i++ {
		if s := d.Observe(obs("r", at(east(1), 0.4), 30, float64(i))); s != nil {
			t.Fatal("fired early")
		}
	}
	d.Observe(obs("r", east(1.1), 30, 8)) // back on the route
	for i := 9; i < 18; i++ {
		if s := d.Observe(obs("r", at(east(1.2), 0.4), 30, float64(i))); s != nil {
			t.Fatalf("fired at %d: the clock should have restarted at 9", i)
		}
	}
	if s := d.Observe(obs("r", at(east(1.2), 0.4), 30, 19)); s == nil {
		t.Fatal("did not fire 10 s after leaving the route again")
	}
}

func TestHarshDecelerationFiresImmediately(t *testing.T) {
	d := NewDetector(DefaultConfig(false))
	d.Observe(obs("r", east(1), 50, 0))
	// 50 -> 33 km/h in 1 s = 0.48 g: under the threshold (normal braking).
	if s := d.Observe(obs("r", east(1.01), 33, 1)); s != nil {
		t.Fatalf("0.48 g fired: %+v", s)
	}
	// 33 -> 0 in 1 s = 0.93 g.
	s := d.Observe(obs("r", east(1.02), 0, 2))
	if s == nil || s.Triggers[0].Type != TriggerHarshDecel {
		t.Fatalf("harsh brake not flagged: %+v", s)
	}
	if g := s.Triggers[0].Value; math.Abs(g-0.93) > 0.01 {
		t.Fatalf("g = %v", g)
	}
	if s.Severity != s.Triggers[0].Severity || math.Abs(s.Severity-DecelSeverity(0.934)) > 0.002 {
		t.Fatalf("severity %v", s.Severity)
	}
}

func TestNoDecelerationOnFirstTickOrZeroDt(t *testing.T) {
	d := NewDetector(DefaultConfig(false))
	if s := d.Observe(obs("r", east(1), 0, 0)); s != nil {
		t.Fatal("first observation cannot have a deceleration")
	}
	d.Observe(obs("r", east(1), 60, 1))
	if s := d.Observe(obs("r", east(1), 0, 1)); s != nil { // same timestamp: dt = 0
		t.Fatal("zero dt produced a signal")
	}
}

func TestUnexpectedStopAwayFromPickupAndDropoff(t *testing.T) {
	for _, tc := range []struct {
		demo bool
		pos  geo.Point
		want int
	}{
		{true, east(2), 45},
		{false, east(2), 180},
		{true, east(0.1), -1},  // waiting at the pickup
		{true, east(4.95), -1}, // at the drop-off
	} {
		d := NewDetector(DefaultConfig(tc.demo))
		got := -1
		for i := 0; i <= 200; i++ {
			if s := d.Observe(obs("r", tc.pos, 0, float64(i))); s != nil {
				got = i
				if s.Triggers[0].Type != TriggerStop || s.Severity != StopSeverity {
					t.Fatalf("stop signal %+v", s)
				}
				break
			}
		}
		if got != tc.want {
			t.Fatalf("demo=%v pos=%v: fired at %d, want %d", tc.demo, tc.pos, got, tc.want)
		}
	}
	// Moving again resets the stop clock.
	d := NewDetector(DefaultConfig(true))
	for i := 0; i < 40; i++ {
		d.Observe(obs("r", east(2), 0, float64(i)))
	}
	d.Observe(obs("r", east(2), 5, 40))
	for i := 41; i < 80; i++ {
		if s := d.Observe(obs("r", east(2), 0, float64(i))); s != nil {
			t.Fatalf("fired at %d after moving at 40", i)
		}
	}
}

func TestSpeedAnomaly(t *testing.T) {
	d := NewDetector(DefaultConfig(false))
	d.Observe(obs("r", east(1), 95, 0))
	d.Observe(obs("r", east(1.03), 80, 1)) // dips under the limit: clock resets (and 0.42 g is no harsh brake)
	var sig *Signal
	for i := 2; i < 20 && sig == nil; i++ {
		sig = d.Observe(obs("r", east(1+0.03*float64(i)), 120, float64(i)))
		if sig != nil && i != 7 {
			t.Fatalf("fired at %d, want 7 (5 s after 2)", i)
		}
	}
	if sig == nil || sig.Triggers[0].Type != TriggerSpeed || sig.Triggers[0].Value != 120 || sig.Severity != SpeedSeverity(120) {
		t.Fatalf("speed signal %+v", sig)
	}
}

func TestOneOpenSignalPerRideUntilTTLOrRideEnd(t *testing.T) {
	d := NewDetector(Config{DemoMode: true, SignalTTL: time.Minute})
	if s, _ := driveOff(d, "a", 0.5, 30); s == nil {
		t.Fatal("no first signal")
	}
	// Still deviating, then a hard brake: suppressed while the signal is open.
	for i := 31; i < 50; i++ {
		if s := d.Observe(obs("a", at(east(1.3), 0.5), 40, float64(i))); s != nil {
			t.Fatalf("duplicate at %d", i)
		}
	}
	if s := d.Observe(obs("a", at(east(1.3), 0.5), 0, 50)); s != nil {
		t.Fatal("duplicate on hard brake")
	}
	// Another ride is independent.
	if s, _ := driveOff(d, "b", 0.5, 30); s == nil {
		t.Fatal("other ride suppressed")
	}
	// After the TTL the ride can signal again.
	if s := d.Observe(obs("a", at(east(1.3), 0.5), 0, 75)); s == nil {
		t.Fatal("no re-arm after TTL")
	}
	// RideEnded clears the state.
	d.RideEnded("a")
	if d.Tracked() != 1 {
		t.Fatalf("tracked %d", d.Tracked())
	}
	if s := d.Observe(obs("a", at(east(1.3), 0.5), 0, 76)); s != nil {
		t.Fatal("fresh state fired immediately")
	}
}

func TestCombinedTriggersEscalateSeverity(t *testing.T) {
	d := NewDetector(Config{DemoMode: true})
	// 6 s off route at 50 km/h, then a hard brake to 0: the brake fires the
	// signal and the (not yet persistent) deviation still counts.
	for i := 0; i < 6; i++ {
		d.Observe(obs("r", at(east(1), 0.9), 50, float64(i)))
	}
	s := d.Observe(obs("r", at(east(1), 0.9), 0, 6))
	if s == nil || len(s.Triggers) != 2 {
		t.Fatalf("signal %+v", s)
	}
	want := CombineSeverity(DeviationSeverity(0.9), DecelSeverity(50/3.6/9.81))
	if s.Severity != want || s.Severity < 0.8 {
		t.Fatalf("severity %v want %v (>= 0.8)", s.Severity, want)
	}
	if s.Triggers[0].Severity < s.Triggers[1].Severity {
		t.Fatal("triggers not sorted by severity")
	}
}

func TestPlanUnknownSkipsDeviation(t *testing.T) {
	d := NewDetector(DefaultConfig(true))
	for i := 0; i < 30; i++ {
		o := obs("r", at(east(1), 2), 30, float64(i))
		o.Plan = nil
		if s := d.Observe(o); s != nil {
			t.Fatalf("fired without a plan: %+v", s)
		}
	}
}

func TestTrailIsBounded(t *testing.T) {
	d := NewDetector(DefaultConfig(false))
	for i := 0; i < maxTrail+50; i++ {
		d.Observe(obs("r", east(1+float64(i)*0.001), 30, float64(i)))
	}
	if n := len(d.rides["r"].trail); n != maxTrail {
		t.Fatalf("trail %d", n)
	}
}

func TestSweepDropsStaleRides(t *testing.T) {
	d := NewDetector(DefaultConfig(false))
	d.Observe(obs("old", east(1), 30, 0))
	d.Observe(obs("new", east(1), 30, 200))
	d.Sweep(t0.Add(200 * time.Second))
	if d.Tracked() != 1 || d.rides["new"] == nil {
		t.Fatalf("tracked %d", d.Tracked())
	}
}

func TestSeverityFunctions(t *testing.T) {
	cases := []struct{ got, want float64 }{
		{DeviationSeverity(0.3), 0.35}, {DeviationSeverity(1), 0.85}, {DeviationSeverity(5), 0.85},
		{DecelSeverity(0.5), 0.3}, {DecelSeverity(1), 0.6}, {DecelSeverity(3), 0.6},
		{SpeedSeverity(90), 0.25}, {SpeedSeverity(200), 0.6},
		{CombineSeverity(), 0}, {CombineSeverity(0.5, 0.5), 0.75}, {CombineSeverity(2), 1}, {CombineSeverity(-1, 0.2), 0.2},
	}
	for i, c := range cases {
		if math.Abs(c.got-c.want) > 1e-9 {
			t.Fatalf("case %d: %v want %v", i, c.got, c.want)
		}
	}
}

func TestGeometryHelpers(t *testing.T) {
	if DistToPolylineKm(origin, nil) != 0 {
		t.Fatal("empty polyline")
	}
	if d := DistToPolylineKm(at(origin, 1), []geo.Point{origin}); math.Abs(d-1) > 0.01 {
		t.Fatalf("single point %v", d)
	}
	if d := DistToPolylineKm(at(east(1), 0.5), plan); math.Abs(d-0.5) > 0.005 {
		t.Fatalf("off route %v", d)
	}
	if d := DistToPolylineKm(east(1), []geo.Point{origin, origin, east(2)}); d > 0.001 {
		t.Fatalf("degenerate segment %v", d)
	}
	if len(downsample(nil, 5)) != 0 {
		t.Fatal("downsample nil")
	}
	pts := make([]geo.Point, 1000)
	for i := range pts {
		pts[i] = east(float64(i) / 100)
	}
	ds := downsample(pts, 200)
	if len(ds) != 200 || ds[0][1] != pts[0].Lng || ds[199][1] != pts[999].Lng {
		t.Fatalf("downsample %d", len(ds))
	}
}
