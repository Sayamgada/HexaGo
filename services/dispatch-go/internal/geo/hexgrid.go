package geo

import (
	"errors"
	"math"
)

// avgEdgeKm holds H3's average hexagon edge length per resolution (H3 v4 tables).
var avgEdgeKm = [16]float64{
	1281.256011, 483.0568391, 182.5129565, 68.97922179,
	26.07175968, 9.854090990, 3.724532667, 1.406475763,
	0.531414010, 0.200786148, 0.075863783, 0.028663897,
	0.010830188, 0.004092010, 0.001546100, 0.000584169,
}

// AvgEdgeKm returns H3's average hexagon edge length at res, or 0 if res is invalid.
func AvgEdgeKm(res int) float64 {
	if !ValidResolution(res) {
		return 0
	}
	return avgEdgeKm[res]
}

// HexGrid is a pure-Go pointy-top hexagonal grid on an equirectangular
// projection centred on a reference latitude. Cells have the same average edge
// length as H3 cells at the chosen resolution, so search radii behave alike.
// It is accurate at city scale, which is all the dispatch broker needs.
type HexGrid struct {
	res    int
	size   float64 // hex edge length (km)
	cosRef float64
}

// NewHexGrid returns a HexGrid at res, projected around refLat.
func NewHexGrid(res int, refLat float64) (*HexGrid, error) {
	if !ValidResolution(res) {
		return nil, ErrInvalidResolution
	}
	if err := (Point{Lat: refLat}).Validate(); err != nil {
		return nil, err
	}
	return &HexGrid{res: res, size: avgEdgeKm[res], cosRef: math.Cos(refLat * degToRad)}, nil
}

// Resolution implements Indexer.
func (g *HexGrid) Resolution() int { return g.res }

// CellOf implements Indexer.
func (g *HexGrid) CellOf(p Point) (Cell, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	x := p.Lng * degToRad * EarthRadiusKm * g.cosRef
	y := p.Lat * degToRad * EarthRadiusKm
	q := (math.Sqrt(3)/3*x - y/3) / g.size
	r := (2.0 / 3 * y) / g.size
	qi, ri := cubeRound(q, r)
	return encodeAxial(qi, ri), nil
}

// axial neighbour directions, in ring-walk order.
var hexDirs = [6][2]int{{1, 0}, {1, -1}, {0, -1}, {-1, 0}, {-1, 1}, {0, 1}}

// Rings implements Indexer.
func (g *HexGrid) Rings(origin Cell, k int) ([][]Cell, error) {
	if k < 0 {
		return nil, errors.New("geo: ring distance must be >= 0")
	}
	q0, r0 := decodeAxial(origin)
	rings := make([][]Cell, k+1)
	rings[0] = []Cell{origin}
	for radius := 1; radius < k; radius++ {
		ring := make([]Cell, 0, 6*radius)
		q, r := q0+hexDirs[4][0]*radius, r0+hexDirs[4][1]*radius
		for side := 0; side < 6; side++ {
			for step := 0; step < radius; step++ {
				ring = append(ring, encodeAxial(q, r))
				q, r = q+hexDirs[side][0], r+hexDirs[side][1]
			}
		}
		rings[radius] = ring
	}
	return rings, nil
}

func cubeRound(qf, rf float64) (int, int) {
	sf := -qf - rf
	q, r, s := math.Round(qf), math.Round(rf), math.Round(sf)
	dq, dr, ds := math.Abs(q-qf), math.Abs(r-rf), math.Abs(s-sf)
	switch {
	case dq > dr && dq > ds:
		q = -r - s
	case dr > ds:
		r = -q - s
	}
	return int(q), int(r)
}

func encodeAxial(q, r int) Cell {
	return Cell(uint64(uint32(int32(q)))<<32 | uint64(uint32(int32(r))))
}

func decodeAxial(c Cell) (int, int) {
	return int(int32(uint32(uint64(c) >> 32))), int(int32(uint32(uint64(c))))
}
