package sim

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Router returns a drivable path from one point to another.
type Router interface {
	Route(ctx context.Context, from, to geo.Point) ([]geo.Point, error)
}

// StraightLine is the fallback router: a direct segment.
type StraightLine struct{}

// Route implements Router.
func (StraightLine) Route(_ context.Context, from, to geo.Point) ([]geo.Point, error) {
	return []geo.Point{from, to}, nil
}

// OSRMOptions configures an OSRM client.
type OSRMOptions struct {
	BaseURL   string        // e.g. https://router.project-osrm.org
	RPS       float64       // max requests per second (public server policy: 1)
	Timeout   time.Duration // per HTTP request, default 4 s
	CacheSize int           // routes kept, default 2048
	// Quantum is the cache grid in degrees (~220 m at 0.002). Endpoints in the
	// same cell share a route; the exact endpoints are added on each end.
	Quantum float64
	Client  *http.Client
}

// OSRMStats are cache and request counters.
type OSRMStats struct {
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	Errors   uint64 `json:"errors"`
	Cached   int    `json:"cached"`
	Requests uint64 `json:"requests"`
}

// OSRM is a rate-limited, caching client for the OSRM /route service.
type OSRM struct {
	base    string
	timeout time.Duration
	quantum float64
	client  *http.Client

	limMu    sync.Mutex
	interval time.Duration
	next     time.Time

	mu    sync.Mutex
	cap   int
	lru   *list.List
	items map[routeKey]*list.Element
	stats OSRMStats
}

type routeKey struct{ a, b, c, d int32 }

type cacheEntry struct {
	key routeKey
	pts []geo.Point // road geometry between the quantized endpoints
}

// NewOSRM returns an OSRM client.
func NewOSRM(o OSRMOptions) *OSRM {
	if o.RPS <= 0 {
		o.RPS = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = 4 * time.Second
	}
	if o.CacheSize <= 0 {
		o.CacheSize = 2048
	}
	if o.Quantum <= 0 {
		o.Quantum = 0.002
	}
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	return &OSRM{
		base: strings.TrimRight(o.BaseURL, "/"), timeout: o.Timeout, quantum: o.Quantum, client: o.Client,
		interval: time.Duration(float64(time.Second) / o.RPS),
		cap:      o.CacheSize, lru: list.New(), items: map[routeKey]*list.Element{},
	}
}

func (c *OSRM) snap(p geo.Point) (int32, int32, geo.Point) {
	la, ln := int32(math.Round(p.Lat/c.quantum)), int32(math.Round(p.Lng/c.quantum))
	return la, ln, geo.Point{Lat: float64(la) * c.quantum, Lng: float64(ln) * c.quantum}
}

// Route implements Router. The result starts at from and ends at to exactly.
func (c *OSRM) Route(ctx context.Context, from, to geo.Point) ([]geo.Point, error) {
	a, b, qf := c.snap(from)
	cc, d, qt := c.snap(to)
	key := routeKey{a, b, cc, d}

	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		c.lru.MoveToFront(el)
		c.stats.Hits++
		pts := el.Value.(*cacheEntry).pts
		c.mu.Unlock()
		return frame(from, pts, to), nil
	}
	c.stats.Misses++
	c.mu.Unlock()

	pts, err := c.fetch(ctx, qf, qt)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.stats.Errors++
		return nil, err
	}
	c.items[key] = c.lru.PushFront(&cacheEntry{key: key, pts: pts})
	for c.lru.Len() > c.cap {
		old := c.lru.Back()
		c.lru.Remove(old)
		delete(c.items, old.Value.(*cacheEntry).key)
	}
	return frame(from, pts, to), nil
}

func frame(from geo.Point, pts []geo.Point, to geo.Point) []geo.Point {
	out := make([]geo.Point, 0, len(pts)+2)
	out = append(out, from)
	out = append(out, pts...)
	return append(out, to)
}

// Stats returns cache and request counters.
func (c *OSRM) Stats() OSRMStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Cached = c.lru.Len()
	return s
}

// wait blocks until the rate limiter grants a request slot (burst 1).
func (c *OSRM) wait(ctx context.Context) error {
	c.limMu.Lock()
	now := time.Now()
	slot := c.next
	if slot.Before(now) {
		slot = now
	}
	c.next = slot.Add(c.interval)
	c.limMu.Unlock()

	d := time.Until(slot)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		c.limMu.Lock()
		if c.next.Equal(slot.Add(c.interval)) { // nobody queued behind us: give the slot back
			c.next = slot
		}
		c.limMu.Unlock()
		return ctx.Err()
	}
}

type osrmResponse struct {
	Code   string `json:"code"`
	Routes []struct {
		Geometry string `json:"geometry"`
	} `json:"routes"`
}

func (c *OSRM) fetch(ctx context.Context, from, to geo.Point) ([]geo.Point, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.stats.Requests++
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	url := fmt.Sprintf("%s/route/v1/driving/%.6f,%.6f;%.6f,%.6f?overview=simplified&geometries=polyline&alternatives=false&steps=false",
		c.base, from.Lng, from.Lat, to.Lng, to.Lat)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hexago-sim/1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("osrm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("osrm: HTTP %d", resp.StatusCode)
	}
	var body osrmResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("osrm: decode: %w", err)
	}
	if body.Code != "Ok" || len(body.Routes) == 0 {
		return nil, fmt.Errorf("osrm: no route (%s)", body.Code)
	}
	return decodePolyline(body.Routes[0].Geometry)
}

var errBadPolyline = errors.New("osrm: malformed polyline")

// decodePolyline decodes Google's Encoded Polyline Algorithm Format (precision 5).
func decodePolyline(s string) ([]geo.Point, error) {
	var pts []geo.Point
	lat, lng := 0, 0
	i := 0
	next := func() (int, error) {
		result, shift := 0, 0
		for {
			if i >= len(s) {
				return 0, errBadPolyline
			}
			b := int(s[i]) - 63
			i++
			if b < 0 {
				return 0, errBadPolyline
			}
			result |= (b & 0x1f) << shift
			shift += 5
			if b < 0x20 {
				break
			}
			if shift > 30 {
				return 0, errBadPolyline
			}
		}
		if result&1 != 0 {
			return ^(result >> 1), nil
		}
		return result >> 1, nil
	}
	for i < len(s) {
		dlat, err := next()
		if err != nil {
			return nil, err
		}
		dlng, err := next()
		if err != nil {
			return nil, err
		}
		lat += dlat
		lng += dlng
		pts = append(pts, geo.Point{Lat: float64(lat) / 1e5, Lng: float64(lng) / 1e5})
	}
	if len(pts) == 0 {
		return nil, errBadPolyline
	}
	return pts, nil
}
