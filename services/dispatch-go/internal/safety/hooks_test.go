package safety

import (
	"context"
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
