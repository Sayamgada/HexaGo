package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is the pg_advisory_lock key that serialises concurrent boots.
const migrationLockID = 0x4845584147 // "HEXAG"

type migration struct {
	version string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: strings.TrimSuffix(e.Name(), ".sql"), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func containsTable(sql, table string) bool {
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+` + regexp.QuoteMeta(table) + `\b`)
	return re.MatchString(sql)
}

// Migrate applies pending embedded migrations in order, each in its own
// transaction. A Postgres advisory lock makes concurrent boots safe.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := loadMigrations()
	if err != nil {
		return fmt.Errorf("store: load migrations: %w", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("store: migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("store: schema_migrations: %w", err)
	}
	for _, m := range ms {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", m.version).Scan(&exists); err != nil {
			return fmt.Errorf("store: check %s: %w", m.version, err)
		}
		if exists {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// No arguments: pgx uses the simple protocol, which allows multiple statements.
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.version)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: apply %s: %w", m.version, err)
		}
	}
	return nil
}
