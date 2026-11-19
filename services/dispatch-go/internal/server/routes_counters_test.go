package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/counters"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

func TestCountersEndpointAndMetricsUseMemoryCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	svc, err := dispatch.New(dispatch.Options{Center: geo.Point{Lat: 12.9716, Lng: 77.5946}, Registry: reg, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(config.Load(), WithDispatch(svc, reg))
	ctx := context.Background()
	svc.RecordAgentEvent(ctx, dispatch.AgentEventInput{Agent: "settlement", EventType: "settlement_scan", Decision: "ledger scan"})
	svc.RecordAgentEvent(ctx, dispatch.AgentEventInput{Agent: dispatch.AgentName, EventType: "match", Decision: "matched"})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/metrics/counters", nil))
	var c counters.Counts
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if c.MatchesToday != 1 || c.DecisionsToday != 2 || c.Source != "memory" || c.Since.IsZero() {
		t.Fatalf("counters %+v", c)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/metrics/dispatch", nil))
	var m dispatch.DispatchMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.MatchesToday != 1 || m.DecisionsToday != 2 {
		t.Fatalf("metrics %+v", m)
	}
}

type failingQ struct{}

func (failingQ) Kind() string { return "postgres" }
func (failingQ) Daily(context.Context, time.Time) (counters.Counts, error) {
	return counters.Counts{}, errors.New("db down")
}

func TestCountersEndpointUnavailable(t *testing.T) {
	r := NewRouter(config.Load(), WithCounters(counters.NewCache(failingQ{}, 0, nil)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/metrics/counters", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// /v1/metrics/dispatch still answers, falling back to the in-process match count.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/metrics/dispatch", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics %d", rec.Code)
	}
}
