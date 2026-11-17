package matching

import (
	"errors"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// faultyIndexer wraps a real grid and fails CellOf, or Rings once k exceeds
// failRingsAbove, so the matcher's error paths can be exercised.
type faultyIndexer struct {
	geo.Indexer
	failCellOf     bool
	failRingsAbove int
}

var errIndexer = errors.New("indexer down")

func (f faultyIndexer) CellOf(p geo.Point) (geo.Cell, error) {
	if f.failCellOf {
		return 0, errIndexer
	}
	return f.Indexer.CellOf(p)
}

func (f faultyIndexer) Rings(origin geo.Cell, k int) ([][]geo.Cell, error) {
	if k > f.failRingsAbove {
		return nil, errIndexer
	}
	return f.Indexer.Rings(origin, k)
}

func TestNewFillsInvalidTuning(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxPickupKm = -1
	cfg.MinCandidates = 0
	g, _ := geo.NewHexGrid(9, center.Lat)
	got := New(fleet.New(g), g, cfg).Config()
	if got.MaxPickupKm != DefaultConfig().MaxPickupKm {
		t.Fatalf("MaxPickupKm = %v, want default %v", got.MaxPickupKm, DefaultConfig().MaxPickupKm)
	}
	if got.MinCandidates != 1 {
		t.Fatalf("MinCandidates = %d, want 1", got.MinCandidates)
	}
}

func TestMaxRingsForInvalidResolution(t *testing.T) {
	if got := MaxRingsFor(DefaultConfig(), -1); got != fineRings {
		t.Fatalf("MaxRingsFor(res=-1) = %d, want %d", got, fineRings)
	}
}

func TestMatchIndexerErrors(t *testing.T) {
	g, err := geo.NewHexGrid(9, center.Lat)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierSedan}
	cases := []struct {
		name string
		idx  faultyIndexer
		want error
	}{
		{"cell lookup", faultyIndexer{Indexer: g, failCellOf: true, failRingsAbove: 1 << 20}, ErrInvalidRequest},
		{"fine rings", faultyIndexer{Indexer: g, failRingsAbove: -1}, errIndexer},
		{"wide rings", faultyIndexer{Indexer: g, failRingsAbove: fineRings}, errIndexer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An empty fleet never reaches MinCandidates, so the wide sweep runs.
			m := New(fleet.New(g), tc.idx, DefaultConfig())
			if _, err := m.Match(req); !errors.Is(err, tc.want) {
				t.Fatalf("Match error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestScoreAVExactlyAtBatteryRequirement(t *testing.T) {
	cfg := DefaultConfig()
	req := Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierAV}
	tripKm := geo.HaversineKm(req.Pickup, req.Dropoff) * cfg.RoadFactor
	required := (0+tripKm)*cfg.AVPctPerKm + cfg.AVReservePct
	d := fleet.Driver{ID: "av", Lat: center.Lat, Lng: center.Lng, Status: fleet.StatusIdle,
		Tier: fleet.TierAV, Rating: 4.8, BatteryPct: required}
	b, ok := Score(cfg, d, req, t0)
	if !ok {
		t.Fatal("AV exactly at the battery requirement should be eligible")
	}
	if b.Battery <= 0 || b.Battery > 1e-5 {
		t.Fatalf("Battery = %v, want a tiny positive score", b.Battery)
	}
}
