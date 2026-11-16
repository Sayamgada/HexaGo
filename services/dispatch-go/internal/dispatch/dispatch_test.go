package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

var center = geo.Point{Lat: 12.9716, Lng: 77.5946}

type harness struct {
	svc    *Service
	router http.Handler
	store  *store.Memory
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mem := store.NewMemory()
	svc, err := New(Options{
		H3Resolution: 9,
		Center:       center,
		Store:        mem,
		Telemetry:    mem,
		Registry:     prometheus.NewRegistry(),
		Logger:       log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc.Routes(r)
	return &harness{svc: svc, router: r, store: mem}
}

func (h *harness) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: invalid JSON %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func (h *harness) addDriver(t *testing.T, id, body string) {
	t.Helper()
	code, out := h.do(t, http.MethodPost, "/v1/drivers/"+id+"/status", body)
	if code != http.StatusOK {
		t.Fatalf("add driver %s: %d %v", id, code, out)
	}
}

const rideBody = `{"rider_id":"rider-1","pickup":{"lat":12.9716,"lng":77.5946},"dropoff":{"lat":12.9784,"lng":77.6408},"tier":"sedan"}`

func TestDriverStatusEndpoint(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name   string
		id     string
		body   string
		status int
	}{
		{"create idle sedan", "d1", `{"status":"idle","lat":12.9720,"lng":77.5950,"tier":"sedan","heading":180,"speed_kmh":20,"rating":4.9}`, 200},
		{"position-only update", "d1", `{"lat":12.9721,"lng":77.5951}`, 200},
		{"status-only update", "d1", `{"status":"offline"}`, 200},
		{"new driver without position", "d2", `{"status":"idle","tier":"mini"}`, 400},
		{"new driver without tier", "d2", `{"status":"idle","lat":12.97,"lng":77.59}`, 400},
		{"bad status", "d1", `{"status":"sleeping"}`, 400},
		{"bad tier", "d1", `{"tier":"bike"}`, 400},
		{"bad battery", "d1", `{"battery_pct":150}`, 400},
		{"half a position", "d1", `{"lat":12.97}`, 400},
		{"malformed json", "d1", `{"status":`, 400},
		{"unknown field", "d1", `{"stauts":"idle"}`, 400},
		{"av with battery", "av1", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"av","battery_pct":80}`, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out := h.do(t, http.MethodPost, "/v1/drivers/"+tc.id+"/status", tc.body)
			if code != tc.status {
				t.Fatalf("status = %d, want %d (%v)", code, tc.status, out)
			}
			if code != 200 {
				if out["error"] == "" || out["error"] == nil {
					t.Errorf("error body missing: %v", out)
				}
				return
			}
			d := out["driver"].(map[string]any)
			if d["id"] != tc.id || d["h3_cell"] == "" {
				t.Errorf("driver = %v", d)
			}
		})
	}

	d, ok := h.svc.fleet.Get("d1")
	if !ok || d.Status != fleet.StatusOffline || d.Lat != 12.9721 || d.Tier != fleet.TierSedan || d.Rating != 4.9 {
		t.Errorf("merged driver = %+v", d)
	}
	pts, _ := h.store.RecentTelemetry(context.Background(), "d1", 10)
	if len(pts) != 3 {
		t.Errorf("telemetry points for d1 = %d, want 3 (one per accepted update)", len(pts))
	}
}

func TestCreateRideMatchesAndPersists(t *testing.T) {
	h := newHarness(t)
	h.addDriver(t, "near", `{"status":"idle","lat":12.9725,"lng":77.5946,"tier":"sedan","rating":4.9}`)
	h.addDriver(t, "far", `{"status":"idle","lat":12.9760,"lng":77.5946,"tier":"sedan","rating":4.9}`)
	h.addDriver(t, "auto", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"auto"}`)

	code, out := h.do(t, http.MethodPost, "/v1/rides", rideBody)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %v", code, out)
	}
	ride := out["ride"].(map[string]any)
	if ride["status"] != "matched" || ride["driver_id"] != "near" {
		t.Errorf("ride = %v", ride)
	}
	if legs := ride["legs"].([]any); len(legs) != 2 {
		t.Errorf("legs = %v", legs)
	}
	match := out["match"].(map[string]any)
	if match["driver"].(map[string]any)["id"] != "near" {
		t.Errorf("match.driver = %v", match["driver"])
	}
	score := match["score"].(map[string]any)
	for _, k := range []string{"total", "eta", "momentum", "rating", "idle", "battery", "eta_seconds", "distance_km"} {
		if _, ok := score[k]; !ok {
			t.Errorf("score breakdown missing %q: %v", k, score)
		}
	}
	alts := match["alternatives"].([]any)
	if len(alts) != 1 || alts[0].(map[string]any)["driver_id"] != "far" {
		t.Errorf("alternatives = %v", alts)
	}

	// Driver is now en route and won't be matched again.
	if d, _ := h.svc.fleet.Get("near"); d.Status != fleet.StatusEnRoute {
		t.Errorf("driver status = %v", d.Status)
	}
	id := ride["id"].(string)
	stored, err := h.store.GetRide(context.Background(), id)
	if err != nil || stored.DriverID != "near" {
		t.Errorf("stored ride = %+v, %v", stored, err)
	}
	evs, _ := h.store.RecentEvents(context.Background(), 5)
	if len(evs) != 1 || evs[0].Agent != "dispatch_broker" || evs[0].RideID != id || evs[0].Confidence <= 0 {
		t.Errorf("agent events = %+v", evs)
	}

	code, out = h.do(t, http.MethodGet, "/v1/rides/"+id, "")
	if code != 200 || out["ride"].(map[string]any)["id"] != id {
		t.Errorf("GET ride: %d %v", code, out)
	}
	if code, _ := h.do(t, http.MethodGet, "/v1/rides/missing", ""); code != http.StatusNotFound {
		t.Errorf("GET missing ride: %d", code)
	}

	// Second request gets the remaining sedan.
	_, out = h.do(t, http.MethodPost, "/v1/rides", rideBody)
	if out["ride"].(map[string]any)["driver_id"] != "far" {
		t.Errorf("second ride driver = %v", out["ride"])
	}
}

func TestCreateRideNoDriverStaysOpen(t *testing.T) {
	h := newHarness(t)
	code, out := h.do(t, http.MethodPost, "/v1/rides", rideBody)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d: %v", code, out)
	}
	if out["ride"].(map[string]any)["status"] != "requested" {
		t.Errorf("ride = %v", out["ride"])
	}
	if _, ok := out["match"]; ok {
		t.Errorf("match should be omitted: %v", out["match"])
	}
	evs, _ := h.store.RecentEvents(context.Background(), 5)
	if len(evs) != 1 || evs[0].EventType != "no_match" {
		t.Errorf("events = %+v", evs)
	}
}

func TestCreateRideValidation(t *testing.T) {
	h := newHarness(t)
	tests := []struct{ name, body string }{
		{"missing rider", `{"pickup":{"lat":12.97,"lng":77.59},"dropoff":{"lat":12.98,"lng":77.64},"tier":"sedan"}`},
		{"bad pickup", `{"rider_id":"r","pickup":{"lat":120,"lng":77.59},"dropoff":{"lat":12.98,"lng":77.64},"tier":"sedan"}`},
		{"missing dropoff", `{"rider_id":"r","pickup":{"lat":12.97,"lng":77.59},"tier":"sedan"}`},
		{"bad tier", `{"rider_id":"r","pickup":{"lat":12.97,"lng":77.59},"dropoff":{"lat":12.98,"lng":77.64},"tier":"rickshaw"}`},
		{"not json", `hello`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := h.do(t, http.MethodPost, "/v1/rides", tc.body); code != http.StatusBadRequest {
				t.Errorf("status = %d: %v", code, out)
			}
		})
	}
}

func TestCancelRide(t *testing.T) {
	h := newHarness(t)
	h.addDriver(t, "d1", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)
	_, out := h.do(t, http.MethodPost, "/v1/rides", rideBody)
	id := out["ride"].(map[string]any)["id"].(string)

	code, out := h.do(t, http.MethodPost, "/v1/rides/"+id+"/cancel", `{"reason":"changed plans"}`)
	if code != 200 {
		t.Fatalf("cancel: %d %v", code, out)
	}
	ride := out["ride"].(map[string]any)
	if ride["status"] != "cancelled" || ride["cancel_reason"] != "changed plans" {
		t.Errorf("ride = %v", ride)
	}
	if d, _ := h.svc.fleet.Get("d1"); d.Status != fleet.StatusIdle {
		t.Errorf("driver not released: %v", d.Status)
	}
	if code, _ := h.do(t, http.MethodPost, "/v1/rides/"+id+"/cancel", ""); code != http.StatusConflict {
		t.Errorf("double cancel: %d, want 409", code)
	}
	if code, _ := h.do(t, http.MethodPost, "/v1/rides/missing/cancel", ""); code != http.StatusNotFound {
		t.Errorf("cancel missing: %d", code)
	}
	if code, _ := h.do(t, http.MethodPost, "/v1/rides/"+id+"/cancel", `{"reason":`); code != http.StatusBadRequest {
		t.Errorf("cancel bad json: %d", code)
	}

	// An open (unmatched) request can be cancelled with no body; default reason applies.
	h2 := newHarness(t)
	_, out = h2.do(t, http.MethodPost, "/v1/rides", rideBody)
	id2 := out["ride"].(map[string]any)["id"].(string)
	code, out = h2.do(t, http.MethodPost, "/v1/rides/"+id2+"/cancel", "")
	if code != 200 || out["ride"].(map[string]any)["cancel_reason"] != "rider_cancelled" {
		t.Errorf("cancel open ride: %d %v", code, out)
	}
}

func TestRideLifecycleFollowsDriverStatus(t *testing.T) {
	h := newHarness(t)
	h.addDriver(t, "d1", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)
	_, out := h.do(t, http.MethodPost, "/v1/rides", rideBody)
	id := out["ride"].(map[string]any)["id"].(string)
	get := func() store.RideStatus {
		r, _ := h.store.GetRide(context.Background(), id)
		return r.Status
	}

	h.addDriver(t, "d1", `{"status":"on_trip"}`)
	if s := get(); s != store.RideInProgress {
		t.Fatalf("after on_trip: %v", s)
	}
	if code, _ := h.do(t, http.MethodPost, "/v1/rides/"+id+"/cancel", ""); code != http.StatusConflict {
		t.Errorf("cancelling an in-progress ride: %d, want 409", code)
	}
	h.addDriver(t, "d1", `{"status":"idle"}`)
	if s := get(); s != store.RideCompleted {
		t.Fatalf("after drop-off: %v", s)
	}

	// A matched driver who goes offline puts the ride back in the open queue.
	_, out = h.do(t, http.MethodPost, "/v1/rides", rideBody)
	id = out["ride"].(map[string]any)["id"].(string)
	h.addDriver(t, "d1", `{"status":"offline"}`)
	r, _ := h.store.GetRide(context.Background(), id)
	if r.Status != store.RideRequested || r.DriverID != "" {
		t.Errorf("abandoned ride = %+v", r)
	}
}

func TestDispatchMetricsShape(t *testing.T) {
	h := newHarness(t)
	h.addDriver(t, "d1", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)
	h.addDriver(t, "d2", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"mini"}`)
	h.addDriver(t, "d3", `{"status":"offline","lat":12.9716,"lng":77.5946,"tier":"mini"}`)
	h.do(t, http.MethodPost, "/v1/rides", rideBody) // matched -> d1
	h.do(t, http.MethodPost, "/v1/rides", rideBody) // no sedan left -> open

	req := httptest.NewRequest(http.MethodGet, "/v1/metrics/dispatch", nil)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	// Exactly these keys, in this order.
	body := rec.Body.Bytes()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var keys []string
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		t.Fatalf("not an object: %s", body)
	}
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var v json.Number
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("value for %v is not a number: %v", k, err)
		}
	}
	want := []string{"p50_ms", "p95_ms", "p99_ms", "matches_total", "active_drivers", "idle_drivers", "open_requests", "rides_in_progress"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	// Latencies are always rendered as floats, counts as integers.
	for _, k := range []string{`"p50_ms":`, `"p95_ms":`, `"p99_ms":`} {
		i := bytes.Index(body, []byte(k))
		rest := body[i+len(k):]
		end := bytes.IndexAny(rest, ",}")
		if !bytes.Contains(rest[:end], []byte(".")) {
			t.Errorf("%s not rendered as float: %s", k, rest[:end])
		}
	}

	var m DispatchMetrics
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.MatchesTotal != 1 || m.ActiveDrivers != 2 || m.IdleDrivers != 1 || m.OpenRequests != 1 || m.RidesInProgress != 1 {
		t.Errorf("metrics = %+v", m)
	}
	if m.P50Ms < 0 || m.P95Ms < m.P50Ms || m.P99Ms < m.P95Ms { // sub-ms matches may read 0 on Windows
		t.Errorf("latencies = %v/%v/%v", m.P50Ms, m.P95Ms, m.P99Ms)
	}
}

func TestConcurrentRidesNeverShareADriver(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.addDriver(t, "d"+string(rune('a'+i)), `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var drivers []string
	open := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ride, err := h.svc.CreateRide(context.Background(), CreateRideInput{
				RiderID: "r", Pickup: &center, Dropoff: &geo.Point{Lat: 12.98, Lng: 77.64}, Tier: "sedan",
			})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if ride.Ride.DriverID == "" {
				open++
			} else {
				drivers = append(drivers, ride.Ride.DriverID)
			}
		}()
	}
	wg.Wait()
	sort.Strings(drivers)
	for i := 1; i < len(drivers); i++ {
		if drivers[i] == drivers[i-1] {
			t.Fatalf("driver %s assigned twice: %v", drivers[i], drivers)
		}
	}
	if len(drivers) != 5 || open != 7 {
		t.Errorf("assigned %d, open %d", len(drivers), open)
	}
}

type failingStore struct {
	*store.Memory
	failCreate bool
}

func (f *failingStore) CreateRide(ctx context.Context, r store.Ride) error {
	if f.failCreate {
		return errors.New("db down")
	}
	return f.Memory.CreateRide(ctx, r)
}

func (f *failingStore) RecordEvent(context.Context, store.AgentEvent) error {
	return errors.New("db down")
}

func TestPersistFailureReleasesDriver(t *testing.T) {
	mem := store.NewMemory()
	fs := &failingStore{Memory: mem, failCreate: true}
	var logs bytes.Buffer
	svc, err := New(Options{H3Resolution: 9, Center: center, Store: fs, Telemetry: mem, Logger: log.New(&logs, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc.Routes(r)
	h := &harness{svc: svc, router: r, store: mem}
	h.addDriver(t, "d1", `{"status":"idle","lat":12.9716,"lng":77.5946,"tier":"sedan"}`)

	code, _ := h.do(t, http.MethodPost, "/v1/rides", rideBody)
	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", code)
	}
	if d, _ := svc.fleet.Get("d1"); d.Status != fleet.StatusIdle {
		t.Errorf("driver not released after persist failure: %v", d.Status)
	}

	// Event logging failures are logged, never fatal.
	fs.failCreate = false
	if code, _ := h.do(t, http.MethodPost, "/v1/rides", rideBody); code != http.StatusCreated {
		t.Errorf("status = %d, want 201", code)
	}
	if !strings.Contains(logs.String(), "agent_events") {
		t.Errorf("expected agent_events warning in logs: %q", logs.String())
	}
}

func TestNewDefaultsAndErrors(t *testing.T) {
	svc, err := New(Options{})
	if err != nil {
		t.Fatalf("New with zero options: %v", err)
	}
	if svc.StoreKind() != "memory" || svc.Geo().Resolution() != geo.DefaultResolution {
		t.Errorf("defaults: store=%s res=%d", svc.StoreKind(), svc.Geo().Resolution())
	}
	if svc.Fleet() == nil || svc.MirrorKind() != "none" {
		t.Errorf("fleet=%v mirror=%s", svc.Fleet(), svc.MirrorKind())
	}
	if _, err := New(Options{H3Resolution: 42}); err == nil {
		t.Error("expected invalid resolution error")
	}
}
