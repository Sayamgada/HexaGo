package sim

import (
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// path is a polyline with cumulative distances for O(log n) interpolation.
type path struct {
	pts   []geo.Point
	cum   []float64 // km from pts[0] to pts[i]
	total float64
}

func newPath(pts []geo.Point) *path {
	p := &path{pts: pts, cum: make([]float64, len(pts))}
	for i := 1; i < len(pts); i++ {
		p.cum[i] = p.cum[i-1] + geo.HaversineKm(pts[i-1], pts[i])
	}
	if len(pts) > 0 {
		p.total = p.cum[len(pts)-1]
	}
	return p
}

// at returns the point d km along the path and the heading of that segment.
func (p *path) at(d float64) (geo.Point, float64) {
	n := len(p.pts)
	if n == 0 {
		return geo.Point{}, 0
	}
	if n == 1 || d <= 0 {
		return p.pts[0], p.segHeading(1)
	}
	if d >= p.total {
		return p.pts[n-1], p.segHeading(n - 1)
	}
	// Binary search for the segment containing d.
	lo, hi := 1, n-1
	for lo < hi {
		mid := (lo + hi) / 2
		if p.cum[mid] < d {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	a, b := p.pts[lo-1], p.pts[lo]
	seg := p.cum[lo] - p.cum[lo-1]
	f := 0.0
	if seg > 0 {
		f = (d - p.cum[lo-1]) / seg
	}
	return geo.Point{Lat: a.Lat + (b.Lat-a.Lat)*f, Lng: a.Lng + (b.Lng-a.Lng)*f}, p.segHeading(lo)
}

// segHeading is the bearing of the segment ending at pts[i], skipping
// zero-length segments.
func (p *path) segHeading(i int) float64 {
	for j := min(i, len(p.pts)-1); j >= 1; j-- {
		if p.pts[j] != p.pts[j-1] {
			return geo.BearingDeg(p.pts[j-1], p.pts[j])
		}
	}
	return 0
}
