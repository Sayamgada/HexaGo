package sim

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

func TestDecodePolyline(t *testing.T) {
	// Example from the Encoded Polyline Algorithm Format documentation.
	pts, err := decodePolyline("_p~iF~ps|U_ulLnnqC_mqNvxq`@")
	if err != nil {
		t.Fatal(err)
	}
	want := []geo.Point{{Lat: 38.5, Lng: -120.2}, {Lat: 40.7, Lng: -120.95}, {Lat: 43.252, Lng: -126.453}}
	if len(pts) != len(want) {
		t.Fatalf("got %d points", len(pts))
	}
	for i := range want {
		if math.Abs(pts[i].Lat-want[i].Lat) > 1e-9 || math.Abs(pts[i].Lng-want[i].Lng) > 1e-9 {
			t.Fatalf("point %d = %+v, want %+v", i, pts[i], want[i])
		}
	}
	if _, err := decodePolyline("_p~iF~ps|U_"); err == nil {
		t.Fatal("expected error for truncated polyline")
	}
}

type osrmFake struct {
	hits  atomic.Int32
	ua    atomic.Value
	path  atomic.Value
	delay time.Duration
	code  int
	body  string
}

func (f *osrmFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	f.ua.Store(r.UserAgent())
	f.path.Store(r.URL.Path + "?" + r.URL.RawQuery)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	if f.code != 0 {
		w.WriteHeader(f.code)
	}
	body := f.body
	if body == "" {
		body = `{"code":"Ok","routes":[{"geometry":"_p~iF~ps|U_ulLnnqC_mqNvxq` + "`" + `@","distance":1000,"duration":120}]}`
	}
	fmt.Fprint(w, body)
}

func newOSRM(t *testing.T, f *osrmFake, mutate ...func(*OSRMOptions)) *OSRM {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	o := OSRMOptions{BaseURL: srv.URL, RPS: 1000, Timeout: 500 * time.Millisecond}
	for _, m := range mutate {
		m(&o)
	}
	return NewOSRM(o)
}

var (
	from = geo.Point{Lat: 12.9716, Lng: 77.5946}
	to   = geo.Point{Lat: 12.9352, Lng: 77.6245}
)

func TestOSRMRouteDecodesAndFramesEndpoints(t *testing.T) {
	f := &osrmFake{}
	c := newOSRM(t, f)
	pts, err := c.Route(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	// Exact endpoints are prepended/appended around the road geometry.
	if pts[0] != from || pts[len(pts)-1] != to || len(pts) != 5 {
		t.Fatalf("route %v", pts)
	}
	p := f.path.Load().(string)
	if !strings.HasPrefix(p, "/route/v1/driving/") || !strings.Contains(p, "overview=simplified") {
		t.Fatalf("request path %q", p)
	}
	if ua := f.ua.Load().(string); !strings.HasPrefix(ua, "hexago-sim/") {
		t.Fatalf("user agent %q", ua)
	}
}

func TestOSRMCachesByQuantizedEndpoints(t *testing.T) {
	f := &osrmFake{}
	c := newOSRM(t, f)
	ctx := context.Background()
	if _, err := c.Route(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	// ~30 m away: same cache cell.
	near := geo.Point{Lat: from.Lat + 0.0002, Lng: from.Lng + 0.0002}
	pts, err := c.Route(ctx, near, to)
	if err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1 (cached)", f.hits.Load())
	}
	if pts[0] != near {
		t.Fatal("cached route must start at the caller's exact origin")
	}
	st := c.Stats()
	if st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestOSRMCacheEvictsOldest(t *testing.T) {
	f := &osrmFake{}
	c := newOSRM(t, f, func(o *OSRMOptions) { o.CacheSize = 2 })
	ctx := context.Background()
	dests := []geo.Point{{Lat: 12.90, Lng: 77.60}, {Lat: 12.92, Lng: 77.60}, {Lat: 12.94, Lng: 77.60}}
	for _, d := range dests {
		if _, err := c.Route(ctx, from, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Route(ctx, from, dests[0]); err != nil { // evicted → refetch
		t.Fatal(err)
	}
	if f.hits.Load() != 4 {
		t.Fatalf("hits %d, want 4", f.hits.Load())
	}
}

func TestOSRMErrors(t *testing.T) {
	cases := map[string]*osrmFake{
		"http 500":  {code: http.StatusInternalServerError},
		"no route":  {body: `{"code":"NoRoute","routes":[]}`},
		"bad json":  {body: `{`},
		"too slow":  {delay: 2 * time.Second},
		"bad shape": {body: `{"code":"Ok","routes":[{"geometry":"_p~iF"}]}`},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			c := newOSRM(t, f, func(o *OSRMOptions) { o.Timeout = 100 * time.Millisecond })
			start := time.Now()
			if _, err := c.Route(context.Background(), from, to); err == nil {
				t.Fatal("expected error")
			}
			if time.Since(start) > time.Second {
				t.Fatal("error path took too long")
			}
		})
	}
}

func TestOSRMRateLimited(t *testing.T) {
	f := &osrmFake{}
	c := newOSRM(t, f, func(o *OSRMOptions) { o.RPS = 10 })
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 4; i++ {
		d := geo.Point{Lat: 12.90 + float64(i)*0.02, Lng: 77.60}
		if _, err := c.Route(ctx, from, d); err != nil {
			t.Fatal(err)
		}
	}
	// 4 uncached requests at 10/s with a burst of 1 need at least ~300 ms.
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("4 requests in %v; not rate limited", el)
	}
}

func TestOSRMRateLimitRespectsContext(t *testing.T) {
	f := &osrmFake{}
	c := newOSRM(t, f, func(o *OSRMOptions) { o.RPS = 0.1 })
	ctx := context.Background()
	if _, err := c.Route(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.Route(short, from, geo.Point{Lat: 12.8, Lng: 77.7}); err == nil {
		t.Fatal("expected context error while waiting for a token")
	}
}

func TestStraightLineRouter(t *testing.T) {
	pts, err := StraightLine{}.Route(context.Background(), from, to)
	if err != nil || len(pts) != 2 || pts[0] != from || pts[1] != to {
		t.Fatalf("%v %v", pts, err)
	}
}

func TestPathInterpolation(t *testing.T) {
	a := geo.Point{Lat: 12.97, Lng: 77.59}
	b := geo.Point{Lat: 12.97, Lng: 77.60} // due east
	c := geo.Point{Lat: 12.98, Lng: 77.60} // then due north
	p := newPath([]geo.Point{a, b, c})
	if math.Abs(p.total-(geo.HaversineKm(a, b)+geo.HaversineKm(b, c))) > 1e-9 {
		t.Fatalf("total %.4f", p.total)
	}
	mid, hdg := p.at(geo.HaversineKm(a, b) / 2)
	if math.Abs(mid.Lng-77.595) > 1e-6 || math.Abs(hdg-90) > 1 {
		t.Fatalf("mid %+v heading %.1f", mid, hdg)
	}
	_, hdg = p.at(geo.HaversineKm(a, b) + 0.1)
	if math.Abs(hdg) > 1 && math.Abs(hdg-360) > 1 {
		t.Fatalf("second leg heading %.1f, want ~0", hdg)
	}
	end, _ := p.at(p.total + 5)
	if end != c {
		t.Fatalf("past the end %+v", end)
	}
	start, _ := p.at(-1)
	if start != a {
		t.Fatalf("before start %+v", start)
	}
	if one := newPath([]geo.Point{a}); one.total != 0 {
		t.Fatal("single-point path")
	}
}
