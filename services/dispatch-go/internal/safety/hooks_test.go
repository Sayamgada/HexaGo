package safety

import (
	"context"
	"errors"
	"testing"
)

func TestSuspensionHooksFireOncePerTransition(t *testing.T) {
	veh := &fakeVehicles{suspended: map[string]bool{}}
	s := New(Options{Logger: quiet})
	s.SetVehicles(veh)
	var held, released []Suspension
	s.OnSuspension(func(_ context.Context, sp Suspension) { held = append(held, sp) },
		func(_ context.Context, sp Suspension) { released = append(released, sp) })
	s.OnSuspension(nil, nil) // nil hooks are ignored
	ctx := context.Background()
	if _, err := s.Suspend(ctx, "d_007", "inc_1", "escalated", "safety_sentinel"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Suspend(ctx, "d_007", "inc_2", "again", "safety_sentinel") // idempotent: no second hook
	if len(held) != 1 || held[0].IncidentID != "inc_1" || held[0].Result.InterruptedRideID == "" && veh.suspended["d_007"] == false {
		t.Fatalf("held %+v", held)
	}
	_ = s.Unsuspend(ctx, "d_007", "rider") // forbidden: no release
	if len(released) != 0 {
		t.Fatal("release fired without supervisor confirmation")
	}
	if err := s.Unsuspend(ctx, "d_007", ConfirmingAgent); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].IncidentID != "inc_1" || released[0].DriverID != "d_007" {
		t.Fatalf("released %+v", released)
	}
}

func TestReleaseIncidentAfterRestartStillReleasesHeldPayouts(t *testing.T) {
	// A restart forgets the in-memory suspension, but the ledger still holds the
	// incident's payouts: the supervisor-confirmed release must still reach them.
	s := New(Options{Logger: quiet})
	var released []Suspension
	s.OnSuspension(nil, func(_ context.Context, sp Suspension) { released = append(released, sp) })
	ctx := context.Background()
	if _, err := s.ReleaseIncident(ctx, "d_046", "inc_old", "safety_sentinel"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-supervisor release: %v", err)
	}
	if _, err := s.ReleaseIncident(ctx, "d_046", "", ConfirmingAgent); !errors.Is(err, ErrNotSuspended) {
		t.Fatalf("release without incident: %v", err)
	}
	was, err := s.ReleaseIncident(ctx, "d_046", "inc_old", ConfirmingAgent)
	if err != nil || was {
		t.Fatalf("was=%v err=%v", was, err)
	}
	if len(released) != 1 || released[0].IncidentID != "inc_old" || released[0].DriverID != "d_046" {
		t.Fatalf("released %+v", released)
	}
	// A live suspension goes through the normal unsuspend.
	veh := &fakeVehicles{suspended: map[string]bool{}}
	s.SetVehicles(veh)
	if _, err := s.Suspend(ctx, "d_046", "inc_new", "escalated", "safety_sentinel"); err != nil {
		t.Fatal(err)
	}
	if was, err := s.ReleaseIncident(ctx, "d_046", "inc_new", ConfirmingAgent); err != nil || !was || veh.suspended["d_046"] {
		t.Fatalf("live release was=%v err=%v", was, err)
	}
	if len(released) != 2 || released[1].IncidentID != "inc_new" {
		t.Fatalf("released %+v", released)
	}
}
