package metrics

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestQuantilesEmpty(t *testing.T) {
	r := NewRecorder(prometheus.NewRegistry())
	p50, p95, p99 := r.Quantiles()
	if p50 != 0 || p95 != 0 || p99 != 0 {
		t.Errorf("empty quantiles = %v %v %v", p50, p95, p99)
	}
}

func TestQuantiles(t *testing.T) {
	tests := []struct {
		name          string
		samplesMs     []float64
		p50, p95, p99 float64
	}{
		{"single", []float64{2}, 2, 2, 2},
		{"1..100", seq(1, 100), 50, 95, 99},
		{"constant", []float64{1, 1, 1, 1}, 1, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRecorder(prometheus.NewRegistry())
			for _, ms := range tc.samplesMs {
				r.ObserveMatch(time.Duration(ms * float64(time.Millisecond)))
			}
			p50, p95, p99 := r.Quantiles()
			for _, c := range []struct{ got, want float64 }{{p50, tc.p50}, {p95, tc.p95}, {p99, tc.p99}} {
				if math.Abs(c.got-c.want) > 1e-6 {
					t.Errorf("quantiles = %v/%v/%v, want %v/%v/%v", p50, p95, p99, tc.p50, tc.p95, tc.p99)
					break
				}
			}
		})
	}
}

func TestWindowKeepsMostRecent(t *testing.T) {
	r := NewRecorder(prometheus.NewRegistry())
	for i := 0; i < windowSize; i++ {
		r.ObserveMatch(100 * time.Millisecond)
	}
	for i := 0; i < windowSize; i++ {
		r.ObserveMatch(time.Millisecond)
	}
	if p99 := func() float64 { _, _, p := r.Quantiles(); return p }(); p99 != 1 {
		t.Errorf("old samples leaked into window: p99 = %v", p99)
	}
}

func TestCountersAndPrometheus(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRecorder(reg)
	r.ObserveMatch(3 * time.Millisecond)
	r.ObserveMatch(4 * time.Millisecond)
	r.IncMatched()
	r.IncUnmatched()
	r.IncUnmatched()

	if r.MatchesTotal() != 1 {
		t.Errorf("MatchesTotal = %d", r.MatchesTotal())
	}
	if n := testutil.CollectAndCount(r.latency); n != 1 {
		t.Errorf("histogram series = %d", n)
	}
	want := `
# HELP dispatch_matches_total Dispatch match outcomes.
# TYPE dispatch_matches_total counter
dispatch_matches_total{outcome="matched"} 1
dispatch_matches_total{outcome="unmatched"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "dispatch_matches_total"); err != nil {
		t.Error(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "dispatch_match_latency_seconds" {
			found = true
			if c := mf.GetMetric()[0].GetHistogram().GetSampleCount(); c != 2 {
				t.Errorf("histogram count = %d", c)
			}
		}
	}
	if !found {
		t.Error("dispatch_match_latency_seconds not registered")
	}
}

func TestNilRegistererAndDoubleRegistration(t *testing.T) {
	_ = NewRecorder(nil) // must not panic or touch the default registry
	reg := prometheus.NewRegistry()
	_ = NewRecorder(reg)
	_ = NewRecorder(reg) // second recorder on the same registry must not panic
}

func TestConcurrentObserve(t *testing.T) {
	r := NewRecorder(prometheus.NewRegistry())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				r.ObserveMatch(time.Millisecond)
				r.IncMatched()
				_, _, _ = r.Quantiles()
			}
		}()
	}
	wg.Wait()
	if r.MatchesTotal() != 8000 {
		t.Errorf("MatchesTotal = %d", r.MatchesTotal())
	}
}

func TestMillisJSON(t *testing.T) {
	tests := []struct {
		in   Millis
		want string
	}{
		{0, "0.0"},
		{1, "1.0"},
		{1.23456, "1.2346"},
		{0.0004, "0.0004"},
		{0.04237, "0.0424"},
		{0.00004, "0.0"},
		{12.5, "12.5"},
	}
	for _, tc := range tests {
		b, err := json.Marshal(tc.in)
		if err != nil || string(b) != tc.want {
			t.Errorf("Marshal(%v) = %s, %v; want %s", float64(tc.in), b, err, tc.want)
		}
	}
}

func seq(from, to int) []float64 {
	out := make([]float64, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, float64(i))
	}
	return out
}
