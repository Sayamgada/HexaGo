package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

// InterruptRide ends a matched or in-progress ride early (a safety incident):
// the ride is cancelled with reason and its driver is taken offline so the
// dispatcher stops offering it work. The ride observer is notified.
func (s *Service) InterruptRide(ctx context.Context, id, reason string) (store.Ride, error) {
	s.rideMu.Lock()
	r, err := s.store.GetRide(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.rideMu.Unlock()
		return store.Ride{}, fmt.Errorf("%w: ride %s", ErrNotFound, id)
	}
	if err != nil {
		s.rideMu.Unlock()
		return store.Ride{}, err
	}
	if !r.Status.Active() {
		s.rideMu.Unlock()
		return store.Ride{}, fmt.Errorf("%w: ride is %s and cannot be interrupted", ErrConflict, r.Status)
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "safety_incident"
	}
	r.Status, r.CancelReason, r.UpdatedAt = store.RideCancelled, reason, s.now()
	if err := s.store.UpdateRide(ctx, r); err != nil {
		s.rideMu.Unlock()
		return store.Ride{}, err
	}
	s.rideMu.Unlock()
	if r.DriverID != "" {
		if _, err := s.fleet.Apply(r.DriverID, func(d *fleet.Driver, exists bool) error {
			if !exists {
				return fleet.ErrNotFound
			}
			d.Status = fleet.StatusOffline
			return nil
		}); err != nil && !errors.Is(err, fleet.ErrNotFound) {
			s.log.Printf("WARN dispatch: take driver %s offline: %v", r.DriverID, err)
		}
	}
	s.notify(r)
	return r, nil
}
