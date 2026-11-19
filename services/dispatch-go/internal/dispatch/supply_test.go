package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

func TestSupplySnapshot(t *testing.T) {
	svc, _, _ := newStreamHarness(t)
	clk := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return clk }
	putDriver(t, svc, "d_1", center, fleet.TierMini)
	putDriver(t, svc, "d_2", geo.Point{Lat: center.Lat + 0.01, Lng: center.Lng}, fleet.TierMini)
	if _, err := svc.Fleet().Upsert(fleet.Driver{ID: "d_off", Lat: center.Lat, Lng: center.Lng, Status: fleet.StatusOffline, Tier: fleet.TierMini}); err != nil {
		t.Fatal(err)
	}
	ride := func() {
		if _, err := svc.CreateRide(context.Background(), CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "mini"}); err != nil {
			t.Fatal(err)
		}
	}
	ride() // old: falls out of a 5 min window
	clk = clk.Add(10 * time.Minute)
	ride()
	ride()
	ride() // no mini left: unmatched

	r := chi.NewRouter()
	svc.Routes(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/supply/snapshot?window_s=300", nil))
	var snap SupplySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if len(snap.Drivers) != 2 || snap.Counts["en_route"] != 2 || snap.WindowS != 300 {
		t.Errorf("drivers %+v counts %v", snap.Drivers, snap.Counts)
	}
	if len(snap.Requests) != 3 || snap.Counts["requests"] != 3 {
		t.Fatalf("requests %+v", snap.Requests)
	}
	matched := 0
	for _, q := range snap.Requests {
		if q.Matched {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("matched = %d, want 1 (only one mini left after the old ride)", matched)
	}
	if got := svc.Snapshot(0); got.WindowS != int(DefaultDemandWin/time.Second) {
		t.Errorf("default window %d", got.WindowS)
	}
}

func TestDemandLogPrunes(t *testing.T) {
	var l demandLog
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	l.add(demandPoint{at: t0})
	l.add(demandPoint{at: t0.Add(demandMemory + time.Second)})
	if len(l.pts) != 1 {
		t.Fatalf("kept %d points, want the old one pruned", len(l.pts))
	}
}
