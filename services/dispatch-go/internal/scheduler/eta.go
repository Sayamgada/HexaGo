package scheduler

import (
	"math"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Trip-time model shared with agents-py's haversine fallback
// (agents/booking/itinerary.py): road distance is 1.3x the straight line; the
// first 6 km run at city speed (18 km/h), the rest on arterials (40 km/h).
const (
	RoadFactor  = 1.3
	CityKm      = 6.0
	CityKmh     = 18.0
	ArterialKmh = 40.0
	MinTripMin  = 3.0
)

// Trip is a pickup → dropoff estimate.
type Trip struct {
	DistanceKm float64 `json:"distance_km"`
	DurationS  float64 `json:"duration_s"`
}

// DurationForRoadKm returns the trip time in minutes for a road distance.
func DurationForRoadKm(roadKm float64) float64 {
	var minutes float64
	if roadKm <= CityKm {
		minutes = roadKm / CityKmh * 60
	} else {
		minutes = CityKm/CityKmh*60 + (roadKm-CityKm)/ArterialKmh*60
	}
	return math.Round(math.Max(MinTripMin, minutes)*10) / 10
}

// EstimateTrip estimates road distance and duration between two points.
func EstimateTrip(a, b geo.Point) Trip {
	road := geo.HaversineKm(a, b) * RoadFactor
	return Trip{DistanceKm: math.Round(road*100) / 100, DurationS: DurationForRoadKm(road) * 60}
}

// CellLoad is supply (idle cars) and demand (cars busy en route or on a trip)
// in the pickup's H3 cell and its neighbours: the input to the surge multiplier.
type CellLoad struct {
	Cell   string `json:"cell"`
	Supply int    `json:"supply"`
	Demand int    `json:"demand"`
	Rings  int    `json:"rings"`
	Cells  int    `json:"cells"`
}

// Load counts cars around p within k H3 rings.
func Load(ix geo.Indexer, idx *fleet.Index, p geo.Point, k int) (CellLoad, error) {
	cell, err := ix.CellOf(p)
	if err != nil {
		return CellLoad{}, err
	}
	rings, err := ix.Rings(cell, k)
	if err != nil {
		return CellLoad{}, err
	}
	var cells []geo.Cell
	for _, ring := range rings {
		cells = append(cells, ring...)
	}
	out := CellLoad{Cell: cell.String(), Rings: k, Cells: len(cells)}
	idx.VisitCells(cells, func(d fleet.Driver) {
		switch d.Status {
		case fleet.StatusIdle:
			out.Supply++
		case fleet.StatusEnRoute, fleet.StatusOnTrip:
			out.Demand++
		}
	})
	return out, nil
}
