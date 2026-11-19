// Package counters computes the Ops "today" counters (matches, rides settled,
// autonomous decisions) from the persistent audit trail and ledger rather than
// from in-process counters, so they survive restarts.
//
// "Today" starts at midnight IST. Results are cached for TTL (10 s) so the
// metrics stream (pushed every few seconds) costs at most one query per TTL.
package counters

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TTL is how long a computed result is served before the next query.
const TTL = 10 * time.Second

// IST is India Standard Time (UTC+05:30, no DST).
var IST = time.FixedZone("IST", 5*3600+1800)

// DayStartIST returns midnight IST of the day containing t.
func DayStartIST(t time.Time) time.Time {
	l := t.In(IST)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, IST)
}

// Counts are the persistent "today" counters.
type Counts struct {
	MatchesToday      int64     `json:"matches_today"`
	RidesSettledToday int64     `json:"rides_settled_today"`
	DecisionsToday    int64     `json:"decisions_today"`
	Since             time.Time `json:"since"`
	Source            string    `json:"source"`
	AsOf              time.Time `json:"as_of"`
}

// Consistent enforces decisions >= matches >= rides settled. Every settled
// ride was matched and every match is an agent decision, so the raw counts
// already agree except for a ride matched before midnight and settled after;
// that ride's match is credited to today.
func (c Counts) Consistent() Counts {
	if c.MatchesToday < c.RidesSettledToday {
		c.MatchesToday = c.RidesSettledToday
	}
	if c.DecisionsToday < c.MatchesToday {
		c.DecisionsToday = c.MatchesToday
	}
	return c
}

// Querier computes the raw counts since a moment.
type Querier interface {
	Kind() string
	Daily(ctx context.Context, since time.Time) (Counts, error)
}

// DispatchAgent and MatchEvent identify dispatch-go's match rows in agent_events.
const (
	DispatchAgent = "dispatch_broker"
	MatchEvent    = "match"
	settlement    = "ride_settlement"
)

// PostgresQuery is the one round trip behind the Postgres querier.
const PostgresQuery = `
SELECT
  (SELECT count(*) FROM agent_events WHERE agent = $2 AND event_type = $3 AND created_at >= $1),
  (SELECT count(DISTINCT ride_id) FROM journal_entries WHERE kind = $4 AND created_at >= $1),
  (SELECT count(*) FROM agent_events WHERE created_at >= $1)`

// Postgres counts agent_events and journal_entries rows.
type Postgres struct{ pool *pgxpool.Pool }

// NewPostgres returns a querier over pool (agent_events and journal_entries must exist).
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Kind implements Querier.
func (p *Postgres) Kind() string { return "postgres" }

// Daily implements Querier.
func (p *Postgres) Daily(ctx context.Context, since time.Time) (Counts, error) {
	c := Counts{Since: since, Source: p.Kind()}
	err := p.pool.QueryRow(ctx, PostgresQuery, since, DispatchAgent, MatchEvent, settlement).
		Scan(&c.MatchesToday, &c.RidesSettledToday, &c.DecisionsToday)
	return c, err
}

// Funcs builds a Querier from functions (the in-memory stores).
type Funcs struct {
	Name string
	// Events returns dispatch matches and all agent decisions since `since`.
	Events func(ctx context.Context, since time.Time) (matches, decisions int64, err error)
	// Settled returns the rides settled since `since`.
	Settled func(ctx context.Context, since time.Time) (int64, error)
}

// Kind implements Querier.
func (f Funcs) Kind() string {
	if f.Name == "" {
		return "memory"
	}
	return f.Name
}

// Daily implements Querier.
func (f Funcs) Daily(ctx context.Context, since time.Time) (Counts, error) {
	c := Counts{Since: since, Source: f.Kind()}
	var err error
	if f.Events != nil {
		if c.MatchesToday, c.DecisionsToday, err = f.Events(ctx, since); err != nil {
			return Counts{}, err
		}
	}
	if f.Settled != nil {
		if c.RidesSettledToday, err = f.Settled(ctx, since); err != nil {
			return Counts{}, err
		}
	}
	return c, nil
}

// ErrUnavailable is returned before the first successful query when the querier fails.
var ErrUnavailable = errors.New("counters: unavailable")

// Cache serves Querier results for TTL.
type Cache struct {
	q   Querier
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	last Counts
	at   time.Time
	ok   bool
}

// NewCache wraps q. ttl <= 0 selects TTL; now == nil selects time.Now.
func NewCache(q Querier, ttl time.Duration, now func() time.Time) *Cache {
	if ttl <= 0 {
		ttl = TTL
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{q: q, ttl: ttl, now: now}
}

// Kind names the underlying querier.
func (c *Cache) Kind() string { return c.q.Kind() }

// Get returns today's counts, querying at most once per TTL. On a query
// error it serves the previous result of the same day if there is one.
func (c *Cache) Get(ctx context.Context) (Counts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	since := DayStartIST(now)
	if c.ok && now.Sub(c.at) < c.ttl && c.last.Since.Equal(since) {
		return c.last, nil
	}
	got, err := c.q.Daily(ctx, since)
	if err != nil {
		if c.ok && c.last.Since.Equal(since) {
			return c.last, nil
		}
		return Counts{Since: since, Source: c.q.Kind()}, errors.Join(ErrUnavailable, err)
	}
	got.Since, got.AsOf = since, now.UTC()
	c.last, c.at, c.ok = got.Consistent(), now, true
	return c.last, nil
}

// Invalidate forces the next Get to query.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.ok = false
	c.mu.Unlock()
}
