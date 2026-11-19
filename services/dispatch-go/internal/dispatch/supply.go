package dispatch

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Recent ride requests kept for the surge map (agents-py Surge Equilibrium).
const (
	demandMemory     = 15 * time.Minute
	demandMax        = 20000
	DefaultDemandWin = 5 * time.Minute
)

type demandPoint struct {
	p       geo.Point
	at      time.Time
	matched bool
}

type demandLog struct {
	mu  sync.Mutex
	pts []demandPoint // oldest first
}

func (l *demandLog) add(d demandPoint) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pts = append(l.pts, d)
	cut := 0
	for cut < len(l.pts) && (d.at.Sub(l.pts[cut].at) > demandMemory || len(l.pts)-cut > demandMax) {
		cut++
	}
	if cut > 0 {
		l.pts = append(l.pts[:0], l.pts[cut:]...)
	}
}

func (l *demandLog) since(t time.Time) []demandPoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []demandPoint
	for _, d := range l.pts {
		if !d.at.Before(t) {
			out = append(out, d)
		}
	}
	return out
}

// SupplyDriver is one vehicle in a supply snapshot.
type SupplyDriver struct {
	Lat    float64 `json:"lat"`
	Lng    float64 `json:"lng"`
	Status string  `json:"status"`
}

// SupplyRequest is one recent ride request.
type SupplyRequest struct {
	Lat     float64   `json:"lat"`
	Lng     float64   `json:"lng"`
	At      time.Time `json:"at"`
	Matched bool      `json:"matched"`
}

// SupplySnapshot is the GET /v1/supply/snapshot body: every vehicle's position
// and status plus the ride requests of the last window, for pricing models
// that bin supply and demand into their own cells.
type SupplySnapshot struct {
	AsOf     time.Time       `json:"as_of"`
	WindowS  int             `json:"window_s"`
	Drivers  []SupplyDriver  `json:"drivers"`
	Requests []SupplyRequest `json:"requests"`
	Counts   map[string]int  `json:"counts"`
}

// Snapshot returns the current supply and the requests of the last window.
func (s *Service) Snapshot(window time.Duration) SupplySnapshot {
	if window <= 0 || window > demandMemory {
		window = DefaultDemandWin
	}
	now := s.now()
	out := SupplySnapshot{AsOf: now, WindowS: int(window / time.Second), Drivers: []SupplyDriver{}, Requests: []SupplyRequest{},
		Counts: map[string]int{}}
	for _, d := range s.fleet.Snapshot() {
		if d.Status == fleet.StatusOffline {
			continue
		}
		out.Drivers = append(out.Drivers, SupplyDriver{Lat: d.Lat, Lng: d.Lng, Status: string(d.Status)})
		out.Counts[string(d.Status)]++
	}
	for _, r := range s.demand.since(now.Add(-window)) {
		out.Requests = append(out.Requests, SupplyRequest{Lat: r.p.Lat, Lng: r.p.Lng, At: r.at, Matched: r.matched})
	}
	out.Counts["requests"] = len(out.Requests)
	return out
}

func (s *Service) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	window := DefaultDemandWin
	if raw := r.URL.Query().Get("window_s"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			window = time.Duration(n) * time.Second
		}
	}
	writeJSON(w, http.StatusOK, s.Snapshot(window))
}
