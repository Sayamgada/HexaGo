package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/apidocs"
	"github.com/hexago/hexago/services/dispatch-go/internal/developer"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/ledger"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

func createKey(t *testing.T, base, body string) string {
	t.Helper()
	resp, err := http.Post(base+"/v1/developer/keys", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 201 {
		t.Fatalf("create key: %d %v", resp.StatusCode, out)
	}
	return out["api_key"].(string)
}

// TestEveryRouteIsDocumented: each route the gateway serves has an operation
// in the OpenAPI document, and each operation the gateway itself serves (not
// proxied to agents-py) is a real route.
func TestEveryRouteIsDocumented(t *testing.T) {
	router := server.NewRouter(testConfig())
	doc := apidocs.Spec()
	paths := doc["paths"].(apidocs.M)
	served := map[string]bool{}
	methods := map[string][]string{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasSuffix(route, "/*") || route == "/docs/" { // proxies and the trailing-slash alias
			return nil
		}
		methods[route] = append(methods[route], strings.ToLower(method))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for route, ms := range methods {
		// Handle()-mounted routes (metrics, stream, the /v1/agents proxy) answer
		// every method; GET is the documented one.
		if len(ms) > 5 {
			ms = []string{"get"}
		}
		item, ok := paths[route].(apidocs.M)
		if !ok {
			t.Errorf("route %s is not documented", route)
			continue
		}
		for _, m := range ms {
			served[m+" "+route] = true
			if _, ok := item[m]; !ok {
				t.Errorf("method %s %s is not documented", m, route)
			}
		}
	}
	// Legal & Jurisdiction and the Ops report are proxied to agents-py too (routes_scheduler.go).
	for p, item := range paths {
		if strings.HasPrefix(p, "/v1/legal/") || strings.HasPrefix(p, "/v1/ops/") {
			for method, o := range item.(apidocs.M) {
				if o.(apidocs.M)["x-served-by"] != "agents-py" {
					t.Errorf("%s %s should be served by agents-py", method, p)
				}
			}
			delete(paths, p)
		}
	}
	for p, item := range paths {
		for method, o := range item.(apidocs.M) {
			if o.(apidocs.M)["x-served-by"] == "agents-py" {
				if !strings.HasPrefix(p, "/v1/agents") && !strings.HasPrefix(p, "/v1/pricing") && !strings.HasPrefix(p, "/v1/safety") &&
					!strings.HasPrefix(p, "/v1/forecast") {
					t.Errorf("%s %s is marked agents-py but the gateway does not proxy it", method, p)
				}
				continue
			}
			if !served[method+" "+p] {
				t.Errorf("documented %s %s is not served by dispatch-go", method, p)
			}
		}
	}
}

type contract struct {
	t      *testing.T
	v      *apidocs.Validator
	router http.Handler
	key    string
}

// call performs a request and validates the response against the spec
// operation at template.
func (c *contract) call(method, path, template, body string, want int) map[string]any {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	if rec.Code != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	if err := c.v.ValidateResponse(method, template, rec.Code, rec.Body.Bytes()); err != nil {
		c.t.Errorf("contract: %v\nbody: %s", err, truncate(rec.Body.String()))
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func truncate(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

// TestResponsesMatchTheSpec drives the main dispatch-go endpoints with an
// API key and validates every response body against the OpenAPI document.
func TestResponsesMatchTheSpec(t *testing.T) {
	h := newLedgerHarness(t)
	c := &contract{t: t, v: apidocs.NewValidator(), router: h.router}

	// Developer platform.
	created := c.call("POST", "/v1/developer/keys", "/v1/developer/keys", `{"name":"contract"}`, 201)
	c.key = created["api_key"].(string)
	c.call("GET", "/v1/developer/scopes", "/v1/developer/scopes", "", 200)
	c.call("GET", "/v1/developer/keys/me", "/v1/developer/keys/me", "", 200)
	wh := c.call("POST", "/v1/developer/webhooks", "/v1/developer/webhooks", `{"url":"https://hooks.example.com/h","events":["ride.completed"]}`, 201)
	c.call("GET", "/v1/developer/webhooks", "/v1/developer/webhooks", "", 200)
	c.call("GET", "/v1/developer/webhooks/"+wh["id"].(string)+"/deliveries", "/v1/developer/webhooks/{id}/deliveries", "", 200)
	c.call("POST", "/v1/developer/webhooks", "/v1/developer/webhooks", `{"url":"ftp://x","events":["ride.completed"]}`, 400)

	// System.
	c.call("GET", "/healthz", "/healthz", "", 200)
	c.call("GET", "/v1/ping", "/v1/ping", "", 200)
	c.call("GET", "/openapi.json", "/openapi.json", "", 200)

	// Rides.
	c.call("POST", "/v1/drivers/d_9/status", "/v1/drivers/{id}/status", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"mini"}`, 200)
	ride := c.call("POST", "/v1/rides", "/v1/rides", `{"rider_id":"r_9","pickup":{"lat":12.972,"lng":77.595},"dropoff":{"lat":12.99,"lng":77.61},"tier":"mini"}`, 201)
	id := ride["ride"].(map[string]any)["id"].(string)
	c.call("GET", "/v1/rides/"+id, "/v1/rides/{id}", "", 200)
	c.call("GET", "/v1/rides/nope", "/v1/rides/{id}", "", 404)
	c.call("POST", "/v1/rides", "/v1/rides", `{"rider_id":"r_10","pickup":{"lat":12.972,"lng":77.595},"dropoff":{"lat":12.99,"lng":77.61},"tier":"mini"}`, 202)
	c.call("POST", "/v1/rides", "/v1/rides", `{"rider_id":""}`, 400)
	c.call("GET", "/v1/metrics/dispatch", "/v1/metrics/dispatch", "", 200)
	c.call("GET", "/v1/eta?from_lat=12.97&from_lng=77.59&to_lat=12.93&to_lng=77.62", "/v1/eta", "", 200)
	c.call("GET", "/v1/supply?lat=12.97&lng=77.59", "/v1/supply", "", 200)
	c.call("GET", "/v1/scheduled", "/v1/scheduled", "", 200)
	c.call("GET", "/v1/supply/snapshot", "/v1/supply/snapshot", "", 200)
	c.call("GET", "/v1/metrics/counters", "/v1/metrics/counters", "", 200)

	// Ledger: complete the ride so there is money to look at.
	if _, err := h.svc.AdvanceRide(context.Background(), id, store.RideInProgress); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AdvanceRide(context.Background(), id, store.RideCompleted); err != nil {
		t.Fatal(err)
	}
	h.led.Wait()
	c.call("GET", "/v1/ledger/entries?ride_id="+id, "/v1/ledger/entries", "", 200)
	c.call("GET", "/v1/ledger/entries?limit=5", "/v1/ledger/entries", "", 200)
	c.call("GET", "/v1/ledger/trial-balance", "/v1/ledger/trial-balance", "", 200)
	c.call("GET", "/v1/ledger/summary", "/v1/ledger/summary", "", 200)
	c.call("GET", "/v1/ledger/payouts", "/v1/ledger/payouts", "", 200)
	c.call("GET", "/v1/wallets/d_9", "/v1/wallets/{owner}", "", 200)
	c.call("POST", "/v1/wallets/r_9/topup", "/v1/wallets/{owner}/topup", `{"amount_paise":10000}`, 201)
	c.call("POST", "/v1/ledger/refunds", "/v1/ledger/refunds", `{"ride_id":"`+id+`","amount_paise":100,"reason":"contract"}`, 201)
	c.call("POST", "/v1/rides/"+id+"/cancel", "/v1/rides/{id}/cancel", `{}`, 409)

	// Safety (dispatch-go side).
	c.call("GET", "/v1/safety/signals", "/v1/safety/signals", "", 200)
	c.call("GET", "/v1/safety/drivers/d_9", "/v1/safety/drivers/{id}", "", 200)
	c.call("GET", "/v1/safety/emergency/log", "/v1/safety/emergency/log", "", 200)

	// Simulator off: 503 with an error body.
	c.call("GET", "/v1/sim/config", "/v1/sim/config", "", 503)

	// Auth failures use the documented component responses.
	c.key = "hxg_" + strings.Repeat("f", 48)
	c.call("GET", "/v1/rides/"+id, "/v1/rides/{id}", "", 401)
}

func TestScopeAndRateLimitResponses(t *testing.T) {
	srv := httptest.NewServer(server.NewRouter(testConfig()))
	defer srv.Close()
	v := apidocs.NewValidator()
	key := createKey(t, srv.URL, `{"scopes":["rides:read"],"rate_limit_per_min":1}`)
	get := func(path string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, body := get("/v1/ledger/summary")
	if resp.StatusCode != 403 {
		t.Fatalf("scope: %d %s", resp.StatusCode, body)
	}
	if err := v.ValidateResponse("GET", "/v1/ledger/summary", 403, body); err != nil {
		t.Error(err)
	}
	// The 403 did not spend the token (scope is checked first); this call does.
	resp, _ = get("/v1/metrics/dispatch")
	if resp.StatusCode != 200 || resp.Header.Get("X-RateLimit-Limit") != "1" || resp.Header.Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("rate headers %d %v", resp.StatusCode, resp.Header)
	}
	resp, body = get("/v1/metrics/dispatch")
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %v", resp.StatusCode, resp.Header)
	}
	if err := v.ValidateResponse("GET", "/v1/metrics/dispatch", 429, body); err != nil {
		t.Error(err)
	}
}

func TestDocsAndHealthComponents(t *testing.T) {
	router := server.NewRouter(testConfig())
	for _, p := range []string{"/docs", "/docs/"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "swagger-ui") {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(rec.Body.String(), `"api_keys":"memory"`) || !strings.Contains(rec.Body.String(), `"rate_limiter":"memory"`) {
		t.Fatalf("healthz: %s", rec.Body)
	}
}

// ---- webhooks wired to platform events ----

type hookSink struct {
	mu     sync.Mutex
	events map[string][]map[string]any
	got    chan string
}

func newHookSink(t *testing.T) (*hookSink, *httptest.Server) {
	s := &hookSink{events: map[string][]map[string]any{}, got: make(chan string, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ev)
		typ, _ := ev["type"].(string)
		s.mu.Lock()
		s.events[typ] = append(s.events[typ], ev)
		s.mu.Unlock()
		w.WriteHeader(204)
		s.got <- typ
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *hookSink) wait(t *testing.T, typ string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		if evs := s.events[typ]; len(evs) > 0 {
			s.mu.Unlock()
			return evs[0]
		}
		s.mu.Unlock()
		select {
		case <-s.got:
		case <-deadline:
			t.Fatalf("no %s delivery", typ)
		}
	}
}

func TestWebhooksFireOnPlatformEvents(t *testing.T) {
	sink, target := newHookSink(t)

	// Safety Sentinel stub: every signal opens an incident.
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"action":"check_in","incident_id":"inc_7","checkin_id":"chk_1","severity":0.7,"reason":"hard brake"}`))
	}))
	defer agents.Close()

	hub := stream.NewHub(stream.HubOptions{})
	svc, err := dispatch.New(dispatch.Options{H3Resolution: 9, Center: geo.Point{Lat: 12.9716, Lng: 77.5946},
		Registry: prometheus.NewRegistry(), Publisher: hub})
	if err != nil {
		t.Fatal(err)
	}
	sf := safety.New(safety.Options{Config: safety.DefaultConfig(true), Positions: svc.Fleet(), AgentsURL: agents.URL})
	led := ledger.New(ledger.Options{TestMode: true, PayoutDelay: time.Millisecond, Publisher: hub})
	t.Cleanup(led.Close)
	ledger.Attach(led, svc, sf)
	dev := developer.New(developer.Options{Webhooks: developer.DispatcherOptions{AllowPrivate: true, Backoff: time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)
	router := server.NewRouter(testConfig(), server.WithDispatch(svc, prometheus.NewRegistry()), server.WithStream(hub),
		server.WithSafety(sf), server.WithLedger(led), server.WithDeveloper(dev))
	srv := httptest.NewServer(router)
	defer srv.Close()

	key := createKey(t, srv.URL, "")
	req, _ := http.NewRequest("POST", srv.URL+"/v1/developer/webhooks",
		strings.NewReader(`{"url":"`+target.URL+`","events":["ride.completed","payout.sent","incident.opened"]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("webhook: %v %v", err, resp)
	}
	resp.Body.Close()

	h := &ledgerHarness{t: t, svc: svc, safety: sf, led: led, router: router}
	id := h.ride("d_w", "r_w")
	if _, err := svc.AdvanceRide(context.Background(), id, store.RideCompleted); err != nil {
		t.Fatal(err)
	}
	ev := sink.wait(t, developer.EventRideCompleted)
	if ev["data"].(map[string]any)["id"] != id || ev["data"].(map[string]any)["status"] != "completed" {
		t.Fatalf("ride.completed %v", ev)
	}
	ev = sink.wait(t, developer.EventPayoutSent)
	if p := ev["data"].(map[string]any); p["ride_id"] != id || p["status"] != "sent" || p["amount_paise"].(float64) <= 0 {
		t.Fatalf("payout.sent %v", ev)
	}

	sf.Emit(context.Background(), safety.Signal{ID: "sig_1", RideID: id, DriverID: "d_w", Severity: 0.7})
	sf.Wait()
	ev = sink.wait(t, developer.EventIncidentOpened)
	if d := ev["data"].(map[string]any); d["incident_id"] != "inc_7" || d["ride_id"] != id || d["action"] != "check_in" {
		t.Fatalf("incident.opened %v", ev)
	}
}

func TestPayoutWebhookWithoutLedgerDetails(t *testing.T) {
	// A payout event whose ride the ledger does not know still delivers the summary.
	sink, target := newHookSink(t)
	hub := stream.NewHub(stream.HubOptions{})
	dev := developer.New(developer.Options{Webhooks: developer.DispatcherOptions{AllowPrivate: true}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dev.Run(ctx)
	srv := httptest.NewServer(server.NewRouter(testConfig(), server.WithStream(hub), server.WithDeveloper(dev)))
	defer srv.Close()
	key := createKey(t, srv.URL, "")
	req, _ := http.NewRequest("POST", srv.URL+"/v1/developer/webhooks", strings.NewReader(`{"url":"`+target.URL+`","events":["payout.sent"]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	ev := stream.NewAgentEvent("settlement", "payout ₹10 sent", "", 1, 0, false, time.Now())
	ev.Kind, ev.RideID, ev.DriverID = ledger.EvPayoutSent, "ghost", "d_1"
	hub.Publish(stream.TopicOps, ev)
	hub.Publish(stream.TopicOps, stream.NewAgentEvent("dispatch_broker", "matched", "", 1, 0, false, time.Now())) // ignored
	got := sink.wait(t, developer.EventPayoutSent)
	if d := got["data"].(map[string]any); d["ride_id"] != "ghost" || d["driver_id"] != "d_1" {
		t.Fatalf("payout summary %v", got)
	}
}
