package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/ledger"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

type ledgerHarness struct {
	t      *testing.T
	svc    *dispatch.Service
	safety *safety.Service
	led    *ledger.Ledger
	router http.Handler
}

func newLedgerHarness(t *testing.T) *ledgerHarness {
	t.Helper()
	svc, err := dispatch.New(dispatch.Options{H3Resolution: 9, Center: geo.Point{Lat: 12.9716, Lng: 77.5946}, Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	sf := safety.New(safety.Options{Config: safety.DefaultConfig(true), Positions: svc.Fleet()})
	led := ledger.New(ledger.Options{TestMode: true, PayoutDelay: time.Millisecond})
	t.Cleanup(led.Close)
	ledger.Attach(led, svc, sf)
	h := &ledgerHarness{t: t, svc: svc, safety: sf, led: led}
	h.router = server.NewRouter(testConfig(), server.WithDispatch(svc, prometheus.NewRegistry()), server.WithSafety(sf), server.WithLedger(led))
	return h
}

func (h *ledgerHarness) do(method, path, body string) (int, map[string]any) {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ride dispatches a ride to driver and drives it to in_progress.
func (h *ledgerHarness) ride(driver, rider string) string {
	h.t.Helper()
	ctx := context.Background()
	if code, out := h.do("POST", "/v1/drivers/"+driver+"/status", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`); code != 200 {
		h.t.Fatalf("driver: %d %v", code, out)
	}
	p, d := geo.Point{Lat: 12.972, Lng: 77.595}, geo.Point{Lat: 12.99, Lng: 77.61}
	v, err := h.svc.CreateRide(ctx, dispatch.CreateRideInput{RiderID: rider, Pickup: &p, Dropoff: &d, Tier: "sedan"})
	if err != nil || v.Match == nil || v.Ride.DriverID != driver {
		h.t.Fatalf("ride %+v %v", v, err)
	}
	if _, err := h.svc.AdvanceRide(ctx, v.Ride.ID, store.RideInProgress); err != nil {
		h.t.Fatal(err)
	}
	return v.Ride.ID
}

func TestLedgerSettlesCompletedRidesEndToEnd(t *testing.T) {
	h := newLedgerHarness(t)
	id := h.ride("d_001", "r_1")
	if _, err := h.svc.AdvanceRide(context.Background(), id, store.RideCompleted); err != nil {
		t.Fatal(err)
	}
	h.led.Wait()

	code, out := h.do("GET", "/v1/ledger/entries?ride_id="+id, "")
	if code != 200 {
		t.Fatalf("entries %d %v", code, out)
	}
	b := out["breakdown"].(map[string]any)
	fare := b["fare_paise"].(float64)
	sum := b["payee_paise"].(float64) + b["city_levy_paise"].(float64) + b["gst_paise"].(float64) + b["platform_fee_paise"].(float64)
	if fare <= 0 || sum != fare+b["tip_paise"].(float64) || b["payout_status"] != "sent" || len(out["entries"].([]any)) != 2 {
		t.Fatalf("breakdown %v", out)
	}
	code, out = h.do("GET", "/v1/ledger/trial-balance", "")
	if code != 200 || out["total_paise"].(float64) != 0 || out["balanced"] != true {
		t.Fatalf("trial balance %d %v", code, out)
	}
	code, out = h.do("GET", "/v1/ledger/summary", "")
	if code != 200 || out["rides_settled"].(float64) != 1 || out["payouts_today"].(map[string]any)["sent_count"].(float64) != 1 {
		t.Fatalf("summary %d %v", code, out)
	}
	code, out = h.do("GET", "/v1/wallets/d_001", "")
	if code != 200 || len(out["payouts"].([]any)) != 1 {
		t.Fatalf("wallet %d %v", code, out)
	}
	code, out = h.do("GET", "/v1/ledger/payouts?status=sent&limit=5&since=2000-01-01T00:00:00Z", "")
	if code != 200 || len(out["payouts"].([]any)) != 1 || out["store"] != "memory" {
		t.Fatalf("payouts %d %v", code, out)
	}
	code, out = h.do("GET", "/v1/ledger/entries?kind=ride_settlement&limit=10", "")
	if code != 200 || len(out["entries"].([]any)) != 1 {
		t.Fatalf("entries by kind %d %v", code, out)
	}
	// Refund (amount alias in paise), then over-refund is rejected.
	code, out = h.do("POST", "/v1/ledger/refunds", `{"ride_id":"`+id+`","amount":1000,"reason":"late pickup"}`)
	if code != 201 || out["kind"] != "refund" || out["reverses_id"] == "" {
		t.Fatalf("refund %d %v", code, out)
	}
	if code, _ = h.do("POST", "/v1/ledger/refunds", `{"ride_id":"`+id+`","amount_paise":99999999}`); code != 400 {
		t.Fatalf("over-refund %d", code)
	}
	if code, _ = h.do("POST", "/v1/ledger/refunds", `{"ride_id":"missing","amount_paise":100}`); code != 404 {
		t.Fatalf("refund unknown ride %d", code)
	}
	// Top-up (test mode).
	code, out = h.do("POST", "/v1/wallets/r_1/topup", `{"amount_paise":50000}`)
	if code != 201 || out["wallet"].(map[string]any)["balance_paise"].(float64) != 51000 {
		t.Fatalf("topup %d %v", code, out)
	}
	if code, _ = h.do("POST", "/v1/wallets/r_1/topup", `{"amount_paise":-1}`); code != 400 {
		t.Fatalf("bad topup %d", code)
	}
	// Unknown ride: empty view, not an error.
	code, out = h.do("GET", "/v1/ledger/entries?ride_id=nope", "")
	if code != 200 || out["breakdown"] != nil || len(out["entries"].([]any)) != 0 {
		t.Fatalf("unknown ride %d %v", code, out)
	}
	for _, path := range []string{"/v1/ledger/entries?since=yesterday", "/v1/ledger/payouts?limit=ten"} {
		if code, _ = h.do("GET", path, ""); code != 400 {
			t.Fatalf("%s: %d", path, code)
		}
	}
	if code, _ = h.do("POST", "/v1/ledger/refunds", `{`); code != 400 {
		t.Fatalf("bad json %d", code)
	}
	if code, _ = h.do("GET", "/v1/ledger/trial-balance", ""); code != 200 {
		t.Fatal("trial balance after everything")
	}
	_, out = h.do("GET", "/v1/ledger/trial-balance", "")
	if out["total_paise"].(float64) != 0 {
		t.Fatalf("unbalanced: %v", out)
	}
}

func TestSafetyEscalationHoldsPayoutUntilResolved(t *testing.T) {
	h := newLedgerHarness(t)
	id := h.ride("d_002", "r_2")
	// The Safety Sentinel escalates: the driver is suspended (no simulator here,
	// so the interrupted ride is the one in progress, cancelled by the escalation).
	if _, err := h.svc.InterruptRide(context.Background(), id, "safety_incident"); err != nil {
		t.Fatal(err)
	}
	sp, err := h.safety.Suspend(context.Background(), "d_002", "inc_42", "escalated", "safety_sentinel")
	if err != nil {
		t.Fatal(err)
	}
	_ = sp
	// No vehicle controller: the hook only knows the driver; settle the ride via the lookup.
	if _, err := h.led.HoldForIncident(context.Background(), ledger.Hold{IncidentID: "inc_42", DriverID: "d_002", RideID: id}); err != nil {
		t.Fatal(err)
	}
	h.led.Wait()
	code, out := h.do("GET", "/v1/ledger/entries?ride_id="+id, "")
	if code != 200 || out["payout"].(map[string]any)["status"] != "held_for_review" || out["breakdown"].(map[string]any)["interrupted"] != true {
		t.Fatalf("held ride %d %v", code, out)
	}
	_, out = h.do("GET", "/v1/ledger/summary", "")
	if out["payouts_today"].(map[string]any)["held_count"].(float64) != 1 {
		t.Fatalf("summary %v", out)
	}
	// Only the Ops Supervisor's confirmed release sends it.
	if code, _ := h.do("POST", "/v1/safety/drivers/d_002/unsuspend", `{"incident_id":"inc_42","confirmed_by":"safety_sentinel"}`); code != 403 {
		t.Fatalf("sentinel unsuspend %d", code)
	}
	if code, out := h.do("POST", "/v1/safety/drivers/d_002/unsuspend", `{"incident_id":"inc_42","confirmed_by":"ops_supervisor"}`); code != 200 {
		t.Fatalf("unsuspend %d %v", code, out)
	}
	_, out = h.do("GET", "/v1/ledger/entries?ride_id="+id, "")
	if p := out["payout"].(map[string]any); p["status"] != "sent" || p["released_at"] == nil {
		t.Fatalf("released payout %v", p)
	}
	_, out = h.do("GET", "/v1/ledger/trial-balance", "")
	if out["total_paise"].(float64) != 0 {
		t.Fatalf("unbalanced %v", out)
	}
}

func TestRouterBuildsDefaultLedger(t *testing.T) {
	router := server.NewRouter(testConfig())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/ledger/trial-balance", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"balanced":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/wallets/r_x", nil))
	if rec.Code != 200 {
		t.Fatalf("wallet %d", rec.Code)
	}
}
