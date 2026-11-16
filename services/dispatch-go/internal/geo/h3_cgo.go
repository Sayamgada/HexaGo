//go:build cgo

package geo

import (
	"fmt"

	"github.com/uber/h3-go/v4"
)

// H3 is the production Indexer backed by Uber's H3 C library.
type H3 struct{ res int }

// NewH3 returns an H3 indexer at res.
func NewH3(res int) (*H3, error) {
	if !ValidResolution(res) {
		return nil, ErrInvalidResolution
	}
	return &H3{res: res}, nil
}

// Resolution implements Indexer.
func (h *H3) Resolution() int { return h.res }

// CellOf implements Indexer.
func (h *H3) CellOf(p Point) (Cell, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	c, err := h3.LatLngToCell(h3.LatLng{Lat: p.Lat, Lng: p.Lng}, h.res)
	if err != nil {
		return 0, fmt.Errorf("geo: h3 LatLngToCell: %w", err)
	}
	return Cell(c), nil
}

// Rings implements Indexer using H3 gridDiskDistances (pentagon-safe).
func (h *H3) Rings(origin Cell, k int) ([][]Cell, error) {
	if k < 0 {
		return nil, fmt.Errorf("geo: ring distance must be >= 0")
	}
	if !h3.Cell(origin).IsValid() {
		return nil, fmt.Errorf("geo: invalid H3 cell %s", origin)
	}
	rs, err := h3.GridDiskDistances(h3.Cell(origin), k)
	if err != nil {
		return nil, fmt.Errorf("geo: h3 GridDiskDistances: %w", err)
	}
	out := make([][]Cell, len(rs))
	for i, ring := range rs {
		cells := make([]Cell, len(ring))
		for j, c := range ring {
			cells[j] = Cell(c)
		}
		out[i] = cells
	}
	return out, nil
}

func newDefaultIndexer(res int, _ Point) (Indexer, error) { return NewH3(res) }

// Backend names the Indexer implementation NewIndexer returns in this build.
func Backend() string { return "h3" }
