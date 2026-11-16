package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func sampleRide(id, rider, driver string, st RideStatus) Ride {
	r := Ride{
		ID:        id,
		RiderID:   rider,
		DriverID:  driver,
		Tier:      "sedan",
		Status:    st,
		Pickup:    geo.Point{Lat: 12.9716, Lng: 77.5946},
		Dropoff:   geo.Point{Lat: 12.9784, Lng: 77.6408},
		PickupH3:  "89618922a4bffff",
		CreatedAt: t0,
		UpdatedAt: t0,
	}
	if driver != "" {
		r.ETASeconds = 180
		r.DistanceKm = 6.4
		r.MatchScore = 0.82
		r.Legs = []RideLeg{
			{Seq: 0, Kind: LegPickup, From: geo.Point{Lat: 12.97, Lng: 77.59}, To: r.Pickup, DistanceKm: 0.6, ETASeconds: 180},
			{Seq: 1, Kind: LegTrip, From: r.Pickup, To: r.Dropoff, DistanceKm: 6.4, ETASeconds: 1280},
		}
	}
	return r
}

// contract exercises behaviour every Store / TelemetryStore implementation must provide.
func contract(t *testing.T, s Store, tel TelemetryStore) {
	ctx := context.Background()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := s.UpsertDriver(ctx, DriverRecord{ID: "drv-1", Status: "idle", Tier: "sedan", Rating: 4.8}); err != nil {
		t.Fatalf("UpsertDriver: %v", err)
	}
	bat := 77.0
	if err := s.UpsertDriver(ctx, DriverRecord{ID: "drv-av", Status: "idle", Tier: "av", Rating: 5, BatteryPct: &bat}); err != nil {
		t.Fatalf("UpsertDriver(av): %v", err)
	}
	// Upsert is idempotent and updates status.
	if err := s.UpsertDriver(ctx, DriverRecord{ID: "drv-1", Status: "en_route", Tier: "sedan", Rating: 4.8}); err != nil {
		t.Fatalf("UpsertDriver again: %v", err)
	}

	matched := sampleRide("ride-1", "rider-1", "drv-1", RideMatched)
	if err := s.CreateRide(ctx, matched); err != nil {
		t.Fatalf("CreateRide: %v", err)
	}
	if err := s.CreateRide(ctx, matched); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate CreateRide err = %v, want ErrConflict", err)
	}
	open := sampleRide("ride-2", "rider-2", "", RideRequested)
	if err := s.CreateRide(ctx, open); err != nil {
		t.Fatalf("CreateRide(open): %v", err)
	}
	// A driver that was never upserted is created on demand.
	other := sampleRide("ride-3", "rider-1", "drv-unknown", RideMatched)
	if err := s.CreateRide(ctx, other); err != nil {
		t.Fatalf("CreateRide(unknown driver): %v", err)
	}

	got, err := s.GetRide(ctx, "ride-1")
	if err != nil {
		t.Fatalf("GetRide: %v", err)
	}
	if got.Status != RideMatched || got.DriverID != "drv-1" || len(got.Legs) != 2 || got.Legs[1].Kind != LegTrip {
		t.Errorf("GetRide = %+v", got)
	}
	if got.Pickup != matched.Pickup || got.MatchScore != matched.MatchScore || !got.CreatedAt.Equal(t0) {
		t.Errorf("GetRide fields differ: %+v", got)
	}
	if _, err := s.GetRide(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRide(nope) err = %v", err)
	}

	active, err := s.ActiveRideForDriver(ctx, "drv-1")
	if err != nil || active.ID != "ride-1" {
		t.Errorf("ActiveRideForDriver = %+v, %v", active, err)
	}
	if _, err := s.ActiveRideForDriver(ctx, "drv-av"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ActiveRideForDriver(no ride) err = %v", err)
	}

	counts, err := s.RideCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (RideCounts{Open: 1, InProgress: 2}) {
		t.Errorf("RideCounts = %+v", counts)
	}

	got.Status = RideInProgress
	got.UpdatedAt = t0.Add(time.Minute)
	if err := s.UpdateRide(ctx, got); err != nil {
		t.Fatalf("UpdateRide: %v", err)
	}
	got.Status = RideCancelled
	got.DriverID = ""
	got.CancelReason = "rider_cancelled"
	if err := s.UpdateRide(ctx, got); err != nil {
		t.Fatalf("UpdateRide: %v", err)
	}
	again, _ := s.GetRide(ctx, "ride-1")
	if again.Status != RideCancelled || again.CancelReason != "rider_cancelled" || again.DriverID != "" || len(again.Legs) != 2 {
		t.Errorf("after UpdateRide = %+v", again)
	}
	if err := s.UpdateRide(ctx, sampleRide("ghost", "r", "", RideRequested)); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateRide(ghost) err = %v", err)
	}
	if _, err := s.ActiveRideForDriver(ctx, "drv-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelled ride still active: %v", err)
	}

	ev := AgentEvent{
		Agent:        "dispatch_broker",
		EventType:    "match",
		RideID:       "ride-1",
		Input:        json.RawMessage(`{"tier":"sedan"}`),
		Decision:     json.RawMessage(`{"driver_id":"drv-1"}`),
		Confidence:   0.82,
		FallbackUsed: false,
		LatencyMs:    0.4,
		CreatedAt:    t0,
	}
	if err := s.RecordEvent(ctx, ev); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if err := s.RecordEvent(ctx, AgentEvent{Agent: "dispatch_broker", EventType: "no_match"}); err != nil {
		t.Fatalf("RecordEvent with empty payloads: %v", err)
	}
	evs, err := s.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].EventType != "no_match" || evs[1].RideID != "ride-1" {
		t.Errorf("RecentEvents = %+v", evs)
	}

	// Telemetry.
	for i := 0; i < 5; i++ {
		p := TelemetryPoint{
			DriverID:   "drv-1",
			Lat:        12.97 + float64(i)*0.001,
			Lng:        77.59,
			Heading:    90,
			SpeedKmh:   20,
			Status:     "idle",
			H3Cell:     "89618922a4bffff",
			RecordedAt: t0.Add(time.Duration(i) * time.Second),
		}
		if err := tel.AppendTelemetry(ctx, p); err != nil {
			t.Fatalf("AppendTelemetry: %v", err)
		}
	}
	pts, err := tel.RecentTelemetry(ctx, "drv-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 || !pts[0].RecordedAt.Equal(t0.Add(4*time.Second)) || pts[0].RecordedAt.Before(pts[2].RecordedAt) {
		t.Errorf("RecentTelemetry newest-first = %+v", pts)
	}
	if pts, _ := tel.RecentTelemetry(ctx, "nobody", 3); len(pts) != 0 {
		t.Errorf("RecentTelemetry(nobody) = %v", pts)
	}
}

func TestMemoryContract(t *testing.T) {
	m := NewMemory()
	contract(t, m, m)
	m.Close()
	if m.Kind() != "memory" {
		t.Errorf("Kind = %q", m.Kind())
	}
}

func TestMemoryTelemetryIsBounded(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	for i := 0; i < maxTelemetryPerDriver+50; i++ {
		_ = m.AppendTelemetry(ctx, TelemetryPoint{DriverID: "d", RecordedAt: t0.Add(time.Duration(i) * time.Second)})
	}
	pts, _ := m.RecentTelemetry(ctx, "d", 0) // limit <= 0 means "all retained"
	if len(pts) != maxTelemetryPerDriver {
		t.Errorf("retained %d points, want %d", len(pts), maxTelemetryPerDriver)
	}
	if want := t0.Add(time.Duration(maxTelemetryPerDriver+49) * time.Second); !pts[0].RecordedAt.Equal(want) {
		t.Errorf("newest = %v, want %v", pts[0].RecordedAt, want)
	}
}

func TestMemoryReturnsCopies(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	r := sampleRide("r", "rider", "d", RideMatched)
	_ = m.CreateRide(ctx, r)
	r.Legs[0].Kind = "mutated"
	got, _ := m.GetRide(ctx, "r")
	got.Legs[1].Kind = "mutated"
	again, _ := m.GetRide(ctx, "r")
	if again.Legs[0].Kind != LegPickup || again.Legs[1].Kind != LegTrip {
		t.Errorf("store shares slices with callers: %+v", again.Legs)
	}
}

func TestMemoryConcurrent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.CreateRide(ctx, sampleRide(fmt.Sprintf("r%d", i), "rider", "", RideRequested))
			_, _ = m.RideCounts(ctx)
			_ = m.AppendTelemetry(ctx, TelemetryPoint{DriverID: "d", RecordedAt: t0})
			_ = m.RecordEvent(ctx, AgentEvent{Agent: "a", EventType: "e"})
		}(i)
	}
	wg.Wait()
	if c, _ := m.RideCounts(ctx); c.Open != 20 {
		t.Errorf("Open = %d", c.Open)
	}
}

func TestRideStatusHelpers(t *testing.T) {
	tests := []struct {
		s              RideStatus
		active, closed bool
	}{
		{RideRequested, false, false},
		{RideMatched, true, false},
		{RideInProgress, true, false},
		{RideCompleted, false, true},
		{RideCancelled, false, true},
	}
	for _, tc := range tests {
		if tc.s.Active() != tc.active || tc.s.Terminal() != tc.closed {
			t.Errorf("%s: Active=%v Terminal=%v", tc.s, tc.s.Active(), tc.s.Terminal())
		}
	}
}

func TestMigrationsEmbedded(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].version != "0001_init" {
		t.Fatalf("migrations = %+v", ms)
	}
	for _, table := range []string{"riders", "drivers", "vehicles", "rides", "ride_legs", "telemetry_points", "agent_events"} {
		found := false
		for _, m := range ms {
			if containsTable(m.sql, table) {
				found = true
			}
		}
		if !found {
			t.Errorf("no migration creates table %s", table)
		}
	}
}
