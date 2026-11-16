package sim

import (
	"errors"
	"fmt"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

// RideStatus is the trip status streamed on ride:<id>.
//
//	accepted --route_ready--> arriving --reached_pickup--> on_trip --reached_dropoff--> completed
type RideStatus string

// Ride statuses (wire values).
const (
	RideAccepted  RideStatus = "accepted"  // driver assigned, route to pickup being planned
	RideArriving  RideStatus = "arriving"  // driving to the pickup
	RideOnTrip    RideStatus = "on_trip"   // rider on board, driving to the dropoff
	RideCompleted RideStatus = "completed" // dropped off
)

// RideEvent drives the state machine.
type RideEvent string

// Ride events.
const (
	EvRouteReady     RideEvent = "route_ready"
	EvReachedPickup  RideEvent = "reached_pickup"
	EvReachedDropoff RideEvent = "reached_dropoff"
)

// ErrInvalidTransition is returned for an event that is not legal in the current state.
var ErrInvalidTransition = errors.New("sim: invalid ride transition")

var transitions = map[RideStatus]map[RideEvent]RideStatus{
	RideAccepted: {EvRouteReady: RideArriving},
	RideArriving: {EvReachedPickup: RideOnTrip},
	RideOnTrip:   {EvReachedDropoff: RideCompleted},
}

// Transition returns the state after e. On error the state is unchanged.
func Transition(s RideStatus, e RideEvent) (RideStatus, error) {
	if next, ok := transitions[s][e]; ok {
		return next, nil
	}
	return s, fmt.Errorf("%w: %s + %s", ErrInvalidTransition, s, e)
}

// Terminal reports whether the ride is finished.
func (s RideStatus) Terminal() bool { return s == RideCompleted }

// StoreStatus maps to the persisted ride lifecycle.
func (s RideStatus) StoreStatus() store.RideStatus {
	switch s {
	case RideOnTrip:
		return store.RideInProgress
	case RideCompleted:
		return store.RideCompleted
	}
	return store.RideMatched
}

// DriverStatus is the driver's fleet status while the ride is in s.
func (s RideStatus) DriverStatus() fleet.Status {
	switch s {
	case RideOnTrip:
		return fleet.StatusOnTrip
	case RideCompleted:
		return fleet.StatusIdle
	}
	return fleet.StatusEnRoute
}
