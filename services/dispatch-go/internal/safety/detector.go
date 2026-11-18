// Package safety is dispatch-go's half of Autonomous Incident Response:
//
//   - Detector watches every on_trip vehicle each simulator tick for route
//     deviation, harsh deceleration, unexpected stops and speed anomalies,
//     scores a severity in [0, 1] and raises at most one open signal per ride.
//   - Service ships each signal to the Safety Sentinel (agents-py
//     POST /v1/safety/signal, plus Redis channel "safety" when configured) and
//     exposes the vehicle-side controls the agent calls back into: suspend /
//     unsuspend a driver, share-live-location links, the simulated emergency
//     dispatch and contact notification, vehicle paths from telemetry, and
//     publishing safety_call frames on the live stream.
//
// The agent decides; this package only detects and executes.
package safety

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Trigger types.
const (
	TriggerDeviation  = "route_deviation"
	TriggerHarshDecel = "harsh_deceleration"
	TriggerStop       = "unexpected_stop"
	TriggerSpeed      = "speed_anomaly"
)

const (
	gravity       = 9.81
	maxTrail      = 180              // trail points kept per ride (3 min at 1 Hz)
	maxPlanPoints = 200              // planned-route points shipped with a signal
	harshWindow   = 30 * time.Second // a harsh brake stays "recent" this long
	staleRide     = 2 * time.Minute  // ride state dropped when not observed for this long
)

// Config holds the detection thresholds.
type Config struct {
	DemoMode        bool          `json:"demo_mode"`
	DeviationKm     float64       `json:"deviation_km"`      // off the planned polyline by more than this...
	DeviationFor    time.Duration `json:"deviation_for"`     // ...for longer than this
	HarshDecelG     float64       `json:"harsh_decel_g"`     // deceleration from speed deltas
	StopFor         time.Duration `json:"stop_for"`          // standstill longer than this...
	StopSpeedKmh    float64       `json:"stop_speed_kmh"`    // ...below this speed...
	StopExclusionKm float64       `json:"stop_exclusion_km"` // ...further than this from pickup and drop-off
	OverspeedKmh    float64       `json:"overspeed_kmh"`     // faster than this...
	OverspeedFor    time.Duration `json:"overspeed_for"`     // ...for longer than this
	SignalTTL       time.Duration `json:"signal_ttl"`        // an open signal re-arms after this
}

// DefaultConfig returns the production thresholds, or the shortened demo ones
// (deviation 10 s instead of 30 s, stop 45 s instead of 3 min).
func DefaultConfig(demo bool) Config {
	c := Config{
		DemoMode: demo, DeviationKm: 0.3, DeviationFor: 30 * time.Second, HarshDecelG: 0.5,
		StopFor: 3 * time.Minute, StopSpeedKmh: 2, StopExclusionKm: 0.15,
		OverspeedKmh: 90, OverspeedFor: 5 * time.Second, SignalTTL: 10 * time.Minute,
	}
	if demo {
		c.DeviationFor, c.StopFor = 10*time.Second, 45*time.Second
	}
	return c
}

// Observation is one on_trip vehicle at one tick.
type Observation struct {
	RideID   string
	DriverID string
	RiderID  string
	Tier     string
	Pos      geo.Point
	SpeedKmh float64
	Plan     []geo.Point // planned polyline; nil while unknown (deviation is then not checked)
	Pickup   geo.Point
	Dropoff  geo.Point
	At       time.Time
}

// Trigger is one anomaly that contributed to a signal.
type Trigger struct {
	Type      string  `json:"type"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	DurationS float64 `json:"duration_s,omitempty"`
	Severity  float64 `json:"severity"`
}

// TrailPoint is one recent position of the vehicle.
type TrailPoint struct {
	Lat      float64   `json:"lat"`
	Lng      float64   `json:"lng"`
	SpeedKmh float64   `json:"speed_kmh"`
	At       time.Time `json:"at"`
}

// Signal is the safety_signal sent to the Safety Sentinel.
type Signal struct {
	Type         string       `json:"type"` // always "safety_signal"
	ID           string       `json:"signal_id"`
	RideID       string       `json:"ride_id"`
	DriverID     string       `json:"driver_id"`
	RiderID      string       `json:"rider_id,omitempty"`
	Tier         string       `json:"tier,omitempty"`
	Severity     float64      `json:"severity"`
	Triggers     []Trigger    `json:"triggers"`
	Location     geo.Point    `json:"location"`
	SpeedKmh     float64      `json:"speed_kmh"`
	Pickup       geo.Point    `json:"pickup"`
	Dropoff      geo.Point    `json:"dropoff"`
	DetectedAt   time.Time    `json:"detected_at"`
	DemoMode     bool         `json:"demo_mode"`
	PlannedRoute [][2]float64 `json:"planned_route"` // [lat, lng]
	Trail        []TrailPoint `json:"trail"`
}

type rideState struct {
	last      TrailPoint
	hasLast   bool
	offSince  time.Time
	maxOffKm  float64
	stopSince time.Time
	overSince time.Time
	maxSpeed  float64
	harshG    float64
	harshAt   time.Time
	trail     []TrailPoint
	plan      []geo.Point
	openedAt  time.Time // zero = no open signal
	lastSeen  time.Time
}

// Detector turns per-tick observations into de-duplicated safety signals.
// It is safe for concurrent use.
type Detector struct {
	cfg   Config
	mu    sync.Mutex
	rides map[string]*rideState
}

// NewDetector builds a Detector; zero thresholds take the DefaultConfig values.
func NewDetector(cfg Config) *Detector {
	def := DefaultConfig(cfg.DemoMode)
	fill := func(v *float64, d float64) {
		if *v <= 0 {
			*v = d
		}
	}
	fillD := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	fill(&cfg.DeviationKm, def.DeviationKm)
	fill(&cfg.HarshDecelG, def.HarshDecelG)
	fill(&cfg.StopSpeedKmh, def.StopSpeedKmh)
	fill(&cfg.StopExclusionKm, def.StopExclusionKm)
	fill(&cfg.OverspeedKmh, def.OverspeedKmh)
	fillD(&cfg.DeviationFor, def.DeviationFor)
	fillD(&cfg.StopFor, def.StopFor)
	fillD(&cfg.OverspeedFor, def.OverspeedFor)
	fillD(&cfg.SignalTTL, def.SignalTTL)
	return &Detector{cfg: cfg, rides: map[string]*rideState{}}
}

// Config returns the effective thresholds.
func (d *Detector) Config() Config { return d.cfg }

// Observe ingests one observation and returns a new signal, or nil when
// nothing crossed a threshold or the ride already has an open signal.
func (d *Detector) Observe(o Observation) *Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.rides[o.RideID]
	if st == nil {
		st = &rideState{}
		d.rides[o.RideID] = st
	}
	st.lastSeen = o.At
	if len(o.Plan) >= 2 {
		st.plan = o.Plan
	}
	pt := TrailPoint{Lat: o.Pos.Lat, Lng: o.Pos.Lng, SpeedKmh: o.SpeedKmh, At: o.At}

	// Harsh deceleration from the speed delta since the previous tick.
	if st.hasLast {
		if dt := o.At.Sub(st.last.At).Seconds(); dt > 0 {
			if g := (st.last.SpeedKmh - o.SpeedKmh) / 3.6 / dt / gravity; g >= d.cfg.HarshDecelG {
				st.harshG, st.harshAt = math.Max(g, recentG(st, o.At)), o.At
			}
		}
	}
	st.last, st.hasLast = pt, true
	st.trail = append(st.trail, pt)
	if len(st.trail) > maxTrail {
		st.trail = append(st.trail[:0], st.trail[len(st.trail)-maxTrail:]...)
	}

	// Route deviation from the planned polyline.
	if len(o.Plan) >= 2 {
		if off := DistToPolylineKm(o.Pos, o.Plan); off > d.cfg.DeviationKm {
			if st.offSince.IsZero() {
				st.offSince = o.At
			}
			st.maxOffKm = math.Max(st.maxOffKm, off)
		} else {
			st.offSince, st.maxOffKm = time.Time{}, 0
		}
	}

	// Unexpected stop: standing still away from pickup and drop-off.
	nearStop := geo.HaversineKm(o.Pos, o.Pickup) <= d.cfg.StopExclusionKm || geo.HaversineKm(o.Pos, o.Dropoff) <= d.cfg.StopExclusionKm
	if o.SpeedKmh < d.cfg.StopSpeedKmh && !nearStop {
		if st.stopSince.IsZero() {
			st.stopSince = o.At
		}
	} else {
		st.stopSince = time.Time{}
	}

	// Speed anomaly: sustained overspeed.
	if o.SpeedKmh > d.cfg.OverspeedKmh {
		if st.overSince.IsZero() {
			st.overSince = o.At
		}
		st.maxSpeed = math.Max(st.maxSpeed, o.SpeedKmh)
	} else {
		st.overSince, st.maxSpeed = time.Time{}, 0
	}

	if !st.openedAt.IsZero() {
		if o.At.Sub(st.openedAt) < d.cfg.SignalTTL {
			return nil // one open signal per ride
		}
		st.openedAt = time.Time{}
	}
	trig := d.triggers(st, o.At)
	if !fired(trig, st, o.At, d.cfg) {
		return nil
	}
	st.openedAt = o.At
	return d.signal(o, st, trig)
}

func recentG(st *rideState, now time.Time) float64 {
	if !st.harshAt.IsZero() && now.Sub(st.harshAt) <= harshWindow {
		return st.harshG
	}
	return 0
}

// triggers lists the anomalies that count towards a signal: a deviation of
// any duration (its distance is evidence on its own), a harsh brake within the
// last 30 s, and stops / overspeed only once they lasted their threshold.
func (d *Detector) triggers(st *rideState, now time.Time) []Trigger {
	var out []Trigger
	if !st.offSince.IsZero() {
		out = append(out, Trigger{Type: TriggerDeviation, Value: math.Round(st.maxOffKm * 1000), Unit: "m",
			DurationS: now.Sub(st.offSince).Seconds(), Severity: DeviationSeverity(st.maxOffKm)})
	}
	if g := recentG(st, now); g > 0 {
		out = append(out, Trigger{Type: TriggerHarshDecel, Value: round(g, 2), Unit: "g", Severity: DecelSeverity(g)})
	}
	if !st.stopSince.IsZero() && now.Sub(st.stopSince) >= d.cfg.StopFor {
		s := now.Sub(st.stopSince).Seconds()
		out = append(out, Trigger{Type: TriggerStop, Value: s, Unit: "s", DurationS: s, Severity: StopSeverity})
	}
	if !st.overSince.IsZero() && now.Sub(st.overSince) >= d.cfg.OverspeedFor {
		out = append(out, Trigger{Type: TriggerSpeed, Value: round(st.maxSpeed, 1), Unit: "km/h",
			DurationS: now.Sub(st.overSince).Seconds(), Severity: SpeedSeverity(st.maxSpeed)})
	}
	return out
}

// fired reports whether a trigger crossed its threshold this tick: a harsh
// brake fires at once, a deviation once it persisted, stops and overspeed are
// only listed once persistent.
func fired(trig []Trigger, st *rideState, now time.Time, cfg Config) bool {
	for _, t := range trig {
		switch t.Type {
		case TriggerHarshDecel:
			if st.harshAt.Equal(now) {
				return true
			}
		case TriggerDeviation:
			if now.Sub(st.offSince) >= cfg.DeviationFor {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func (d *Detector) signal(o Observation, st *rideState, trig []Trigger) *Signal {
	sev := make([]float64, len(trig))
	for i, t := range trig {
		sev[i] = t.Severity
	}
	sig := &Signal{
		Type: "safety_signal", ID: newID("sig_"), RideID: o.RideID, DriverID: o.DriverID, RiderID: o.RiderID, Tier: o.Tier,
		Severity: CombineSeverity(sev...), Triggers: trig, Location: o.Pos, SpeedKmh: round(o.SpeedKmh, 1),
		Pickup: o.Pickup, Dropoff: o.Dropoff, DetectedAt: o.At.UTC(), DemoMode: d.cfg.DemoMode,
		PlannedRoute: downsample(st.plan, maxPlanPoints), Trail: append([]TrailPoint(nil), st.trail...),
	}
	sort.SliceStable(sig.Triggers, func(i, j int) bool { return sig.Triggers[i].Severity > sig.Triggers[j].Severity })
	return sig
}

// RideEnded forgets a ride (its open signal included).
func (d *Detector) RideEnded(rideID string) {
	d.mu.Lock()
	delete(d.rides, rideID)
	d.mu.Unlock()
}

// Sweep forgets rides not observed since before now-staleRide.
func (d *Detector) Sweep(now time.Time) {
	d.mu.Lock()
	for id, st := range d.rides {
		if now.Sub(st.lastSeen) > staleRide {
			delete(d.rides, id)
		}
	}
	d.mu.Unlock()
}

// Tracked returns how many rides have detector state.
func (d *Detector) Tracked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.rides)
}

// ---- severity ----

// StopSeverity is the severity of an unexpected stop.
const StopSeverity = 0.3

// DeviationSeverity maps metres off route to [0.35, 0.85]: 300 m → 0.35, 1 km → 0.85.
func DeviationSeverity(offKm float64) float64 {
	return round(clamp(0.35+0.5*(offKm-0.3)/0.7, 0.35, 0.85), 3)
}

// DecelSeverity maps a deceleration in g to [0.3, 0.6]: 0.5 g → 0.3, 1 g → 0.6.
func DecelSeverity(g float64) float64 {
	return round(clamp(0.3+0.6*(g-0.5), 0.3, 0.6), 3)
}

// SpeedSeverity maps a speed in km/h to [0.25, 0.6]: 90 → 0.25, 125+ → 0.6.
func SpeedSeverity(kmh float64) float64 {
	return round(clamp(0.25+(kmh-90)/100, 0.25, 0.6), 3)
}

// CombineSeverity treats each component as an independent probability of a
// real incident: 1 - Π(1 - s). Two moderate anomalies outrank one.
func CombineSeverity(s ...float64) float64 {
	p := 1.0
	for _, v := range s {
		p *= 1 - clamp(v, 0, 1)
	}
	return round(1-p, 3)
}

// ---- geometry ----

// DistToPolylineKm is the distance from p to the nearest segment of pts
// (local equirectangular projection; accurate to well under 1% within a city).
func DistToPolylineKm(p geo.Point, pts []geo.Point) float64 {
	switch len(pts) {
	case 0:
		return 0
	case 1:
		return geo.HaversineKm(p, pts[0])
	}
	kx := 111.320 * math.Cos(p.Lat*math.Pi/180)
	const ky = 110.574
	best := math.Inf(1)
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		ax, ay := (a.Lng-p.Lng)*kx, (a.Lat-p.Lat)*ky
		bx, by := (b.Lng-p.Lng)*kx, (b.Lat-p.Lat)*ky
		dx, dy := bx-ax, by-ay
		t := 0.0
		if l := dx*dx + dy*dy; l > 0 {
			t = math.Max(0, math.Min(1, -(ax*dx+ay*dy)/l))
		}
		best = math.Min(best, math.Hypot(ax+t*dx, ay+t*dy))
	}
	return best
}

// downsample keeps at most n points of pts (always the first and last) as [lat, lng].
func downsample(pts []geo.Point, n int) [][2]float64 {
	out := make([][2]float64, 0, min(len(pts), n))
	if len(pts) == 0 {
		return out
	}
	if len(pts) <= n {
		for _, p := range pts {
			out = append(out, [2]float64{p.Lat, p.Lng})
		}
		return out
	}
	step := float64(len(pts)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		p := pts[int(math.Round(float64(i)*step))]
		out = append(out, [2]float64{p.Lat, p.Lng})
	}
	return out
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
