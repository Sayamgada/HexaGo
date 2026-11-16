package store

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

type plainTelemetry struct {
	mu  sync.Mutex
	pts []TelemetryPoint
	err error
}

func (p *plainTelemetry) AppendTelemetry(_ context.Context, pt TelemetryPoint) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.pts = append(p.pts, pt)
	return nil
}

func (p *plainTelemetry) RecentTelemetry(context.Context, string, int) ([]TelemetryPoint, error) {
	return nil, nil
}

func (p *plainTelemetry) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pts)
}

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func TestBatcherKeepsLatestPerDriverAndSkipsUnmoved(t *testing.T) {
	mem := NewMemory()
	b := NewTelemetryBatcher(mem, BatcherOptions{Logger: quiet()})
	ctx := context.Background()
	t0 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	b.Add(TelemetryPoint{DriverID: "d_1", Lat: 1, Lng: 1, RecordedAt: t0})
	b.Add(TelemetryPoint{DriverID: "d_1", Lat: 2, Lng: 2, RecordedAt: t0.Add(time.Second)})
	b.Add(TelemetryPoint{DriverID: "d_2", Lat: 3, Lng: 3, RecordedAt: t0})
	if b.Pending() != 2 {
		t.Fatalf("pending %d, want 2", b.Pending())
	}
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := mem.RecentTelemetry(ctx, "d_1", 0)
	if len(got) != 1 || got[0].Lat != 2 {
		t.Fatalf("d_1 telemetry %+v", got)
	}
	// Same position and status again: nothing to write.
	b.Add(TelemetryPoint{DriverID: "d_1", Lat: 2, Lng: 2, RecordedAt: t0.Add(5 * time.Second)})
	if b.Pending() != 0 {
		t.Fatalf("unmoved driver queued (%d pending)", b.Pending())
	}
	// Status change at the same spot is still recorded.
	b.Add(TelemetryPoint{DriverID: "d_1", Lat: 2, Lng: 2, Status: "on_trip", RecordedAt: t0.Add(6 * time.Second)})
	if b.Pending() != 1 {
		t.Fatalf("status change not queued")
	}
	st := b.Stats()
	if st.Flushes != 1 || st.Written != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestBatcherFallsBackToSingleInserts(t *testing.T) {
	p := &plainTelemetry{}
	b := NewTelemetryBatcher(p, BatcherOptions{Logger: quiet()})
	for _, id := range []string{"a", "b", "c"} {
		b.Add(TelemetryPoint{DriverID: id, Lat: 1})
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.len() != 3 {
		t.Fatalf("wrote %d", p.len())
	}
}

func TestBatcherRunFlushesPeriodicallyAndOnStop(t *testing.T) {
	p := &plainTelemetry{}
	b := NewTelemetryBatcher(p, BatcherOptions{Interval: 20 * time.Millisecond, Logger: quiet()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	b.Add(TelemetryPoint{DriverID: "a", Lat: 1})
	deadline := time.Now().Add(2 * time.Second)
	for p.len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no periodic flush")
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.Add(TelemetryPoint{DriverID: "b", Lat: 1})
	cancel()
	<-done
	if p.len() != 2 {
		t.Fatalf("final flush missing: %d points", p.len())
	}
}

func TestBatcherDropsBatchOnError(t *testing.T) {
	p := &plainTelemetry{err: errors.New("db down")}
	b := NewTelemetryBatcher(p, BatcherOptions{Logger: quiet()})
	b.Add(TelemetryPoint{DriverID: "a", Lat: 1})
	if err := b.Flush(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if b.Pending() != 0 {
		t.Fatal("failed batch should be dropped, not retried forever")
	}
	if b.Stats().Errors != 1 {
		t.Fatalf("stats %+v", b.Stats())
	}
}

func TestBatcherDefaultInterval(t *testing.T) {
	b := NewTelemetryBatcher(NewMemory(), BatcherOptions{})
	if b.Interval() != 5*time.Second {
		t.Fatalf("default interval %v", b.Interval())
	}
}

func TestMemoryPruneTelemetry(t *testing.T) {
	mem := NewMemory()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	_ = mem.AppendTelemetryBatch(ctx, []TelemetryPoint{
		{DriverID: "a", RecordedAt: t0},
		{DriverID: "a", RecordedAt: t0.Add(time.Hour)},
		{DriverID: "b", RecordedAt: t0},
	})
	n, err := mem.PruneTelemetry(ctx, t0.Add(time.Minute))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	a, _ := mem.RecentTelemetry(ctx, "a", 0)
	b, _ := mem.RecentTelemetry(ctx, "b", 0)
	if len(a) != 1 || len(b) != 0 {
		t.Fatalf("left a=%d b=%d", len(a), len(b))
	}
}

func TestMemoryCloseStaleRides(t *testing.T) {
	mem := NewMemory()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for i, st := range []RideStatus{RideRequested, RideMatched, RideInProgress, RideCompleted} {
		_ = mem.CreateRide(ctx, Ride{ID: string(rune('a' + i)), RiderID: "r", Status: st, UpdatedAt: t0})
	}
	_ = mem.CreateRide(ctx, Ride{ID: "fresh", RiderID: "r", Status: RideRequested, UpdatedAt: t0.Add(time.Hour)})
	n, err := mem.CloseStaleRides(ctx, t0.Add(time.Minute), "gateway_restart", t0.Add(2*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("closed %d, %v", n, err)
	}
	r, _ := mem.GetRide(ctx, "b")
	if r.Status != RideCancelled || r.CancelReason != "gateway_restart" {
		t.Fatalf("ride b %+v", r)
	}
	c, _ := mem.RideCounts(ctx)
	if c.Open != 1 || c.InProgress != 0 {
		t.Fatalf("counts %+v", c)
	}
}

func TestBatcherIsATelemetryStore(t *testing.T) {
	mem := NewMemory()
	var ts TelemetryStore = NewTelemetryBatcher(mem, BatcherOptions{Logger: quiet()})
	ctx := context.Background()
	if err := ts.AppendTelemetry(ctx, TelemetryPoint{DriverID: "a", Lat: 1}); err != nil {
		t.Fatal(err)
	}
	if pts, _ := ts.RecentTelemetry(ctx, "a", 0); len(pts) != 0 {
		t.Fatal("point written before flush")
	}
	_ = ts.(*TelemetryBatcher).Flush(ctx)
	if pts, _ := ts.RecentTelemetry(ctx, "a", 0); len(pts) != 1 {
		t.Fatalf("after flush %d", len(pts))
	}
}
