package matching

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Three fine rings at res 9 cover roughly 1 km; a driver 2 km away used to be
// "no driver available". The adaptive search must keep expanding.
func TestMatchAdaptiveRadiusFindsDriverAt2Km(t *testing.T) {
	idx, m, _ := newFixture(t)
	mustUpsert(t, idx, at("two-km", offset(center, 2, 0), fleet.TierMini))
	mustUpsert(t, idx, at("decoy-auto", center, fleet.TierAuto)) // auto never upgrades to mini

	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini})
	if err != nil {
		t.Fatalf("err = %v, want a match 2 km out", err)
	}
	if res.Best.Driver.ID != "two-km" {
		t.Fatalf("Best = %s", res.Best.Driver.ID)
	}
	if res.RingsSearched <= 4 {
		t.Errorf("RingsSearched = %d, want > 4 (beyond the old 3-ring limit)", res.RingsSearched)
	}
	if res.Upgraded || res.Tier != fleet.TierMini || res.RequestedTier != fleet.TierMini {
		t.Errorf("tier fields: requested %s matched %s upgraded %v", res.RequestedTier, res.Tier, res.Upgraded)
	}
	if res.SearchRadiusKm < 2 || res.SearchRadiusKm > DefaultConfig().MaxPickupKm {
		t.Errorf("SearchRadiusKm = %.2f", res.SearchRadiusKm)
	}
	if res.Considered != 1 {
		t.Errorf("Considered = %d, want 1", res.Considered)
	}
}

func TestMatchExpandsUntilMinCandidates(t *testing.T) {
	idx, m, _ := newFixture(t)
	// One driver near, four more at ~1.5 km: the search must reach them.
	mustUpsert(t, idx, at("near", offset(center, 0.1, 0), fleet.TierSedan))
	for i := 0; i < 4; i++ {
		a := float64(i) * math.Pi / 2
		mustUpsert(t, idx, at(fmt.Sprintf("ring%d", i), offset(center, 1.5*math.Cos(a), 1.5*math.Sin(a)), fleet.TierSedan))
	}
	mustUpsert(t, idx, at("far", offset(center, 4.5, 0), fleet.TierSedan))
	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierSedan})
	if err != nil {
		t.Fatal(err)
	}
	if res.Considered != 5 {
		t.Errorf("Considered = %d, want 5 (stop once MinCandidates found, before 'far')", res.Considered)
	}
	if res.Best.Driver.ID != "near" {
		t.Errorf("Best = %s", res.Best.Driver.ID)
	}
}

func TestMatchRespectsMaxPickupKm(t *testing.T) {
	g, _ := geo.NewHexGrid(9, center.Lat)
	idx := fleet.New(g)
	cfg := DefaultConfig()
	cfg.MaxPickupKm = 1.5
	m := New(idx, g, cfg)
	mustUpsert(t, idx, at("two-km", offset(center, 2, 0), fleet.TierMini))
	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("err = %v, want ErrNoCandidates with a 1.5 km cap", err)
	}
	if res.SearchRadiusKm != 1.5 {
		t.Errorf("SearchRadiusKm = %v, want 1.5", res.SearchRadiusKm)
	}
}

func TestMatchTierUpgrade(t *testing.T) {
	tests := []struct {
		name         string
		drivers      []fleet.Driver
		tier         fleet.Tier
		wantID       string // "" = no match
		wantTier     fleet.Tier
		wantUpgraded bool
	}{
		{"mini upgrades to sedan", []fleet.Driver{at("s", offset(center, 0.3, 0), fleet.TierSedan)},
			fleet.TierMini, "s", fleet.TierSedan, true},
		{"mini upgrades to premium when no sedan", []fleet.Driver{at("p", offset(center, 0.3, 0), fleet.TierPremium)},
			fleet.TierMini, "p", fleet.TierPremium, true},
		{"sedan preferred over premium", []fleet.Driver{
			at("p", offset(center, 0.1, 0), fleet.TierPremium), at("s", offset(center, 1, 0), fleet.TierSedan)},
			fleet.TierMini, "s", fleet.TierSedan, true},
		{"requested tier wins even if farther", []fleet.Driver{
			at("s", offset(center, 0.1, 0), fleet.TierSedan), at("m", offset(center, 3, 0), fleet.TierMini)},
			fleet.TierMini, "m", fleet.TierMini, false},
		{"sedan upgrades to premium", []fleet.Driver{at("p", offset(center, 0.5, 0), fleet.TierPremium)},
			fleet.TierSedan, "p", fleet.TierPremium, true},
		{"premium has no upgrade", []fleet.Driver{at("x", center, fleet.TierXL)},
			fleet.TierPremium, "", "", false},
		{"auto upgrades to mini", []fleet.Driver{at("m", center, fleet.TierMini)},
			fleet.TierAuto, "m", fleet.TierMini, true},
		{"auto never jumps to premium", []fleet.Driver{at("p", center, fleet.TierPremium)},
			fleet.TierAuto, "", "", false},
		{"xl does not upgrade", []fleet.Driver{at("p", center, fleet.TierPremium)},
			fleet.TierXL, "", "", false},
		{"upgrade candidate must be within radius", []fleet.Driver{at("s", offset(center, 7, 0), fleet.TierSedan)},
			fleet.TierMini, "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx, m, _ := newFixture(t)
			for _, d := range tc.drivers {
				mustUpsert(t, idx, d)
			}
			res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: tc.tier})
			if tc.wantID == "" {
				if !errors.Is(err, ErrNoCandidates) {
					t.Fatalf("err = %v, want ErrNoCandidates", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Best.Driver.ID != tc.wantID || res.Tier != tc.wantTier || res.Upgraded != tc.wantUpgraded {
				t.Fatalf("best %s tier %s upgraded %v; want %s %s %v",
					res.Best.Driver.ID, res.Tier, res.Upgraded, tc.wantID, tc.wantTier, tc.wantUpgraded)
			}
			if res.RequestedTier != tc.tier {
				t.Errorf("RequestedTier = %s", res.RequestedTier)
			}
		})
	}
}

func TestMatchUpgradeDisabled(t *testing.T) {
	g, _ := geo.NewHexGrid(9, center.Lat)
	idx := fleet.New(g)
	cfg := DefaultConfig()
	cfg.AllowUpgrade = false
	m := New(idx, g, cfg)
	mustUpsert(t, idx, at("s", center, fleet.TierSedan))
	if _, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini}); !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("err = %v, want ErrNoCandidates", err)
	}
}

func TestMatchTrulyEmptyCity(t *testing.T) {
	_, m, _ := newFixture(t)
	res, err := m.Match(Request{Pickup: center, Dropoff: dropoff, Tier: fleet.TierMini})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("err = %v, want ErrNoCandidates", err)
	}
	cfg := m.Config()
	if res.SearchRadiusKm != cfg.MaxPickupKm {
		t.Errorf("SearchRadiusKm = %v, want %v", res.SearchRadiusKm, cfg.MaxPickupKm)
	}
	if want := MaxRingsFor(cfg, 9) + 1; res.RingsSearched != want {
		t.Errorf("RingsSearched = %d, want %d", res.RingsSearched, want)
	}
	if res.Considered != 0 {
		t.Errorf("Considered = %d", res.Considered)
	}
	want := []fleet.Tier{fleet.TierMini, fleet.TierSedan, fleet.TierPremium}
	if fmt.Sprint(res.TiersTried) != fmt.Sprint(want) {
		t.Errorf("TiersTried = %v, want %v", res.TiersTried, want)
	}
	if res.RequestedTier != fleet.TierMini {
		t.Errorf("RequestedTier = %s", res.RequestedTier)
	}
}

func TestMaxRingsForCoversRadius(t *testing.T) {
	cfg := DefaultConfig()
	for _, res := range []int{7, 8, 9, 10} {
		k := MaxRingsFor(cfg, res)
		edge := geo.AvgEdgeKm(res)
		// The innermost edge of ring k is ~1.5·edge·k − edge from the origin centre.
		if covered := 1.5*edge*float64(k) - edge; covered < cfg.MaxPickupKm {
			t.Errorf("res %d: k=%d covers only %.2f km", res, k, covered)
		}
	}
	if k := MaxRingsFor(Config{MaxPickupKm: 5, MaxRings: 6}, 9); k != 6 {
		t.Errorf("MaxRings cap ignored: %d", k)
	}
}

func TestUpgradeChain(t *testing.T) {
	if got := fmt.Sprint(fleet.UpgradeChain(fleet.TierMini)); got != "[sedan premium]" {
		t.Errorf("mini chain %s", got)
	}
	if got := fmt.Sprint(fleet.UpgradeChain(fleet.TierAuto)); got != "[mini sedan]" {
		t.Errorf("auto chain %s (at most two steps up)", got)
	}
	if got := fleet.UpgradeChain(fleet.TierPremium); len(got) != 0 {
		t.Errorf("premium chain %v", got)
	}
	if got := fleet.UpgradeChain(fleet.TierAV); len(got) != 0 {
		t.Errorf("av chain %v", got)
	}
}
