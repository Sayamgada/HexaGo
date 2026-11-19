package store

import (
	"context"
	"sync"
	"time"
)

// maxTelemetryPerDriver bounds in-memory telemetry so long-running dev servers don't grow forever.
const maxTelemetryPerDriver = 1000

// Memory is an in-process Store and TelemetryStore used when DATABASE_URL is
// unset (local dev, tests). Data is lost on restart.
type Memory struct {
	mu        sync.RWMutex
	drivers   map[string]DriverRecord
	rides     map[string]Ride
	events    []AgentEvent
	telemetry map[string][]TelemetryPoint // oldest first
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		drivers:   map[string]DriverRecord{},
		rides:     map[string]Ride{},
		telemetry: map[string][]TelemetryPoint{},
	}
}

// Kind implements Store.
func (m *Memory) Kind() string { return "memory" }

// Ping implements Store.
func (m *Memory) Ping(context.Context) error { return nil }

// Close implements Store.
func (m *Memory) Close() {}

// UpsertDriver implements Store.
func (m *Memory) UpsertDriver(_ context.Context, d DriverRecord) error {
	m.mu.Lock()
	m.drivers[d.ID] = d
	m.mu.Unlock()
	return nil
}

// CreateRide implements Store.
func (m *Memory) CreateRide(_ context.Context, r Ride) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rides[r.ID]; ok {
		return ErrConflict
	}
	m.rides[r.ID] = r.clone()
	return nil
}

// UpdateRide implements Store.
func (m *Memory) UpdateRide(_ context.Context, r Ride) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.rides[r.ID]
	if !ok {
		return ErrNotFound
	}
	cur.Status = r.Status
	cur.DriverID = r.DriverID
	cur.ETASeconds = r.ETASeconds
	cur.MatchScore = r.MatchScore
	cur.CancelReason = r.CancelReason
	cur.UpdatedAt = r.UpdatedAt
	m.rides[r.ID] = cur
	return nil
}

// GetRide implements Store.
func (m *Memory) GetRide(_ context.Context, id string) (Ride, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rides[id]
	if !ok {
		return Ride{}, ErrNotFound
	}
	return r.clone(), nil
}

// ActiveRideForDriver implements Store.
func (m *Memory) ActiveRideForDriver(_ context.Context, driverID string) (Ride, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.rides {
		if r.DriverID == driverID && r.Status.Active() {
			return r.clone(), nil
		}
	}
	return Ride{}, ErrNotFound
}

// RideCounts implements Store.
func (m *Memory) RideCounts(context.Context) (RideCounts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var c RideCounts
	for _, r := range m.rides {
		switch {
		case r.Status == RideRequested:
			c.Open++
		case r.Status.Active():
			c.InProgress++
		}
	}
	return c, nil
}

// RecordEvent implements Store.
func (m *Memory) RecordEvent(_ context.Context, e AgentEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.events) + 1)
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	m.events = append(m.events, e)
	return nil
}

// RecentEvents implements Store.
func (m *Memory) RecentEvents(_ context.Context, limit int) ([]AgentEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AgentEvent, 0, limit)
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.events[i])
	}
	return out, nil
}

// AppendTelemetry implements TelemetryStore.
func (m *Memory) AppendTelemetry(_ context.Context, p TelemetryPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pts := append(m.telemetry[p.DriverID], p)
	if len(pts) > maxTelemetryPerDriver {
		pts = append(pts[:0:0], pts[len(pts)-maxTelemetryPerDriver:]...)
	}
	m.telemetry[p.DriverID] = pts
	return nil
}

// RecentTelemetry implements TelemetryStore.
func (m *Memory) RecentTelemetry(_ context.Context, driverID string, limit int) ([]TelemetryPoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pts := m.telemetry[driverID]
	if limit <= 0 || limit > len(pts) {
		limit = len(pts)
	}
	out := make([]TelemetryPoint, 0, limit)
	for i := len(pts) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, pts[i])
	}
	return out, nil
}

// AppendTelemetryBatch implements BatchTelemetryStore.
func (m *Memory) AppendTelemetryBatch(ctx context.Context, ps []TelemetryPoint) error {
	for _, p := range ps {
		if err := m.AppendTelemetry(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// PruneTelemetry implements TelemetryPruner.
func (m *Memory) PruneTelemetry(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, pts := range m.telemetry {
		kept := pts[:0]
		for _, p := range pts {
			if p.RecordedAt.Before(before) {
				n++
				continue
			}
			kept = append(kept, p)
		}
		if len(kept) == 0 {
			delete(m.telemetry, id)
		} else {
			m.telemetry[id] = kept
		}
	}
	return n, nil
}

// CloseStaleRides implements Store.
func (m *Memory) CloseStaleRides(_ context.Context, before time.Time, reason string, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, r := range m.rides {
		if r.Status.Terminal() || !r.UpdatedAt.Before(before) {
			continue
		}
		r.Status = RideCancelled
		r.CancelReason = reason
		r.UpdatedAt = now
		m.rides[id] = r
		n++
	}
	return n, nil
}

// EventCountsSince returns how many events at or after since match agent and
// eventType, and how many there are in total (the Ops "today" counters).
func (m *Memory) EventCountsSince(agent, eventType string, since time.Time) (matching, total int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if e.CreatedAt.Before(since) {
			continue
		}
		total++
		if e.Agent == agent && e.EventType == eventType {
			matching++
		}
	}
	return matching, total
}
