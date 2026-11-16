//go:build !cgo

package geo

func newDefaultIndexer(res int, ref Point) (Indexer, error) { return NewHexGrid(res, ref.Lat) }

// Backend names the Indexer implementation NewIndexer returns in this build.
func Backend() string { return "hexgrid" }
