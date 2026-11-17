package matching

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

var (
	center  = geo.Point{Lat: 12.9716, Lng: 77.5946}
	dropoff = geo.Point{Lat: 12.9784, Lng: 77.6408} // Indiranagar, ~5 km east
	t0      = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
)

// offset returns a point dKm north and eKm east of p.
func offset(p geo.Point, nKm, eKm float64) geo.Point {
	return geo.Point{
		Lat: p.Lat + nKm/111.195,
		Lng: p.Lng + eKm/(111.195*math.Cos(p.Lat*math.Pi/180)),
	}
}

func newFixture(t testing.TB) (*fleet.Index, *Matcher, *time.Time) {
	t.Helper()
	g, err := geo.NewHexGrid(9, center.Lat)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	clock := func() time.Time { return now }
	idx := fleet.New(g, fleet.WithClock(clock))
	return idx, New(idx, g, DefaultConfig(), WithClock(clock)), &now
}

func mustUpsert(t testing.TB, idx *fleet.Index, d fleet.Driver) {
	t.Helper()
	if d.Rating == 0 {
		d.Rating = 4.8
	}
	if d.Status == "" {
		d.Status = fleet.StatusIdle
	}
	if _, err := idx.Upsert(d); err != nil {
		t.Fatal(err)
	}
}

func at(id string, p geo.Point, tier fleet.Tier) fleet.Driver {
	return fleet.Driver{ID: id, Lat: p.Lat, Lng: p.Lng, Tier: tier}
}

func TestScore(t *testing.T) {
	cfg := DefaultConfig()
	req := Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierSedan}
	north := offset(center, 0.5, 0) // 500 m north of pickup; bearing to pickup is 180°

	base := func() fleet.Driver {
		d := at("d", north, fleet.TierSedan)
		d.Status = fleet.StatusIdle
		d.Rating = 4.8
		d.IdleSince = t0.Add(-5 * time.Minute)
		return d
	}

	tests := []struct {
		name     string
		mutate   func(*fleet.Driver)
		eligible bool
		check    func(t *testing.T, b Breakdown)
	}{
		{"baseline idle sedan", func(*fleet.Driver) {}, true, func(t *testing.T, b Breakdown) {
			if b.DistanceKm < 0.6 || b.DistanceKm > 0.7 { // 0.5 km * road factor 1.3
				t.Errorf("DistanceKm = %v", b.DistanceKm)
			}
			if b.Momentum != 0.5 {
				t.Errorf("stationary momentum = %v, want 0.5", b.Momentum)
			}
			if b.Battery != 1 {
				t.Errorf("non-AV battery score = %v, want 1", b.Battery)
			}
			if b.Total <= 0 || b.Total > 1 {
				t.Errorf("Total = %v", b.Total)
			}
		}},
		{"heading toward pickup", func(d *fleet.Driver) { d.Heading, d.SpeedKmh = 180, 30 }, true, func(t *testing.T, b Breakdown) {
			if b.Momentum < 0.99 {
				t.Errorf("momentum toward pickup = %v", b.Momentum)
			}
		}},
		{"heading away from pickup", func(d *fleet.Driver) { d.Heading, d.SpeedKmh = 0, 30 }, true, func(t *testing.T, b Breakdown) {
			if b.Momentum > 0.01 {
				t.Errorf("momentum away from pickup = %v", b.Momentum)
			}
		}},
		{"perpendicular heading", func(d *fleet.Driver) { d.Heading, d.SpeedKmh = 90, 30 }, true, func(t *testing.T, b Breakdown) {
			if math.Abs(b.Momentum-0.5) > 0.01 {
				t.Errorf("perpendicular momentum = %v", b.Momentum)
			}
		}},
		{"unrated driver uses default rating", func(d *fleet.Driver) { d.Rating = 0 }, true, func(t *testing.T, b Breakdown) {
			if want := (cfg.DefaultRating - 1) / 4; math.Abs(b.Rating-want) > 1e-9 {
				t.Errorf("Rating = %v, want %v", b.Rating, want)
			}
		}},
		{"long idle saturates", func(d *fleet.Driver) { d.IdleSince = t0.Add(-2 * time.Hour) }, true, func(t *testing.T, b Breakdown) {
			if b.Idle != 1 {
				t.Errorf("Idle = %v, want 1", b.Idle)
			}
		}},
		{"unknown idle time", func(d *fleet.Driver) { d.IdleSince = time.Time{} }, true, func(t *testing.T, b Breakdown) {
			if b.Idle != 0 {
				t.Errorf("Idle = %v, want 0", b.Idle)
			}
		}},
		{"wrong tier", func(d *fleet.Driver) { d.Tier = fleet.TierAuto }, false, nil},
		{"not idle", func(d *fleet.Driver) { d.Status = fleet.StatusOnTrip }, false, nil},
		{"offline", func(d *fleet.Driver) { d.Status = fleet.StatusOffline }, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := base()
			tc.mutate(&d)
			b, ok := Score(cfg, d, req, t0)
			if ok != tc.eligible {
				t.Fatalf("eligible = %v, want %v", ok, tc.eligible)
			}
			if ok && tc.check != nil {
				tc.check(t, b)
			}
		})
	}
}

func TestScoreAVBattery(t *testing.T) {
	cfg := DefaultConfig()
	req := Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierAV}
	tests := []struct {
		battery  float64
		eligible bool
	}{
		{100, true},
		{40, true},
		{cfg.AVReservePct, false}, // reserve alone can't cover the trip
		{5, false},
		{0, false},
	}
	var prev float64 = 2
	for _, tc := range tests {
		d := at("av", offset(center, 0.3, 0), fleet.TierAV)
		d.Status, d.Rating, d.BatteryPct = fleet.StatusIdle, 4.9, tc.battery
		b, ok := Score(cfg, d, req, t0)
		if ok != tc.eligible {
			t.Errorf("battery %v: eligible = %v, want %v", tc.battery, ok, tc.eligible)
			continue
		}
		if ok {
			if b.Battery <= 0 || b.Battery > 1 || b.Battery >= prev {
				t.Errorf("battery %v: score %v (prev %v) should be in (0,1] and decreasing", tc.battery, b.Battery, prev)
			}
			prev = b.Battery
		}
	}
}

func TestScoreETAMonotonic(t *testing.T) {
	cfg := DefaultConfig()
	req := Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini}
	prevETA, prevScore := -1.0, 2.0
	for _, km := range []float64{0, 0.2, 0.5, 1, 3, 10} {
		d := at("d", offset(center, km, 0), fleet.TierMini)
		d.Status, d.Rating = fleet.StatusIdle, 4.5
		b, ok := Score(cfg, d, req, t0)
		if !ok {
			t.Fatalf("ineligible at %v km", km)
		}
		if b.ETASeconds < prevETA || b.ETA > prevScore {
			t.Errorf("at %v km: eta %v (prev %v), score %v (prev %v) not monotonic", km, b.ETASeconds, prevETA, b.ETA, prevScore)
		}
		if b.ETA < 0 || b.ETA > 1 {
			t.Errorf("ETA score out of range: %v", b.ETA)
		}
		prevETA, prevScore = b.ETASeconds, b.ETA
	}
	// A fast-moving driver gets a shorter ETA than a stationary one at the same spot.
	slow := at("slow", offset(center, 1, 0), fleet.TierMini)
	slow.Status = fleet.StatusIdle
	fast := slow
	fast.SpeedKmh = 60
	bs, _ := Score(cfg, slow, req, t0)
	bf, _ := Score(cfg, fast, req, t0)
	if bf.ETASeconds >= bs.ETASeconds {
		t.Errorf("fast ETA %v should beat stationary ETA %v", bf.ETASeconds, bs.ETASeconds)
	}
}

func TestMatchPicksBestAndAlternatives(t *testing.T) {
	idx, m, _ := newFixture(t)
	// Four sedans at increasing distance, plus decoys that must be ignored.
	mustUpsert(t, idx, at("near", offset(center, 0.15, 0), fleet.TierSedan))
	mustUpsert(t, idx, at("mid", offset(center, 0.4, 0), fleet.TierSedan))
	mustUpsert(t, idx, at("far", offset(center, 0.6, 0.1), fleet.TierSedan))
	mustUpsert(t, idx, at("farther", offset(center, -0.7, 0), fleet.TierSedan))
	mustUpsert(t, idx, at("fifth", offset(center, 0, 0.75), fleet.TierSedan))
	mustUpsert(t, idx, at("auto-decoy", center, fleet.TierAuto))
	busy := at("busy-decoy", center, fleet.TierSedan)
	busy.Status = fleet.StatusOnTrip
	mustUpsert(t, idx, busy)

	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierSedan})
	if err != nil {
		t.Fatal(err)
	}
	if res.Best.Driver.ID != "near" {
		t.Errorf("Best = %s, want near", res.Best.Driver.ID)
	}
	if len(res.Alternatives) != 3 {
		t.Fatalf("Alternatives = %d, want 3", len(res.Alternatives))
	}
	if res.Alternatives[0].Driver.ID != "mid" {
		t.Errorf("first alternative = %s, want mid", res.Alternatives[0].Driver.ID)
	}
	prev := res.Best.Score.Total
	for _, a := range res.Alternatives {
		if a.Driver.Tier != fleet.TierSedan || a.Driver.Status != fleet.StatusIdle {
			t.Errorf("ineligible alternative %+v", a.Driver)
		}
		if a.Score.Total > prev {
			t.Errorf("alternatives not sorted by score")
		}
		prev = a.Score.Total
	}
	if res.Considered != 5 {
		t.Errorf("Considered = %d, want 5", res.Considered)
	}
	if res.RingsSearched < 1 || res.RingsSearched > 4 {
		t.Errorf("RingsSearched = %d", res.RingsSearched)
	}
	if res.Latency < 0 { // can be 0 on Windows' coarse clock
		t.Errorf("Latency = %v", res.Latency)
	}
}

func TestMatchMomentumBreaksNearTie(t *testing.T) {
	idx, m, _ := newFixture(t)
	// Two drivers equally far away on opposite sides; one is driving toward the pickup.
	toward := at("toward", offset(center, 0.4, 0), fleet.TierMini)
	toward.Heading, toward.SpeedKmh = 180, 25
	away := at("away", offset(center, -0.4, 0), fleet.TierMini)
	away.Heading, away.SpeedKmh = 180, 25
	mustUpsert(t, idx, toward)
	mustUpsert(t, idx, away)

	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini})
	if err != nil {
		t.Fatal(err)
	}
	if res.Best.Driver.ID != "toward" {
		t.Errorf("Best = %s, want toward (momentum)", res.Best.Driver.ID)
	}
}

func TestMatchIdleFairness(t *testing.T) {
	idx, m, now := newFixture(t)
	p := offset(center, 0.2, 0)
	// "veteran" became idle 30 minutes before "fresh" at the same spot.
	*now = t0.Add(-30 * time.Minute)
	mustUpsert(t, idx, at("veteran", p, fleet.TierMini))
	*now = t0
	mustUpsert(t, idx, at("fresh", p, fleet.TierMini))

	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini})
	if err != nil {
		t.Fatal(err)
	}
	if res.Best.Driver.ID != "veteran" {
		t.Errorf("Best = %s, want veteran (longer idle)", res.Best.Driver.ID)
	}
}

func TestMatchExpandsRings(t *testing.T) {
	g, _ := geo.NewHexGrid(9, center.Lat)
	edge := geo.AvgEdgeKm(9)
	tests := []struct {
		name      string
		distKm    float64
		wantFound bool
	}{
		{"same cell", 0, true},
		{"about two rings out", 3 * edge, true},
		{"beyond three rings, inside max pickup", 12 * edge, true},
		{"beyond max pickup radius", 6.0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := t0
			clock := func() time.Time { return now }
			idx := fleet.New(g, fleet.WithClock(clock))
			m := New(idx, g, DefaultConfig(), WithClock(clock))
			mustUpsert(t, idx, at("only", offset(center, tc.distKm, 0), fleet.TierXL))
			res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierXL})
			if tc.wantFound {
				if err != nil || res.Best.Driver.ID != "only" {
					t.Fatalf("res=%+v err=%v", res.Best, err)
				}
				if len(res.Alternatives) != 0 {
					t.Errorf("Alternatives = %d, want 0", len(res.Alternatives))
				}
			} else if !errors.Is(err, ErrNoCandidates) {
				t.Fatalf("err = %v, want ErrNoCandidates", err)
			}
		})
	}
}

func TestMatchStopsExpandingWhenEnoughCandidates(t *testing.T) {
	idx, m, _ := newFixture(t)
	for i := 0; i < 6; i++ {
		mustUpsert(t, idx, at(fmt.Sprintf("d%d", i), center, fleet.TierMini))
	}
	mustUpsert(t, idx, at("outer", offset(center, 0.5, 0), fleet.TierMini))
	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini})
	if err != nil {
		t.Fatal(err)
	}
	if res.RingsSearched != 1 {
		t.Errorf("RingsSearched = %d, want 1 (origin cell had enough)", res.RingsSearched)
	}
	if res.Considered != 6 {
		t.Errorf("Considered = %d, want 6", res.Considered)
	}
	// Equal scores are broken by driver ID for determinism.
	if res.Best.Driver.ID != "d0" || res.Alternatives[0].Driver.ID != "d1" {
		t.Errorf("tie-break order: best=%s alt0=%s", res.Best.Driver.ID, res.Alternatives[0].Driver.ID)
	}
}

func TestMatchErrors(t *testing.T) {
	idx, m, _ := newFixture(t)
	mustUpsert(t, idx, at("d", center, fleet.TierMini))
	tests := []struct {
		name string
		req  Request
		want error
	}{
		{"bad pickup", Request{Pickup: geo.Point{Lat: 99}, Dropoff: dropoff, Tier: fleet.TierMini}, ErrInvalidRequest},
		{"bad dropoff", Request{Pickup: center, Dropoff: geo.Point{Lng: 200}, Tier: fleet.TierMini}, ErrInvalidRequest},
		{"bad tier", Request{Pickup: center, Dropoff: dropoff, Tier: "bike"}, ErrInvalidRequest},
		{"no drivers of tier", Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierPremium}, ErrNoCandidates},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.Match(tc.req); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

type countingObserver struct {
	n   int
	sum time.Duration
}

func (o *countingObserver) ObserveMatch(d time.Duration) { o.n++; o.sum += d }

func TestMatchObserverRecordsEveryAttempt(t *testing.T) {
	g, _ := geo.NewHexGrid(9, center.Lat)
	idx := fleet.New(g)
	obs := &countingObserver{}
	m := New(idx, g, Config{}, WithObserver(obs)) // zero Config falls back to defaults
	mustUpsert(t, idx, at("d", center, fleet.TierMini))

	_, _ = m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini})
	_, _ = m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierAV})
	_, _ = m.Match(Request{Pickup: geo.Point{Lat: 100}, Tier: fleet.TierMini}) // invalid: not a match attempt
	if obs.n != 2 {
		t.Errorf("observer called %d times, want 2", obs.n)
	}
	if c := m.Config(); c.MaxPickupKm != 5 || c.MinCandidates != 5 || !c.AllowUpgrade {
		t.Errorf("Config() = %+v", c)
	}
}

func TestResultWithBest(t *testing.T) {
	ranked := make([]Candidate, 6)
	for i := range ranked {
		ranked[i] = Candidate{Driver: fleet.Driver{ID: fmt.Sprintf("d%d", i)}}
	}
	r := newResult(ranked)
	if r.Best.Driver.ID != "d0" || len(r.Alternatives) != 3 {
		t.Fatalf("newResult = %+v", r)
	}
	r2 := r.WithBest(2)
	if r2.Best.Driver.ID != "d2" {
		t.Errorf("Best = %s", r2.Best.Driver.ID)
	}
	want := []string{"d0", "d1", "d3"}
	for i, a := range r2.Alternatives {
		if a.Driver.ID != want[i] {
			t.Errorf("alt %d = %s, want %s", i, a.Driver.ID, want[i])
		}
	}
	if r.Best.Driver.ID != "d0" {
		t.Error("WithBest must not mutate the receiver")
	}
	if r3 := r.WithBest(99); r3.Best.Driver.ID != "d0" {
		t.Error("out-of-range WithBest should return the receiver unchanged")
	}
}
