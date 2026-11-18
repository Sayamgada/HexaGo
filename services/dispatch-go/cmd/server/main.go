package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/redisdiag"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
	"github.com/hexago/hexago/services/dispatch-go/internal/sim"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

func main() {
	cfg := config.Load()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	center := geo.Point{Lat: cfg.CityCenterLat, Lng: cfg.CityCenterLng}
	opts := dispatch.Options{
		H3Resolution:    cfg.H3Resolution,
		Center:          center,
		CitySpeedFactor: cfg.CitySpeedFactor,
		MaxPickupKm:     cfg.MaxPickupKm,
	}

	// Persistence: Postgres (migrated on boot) when configured, else in memory.
	var telemetry store.TelemetryStore = store.NewMemory()
	if cfg.DatabaseURL != "" {
		pg, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Printf("WARN: Postgres unavailable, using in-memory store (data will not persist): %v", err)
		} else {
			defer pg.Close()
			opts.Store = pg
			telemetry = store.NewPostgresTelemetry(pg.Pool())
			log.Println("Postgres connected; migrations applied")
		}
	} else {
		log.Println("DATABASE_URL not set; using in-memory store")
	}
	if opts.Store == nil {
		mem := store.NewMemory()
		opts.Store, telemetry = mem, mem
	}

	// Telemetry is buffered and written in batches (one COPY per interval).
	batcher := store.NewTelemetryBatcher(telemetry, store.BatcherOptions{
		Interval: cfg.TelemetryFlush, Retention: cfg.TelemetryRetention,
	})
	opts.Telemetry = batcher
	batchDone := make(chan struct{})
	go func() { batcher.Run(ctx); close(batchDone) }()

	// Redis: GEO mirror of driver positions and pub/sub for /v1/stream; both optional.
	var broker stream.Broker
	redisMon := redisdiag.NewMonitor(cfg.RedisURL, time.Minute, 5*time.Second)
	if cfg.RedisURL == "" {
		log.Println("REDIS_URL not set; driver index and /v1/stream run in memory only")
	} else {
		masked := cfg.RedisURL
		if info, err := redisdiag.Describe(cfg.RedisURL); err == nil {
			masked = info.URL
		}
		log.Printf("Redis: connecting to %s", masked)
		m, err := fleet.NewRedisMirror(ctx, cfg.RedisURL)
		if err != nil {
			log.Printf("ERROR: Redis %s unavailable, driver index runs in memory only: %v. %s", masked, err, redisdiag.Hint(err))
		} else {
			defer m.Close()
			opts.Mirror = m
		}
		b, err := stream.NewRedisBroker(ctx, cfg.RedisURL)
		if err != nil {
			log.Printf("ERROR: Redis %s pub/sub unavailable, /v1/stream fans out in-process only: %v. %s", masked, err, redisdiag.Hint(err))
		} else {
			defer b.Close()
			broker = b
		}
	}
	hub := stream.NewHub(stream.HubOptions{Broker: broker})
	go hub.Run(ctx)
	go redisMon.Run(ctx)

	reg := prometheus.NewRegistry()
	opts.Registry = reg
	opts.Publisher = hub
	svc, err := dispatch.New(opts)
	if err != nil {
		log.Fatalf("dispatch broker: %v", err)
	}

	// Safety: incident detection on every simulator tick; signals go to the
	// Safety Sentinel (agents-py) over HTTP and to Redis channel "safety" when available.
	safetyOpts := safety.Options{
		Config: safety.DefaultConfig(cfg.DemoMode), AgentsURL: cfg.AgentsURL, Stream: hub,
		Events: svc.RecordAgentEvent, Positions: svc.Fleet(), Telemetry: batcher,
	}
	if cfg.RedisURL != "" {
		if rc, err := safety.NewRedis(ctx, cfg.RedisURL); err != nil {
			log.Printf("WARN: safety signals will not be published on Redis: %v", err)
		} else {
			defer rc.Close()
			safetyOpts.Redis = rc
		}
	}
	safetySvc := safety.New(safetyOpts)

	routerOpts := []server.Option{server.WithDispatch(svc, reg), server.WithStream(hub), server.WithRedisMonitor(redisMon), server.WithSafety(safetySvc)}
	simDone := make(chan struct{})
	if cfg.DemoMode {
		var router sim.Router = sim.StraightLine{}
		if cfg.OSRMURL != "" {
			router = sim.NewOSRM(sim.OSRMOptions{BaseURL: cfg.OSRMURL, RPS: cfg.OSRMRPS})
		}
		demand := cfg.SimDemandPerMin
		if demand == 0 {
			demand = sim.AutoDemandPerMin(cfg.SimDrivers)
		}
		s, err := sim.New(sim.Options{
			Service: svc, Publisher: hub, Router: router, Drivers: cfg.SimDrivers,
			DemandPerMin: demand, AutoDemand: cfg.SimDemandPerMin == 0, Center: center, Telemetry: batcher, Registry: reg,
			Safety:  safetySvc,
			Watched: func(rideID string) bool { return hub.Subscribers(stream.RideTopic(rideID)) > 0 },
		})
		if err != nil {
			log.Fatalf("simulator: %v", err)
		}
		safetySvc.SetVehicles(s)
		if cfg.SimDrivers == 0 {
			zero := 0
			_, _ = s.Configure(ctx, sim.ConfigInput{Drivers: &zero})
		}
		routerOpts = append(routerOpts, server.WithSim(s))
		go func() { s.Run(ctx); close(simDone) }()
		log.Printf("DEMO_MODE: fleet simulator on (%d drivers, %.1f requests/min base demand, router %s)", cfg.SimDrivers, demand, routerName(cfg))
	} else {
		close(simDone)
	}
	router := server.NewRouter(cfg, routerOpts...)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second, // cleared for hijacked WebSocket connections
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("Starting dispatch-go gateway on %s (city centre %.4f, %.4f; geo=%s res=%d; store=%s; mirror=%s; stream=%s)",
			addr, cfg.CityCenterLat, cfg.CityCenterLng, geo.Backend(), cfg.H3Resolution, svc.StoreKind(), svc.MirrorKind(), hub.BrokerKind())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server listen failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down dispatch-go gateway...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stop() // stop simulator, hub and batcher (final telemetry flush)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}
	<-simDone
	<-batchDone
	log.Println("dispatch-go gateway stopped gracefully.")
}

func routerName(cfg *config.Config) string {
	if cfg.OSRMURL == "" {
		return "straight-line"
	}
	return fmt.Sprintf("OSRM %s @ %.1f req/s", cfg.OSRMURL, cfg.OSRMRPS)
}
