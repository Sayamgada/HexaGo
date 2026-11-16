package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestPostgresContract runs the store contract against a real Postgres when
// TEST_DATABASE_URL is set (e.g. the docker-compose instance). Each run uses a
// throwaway schema so it never touches application tables.
func TestPostgresContract(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	schema := fmt.Sprintf("dispatch_test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	pg, err := OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer pg.Close()
	if pg.Kind() != "postgres" {
		t.Errorf("Kind = %q", pg.Kind())
	}

	// Migrations are idempotent: a second run applies nothing and succeeds.
	if err := Migrate(ctx, pg.Pool()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var applied int
	if err := pg.Pool().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil || applied == 0 {
		t.Fatalf("schema_migrations count = %d, %v", applied, err)
	}

	contract(t, pg, NewPostgresTelemetry(pg.Pool()))

	// agent_events is append-only.
	if _, err := pg.Pool().Exec(ctx, "UPDATE agent_events SET agent = 'x'"); err == nil {
		t.Error("UPDATE on agent_events should be rejected")
	}
	if _, err := pg.Pool().Exec(ctx, "DELETE FROM agent_events"); err == nil {
		t.Error("DELETE on agent_events should be rejected")
	}
}

func TestOpenPostgresBadURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := OpenPostgres(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"); err == nil {
		t.Error("expected connection error")
	}
	if _, err := OpenPostgres(ctx, "::bad::"); err == nil {
		t.Error("expected parse error")
	}
}
