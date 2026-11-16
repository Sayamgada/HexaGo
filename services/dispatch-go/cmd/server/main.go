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
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()

	opts := dispatch.Options{
		H3Resolution:    cfg.H3Resolution,
		Center:          geo.Point{Lat: cfg.CityCenterLat, Lng: cfg.CityCenterLng},
		CitySpeedFactor: cfg.CitySpeedFactor,
	}

	// Persistence: Postgres (migrated on boot) when configured, else in memory.
	if cfg.DatabaseURL != "" {
		pg, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Printf("WARN: Postgres unavailable, using in-memory store (data will not persist): %v", err)
		} else {
			defer pg.Close()
			opts.Store = pg
			opts.Telemetry = store.NewPostgresTelemetry(pg.Pool())
			log.Println("Postgres connected; migrations applied")
		}
	} else {
		log.Println("DATABASE_URL not set; using in-memory store")
	}

	// Redis GEO mirror of driver positions; optional.
	if cfg.RedisURL != "" {
		m, err := fleet.NewRedisMirror(ctx, cfg.RedisURL)
		if err != nil {
			log.Printf("WARN: Redis unavailable, driver index runs in memory only: %v", err)
		} else {
			defer m.Close()
			opts.Mirror = m
		}
	}

	reg := prometheus.NewRegistry()
	opts.Registry = reg
	svc, err := dispatch.New(opts)
	if err != nil {
		log.Fatalf("dispatch broker: %v", err)
	}
	router := server.NewRouter(cfg, server.WithDispatch(svc, reg))

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("Starting dispatch-go gateway on %s (city centre %.4f, %.4f; geo=%s res=%d; store=%s; mirror=%s)",
			addr, cfg.CityCenterLat, cfg.CityCenterLng, geo.Backend(), cfg.H3Resolution, svc.StoreKind(), svc.MirrorKind())
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
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}
	log.Println("dispatch-go gateway stopped gracefully.")
}
