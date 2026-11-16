package geo

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

var bengaluru = Point{Lat: 12.9716, Lng: 77.5946}

func TestHaversineKm(t *testing.T) {
	tests := []struct {
		name string
		a, b Point
		want float64
		tol  float64
	}{
		{"same point", bengaluru, bengaluru, 0, 1e-9},
		{"one degree latitude", Point{0, 0}, Point{1, 0}, 111.195, 0.01},
		{"one degree longitude at equator", Point{0, 0}, Point{0, 1}, 111.195, 0.01},
		{"MG Road to Indiranagar", Point{12.9756, 77.6066}, Point{12.9784, 77.6408}, 3.72, 0.05},
		{"antipodal", Point{0, 0}, Point{0, 180}, math.Pi * EarthRadiusKm, 0.001},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HaversineKm(tc.a, tc.b)
			if math.Abs(got-tc.want) > tc.tol {
				t.Errorf("HaversineKm = %.4f, want %.4f ± %.4f", got, tc.want, tc.tol)
			}
			if back := HaversineKm(tc.b, tc.a); math.Abs(back-got) > 1e-9 {
				t.Errorf("not symmetric: %.6f vs %.6f", got, back)
			}
		})
	}
}

func TestBearingDeg(t *testing.T) {
	tests := []struct {
		name string
		a, b Point
		want float64
	}{
		{"north", Point{0, 0}, Point{1, 0}, 0},
		{"east", Point{0, 0}, Point{0, 1}, 90},
		{"south", Point{1, 0}, Point{0, 0}, 180},
		{"west", Point{0, 1}, Point{0, 0}, 270},
		{"same point", bengaluru, bengaluru, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BearingDeg(tc.a, tc.b)
			if math.Abs(got-tc.want) > 0.01 {
				t.Errorf("BearingDeg = %.4f, want %.4f", got, tc.want)
			}
			if got < 0 || got >= 360 {
				t.Errorf("BearingDeg out of range: %f", got)
			}
		})
	}
}

func TestAngleDiffDeg(t *testing.T) {
	tests := []struct{ a, b, want float64 }{
		{0, 0, 0},
		{10, 350, 20},
		{350, 10, 20},
		{90, 270, 180},
		{0, 180, 180},
		{720, 45, 45},
		{-90, 90, 180},
	}
	for _, tc := range tests {
		if got := AngleDiffDeg(tc.a, tc.b); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("AngleDiffDeg(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPointValidate(t *testing.T) {
	tests := []struct {
		name string
		p    Point
		ok   bool
	}{
		{"bengaluru", bengaluru, true},
		{"poles and dateline", Point{90, 180}, true},
		{"lat too big", Point{91, 0}, false},
		{"lng too small", Point{0, -181}, false},
		{"NaN", Point{math.NaN(), 0}, false},
		{"Inf", Point{0, math.Inf(1)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate() err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidPoint) {
				t.Errorf("error %v is not ErrInvalidPoint", err)
			}
		})
	}
}

func TestValidResolution(t *testing.T) {
	for _, tc := range []struct {
		res int
		ok  bool
	}{{-1, false}, {0, true}, {9, true}, {15, true}, {16, false}} {
		if got := ValidResolution(tc.res); got != tc.ok {
			t.Errorf("ValidResolution(%d) = %v, want %v", tc.res, got, tc.ok)
		}
	}
}

func TestCellStringAndJSON(t *testing.T) {
	c := Cell(0x89618922a4bffff)
	if got := c.String(); got != "89618922a4bffff" {
		t.Errorf("String() = %q", got)
	}
	b, err := json.Marshal(map[string]Cell{"cell": c})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"cell":"89618922a4bffff"}` {
		t.Errorf("json = %s", b)
	}
}

// indexerContract runs the behaviour every Indexer must satisfy.
func indexerContract(t *testing.T, idx Indexer, res int) {
	t.Helper()
	if idx.Resolution() != res {
		t.Errorf("Resolution() = %d, want %d", idx.Resolution(), res)
	}

	origin, err := idx.CellOf(bengaluru)
	if err != nil {
		t.Fatalf("CellOf: %v", err)
	}
	again, _ := idx.CellOf(bengaluru)
	if origin != again {
		t.Errorf("CellOf not deterministic: %v vs %v", origin, again)
	}
	if _, err := idx.CellOf(Point{Lat: 100, Lng: 0}); !errors.Is(err, ErrInvalidPoint) {
		t.Errorf("CellOf(invalid) err = %v, want ErrInvalidPoint", err)
	}

	tests := []struct {
		k     int
		sizes []int
	}{
		{0, []int{1}},
		{1, []int{1, 6}},
		{3, []int{1, 6, 12, 18}},
	}
	for _, tc := range tests {
		rings, err := idx.Rings(origin, tc.k)
		if err != nil {
			t.Fatalf("Rings(k=%d): %v", tc.k, err)
		}
		if len(rings) != len(tc.sizes) {
			t.Fatalf("Rings(k=%d) returned %d rings, want %d", tc.k, len(rings), len(tc.sizes))
		}
		seen := map[Cell]bool{}
		for i, ring := range rings {
			if len(ring) != tc.sizes[i] {
				t.Errorf("ring %d has %d cells, want %d", i, len(ring), tc.sizes[i])
			}
			for _, c := range ring {
				if seen[c] {
					t.Errorf("cell %v appears twice", c)
				}
				seen[c] = true
			}
		}
		if rings[0][0] != origin {
			t.Errorf("ring 0 = %v, want origin %v", rings[0][0], origin)
		}
	}

	if _, err := idx.Rings(origin, -1); err == nil {
		t.Error("Rings(k=-1) should fail")
	}

	// A point half an edge length away is in the origin cell or its first ring.
	halfEdgeDeg := AvgEdgeKm(res) / 2 / 111.195
	near, _ := idx.CellOf(Point{Lat: bengaluru.Lat + halfEdgeDeg, Lng: bengaluru.Lng})
	rings, _ := idx.Rings(origin, 1)
	found := false
	for _, ring := range rings {
		for _, c := range ring {
			if c == near {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("nearby cell %v not within 1 ring of %v", near, origin)
	}
}

func TestHexGridContract(t *testing.T) {
	for _, res := range []int{7, 9, 12} {
		g, err := NewHexGrid(res, bengaluru.Lat)
		if err != nil {
			t.Fatal(err)
		}
		indexerContract(t, g, res)
	}
}

func TestHexGridErrors(t *testing.T) {
	for _, res := range []int{-1, 16} {
		if _, err := NewHexGrid(res, 0); !errors.Is(err, ErrInvalidResolution) {
			t.Errorf("NewHexGrid(%d) err = %v", res, err)
		}
	}
	if _, err := NewHexGrid(9, 95); !errors.Is(err, ErrInvalidPoint) {
		t.Errorf("NewHexGrid with bad ref lat err = %v", err)
	}
}

func TestHexGridCellSize(t *testing.T) {
	g, _ := NewHexGrid(9, bengaluru.Lat)
	origin, _ := g.CellOf(bengaluru)
	// Walking due east, the cell should change within ~2 edge lengths and
	// not before the first ~0.5 edge length on average.
	edge := AvgEdgeKm(9)
	changed := -1.0
	for d := 0.0; d < 4*edge; d += edge / 20 {
		p := Point{Lat: bengaluru.Lat, Lng: bengaluru.Lng + d/(111.195*math.Cos(bengaluru.Lat*math.Pi/180))}
		c, _ := g.CellOf(p)
		if c != origin {
			changed = d
			break
		}
	}
	if changed <= 0 || changed > 2*edge {
		t.Errorf("cell changed after %.3f km, edge %.3f km", changed, edge)
	}
}

func TestHexGridEncodingRoundTrip(t *testing.T) {
	for _, qr := range [][2]int{{0, 0}, {1, -1}, {-5, 7}, {123456, -654321}, {-2147483648, 2147483647}} {
		c := encodeAxial(qr[0], qr[1])
		q, r := decodeAxial(c)
		if q != qr[0] || r != qr[1] {
			t.Errorf("round trip (%d,%d) -> (%d,%d)", qr[0], qr[1], q, r)
		}
	}
}

func TestAvgEdgeKm(t *testing.T) {
	if AvgEdgeKm(-1) != 0 || AvgEdgeKm(16) != 0 {
		t.Error("out-of-range resolution should return 0")
	}
	if e := AvgEdgeKm(9); e < 0.15 || e > 0.25 {
		t.Errorf("AvgEdgeKm(9) = %v", e)
	}
	for r := 1; r <= 15; r++ {
		if AvgEdgeKm(r) >= AvgEdgeKm(r-1) {
			t.Errorf("edge length not decreasing at res %d", r)
		}
	}
}

func TestNewIndexer(t *testing.T) {
	idx, err := NewIndexer(DefaultResolution, bengaluru)
	if err != nil {
		t.Fatal(err)
	}
	indexerContract(t, idx, DefaultResolution)
	if Backend() == "" {
		t.Error("Backend() should name the active implementation")
	}

	if _, err := NewIndexer(16, bengaluru); !errors.Is(err, ErrInvalidResolution) {
		t.Errorf("NewIndexer(16) err = %v", err)
	}
}
