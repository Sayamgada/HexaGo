package scheduler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the subset of *pgxpool.Pool the Postgres repo uses.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PGRepo writes legs to the scheduled_legs table (migration 0300_scheduler_legs.sql).
type PGRepo struct{ db DB }

// NewPGRepo returns a repo over db.
func NewPGRepo(db DB) *PGRepo { return &PGRepo{db: db} }

// Save upserts a leg; columns hold what is queried, `data` the whole leg.
func (r *PGRepo) Save(ctx context.Context, l Leg) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `INSERT INTO scheduled_legs
		(id, plan_id, rider_id, seq, status, ride_id, pickup_at, data, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, ride_id = EXCLUDED.ride_id,
			data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`,
		l.ID, l.PlanID, l.RiderID, l.Seq, string(l.Status), l.RideID, l.PickupAt, data, l.CreatedAt, l.UpdatedAt)
	return err
}

// Load returns the legs that still need work.
func (r *PGRepo) Load(ctx context.Context) ([]Leg, error) {
	rows, err := r.db.Query(ctx, `SELECT data FROM scheduled_legs
		WHERE status IN ('scheduled', 'dispatched') ORDER BY created_at, plan_id, seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Leg
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var l Leg
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("scheduled_legs: decode: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
