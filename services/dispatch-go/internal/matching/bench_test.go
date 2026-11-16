package matching

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

const (
	fleetSize   = 10_000
	cityRadius  = 12.0 // km; roughly the Bengaluru ORR
	latencyGoal = 5 * time.Millisecond
)

// seedCity places n drivers uniformly over a disc around the city centre,
// with a realistic mix of tiers, statuses, headings, speeds and AV batteries.
func seedCity(tb testing.TB, n int, rng *rand.Rand) (*fleet.Index, *Matcher) {
	tb.Helper()
	g, err := geo.NewIndexer(geo.DefaultResolution, center)
	if err != nil {
		tb.Fatal(err)
	}
	idx := fleet.New(g)
	tiers := fleet.Tiers()
	statuses := []fleet.Status{fleet.StatusIdle, fleet.StatusIdle, fleet.StatusIdle, fleet.StatusEnRoute, fleet.StatusOnTrip}
	for i := 0; i < n; i++ {
		d := fleet.Driver{
			ID:        fmt.Sprintf("drv-%05d", i),
			Status:    statuses[rng.Intn(len(statuses))],
			Tier:      tiers[rng.Intn(len(tiers))],
			Heading:   rng.Float64() * 360,
			SpeedKmh:  rng.Float64() * 40,
			Rating:    3.5 + rng.Float64()*1.5,
			IdleSince: time.Now().Add(-time.Duration(rng.Intn(1800)) * time.Second),
		}
		p := randomPoint(rng, cityRadius)
		d.Lat, d.Lng = p.Lat, p.Lng
		if d.Tier == fleet.TierAV {
			d.BatteryPct = 10 + rng.Float64()*90
		}
		if _, err := idx.Upsert(d); err != nil {
			tb.Fatal(err)
		}
	}
	return idx, New(idx, g, DefaultConfig())
}

func randomPoint(rng *rand.Rand, radiusKm float64) geo.Point {
	r := radiusKm * math.Sqrt(rng.Float64())
	theta := rng.Float64() * 2 * math.Pi
	return offset(center, r*math.Cos(theta), r*math.Sin(theta))
}

func randomRequest(rng *rand.Rand) Request {
	tiers := fleet.Tiers()
	return Request{
		Pickup:  randomPoint(rng, cityRadius*0.8),
		Dropoff: randomPoint(rng, cityRadius),
		Tier:    tiers[rng.Intn(len(tiers))],
	}
}

// BenchmarkMatch measures one dispatch decision against a 10k-driver fleet.
func BenchmarkMatch(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	_, m := seedCity(b, fleetSize, rng)
	reqs := make([]Request, 1024)
	for i := range reqs {
		reqs[i] = randomRequest(rng)
	}
	b.ReportAllocs()
	b.ResetTimer()
	matched := 0
	for i := 0; i < b.N; i++ {
		if _, err := m.Match(reqs[i%len(reqs)]); err == nil {
			matched++
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(matched)/float64(b.N), "hit-rate")
}

// BenchmarkMatchParallel measures throughput when many requests match concurrently.
func BenchmarkMatchParallel(b *testing.B) {
	rng := rand.New(rand.NewSource(7))
	_, m := seedCity(b, fleetSize, rng)
	reqs := make([]Request, 1024)
	for i := range reqs {
		reqs[i] = randomRequest(rng)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, _ = m.Match(reqs[i%len(reqs)])
			i++
		}
	})
}

// TestMatchLatencyBudget enforces the < 5 ms per-match budget at 10k drivers.
func TestMatchLatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("latency budget test skipped in -short mode")
	}
	rng := rand.New(rand.NewSource(1))
	_, m := seedCity(t, fleetSize, rng)

	const runs = 500
	lat := make([]time.Duration, 0, runs)
	hits := 0
	for i := 0; i < runs; i++ {
		start := time.Now()
		_, err := m.Match(randomRequest(rng))
		lat = append(lat, time.Since(start))
		if err == nil {
			hits++
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50, p99 := lat[runs/2], lat[runs*99/100]
	t.Logf("10k drivers, %d matches (%d hits): p50=%v p99=%v max=%v backend=%s",
		runs, hits, p50, p99, lat[runs-1], geo.Backend())
	if hits < runs/2 {
		t.Errorf("only %d/%d requests found a driver; fixture too sparse", hits, runs)
	}
	// p50 is the budget gate; p99 gets headroom for the race detector and noisy CI runners.
	if p50 > latencyGoal {
		t.Errorf("p50 match latency %v exceeds %v", p50, latencyGoal)
	}
	if p99 > 4*latencyGoal {
		t.Errorf("p99 match latency %v exceeds %v", p99, 4*latencyGoal)
	}
}
