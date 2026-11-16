// Package geo provides hexagonal spatial indexing (H3) and great-circle helpers
// for the dispatch broker.
//
// The Indexer interface has two implementations:
//   - H3 (uber/h3-go), compiled when CGO is enabled (CI, Docker, production).
//   - HexGrid, a pure-Go axial hex grid sized like H3 cells, used when CGO is
//     off (e.g. Windows dev machines without a C toolchain).
//
// NewIndexer picks the best one available for the current build.
package geo

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// EarthRadiusKm is the mean Earth radius (IUGG).
const EarthRadiusKm = 6371.0088

// DefaultResolution is the H3 resolution used for driver indexing (~0.1 km² cells).
const DefaultResolution = 9

var (
	// ErrInvalidResolution is returned for resolutions outside H3's 0..15.
	ErrInvalidResolution = errors.New("geo: resolution must be between 0 and 15")
	// ErrInvalidPoint is returned for out-of-range or non-finite coordinates.
	ErrInvalidPoint = errors.New("geo: invalid coordinates")
)

// Cell is a hexagonal cell identifier. For the H3 backend it is the H3 index.
type Cell uint64

// String renders the cell as lower-case hex, the canonical H3 string form.
func (c Cell) String() string { return strconv.FormatUint(uint64(c), 16) }

// MarshalText makes cells render as hex strings in JSON.
func (c Cell) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// Point is a WGS84 coordinate in degrees.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Validate reports whether the point is a finite, in-range coordinate.
func (p Point) Validate() error {
	if math.IsNaN(p.Lat) || math.IsNaN(p.Lng) || math.IsInf(p.Lat, 0) || math.IsInf(p.Lng, 0) ||
		p.Lat < -90 || p.Lat > 90 || p.Lng < -180 || p.Lng > 180 {
		return fmt.Errorf("%w: lat=%v lng=%v", ErrInvalidPoint, p.Lat, p.Lng)
	}
	return nil
}

// ValidResolution reports whether res is a valid H3 resolution.
func ValidResolution(res int) bool { return res >= 0 && res <= 15 }

// Indexer maps coordinates to hexagonal cells and enumerates neighbourhoods.
type Indexer interface {
	// Resolution returns the cell resolution (0 = coarsest, 15 = finest).
	Resolution() int
	// CellOf returns the cell containing p.
	CellOf(p Point) (Cell, error)
	// Rings returns k+1 rings around origin: rings[i] holds the cells exactly
	// i grid steps away, so rings[0] is {origin}.
	Rings(origin Cell, k int) ([][]Cell, error)
}

// NewIndexer returns the H3 indexer when built with CGO, otherwise a HexGrid
// projected around ref (normally the city centre).
func NewIndexer(res int, ref Point) (Indexer, error) {
	if !ValidResolution(res) {
		return nil, ErrInvalidResolution
	}
	return newDefaultIndexer(res, ref)
}

const degToRad = math.Pi / 180

// HaversineKm returns the great-circle distance between a and b in kilometres.
func HaversineKm(a, b Point) float64 {
	lat1, lat2 := a.Lat*degToRad, b.Lat*degToRad
	dLat := lat2 - lat1
	dLng := (b.Lng - a.Lng) * degToRad
	s := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * EarthRadiusKm * math.Asin(math.Min(1, math.Sqrt(s)))
}

// BearingDeg returns the initial bearing from a to b in degrees, in [0, 360).
func BearingDeg(a, b Point) float64 {
	lat1, lat2 := a.Lat*degToRad, b.Lat*degToRad
	dLng := (b.Lng - a.Lng) * degToRad
	y := math.Sin(dLng) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLng)
	deg := math.Atan2(y, x) / degToRad
	return math.Mod(deg+360, 360)
}

// AngleDiffDeg returns the absolute smallest difference between two headings, in [0, 180].
func AngleDiffDeg(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		d = 360 - d
	}
	return d
}
