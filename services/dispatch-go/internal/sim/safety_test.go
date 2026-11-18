package sim

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// tripFor books a ride for driver n and steps until it is on_trip.
func (h *harness) tripFor(t *testing.T, n int, km float64) (string, string) {
	t.Helper()
	ctx := context.Background()
	d, ok := h.svc.Fleet().Get(DriverID(n))
	if !ok {
		t.Fatalf("driver %d missing", n)
	}
	view := h.requestRideFor(t, d, km)
	id := view.Match.Driver.ID
	if !h.runUntil(ctx, 300, func() bool {
		fd, _ := h.svc.Fleet().Get(id)
		return fd.Status == fleet.StatusOnTrip
	}) {
		t.Fatalf("%s never on trip", id)
	}
	h.step(ctx, 5)
	return id, view.Ride.ID
}

type hookRecorder struct {
	mu    sync.Mutex
	obs   []safety.Observation
	ended []string
}

func (r *hookRecorder) ObserveTick(_ context.Context, obs []safety.Observation) {
	r.mu.Lock()
	r.obs = append(r.obs, obs...)
	r.mu.Unlock()
}

func (r *hookRecorder) RideEnded(id string) {
	r.mu.Lock()
	r.ended = append(r.ended, id)
	r.mu.Unlock()
}

func TestSafetyHookSeesOnTripVehiclesAndRideEnds(t *testing.T) {
	rec := &hookRecorder{}
	h := newHarness(t, 2, func(o *Options) { o.Safety = rec; o.DemandPerMin = 0 })
	ctx := context.Background()
	h.sim.Spawn(ctx)
	driverID, rideID := h.tripFor(t, 1, 1.5)
	rec.mu.Lock()
	n := len(rec.obs)
	last := rec.obs[n-1]
	rec.mu.Unlock()
	if n == 0 || last.RideID != rideID || last.DriverID != driverID || last.RiderID != "rider-test" || len(last.Plan) < 2 {
		t.Fatalf("observations %d, last %+v", n, last)
	}
	if !h.runUntil(ctx, 900, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.ended) > 0
	}) {
		t.Fatal("RideEnded never called")
	}
	if rec.ended[0] != rideID {
		t.Fatalf("ended %v", rec.ended)
	}
}

func TestSafetyIncidentPrefersWatchedRide(t *testing.T) {
	var watched string
	h := newHarness(t, 2, func(o *Options) {
		o.DemandPerMin = 0
		o.Watched = func(id string) bool { return id == watched }
	})
	ctx := context.Background()
	h.sim.Spawn(ctx)
	_, longRide := h.tripFor(t, 1, 8) // most road left: picked when nobody watches
	d2, shortRide := h.tripFor(t, 2, 2)
	watched = shortRide
	v, err := h.sim.TriggerScenario(ctx, "safety_incident")
	if err != nil {
		t.Fatal(err)
	}
	if v.RideID != shortRide || v.DriverID != d2 {
		t.Fatalf("picked %s, want the watched ride %s (not %s)", v.RideID, shortRide, longRide)
	}
}

func TestSuspendAVStopsVehicleAndDispatchesReplacement(t *testing.T) {
	h := newHarness(t, 7, func(o *Options) { o.DemandPerMin = 0 })
	ctx := context.Background()
	h.sim.Spawn(ctx)
	av, _ := h.svc.Fleet().Get(DriverID(7))
	if av.Tier != fleet.TierAV {
		t.Fatalf("d_007 tier %s, want av", av.Tier)
	}
	driverID, rideID := h.tripFor(t, 7, 6)
	h.pub.reset()

	res, err := h.sim.Suspend(ctx, driverID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.VehicleStopped || res.InterruptedRideID != rideID || res.ReplacementRideID == "" || res.Tier != "av" {
		t.Fatalf("suspend result %+v", res)
	}
	r, _ := h.mem.GetRide(ctx, rideID)
	if r.Status != store.RideCancelled || r.CancelReason != "safety_incident" {
		t.Fatalf("original ride %+v", r)
	}
	rep, err := h.mem.GetRide(ctx, res.ReplacementRideID)
	if err != nil || rep.RiderID != "rider-test" || rep.DriverID == driverID || rep.Tier != "av" {
		t.Fatalf("replacement %+v %v", rep, err)
	}
	if got := h.pub.rideStatuses(rideID); len(got) == 0 || got[len(got)-1] != "interrupted" {
		t.Fatalf("ride statuses %v", got)
	}

	pos, _ := h.sim.position(driverID)
	h.step(ctx, 30)
	after, _ := h.sim.position(driverID)
	fd, _ := h.svc.Fleet().Get(driverID)
	if after != pos || fd.Status != fleet.StatusOffline || !h.sim.Suspended(driverID) {
		t.Fatalf("suspended vehicle moved or came back: %v -> %v, %s", pos, after, fd.Status)
	}
	for _, pd := range h.pub.lastDrivers() {
		if pd.ID == driverID && pd.Status != "suspended" {
			t.Fatalf("published status %q", pd.Status)
		}
	}

	if err := h.sim.Unsuspend(ctx, driverID); err != nil {
		t.Fatal(err)
	}
	h.step(ctx, 1)
	fd, _ = h.svc.Fleet().Get(driverID)
	if fd.Status != fleet.StatusIdle || h.sim.Suspended(driverID) {
		t.Fatalf("after unsuspend: %s", fd.Status)
	}
}

func TestSuspendHumanDriverHasNoReplacement(t *testing.T) {
	h := newHarness(t, 2, func(o *Options) { o.DemandPerMin = 0 })
	ctx := context.Background()
	h.sim.Spawn(ctx)
	driverID, rideID := h.tripFor(t, 1, 4)
	res, err := h.sim.Suspend(ctx, driverID, "rider distress")
	if err != nil || res.InterruptedRideID != rideID || res.ReplacementRideID != "" {
		t.Fatalf("%+v %v", res, err)
	}
	r, _ := h.mem.GetRide(ctx, rideID)
	if r.CancelReason != "rider distress" {
		t.Fatalf("reason %q", r.CancelReason)
	}
	// An idle driver can be suspended too (no ride to interrupt).
	res, err = h.sim.Suspend(ctx, DriverID(2), "")
	if err != nil || res.InterruptedRideID != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := h.sim.Suspend(ctx, "d_999", ""); !errors.Is(err, safety.ErrUnknownDriver) {
		t.Fatalf("unknown driver: %v", err)
	}
	if err := h.sim.Unsuspend(ctx, "d_999"); !errors.Is(err, safety.ErrUnknownDriver) {
		t.Fatalf("unknown unsuspend: %v", err)
	}
}

// End to end inside the gateway process: the scenario's swerve is detected and
// a suspension through the safety Service stops the simulated vehicle.
func TestSafetyServiceDrivesSimulator(t *testing.T) {
	var svc *safety.Service
	h := newHarness(t, 3, func(o *Options) {
		o.DemandPerMin = 0
		svc = safety.New(safety.Options{Config: safety.DefaultConfig(true), Events: o.Service.RecordAgentEvent,
			Positions: o.Service.Fleet(), Logger: log.New(io.Discard, "", 0)})
		o.Safety = svc
	})
	svc.SetVehicles(h.sim)
	ctx := context.Background()
	h.sim.Spawn(ctx)
	driverID, rideID := h.tripFor(t, 1, 6)
	if _, err := h.sim.TriggerScenario(ctx, "safety_incident"); err != nil {
		t.Fatal(err)
	}
	if !h.runUntil(ctx, 60, func() bool { return len(svc.Signals()) > 0 }) {
		t.Fatal("no signal")
	}
	if _, err := svc.Suspend(ctx, driverID, "inc_1", "escalated", "safety_sentinel"); err != nil {
		t.Fatal(err)
	}
	if !h.sim.Suspended(driverID) {
		t.Fatal("not suspended in the simulator")
	}
	r, _ := h.mem.GetRide(ctx, rideID)
	if r.Status != store.RideCancelled {
		t.Fatalf("ride %s", r.Status)
	}
	if err := svc.Unsuspend(ctx, driverID, "safety_sentinel"); !errors.Is(err, safety.ErrForbidden) {
		t.Fatalf("safety agent unfroze the driver: %v", err)
	}
	if err := svc.Unsuspend(ctx, driverID, safety.ConfirmingAgent); err != nil || h.sim.Suspended(driverID) {
		t.Fatalf("supervisor unsuspend: %v", err)
	}
	_ = stream.TopicOps
}
