//go:build !cgo

package spatial

import (
	"fmt"
)

// LatLngToH3Cell fallback for non-CGO environments. In Docker/CI with CGO enabled,
// the native uber/h3-go v4 C implementation is compiled and executed.
func LatLngToH3Cell(lat, lng float64, resolution int) (string, error) {
	// Deterministic spatial cell representation for non-CGO host dev
	return fmt.Sprintf("hex_%d_%.4f_%.4f", resolution, lat, lng), nil
}
