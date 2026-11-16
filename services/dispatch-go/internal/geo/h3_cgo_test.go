//go:build cgo

package geo

import (
	"errors"
	"testing"
)

func TestH3Contract(t *testing.T) {
	for _, res := range []int{7, 9, 12} {
		h, err := NewH3(res)
		if err != nil {
			t.Fatal(err)
		}
		indexerContract(t, h, res)
	}
}

func TestH3KnownCell(t *testing.T) {
	h, _ := NewH3(9)
	c, err := h.CellOf(bengaluru)
	if err != nil {
		t.Fatal(err)
	}
	// H3 res-9 cells are 15 hex digits starting with "89".
	if s := c.String(); len(s) != 15 || s[:2] != "89" {
		t.Errorf("unexpected H3 cell %q", s)
	}
}

func TestH3Errors(t *testing.T) {
	if _, err := NewH3(16); !errors.Is(err, ErrInvalidResolution) {
		t.Errorf("NewH3(16) err = %v", err)
	}
	h, _ := NewH3(9)
	if _, err := h.Rings(Cell(0), 1); err == nil {
		t.Error("Rings on invalid cell should fail")
	}
	if Backend() != "h3" {
		t.Errorf("Backend() = %q, want h3", Backend())
	}
}
