package sim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// offers returns the ride_offer / offer_closed messages published on driver:<id>.
func (r *recorder) offers(driverID string) []RideOffer {
	var out []RideOffer
	for _, m := range r.all() {
		if m.topic == stream.DriverTopic(driverID) {
			out = append(out, m.data.(RideOffer))
		}
	}
	return out
}

func takeoverHarness(t *testing.T, drivers int) (*harness, string) {
	t.Helper()
	h := newHarness(t, drivers, func(o *Options) { o.DemandPerMin = 0 })
	h.sim.Spawn(context.Background())
	id := DriverID(1)
	if _, err := h.sim.TakeOver(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return h, id
}

// sendRide sends a demo ride to id and steps once so the offer goes out.
func sendRide(t *testing.T, h *harness, id string) (dispatch.RideView, RideOffer) {
	t.Helper()
	v, err := h.sim.SendRide(context.Background(), id)
	if err != nil {
		t.Fatalf("send ride: %v", err)
	}
	h.step(context.Background(), 1)
	got := h.pub.offers(id)
	if len(got) == 0 || got[len(got)-1].Type != "ride_offer" {
		t.Fatalf("no ride_offer on driver:%s: %+v", id, got)
	}
	return v, got[len(got)-1]
}

func TestTakeoverOffersInsteadOfAutoAcceptingAndAcceptStartsTheTrip(t *testing.T) {
	h, id := takeoverHarness(t, 3)
	ctx := context.Background()
	view, err := h.sim.Takeover(id)
	if err != nil || !view.HumanControlled || view.TTLS != int(TakeoverTTL/time.Second) || view.Offer != nil {
		t.Fatalf("takeover view %+v %v", view, err)
	}
	ride, offer := sendRide(t, h, id)
	if ride.Ride.DriverID != id || offer.RideID != ride.Ride.ID || offer.FareINR <= 0 || offer.PickupETASec <= 0 ||
		offer.TimeoutS != int(OfferTimeout/time.Second) || offer.TripKm <= 0 || offer.Pickup != ride.Ride.Pickup {
		t.Fatalf("offer %+v for ride %+v", offer, ride.Ride)
	}
	if d, _ := h.sim.Driver(id); d.Ride != nil {
		t.Fatalf("offered ride must not start before accept: %+v", d.Ride)
	}
	if v, _ := h.sim.Takeover(id); v.Offer == nil || v.Offer.RideID != ride.Ride.ID {
		t.Fatalf("pending offer not in view: %+v", v)
	}
	h.step(ctx, 5)
	if got := h.pub.rideStatuses(ride.Ride.ID); len(got) != 0 {
		t.Fatalf("ride progressed without accept: %v", got)
	}
	res, err := h.sim.RespondOffer(ctx, ride.Ride.ID, true)
	if err != nil || res.Decision != "accepted" || res.DriverID != id {
		t.Fatalf("accept: %+v %v", res, err)
	}
	if last := h.pub.offers(id); last[len(last)-1].Type != "offer_closed" || last[len(last)-1].Reason != "accepted" {
		t.Fatalf("closed message %+v", last)
	}
	done := h.runUntil(ctx, 1500, func() bool {
		r, _ := h.mem.GetRide(ctx, ride.Ride.ID)
		return r.Status == store.RideCompleted
	})
	if !done {
		t.Fatalf("accepted ride never completed: %v", h.pub.rideStatuses(ride.Ride.ID))
	}
	if _, err := h.sim.RespondOffer(ctx, ride.Ride.ID, true); !errors.Is(err, ErrNoOffer) {
		t.Fatalf("second accept: %v", err)
	}
}

func TestDeclineRematchesExcludingTheHuman(t *testing.T) {
	h, id := takeoverHarness(t, 40)
	ctx := context.Background()
	ride, _ := sendRide(t, h, id)
	res, err := h.sim.RespondOffer(ctx, ride.Ride.ID, false)
	if err != nil || res.Decision != "declined" || res.RematchedTo == "" || res.RematchedTo == id || res.Status != "matched" {
		t.Fatalf("decline: %+v %v", res, err)
	}
	last := h.pub.offers(id)
	if c := last[len(last)-1]; c.Type != "offer_closed" || c.Reason != "declined" {
		t.Fatalf("closed %+v", c)
	}
	if d, _ := h.svc.Fleet().Get(id); d.Status != fleet.StatusIdle {
		t.Fatalf("decliner is %s", d.Status)
	}
	h.step(ctx, 3) // the new (simulated) driver auto-accepts
	if got := h.pub.rideStatuses(ride.Ride.ID); len(got) == 0 || got[0] != "accepted" {
		t.Fatalf("re-matched ride statuses %v", got)
	}
}

func TestOfferTimesOutAndDeclinedRideIsNeverOfferedAgain(t *testing.T) {
	h, id := takeoverHarness(t, 1) // nobody else to take it
	ctx := context.Background()
	ride, _ := sendRide(t, h, id)
	h.step(ctx, int(OfferTimeout/time.Second)+1)
	closed := h.pub.offers(id)
	if c := closed[len(closed)-1]; c.Type != "offer_closed" || c.Reason != "timeout" {
		t.Fatalf("closed %+v", closed)
	}
	r, _ := h.mem.GetRide(ctx, ride.Ride.ID)
	if r.Status != store.RideRequested {
		t.Fatalf("timed-out ride with no other driver: %s", r.Status)
	}
	// The open request is retried every 5 s; it may pick the human again but is not re-offered.
	for i := 0; i < 12; i++ {
		h.clock.advance(time.Second)
		h.sim.DemandTick(ctx)
		h.sim.Step(ctx)
	}
	n := 0
	for _, o := range h.pub.offers(id) {
		if o.Type == "ride_offer" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("declined ride offered %d times", n)
	}
	if d, _ := h.svc.Fleet().Get(id); d.Status != fleet.StatusIdle {
		t.Fatalf("human driver is %s", d.Status)
	}
}

func TestTakeoverTTLRenewsAndExpiresBackToSimulator(t *testing.T) {
	h, id := takeoverHarness(t, 40)
	ctx := context.Background()
	h.clock.advance(TakeoverTTL - time.Minute)
	if _, err := h.sim.TakeOver(ctx, id); err != nil { // renewed while the screen is open
		t.Fatal(err)
	}
	h.clock.advance(TakeoverTTL - time.Minute)
	h.step(ctx, 1)
	if v, _ := h.sim.Takeover(id); !v.HumanControlled {
		t.Fatal("renewed takeover expired early")
	}
	ride, _ := sendRide(t, h, id)
	h.clock.advance(2 * time.Minute)
	h.step(ctx, 1)
	if v, _ := h.sim.Takeover(id); v.HumanControlled || v.ExpiresAt != nil {
		t.Fatalf("expired takeover still active: %+v", v)
	}
	r, _ := h.mem.GetRide(ctx, ride.Ride.ID)
	if r.DriverID == id {
		t.Fatalf("pending offer of an expired takeover was not re-matched: %+v", r)
	}
	found := false
	for _, ev := range h.pub.agentEvents("fleet_simulator") {
		if ev.Kind == "takeover" && containsAll(ev.Decision, id, "simulator") {
			found = true
		}
	}
	if !found {
		t.Fatal("no takeover-ended event")
	}
}

func TestReleaseRematchesPendingOfferAndHumanDriverParks(t *testing.T) {
	h, id := takeoverHarness(t, 40)
	ctx := context.Background()
	before, _ := h.svc.Fleet().Get(id)
	h.step(ctx, 120) // a simulated idle driver would have wandered off by now
	after, _ := h.svc.Fleet().Get(id)
	if geo.HaversineKm(before.Point(), after.Point()) > 0.001 {
		t.Fatalf("human-controlled driver moved %.3f km", geo.HaversineKm(before.Point(), after.Point()))
	}
	ride, _ := sendRide(t, h, id)
	v, err := h.sim.Release(ctx, id)
	if err != nil || v.HumanControlled {
		t.Fatalf("release: %+v %v", v, err)
	}
	r, _ := h.mem.GetRide(ctx, ride.Ride.ID)
	if r.DriverID == id {
		t.Fatalf("released driver kept the offered ride")
	}
	if c := h.pub.offers(id); c[len(c)-1].Reason != "released" {
		t.Fatalf("closed %+v", c[len(c)-1])
	}
	if _, err := h.sim.Release(ctx, id); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestCancelledOfferIsWithdrawn(t *testing.T) {
	h, id := takeoverHarness(t, 2)
	ctx := context.Background()
	ride, _ := sendRide(t, h, id)
	if _, err := h.svc.CancelRide(ctx, ride.Ride.ID, "rider_cancelled"); err != nil {
		t.Fatal(err)
	}
	h.step(ctx, 1)
	if c := h.pub.offers(id); c[len(c)-1].Reason != "cancelled" {
		t.Fatalf("closed %+v", c[len(c)-1])
	}
	if _, err := h.sim.RespondOffer(ctx, ride.Ride.ID, true); !errors.Is(err, ErrNoOffer) {
		t.Fatalf("accepting a cancelled offer: %v", err)
	}
}

func TestTakeoverErrors(t *testing.T) {
	h, id := takeoverHarness(t, 2)
	ctx := context.Background()
	if _, err := h.sim.TakeOver(ctx, "d_999"); !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := h.sim.Takeover("d_999"); !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("unknown view: %v", err)
	}
	if _, err := h.sim.Release(ctx, "d_999"); !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("unknown release: %v", err)
	}
	if _, err := h.sim.SendRide(ctx, DriverID(2)); !errors.Is(err, ErrNotTakenOver) {
		t.Fatalf("send to a simulated driver: %v", err)
	}
	if _, err := h.sim.SendRide(ctx, "d_999"); !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("send to unknown: %v", err)
	}
	if _, err := h.sim.RespondOffer(ctx, "nope", false); !errors.Is(err, ErrNoOffer) {
		t.Fatalf("respond unknown: %v", err)
	}
	sendRide(t, h, id)
	if _, err := h.sim.SendRide(ctx, id); !errors.Is(err, ErrDriverBusy) {
		t.Fatalf("send while an offer is pending: %v", err)
	}
	if !h.sim.HumanControlled(id) || h.sim.HumanControlled(DriverID(2)) {
		t.Fatal("HumanControlled")
	}
}

func TestHumanDriverStaysOnlineInRain(t *testing.T) {
	h, id := takeoverHarness(t, 8)
	ctx := context.Background()
	if _, err := h.sim.TriggerScenario(ctx, "rain_storm"); err != nil {
		t.Fatal(err)
	}
	h.step(ctx, 2)
	if d, _ := h.svc.Fleet().Get(id); d.Status == fleet.StatusOffline {
		t.Fatal("human-controlled driver was logged off by the rain")
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !contains(s, p) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

type fixedRouter struct{ err error }

func (r fixedRouter) Route(_ context.Context, a, b geo.Point) ([]geo.Point, error) {
	if r.err != nil {
		return nil, r.err
	}
	return []geo.Point{a, {Lat: a.Lat, Lng: b.Lng}, b}, nil
}

func TestRouteForUsesTheRouterWithStraightLineFallback(t *testing.T) {
	a, b := geo.Point{Lat: 12.97, Lng: 77.59}, geo.Point{Lat: 12.98, Lng: 77.61}
	h := newHarness(t, 1)
	if pts, src := h.sim.RouteFor(context.Background(), a, b); src != "straight_line" || len(pts) != 2 {
		t.Fatalf("straight: %s %v", src, pts)
	}
	h = newHarness(t, 1, func(o *Options) { o.Router = fixedRouter{} })
	if pts, src := h.sim.RouteFor(context.Background(), a, b); src != "osrm" || len(pts) != 3 {
		t.Fatalf("road: %s %v", src, pts)
	}
	h = newHarness(t, 1, func(o *Options) { o.Router = fixedRouter{err: errors.New("down")} })
	if _, src := h.sim.RouteFor(context.Background(), a, b); src != "straight_line" {
		t.Fatalf("fallback: %s", src)
	}
}
