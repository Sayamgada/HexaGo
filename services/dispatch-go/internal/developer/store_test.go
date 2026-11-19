package developer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func testStoreBehaviour(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	k := Key{ID: "key_1", Name: "n", Prefix: "hxg_ab", Hash: HashKey("hxg_secret"), Scopes: []string{ScopeRidesRead},
		RateLimitPerMin: 60, CreatedAt: now}
	if err := s.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	got, err := s.KeyByHash(ctx, k.Hash)
	if err != nil || got.ID != "key_1" || !got.HasScope(ScopeRidesRead) || got.HasScope(ScopeRidesWrite) || got.RevokedAt != nil {
		t.Fatalf("KeyByHash: %+v %v", got, err)
	}
	if _, err := s.KeyByHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}
	k2 := Key{ID: "key_2", Name: "other", Prefix: "hxg_cd", Hash: HashKey("hxg_other"), Scopes: AllScopes(), RateLimitPerMin: 10, CreatedAt: now}
	if err := s.CreateKey(ctx, k2); err != nil {
		t.Fatal(err)
	}

	w1 := Webhook{ID: "wh_1", KeyID: "key_1", URL: "https://a.example/h", Events: []string{EventRideCompleted, EventPayoutSent}, Secret: "s1", CreatedAt: now}
	w2 := Webhook{ID: "wh_2", KeyID: "key_1", URL: "https://b.example/h", Events: []string{EventIncidentOpened}, Secret: "s2", CreatedAt: now.Add(time.Second)}
	w3 := Webhook{ID: "wh_3", KeyID: "key_2", URL: "https://c.example/h", Events: []string{EventRideCompleted}, Secret: "s3", CreatedAt: now.Add(2 * time.Second)}
	for _, w := range []Webhook{w1, w2, w3} {
		if err := s.CreateWebhook(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	mine, _ := s.Webhooks(ctx, "key_1")
	if len(mine) != 2 || mine[0].ID != "wh_1" || mine[1].ID != "wh_2" || mine[0].Secret != "s1" {
		t.Fatalf("Webhooks(key_1) = %+v", mine)
	}
	rides, _ := s.WebhooksFor(ctx, EventRideCompleted)
	if len(rides) != 2 || rides[0].ID != "wh_1" || rides[1].ID != "wh_3" {
		t.Fatalf("WebhooksFor(ride.completed) = %+v", rides)
	}
	if none, _ := s.WebhooksFor(ctx, "nothing"); len(none) != 0 {
		t.Fatalf("WebhooksFor(nothing) = %+v", none)
	}
	if err := s.DeleteWebhook(ctx, "key_2", "wh_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete other key's webhook: %v", err)
	}
	if err := s.DeleteWebhook(ctx, "key_1", "wh_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWebhook(ctx, "key_1", "wh_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
	if err := s.RevokeKey(ctx, "key_1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, "key_1", now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
	got, _ = s.KeyByHash(ctx, k.Hash)
	if got.RevokedAt == nil {
		t.Fatal("revoked key has no revoked_at")
	}
	if left, _ := s.Webhooks(ctx, "key_1"); len(left) != 0 {
		t.Fatalf("revoked key kept webhooks: %+v", left)
	}
	if left, _ := s.WebhooksFor(ctx, EventRideCompleted); len(left) != 1 || left[0].ID != "wh_3" {
		t.Fatalf("after revoke: %+v", left)
	}
}

func TestMemoryStore(t *testing.T) {
	m := NewMemory()
	if m.Kind() != "memory" {
		t.Fatal(m.Kind())
	}
	testStoreBehaviour(t, m)
}

func TestPostgresStore(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres developer-store tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := fmt.Sprintf("developer_test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), base); err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	p := NewPostgres(pool)
	if p.Kind() != "postgres" {
		t.Fatal(p.Kind())
	}
	testStoreBehaviour(t, p)

	// The database enforces the rate-limit range and unique hashes.
	bad := Key{ID: "key_bad", Name: "n", Prefix: "p", Hash: "h-bad", Scopes: []string{}, RateLimitPerMin: 0, CreatedAt: time.Now()}
	if err := p.CreateKey(ctx, bad); err == nil {
		t.Fatal("rate_limit_per_min 0 accepted")
	}
	dup := Key{ID: "key_dup", Name: "n", Prefix: "p", Hash: HashKey("hxg_other"), Scopes: []string{}, RateLimitPerMin: 5, CreatedAt: time.Now()}
	if err := p.CreateKey(ctx, dup); err == nil {
		t.Fatal("duplicate hash accepted")
	}
	// Errors from a closed pool surface.
	pool.Close()
	if _, err := p.KeyByHash(ctx, "x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("closed pool: %v", err)
	}
	if err := p.RevokeKey(ctx, "x", time.Now()); err == nil {
		t.Fatal("closed pool revoke")
	}
	if _, err := p.Webhooks(ctx, "x"); err == nil {
		t.Fatal("closed pool list")
	}
	if err := p.DeleteWebhook(ctx, "x", "y"); err == nil {
		t.Fatal("closed pool delete")
	}
}

func TestKeyHelpers(t *testing.T) {
	a, b := NewSecret(KeyPrefix, 24), NewSecret(KeyPrefix, 24)
	if a == b || len(a) != len(KeyPrefix)+48 || !LooksLikeKey(a) {
		t.Fatalf("NewSecret: %s %s", a, b)
	}
	for _, s := range []string{"", "hxg_", "hxg_short", "abc_0123456789", "hxg_" + string(make([]byte, 200))} {
		if LooksLikeKey(s) {
			t.Errorf("LooksLikeKey(%q)", s)
		}
	}
	if HashKey("x") == HashKey("y") || len(HashKey("x")) != 64 {
		t.Fatal("HashKey")
	}
	w := Webhook{Events: []string{EventPayoutSent}}
	if !w.Subscribed(EventPayoutSent) || w.Subscribed(EventRideCompleted) {
		t.Fatal("Subscribed")
	}
}
