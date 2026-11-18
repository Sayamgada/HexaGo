package server

import (
	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/ledger"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// WithLedger mounts a pre-built ledger (already attached to the dispatch and
// safety services). Without it the router builds an in-memory one and attaches it.
func WithLedger(l *ledger.Ledger) Option {
	return func(o *RouterOptions) { o.Ledger = l }
}

// mountLedger registers /v1/wallets/* and /v1/ledger/*.
func mountLedger(r chi.Router, l *ledger.Ledger, cfg *config.Config, svc *dispatch.Service, sf *safety.Service, hub *stream.Hub) *ledger.Ledger {
	if l == nil {
		l = ledger.New(ledger.Options{Publisher: hub, TestMode: cfg.DemoMode})
		ledger.Attach(l, svc, sf)
	}
	l.Routes(r)
	return l
}
