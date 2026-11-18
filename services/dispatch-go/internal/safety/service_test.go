package safety

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

var quiet = log.New(io.Discard, "", 0)

type fakeRedis struct {
	mu   sync.Mutex
	msgs []string
	err  error
}

func (f *fakeRedis) Publish(_ context.Context, ch, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, ch+"|"+msg)
	return nil
}

type fakeStream struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeStream) Publish(topic string, data any) {
	b, _ := json.Marshal(data)
	f.mu.Lock()
	f.msgs = append(f.msgs, topic+"|"+string(b))
	f.mu.Unlock()
}

type fakePositions map[string]fleet.Driver

func (f fakePositions) Get(id string) (fleet.Driver, bool) { d, ok := f[id]; return d, ok }

type fakeVehicles struct {
	suspended map[string]bool
	err       error
}

func (f *fakeVehicles) Suspend(_ context.Context, id, _ string) (SuspendResult, error) {
	if f.err != nil {
		return SuspendResult{}, f.err
	}
	f.suspended[id] = true
	return SuspendResult{DriverID: id, Tier: "av", VehicleStopped: true, InterruptedRideID: "ride1", ReplacementRideID: "ride2"}, nil
}

func (f *fakeVehicles) Unsuspend(_ context.Context, id string) error {
	if !f.suspended[id] {
		return ErrUnknownDriver
	}
	delete(f.suspended, id)
	return nil
}

func sampleSignal() Signal {
	return Signal{Type: "safety_signal", ID: "sig_1", RideID: "ride1", DriverID: "d_001", Severity: 0.46,
		Triggers: []Trigger{{Type: TriggerDeviation, Value: 420, Unit: "m", DurationS: 10, Severity: 0.46}}}
}

func TestEmitPostsToAgentsAndRedisAndRecordsEvent(t *testing.T) {
	var got atomic.Value
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/safety/signal" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		got.Store(string(b))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer agents.Close()
	rd := &fakeRedis{}
	var events []dispatch.AgentEventInput
	s := New(Options{AgentsURL: agents.URL + "/", Redis: rd, Logger: quiet,
		Events: func(_ context.Context, e dispatch.AgentEventInput) { events = append(events, e) }})
	s.Emit(context.Background(), sampleSignal())
	s.Wait()

	body, _ := got.Load().(string)
	var sig Signal
	if err := json.Unmarshal([]byte(body), &sig); err != nil || sig.ID != "sig_1" || sig.Severity != 0.46 {
		t.Fatalf("posted %q: %v", body, err)
	}
	if len(rd.msgs) != 1 || !strings.HasPrefix(rd.msgs[0], "safety|") {
		t.Fatalf("redis %v", rd.msgs)
	}
	if len(events) != 1 || events[0].Agent != "safety_sentinel" || events[0].Kind != "incident" ||
		!strings.Contains(events[0].Decision, "route deviation 420 m") || events[0].UsedFallback {
		t.Fatalf("events %+v", events)
	}
	if p, f := s.Delivery(); p != 1 || f != 0 {
		t.Fatalf("delivery %d/%d", p, f)
	}
	if sigs := s.Signals(); len(sigs) != 1 {
		t.Fatalf("signals %v", sigs)
	}
}

func TestEmitWorksWithoutRedisAndRetriesOnce(t *testing.T) {
	var calls atomic.Int32
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // cold start
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer agents.Close()
	s := New(Options{AgentsURL: agents.URL, Logger: quiet})
	s.Emit(context.Background(), sampleSignal())
	s.Wait()
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
	}
	if p, _ := s.Delivery(); p != 1 {
		t.Fatal("not delivered on retry")
	}
}

func TestEmitCountsFailuresAndRedisErrors(t *testing.T) {
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer agents.Close()
	s := New(Options{AgentsURL: agents.URL, Redis: &fakeRedis{err: errors.New("down")}, Logger: quiet})
	s.Emit(context.Background(), sampleSignal())
	s.Wait()
	if _, f := s.Delivery(); f != 1 {
		t.Fatalf("failures %d", f)
	}
	// Unreachable host and a malformed URL also count as failures, never panics.
	for _, u := range []string{"http://127.0.0.1:1", "http://bad host"} {
		s2 := New(Options{AgentsURL: u, Logger: quiet})
		s2.Emit(context.Background(), sampleSignal())
		s2.Wait()
		if _, f := s2.Delivery(); f != 1 {
			t.Fatalf("%s failures %d", u, f)
		}
	}
	// Cancelled context during the retry wait: gives up quietly.
	ctx, cancel := context.WithCancel(context.Background())
	s3 := New(Options{AgentsURL: agents.URL, Logger: quiet})
	cancel()
	s3.post(ctx, "x", []byte("{}"))
	// No agents URL: nothing is posted.
	s4 := New(Options{})
	s4.Emit(context.Background(), sampleSignal())
	s4.Wait()
	if p, f := s4.Delivery(); p != 0 || f != 0 {
		t.Fatal("posted without AGENTS_URL")
	}
}

func TestSignalsAreCappedNewestFirst(t *testing.T) {
	s := New(Options{Logger: quiet})
	for i := 0; i < recentSignals+5; i++ {
		sig := sampleSignal()
		sig.ID = time.Duration(i).String()
		s.Emit(context.Background(), sig)
	}
	s.Wait()
	sigs := s.Signals()
	if len(sigs) != recentSignals || sigs[0].ID != time.Duration(recentSignals+4).String() {
		t.Fatalf("%d signals, newest %s", len(sigs), sigs[0].ID)
	}
}

func TestObserveTickShipsSignalsAndSweeps(t *testing.T) {
	s := New(Options{Config: DefaultConfig(true), Logger: quiet})
	for i := 0; i <= 10; i++ {
		s.ObserveTick(context.Background(), []Observation{obs("r", at(east(1), 0.5), 30, float64(i))})
	}
	s.ObserveTick(context.Background(), nil)
	s.Wait()
	if len(s.Signals()) != 1 {
		t.Fatalf("signals %d", len(s.Signals()))
	}
	s.RideEnded("r")
	if s.Detector().Tracked() != 0 {
		t.Fatal("ride not forgotten")
	}
}

func TestSuspendGuardrails(t *testing.T) {
	veh := &fakeVehicles{suspended: map[string]bool{}}
	s := New(Options{Logger: quiet})
	s.SetVehicles(veh)
	ctx := context.Background()
	if _, err := s.Suspend(ctx, " ", "", "", ""); !errors.Is(err, ErrUnknownDriver) {
		t.Fatal("empty id accepted")
	}
	sp, err := s.Suspend(ctx, "d_007", "inc_1", "escalated", "safety_sentinel")
	if err != nil || sp.Result.ReplacementRideID != "ride2" || sp.IncidentID != "inc_1" || !veh.suspended["d_007"] {
		t.Fatalf("%+v %v", sp, err)
	}
	again, _ := s.Suspend(ctx, "d_007", "inc_2", "again", "safety_sentinel")
	if again.IncidentID != "inc_1" {
		t.Fatal("second suspend was not idempotent")
	}
	// The Safety agent itself (or anyone but the supervisor) cannot unfreeze.
	for _, who := range []string{"", "safety_sentinel", "rider"} {
		if err := s.Unsuspend(ctx, "d_007", who); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%q unfroze the driver: %v", who, err)
		}
	}
	if _, ok := s.Suspended("d_007"); !ok {
		t.Fatal("suspension lost")
	}
	if err := s.Unsuspend(ctx, "d_007", ConfirmingAgent); err != nil {
		t.Fatal(err)
	}
	if err := s.Unsuspend(ctx, "d_007", ConfirmingAgent); !errors.Is(err, ErrNotSuspended) {
		t.Fatalf("double unsuspend: %v", err)
	}
	veh.err = errors.New("boom")
	if _, err := s.Suspend(ctx, "d_008", "", "", ""); err == nil {
		t.Fatal("controller error swallowed")
	}
}

func TestSuspendWithoutControllerUsesPositions(t *testing.T) {
	s := New(Options{Logger: quiet, Positions: fakePositions{"d_1": {ID: "d_1", Tier: fleet.TierAV, Lat: 1, Lng: 2}}})
	sp, err := s.Suspend(context.Background(), "d_1", "", "", "")
	if err != nil || sp.Result.Tier != "av" || sp.Result.Location != (geo.Point{Lat: 1, Lng: 2}) || sp.Result.VehicleStopped {
		t.Fatalf("%+v %v", sp, err)
	}
	if _, err := s.Suspend(context.Background(), "nobody", "", "", ""); !errors.Is(err, ErrUnknownDriver) {
		t.Fatalf("unknown: %v", err)
	}
	// Unsuspend tolerates a controller that no longer knows the driver.
	veh := &fakeVehicles{suspended: map[string]bool{}}
	s.SetVehicles(veh)
	if err := s.Unsuspend(context.Background(), "d_1", ConfirmingAgent); err != nil {
		t.Fatal(err)
	}
}

func TestTrackLinks(t *testing.T) {
	now := t0
	s := New(Options{Logger: quiet, Now: func() time.Time { return now },
		Positions: fakePositions{"d_1": {ID: "d_1", Lat: 12.9, Lng: 77.6, SpeedKmh: 20, Status: fleet.StatusOnTrip}}})
	if _, err := s.TrackLink("", "", ""); !errors.Is(err, ErrUnknownDriver) {
		t.Fatal("empty driver")
	}
	l, err := s.TrackLink("d_1", "ride1", "inc_1")
	if err != nil || len(l.Token) != 16 {
		t.Fatalf("%+v %v", l, err)
	}
	v, ok := s.Track(l.Token)
	if !ok || v.Lat != 12.9 || v.Status != "on_trip" || v.IncidentID != "inc_1" || v.Suspended {
		t.Fatalf("%+v", v)
	}
	_, _ = s.Suspend(context.Background(), "d_1", "", "", "")
	if v, _ := s.Track(l.Token); v.Status != "suspended" || !v.Suspended {
		t.Fatalf("suspended view %+v", v)
	}
	if _, ok := s.Track("nope"); ok {
		t.Fatal("unknown token resolved")
	}
	old := l.Token
	now = now.Add(trackTTL + time.Second)
	if _, ok := s.Track(old); ok {
		t.Fatal("expired token resolved")
	}
	_, _ = s.TrackLink("d_1", "", "") // sweeps expired links
	s.mu.Lock()
	_, still := s.tracks[old]
	s.mu.Unlock()
	if still {
		t.Fatal("expired link not swept")
	}
	// Without positions the view still resolves with an unknown status.
	s2 := New(Options{Logger: quiet})
	l2, _ := s2.TrackLink("d_9", "", "")
	if v, ok := s2.Track(l2.Token); !ok || v.Status != "unknown" {
		t.Fatalf("%+v", v)
	}
}

func TestDescribeAndExplain(t *testing.T) {
	ts := []Trigger{
		{Type: TriggerDeviation, Value: 420, Unit: "m", DurationS: 12},
		{Type: TriggerHarshDecel, Value: 1.4, Unit: "g"},
		{Type: TriggerStop, Value: 50, Unit: "s", DurationS: 50},
		{Type: TriggerSpeed, Value: 120, Unit: "km/h", DurationS: 6},
	}
	if d := Describe(ts); d != "route deviation 420 m, harsh deceleration 1.4 g, unexpected stop 50 s, speed anomaly 120 km/h" {
		t.Fatalf("describe %q", d)
	}
	if Describe(nil) != "no anomaly" {
		t.Fatal("empty describe")
	}
	e := Explain(ts)
	for _, want := range []string{"420 m off the planned route for 12 s", "1.4 g", "Standing still for 50 s", "Sustained 120 km/h for 6 s"} {
		if !strings.Contains(e, want) {
			t.Fatalf("explain %q missing %q", e, want)
		}
	}
}

func TestRedisClient(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := NewRedis(context.Background(), "redis://"+mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sub := mr.NewSubscriber()
	sub.Subscribe(RedisChannel)
	got := make(chan string, 1)
	go func() { got <- (<-sub.Messages()).Message }()
	if err := c.Publish(context.Background(), RedisChannel, "hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != "hello" {
			t.Fatalf("got %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message")
	}
	if _, err := NewRedis(context.Background(), "::bad"); err == nil {
		t.Fatal("bad URL accepted")
	}
	addr := mr.Addr()
	mr.Close()
	if _, err := NewRedis(context.Background(), "redis://"+addr); err == nil {
		t.Fatal("ping to a closed server succeeded")
	}
}

// ---- HTTP ----

type tel struct{ pts []store.TelemetryPoint }

func (t *tel) AppendTelemetry(context.Context, store.TelemetryPoint) error { return nil }
func (t *tel) RecentTelemetry(_ context.Context, id string, limit int) ([]store.TelemetryPoint, error) {
	if id == "boom" {
		return nil, errors.New("db down")
	}
	return t.pts, nil
}

func newHTTP(t *testing.T, o Options) (*Service, *httptest.Server) {
	t.Helper()
	if o.Logger == nil {
		o.Logger = quiet
	}
	s := New(o)
	r := chi.NewRouter()
	s.Routes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return s, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHTTPSuspendTrackAndEmergency(t *testing.T) {
	st := &fakeStream{}
	veh := &fakeVehicles{suspended: map[string]bool{}}
	s, srv := newHTTP(t, Options{Stream: st, Positions: fakePositions{"d_7": {ID: "d_7", Lat: 12.97, Lng: 77.59}}})
	s.SetVehicles(veh)

	if code, out := call(t, srv, "GET", "/v1/safety/drivers/d_7", ""); code != 200 || out["suspended"] != false {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := call(t, srv, "POST", "/v1/safety/drivers/d_7/suspend", "{bad"); code != 400 {
		t.Fatalf("bad json %d", code)
	}
	code, out := call(t, srv, "POST", "/v1/safety/drivers/d_7/suspend", `{"incident_id":"inc_1","reason":"escalated","actor":"safety_sentinel"}`)
	if code != 200 || out["incident_id"] != "inc_1" {
		t.Fatalf("suspend %d %v", code, out)
	}
	if code, out := call(t, srv, "GET", "/v1/safety/drivers/d_7", ""); code != 200 || out["suspended"] != true || out["suspension"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := call(t, srv, "POST", "/v1/safety/drivers/d_7/unsuspend", `{"confirmed_by":"safety_sentinel"}`); code != 403 {
		t.Fatalf("safety agent unsuspend %d", code)
	}
	if code, _ := call(t, srv, "POST", "/v1/safety/drivers/d_7/unsuspend", `{bad`); code != 400 {
		t.Fatalf("bad json %d", code)
	}
	if code, _ := call(t, srv, "POST", "/v1/safety/drivers/d_7/unsuspend", `{"confirmed_by":"ops_supervisor"}`); code != 200 {
		t.Fatalf("supervisor unsuspend %d", code)
	}
	if code, _ := call(t, srv, "POST", "/v1/safety/drivers/d_7/unsuspend", `{"confirmed_by":"ops_supervisor"}`); code != 409 {
		t.Fatalf("not suspended %d", code)
	}
	veh.err = errors.New("boom")
	if code, _ := call(t, srv, "POST", "/v1/safety/drivers/x/suspend", `{}`); code != 500 {
		t.Fatalf("controller error %d", code)
	}

	if code, _ := call(t, srv, "POST", "/v1/safety/track", `{bad`); code != 400 {
		t.Fatalf("track bad json %d", code)
	}
	if code, _ := call(t, srv, "POST", "/v1/safety/track", `{}`); code != 404 {
		t.Fatalf("track without driver %d", code)
	}
	code, out = call(t, srv, "POST", "/v1/safety/track", `{"driver_id":"d_7","ride_id":"ride1"}`)
	path, _ := out["path"].(string)
	if code != 201 || !strings.HasPrefix(path, "/track/") || !strings.HasPrefix(out["url"].(string), "http://") {
		t.Fatalf("track %d %v", code, out)
	}
	if code, out := call(t, srv, "GET", path, ""); code != 200 || out["driver_id"] != "d_7" {
		t.Fatalf("follow link %d %v", code, out)
	}
	if code, _ := call(t, srv, "GET", "/track/nope", ""); code != 404 {
		t.Fatalf("unknown link %d", code)
	}

	code, out = call(t, srv, "POST", "/v1/safety/emergency/dispatch", `{"incident_id":"inc_1","location":{"lat":12.9,"lng":77.6}}`)
	if code != 200 || out["status"] != "dispatched" || !strings.HasPrefix(out["reference"].(string), "EMS-") {
		t.Fatalf("dispatch %d %v", code, out)
	}
	code, out = call(t, srv, "POST", "/v1/safety/emergency/notify-contact", `{"incident_id":"inc_1","phone":"+91"}`)
	if code != 200 || out["status"] != "sent" || !strings.HasPrefix(out["reference"].(string), "SMS-") {
		t.Fatalf("contact %d %v", code, out)
	}
	for _, p := range []string{"/v1/safety/emergency/dispatch", "/v1/safety/emergency/notify-contact"} {
		if code, _ := call(t, srv, "POST", p, `[1]`); code != 400 {
			t.Fatalf("%s non-object %d", p, code)
		}
	}
	code, out = call(t, srv, "GET", "/v1/safety/emergency/log", "")
	recs, _ := out["records"].([]any)
	if code != 200 || len(recs) != 2 || recs[0].(map[string]any)["kind"] != "contact" {
		t.Fatalf("log %d %v", code, out)
	}
	for i := 0; i < recentEmergency; i++ {
		s.recordEmergency("dispatch", "ems-", map[string]any{})
	}
	if n := len(s.EmergencyLog()); n != recentEmergency {
		t.Fatalf("emergency log %d", n)
	}
}

func TestHTTPPublishAndSignalsAndPath(t *testing.T) {
	st := &fakeStream{}
	pts := []store.TelemetryPoint{
		{DriverID: "d_1", Lat: 2, RecordedAt: t0.Add(2 * time.Second)},
		{DriverID: "d_1", Lat: 1, RecordedAt: t0.Add(time.Second)},
		{DriverID: "d_1", Lat: 0, RecordedAt: t0.Add(-time.Hour)},
	}
	s, srv := newHTTP(t, Options{Stream: st, Telemetry: &tel{pts: pts}, AgentsURL: "http://agents"})

	for _, body := range []string{
		`{bad`,
		`{"topic":"drivers","data":{"type":"safety_call"}}`,
		`{"topic":"ride:","data":{"type":"safety_call"}}`,
		`{"topic":"ops","data":{"type":"agent_event"}}`,
		`{"topic":"ops"}`,
	} {
		if code, _ := call(t, srv, "POST", "/v1/safety/publish", body); code != 400 {
			t.Fatalf("%s accepted: %d", body, code)
		}
	}
	for _, topic := range []string{"ops", "ride:abc"} {
		if code, _ := call(t, srv, "POST", "/v1/safety/publish", `{"topic":"`+topic+`","data":{"type":"safety_call","checkin_id":"c1"}}`); code != 202 {
			t.Fatalf("publish %s %d", topic, code)
		}
	}
	if len(st.msgs) != 2 || !strings.Contains(st.msgs[1], `ride:abc|{"checkin_id":"c1","type":"safety_call"}`) {
		t.Fatalf("stream %v", st.msgs)
	}

	s.Emit(context.Background(), sampleSignal())
	s.Wait()
	code, out := call(t, srv, "GET", "/v1/safety/signals", "")
	if code != 200 || len(out["signals"].([]any)) != 1 || out["agents_url_set"] != true || out["config"] == nil {
		t.Fatalf("signals %d %v", code, out)
	}

	code, out = call(t, srv, "GET", "/v1/safety/vehicle-path/d_1?since="+t0.Format(time.RFC3339)+"&limit=10", "")
	ps, _ := out["points"].([]any)
	if code != 200 || len(ps) != 2 || ps[0].(map[string]any)["lat"].(float64) != 1 {
		t.Fatalf("path %d %v", code, out)
	}
	if code, _ := call(t, srv, "GET", "/v1/safety/vehicle-path/d_1?since=yesterday", ""); code != 400 {
		t.Fatalf("bad since %d", code)
	}
	if code, _ := call(t, srv, "GET", "/v1/safety/vehicle-path/boom", ""); code != 500 {
		t.Fatalf("telemetry error %d", code)
	}

	_, srv2 := newHTTP(t, Options{})
	if code, out := call(t, srv2, "GET", "/v1/safety/vehicle-path/d_1", ""); code != 200 || out["source"] != "none" {
		t.Fatalf("no telemetry %d %v", code, out)
	}
	if code, _ := call(t, srv2, "POST", "/v1/safety/publish", `{"topic":"ops","data":{"type":"safety_call"}}`); code != 503 {
		t.Fatalf("no stream %d", code)
	}
}

func TestBaseURLHonoursForwardedProto(t *testing.T) {
	r := httptest.NewRequest("GET", "http://dispatch.example/x", nil)
	if got := baseURL(r); got != "http://dispatch.example" {
		t.Fatal(got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := baseURL(r); got != "https://dispatch.example" {
		t.Fatal(got)
	}
}
