package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

var center = geo.Point{Lat: 12.9716, Lng: 77.5946}

func testGrid(t testing.TB) geo.Indexer {
	t.Helper()
	g, err := geo.NewHexGrid(9, center.Lat)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestIndex(t testing.TB, opts ...Option) (*Index, *fakeClock) {
	clk := &fakeClock{now: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	opts = append([]Option{WithClock(clk.Now)}, opts...)
	return New(testGrid(t), opts...), clk
}

func driver(id string, p geo.Point, st Status, tier Tier) Driver {
	return Driver{ID: id, Lat: p.Lat, Lng: p.Lng, Status: st, Tier: tier, Rating: 4.8}
}

func TestParseStatus(t *testing.T) {
	tests := []struct {
		in   string
		want Status
		ok   bool
	}{
		{"idle", StatusIdle, true},
		{"IDLE", StatusIdle, true},
		{" offline ", StatusOffline, true},
		{"en_route", StatusEnRoute, true},
		{"on_trip", StatusOnTrip, true},
		{"busy", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, err := ParseStatus(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseStatus(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestParseTier(t *testing.T) {
	tests := []struct {
		in   string
		want Tier
		ok   bool
	}{
		{"auto", TierAuto, true},
		{"mini", TierMini, true},
		{"Sedan", TierSedan, true},
		{"premium", TierPremium, true},
		{"xl", TierXL, true},
		{"AV", TierAV, true},
		{"bike", "", false},
	}
	for _, tc := range tests {
		got, err := ParseTier(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseTier(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
	if len(Tiers()) != 6 {
		t.Errorf("Tiers() = %v", Tiers())
	}
}

func TestUpsertValidation(t *testing.T) {
	idx, _ := newTestIndex(t)
	tests := []struct {
		name string
		d    Driver
	}{
		{"empty id", driver("", center, StatusIdle, TierMini)},
		{"bad position", driver("d1", geo.Point{Lat: 95, Lng: 0}, StatusIdle, TierMini)},
		{"bad status", driver("d1", center, Status("busy"), TierMini)},
		{"bad tier", driver("d1", center, StatusIdle, Tier("bike"))},
		{"bad battery", func() Driver { d := driver("d1", center, StatusIdle, TierAV); d.BatteryPct = 120; return d }()},
		{"bad rating", func() Driver { d := driver("d1", center, StatusIdle, TierMini); d.Rating = 7; return d }()},
		{"bad speed", func() Driver { d := driver("d1", center, StatusIdle, TierMini); d.SpeedKmh = -3; return d }()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := idx.Upsert(tc.d); !errors.Is(err, ErrInvalidDriver) {
				t.Errorf("err = %v, want ErrInvalidDriver", err)
			}
		})
	}
	if idx.Len() != 0 {
		t.Errorf("invalid drivers were stored: Len() = %d", idx.Len())
	}
}

func TestUpsertGetMoveRemove(t *testing.T) {
	idx, clk := newTestIndex(t)
	g := testGrid(t)

	d, err := idx.Upsert(driver("d1", center, StatusIdle, TierSedan))
	if err != nil {
		t.Fatal(err)
	}
	wantCell, _ := g.CellOf(center)
	if d.Cell != wantCell {
		t.Errorf("Cell = %v, want %v", d.Cell, wantCell)
	}
	if !d.IdleSince.Equal(clk.Now()) || !d.UpdatedAt.Equal(clk.Now()) {
		t.Errorf("IdleSince/UpdatedAt not stamped: %+v", d)
	}
	d.Heading = 400
	d.Heading = normalizeHeading(d.Heading)
	if d.Heading != 40 {
		t.Errorf("normalizeHeading(400) = %v", d.Heading)
	}

	// Staying idle keeps the original IdleSince.
	clk.Advance(5 * time.Minute)
	far := geo.Point{Lat: center.Lat + 0.05, Lng: center.Lng}
	moved, err := idx.Upsert(driver("d1", far, StatusIdle, TierSedan))
	if err != nil {
		t.Fatal(err)
	}
	if !moved.IdleSince.Equal(d.IdleSince) {
		t.Errorf("IdleSince reset on move: %v vs %v", moved.IdleSince, d.IdleSince)
	}
	if moved.Cell == d.Cell {
		t.Fatal("cell should change after a 5 km move")
	}
	if n := countIn(idx, d.Cell); n != 0 {
		t.Errorf("driver still indexed in old cell (%d)", n)
	}
	if n := countIn(idx, moved.Cell); n != 1 {
		t.Errorf("driver not indexed in new cell (%d)", n)
	}

	// Going on a trip clears IdleSince; returning to idle restamps it.
	busy, _ := idx.Upsert(driver("d1", far, StatusOnTrip, TierSedan))
	if !busy.IdleSince.IsZero() {
		t.Errorf("IdleSince should be zero when on_trip: %v", busy.IdleSince)
	}
	clk.Advance(time.Minute)
	back, _ := idx.Upsert(driver("d1", far, StatusIdle, TierSedan))
	if !back.IdleSince.Equal(clk.Now()) {
		t.Errorf("IdleSince = %v, want %v", back.IdleSince, clk.Now())
	}

	got, ok := idx.Get("d1")
	if !ok || got.Status != StatusIdle {
		t.Errorf("Get = %+v, %v", got, ok)
	}
	if _, ok := idx.Get("missing"); ok {
		t.Error("Get(missing) should be false")
	}

	if !idx.Remove("d1") {
		t.Error("Remove should report true")
	}
	if idx.Remove("d1") {
		t.Error("second Remove should report false")
	}
	if idx.Len() != 0 || countIn(idx, moved.Cell) != 0 {
		t.Error("driver not fully removed")
	}
	if c := idx.Counts(); c != (Counts{}) {
		t.Errorf("Counts after remove = %+v", c)
	}
}

func countIn(idx *Index, c geo.Cell) int {
	n := 0
	idx.VisitCells([]geo.Cell{c}, func(Driver) { n++ })
	return n
}

func TestCounts(t *testing.T) {
	idx, _ := newTestIndex(t)
	statuses := []Status{StatusIdle, StatusIdle, StatusEnRoute, StatusOnTrip, StatusOffline}
	for i, st := range statuses {
		if _, err := idx.Upsert(driver(fmt.Sprintf("d%d", i), center, st, TierMini)); err != nil {
			t.Fatal(err)
		}
	}
	want := Counts{Total: 5, Active: 4, Idle: 2, EnRoute: 1, OnTrip: 1, Offline: 1}
	if got := idx.Counts(); got != want {
		t.Errorf("Counts = %+v, want %+v", got, want)
	}
	// Status transitions move counters.
	if _, err := idx.Upsert(driver("d4", center, StatusIdle, TierMini)); err != nil {
		t.Fatal(err)
	}
	want = Counts{Total: 5, Active: 5, Idle: 3, EnRoute: 1, OnTrip: 1}
	if got := idx.Counts(); got != want {
		t.Errorf("Counts after transition = %+v, want %+v", got, want)
	}
}

func TestApply(t *testing.T) {
	idx, _ := newTestIndex(t)

	// Callback sees exists=false for a new driver and can reject it.
	_, err := idx.Apply("new", func(d *Driver, exists bool) error {
		if exists {
			t.Error("exists should be false")
		}
		return errors.New("nope")
	})
	if err == nil || idx.Len() != 0 {
		t.Fatalf("rejected Apply should not store: err=%v len=%d", err, idx.Len())
	}

	created, err := idx.Apply("new", func(d *Driver, exists bool) error {
		*d = driver("ignored-id", center, StatusIdle, TierXL)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "new" {
		t.Errorf("Apply must pin the ID, got %q", created.ID)
	}

	updated, err := idx.Apply("new", func(d *Driver, exists bool) error {
		if !exists || d.Tier != TierXL {
			t.Errorf("exists=%v tier=%v", exists, d.Tier)
		}
		d.Heading = -90
		return nil
	})
	if err != nil || updated.Heading != 270 {
		t.Errorf("Apply update = %+v, %v", updated, err)
	}

	// An invalid mutation leaves the stored driver untouched.
	_, err = idx.Apply("new", func(d *Driver, exists bool) error {
		d.Status = "bogus"
		return nil
	})
	if !errors.Is(err, ErrInvalidDriver) {
		t.Errorf("err = %v", err)
	}
	if got, _ := idx.Get("new"); got.Status != StatusIdle {
		t.Errorf("status changed by failed Apply: %v", got.Status)
	}
}

func TestClaim(t *testing.T) {
	idx, _ := newTestIndex(t)
	if _, err := idx.Upsert(driver("d1", center, StatusIdle, TierMini)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		id       string
		from, to Status
		wantErr  error
		wantStat Status
	}{
		{"claim idle", "d1", StatusIdle, StatusEnRoute, nil, StatusEnRoute},
		{"double claim conflicts", "d1", StatusIdle, StatusEnRoute, ErrConflict, StatusEnRoute},
		{"release", "d1", StatusEnRoute, StatusIdle, nil, StatusIdle},
		{"missing driver", "nobody", StatusIdle, StatusEnRoute, ErrNotFound, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := idx.Claim(tc.id, tc.from, tc.to)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if err == nil && d.Status != tc.wantStat {
				t.Errorf("status = %v, want %v", d.Status, tc.wantStat)
			}
		})
	}
}

func TestConcurrentClaimHasOneWinner(t *testing.T) {
	idx, _ := newTestIndex(t)
	if _, err := idx.Upsert(driver("d1", center, StatusIdle, TierMini)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := idx.Claim("d1", StatusIdle, StatusEnRoute); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("wins = %d, want 1", wins)
	}
}

func TestConcurrentUpsertsAndReads(t *testing.T) {
	idx, _ := newTestIndex(t)
	g := testGrid(t)
	origin, _ := g.CellOf(center)
	rings, _ := g.Rings(origin, 3)
	var cells []geo.Cell
	for _, r := range rings {
		cells = append(cells, r...)
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				p := geo.Point{Lat: center.Lat + float64(i%10)*0.001, Lng: center.Lng + float64(w)*0.001}
				if _, err := idx.Upsert(driver(fmt.Sprintf("w%d-%d", w, i%20), p, StatusIdle, TierMini)); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				idx.VisitCells(cells, func(Driver) {})
				_ = idx.Counts()
			}
		}()
	}
	wg.Wait()
	if idx.Len() != 8*20 {
		t.Errorf("Len = %d, want %d", idx.Len(), 160)
	}
	if c := idx.Counts(); c.Total != 160 || c.Idle != 160 {
		t.Errorf("Counts = %+v", c)
	}
}

type recordingMirror struct {
	mu      sync.Mutex
	set     map[string]geo.Point
	removed []string
	fail    bool
}

func (m *recordingMirror) Set(_ context.Context, d Driver) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("redis down")
	}
	if m.set == nil {
		m.set = map[string]geo.Point{}
	}
	m.set[d.ID] = geo.Point{Lat: d.Lat, Lng: d.Lng}
	return nil
}

func (m *recordingMirror) Remove(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("redis down")
	}
	delete(m.set, id)
	m.removed = append(m.removed, id)
	return nil
}

func TestMirror(t *testing.T) {
	m := &recordingMirror{}
	idx, _ := newTestIndex(t, WithMirror(m))

	if _, err := idx.Upsert(driver("d1", center, StatusIdle, TierMini)); err != nil {
		t.Fatal(err)
	}
	if p, ok := m.set["d1"]; !ok || p != center {
		t.Errorf("mirror not updated: %+v", m.set)
	}
	// Going offline removes the driver from the GEO set.
	if _, err := idx.Upsert(driver("d1", center, StatusOffline, TierMini)); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.set["d1"]; ok || len(m.removed) != 1 {
		t.Errorf("offline driver still mirrored: %+v removed=%v", m.set, m.removed)
	}
	if _, err := idx.Upsert(driver("d2", center, StatusIdle, TierMini)); err != nil {
		t.Fatal(err)
	}
	idx.Remove("d2")
	if _, ok := m.set["d2"]; ok {
		t.Error("removed driver still mirrored")
	}
}

func TestMirrorFailureKeepsWorkingInMemory(t *testing.T) {
	var buf bytes.Buffer
	m := &recordingMirror{fail: true}
	idx, _ := newTestIndex(t, WithMirror(m), WithLogger(log.New(&buf, "", 0)))

	for i := 0; i < 3; i++ {
		if _, err := idx.Upsert(driver(fmt.Sprintf("d%d", i), center, StatusIdle, TierMini)); err != nil {
			t.Fatalf("Upsert must succeed while Redis is down: %v", err)
		}
	}
	idx.Remove("d0")
	if idx.Len() != 2 {
		t.Errorf("Len = %d", idx.Len())
	}
	if n := strings.Count(buf.String(), "WARN"); n != 1 {
		t.Errorf("want exactly one warning while degraded, got %d: %q", n, buf.String())
	}

	m.mu.Lock()
	m.fail = false
	m.mu.Unlock()
	if _, err := idx.Upsert(driver("d1", center, StatusIdle, TierMini)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "recovered") {
		t.Errorf("expected recovery log, got %q", buf.String())
	}
	if !idx.MirrorHealthy() {
		t.Error("MirrorHealthy should be true after recovery")
	}
}

func TestRedisMirrorUnreachable(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRedisMirror(ctx, "::not a url::"); err == nil {
		t.Error("expected parse error")
	}
	// Port 1 on loopback refuses connections immediately.
	if _, err := NewRedisMirror(ctx, "redis://127.0.0.1:1/0"); err == nil {
		t.Error("expected ping error for unreachable Redis")
	}
}

type batchMirror struct {
	recordingMirror
	batches int
}

func (b *batchMirror) SetMany(ctx context.Context, ds []Driver) error {
	b.batches++
	for _, d := range ds {
		if err := b.Set(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func TestApplyLocalDefersMirrorUntilSync(t *testing.T) {
	for _, batching := range []bool{false, true} {
		rm := &recordingMirror{}
		bm := &batchMirror{}
		var m Mirror = rm
		if batching {
			m = bm
		}
		idx, _ := newTestIndex(t, WithMirror(m))
		if _, err := idx.ApplyLocal("d1", func(d *Driver, _ bool) error {
			*d = driver("d1", center, StatusIdle, TierMini)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := idx.ApplyLocal("d2", func(d *Driver, _ bool) error {
			*d = driver("d2", center, StatusOffline, TierMini)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		mirrored := func() int {
			if batching {
				return len(bm.set)
			}
			return len(rm.set)
		}
		if mirrored() != 0 {
			t.Fatal("ApplyLocal wrote to the mirror")
		}
		idx.SyncMirror()
		if mirrored() != 1 { // offline d2 is not mirrored
			t.Fatalf("after sync %d mirrored (batching=%v)", mirrored(), batching)
		}
		if batching && bm.batches != 1 {
			t.Fatalf("batches %d", bm.batches)
		}
		if got := len(idx.Snapshot()); got != 2 {
			t.Fatalf("snapshot %d", got)
		}
	}
	// No mirror: no-op.
	idx, _ := newTestIndex(t)
	idx.SyncMirror()
}
