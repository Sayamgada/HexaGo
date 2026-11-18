package dispatch

import (
	"context"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func TestOnRideCompletedRunsOnceAfterCompletion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.addDriver(t, "d1", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)
	var a, b []store.Ride
	h.svc.OnRideCompleted(func(_ context.Context, r store.Ride) { a = append(a, r) })
	h.svc.OnRideCompleted(func(_ context.Context, r store.Ride) { b = append(b, r) })
	p, d := geo.Point{Lat: 12.972, Lng: 77.595}, geo.Point{Lat: 12.99, Lng: 77.61}
	view, err := h.svc.CreateRide(ctx, CreateRideInput{RiderID: "r1", Pickup: &p, Dropoff: &d, Tier: "sedan"})
	if err != nil || view.Match == nil {
		t.Fatalf("%v %+v", err, view)
	}
	if _, err := h.svc.AdvanceRide(ctx, view.Ride.ID, store.RideInProgress); err != nil {
		t.Fatal(err)
	}
	if len(a) != 0 {
		t.Fatal("hook ran before completion")
	}
	if _, err := h.svc.AdvanceRide(ctx, view.Ride.ID, store.RideCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AdvanceRide(ctx, view.Ride.ID, store.RideCompleted); err == nil {
		t.Fatal("second completion accepted")
	}
	if len(a) != 1 || len(b) != 1 || a[0].ID != view.Ride.ID || a[0].Status != store.RideCompleted || a[0].DriverID != "d1" {
		t.Fatalf("hooks saw %+v / %+v", a, b)
	}
}
