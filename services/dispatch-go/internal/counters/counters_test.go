package counters

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func TestDayStartIST(t *testing.T) {
	cases := []struct{ in, want string }{
		// 00:10 IST on the 19th is 18:40 UTC on the 18th.
		{"2026-11-18T18:40:00Z", "2026-11-18T18:30:00Z"},
		{"2026-11-18T18:29:59Z", "2026-11-17T18:30:00Z"},
		{"2026-11-19T12:00:00Z", "2026-11-18T18:30:00Z"},
	}
	for _, c := range cases {
		in, _ := time.Parse(time.RFC3339, c.in)
		if got := DayStartIST(in).UTC().Format(time.RFC3339); got != c.want {
			t.Errorf("DayStartIST(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestConsistent(t *testing.T) {
	c := Counts{MatchesToday: 3, RidesSettledToday: 5, DecisionsToday: 4}.Consistent()
	if c.MatchesToday != 5 || c.DecisionsToday != 5 || c.RidesSettledToday != 5 {
		t.Fatalf("got %+v", c)
	}
	ok := Counts{MatchesToday: 9, RidesSettledToday: 5, DecisionsToday: 40}.Consistent()
	if ok.MatchesToday != 9 || ok.DecisionsToday != 40 {
		t.Fatalf("already consistent counts changed: %+v", ok)
	}
}

type fakeQ struct {
	calls int
	err   error
	c     Counts
	since []time.Time
}

func (f *fakeQ) Kind() string { return "fake" }
func (f *fakeQ) Daily(_ context.Context, since time.Time) (Counts, error) {
	f.calls++
	f.since = append(f.since, since)
	if f.err != nil {
		return Counts{}, f.err
	}
	return f.c, nil
}

func TestCacheTTLAndStaleOnError(t *testing.T) {
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	q := &fakeQ{c: Counts{MatchesToday: 10, RidesSettledToday: 4, DecisionsToday: 30}}
	c := NewCache(q, 0, func() time.Time { return now })
	if c.Kind() != "fake" {
		t.Errorf("Kind = %q", c.Kind())
	}
	got, err := c.Get(context.Background())
	if err != nil || got.MatchesToday != 10 || q.calls != 1 {
		t.Fatalf("first Get: %+v %v calls=%d", got, err, q.calls)
	}
	if !got.Since.Equal(DayStartIST(now)) || !got.AsOf.Equal(now) {
		t.Errorf("since/as_of = %v / %v", got.Since, got.AsOf)
	}

	now = now.Add(9 * time.Second)
	q.c.MatchesToday = 11
	if got, _ = c.Get(context.Background()); got.MatchesToday != 10 || q.calls != 1 {
		t.Fatalf("within TTL must be cached: %+v calls=%d", got, q.calls)
	}
	now = now.Add(2 * time.Second)
	if got, _ = c.Get(context.Background()); got.MatchesToday != 11 || q.calls != 2 {
		t.Fatalf("after TTL must refresh: %+v calls=%d", got, q.calls)
	}

	// A failing query serves the last good value of the same day.
	now = now.Add(time.Minute)
	q.err = errors.New("db down")
	if got, err = c.Get(context.Background()); err != nil || got.MatchesToday != 11 {
		t.Fatalf("stale fallback: %+v %v", got, err)
	}

	// ...but never yesterday's numbers after midnight IST.
	now = DayStartIST(now).Add(24*time.Hour + time.Second)
	if _, err = c.Get(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("new day with failing db: err = %v", err)
	}
	q.err = nil
	q.c = Counts{MatchesToday: 1}
	if got, _ = c.Get(context.Background()); got.MatchesToday != 1 || !got.Since.Equal(DayStartIST(now)) {
		t.Fatalf("new day: %+v", got)
	}

	// Invalidate forces a query inside the TTL.
	calls := q.calls
	c.Invalidate()
	if _, _ = c.Get(context.Background()); q.calls != calls+1 {
		t.Fatal("Invalidate did not force a query")
	}
}

func TestCacheResetsAtMidnightEvenWithinTTL(t *testing.T) {
	now := time.Date(2026, 11, 19, 18, 29, 58, 0, time.UTC) // 23:59:58 IST
	q := &fakeQ{c: Counts{MatchesToday: 50}}
	c := NewCache(q, time.Minute, func() time.Time { return now })
	_, _ = c.Get(context.Background())
	now = now.Add(3 * time.Second) // 00:00:01 IST
	q.c = Counts{}
	got, _ := c.Get(context.Background())
	if q.calls != 2 || got.MatchesToday != 0 {
		t.Fatalf("midnight must reset: %+v calls=%d", got, q.calls)
	}
}

func TestFuncs(t *testing.T) {
	since := time.Date(2026, 11, 18, 18, 30, 0, 0, time.UTC)
	f := Funcs{
		Events:  func(context.Context, time.Time) (int64, int64, error) { return 2, 9, nil },
		Settled: func(context.Context, time.Time) (int64, error) { return 3, nil },
	}
	c, err := f.Daily(context.Background(), since)
	if err != nil || c.MatchesToday != 2 || c.DecisionsToday != 9 || c.RidesSettledToday != 3 || c.Source != "memory" {
		t.Fatalf("got %+v %v", c, err)
	}
	if (Funcs{Name: "x"}).Kind() != "x" {
		t.Error("custom name")
	}
	boom := errors.New("boom")
	if _, err := (Funcs{Events: func(context.Context, time.Time) (int64, int64, error) { return 0, 0, boom }}).Daily(context.Background(), since); !errors.Is(err, boom) {
		t.Errorf("events error: %v", err)
	}
	if _, err := (Funcs{Settled: func(context.Context, time.Time) (int64, error) { return 0, boom }}).Daily(context.Background(), since); !errors.Is(err, boom) {
		t.Errorf("settled error: %v", err)
	}
	// Through the cache the clamp applies: 3 settled implies >= 3 matches.
	got, _ := NewCache(f, 0, func() time.Time { return since.Add(time.Hour) }).Get(context.Background())
	if got.MatchesToday != 3 {
		t.Errorf("cache must return consistent counts: %+v", got)
	}
}

// TestPostgresDaily runs the counter query against a migrated throwaway schema
// when TEST_DATABASE_URL is set (CI), including a "restart": a second querier
// on a fresh pool sees the same numbers.
func TestPostgresDaily(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := fmt.Sprintf("counters_test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	open := func() *store.Postgres {
		pg, err := store.OpenPostgres(ctx, u.String())
		if err != nil {
			t.Fatalf("OpenPostgres: %v", err)
		}
		return pg
	}
	pg := open()
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC) // 11:30 IST
	since := DayStartIST(now)
	yesterday := since.Add(-time.Hour)
	ev := func(agent, typ string, at time.Time) {
		if err := pg.RecordEvent(ctx, store.AgentEvent{Agent: agent, EventType: typ, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	ev(DispatchAgent, MatchEvent, yesterday)
	ev(DispatchAgent, MatchEvent, since.Add(time.Minute))
	ev(DispatchAgent, MatchEvent, since.Add(2*time.Minute))
	ev(DispatchAgent, "no_match", since.Add(3*time.Minute))
	ev("settlement", "settlement_scan", since.Add(4*time.Minute))
	ev("ops_supervisor", "heartbeat", yesterday)
	if _, err := pg.Pool().Exec(ctx, `INSERT INTO accounts (id, type, owner, guarded) VALUES
		('platform_revenue:hexago', 'platform_revenue', 'hexago', false),
		('payment_clearing:hexago', 'payment_clearing', 'hexago', false)`); err != nil {
		t.Fatal(err)
	}
	entry := func(id, kind, ride string, at time.Time) {
		tx, err := pg.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, kind, idempotency_key, ride_id, created_at)
			VALUES ($1, $2, $3, $4, $5)`, id, kind, "k:"+id, ride, at); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO postings (entry_id, account_id, amount_paise) VALUES
			($1, 'payment_clearing:hexago', 100), ($1, 'platform_revenue:hexago', -100)`, id); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	entry("e1", "ride_settlement", "r1", since.Add(5*time.Minute))
	entry("e2", "ride_settlement", "r0", yesterday)
	entry("e3", "refund", "r1", since.Add(6*time.Minute))

	got, err := NewPostgres(pg.Pool()).Daily(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if got.MatchesToday != 2 || got.RidesSettledToday != 1 || got.DecisionsToday != 4 || got.Source != "postgres" {
		t.Fatalf("got %+v", got)
	}
	pg.Close()

	// "Restart": a new pool and a cold cache report the same, consistent numbers.
	pg2 := open()
	defer pg2.Close()
	cold, err := NewCache(NewPostgres(pg2.Pool()), 0, func() time.Time { return now }).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cold.MatchesToday != got.MatchesToday || cold.DecisionsToday != got.DecisionsToday ||
		cold.RidesSettledToday != got.RidesSettledToday || cold.MatchesToday < cold.RidesSettledToday {
		t.Fatalf("after restart %+v, before %+v", cold, got)
	}
}
