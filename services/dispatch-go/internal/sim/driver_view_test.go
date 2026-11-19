package sim

import (
	"context"
	"testing"
)

func TestDriverViewsTrackRideAndAV(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 20)
	h.sim.Spawn(ctx)
	list := h.sim.Drivers()
	if len(list) != 20 {
		t.Fatalf("drivers = %d", len(list))
	}
	avs := 0
	for _, d := range list {
		if d.AV {
			avs++
			if d.BatteryPct == nil {
				t.Fatalf("AV %s without battery", d.ID)
			}
			v, ok := h.sim.Driver(d.ID)
			if !ok || v.AV == nil || v.AV.Mode == "" || v.AV.Health == "" {
				t.Fatalf("AV detail: %+v", v)
			}
		}
	}
	if avs == 0 {
		t.Fatal("no AVs in a 20-driver fleet")
	}
	if _, ok := h.sim.Driver("nope"); ok {
		t.Fatal("unknown driver found")
	}

	fd, _ := h.svc.Fleet().Get(list[0].ID)
	ride := h.requestRideFor(t, fd, 3)
	h.step(ctx, 1)
	var id string
	if ride.Match != nil {
		id = ride.Match.Driver.ID
	}
	v, ok := h.sim.Driver(id)
	if !ok || v.Ride == nil || v.Ride.RiderID != "rider-test" || v.RideID == "" {
		t.Fatalf("ride detail missing: %+v", v)
	}
	h.step(ctx, 30)
	v, _ = h.sim.Driver(id)
	if v.Ride != nil && (v.Ride.Progress < 0 || v.Ride.Progress > 1) {
		t.Fatalf("progress out of range: %v", v.Ride.Progress)
	}
}
