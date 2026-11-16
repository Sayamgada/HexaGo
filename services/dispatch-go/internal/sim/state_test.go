package sim

import (
	"errors"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func TestRideStateMachineHappyPath(t *testing.T) {
	s := RideAccepted
	steps := []struct {
		ev   RideEvent
		want RideStatus
	}{
		{EvRouteReady, RideArriving},
		{EvReachedPickup, RideOnTrip},
		{EvReachedDropoff, RideCompleted},
	}
	for _, st := range steps {
		next, err := Transition(s, st.ev)
		if err != nil {
			t.Fatalf("%s + %s: %v", s, st.ev, err)
		}
		if next != st.want {
			t.Fatalf("%s + %s = %s, want %s", s, st.ev, next, st.want)
		}
		s = next
	}
	if !s.Terminal() {
		t.Fatal("completed should be terminal")
	}
}

func TestRideStateMachineRejectsIllegalTransitions(t *testing.T) {
	illegal := []struct {
		from RideStatus
		ev   RideEvent
	}{
		{RideAccepted, EvReachedPickup},
		{RideAccepted, EvReachedDropoff},
		{RideArriving, EvRouteReady},
		{RideArriving, EvReachedDropoff},
		{RideOnTrip, EvRouteReady},
		{RideOnTrip, EvReachedPickup},
		{RideCompleted, EvRouteReady},
		{RideCompleted, EvReachedPickup},
		{RideCompleted, EvReachedDropoff},
		{RideStatus("bogus"), EvRouteReady},
		{RideAccepted, RideEvent("bogus")},
	}
	for _, c := range illegal {
		next, err := Transition(c.from, c.ev)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s + %s = %s, %v; want ErrInvalidTransition", c.from, c.ev, next, err)
		}
		if next != c.from {
			t.Errorf("%s + %s changed state to %s on error", c.from, c.ev, next)
		}
	}
}

func TestRideStatusWireValues(t *testing.T) {
	want := map[RideStatus]string{
		RideAccepted: "accepted", RideArriving: "arriving", RideOnTrip: "on_trip", RideCompleted: "completed",
	}
	for s, w := range want {
		if string(s) != w {
			t.Errorf("%v wire value %q, want %q", s, s, w)
		}
	}
}

func TestRideStatusMappings(t *testing.T) {
	cases := []struct {
		s      RideStatus
		store  store.RideStatus
		driver fleet.Status
	}{
		{RideAccepted, store.RideMatched, fleet.StatusEnRoute},
		{RideArriving, store.RideMatched, fleet.StatusEnRoute},
		{RideOnTrip, store.RideInProgress, fleet.StatusOnTrip},
		{RideCompleted, store.RideCompleted, fleet.StatusIdle},
	}
	for _, c := range cases {
		if got := c.s.StoreStatus(); got != c.store {
			t.Errorf("%s store status %s, want %s", c.s, got, c.store)
		}
		if got := c.s.DriverStatus(); got != c.driver {
			t.Errorf("%s driver status %s, want %s", c.s, got, c.driver)
		}
		if c.s.Terminal() != (c.s == RideCompleted) {
			t.Errorf("%s terminal %v", c.s, c.s.Terminal())
		}
	}
}
