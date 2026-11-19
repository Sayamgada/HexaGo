package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/counters"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/ledger"
)

// WithCounters reports the persistent "today" counters from c (main passes a
// Postgres-backed cache). Without it the router counts the in-memory stores.
func WithCounters(c *counters.Cache) Option {
	return func(o *RouterOptions) { o.Counters = c }
}

// memoryCounters counts the dispatch broker's in-memory agent events and the ledger.
func memoryCounters(svc *dispatch.Service, l *ledger.Ledger) *counters.Cache {
	return counters.NewCache(counters.Funcs{
		Events: func(_ context.Context, since time.Time) (int64, int64, error) {
			m, d, _ := svc.MemoryEventCounts(since)
			return m, d, nil
		},
		Settled: l.RidesSettledSince,
	}, 0, nil)
}

// mountCounters wires the counters into /v1/metrics/dispatch (and the metrics
// stream) and serves GET /v1/metrics/counters.
func mountCounters(r chi.Router, c *counters.Cache, svc *dispatch.Service, l *ledger.Ledger) {
	if c == nil {
		c = memoryCounters(svc, l)
	}
	svc.SetCounters(c)
	r.Get("/v1/metrics/counters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		got, err := c.Get(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(got)
	})
}
