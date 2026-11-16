package server

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/cors"
)

// Option allows configuring optional router settings.
type Option func(*RouterOptions)

// RouterOptions holds optional dependencies like custom HTTP transport.
type RouterOptions struct {
	Transport http.RoundTripper
}

// WithTransport configures a custom RoundTripper for reverse proxy requests.
func WithTransport(tr http.RoundTripper) Option {
	return func(o *RouterOptions) {
		o.Transport = tr
	}
}

// NewRouter constructs and configures the Chi router for dispatch-go.
func NewRouter(cfg *config.Config, opts ...Option) *chi.Mux {
	var options RouterOptions
	for _, opt := range opts {
		opt(&options)
	}

	allow := cors.Parse(cfg.CORSOrigins)
	upgrader := websocket.Upgrader{
		// Browsers always send Origin; native clients (desktop, Android) usually do not.
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == "" || allow.Allowed(origin)
		},
	}

	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(allow.Middleware)

	// Liveness / readiness probe
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "ok",
			"service":   "dispatch-go",
			"demo_mode": cfg.DemoMode,
			"city_center": map[string]float64{
				"lat": cfg.CityCenterLat,
				"lng": cfg.CityCenterLng,
			},
		})
	})

	// Prometheus metrics
	r.Handle("/metrics", promhttp.Handler())

	// Gateway ping route
	r.Get("/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "pong",
			"service": "dispatch-go",
		})
	})

	// Real-time bidirectional WebSocket connection for telemetry and dispatch events
	r.Get("/v1/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade error: %v", err)
			return
		}
		defer conn.Close()

		for {
			messageType, p, err := conn.ReadMessage()
			if err != nil {
				break
			}
			// Echo/ack for ping-pong protocol
			if err := conn.WriteMessage(messageType, p); err != nil {
				break
			}
		}
	})

	// Reverse proxy routes /v1/agents/* to AGENTS_URL
	target, err := url.Parse(cfg.AgentsURL)
	if err != nil {
		log.Printf("Warning: Failed to parse AGENTS_URL '%s': %v", cfg.AgentsURL, err)
	} else {
		proxy := httputil.NewSingleHostReverseProxy(target)
		if options.Transport != nil {
			proxy.Transport = options.Transport
		}
		proxy.ModifyResponse = func(resp *http.Response) error {
			cors.StripUpstream(resp.Header)
			return nil
		}
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			originalDirector(req)
			req.Host = target.Host
		}

		r.HandleFunc("/v1/agents", func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		})
		r.HandleFunc("/v1/agents/*", func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		})
	}

	return r
}
