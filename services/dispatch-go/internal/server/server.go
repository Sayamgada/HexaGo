package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/cors"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/redisdiag"
	"github.com/hexago/hexago/services/dispatch-go/internal/sim"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// Option allows configuring optional router settings.
type Option func(*RouterOptions)

// RouterOptions holds optional dependencies like custom HTTP transport.
type RouterOptions struct {
	Transport http.RoundTripper
	Dispatch  *dispatch.Service
	Gatherer  prometheus.Gatherer
	Hub       *stream.Hub
	Sim       *sim.Sim
	Redis     *redisdiag.Monitor
}

// WithRedisMonitor reports m's latest Redis check on /healthz;
// /healthz?redis=fresh runs a new check first.
func WithRedisMonitor(m *redisdiag.Monitor) Option {
	return func(o *RouterOptions) { o.Redis = m }
}

// WithTransport configures a custom RoundTripper for reverse proxy requests.
func WithTransport(tr http.RoundTripper) Option {
	return func(o *RouterOptions) {
		o.Transport = tr
	}
}

// WithDispatch mounts a pre-built dispatch broker (e.g. backed by Postgres and
// Redis) whose collectors are exposed via gatherer on /metrics.
func WithDispatch(svc *dispatch.Service, gatherer prometheus.Gatherer) Option {
	return func(o *RouterOptions) {
		o.Dispatch = svc
		o.Gatherer = gatherer
	}
}

// WithStream uses hub for /v1/stream. The caller runs hub.Run; without this
// option the router creates an in-process hub and runs it in the background.
func WithStream(hub *stream.Hub) Option {
	return func(o *RouterOptions) { o.Hub = hub }
}

// WithSim mounts the fleet simulator controls (otherwise /v1/sim/* answer 503).
func WithSim(s *sim.Sim) Option {
	return func(o *RouterOptions) { o.Sim = s }
}

// NewRouter constructs and configures the Chi router for dispatch-go.
func NewRouter(cfg *config.Config, opts ...Option) *chi.Mux {
	var options RouterOptions
	for _, opt := range opts {
		opt(&options)
	}

	if options.Hub == nil {
		options.Hub = stream.NewHub(stream.HubOptions{})
		go options.Hub.Run(context.Background())
	}
	hub := options.Hub

	if options.Dispatch == nil {
		// No broker supplied: run fully in memory (tests, local dev).
		reg := prometheus.NewRegistry()
		svc, err := dispatch.New(dispatch.Options{
			H3Resolution:    cfg.H3Resolution,
			Center:          geo.Point{Lat: cfg.CityCenterLat, Lng: cfg.CityCenterLng},
			CitySpeedFactor: cfg.CitySpeedFactor,
			Registry:        reg,
			Publisher:       hub,
		})
		if err != nil {
			log.Fatalf("dispatch: %v", err)
		}
		options.Dispatch, options.Gatherer = svc, reg
	}
	svc := options.Dispatch
	hub.SetMetricsSource(func(ctx context.Context) (any, error) { return svc.Metrics(ctx) })
	gatherers := prometheus.Gatherers{prometheus.DefaultGatherer}
	if options.Gatherer != nil {
		gatherers = append(gatherers, options.Gatherer)
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
		redisStatus := redisdiag.Result{}
		if options.Redis != nil {
			if r.URL.Query().Get("redis") == "fresh" {
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				options.Redis.CheckNow(ctx)
				cancel()
			}
			redisStatus = options.Redis.Last()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "ok",
			"service":   "dispatch-go",
			"demo_mode": cfg.DemoMode,
			"components": map[string]interface{}{
				"store":          svc.StoreKind(),
				"fleet_mirror":   svc.MirrorKind(),
				"mirror_ok":      svc.Fleet().MirrorHealthy(),
				"geo_backend":    geo.Backend(),
				"h3_resolution":  svc.Geo().Resolution(),
				"stream_broker":  hub.BrokerKind(),
				"stream_clients": hub.Clients(),
				"simulator":      options.Sim != nil && options.Sim.Status().Running,
			},
			"redis": redisStatus,
			"city_center": map[string]float64{
				"lat": cfg.CityCenterLat,
				"lng": cfg.CityCenterLng,
			},
		})
	})

	// Prometheus metrics
	r.Handle("/metrics", promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{}))

	// Dispatch broker: rides, driver status, dispatch metrics.
	svc.Routes(r)

	// Live stream (drivers, rides, ops) and simulator controls.
	r.Handle("/v1/stream", hub.Handler(upgrader))
	if options.Sim != nil {
		options.Sim.Routes(r)
	} else {
		sim.DisabledRoutes(r)
	}

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
