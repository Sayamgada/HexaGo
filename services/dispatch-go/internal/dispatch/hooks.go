package dispatch

import (
	"context"

	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

// OnRideCompleted registers fn to run, synchronously and outside the ride
// lock, after a ride is persisted as completed (the ledger settles it).
func (s *Service) OnRideCompleted(fn func(context.Context, store.Ride)) {
	s.obsMu.Lock()
	s.completed = append(s.completed, fn)
	s.obsMu.Unlock()
}

func (s *Service) rideCompleted(ctx context.Context, r store.Ride) {
	s.obsMu.RLock()
	hooks := append([]func(context.Context, store.Ride){}, s.completed...)
	s.obsMu.RUnlock()
	for _, fn := range hooks {
		fn(ctx, r)
	}
}
