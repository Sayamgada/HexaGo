// Package fleet keeps the live, in-memory driver index used by the dispatch
// broker. Drivers are sharded by ID (for updates) and by H3 cell (for spatial
// lookups); every shard has its own RW lock so location pings and match
// queries for different areas never contend.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Status is a driver's availability.
type Status string

// Driver statuses.
const (
	StatusOffline Status = "offline"
	StatusIdle    Status = "idle"
	StatusEnRoute Status = "en_route" // heading to a pickup
	StatusOnTrip  Status = "on_trip"
)

// ParseStatus parses a status name case-insensitively.
func ParseStatus(s string) (Status, error) {
	st := Status(strings.ToLower(strings.TrimSpace(s)))
	switch st {
	case StatusOffline, StatusIdle, StatusEnRoute, StatusOnTrip:
		return st, nil
	}
	return "", fmt.Errorf("%w: unknown status %q", ErrInvalidDriver, s)
}

// Tier is a vehicle service class.
type Tier string

// Vehicle tiers.
const (
	TierAuto    Tier = "auto"
	TierMini    Tier = "mini"
	TierSedan   Tier = "sedan"
	TierPremium Tier = "premium"
	TierXL      Tier = "xl"
	TierAV      Tier = "av" // autonomous vehicle (battery-electric)
)

var allTiers = []Tier{TierAuto, TierMini, TierSedan, TierPremium, TierXL, TierAV}

// Tiers returns every supported tier.
func Tiers() []Tier { return append([]Tier(nil), allTiers...) }

// upgradeLadder is the free-upgrade order: auto -> mini -> sedan -> premium.
// XL (a size class) and AVs never upgrade.
var upgradeLadder = []Tier{TierAuto, TierMini, TierSedan, TierPremium}

// maxUpgradeSteps caps how far up the ladder a free upgrade may go.
const maxUpgradeSteps = 2

// UpgradeChain returns the tiers t may be upgraded to, nearest first.
func UpgradeChain(t Tier) []Tier {
	for i, x := range upgradeLadder {
		if x == t {
			end := min(i+1+maxUpgradeSteps, len(upgradeLadder))
			return append([]Tier(nil), upgradeLadder[i+1:end]...)
		}
	}
	return nil
}

// ParseTier parses a tier name case-insensitively.
func ParseTier(s string) (Tier, error) {
	t := Tier(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range allTiers {
		if t == known {
			return t, nil
		}
	}
	return "", fmt.Errorf("%w: unknown tier %q", ErrInvalidDriver, s)
}

// Driver is a snapshot of one driver's live state.
type Driver struct {
	ID         string    `json:"id"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	Heading    float64   `json:"heading"`   // degrees clockwise from north, [0, 360)
	SpeedKmh   float64   `json:"speed_kmh"` // current ground speed
	Status     Status    `json:"status"`
	Tier       Tier      `json:"tier"`
	BatteryPct float64   `json:"battery_pct,omitempty"` // AVs only
	Rating     float64   `json:"rating,omitempty"`      // 1..5, 0 = unrated
	IdleSince  time.Time `json:"idle_since,omitempty"`  // zero unless idle
	UpdatedAt  time.Time `json:"updated_at"`
	Cell       geo.Cell  `json:"h3_cell"`
}

// Point returns the driver's position.
func (d Driver) Point() geo.Point { return geo.Point{Lat: d.Lat, Lng: d.Lng} }

func (d *Driver) validate() error {
	if d.ID == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidDriver)
	}
	if err := d.Point().Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDriver, err)
	}
	if _, err := ParseStatus(string(d.Status)); err != nil {
		return err
	}
	if _, err := ParseTier(string(d.Tier)); err != nil {
		return err
	}
	if !inRange(d.BatteryPct, 0, 100) {
		return fmt.Errorf("%w: battery_pct %v not in [0,100]", ErrInvalidDriver, d.BatteryPct)
	}
	if !inRange(d.Rating, 0, 5) {
		return fmt.Errorf("%w: rating %v not in [0,5]", ErrInvalidDriver, d.Rating)
	}
	if !inRange(d.SpeedKmh, 0, 300) || math.IsNaN(d.Heading) || math.IsInf(d.Heading, 0) {
		return fmt.Errorf("%w: invalid speed/heading", ErrInvalidDriver)
	}
	return nil
}

func inRange(v, lo, hi float64) bool { return v >= lo && v <= hi }

func normalizeHeading(h float64) float64 { return math.Mod(math.Mod(h, 360)+360, 360) }

// Errors returned by the index.
var (
	ErrInvalidDriver = errors.New("fleet: invalid driver")
	ErrNotFound      = errors.New("fleet: driver not found")
	ErrConflict      = errors.New("fleet: driver status changed concurrently")
)

// Mirror receives position updates for an external geospatial index (Redis GEO).
type Mirror interface {
	Set(ctx context.Context, d Driver) error
	Remove(ctx context.Context, id string) error
}

// Counts summarises the fleet by status.
type Counts struct {
	Total   int `json:"total"`
	Active  int `json:"active"` // not offline
	Idle    int `json:"idle"`
	EnRoute int `json:"en_route"`
	OnTrip  int `json:"on_trip"`
	Offline int `json:"offline"`
}

const shardCount = 64

type idShard struct {
	mu sync.Mutex
	m  map[string]Driver
}

type cellShard struct {
	mu sync.RWMutex
	m  map[geo.Cell]map[string]Driver
}

// Index is a concurrency-safe driver index.
//
// Lock order: an ID shard lock may be held while taking a cell shard lock,
// never the reverse, and at most one cell shard lock is held at a time.
type Index struct {
	geo     geo.Indexer
	seed    maphash.Seed
	ids     [shardCount]idShard
	cells   [shardCount]cellShard
	now     func() time.Time
	mirror  Mirror
	logger  *log.Logger
	timeout time.Duration

	byStatus      [4]atomic.Int64 // offline, idle, en_route, on_trip
	mirrorFailing atomic.Bool
}

// Option configures an Index.
type Option func(*Index)

// WithMirror mirrors positions to an external index (e.g. Redis GEO).
func WithMirror(m Mirror) Option { return func(i *Index) { i.mirror = m } }

// WithClock overrides time.Now (tests).
func WithClock(now func() time.Time) Option { return func(i *Index) { i.now = now } }

// WithLogger sets the logger for mirror warnings.
func WithLogger(l *log.Logger) Option { return func(i *Index) { i.logger = l } }

// New returns an empty index that buckets drivers into cells from g.
func New(g geo.Indexer, opts ...Option) *Index {
	idx := &Index{geo: g, seed: maphash.MakeSeed(), now: time.Now, logger: log.Default(), timeout: 250 * time.Millisecond}
	for i := range idx.ids {
		idx.ids[i].m = make(map[string]Driver)
		idx.cells[i].m = make(map[geo.Cell]map[string]Driver)
	}
	for _, o := range opts {
		o(idx)
	}
	return idx
}

func (i *Index) idShardFor(id string) *idShard {
	return &i.ids[maphash.String(i.seed, id)%shardCount]
}

func (i *Index) cellShardFor(c geo.Cell) *cellShard {
	// Mix the bits: neighbouring H3 cells differ mostly in low-middle bits.
	h := uint64(c) * 0x9E3779B97F4A7C15
	return &i.cells[h>>58]
}

func statusSlot(s Status) int {
	switch s {
	case StatusIdle:
		return 1
	case StatusEnRoute:
		return 2
	case StatusOnTrip:
		return 3
	}
	return 0
}

// Apply atomically creates or mutates driver id. fn receives a copy of the
// current state (zero value with exists=false for a new driver); returning an
// error aborts without changes. The ID is pinned, the heading normalised, the
// cell recomputed and IdleSince/UpdatedAt maintained automatically.
func (i *Index) Apply(id string, fn func(d *Driver, exists bool) error) (Driver, error) {
	return i.apply(id, fn, true)
}

// ApplyLocal is Apply without the synchronous mirror write. High-frequency
// writers (the fleet simulator) use it and call SyncMirror periodically.
func (i *Index) ApplyLocal(id string, fn func(d *Driver, exists bool) error) (Driver, error) {
	return i.apply(id, fn, false)
}

// BatchMirror is implemented by mirrors that can write many drivers at once.
type BatchMirror interface {
	SetMany(ctx context.Context, ds []Driver) error
}

// SyncMirror writes the current position of every non-offline driver to the
// mirror in one batch (one call per driver if the mirror cannot batch).
func (i *Index) SyncMirror() {
	if i.mirror == nil {
		return
	}
	ds := i.Snapshot()
	active := ds[:0]
	for _, d := range ds {
		if d.Status != StatusOffline {
			active = append(active, d)
		}
	}
	if bm, ok := i.mirror.(BatchMirror); ok {
		i.mirrorDo(func(ctx context.Context) error { return bm.SetMany(ctx, active) })
		return
	}
	for _, d := range active {
		i.mirrorDo(func(ctx context.Context) error { return i.mirror.Set(ctx, d) })
	}
}

// Snapshot returns a copy of every indexed driver (unordered).
func (i *Index) Snapshot() []Driver {
	out := make([]Driver, 0, i.Len())
	for s := range i.ids {
		sh := &i.ids[s]
		sh.mu.Lock()
		for _, d := range sh.m {
			out = append(out, d)
		}
		sh.mu.Unlock()
	}
	return out
}

func (i *Index) apply(id string, fn func(d *Driver, exists bool) error, mirror bool) (Driver, error) {
	sh := i.idShardFor(id)
	sh.mu.Lock()
	old, exists := sh.m[id]
	next := old
	if err := fn(&next, exists); err != nil {
		sh.mu.Unlock()
		return Driver{}, err
	}
	next.ID = id
	next.Heading = normalizeHeading(next.Heading)
	if err := next.validate(); err != nil {
		sh.mu.Unlock()
		return Driver{}, err
	}
	cell, err := i.geo.CellOf(next.Point())
	if err != nil {
		sh.mu.Unlock()
		return Driver{}, fmt.Errorf("%w: %v", ErrInvalidDriver, err)
	}
	next.Cell = cell
	now := i.now()
	next.UpdatedAt = now
	switch {
	case next.Status != StatusIdle:
		next.IdleSince = time.Time{}
	case !exists || old.Status != StatusIdle || next.IdleSince.IsZero():
		next.IdleSince = now
	}

	sh.m[id] = next
	if exists && old.Cell != next.Cell {
		i.removeFromCell(old.Cell, id)
	}
	i.putInCell(next)
	if exists {
		i.byStatus[statusSlot(old.Status)].Add(-1)
	}
	i.byStatus[statusSlot(next.Status)].Add(1)
	sh.mu.Unlock()

	switch {
	case !mirror:
	case next.Status == StatusOffline:
		i.mirrorDo(func(ctx context.Context) error { return i.mirror.Remove(ctx, id) })
	default:
		i.mirrorDo(func(ctx context.Context) error { return i.mirror.Set(ctx, next) })
	}
	return next, nil
}

func (i *Index) putInCell(d Driver) {
	cs := i.cellShardFor(d.Cell)
	cs.mu.Lock()
	bucket := cs.m[d.Cell]
	if bucket == nil {
		bucket = make(map[string]Driver, 4)
		cs.m[d.Cell] = bucket
	}
	bucket[d.ID] = d
	cs.mu.Unlock()
}

func (i *Index) removeFromCell(c geo.Cell, id string) {
	cs := i.cellShardFor(c)
	cs.mu.Lock()
	if bucket := cs.m[c]; bucket != nil {
		delete(bucket, id)
		if len(bucket) == 0 {
			delete(cs.m, c)
		}
	}
	cs.mu.Unlock()
}

// Upsert replaces driver d.ID with d (see Apply for the fields it maintains).
func (i *Index) Upsert(d Driver) (Driver, error) {
	return i.Apply(d.ID, func(cur *Driver, _ bool) error {
		idle := cur.IdleSince
		*cur = d
		if cur.IdleSince.IsZero() {
			cur.IdleSince = idle
		}
		return nil
	})
}

// Claim atomically moves driver id from status `from` to `to`. It returns
// ErrConflict if the driver is not currently in `from`.
func (i *Index) Claim(id string, from, to Status) (Driver, error) {
	return i.Apply(id, func(d *Driver, exists bool) error {
		if !exists {
			return ErrNotFound
		}
		if d.Status != from {
			return fmt.Errorf("%w: %s is %s, not %s", ErrConflict, id, d.Status, from)
		}
		d.Status = to
		return nil
	})
}

// Get returns a snapshot of driver id.
func (i *Index) Get(id string) (Driver, bool) {
	sh := i.idShardFor(id)
	sh.mu.Lock()
	d, ok := sh.m[id]
	sh.mu.Unlock()
	return d, ok
}

// Remove deletes driver id, reporting whether it existed.
func (i *Index) Remove(id string) bool {
	sh := i.idShardFor(id)
	sh.mu.Lock()
	d, ok := sh.m[id]
	if ok {
		delete(sh.m, id)
		i.removeFromCell(d.Cell, id)
		i.byStatus[statusSlot(d.Status)].Add(-1)
	}
	sh.mu.Unlock()
	if ok {
		i.mirrorDo(func(ctx context.Context) error { return i.mirror.Remove(ctx, id) })
	}
	return ok
}

// VisitCells calls fn with a snapshot of every driver in the given cells.
// fn runs under a read lock and must be fast and must not call back into the index.
func (i *Index) VisitCells(cells []geo.Cell, fn func(Driver)) {
	for _, c := range cells {
		cs := i.cellShardFor(c)
		cs.mu.RLock()
		for _, d := range cs.m[c] {
			fn(d)
		}
		cs.mu.RUnlock()
	}
}

// Counts returns live per-status totals (O(1)).
func (i *Index) Counts() Counts {
	c := Counts{
		Offline: int(i.byStatus[0].Load()),
		Idle:    int(i.byStatus[1].Load()),
		EnRoute: int(i.byStatus[2].Load()),
		OnTrip:  int(i.byStatus[3].Load()),
	}
	c.Active = c.Idle + c.EnRoute + c.OnTrip
	c.Total = c.Active + c.Offline
	return c
}

// Len returns the number of indexed drivers.
func (i *Index) Len() int { return i.Counts().Total }

// MirrorHealthy reports whether the last mirror write succeeded (true if no mirror).
func (i *Index) MirrorHealthy() bool { return !i.mirrorFailing.Load() }

// HasMirror reports whether an external mirror is configured.
func (i *Index) HasMirror() bool { return i.mirror != nil }

// mirrorDo runs a mirror write. Failures never block the in-memory index: the
// first failure logs a warning, later ones are silent until the mirror recovers.
func (i *Index) mirrorDo(op func(ctx context.Context) error) {
	if i.mirror == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()
	if err := op(ctx); err != nil {
		if !i.mirrorFailing.Swap(true) {
			i.logger.Printf("WARN fleet: geo mirror unavailable, continuing in memory only: %v", err)
		}
		return
	}
	if i.mirrorFailing.Swap(false) {
		i.logger.Printf("INFO fleet: geo mirror recovered")
	}
}
