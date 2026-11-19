package dispatch

import (
	"sync"
	"time"
)

// DefaultMatchFeedInterval is the minimum gap between Dispatch Broker match
// decisions published to the Ops feed. Every decision is still written to
// agent_events; only the live feed is thinned so other agents stay visible.
const DefaultMatchFeedInterval = 5 * time.Second

// feedLimiter lets one event through per interval and counts the ones it drops.
type feedLimiter struct {
	mu         sync.Mutex
	interval   time.Duration // <= 0: unlimited
	last       time.Time
	suppressed int
}

// allow reports whether an event at `at` may be published and, if so, how many
// were suppressed since the previous published one.
func (f *feedLimiter) allow(at time.Time) (ok bool, skipped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.interval <= 0 {
		return true, 0
	}
	if !f.last.IsZero() && at.Sub(f.last) < f.interval {
		f.suppressed++
		return false, 0
	}
	f.last, skipped, f.suppressed = at, f.suppressed, 0
	return true, skipped
}

// SetMatchFeedInterval changes the Ops feed rate limit for match events (<= 0 disables it).
func (s *Service) SetMatchFeedInterval(d time.Duration) {
	s.feed.mu.Lock()
	s.feed.interval = d
	s.feed.mu.Unlock()
}
