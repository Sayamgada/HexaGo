package spatial_test

import (
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/spatial"
)

func TestLatLngToH3Cell(t *testing.T) {
	// Bengaluru center
	lat := 12.9716
	lng := 77.5946
	res := 8

	cell, err := spatial.LatLngToH3Cell(lat, lng, res)
	if err != nil {
		t.Fatalf("Unexpected error converting LatLng to H3 cell: %v", err)
	}

	if cell == "" {
		t.Fatal("Expected non-empty H3 cell string")
	}
}

