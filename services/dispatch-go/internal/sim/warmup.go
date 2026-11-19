package sim

import (
	"fmt"
	"math"
	"time"
)

// Cold-start ramp. After a restart every vehicle is idle and steady-state
// demand would take a full ride time (~25 min) to fill the fleet, so the demo
// map shows no on-trip (purple) vehicles for a long while. For WarmupWindow
// the simulator tops demand up towards WarmupBusyShare of the fleet with
// requests placed a short walk from idle vehicles: pickups are ~1 min away, so
// on-trip vehicles appear within the first two minutes.
const (
	WarmupWindow    = 2 * time.Minute
	WarmupBusyShare = 0.3              // share of active vehicles the ramp aims for (DemoTargetBusy is 0.5)
	warmupFill      = 30 * time.Second // the deficit is spread over this horizon
	warmupPickupKm  = 0.15             // pickup jitter around an idle vehicle
	warmupMaxPerSec = 20               // cap per tick-second, so a large fleet does not flood dispatch
)

// StartWarmup opens the cold-start ramp at now (Run calls it after Spawn).
func (s *Sim) StartWarmup(now time.Time) {
	s.mu.Lock()
	s.warmUntil = now.Add(WarmupWindow)
	s.mu.Unlock()
}

// Warming reports whether the cold-start ramp is active at now.
func (s *Sim) Warming(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.warmUntil)
}

// warmupRequests returns this tick's extra ramp requests. Callers hold s.mu.
func (s *Sim) warmupRequests(now time.Time) []rideRequest {
	if !now.Before(s.warmUntil) {
		return nil
	}
	active, busy := 0, 0
	var idle []*simDriver
	for _, d := range s.order {
		if d.offline || d.suspended {
			continue
		}
		active++
		switch {
		case d.ride != nil:
			busy++
		case d.incident == nil:
			idle = append(idle, d)
		}
	}
	deficit := WarmupBusyShare*float64(active) - float64(busy) - float64(len(s.open))
	if deficit < 1 || len(idle) == 0 {
		return nil
	}
	n := int(math.Ceil(deficit * s.tick.Seconds() / warmupFill.Seconds()))
	if limit := int(math.Max(1, warmupMaxPerSec*s.tick.Seconds())); n > limit {
		n = limit
	}
	if n > len(idle) {
		n = len(idle)
	}
	reqs := make([]rideRequest, n)
	for i := range reqs {
		d := idle[s.rng.IntN(len(idle))]
		p := s.jitter(d.pos, warmupPickupKm)
		reqs[i] = rideRequest{rider: fmt.Sprintf("r_sim_%04d", s.rng.IntN(5000)), pickup: p, dropoff: s.randomDropoff(p), tier: d.tier}
	}
	return reqs
}
