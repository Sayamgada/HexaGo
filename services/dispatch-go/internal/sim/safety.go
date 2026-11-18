package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// observation snapshots an on_trip vehicle for the safety detector. Callers hold s.mu.
func (s *Sim) observation(d *simDriver, now time.Time) safety.Observation {
	o := safety.Observation{
		RideID: d.ride.id, DriverID: d.id, RiderID: d.ride.rider, Tier: string(d.tier),
		Pos: d.pos, SpeedKmh: d.speed, Pickup: d.ride.pickup, Dropoff: d.ride.dropoff, At: now,
	}
	switch {
	case d.incident != nil && d.incident.plan != nil:
		o.Plan = d.incident.plan.pts // the route the vehicle should have kept to
	case d.route != nil && d.purpose == toDropoff:
		o.Plan = d.route.pts // paths are immutable once built, so sharing is race-free
	}
	return o
}

// Suspend implements safety.VehicleController: the vehicle stops where it is
// and goes offline; its ride is interrupted and, for an AV on a trip, a
// replacement ride is dispatched from the vehicle's position to the drop-off.
func (s *Sim) Suspend(ctx context.Context, driverID, reason string) (safety.SuspendResult, error) {
	s.mu.Lock()
	d, ok := s.drivers[driverID]
	if !ok {
		s.mu.Unlock()
		return safety.SuspendResult{}, fmt.Errorf("%w: %s", safety.ErrUnknownDriver, driverID)
	}
	ride := d.ride
	onTrip := ride != nil && ride.status == RideOnTrip
	if ride != nil {
		s.rideFinished()
		d.ride = nil
	}
	d.suspended, d.incident = true, nil
	s.clearRoute(d)
	d.speed = 0
	pos, tier := d.pos, d.tier
	wd := s.wireDriver(d)
	s.mu.Unlock()

	res := safety.SuspendResult{DriverID: driverID, Tier: string(tier), VehicleStopped: true, Location: pos}
	if _, err := s.svc.Fleet().Apply(driverID, func(fd *fleet.Driver, exists bool) error {
		if !exists {
			return fleet.ErrNotFound
		}
		fd.Status, fd.SpeedKmh = fleet.StatusOffline, 0
		return nil
	}); err != nil {
		s.log.Printf("WARN sim: take %s offline: %v", driverID, err)
	}
	if ride == nil {
		return res, nil
	}
	if reason == "" {
		reason = "safety_incident"
	}
	if _, err := s.svc.InterruptRide(ctx, ride.id, reason); err != nil {
		s.log.Printf("WARN sim: interrupt ride %s: %v", ride.id, err)
	} else {
		res.InterruptedRideID = ride.id
	}
	s.pub.Publish(stream.RideTopic(ride.id), stream.RideUpdate{RideID: ride.id, Status: "interrupted", Driver: wd})
	if tier != fleet.TierAV || !onTrip {
		return res, nil
	}
	from, to := pos, ride.dropoff
	view, err := s.svc.CreateRide(ctx, dispatch.CreateRideInput{RiderID: ride.rider, Pickup: &from, Dropoff: &to, Tier: string(fleet.TierAV)})
	if err != nil {
		s.log.Printf("WARN sim: replacement ride for %s: %v", ride.id, err)
		return res, nil
	}
	res.ReplacementRideID = view.Ride.ID
	if view.Match == nil {
		s.mu.Lock()
		now := s.now()
		s.open = append(s.open, openRequest{id: view.Ride.ID, created: now, tried: now})
		s.mu.Unlock()
	}
	s.announce(ctx, fmt.Sprintf("replacement AV ride %s dispatched for the rider of %s", view.Ride.ID, ride.id),
		fmt.Sprintf("%s was stopped by the Safety Sentinel at %.5f, %.5f; the rider continues to the original drop-off.", driverID, pos.Lat, pos.Lng))
	return res, nil
}

// Unsuspend implements safety.VehicleController: the vehicle rejoins the fleet
// (idle) on the next tick.
func (s *Sim) Unsuspend(_ context.Context, driverID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drivers[driverID]
	if !ok {
		return fmt.Errorf("%w: %s", safety.ErrUnknownDriver, driverID)
	}
	d.suspended = false
	d.dwell = s.now().Add(10 * time.Second)
	return nil
}

// Suspended reports whether the simulator holds driverID suspended.
func (s *Sim) Suspended(driverID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drivers[driverID]
	return ok && d.suspended
}

// position returns a driver's simulated position (tests).
func (s *Sim) position(driverID string) (geo.Point, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drivers[driverID]
	if !ok {
		return geo.Point{}, false
	}
	return d.pos, true
}
