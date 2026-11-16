package store

import (
	"context"
	"log"
	"sync"
	"time"
)

// DefaultBatchInterval is how often buffered telemetry is written.
const DefaultBatchInterval = 5 * time.Second

// BatcherOptions configures a TelemetryBatcher.
type BatcherOptions struct {
	Interval time.Duration // default 5 s
	// Retention prunes points older than this every PruneEvery, if the store
	// supports it (0 = keep everything).
	Retention  time.Duration
	PruneEvery time.Duration // default 10 min
	Logger     *log.Logger
}

// BatcherStats are cumulative counters.
type BatcherStats struct {
	Flushes uint64 `json:"flushes"`
	Written uint64 `json:"written"`
	Errors  uint64 `json:"errors"`
	Pruned  int64  `json:"pruned"`
}

// TelemetryBatcher buffers driver positions and writes them in batches so a
// 1 Hz simulator does not issue one INSERT per driver per second. Within a
// window only the newest point per driver is kept, and a driver whose position
// and status have not changed since the last write is skipped.
type TelemetryBatcher struct {
	dst  TelemetryStore
	opts BatcherOptions

	mu      sync.Mutex
	buf     map[string]TelemetryPoint
	written map[string]TelemetryPoint // last point written per driver
	stats   BatcherStats
}

// NewTelemetryBatcher wraps dst.
func NewTelemetryBatcher(dst TelemetryStore, o BatcherOptions) *TelemetryBatcher {
	if o.Interval <= 0 {
		o.Interval = DefaultBatchInterval
	}
	if o.PruneEvery <= 0 {
		o.PruneEvery = 10 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return &TelemetryBatcher{dst: dst, opts: o, buf: map[string]TelemetryPoint{}, written: map[string]TelemetryPoint{}}
}

// Interval returns the flush interval.
func (b *TelemetryBatcher) Interval() time.Duration { return b.opts.Interval }

func samePlace(a, b TelemetryPoint) bool {
	return a.Lat == b.Lat && a.Lng == b.Lng && a.Status == b.Status
}

// Add buffers p (replacing any earlier point for the same driver in this window).
func (b *TelemetryBatcher) Add(p TelemetryPoint) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if last, ok := b.written[p.DriverID]; ok && samePlace(last, p) {
		delete(b.buf, p.DriverID)
		return
	}
	b.buf[p.DriverID] = p
}

// AppendTelemetry implements TelemetryStore by buffering p.
func (b *TelemetryBatcher) AppendTelemetry(_ context.Context, p TelemetryPoint) error {
	b.Add(p)
	return nil
}

// RecentTelemetry implements TelemetryStore by reading the underlying store
// (points still buffered appear after the next flush).
func (b *TelemetryBatcher) RecentTelemetry(ctx context.Context, driverID string, limit int) ([]TelemetryPoint, error) {
	return b.dst.RecentTelemetry(ctx, driverID, limit)
}

// Pending returns the number of buffered points.
func (b *TelemetryBatcher) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// Stats returns cumulative counters.
func (b *TelemetryBatcher) Stats() BatcherStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

// Flush writes the buffer now. A failed batch is dropped (telemetry is
// best-effort) so a database outage cannot grow memory without bound.
func (b *TelemetryBatcher) Flush(ctx context.Context) error {
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return nil
	}
	pts := make([]TelemetryPoint, 0, len(b.buf))
	for _, p := range b.buf {
		pts = append(pts, p)
	}
	b.buf = map[string]TelemetryPoint{}
	b.mu.Unlock()

	err := b.write(ctx, pts)

	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.stats.Errors++
		return err
	}
	b.stats.Flushes++
	b.stats.Written += uint64(len(pts))
	for _, p := range pts {
		b.written[p.DriverID] = p
	}
	return nil
}

func (b *TelemetryBatcher) write(ctx context.Context, pts []TelemetryPoint) error {
	if bs, ok := b.dst.(BatchTelemetryStore); ok {
		return bs.AppendTelemetryBatch(ctx, pts)
	}
	for _, p := range pts {
		if err := b.dst.AppendTelemetry(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// Run flushes every interval (and prunes old points) until ctx ends, then
// flushes once more.
func (b *TelemetryBatcher) Run(ctx context.Context) {
	flush := time.NewTicker(b.opts.Interval)
	defer flush.Stop()
	prune := time.NewTicker(b.opts.PruneEvery)
	defer prune.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := b.Flush(fctx); err != nil {
				b.opts.Logger.Printf("WARN telemetry: final flush: %v", err)
			}
			cancel()
			return
		case <-flush.C:
			fctx, cancel := context.WithTimeout(ctx, b.opts.Interval)
			err := b.Flush(fctx)
			cancel()
			if err != nil && !failing {
				b.opts.Logger.Printf("WARN telemetry: batch write failed (dropping batch): %v", err)
			}
			failing = err != nil
		case <-prune.C:
			b.prune(ctx)
		}
	}
}

func (b *TelemetryBatcher) prune(ctx context.Context) {
	pr, ok := b.dst.(TelemetryPruner)
	if !ok || b.opts.Retention <= 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	n, err := pr.PruneTelemetry(pctx, time.Now().Add(-b.opts.Retention))
	if err != nil {
		b.opts.Logger.Printf("WARN telemetry: prune: %v", err)
		return
	}
	b.mu.Lock()
	b.stats.Pruned += n
	b.mu.Unlock()
}
