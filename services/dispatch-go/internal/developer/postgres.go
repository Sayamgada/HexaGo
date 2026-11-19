package developer

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres stores keys and webhooks in api_keys / webhooks (migration 0400).
type Postgres struct{ pool *pgxpool.Pool }

// NewPostgres returns a Store on pool (already migrated).
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Kind implements Store.
func (p *Postgres) Kind() string { return "postgres" }

// CreateKey implements Store.
func (p *Postgres) CreateKey(ctx context.Context, k Key) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO api_keys (id, name, prefix, key_hash, scopes, rate_limit_per_min, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, k.ID, k.Name, k.Prefix, k.Hash, k.Scopes, k.RateLimitPerMin, k.CreatedAt)
	return err
}

// KeyByHash implements Store.
func (p *Postgres) KeyByHash(ctx context.Context, hash string) (Key, error) {
	var k Key
	err := p.pool.QueryRow(ctx, `SELECT id, name, prefix, key_hash, scopes, rate_limit_per_min, created_at, revoked_at
		FROM api_keys WHERE key_hash = $1`, hash).
		Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.Scopes, &k.RateLimitPerMin, &k.CreatedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	return k, err
}

// RevokeKey implements Store; the key's webhooks are deleted with it.
func (p *Postgres) RevokeKey(ctx context.Context, id string, at time.Time) error {
	tag, err := p.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = p.pool.Exec(ctx, `UPDATE webhooks SET deleted_at = $2 WHERE key_id = $1 AND deleted_at IS NULL`, id, at)
	return err
}

// CreateWebhook implements Store.
func (p *Postgres) CreateWebhook(ctx context.Context, w Webhook) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO webhooks (id, key_id, url, events, secret, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, w.ID, w.KeyID, w.URL, w.Events, w.Secret, w.CreatedAt)
	return err
}

func (p *Postgres) query(ctx context.Context, where string, arg any) ([]Webhook, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, key_id, url, events, secret, created_at FROM webhooks
		WHERE deleted_at IS NULL AND `+where+` ORDER BY created_at, id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(&w.ID, &w.KeyID, &w.URL, &w.Events, &w.Secret, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Webhooks implements Store.
func (p *Postgres) Webhooks(ctx context.Context, keyID string) ([]Webhook, error) {
	return p.query(ctx, "key_id = $1", keyID)
}

// WebhooksFor implements Store.
func (p *Postgres) WebhooksFor(ctx context.Context, event string) ([]Webhook, error) {
	return p.query(ctx, "events @> ARRAY[$1]::text[]", event)
}

// DeleteWebhook implements Store.
func (p *Postgres) DeleteWebhook(ctx context.Context, keyID, id string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE webhooks SET deleted_at = now() WHERE id = $1 AND key_id = $2 AND deleted_at IS NULL`, id, keyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
