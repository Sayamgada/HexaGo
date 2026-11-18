package safety

import "context"

// SuspensionHook observes a suspension (escalation) or its Ops-confirmed release.
type SuspensionHook func(ctx context.Context, sp Suspension)

// OnSuspension registers hooks run synchronously after a driver is suspended
// (the ledger holds that driver's payouts) and after an ops_supervisor-
// confirmed release (the ledger releases them). Either may be nil.
func (s *Service) OnSuspension(suspended, released SuspensionHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if suspended != nil {
		s.onSuspend = append(s.onSuspend, suspended)
	}
	if released != nil {
		s.onRelease = append(s.onRelease, released)
	}
}

func (s *Service) fire(ctx context.Context, release bool, sp Suspension) {
	s.mu.Lock()
	hooks := s.onSuspend
	if release {
		hooks = s.onRelease
	}
	hs := append([]SuspensionHook{}, hooks...)
	s.mu.Unlock()
	for _, h := range hs {
		h(ctx, sp)
	}
}
