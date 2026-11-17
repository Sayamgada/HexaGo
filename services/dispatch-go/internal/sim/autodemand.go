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
//
// Ride minutes are estimated as busy-driver minutes ÷ rides, all decayed so
// the estimate follows the last ~10 minutes. Averaging only finished rides is
// biased after a restart (only short rides have finished yet: demand too
// high); dividing by rides finished overestimates and dividing by rides
// started underestimates while the fleet fills up, so the divisor is the
// mean of the two, which converges to the true value in steady state.
type autoDemand struct {
	on         bool
	exposure   float64 // decayed busy-driver minutes
	finished   float64 // decayed rides finished
	started    float64 // decayed rides started
	matchShare float64 // EWMA of requests that got a driver (incl. re-dispatch)
	reqs       int     // since the last retune
	matched    int
	last       time.Time
}

const (
	retuneEvery   = 30 * time.Second
	retuneDecay   = 0.95 // per retune: ~10 min memory
	shareEWMA     = 0.3  // weight of each 30 s window
	minMatchShare = 0.3
	priorRides    = 10.0 // pseudo-rides of avgRideMinutes seeding the estimate
	brakeAbove    = 0.6  // busy share above which demand is cut immediately
)

func newAutoDemand(on bool) autoDemand {
	return autoDemand{on: on, exposure: priorRides * avgRideMinutes, finished: priorRides, started: priorRides, matchShare: 0.85}
}

// rideMinutes is the current busy-minutes-per-ride estimate.
func (a *autoDemand) rideMinutes() float64 {
	return a.exposure / math.Max((a.started+a.finished)/2, 1e-9)
}

// accrue adds one tick of busy-driver time. Callers hold s.mu.
func (s *Sim) accrue(busy int, dt time.Duration) {
	s.auto.exposure += float64(busy) * dt.Minutes()
}

// rideFinished records that a driver finished (or dropped) a ride. Callers hold s.mu.
func (s *Sim) rideFinished() { s.auto.finished++ }

// rideStarted records that a driver accepted a ride. Callers hold s.mu.
func (s *Sim) rideStarted() { s.auto.started++ }

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
// match rates, so the demand is frozen while any is active (the ride-time
// estimate keeps learning). Callers hold s.mu.
func (s *Sim) retune(now time.Time) {
	a := &s.auto
	if !a.on || now.Sub(a.last) < retuneEvery {
		return
	}
	a.last = now
	rideMin := a.rideMinutes()
	a.exposure *= retuneDecay
	a.finished *= retuneDecay
	a.started *= retuneDecay
	reqs, matched := a.reqs, a.matched
	a.reqs, a.matched = 0, 0
	if len(s.scenarios) > 0 || len(s.spikes) > 0 {
		return
	}
	if reqs >= 5 {
		share := math.Min(1, float64(matched)/float64(reqs))
		a.matchShare += shareEWMA * (share - a.matchShare)
	}
	active, busy := 0, 0
	for _, d := range s.order {
		if !d.offline {
			active++
		}
		if d.ride != nil {
			busy++
		}
	}
	base := DemoTargetBusy * float64(active) / (rideMin * math.Max(a.matchShare, minMatchShare))
	// Brake: the estimate lags by a ride time, so if the fleet is already
	// overloaded, cut demand in proportion right away.
	if share := float64(busy) / math.Max(float64(active), 1); share > brakeAbove {
		base *= DemoTargetBusy / share
	}
	s.base = math.Min(round(base, 2), MaxDemandPerMin)
}
