package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/scheduler"
)

// WithScheduler mounts a pre-built scheduler (main runs it and gives it the
// Postgres repo). Without it the router builds an in-memory one and runs it.
func WithScheduler(s *scheduler.Scheduler) Option {
	return func(o *RouterOptions) { o.Scheduler = s }
}

// mountScheduler registers the multi-leg booking scheduler (/v1/scheduled),
// the trip estimate (/v1/eta) and cell supply (/v1/supply) that agents-py's
// Booking Intent agent uses, and proxies /v1/pricing/*, /v1/legal/* (Legal &
// Jurisdiction) and /v1/ops/* (Autonomous Ops Report) to agents-py.
func mountScheduler(r chi.Router, s *scheduler.Scheduler, cfg *config.Config, svc *dispatch.Service,
	agents http.Handler) *scheduler.Scheduler {
	if s == nil {
		s = scheduler.New(scheduler.Options{
			Rides: svc, Demo: cfg.DemoMode, Events: svc.RecordAgentEvent, Geo: svc.Geo(), Fleet: svc.Fleet(),
		})
		go s.Run(context.Background())
	}
	s.Routes(r)
	if agents != nil {
		r.Handle("/v1/pricing/*", agents)
		r.Handle("/v1/legal/*", agents)
		r.Handle("/v1/ops/*", agents)
	}
	return s
}
