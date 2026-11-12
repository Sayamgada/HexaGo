//go:build cgo

package spatial

import (
	"fmt"

	"github.com/uber/h3-go/v4"
)

// LatLngToH3Cell converts latitude and longitude into an H3 index string at the given resolution.
func LatLngToH3Cell(lat, lng float64, resolution int) (string, error) {
	coord := h3.LatLng{
		Lat: lat,
		Lng: lng,
	}
	cell := h3.LatLngToCell(coord, resolution)
	if !cell.IsValid() {
		return "", fmt.Errorf("invalid H3 cell for lat: %f, lng: %f", lat, lng)
	}
	return cell.String(), nil
}
