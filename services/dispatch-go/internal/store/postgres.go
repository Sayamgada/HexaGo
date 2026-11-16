package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the production Store.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to url, verifies the connection and runs migrations.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 25
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

// Pool exposes the connection pool (for the telemetry store and tests).
func (p *Postgres) Pool() *pgxpool.Pool { return p.pool }

// Kind implements Store.
func (p *Postgres) Kind() string { return "postgres" }

// Ping implements Store.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Close implements Store.
func (p *Postgres) Close() { p.pool.Close() }

// UpsertDriver implements Store.
func (p *Postgres) UpsertDriver(ctx context.Context, d DriverRecord) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO drivers (id, status, rating) VALUES ($1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, rating = EXCLUDED.rating, updated_at = now()`,
			d.ID, d.Status, d.Rating); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO vehicles (id, driver_id, tier, is_autonomous, battery_pct) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (driver_id) DO UPDATE SET tier = EXCLUDED.tier, is_autonomous = EXCLUDED.is_autonomous,
				battery_pct = EXCLUDED.battery_pct, updated_at = now()`,
			"veh-"+d.ID, d.ID, d.Tier, d.Tier == "av", d.BatteryPct)
		return err
	})
}

// CreateRide implements Store.
func (p *Postgres) CreateRide(ctx context.Context, r Ride) error {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO riders (id) VALUES ($1) ON CONFLICT DO NOTHING`, r.RiderID); err != nil {
			return err
		}
		if r.DriverID != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO drivers (id) VALUES ($1) ON CONFLICT DO NOTHING`, r.DriverID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO rides (id, rider_id, driver_id, tier, status, pickup_lat, pickup_lng, dropoff_lat, dropoff_lng,
				pickup_h3, eta_seconds, distance_km, match_score, cancel_reason, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
			r.ID, r.RiderID, nullStr(r.DriverID), r.Tier, string(r.Status), r.Pickup.Lat, r.Pickup.Lng,
			r.Dropoff.Lat, r.Dropoff.Lng, r.PickupH3, r.ETASeconds, r.DistanceKm, r.MatchScore,
			nullStr(r.CancelReason), r.CreatedAt, r.UpdatedAt); err != nil {
			return err
		}
		for _, l := range r.Legs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO ride_legs (ride_id, seq, kind, from_lat, from_lng, to_lat, to_lng, distance_km, eta_seconds)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				r.ID, l.Seq, l.Kind, l.From.Lat, l.From.Lng, l.To.Lat, l.To.Lng, l.DistanceKm, l.ETASeconds); err != nil {
				return err
			}
		}
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "rides_pkey" {
		return ErrConflict
	}
	return err
}

// UpdateRide implements Store.
func (p *Postgres) UpdateRide(ctx context.Context, r Ride) error {
	if r.DriverID != "" {
		if _, err := p.pool.Exec(ctx, `INSERT INTO drivers (id) VALUES ($1) ON CONFLICT DO NOTHING`, r.DriverID); err != nil {
			return err
		}
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE rides SET status = $2, driver_id = $3, eta_seconds = $4, match_score = $5, cancel_reason = $6, updated_at = $7
		WHERE id = $1`,
		r.ID, string(r.Status), nullStr(r.DriverID), r.ETASeconds, r.MatchScore, nullStr(r.CancelReason), r.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const rideColumns = `id, rider_id, COALESCE(driver_id, ''), tier, status, pickup_lat, pickup_lng, dropoff_lat, dropoff_lng,
	COALESCE(pickup_h3, ''), COALESCE(eta_seconds, 0), COALESCE(distance_km, 0), COALESCE(match_score, 0),
	COALESCE(cancel_reason, ''), created_at, updated_at`

func (p *Postgres) getRide(ctx context.Context, where string, arg any) (Ride, error) {
	var r Ride
	var status string
	err := p.pool.QueryRow(ctx, "SELECT "+rideColumns+" FROM rides WHERE "+where+" LIMIT 1", arg).Scan(
		&r.ID, &r.RiderID, &r.DriverID, &r.Tier, &status, &r.Pickup.Lat, &r.Pickup.Lng, &r.Dropoff.Lat, &r.Dropoff.Lng,
		&r.PickupH3, &r.ETASeconds, &r.DistanceKm, &r.MatchScore, &r.CancelReason, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ride{}, ErrNotFound
	}
	if err != nil {
		return Ride{}, err
	}
	r.Status = RideStatus(status)
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()

	rows, err := p.pool.Query(ctx, `
		SELECT seq, kind, from_lat, from_lng, to_lat, to_lng, distance_km, eta_seconds
		FROM ride_legs WHERE ride_id = $1 ORDER BY seq`, r.ID)
	if err != nil {
		return Ride{}, err
	}
	r.Legs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RideLeg, error) {
		var l RideLeg
		err := row.Scan(&l.Seq, &l.Kind, &l.From.Lat, &l.From.Lng, &l.To.Lat, &l.To.Lng, &l.DistanceKm, &l.ETASeconds)
		return l, err
	})
	return r, err
}

// GetRide implements Store.
func (p *Postgres) GetRide(ctx context.Context, id string) (Ride, error) {
	return p.getRide(ctx, "id = $1", id)
}

// ActiveRideForDriver implements Store.
func (p *Postgres) ActiveRideForDriver(ctx context.Context, driverID string) (Ride, error) {
	return p.getRide(ctx, "driver_id = $1 AND status IN ('matched', 'in_progress') ORDER BY created_at DESC", driverID)
}

// RideCounts implements Store.
func (p *Postgres) RideCounts(ctx context.Context) (RideCounts, error) {
	var c RideCounts
	err := p.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'requested'),
		       count(*) FILTER (WHERE status IN ('matched', 'in_progress'))
		FROM rides WHERE status IN ('requested', 'matched', 'in_progress')`).Scan(&c.Open, &c.InProgress)
	return c, err
}

// RecordEvent implements Store.
func (p *Postgres) RecordEvent(ctx context.Context, e AgentEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO agent_events (agent, event_type, ride_id, input, decision, confidence, fallback_used, token_cost, latency_ms, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.Agent, e.EventType, nullStr(e.RideID), jsonOrEmpty(e.Input), jsonOrEmpty(e.Decision),
		e.Confidence, e.FallbackUsed, e.TokenCost, e.LatencyMs, e.CreatedAt)
	return err
}

// RecentEvents implements Store.
func (p *Postgres) RecentEvents(ctx context.Context, limit int) ([]AgentEvent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, agent, event_type, COALESCE(ride_id, ''), input, decision, COALESCE(confidence, 0),
		       fallback_used, token_cost, COALESCE(latency_ms, 0), created_at
		FROM agent_events ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AgentEvent, error) {
		var e AgentEvent
		var in, dec []byte
		err := row.Scan(&e.ID, &e.Agent, &e.EventType, &e.RideID, &in, &dec, &e.Confidence,
			&e.FallbackUsed, &e.TokenCost, &e.LatencyMs, &e.CreatedAt)
		e.Input, e.Decision = in, dec
		return e, err
	})
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonOrEmpty(b json.RawMessage) string {
	if len(b) == 0 {
		return "{}"
	}
	return string(b)
}

// PostgresTelemetry stores telemetry in the telemetry_points table. It is a
// separate type so it can be replaced by a ScyllaDB-backed TelemetryStore.
type PostgresTelemetry struct {
	pool *pgxpool.Pool
}

// NewPostgresTelemetry returns a TelemetryStore on pool.
func NewPostgresTelemetry(pool *pgxpool.Pool) *PostgresTelemetry {
	return &PostgresTelemetry{pool: pool}
}

// AppendTelemetry implements TelemetryStore.
func (t *PostgresTelemetry) AppendTelemetry(ctx context.Context, p TelemetryPoint) error {
	_, err := t.pool.Exec(ctx, `
		INSERT INTO telemetry_points (driver_id, lat, lng, heading, speed_kmh, status, h3_cell, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.DriverID, p.Lat, p.Lng, p.Heading, p.SpeedKmh, p.Status, p.H3Cell, p.RecordedAt)
	return err
}

// RecentTelemetry implements TelemetryStore.
func (t *PostgresTelemetry) RecentTelemetry(ctx context.Context, driverID string, limit int) ([]TelemetryPoint, error) {
	if limit <= 0 {
		limit = maxTelemetryPerDriver
	}
	rows, err := t.pool.Query(ctx, `
		SELECT driver_id, lat, lng, heading, speed_kmh, COALESCE(status, ''), COALESCE(h3_cell, ''), recorded_at
		FROM telemetry_points WHERE driver_id = $1 ORDER BY recorded_at DESC LIMIT $2`, driverID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TelemetryPoint, error) {
		var p TelemetryPoint
		err := row.Scan(&p.DriverID, &p.Lat, &p.Lng, &p.Heading, &p.SpeedKmh, &p.Status, &p.H3Cell, &p.RecordedAt)
		p.RecordedAt = p.RecordedAt.UTC()
		return p, err
	})
}
