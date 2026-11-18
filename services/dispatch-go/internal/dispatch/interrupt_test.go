package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func TestInterruptRideCancelsAndTakesDriverOffline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.addDriver(t, "d1", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)
	var seen []store.Ride
	h.svc.SetRideObserver(func(r store.Ride) { seen = append(seen, r) })
	p, d := geo.Point{Lat: 12.972, Lng: 77.595}, geo.Point{Lat: 12.99, Lng: 77.61}
	view, err := h.svc.CreateRide(ctx, CreateRideInput{RiderID: "r1", Pickup: &p, Dropoff: &d, Tier: "sedan"})
	if err != nil || view.Match == nil {
		t.Fatalf("%v %+v", err, view)
	}
	if _, err := h.svc.AdvanceRide(ctx, view.Ride.ID, store.RideInProgress); err != nil {
		t.Fatal(err)
	}
	r, err := h.svc.InterruptRide(ctx, view.Ride.ID, " ")
	if err != nil || r.Status != store.RideCancelled || r.CancelReason != "safety_incident" {
		t.Fatalf("%+v %v", r, err)
	}
	if fd, _ := h.svc.Fleet().Get("d1"); fd.Status != fleet.StatusOffline {
		t.Fatalf("driver %s", fd.Status)
	}
	if len(seen) == 0 || seen[len(seen)-1].Status != store.RideCancelled {
		t.Fatalf("observer %+v", seen)
	}
	if _, err := h.svc.InterruptRide(ctx, view.Ride.ID, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal ride: %v", err)
	}
	if _, err := h.svc.InterruptRide(ctx, "missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ride: %v", err)
	}
	// A ride whose driver is no longer in the fleet still ends cleanly.
	view2, _ := h.svc.CreateRide(ctx, CreateRideInput{RiderID: "r2", Pickup: &p, Dropoff: &d, Tier: "sedan"})
	h.addDriver(t, "d2", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)
	view2, _ = h.svc.RetryRide(ctx, view2.Ride.ID)
	if view2.Match != nil {
		h.svc.Fleet().Remove(view2.Ride.DriverID)
		if r, err := h.svc.InterruptRide(ctx, view2.Ride.ID, "rider distress"); err != nil || r.CancelReason != "rider distress" {
			t.Fatalf("%+v %v", r, err)
		}
	}
}
