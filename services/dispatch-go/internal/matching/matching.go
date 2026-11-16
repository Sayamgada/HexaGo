// Package matching implements the Dispatch Broker's deterministic scorer: it
// finds candidate drivers by expanding H3 k-rings around the pickup and ranks
// them by ETA, momentum, rating, idle time and (for AVs) battery sufficiency.
package matching

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Errors returned by Match.
var (
	ErrInvalidRequest = errors.New("matching: invalid request")
	ErrNoCandidates   = errors.New("matching: no eligible drivers nearby")
)

// Weights combine the normalised sub-scores into a total. They should sum to 1.
type Weights struct {
	ETA      float64 `json:"eta"`
	Momentum float64 `json:"momentum"`
	Rating   float64 `json:"rating"`
	Idle     float64 `json:"idle"`
	Battery  float64 `json:"battery"`
}

// Config tunes the matcher. The zero value is replaced by DefaultConfig.
type Config struct {
	MaxRings        int           // k-ring expansion limit
	MinCandidates   int           // stop expanding once this many eligible drivers are found
	MaxRanked       int           // ranked list length kept for claim retries
	CitySpeedFactor float64       // congestion multiplier on driving speed (0..1]
	BaseSpeedKmh    float64       // assumed speed for slow/stationary drivers
	RoadFactor      float64       // road distance ÷ straight-line distance
	MaxETA          time.Duration // ETA that scores 0
	IdleSaturation  time.Duration // idle time that scores 1
	MovingKmh       float64       // below this speed heading is ignored (momentum = 0.5)
	DefaultRating   float64       // used for unrated drivers
	AVPctPerKm      float64       // AV battery drain per road km
	AVReservePct    float64       // battery that must remain after the trip
	Weights         Weights
}

// DefaultConfig returns tuning suitable for a dense Indian metro.
func DefaultConfig() Config {
	return Config{
		MaxRings:        3,
		MinCandidates:   4, // best + 3 alternatives
		MaxRanked:       10,
		CitySpeedFactor: 0.6,
		BaseSpeedKmh:    30,
		RoadFactor:      1.3,
		MaxETA:          15 * time.Minute,
		IdleSaturation:  20 * time.Minute,
		MovingKmh:       3,
		DefaultRating:   4.5,
		AVPctPerKm:      0.25,
		AVReservePct:    15,
		Weights:         Weights{ETA: 0.45, Momentum: 0.15, Rating: 0.15, Idle: 0.15, Battery: 0.10},
	}
}

// Request is a ride to be matched.
type Request struct {
	Pickup  geo.Point
	Dropoff geo.Point
	Tier    fleet.Tier
}

func (r Request) validate() error {
	if err := r.Pickup.Validate(); err != nil {
		return fmt.Errorf("%w: pickup: %v", ErrInvalidRequest, err)
	}
	if err := r.Dropoff.Validate(); err != nil {
		return fmt.Errorf("%w: dropoff: %v", ErrInvalidRequest, err)
	}
	if _, err := fleet.ParseTier(string(r.Tier)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return nil
}

// Breakdown explains a candidate's score. Sub-scores are in [0, 1].
type Breakdown struct {
	DistanceKm float64 `json:"distance_km"` // estimated road distance to pickup
	ETASeconds float64 `json:"eta_seconds"`
	ETA        float64 `json:"eta"`
	Momentum   float64 `json:"momentum"`
	Rating     float64 `json:"rating"`
	Idle       float64 `json:"idle"`
	Battery    float64 `json:"battery"`
	Total      float64 `json:"total"`
}

// Candidate is a scored driver.
type Candidate struct {
	Driver fleet.Driver `json:"driver"`
	Ring   int          `json:"ring"` // k-ring distance from the pickup cell
	Score  Breakdown    `json:"score"`
}

// Result is the outcome of a match.
type Result struct {
	Best          Candidate
	Alternatives  []Candidate   // up to 3, best first
	Ranked        []Candidate   // all kept candidates, best first (for claim retries)
	Considered    int           // eligible drivers scored
	RingsSearched int           // rings expanded (1 = pickup cell only)
	Latency       time.Duration // time spent in Match
}

const maxAlternatives = 3

func newResult(ranked []Candidate) Result {
	return Result{Ranked: ranked}.WithBest(0)
}

// WithBest returns a copy of r whose Best is Ranked[i] and whose alternatives
// are the next best others. Used when the top driver was claimed concurrently.
func (r Result) WithBest(i int) Result {
	if i < 0 || i >= len(r.Ranked) {
		return r
	}
	out := r
	out.Best = r.Ranked[i]
	out.Alternatives = make([]Candidate, 0, maxAlternatives)
	for j, c := range r.Ranked {
		if j == i {
			continue
		}
		if len(out.Alternatives) == maxAlternatives {
			break
		}
		out.Alternatives = append(out.Alternatives, c)
	}
	return out
}

// Observer receives the latency of every match attempt.
type Observer interface {
	ObserveMatch(time.Duration)
}

// Matcher ranks drivers for ride requests.
type Matcher struct {
	fleet    *fleet.Index
	geo      geo.Indexer
	cfg      Config
	now      func() time.Time
	observer Observer
}

// Option configures a Matcher.
type Option func(*Matcher)

// WithObserver reports match latencies to o.
func WithObserver(o Observer) Option { return func(m *Matcher) { m.observer = o } }

// WithClock overrides time.Now (tests).
func WithClock(now func() time.Time) Option { return func(m *Matcher) { m.now = now } }

// New returns a Matcher over f. A zero cfg means DefaultConfig.
func New(f *fleet.Index, g geo.Indexer, cfg Config, opts ...Option) *Matcher {
	if cfg == (Config{}) {
		cfg = DefaultConfig()
	}
	m := &Matcher{fleet: f, geo: g, cfg: cfg, now: time.Now}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Config returns the matcher's tuning.
func (m *Matcher) Config() Config { return m.cfg }

// Match finds the best idle driver of the requested tier near the pickup,
// expanding up to MaxRings k-rings until MinCandidates are found.
func (m *Matcher) Match(req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	start := time.Now()
	res, err := m.match(req)
	res.Latency = time.Since(start)
	if m.observer != nil {
		m.observer.ObserveMatch(res.Latency)
	}
	return res, err
}

func (m *Matcher) match(req Request) (Result, error) {
	origin, err := m.geo.CellOf(req.Pickup)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	rings, err := m.geo.Rings(origin, m.cfg.MaxRings)
	if err != nil {
		return Result{}, err
	}
	now := m.now()
	var cands []Candidate
	searched := 0
	for k, ring := range rings {
		searched = k + 1
		m.fleet.VisitCells(ring, func(d fleet.Driver) {
			if b, ok := Score(m.cfg, d, req, now); ok {
				cands = append(cands, Candidate{Driver: d, Ring: k, Score: b})
			}
		})
		if len(cands) >= m.cfg.MinCandidates {
			break
		}
	}
	if len(cands) == 0 {
		return Result{RingsSearched: searched}, ErrNoCandidates
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Score.Total != cands[j].Score.Total {
			return cands[i].Score.Total > cands[j].Score.Total
		}
		return cands[i].Driver.ID < cands[j].Driver.ID
	})
	considered := len(cands)
	if len(cands) > m.cfg.MaxRanked {
		cands = cands[:m.cfg.MaxRanked]
	}
	res := newResult(cands)
	res.Considered = considered
	res.RingsSearched = searched
	return res, nil
}

// Score rates driver d for req at time now. It returns ok=false if the driver
// is ineligible (not idle, wrong tier, or an AV without enough charge).
func Score(cfg Config, d fleet.Driver, req Request, now time.Time) (Breakdown, bool) {
	if d.Status != fleet.StatusIdle || d.Tier != req.Tier {
		return Breakdown{}, false
	}
	var b Breakdown
	pos := d.Point()

	// ETA: road-adjusted haversine over congestion-adjusted speed.
	b.DistanceKm = geo.HaversineKm(pos, req.Pickup) * cfg.RoadFactor
	speed := math.Max(d.SpeedKmh, cfg.BaseSpeedKmh) * cfg.CitySpeedFactor
	b.ETASeconds = b.DistanceKm / speed * 3600
	b.ETA = clamp01(1 - b.ETASeconds/cfg.MaxETA.Seconds())

	// Momentum: is the driver already heading toward the pickup?
	b.Momentum = 0.5
	if d.SpeedKmh >= cfg.MovingKmh && b.DistanceKm > 0 {
		diff := geo.AngleDiffDeg(d.Heading, geo.BearingDeg(pos, req.Pickup))
		b.Momentum = (1 + math.Cos(diff*math.Pi/180)) / 2
	}

	rating := d.Rating
	if rating == 0 {
		rating = cfg.DefaultRating
	}
	b.Rating = clamp01((rating - 1) / 4)

	if !d.IdleSince.IsZero() {
		b.Idle = clamp01(now.Sub(d.IdleSince).Seconds() / cfg.IdleSaturation.Seconds())
	}

	b.Battery = 1
	if d.Tier == fleet.TierAV {
		tripKm := geo.HaversineKm(req.Pickup, req.Dropoff) * cfg.RoadFactor
		required := (b.DistanceKm+tripKm)*cfg.AVPctPerKm + cfg.AVReservePct
		if d.BatteryPct < required {
			return Breakdown{}, false
		}
		b.Battery = clamp01((d.BatteryPct - required) / (100 - required))
		if b.Battery == 0 { // exactly at the requirement: eligible but least preferred
			b.Battery = 1e-6
		}
	}

	w := cfg.Weights
	b.Total = w.ETA*b.ETA + w.Momentum*b.Momentum + w.Rating*b.Rating + w.Idle*b.Idle + w.Battery*b.Battery
	return b, true
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
