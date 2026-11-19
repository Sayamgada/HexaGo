package server

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/apidocs"
	"github.com/hexago/hexago/services/dispatch-go/internal/developer"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/ledger"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// WithDeveloper mounts a pre-built developer platform (Postgres key store,
// Redis rate limiter; main runs it). Without it the router builds an
// in-memory one and runs its webhook workers in the background.
func WithDeveloper(d *developer.Service) Option {
	return func(o *RouterOptions) { o.Developer = d }
}

// mountDocs serves the OpenAPI document and Swagger UI.
func mountDocs(r chi.Router) {
	r.Get("/openapi.json", apidocs.SpecHandler)
	r.Get("/docs", apidocs.SwaggerHandler)
	r.Get("/docs/", apidocs.SwaggerHandler)
}

// attachWebhooks turns platform events into webhook deliveries:
// ride.completed (dispatch), incident.opened (Safety Sentinel replies) and
// payout.sent (ledger events on the ops stream, enriched with the payout).
func attachWebhooks(dev *developer.Service, svc *dispatch.Service, sf *safety.Service, hub *stream.Hub, led *ledger.Ledger) {
	hooks := dev.Webhooks()
	svc.OnRideCompleted(func(ctx context.Context, r store.Ride) {
		hooks.Emit(ctx, developer.EventRideCompleted, r)
	})
	if sf != nil {
		sf.OnIncidentOpened(func(ctx context.Context, inc safety.IncidentOpened) {
			hooks.Emit(ctx, developer.EventIncidentOpened, inc)
		})
	}
	hub.OnPublish(func(topic string, data any) {
		ev, ok := data.(stream.AgentEvent)
		if !ok || topic != stream.TopicOps || (ev.Kind != ledger.EvPayoutSent && ev.Kind != ledger.EvPayoutReleased) {
			return
		}
		// Off the ledger's goroutine: the lookups may hit Postgres.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if subs, err := hooks.Subscribers(ctx, developer.EventPayoutSent); err != nil || len(subs) == 0 {
				return
			}
			var payload any = map[string]any{"ride_id": ev.RideID, "driver_id": ev.DriverID, "description": ev.Decision, "ts": ev.TS}
			if led != nil && ev.RideID != "" {
				if ps, err := led.Payouts(ctx, ledger.PayoutFilter{RideID: ev.RideID, Status: ledger.PayoutSent, Limit: 1}); err == nil && len(ps) > 0 {
					payload = ps[0]
				}
			}
			hooks.Emit(ctx, developer.EventPayoutSent, payload)
		}()
	})
}
