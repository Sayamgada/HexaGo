// Package metrics records dispatch match latency and outcomes for both
// Prometheus (/metrics) and the JSON summary at /v1/metrics/dispatch.
package metrics

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// windowSize is how many recent match latencies feed the JSON percentiles.
const windowSize = 4096

// Recorder tracks match latency in a Prometheus histogram plus a sliding
// window of recent samples for exact p50/p95/p99.
type Recorder struct {
	latency   prometheus.Histogram
	matched   prometheus.Counter
	unmatched prometheus.Counter

	matchedTotal atomic.Uint64

	mu      sync.Mutex
	samples [windowSize]float64 // milliseconds
	next    int
	filled  int
}

// NewRecorder creates a Recorder and registers its collectors on reg (nil = don't register).
func NewRecorder(reg prometheus.Registerer) *Recorder {
	outcomes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dispatch_matches_total",
		Help: "Dispatch match outcomes.",
	}, []string{"outcome"})
	r := &Recorder{
		latency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "dispatch_match_latency_seconds",
			Help: "Time to compute a dispatch match (candidate search + scoring).",
			// 10µs .. ~80ms; the SLO is 5 ms.
			Buckets: prometheus.ExponentialBuckets(0.00001, 2, 14),
		}),
	}
	if reg != nil {
		r.latency = register(reg, r.latency).(prometheus.Histogram)
		outcomes = register(reg, outcomes).(*prometheus.CounterVec)
	}
	r.matched = outcomes.WithLabelValues("matched")
	r.unmatched = outcomes.WithLabelValues("unmatched")
	return r
}

// register adds c to reg, reusing an identical collector if one is already registered.
func register(reg prometheus.Registerer, c prometheus.Collector) prometheus.Collector {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			return are.ExistingCollector
		}
		panic(err)
	}
	return c
}

// ObserveMatch records the latency of one match attempt.
func (r *Recorder) ObserveMatch(d time.Duration) {
	r.latency.Observe(d.Seconds())
	ms := float64(d) / float64(time.Millisecond)
	r.mu.Lock()
	r.samples[r.next] = ms
	r.next = (r.next + 1) % windowSize
	if r.filled < windowSize {
		r.filled++
	}
	r.mu.Unlock()
}

// IncMatched counts a ride assigned to a driver.
func (r *Recorder) IncMatched() {
	r.matched.Inc()
	r.matchedTotal.Add(1)
}

// IncUnmatched counts a ride left open because no driver was available.
func (r *Recorder) IncUnmatched() { r.unmatched.Inc() }

// MatchesTotal returns the number of successful matches since start.
func (r *Recorder) MatchesTotal() uint64 { return r.matchedTotal.Load() }

// Quantiles returns nearest-rank p50/p95/p99 in milliseconds over the recent window.
func (r *Recorder) Quantiles() (p50, p95, p99 float64) {
	r.mu.Lock()
	buf := make([]float64, r.filled)
	copy(buf, r.samples[:r.filled])
	r.mu.Unlock()
	if len(buf) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(buf)
	return rank(buf, 0.50), rank(buf, 0.95), rank(buf, 0.99)
}

func rank(sorted []float64, q float64) float64 {
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

// Millis is a millisecond value that always marshals as a JSON float with at
// least one decimal place (0.0, 1.5, 0.0423), rounded to 0.1 µs so
// sub-millisecond match latencies never collapse to zero.
type Millis float64

// MarshalJSON implements json.Marshaler.
func (m Millis) MarshalJSON() ([]byte, error) {
	v := math.Round(float64(m)*10000) / 10000
	s := strconv.FormatFloat(v, 'f', -1, 64)
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return []byte(s), nil
		}
	}
	return []byte(s + ".0"), nil
}
