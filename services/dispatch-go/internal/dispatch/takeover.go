package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/matching"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

// Dispatch support for take-over mode (a human driving a simulated car, see
// sim/takeover.go): re-matching a ride the human declined or let time out,
// and the DEMO_MODE "Send me a ride" request aimed at one driver.

// RematchExcluding re-runs matching for a matched ride whose driver declined
// it (or let the offer time out). The declining driver is still claimed
// (en_route) while the matcher runs, so it cannot win again; it is released
// afterwards. With no other driver the ride is reopened (status "requested").
func (s *Service) RematchExcluding(ctx context.Context, rideID, driverID, reason string) (RideView, error) {
	s.rideMu.Lock()
	r, err := s.store.GetRide(ctx, rideID)
	if errors.Is(err, store.ErrNotFound) {
		s.rideMu.Unlock()
		return RideView{}, fmt.Errorf("%w: ride %s", ErrNotFound, rideID)
	}
	if err != nil {
		s.rideMu.Unlock()
		return RideView{}, err
	}
	if r.Status != store.RideMatched || r.DriverID != driverID {
		s.rideMu.Unlock()
		return RideView{Ride: r}, fmt.Errorf("%w: ride %s is %s with driver %q, not matched to %s", ErrConflict, rideID, r.Status, r.DriverID, driverID)
	}
	req := matching.Request{Pickup: r.Pickup, Dropoff: r.Dropoff, Tier: fleet.Tier(r.Tier)}
	res, merr := s.matcher.Match(req)
	var view *MatchView
	if merr == nil {
		res, view = s.claimBest(&r, res, r.DistanceKm)
	}
	if view == nil {
		r.Status, r.DriverID, r.ETASeconds, r.MatchScore = store.RideRequested, "", 0, 0
	}
	r.UpdatedAt = s.now()
	if err := s.store.UpdateRide(ctx, r); err != nil {
		s.rideMu.Unlock()
		if view != nil {
			s.release(r.DriverID)
		}
		return RideView{}, fmt.Errorf("persist ride: %w", err)
	}
	s.rideMu.Unlock()
	s.release(driverID)

	note := fmt.Sprintf("re-matched after %s %s the offer", driverID, pastTense(reason))
	if view != nil {
		s.rec.IncMatched()
	} else {
		s.rec.IncUnmatched()
		note = fmt.Sprintf("%s %s the offer; no other driver, request reopened", driverID, pastTense(reason))
	}
	s.recordMatchEvent(ctx, r, req, view, res, note)
	if view != nil {
		s.notify(r)
	}
	return RideView{Ride: r, Match: view}, nil
}

func pastTense(reason string) string {
	switch reason {
	case "timeout":
		return "let time out"
	case "released":
		return "left take-over mode with"
	}
	return "declined"
}

// CreateRideForDriver books a ride and assigns it to driverID directly (the
// DEMO_MODE "Send me a ride" button), in the driver's own tier. The driver
// must be idle and eligible (an AV needs enough charge for the trip).
func (s *Service) CreateRideForDriver(ctx context.Context, in CreateRideInput, driverID string) (RideView, error) {
	in.RiderID = strings.TrimSpace(in.RiderID)
	if in.RiderID == "" || in.Pickup == nil || in.Dropoff == nil {
		return RideView{}, fmt.Errorf("%w: rider_id, pickup and dropoff are required", ErrValidation)
	}
	if err := in.Pickup.Validate(); err != nil {
		return RideView{}, fmt.Errorf("%w: pickup: %v", ErrValidation, err)
	}
	if err := in.Dropoff.Validate(); err != nil {
		return RideView{}, fmt.Errorf("%w: dropoff: %v", ErrValidation, err)
	}
	d, ok := s.fleet.Get(driverID)
	if !ok {
		return RideView{}, fmt.Errorf("%w: driver %s", ErrNotFound, driverID)
	}
	req := matching.Request{Pickup: *in.Pickup, Dropoff: *in.Dropoff, Tier: d.Tier}
	now := s.now()
	score, eligible := matching.Score(s.matcher.Config(), d, req, now)
	if !eligible {
		return RideView{}, fmt.Errorf("%w: driver %s is %s and cannot take this ride", ErrConflict, driverID, d.Status)
	}
	claimed, err := s.fleet.Claim(driverID, fleet.StatusIdle, fleet.StatusEnRoute)
	if err != nil {
		return RideView{}, fmt.Errorf("%w: driver %s: %v", ErrConflict, driverID, err)
	}
	pickupCell, _ := s.geo.CellOf(req.Pickup)
	tripKm := geo.HaversineKm(req.Pickup, req.Dropoff) * s.matcher.Config().RoadFactor
	ride := store.Ride{
		ID: newRideID(), RiderID: in.RiderID, Tier: string(d.Tier), Status: store.RideRequested,
		Pickup: req.Pickup, Dropoff: req.Dropoff, PickupH3: pickupCell.String(), DistanceKm: tripKm,
		Legs: []store.RideLeg{}, CreatedAt: now, UpdatedAt: now,
	}
	best := matching.Candidate{Driver: claimed, Score: score}
	s.assign(&ride, best, tripKm)
	res := matching.Result{Best: best, Ranked: []matching.Candidate{best}, Considered: 1, RingsSearched: 1,
		RequestedTier: d.Tier, Tier: d.Tier, TiersTried: []fleet.Tier{d.Tier}}
	if err := s.store.CreateRide(ctx, ride); err != nil {
		s.release(driverID)
		return RideView{}, fmt.Errorf("persist ride: %w", err)
	}
	view := buildMatchView(res)
	s.rec.IncMatched()
	s.demand.add(demandPoint{p: req.Pickup, at: now, matched: true})
	s.recordMatchEvent(ctx, ride, req, view, res, "demo request sent to the taken-over driver")
	s.notify(ride)
	return RideView{Ride: ride, Match: view}, nil
}
