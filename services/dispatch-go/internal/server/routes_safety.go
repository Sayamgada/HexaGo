package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
)

// WithSafety mounts a pre-built safety service (the one the simulator feeds).
// Without it the router builds one with no detector input.
func WithSafety(s *safety.Service) Option {
	return func(o *RouterOptions) { o.Safety = s }
}

// mountSafety registers dispatch-go's own safety endpoints (signals, driver
// suspension, share links, vehicle paths, stream publishing, simulated
// emergency services) and proxies the rest of /v1/safety/* - incidents,
// check-ins and the signal intake - to the Safety Sentinel in agents-py.
func mountSafety(r chi.Router, svc *safety.Service, agents http.Handler) {
	svc.Routes(r)
	if agents == nil {
		agents = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"AGENTS_URL is not configured"}`))
		})
	}
	r.Handle("/v1/safety/*", agents)
}
