package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Scenario parameters.
const (
	ScenarioDuration = 5 * time.Minute
	ConcertBurst     = 20   // requests fired the moment the concert lets out
	concertPerMin    = 30.0 // extra requests/min around the venue afterwards
	detourSideKm     = 0.45 // safety incident: swerve this far off the route...
	detourAheadKm    = 0.15 // ...while moving this far forward
	incidentSpeed    = 50.0 // km/h while deviating
	incidentStop     = 25 * time.Second
	deviationAlertKm = 0.3
)

// Scenario errors.
var (
	ErrUnknownScenario = errors.New("sim: unknown scenario")
	ErrNoOnTripVehicle = errors.New("sim: no vehicle is on a trip right now")
)

// Scenarios lists the supported scenario names.
var Scenarios = []string{"rush_hour", "rain_storm", "concert_exit", "safety_incident"}

// ScenarioView is the POST /v1/sim/scenario/{name} response.
type ScenarioView struct {
	Name        string    `json:"name"`
	ActiveUntil time.Time `json:"active_until"`
	Detail      string    `json:"detail"`
	Location    geo.Point `json:"location"`
	DriverID    string    `json:"driver_id,omitempty"`
	RideID      string    `json:"ride_id,omitempty"`
}

// TriggerScenario starts a what-if scenario.
func (s *Sim) TriggerScenario(ctx context.Context, name string) (ScenarioView, error) {
	now := s.now()
	until := now.Add(ScenarioDuration)
	v := ScenarioView{Name: name, ActiveUntil: until, Location: s.center}
	switch name {
	case "rush_hour":
		s.addScenario(scenarioState{name: name, until: until, demandMult: 2.5, speedFactor: 0.7})
		v.Detail = "demand x2.5, traffic speed x0.7 for 5m"
	case "rain_storm":
		s.addScenario(scenarioState{name: name, until: until, demandMult: 1.8, speedFactor: 0.55})
		v.Detail = "demand x1.8, traffic speed x0.55 for 5m"
	case "concert_exit":
		venue := shift(ConcertVenue, s.center)
		venue = geo.Point{Lat: round(venue.Lat, 6), Lng: round(venue.Lng, 6)}
		v.Location = venue
		v.Detail = fmt.Sprintf("%d requests now, then +%.0f/min around the venue for 5m", ConcertBurst, concertPerMin)
		s.mu.Lock()
		s.scenarios = append(s.scenarios, scenarioState{name: name, until: until, demandMult: 1, speedFactor: 0.9})
		s.spikes = append(s.spikes, spike{name: "concert exit", at: venue, perMin: concertPerMin, until: until})
		reqs := make([]rideRequest, ConcertBurst)
		for i := range reqs {
			p := s.jitter(venue, 0.3)
			reqs[i] = rideRequest{rider: fmt.Sprintf("r_sim_%04d", s.rng.IntN(5000)), pickup: p, dropoff: s.randomDropoff(p), tier: s.pickTier()}
		}
		s.mu.Unlock()
		s.announce(ctx, "scenario concert_exit started: "+v.Detail, "Crowd leaving M. Chinnaswamy Stadium; injected by the fleet simulator.")
		s.createRides(ctx, reqs, now)
		return v, nil
	case "safety_incident":
		return s.triggerIncident(ctx, v)
	default:
		return ScenarioView{}, fmt.Errorf("%w: %q (want one of %v)", ErrUnknownScenario, name, Scenarios)
	}
	s.announce(ctx, fmt.Sprintf("scenario %s started: %s", name, v.Detail), "What-if scenario injected by the fleet simulator.")
	return v, nil
}

func (s *Sim) addScenario(x scenarioState) {
	s.mu.Lock()
	s.scenarios = append(s.scenarios, x)
	s.mu.Unlock()
}

func (s *Sim) triggerIncident(ctx context.Context, v ScenarioView) (ScenarioView, error) {
	s.mu.Lock()
	var pick *simDriver
	for _, d := range s.order {
		if d.ride == nil || d.ride.status != RideOnTrip || d.route == nil || d.purpose != toDropoff || d.incident != nil {
			continue
		}
		if pick == nil || d.route.total-d.dist > pick.route.total-pick.dist {
			pick = d
		}
	}
	if pick == nil {
		s.mu.Unlock()
		return ScenarioView{}, ErrNoOnTripVehicle
	}
	d := pick
	h := d.heading * math.Pi / 180
	// Forward is (cos h, sin h) in (north, east); right of travel is (-sin h, cos h).
	target := offsetKm(d.pos, detourAheadKm*math.Cos(h)-detourSideKm*math.Sin(h), detourAheadKm*math.Sin(h)+detourSideKm*math.Cos(h))
	d.incident = &incident{phase: incDeviating, plan: d.route, topSpeed: d.speed}
	d.route, d.dist, d.purpose = newPath([]geo.Point{d.pos, target}), 0, 0
	v.DriverID, v.RideID, v.Location = d.id, d.ride.id, d.pos
	v.ActiveUntil = s.now().Add(time.Minute)
	v.Detail = fmt.Sprintf("%s swerves %.0f m off route and brakes hard", d.id, detourSideKm*1000)
	s.mu.Unlock()
	s.announce(ctx, "scenario safety_incident started: "+v.Detail, "Injected by the fleet simulator to exercise the Safety Sentinel.")
	return v, nil
}

// advanceIncident drives a vehicle through the safety incident. Callers hold s.mu.
func (s *Sim) advanceIncident(d *simDriver, now time.Time, dt float64) []effect {
	inc := d.incident
	switch inc.phase {
	case incDeviating:
		d.speed = math.Min(incidentSpeed, d.speed+15)
		inc.topSpeed = math.Max(inc.topSpeed, d.speed)
		d.dist = math.Min(d.dist+d.speed/3600*dt, d.route.total)
		d.pos, d.heading = d.route.at(d.dist)
		inc.maxOffKm = math.Max(inc.maxOffKm, distToPathKm(d.pos, inc.plan))
		if d.dist < d.route.total {
			return nil
		}
		// Hard brake: from top speed to standstill within this tick.
		top := d.speed
		d.speed = 0
		d.route, d.dist = nil, 0
		inc.phase = incStopped
		inc.stopUntil = now.Add(incidentStop)
		if inc.maxOffKm < deviationAlertKm {
			return nil
		}
		id, rideID, pos, off := d.id, d.ride.id, d.pos, inc.maxOffKm
		g := top / 3.6 / dt / 9.81
		return []effect{func(ctx context.Context) {
			s.svc.RecordAgentEvent(ctx, dispatch.AgentEventInput{
				Agent: "safety_sentinel", EventType: "route_deviation", RideID: rideID,
				Input:    map[string]any{"driver_id": id, "lat": pos.Lat, "lng": pos.Lng, "deviation_m": math.Round(off * 1000), "speed_before_kmh": top},
				Decision: fmt.Sprintf("flagged %s on ride %s: %.0f m off planned route, hard brake %.0f→0 km/h", id, rideID, off*1000, top),
				Reasoning: fmt.Sprintf("Geofence rule (deterministic fallback): vehicle left the planned path by more than %.0f m "+
					"and decelerated from %.0f km/h to a standstill within %.0f s (~%.1f g).", deviationAlertKm*1000, top, dt, g),
				Confidence: 0.9, UsedFallback: true,
			})
		}}
	case incStopped:
		d.speed = 0
		if now.Before(inc.stopUntil) {
			return nil
		}
		d.incident = nil
		if d.ride != nil {
			d.pending = s.planner.request(d.pos, d.ride.dropoff, true, now.Add(s.routeWait))
			d.pendFor = toDropoff
		}
	}
	return nil
}

// distToPathKm is the distance from p to the nearest segment of pa (local flat projection).
func distToPathKm(p geo.Point, pa *path) float64 {
	if pa == nil || len(pa.pts) == 0 {
		return 0
	}
	best := math.Inf(1)
	kx := 111.320 * math.Cos(p.Lat*math.Pi/180)
	const ky = 110.574
	for i := 1; i < len(pa.pts); i++ {
		a, b := pa.pts[i-1], pa.pts[i]
		ax, ay := (a.Lng-p.Lng)*kx, (a.Lat-p.Lat)*ky
		bx, by := (b.Lng-p.Lng)*kx, (b.Lat-p.Lat)*ky
		dx, dy := bx-ax, by-ay
		t := 0.0
		if l := dx*dx + dy*dy; l > 0 {
			t = math.Max(0, math.Min(1, -(ax*dx+ay*dy)/l))
		}
		best = math.Min(best, math.Hypot(ax+t*dx, ay+t*dy))
	}
	if len(pa.pts) == 1 {
		best = geo.HaversineKm(p, pa.pts[0])
	}
	return best
}
