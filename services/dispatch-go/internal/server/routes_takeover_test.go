package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
	"github.com/hexago/hexago/services/dispatch-go/internal/sim"
)

type configT = config.Config

type nopPublisher struct{}

func (nopPublisher) Publish(string, any) {}

func takeoverRouter(t *testing.T, mutate func(c *configT)) (http.Handler, *sim.Sim) {
	t.Helper()
	reg := prometheus.NewRegistry()
	svc, err := dispatch.New(dispatch.Options{H3Resolution: 9, Center: geo.Point{Lat: 12.9716, Lng: 77.5946},
		Registry: reg, Logger: log.New(io.Discard, "", 0), Publisher: nopPublisher{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sim.New(sim.Options{Service: svc, Publisher: nopPublisher{}, Drivers: 40, Seed: 42,
		Logger: log.New(io.Discard, "", 0), RandomSpikesOff: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Spawn(context.Background())
	cfg := testConfig()
	cfg.TakeoverEnabled = true
	if mutate != nil {
		mutate(cfg)
	}
	return server.NewRouter(cfg, server.WithDispatch(svc, reg), server.WithSim(s)), s
}

func call(t *testing.T, h http.Handler, method, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("")))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTakeoverRoutesFullFlow(t *testing.T) {
	h, s := takeoverRouter(t, nil)
	ctx := context.Background()
	if code, out := call(t, h, "POST", "/v1/drivers/d_001/takeover"); code != 200 || out["human_controlled"] != true {
		t.Fatalf("take over: %d %v", code, out)
	}
	if code, out := call(t, h, "GET", "/v1/drivers/d_001/takeover"); code != 200 || out["ttl_s"].(float64) <= 0 {
		t.Fatalf("view: %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/v1/drivers/d_999/takeover"); code != 404 {
		t.Fatalf("unknown driver: %d", code)
	}
	if code, _ := call(t, h, "POST", "/v1/drivers/d_002/send-ride"); code != 409 {
		t.Fatalf("send to simulated driver: %d", code)
	}
	code, out := call(t, h, "POST", "/v1/drivers/d_001/send-ride")
	if code != 201 {
		t.Fatalf("send ride: %d %v", code, out)
	}
	rideID := out["ride"].(map[string]any)["id"].(string)
	s.Step(ctx)
	_, view := call(t, h, "GET", "/v1/drivers/d_001/takeover")
	if offer, ok := view["offer"].(map[string]any); !ok || offer["ride_id"] != rideID || offer["fare_inr"].(float64) <= 0 {
		t.Fatalf("offer in view: %v", view)
	}
	if code, _ := call(t, h, "POST", "/v1/drivers/d_001/send-ride"); code != 409 {
		t.Fatalf("second send while offered: %d", code)
	}
	if code, _ := call(t, h, "POST", "/v1/rides/"+rideID+"/offer/maybe"); code != 400 {
		t.Fatalf("bad decision: %d", code)
	}
	if code, _ := call(t, h, "POST", "/v1/rides/nope/offer/accept"); code != 404 {
		t.Fatalf("no offer: %d", code)
	}
	code, out = call(t, h, "POST", "/v1/rides/"+rideID+"/offer/decline")
	if code != 200 || out["decision"] != "declined" || out["rematched_to"] == "d_001" {
		t.Fatalf("decline: %d %v", code, out)
	}
	_, out = call(t, h, "POST", "/v1/drivers/d_001/send-ride")
	rideID = out["ride"].(map[string]any)["id"].(string)
	s.Step(ctx)
	if code, out = call(t, h, "POST", "/v1/rides/"+rideID+"/offer/accept"); code != 200 || out["decision"] != "accepted" {
		t.Fatalf("accept: %d %v", code, out)
	}
	if code, out = call(t, h, "DELETE", "/v1/drivers/d_001/takeover"); code != 200 || out["human_controlled"] != false {
		t.Fatalf("release: %d %v", code, out)
	}
}

func TestTakeoverRoutesFlagsAndForecastProxy(t *testing.T) {
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `","horizon":"` + r.URL.Query().Get("horizon") + `"}`))
	}))
	defer agents.Close()
	h, _ := takeoverRouter(t, func(c *configT) { c.TakeoverEnabled = false; c.AgentsURL = agents.URL })
	if code, out := call(t, h, "POST", "/v1/drivers/d_001/takeover"); code != 503 || !strings.Contains(out["error"].(string), "TAKEOVER_ENABLED") {
		t.Fatalf("disabled: %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/v1/rides/x/offer/accept"); code != 503 {
		t.Fatalf("disabled offer: %d", code)
	}
	if code, out := call(t, h, "GET", "/v1/forecast/heatmap?horizon=15"); code != 200 || out["path"] != "/v1/forecast/heatmap" || out["horizon"] != "15" {
		t.Fatalf("forecast proxy: %d %v", code, out)
	}
	h, _ = takeoverRouter(t, func(c *configT) { c.DemoMode = false })
	call(t, h, "POST", "/v1/drivers/d_001/takeover")
	if code, _ := call(t, h, "POST", "/v1/drivers/d_001/send-ride"); code != 403 {
		t.Fatalf("send ride outside DEMO_MODE: %d", code)
	}
	code, out := call(t, h, "GET", "/v1/route?from_lat=12.97&from_lng=77.59&to_lat=12.98&to_lng=77.61")
	if code != 200 || out["source"] != "straight_line" || len(out["points"].([]any)) != 2 || out["distance_km"].(float64) <= 0 {
		t.Fatalf("route: %d %v", code, out)
	}
	if code, _ := call(t, h, "GET", "/v1/route?from_lat=x"); code != 400 {
		t.Fatalf("bad route query: %d", code)
	}
	if code, _ := call(t, h, "GET", "/v1/route?from_lat=99&from_lng=77.59&to_lat=12.98&to_lng=77.61"); code != 400 {
		t.Fatalf("out of range: %d", code)
	}
	// Without the simulator the take-over endpoints answer 503 and routes are straight lines.
	cfg := testConfig()
	cfg.TakeoverEnabled = true
	bare := server.NewRouter(cfg)
	if code, _ := call(t, bare, "DELETE", "/v1/drivers/d_001/takeover"); code != 503 {
		t.Fatalf("no simulator: %d", code)
	}
	if code, out := call(t, bare, "GET", "/v1/route?from_lat=12.97&from_lng=77.59&to_lat=12.98&to_lng=77.61"); code != 200 || out["source"] != "straight_line" {
		t.Fatalf("route without simulator: %d %v", code, out)
	}
}
