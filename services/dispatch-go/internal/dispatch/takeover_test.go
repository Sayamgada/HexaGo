package dispatch

import (
	"context"
	"errors"
	"io"
	"log"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func (h *harness) seed(t *testing.T, ids ...string) {
	t.Helper()
	for i, id := range ids {
		lat := 12.9716 + float64(i)*0.002 // the first driver is closest to the toPickup
		h.addDriver(t, id, `{"status":"idle","tier":"mini","rating":4.8,"lat":`+ftoa(lat)+`,"lng":77.5946}`)
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

var toPickup = geo.Point{Lat: 12.9710, Lng: 77.5946}
var toDrop = geo.Point{Lat: 12.9900, Lng: 77.6100}

func TestRematchExcludingPicksAnotherDriverAndFreesTheDecliner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seed(t, "a", "b")
	var seen []store.Ride
	h.svc.SetRideObserver(func(r store.Ride) { seen = append(seen, r) })
	v, err := h.svc.CreateRide(ctx, CreateRideInput{RiderID: "r1", Pickup: &toPickup, Dropoff: &toDrop, Tier: "mini"})
	if err != nil || v.Match == nil || v.Ride.DriverID != "a" {
		t.Fatalf("create: %v %+v", err, v.Ride)
	}
	if _, err := h.svc.RematchExcluding(ctx, v.Ride.ID, "b", "declined"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong driver: %v", err)
	}
	if _, err := h.svc.RematchExcluding(ctx, "nope", "a", "declined"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ride: %v", err)
	}
	got, err := h.svc.RematchExcluding(ctx, v.Ride.ID, "a", "declined")
	if err != nil || got.Match == nil || got.Ride.DriverID != "b" || got.Ride.Status != store.RideMatched {
		t.Fatalf("rematch: %v %+v", err, got.Ride)
	}
	if d, _ := h.svc.Fleet().Get("a"); d.Status != fleet.StatusIdle {
		t.Fatalf("decliner is %s", d.Status)
	}
	if d, _ := h.svc.Fleet().Get("b"); d.Status != fleet.StatusEnRoute {
		t.Fatalf("new driver is %s", d.Status)
	}
	if len(seen) != 2 || seen[1].DriverID != "b" {
		t.Fatalf("observer saw %+v", seen)
	}
	stored, _ := h.store.GetRide(ctx, v.Ride.ID)
	if stored.DriverID != "b" {
		t.Fatalf("stored driver %q", stored.DriverID)
	}
}

func TestRematchWithNobodyElseReopensTheRequest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seed(t, "a")
	v, _ := h.svc.CreateRide(ctx, CreateRideInput{RiderID: "r1", Pickup: &toPickup, Dropoff: &toDrop, Tier: "mini"})
	got, err := h.svc.RematchExcluding(ctx, v.Ride.ID, "a", "timeout")
	if err != nil || got.Match != nil || got.Ride.Status != store.RideRequested || got.Ride.DriverID != "" {
		t.Fatalf("rematch: %v %+v", err, got.Ride)
	}
	if d, _ := h.svc.Fleet().Get("a"); d.Status != fleet.StatusIdle {
		t.Fatalf("decliner is %s", d.Status)
	}
	if _, err := h.svc.RematchExcluding(ctx, v.Ride.ID, "a", "timeout"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second rematch of an open request: %v", err)
	}
}

func TestCreateRideForDriverAssignsThatDriver(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seed(t, "a", "b")
	v, err := h.svc.CreateRideForDriver(ctx, CreateRideInput{RiderID: "r_demo", Pickup: &toPickup, Dropoff: &toDrop}, "b")
	if err != nil || v.Match == nil || v.Ride.DriverID != "b" || v.Ride.Tier != "mini" || v.Ride.ETASeconds <= 0 {
		t.Fatalf("send ride: %v %+v", err, v)
	}
	if len(v.Ride.Legs) != 2 || v.Ride.DistanceKm <= 0 {
		t.Fatalf("legs %+v", v.Ride)
	}
	if _, err := h.svc.CreateRideForDriver(ctx, CreateRideInput{RiderID: "r_demo", Pickup: &toPickup, Dropoff: &toDrop}, "b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("busy driver: %v", err)
	}
	if _, err := h.svc.CreateRideForDriver(ctx, CreateRideInput{RiderID: "r_demo", Pickup: &toPickup, Dropoff: &toDrop}, "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown driver: %v", err)
	}
	if _, err := h.svc.CreateRideForDriver(ctx, CreateRideInput{RiderID: "", Pickup: &toPickup, Dropoff: &toDrop}, "a"); !errors.Is(err, ErrValidation) {
		t.Fatalf("validation: %v", err)
	}
	if _, err := h.svc.CreateRideForDriver(ctx, CreateRideInput{RiderID: "r", Pickup: &geo.Point{Lat: 99}, Dropoff: &toDrop}, "a"); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad point: %v", err)
	}
}

// flakyRides fails ride reads or updates on demand.
type flakyRides struct {
	*store.Memory
	failGet, failUpdate, failCreate bool
}

func (f *flakyRides) GetRide(ctx context.Context, id string) (store.Ride, error) {
	if f.failGet {
		return store.Ride{}, errors.New("db down")
	}
	return f.Memory.GetRide(ctx, id)
}

func (f *flakyRides) UpdateRide(ctx context.Context, r store.Ride) error {
	if f.failUpdate {
		return errors.New("db down")
	}
	return f.Memory.UpdateRide(ctx, r)
}

func (f *flakyRides) CreateRide(ctx context.Context, r store.Ride) error {
	if f.failCreate {
		return errors.New("db down")
	}
	return f.Memory.CreateRide(ctx, r)
}

func flakyHarness(t *testing.T) (*harness, *flakyRides) {
	t.Helper()
	mem := store.NewMemory()
	fr := &flakyRides{Memory: mem}
	svc, err := New(Options{H3Resolution: 9, Center: center, Store: fr, Telemetry: mem, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc.Routes(r)
	return &harness{svc: svc, router: r, store: mem}, fr
}

func TestTakeoverDispatchPersistenceFailuresReleaseDrivers(t *testing.T) {
	ctx := context.Background()
	h, fr := flakyHarness(t)
	h.seed(t, "a", "b")
	v, _ := h.svc.CreateRide(ctx, CreateRideInput{RiderID: "r1", Pickup: &toPickup, Dropoff: &toDrop, Tier: "mini"})

	fr.failGet = true
	if _, err := h.svc.RematchExcluding(ctx, v.Ride.ID, "a", "declined"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("read failure: %v", err)
	}
	fr.failGet, fr.failUpdate = false, true
	if _, err := h.svc.RematchExcluding(ctx, v.Ride.ID, "a", "declined"); err == nil {
		t.Fatal("update failure not reported")
	}
	if d, _ := h.svc.Fleet().Get("b"); d.Status != fleet.StatusIdle {
		t.Fatalf("new driver kept after a failed re-match: %s", d.Status)
	}
	fr.failUpdate = false

	if _, err := h.svc.CreateRideForDriver(ctx, CreateRideInput{RiderID: "r", Pickup: &toPickup, Dropoff: &geo.Point{Lat: 99}}, "b"); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad dropoff: %v", err)
	}
	fr.failCreate = true
	if _, err := h.svc.CreateRideForDriver(ctx, CreateRideInput{RiderID: "r", Pickup: &toPickup, Dropoff: &toDrop}, "b"); err == nil {
		t.Fatal("create failure not reported")
	}
	if d, _ := h.svc.Fleet().Get("b"); d.Status != fleet.StatusIdle {
		t.Fatalf("driver kept after a failed send-ride: %s", d.Status)
	}
}

func TestPastTense(t *testing.T) {
	for reason, want := range map[string]string{"timeout": "let time out", "released": "left take-over mode with", "declined": "declined"} {
		if got := pastTense(reason); got != want {
			t.Fatalf("%s: %q", reason, got)
		}
	}
}
