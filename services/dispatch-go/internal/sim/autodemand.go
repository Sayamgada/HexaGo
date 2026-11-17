package sim

import (
	"math"
	"time"
)

// autoDemand sizes the base demand from what the simulator observes, so the
// busy share stays near DemoTargetBusy whether routes come from OSRM (long,
// rate-limited road routes) or straight lines. By Little's law
//
//	busy drivers = matched requests/min × busy minutes per ride
//
// so base = target × active drivers ÷ (ride minutes × match share).
type autoDemand struct {
	on         bool
	rideMin    float64 // EWMA of accept → finish minutes
	matchShare float64 // EWMA of requests that got a driver (incl. re-dispatch)
	reqs       int     // since the last retune
	matched    int
	last       time.Time
}

const (
	retuneEvery   = 30 * time.Second
	rideEWMA      = 0.03 // weight of each finished ride
	shareEWMA     = 0.3  // weight of each 30 s window
	minMatchShare = 0.3
)

func newAutoDemand(on bool) autoDemand {
	return autoDemand{on: on, rideMin: avgRideMinutes, matchShare: 0.85}
}

// rideFinished records how long a driver was busy with one ride. Callers hold s.mu.
func (s *Sim) rideFinished(d time.Duration) {
	if d <= 0 {
		return
	}
	a := &s.auto
	a.rideMin += rideEWMA * (d.Minutes() - a.rideMin)
}

// countRequest records one dispatch outcome. Callers hold s.mu.
func (s *Sim) countRequest(newRequest, matched bool) {
	if newRequest {
		s.auto.reqs++
	}
	if matched {
		s.auto.matched++
	}
}

// retune updates the base demand. Scenarios and spikes distort ride times and
// match rates, so the estimate is frozen while any is active. Callers hold s.mu.
func (s *Sim) retune(now time.Time) {
	a := &s.auto
	if !a.on || now.Sub(a.last) < retuneEvery {
		return
	}
	a.last = now
	reqs, matched := a.reqs, a.matched
	a.reqs, a.matched = 0, 0
	if len(s.scenarios) > 0 || len(s.spikes) > 0 {
		return
	}
	if reqs >= 5 {
		share := math.Min(1, float64(matched)/float64(reqs))
		a.matchShare += shareEWMA * (share - a.matchShare)
	}
	active := 0
	for _, d := range s.order {
		if !d.offline {
			active++
		}
	}
	base := DemoTargetBusy * float64(active) / (a.rideMin * math.Max(a.matchShare, minMatchShare))
	s.base = math.Min(round(base, 2), MaxDemandPerMin)
}
