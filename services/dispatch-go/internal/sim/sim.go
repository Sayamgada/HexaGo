// Package sim is the demo fleet simulator: it spawns drivers around the city
// centre, moves them along OSRM road routes (straight lines when OSRM is slow
// or down), generates synthetic ride demand, drives matched rides through
// accepted → arriving → on_trip → completed, and injects what-if scenarios.
package sim

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// Limits and timings.
const (
	MaxDrivers       = 2000
	MaxDemandPerMin  = 600.0
	UnmatchedTimeout = 90 * time.Second // open sim requests are cancelled after this
	retryEvery       = 5 * time.Second  // open sim requests are re-dispatched this often
	wanderWait       = 30 * time.Second // idle drivers wait this long for a road route
	mirrorEvery      = 5 * time.Second
	logEvery         = time.Minute
	slowTick         = 200 * time.Millisecond
)

// Options configures a Sim.
type Options struct {
	Service      *dispatch.Service // required
	Publisher    stream.Publisher  // required
	Router       Router            // nil = StraightLine
	Drivers      int               // fleet size, default 300 (use Configure for 0)
	DemandPerMin float64           // mean requests per minute before time-of-day/scenarios
	Center       geo.Point
	Seed         uint64           // 0 = random
	Now          func() time.Time // nil = time.Now
	Tick         time.Duration    // default 1 s
	RouteWait    time.Duration    // ride legs wait this long for a road route; default 3 s
	Telemetry    *store.TelemetryBatcher
	Logger       *log.Logger
	Registry     prometheus.Registerer
	// RandomSpikesOff disables spontaneous demand spikes (tests).
	RandomSpikesOff bool
	// AutoDemand re-sizes the base demand every 30 s from the measured ride
	// time and match share so about DemoTargetBusy of the fleet is busy
	// (Little's law). DemandPerMin is only the starting point.
	AutoDemand bool
	// Safety receives every on_trip vehicle each tick (nil = no detection).
	Safety SafetyHook
	// Watched reports whether a rider client is watching ride:<id>; the
	// safety_incident scenario prefers such a ride (nil = none watched).
	Watched func(rideID string) bool
}

// SafetyHook is the incident detector the simulator feeds.
type SafetyHook interface {
	ObserveTick(ctx context.Context, obs []safety.Observation)
	RideEnded(rideID string)
}

type purpose int

const (
	toPickup purpose = iota + 1
	toDropoff
	toWander
)

type simRide struct {
	id      string
	rider   string
	status  RideStatus
	pickup  geo.Point
	dropoff geo.Point
	fresh   bool // accepted this tick: stays put so "accepted" is visible for a full tick
}

type incidentPhase int

const (
	incDeviating incidentPhase = iota + 1
	incStopped
)

type incident struct {
	phase     incidentPhase
	plan      *path // the planned route at the moment of the incident
	maxOffKm  float64
	topSpeed  float64
	stopUntil time.Time
}

type simDriver struct {
	n       int
	id      string
	tier    fleet.Tier
	rating  float64
	battery float64
	cruise  float64 // preferred speed, km/h

	pos     geo.Point
	heading float64
	speed   float64

	route   *path
	dist    float64 // km travelled along route
	purpose purpose
	pending *ticket
	pendFor purpose
	dwell   time.Time

	ride     *simRide
	incident *incident

	offlineUntil time.Time // logged off (rain storm) until then
	offline      bool      // currently offline; refreshed every Step
	suspended    bool      // frozen by the Safety Sentinel until an Ops Supervisor-confirmed release
}

func (d *simDriver) busy() bool { return d.ride != nil }

func (d *simDriver) wireStatus() string {
	switch {
	case d.suspended:
		return "suspended"
	case d.incident != nil:
		return "incident"
	case d.ride != nil:
		return string(d.ride.status.DriverStatus())
	case d.offline:
		return string(fleet.StatusOffline)
	}
	return string(fleet.StatusIdle)
}

type scenarioState struct {
	name        string
	until       time.Time
	demandMult  float64
	speedFactor float64
}

type openRequest struct {
	id      string
	created time.Time
	tried   time.Time // last dispatch attempt
}

// Sim is the fleet simulator.
type Sim struct {
	svc       *dispatch.Service
	pub       stream.Publisher
	planner   *planner
	router    Router
	tel       *store.TelemetryBatcher
	log       *log.Logger
	now       func() time.Time
	tick      time.Duration
	routeWait time.Duration
	center    geo.Point
	hotspots  []hotspot
	spikesOn  bool
	tickHist  prometheus.Histogram
	safety    SafetyHook
	watched   func(string) bool

	mu        sync.Mutex
	rng       *rand.Rand
	drivers   map[string]*simDriver
	order     []*simDriver // by number, for deterministic iteration
	nextN     int
	target    int
	base      float64 // demand per minute
	scenarios []scenarioState
	spikes    []spike
	open      []openRequest
	auto      autoDemand
	warmUntil time.Time // cold-start demand ramp (warmup.go)
	running   bool
	stats     tickStats

	inMu  sync.Mutex
	inbox []store.Ride
}

type tickStats struct {
	n        int
	total    time.Duration
	max      time.Duration
	lastLog  time.Time
	requests int
}

// ErrInvalidConfig is returned by Configure for out-of-range values.
var ErrInvalidConfig = errors.New("sim: invalid config")

// New builds a simulator. Call Run (or Spawn + Step for tests).
func New(o Options) (*Sim, error) {
	if o.Service == nil || o.Publisher == nil {
		return nil, errors.New("sim: Service and Publisher are required")
	}
	if o.Router == nil {
		o.Router = StraightLine{}
	}
	if o.Drivers <= 0 {
		o.Drivers = 300
	}
	if o.Drivers > MaxDrivers {
		o.Drivers = MaxDrivers
	}
	if o.DemandPerMin < 0 {
		o.DemandPerMin = 0
	}
	if o.Center == (geo.Point{}) {
		o.Center = defaultCenter
	}
	if o.Seed == 0 {
		o.Seed = rand.Uint64()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Tick <= 0 {
		o.Tick = time.Second
	}
	if o.RouteWait <= 0 {
		o.RouteWait = 3 * time.Second
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	s := &Sim{
		svc: o.Service, pub: o.Publisher, router: o.Router, planner: newPlanner(o.Router), tel: o.Telemetry,
		log: o.Logger, now: func() time.Time { return o.Now().UTC() }, tick: o.Tick, routeWait: o.RouteWait, center: o.Center,
		spikesOn: !o.RandomSpikesOff,
		rng:      rand.New(rand.NewPCG(o.Seed, o.Seed^0x9E3779B97F4A7C15)),
		drivers:  map[string]*simDriver{}, target: o.Drivers, base: o.DemandPerMin,
		auto: newAutoDemand(o.AutoDemand), safety: o.Safety, watched: o.Watched,
	}
	for _, h := range baseHotspots {
		s.hotspots = append(s.hotspots, hotspot{h.name, shift(h.at, o.Center), h.weight})
	}
	s.tickHist = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sim_tick_duration_seconds",
		Help:    "Fleet simulator tick duration.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	})
	if o.Registry != nil {
		if err := o.Registry.Register(s.tickHist); err != nil {
			s.log.Printf("WARN sim: register tick histogram: %v", err)
		}
	}
	o.Service.SetRideObserver(s.observe)
	return s, nil
}

// DriverID formats driver n as d_001.
func DriverID(n int) string { return fmt.Sprintf("d_%03d", n) }

// observe receives matched/cancelled rides from the dispatcher. It must not
// block or take s.mu: CreateRide may be running inside a demand tick.
func (s *Sim) observe(r store.Ride) {
	s.inMu.Lock()
	s.inbox = append(s.inbox, r)
	s.inMu.Unlock()
}

// Run spawns the fleet and runs the movement, demand and housekeeping loops
// until ctx is cancelled.
func (s *Sim) Run(ctx context.Context) {
	s.mu.Lock()
	s.running = true
	s.stats.lastLog = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	if n, err := s.svc.CloseStaleRides(ctx, s.now()); err != nil {
		s.log.Printf("WARN sim: close stale rides: %v", err)
	} else if n > 0 {
		s.log.Printf("sim: cancelled %d rides left over from a previous run", n)
	}
	s.planner.start(ctx)
	s.Spawn(ctx)
	s.StartWarmup(s.now())
	s.log.Printf("sim: running with %d drivers, %.1f requests/min base demand, tick %v", s.target, s.base, s.tick)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.every(ctx, s.tick, s.DemandTick) }()
	go func() {
		defer wg.Done()
		s.every(ctx, mirrorEvery, func(context.Context) { s.svc.Fleet().SyncMirror() })
	}()

	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			start := time.Now()
			s.Step(ctx)
			s.recordTick(time.Since(start))
		}
	}
}

func (s *Sim) every(ctx context.Context, d time.Duration, fn func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

func (s *Sim) recordTick(d time.Duration) {
	s.tickHist.Observe(d.Seconds())
	if d > slowTick {
		s.log.Printf("WARN sim: slow tick %v", d)
	}
	s.mu.Lock()
	st := &s.stats
	st.n++
	st.total += d
	st.max = max(st.max, d)
	if time.Since(st.lastLog) < logEvery {
		s.mu.Unlock()
		return
	}
	n, avg, mx, reqs := st.n, st.total/time.Duration(st.n), st.max, st.requests
	*st = tickStats{lastLog: time.Now()}
	active := 0
	for _, d := range s.order {
		if d.busy() {
			active++
		}
	}
	drivers := len(s.order)
	s.mu.Unlock()

	c := s.svc.Fleet().Counts()
	msg := fmt.Sprintf("sim: %d ticks avg %v max %v; drivers %d (idle %d, en_route %d, on_trip %d); active rides %d; %d requests",
		n, avg.Round(time.Microsecond), mx.Round(time.Microsecond), drivers, c.Idle, c.EnRoute, c.OnTrip, active, reqs)
	if o, ok := s.router.(*OSRM); ok {
		os := o.Stats()
		msg += fmt.Sprintf("; osrm %d req, cache %d hit / %d miss, %d err", os.Requests, os.Hits, os.Misses, os.Errors)
	}
	s.log.Print(msg)
}

// Spawn tops the fleet up to the configured size.
func (s *Sim) Spawn(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcileFleet()
}

// reconcileFleet adds or removes (idle) drivers to reach s.target. Callers hold s.mu.
func (s *Sim) reconcileFleet() {
	for len(s.order) < s.target {
		s.nextN++
		n := s.nextN
		tier := s.pickHumanTier()
		if n*15/100 != (n-1)*15/100 { // exactly 15% of driver numbers are AVs
			tier = fleet.TierAV
		}
		pos := s.jitter(s.center, 4.5)
		if s.rng.Float64() < 0.75 {
			pos = s.jitter(s.pickHotspot().at, 1.2)
		}
		d := &simDriver{
			n: n, id: DriverID(n), tier: tier, pos: pos,
			rating: 4.2 + s.rng.Float64()*0.8, cruise: 24 + s.rng.Float64()*12,
			heading: s.rng.Float64() * 360,
			dwell:   s.now().Add(time.Duration(s.rng.IntN(20)) * time.Second),
		}
		if tier == fleet.TierAuto {
			d.cruise = 20 + s.rng.Float64()*8
		}
		if tier == fleet.TierAV {
			d.battery = 40 + s.rng.Float64()*60
		}
		if _, err := s.svc.Fleet().ApplyLocal(d.id, func(fd *fleet.Driver, _ bool) error {
			*fd = fleet.Driver{ID: d.id, Lat: pos.Lat, Lng: pos.Lng, Heading: d.heading, Status: fleet.StatusIdle,
				Tier: tier, BatteryPct: d.battery, Rating: d.rating}
			return nil
		}); err != nil {
			s.log.Printf("WARN sim: spawn %s: %v", d.id, err)
			continue
		}
		s.drivers[d.id] = d
		s.order = append(s.order, d)
	}
	for i := len(s.order) - 1; i >= 0 && len(s.order) > s.target; i-- {
		d := s.order[i]
		if d.busy() {
			continue
		}
		if d.pending != nil {
			d.pending.abandoned.Store(true)
		}
		s.svc.Fleet().Remove(d.id)
		delete(s.drivers, d.id)
		s.order = append(s.order[:i], s.order[i+1:]...)
	}
}

// ActiveRides returns the number of rides the simulator is driving.
func (s *Sim) ActiveRides() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.order {
		if d.busy() {
			n++
		}
	}
	return n
}

// effect is a side effect (DB write, agent event) run after s.mu is released.
type effect func(ctx context.Context)

// Step advances the simulation by one tick and publishes the fleet.
func (s *Sim) Step(ctx context.Context) {
	now := s.now()
	dt := s.tick.Seconds()

	s.inMu.Lock()
	inbox := s.inbox
	s.inbox = nil
	s.inMu.Unlock()

	s.mu.Lock()
	var effects []effect
	var rideMsgs []stream.RideUpdate
	s.expire(now)
	speedFactor := s.speedFactorLocked()
	for _, r := range inbox {
		if u, ok := s.intake(r, now); ok {
			rideMsgs = append(rideMsgs, u)
		}
	}
	s.reconcileFleet()

	busy := 0
	drivers := make([]stream.Driver, 0, len(s.order))
	var obs []safety.Observation
	var ended []string
	for _, d := range s.order {
		wantOffline := d.suspended || (d.ride == nil && now.Before(d.offlineUntil))
		switch {
		case d.suspended:
			s.clearRoute(d)
			d.speed = 0
		case d.ride != nil && d.ride.fresh:
			d.ride.fresh = false
		case wantOffline:
			s.clearRoute(d)
			d.speed = 0
		default:
			effects = append(effects, s.advance(d, now, dt, speedFactor)...)
		}
		d.offline = false
		fd, err := s.svc.Fleet().ApplyLocal(d.id, func(fd *fleet.Driver, exists bool) error {
			if !exists {
				*fd = fleet.Driver{ID: d.id, Status: fleet.StatusIdle, Tier: d.tier, Rating: d.rating}
			}
			// Only an idle driver logs off; one the dispatcher just claimed
			// keeps its ride and goes offline after it (if the storm lasts).
			switch {
			case wantOffline && fd.Status == fleet.StatusIdle:
				fd.Status = fleet.StatusOffline
			case !wantOffline && fd.Status == fleet.StatusOffline:
				fd.Status = fleet.StatusIdle
			}
			d.offline = fd.Status == fleet.StatusOffline
			fd.Lat, fd.Lng, fd.Heading, fd.SpeedKmh = d.pos.Lat, d.pos.Lng, d.heading, d.speed
			if d.tier == fleet.TierAV {
				fd.BatteryPct = d.battery
			}
			return nil
		})
		if err != nil {
			continue
		}
		wd := s.wireDriver(d)
		drivers = append(drivers, wd)
		if s.tel != nil {
			s.tel.Add(store.TelemetryPoint{
				DriverID: d.id, Lat: d.pos.Lat, Lng: d.pos.Lng, Heading: d.heading, SpeedKmh: d.speed,
				Status: wd.Status, H3Cell: fd.Cell.String(), RecordedAt: now.UTC(),
			})
		}
		if d.ride != nil {
			busy++
			rideMsgs = append(rideMsgs, s.rideUpdate(d, wd))
			if s.safety != nil && d.ride.status == RideOnTrip {
				obs = append(obs, s.observation(d, now))
			}
			if d.ride.status.Terminal() {
				ended = append(ended, d.ride.id)
				s.rideFinished()
				d.ride = nil
			}
		}
	}
	s.accrue(busy, s.tick)
	s.mu.Unlock()

	for _, e := range effects {
		e(ctx)
	}
	if s.safety != nil {
		for _, id := range ended {
			s.safety.RideEnded(id)
		}
		if len(obs) > 0 {
			s.safety.ObserveTick(ctx, obs)
		}
	}
	for _, u := range rideMsgs {
		s.pub.Publish(stream.RideTopic(u.RideID), u)
	}
	s.pub.Publish(stream.TopicDrivers, drivers)
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func (s *Sim) wireDriver(d *simDriver) stream.Driver {
	w := stream.Driver{
		ID: d.id, Lat: round(d.pos.Lat, 6), Lng: round(d.pos.Lng, 6),
		Heading: round(math.Mod(d.heading+360, 360), 1), SpeedKmh: round(d.speed, 1),
		Status: d.wireStatus(), Tier: string(d.tier),
	}
	if d.tier == fleet.TierAV {
		b := round(d.battery, 1)
		w.Battery = &b
	}
	return w
}

func (s *Sim) rideUpdate(d *simDriver, wd stream.Driver) stream.RideUpdate {
	return stream.RideUpdate{RideID: d.ride.id, Status: string(d.ride.status), Driver: wd, ETASec: s.etaSec(d)}
}

// etaSec estimates seconds to the current ride milestone (pickup or dropoff).
func (s *Sim) etaSec(d *simDriver) int {
	r := d.ride
	if r == nil || r.status.Terminal() {
		return 0
	}
	var km float64
	switch {
	case d.route != nil && d.purpose == toPickup && r.status == RideArriving,
		d.route != nil && d.purpose == toDropoff && r.status == RideOnTrip:
		km = d.route.total - d.dist
	case r.status == RideOnTrip:
		km = geo.HaversineKm(d.pos, r.dropoff) * 1.3
	default:
		km = geo.HaversineKm(d.pos, r.pickup) * 1.3
	}
	v := math.Max(d.speed, d.cruise*0.8)
	return int(math.Round(km / v * 3600))
}

// intake handles a ride event from the dispatcher. Callers hold s.mu.
func (s *Sim) intake(r store.Ride, now time.Time) (stream.RideUpdate, bool) {
	d, ok := s.drivers[r.DriverID]
	if r.Status == store.RideCancelled {
		if ok && d.ride != nil && d.ride.id == r.ID {
			s.dropRide(d, now)
		}
		return stream.RideUpdate{}, false
	}
	if !ok || r.Status != store.RideMatched || d.busy() {
		return stream.RideUpdate{}, false
	}
	d.ride = &simRide{id: r.ID, rider: r.RiderID, status: RideAccepted, pickup: r.Pickup, dropoff: r.Dropoff, fresh: true}
	s.rideStarted()
	s.clearRoute(d)
	d.dwell = time.Time{}
	d.pending = s.planner.request(d.pos, r.Pickup, true, now.Add(s.routeWait))
	d.pendFor = toPickup
	return s.rideUpdate(d, s.wireDriver(d)), true
}

func (s *Sim) clearRoute(d *simDriver) {
	if d.pending != nil {
		d.pending.abandoned.Store(true)
	}
	d.pending, d.route, d.dist, d.purpose = nil, nil, 0, 0
}

func (s *Sim) dropRide(d *simDriver, now time.Time) {
	if d.ride != nil {
		s.rideFinished()
	}
	d.ride, d.incident = nil, nil
	s.clearRoute(d)
	d.speed = 0
	d.dwell = now.Add(time.Duration(5+s.rng.IntN(15)) * time.Second)
}

// advance moves one driver by dt seconds. Callers hold s.mu.
func (s *Sim) advance(d *simDriver, now time.Time, dt, speedFactor float64) []effect {
	if d.incident != nil {
		return s.advanceIncident(d, now, dt)
	}
	var effects []effect

	// Resolve a pending route request.
	if t := d.pending; t != nil {
		pts, err, done := t.result()
		expired := !now.Before(t.expires)
		switch {
		case done && err == nil && len(pts) >= 2:
			s.setRoute(d, pts, d.pendFor)
		case d.pendFor == toWander && !done && !expired:
			// keep waiting: idle drivers would rather park than cut through buildings
		case d.pendFor == toWander && (expired || errors.Is(err, errPlannerBusy) || errors.Is(err, context.Canceled)):
			t.abandoned.Store(true)
			d.pending = nil
			d.dwell = now.Add(time.Duration(10+s.rng.IntN(20)) * time.Second)
		case done || expired:
			t.abandoned.Store(true)
			s.setRoute(d, []geo.Point{d.pos, t.to}, d.pendFor) // straight-line fallback
		}
		if d.pending == t {
			d.speed = 0 // still waiting for the route
			return nil
		}
		if d.route != nil && d.purpose == toPickup && d.ride != nil && d.ride.status == RideAccepted {
			d.ride.status, _ = Transition(d.ride.status, EvRouteReady)
		}
	}

	if d.route == nil {
		d.speed = 0
		if d.ride == nil {
			s.maybeWander(d, now)
			s.charge(d, dt)
		}
		return nil
	}

	// Drive along the route.
	target := d.cruise * speedFactor
	remaining := d.route.total - d.dist
	if brake := math.Sqrt(math.Max(remaining, 0)*1000*2*2.5) * 3.6; brake < target { // decelerate ~2.5 m/s² into the stop
		target = math.Max(brake, 5)
	}
	d.speed = math.Max(0, d.speed+math.Max(-15, math.Min(8, target-d.speed)))
	step := d.speed / 3600 * dt
	prev := d.pos
	d.dist = math.Min(d.dist+step, d.route.total)
	d.pos, d.heading = d.route.at(d.dist)
	if d.tier == fleet.TierAV {
		d.battery = math.Max(0, d.battery-geo.HaversineKm(prev, d.pos)*0.25)
	}
	if d.dist < d.route.total {
		return nil
	}

	// Arrived.
	d.speed = 0
	arrivedFor := d.purpose
	d.route, d.dist, d.purpose = nil, 0, 0
	switch {
	case arrivedFor == toPickup && d.ride != nil && d.ride.status == RideArriving:
		d.ride.status, _ = Transition(d.ride.status, EvReachedPickup)
		id := d.ride.id
		effects = append(effects, s.advanceRideEffect(d.id, id, store.RideInProgress))
		d.pending = s.planner.request(d.pos, d.ride.dropoff, true, now.Add(s.routeWait))
		d.pendFor = toDropoff
	case arrivedFor == toDropoff && d.ride != nil && d.ride.status == RideOnTrip:
		d.ride.status, _ = Transition(d.ride.status, EvReachedDropoff)
		effects = append(effects, s.advanceRideEffect(d.id, d.ride.id, store.RideCompleted))
		d.dwell = now.Add(time.Duration(5+s.rng.IntN(20)) * time.Second)
	default: // wander target reached: park for a while
		d.dwell = now.Add(time.Duration(10+s.rng.IntN(80)) * time.Second)
	}
	return effects
}

func (s *Sim) setRoute(d *simDriver, pts []geo.Point, p purpose) {
	d.pending = nil
	d.route, d.dist, d.purpose = newPath(pts), 0, p
}

// advanceRideEffect persists a ride transition; if the dispatcher refuses
// (e.g. the rider cancelled meanwhile) the simulator lets the ride go.
func (s *Sim) advanceRideEffect(driverID, rideID string, to store.RideStatus) effect {
	return func(ctx context.Context) {
		if _, err := s.svc.AdvanceRide(ctx, rideID, to); err != nil {
			s.log.Printf("WARN sim: advance ride %s to %s: %v", rideID, to, err)
			s.mu.Lock()
			if d, ok := s.drivers[driverID]; ok && d.ride != nil && d.ride.id == rideID {
				s.dropRide(d, s.now())
			}
			s.mu.Unlock()
			for _, from := range []fleet.Status{fleet.StatusEnRoute, fleet.StatusOnTrip} {
				_, _ = s.svc.Fleet().Claim(driverID, from, fleet.StatusIdle)
			}
		}
	}
}

// maybeWander sends a parked idle driver toward a demand hotspot.
func (s *Sim) maybeWander(d *simDriver, now time.Time) {
	if d.pending != nil || now.Before(d.dwell) {
		return
	}
	if d.tier == fleet.TierAV && d.battery < 25 {
		d.dwell = now.Add(30 * time.Second) // recharge before roaming
		return
	}
	if s.rng.Float64() < 0.3 {
		d.dwell = now.Add(time.Duration(20+s.rng.IntN(60)) * time.Second)
		return
	}
	// Bias toward hotspots and active spikes; keep trips short (≤ ~4 km).
	var target geo.Point
	if len(s.spikes) > 0 && s.rng.Float64() < 0.5 {
		target = s.jitter(s.spikes[s.rng.IntN(len(s.spikes))].at, 0.5)
	} else {
		target = s.jitter(s.pickHotspotNear(d.pos).at, 0.8)
	}
	if km := geo.HaversineKm(d.pos, target); km > 4 {
		f := 4 / km
		target = geo.Point{Lat: d.pos.Lat + (target.Lat-d.pos.Lat)*f, Lng: d.pos.Lng + (target.Lng-d.pos.Lng)*f}
	}
	// Snap to the route cache grid so different drivers share cached routes.
	target = geo.Point{Lat: math.Round(target.Lat/0.002) * 0.002, Lng: math.Round(target.Lng/0.002) * 0.002}
	d.pending = s.planner.request(d.pos, target, false, now.Add(wanderWait))
	d.pendFor = toWander
}

// charge tops up a parked AV.
func (s *Sim) charge(d *simDriver, dt float64) {
	if d.tier == fleet.TierAV && d.battery < 95 {
		d.battery = math.Min(100, d.battery+0.3*dt)
	}
}

// expire drops finished scenarios and spikes. Callers hold s.mu.
func (s *Sim) expire(now time.Time) {
	sc := s.scenarios[:0]
	for _, x := range s.scenarios {
		if now.Before(x.until) {
			sc = append(sc, x)
		}
	}
	s.scenarios = sc
	sp := s.spikes[:0]
	for _, x := range s.spikes {
		if now.Before(x.until) {
			sp = append(sp, x)
		}
	}
	s.spikes = sp
}

func (s *Sim) speedFactorLocked() float64 {
	f := 1.0
	for _, x := range s.scenarios {
		f *= x.speedFactor
	}
	return round(math.Max(f, 0.3), 3)
}

func (s *Sim) demandRateLocked(now time.Time) float64 {
	mult := 1.0
	for _, x := range s.scenarios {
		if now.Before(x.until) {
			mult *= x.demandMult
		}
	}
	r := s.base * s.curve(now) * mult
	for _, sp := range s.spikes {
		if now.Before(sp.until) {
			r += sp.perMin
		}
	}
	return r
}

// DemandRate is the current expected requests per minute.
func (s *Sim) DemandRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demandRateLocked(s.now())
}

// SpeedFactor is the current traffic speed multiplier (1 = normal).
func (s *Sim) SpeedFactor() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(s.now())
	return s.speedFactorLocked()
}

// SetDemandPerMin sets the base demand.
func (s *Sim) SetDemandPerMin(v float64) {
	s.mu.Lock()
	s.base = math.Max(0, v)
	s.auto.on = false
	s.mu.Unlock()
}

type rideRequest struct {
	rider   string
	pickup  geo.Point
	dropoff geo.Point
	tier    fleet.Tier
}

// DemandTick generates this tick's synthetic requests and cancels sim
// requests that stayed unmatched for UnmatchedTimeout.
func (s *Sim) DemandTick(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	s.expire(now)
	s.retune(now)
	if s.spikesOn && s.rng.Float64() < s.tick.Seconds()/600 {
		h := s.pickHotspot()
		dur := time.Duration(120+s.rng.IntN(180)) * time.Second
		s.spikes = append(s.spikes, spike{name: "random spike near " + h.name, at: h.at, perMin: math.Max(s.base, 2), until: now.Add(dur)})
		s.mu.Unlock()
		s.announce(ctx, fmt.Sprintf("demand spike near %s for %v", h.name, dur), "Synthetic demand surge injected by the fleet simulator.")
		s.mu.Lock()
	}
	rate := s.demandRateLocked(now)
	n := s.poisson(rate * s.tick.Seconds() / 60)
	reqs := make([]rideRequest, 0, n)
	baseShare := rate
	for _, sp := range s.spikes {
		baseShare -= sp.perMin
	}
	for range n {
		p := s.randomPickup(math.Max(baseShare, 0))
		reqs = append(reqs, rideRequest{
			rider: fmt.Sprintf("r_sim_%04d", s.rng.IntN(5000)), pickup: p, dropoff: s.randomDropoff(p), tier: s.pickTier(),
		})
	}
	warm := s.warmupRequests(now)
	reqs = append(reqs, warm...)
	n += len(warm)
	var expired, retry []string
	kept := s.open[:0]
	for _, o := range s.open {
		switch {
		case now.Sub(o.created) >= UnmatchedTimeout:
			expired = append(expired, o.id)
			continue
		case now.Sub(o.tried) >= retryEvery:
			retry = append(retry, o.id)
			o.tried = now
		}
		kept = append(kept, o)
	}
	s.open = kept
	s.stats.requests += n
	s.mu.Unlock()

	s.retryOpen(ctx, retry)
	for _, id := range expired {
		if _, err := s.svc.CancelRide(ctx, id, "no_driver_timeout"); err != nil && !errors.Is(err, dispatch.ErrConflict) {
			s.log.Printf("WARN sim: expire request %s: %v", id, err)
		}
	}
	s.createRides(ctx, reqs, now)
}

func (s *Sim) createRides(ctx context.Context, reqs []rideRequest, now time.Time) {
	for _, r := range reqs {
		pickup, dropoff := r.pickup, r.dropoff
		view, err := s.svc.CreateRide(ctx, dispatch.CreateRideInput{RiderID: r.rider, Pickup: &pickup, Dropoff: &dropoff, Tier: string(r.tier)})
		if err != nil {
			s.log.Printf("WARN sim: create ride: %v", err)
			continue
		}
		s.mu.Lock()
		s.countRequest(true, view.Match != nil)
		s.mu.Unlock()
		if view.Match == nil {
			s.mu.Lock()
			s.open = append(s.open, openRequest{id: view.Ride.ID, created: now, tried: now})
			s.mu.Unlock()
		}
	}
}

// announce streams a simulator notice on ops (and records it in agent_events).
func (s *Sim) announce(ctx context.Context, decision, reasoning string) {
	s.svc.RecordAgentEvent(ctx, dispatch.AgentEventInput{
		Agent: "fleet_simulator", EventType: "scenario", Kind: "scenario", Decision: decision, Reasoning: reasoning, Confidence: 1,
	})
}

// ---- configuration ----

// ConfigInput is the POST /v1/sim/config body; omitted fields are unchanged.
type ConfigInput struct {
	Drivers      *int     `json:"drivers"`
	DemandPerMin *float64 `json:"demand_per_min"`
	SpikeLat     *float64 `json:"spike_lat"`
	SpikeLng     *float64 `json:"spike_lng"`
}

// ScenarioInfo describes an active scenario.
type ScenarioInfo struct {
	Name        string    `json:"name"`
	ActiveUntil time.Time `json:"active_until"`
}

// SpikeInfo describes an active demand spike.
type SpikeInfo struct {
	Name        string    `json:"name"`
	Lat         float64   `json:"lat"`
	Lng         float64   `json:"lng"`
	PerMin      float64   `json:"extra_per_min"`
	ActiveUntil time.Time `json:"active_until"`
}

// ConfigView is the simulator state returned by the config endpoints.
type ConfigView struct {
	Running        bool           `json:"running"`
	Drivers        int            `json:"drivers"`
	LiveDrivers    int            `json:"live_drivers"`
	ActiveRides    int            `json:"active_rides"`
	DemandPerMin   float64        `json:"demand_per_min"`
	CurrentPerMin  float64        `json:"current_demand_per_min"`
	TimeOfDay      float64        `json:"time_of_day_factor"`
	AutoDemand     bool           `json:"auto_demand"`
	AvgRideMinutes float64        `json:"avg_ride_minutes"`
	MatchShare     float64        `json:"match_share"`
	SpeedFactor    float64        `json:"speed_factor"`
	Scenarios      []ScenarioInfo `json:"scenarios"`
	Spikes         []SpikeInfo    `json:"spikes"`
	Router         string         `json:"router"`
	OSRM           *OSRMStats     `json:"osrm,omitempty"`
	TelemetryStats any            `json:"telemetry,omitempty"`
}

// Status returns the current simulator state.
func (s *Sim) Status() ConfigView {
	now := s.now()
	s.mu.Lock()
	s.expire(now)
	v := ConfigView{
		Running: s.running, Drivers: s.target, LiveDrivers: len(s.order), DemandPerMin: s.base,
		CurrentPerMin: round(s.demandRateLocked(now), 2), TimeOfDay: round(s.curve(now), 3),
		SpeedFactor: s.speedFactorLocked(), Scenarios: []ScenarioInfo{}, Spikes: []SpikeInfo{}, Router: "straight_line",
		AutoDemand: s.auto.on, AvgRideMinutes: round(s.auto.rideMinutes(), 2), MatchShare: round(s.auto.matchShare, 3),
	}
	for _, d := range s.order {
		if d.busy() {
			v.ActiveRides++
		}
	}
	for _, x := range s.scenarios {
		v.Scenarios = append(v.Scenarios, ScenarioInfo{Name: x.name, ActiveUntil: x.until})
	}
	for _, x := range s.spikes {
		v.Spikes = append(v.Spikes, SpikeInfo{Name: x.name, Lat: x.at.Lat, Lng: x.at.Lng, PerMin: x.perMin, ActiveUntil: x.until})
	}
	s.mu.Unlock()
	if o, ok := s.router.(*OSRM); ok {
		st := o.Stats()
		v.OSRM, v.Router = &st, "osrm"
	}
	if s.tel != nil {
		v.TelemetryStats = s.tel.Stats()
	}
	sort.Slice(v.Scenarios, func(i, j int) bool { return v.Scenarios[i].Name < v.Scenarios[j].Name })
	return v
}

// Configure applies a config change. The fleet is resized on the next tick
// (busy drivers are removed once they finish their ride).
func (s *Sim) Configure(ctx context.Context, in ConfigInput) (ConfigView, error) {
	if in.Drivers != nil && (*in.Drivers < 0 || *in.Drivers > MaxDrivers) {
		return ConfigView{}, fmt.Errorf("%w: drivers must be 0..%d", ErrInvalidConfig, MaxDrivers)
	}
	if in.DemandPerMin != nil && (*in.DemandPerMin < 0 || *in.DemandPerMin > MaxDemandPerMin || math.IsNaN(*in.DemandPerMin)) {
		return ConfigView{}, fmt.Errorf("%w: demand_per_min must be 0..%g", ErrInvalidConfig, MaxDemandPerMin)
	}
	if (in.SpikeLat == nil) != (in.SpikeLng == nil) {
		return ConfigView{}, fmt.Errorf("%w: spike_lat and spike_lng must be sent together", ErrInvalidConfig)
	}
	var spikeAt geo.Point
	if in.SpikeLat != nil {
		spikeAt = geo.Point{Lat: *in.SpikeLat, Lng: *in.SpikeLng}
		if err := spikeAt.Validate(); err != nil {
			return ConfigView{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}
	}
	now := s.now()
	s.mu.Lock()
	if in.Drivers != nil {
		s.target = *in.Drivers
	}
	if in.DemandPerMin != nil {
		s.base = *in.DemandPerMin
		s.auto.on = false // an explicit demand overrides auto-sizing
	}
	var spikeRate float64
	if in.SpikeLat != nil {
		spikeRate = math.Max(s.base, 2) * 1.5
		s.spikes = append(s.spikes, spike{name: "manual spike", at: spikeAt, perMin: spikeRate, until: now.Add(10 * time.Minute)})
	}
	s.mu.Unlock()
	if in.SpikeLat != nil {
		s.announce(ctx, fmt.Sprintf("demand spike at %.4f, %.4f (+%.1f requests/min for 10m)", spikeAt.Lat, spikeAt.Lng, spikeRate),
			"Operator-requested demand spike via POST /v1/sim/config.")
	}
	return s.Status(), nil
}

// retryOpen re-dispatches open requests now that drivers may have freed up;
// matched (or no longer open) requests leave the open list.
func (s *Sim) retryOpen(ctx context.Context, ids []string) {
	done := map[string]bool{}
	for _, id := range ids {
		view, err := s.svc.RetryRide(ctx, id)
		switch {
		case err == nil && view.Match != nil, errors.Is(err, dispatch.ErrConflict), errors.Is(err, dispatch.ErrNotFound):
			done[id] = true
			if err == nil {
				s.mu.Lock()
				s.countRequest(false, true)
				s.mu.Unlock()
			}
		case err != nil:
			s.log.Printf("WARN sim: retry request %s: %v", id, err)
		}
	}
	if len(done) == 0 {
		return
	}
	s.mu.Lock()
	kept := s.open[:0]
	for _, o := range s.open {
		if !done[o.id] {
			kept = append(kept, o)
		}
	}
	s.open = kept
	s.mu.Unlock()
}
