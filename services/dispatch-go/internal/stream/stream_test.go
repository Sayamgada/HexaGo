package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

func ptr(f float64) *float64 { return &f }

// ---- Message formats (the wire contract) ----

func TestDriversMessageFormat(t *testing.T) {
	data := []stream.Driver{
		{ID: "d_123", Lat: 12.97, Lng: 77.59, Heading: 90, SpeedKmh: 32.5, Status: "idle", Tier: "sedan"},
		{ID: "d_007", Lat: 12.9, Lng: 77.6, Heading: 180.25, SpeedKmh: 0, Status: "on_trip", Tier: "av", Battery: ptr(81.5)},
	}
	got, err := stream.Encode(stream.TopicDrivers, data)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"topic":"drivers","data":[` +
		`{"id":"d_123","lat":12.97,"lng":77.59,"heading":90,"speed_kmh":32.5,"status":"idle","tier":"sedan","battery":null},` +
		`{"id":"d_007","lat":12.9,"lng":77.6,"heading":180.25,"speed_kmh":0,"status":"on_trip","tier":"av","battery":81.5}]}`
	if string(got) != want {
		t.Fatalf("drivers message:\n got %s\nwant %s", got, want)
	}
}

func TestEmptyDriversEncodesArray(t *testing.T) {
	got, err := stream.Encode(stream.TopicDrivers, []stream.Driver(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"topic":"drivers","data":[]}` {
		t.Fatalf("got %s", got)
	}
}

func TestRideMessageFormat(t *testing.T) {
	d := stream.Driver{ID: "d_123", Lat: 12.97, Lng: 77.59, Heading: 90, SpeedKmh: 32.5, Status: "en_route", Tier: "sedan"}
	got, err := stream.Encode(stream.RideTopic("r1"), stream.RideUpdate{RideID: "r1", Status: "arriving", Driver: d, ETASec: 240})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"topic":"ride:r1","data":{"ride_id":"r1","status":"arriving",` +
		`"driver":{"id":"d_123","lat":12.97,"lng":77.59,"heading":90,"speed_kmh":32.5,"status":"en_route","tier":"sedan","battery":null},` +
		`"eta_sec":240}}`
	if string(got) != want {
		t.Fatalf("ride message:\n got %s\nwant %s", got, want)
	}
}

func TestAgentEventMessageFormat(t *testing.T) {
	ts := time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC)
	ev := stream.NewAgentEvent("dispatch_broker", "matched", "best ETA", 0.92, 3.1, false, ts)
	got, err := stream.Encode(stream.TopicOps, ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"topic":"ops","data":{"type":"agent_event","agent":"dispatch_broker","decision":"matched",` +
		`"reasoning":"best ETA","confidence":0.92,"latency_ms":3.1,"used_fallback":false,"ts":"2026-11-16T10:30:00Z"}}`
	if string(got) != want {
		t.Fatalf("agent_event message:\n got %s\nwant %s", got, want)
	}
}

func TestAgentEventTimestampIsRFC3339UTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	ev := stream.NewAgentEvent("a", "d", "r", 1, 0, true, time.Date(2026, 1, 2, 3, 4, 5, 600, ist))
	if _, err := time.Parse(time.RFC3339, ev.TS); err != nil {
		t.Fatalf("ts %q is not RFC3339: %v", ev.TS, err)
	}
	if !strings.HasSuffix(ev.TS, "Z") {
		t.Fatalf("ts %q should be UTC", ev.TS)
	}
}

// metricsShape mirrors GET /v1/metrics/dispatch.
type metricsShape struct {
	P50Ms           float64 `json:"p50_ms"`
	P95Ms           float64 `json:"p95_ms"`
	P99Ms           float64 `json:"p99_ms"`
	MatchesTotal    uint64  `json:"matches_total"`
	ActiveDrivers   int     `json:"active_drivers"`
	IdleDrivers     int     `json:"idle_drivers"`
	OpenRequests    int     `json:"open_requests"`
	RidesInProgress int     `json:"rides_in_progress"`
}

func TestMetricsMessageFormat(t *testing.T) {
	m := metricsShape{P50Ms: 0.5, P95Ms: 1.25, P99Ms: 2, MatchesTotal: 7, ActiveDrivers: 300, IdleDrivers: 250, OpenRequests: 3, RidesInProgress: 47}
	data, err := stream.MetricsData(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := stream.Encode(stream.TopicOps, data)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"topic":"ops","data":{"type":"metrics","p50_ms":0.5,"p95_ms":1.25,"p99_ms":2,"matches_total":7,` +
		`"active_drivers":300,"idle_drivers":250,"open_requests":3,"rides_in_progress":47}}`
	if string(got) != want {
		t.Fatalf("metrics message:\n got %s\nwant %s", got, want)
	}
}

func TestMetricsDataRejectsNonObject(t *testing.T) {
	if _, err := stream.MetricsData([]int{1}); err == nil {
		t.Fatal("expected error for non-object metrics")
	}
}

func TestParseClientMessage(t *testing.T) {
	m, err := stream.ParseClientMessage([]byte(`{"action":"subscribe","topics":["drivers","ops","ride:abc-123"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Action != "subscribe" || len(m.Topics) != 3 || m.Topics[2] != "ride:abc-123" {
		t.Fatalf("parsed %+v", m)
	}
	if _, err := stream.ParseClientMessage([]byte(`{"action":"unsubscribe","topics":["ops"]}`)); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	bad := []string{
		`not json`,
		`{"action":"publish","topics":["drivers"]}`,
		`{"action":"subscribe","topics":["weather"]}`,
		`{"action":"subscribe","topics":["ride:"]}`,
		`{"action":"subscribe","topics":["ride:` + strings.Repeat("x", 100) + `"]}`,
	}
	for _, b := range bad {
		if _, err := stream.ParseClientMessage([]byte(b)); err == nil {
			t.Errorf("expected error for %s", b)
		}
	}
}

// ---- Hub over a real WebSocket ----

type wsHarness struct {
	hub  *stream.Hub
	srv  *httptest.Server
	conn *websocket.Conn
}

func newHarness(t *testing.T, broker stream.Broker) *wsHarness {
	t.Helper()
	hub := stream.NewHub(stream.HubOptions{FlushInterval: time.Hour, Broker: broker}) // flush manually
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(hub.Handler(up))
	t.Cleanup(srv.Close)
	return &wsHarness{hub: hub, srv: srv}
}

func (h *wsHarness) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func subscribe(t *testing.T, hub *stream.Hub, conn *websocket.Conn, topics ...string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"action": "subscribe", "topics": topics}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, tp := range topics {
			if hub.Subscribers(tp) == 0 {
				return false
			}
		}
		return true
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readFrames(t *testing.T, conn *websocket.Conn, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for len(out) < n {
		_, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read after %d frames: %v", len(out), err)
		}
		out = append(out, string(b))
	}
	return out
}

func expectNoFrame(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, b, err := conn.ReadMessage(); err == nil {
		t.Fatalf("unexpected frame %s", b)
	}
}

func TestHubDeliversSubscribedTopicsBatched(t *testing.T) {
	h := newHarness(t, nil)
	conn := h.dial(t)
	subscribe(t, h.hub, conn, "drivers", "ops")

	// Two driver snapshots within one window: only the latest is sent.
	h.hub.Publish(stream.TopicDrivers, []stream.Driver{{ID: "d_1", Status: "idle", Tier: "mini"}})
	h.hub.Publish(stream.TopicDrivers, []stream.Driver{{ID: "d_2", Status: "idle", Tier: "mini"}})
	// Ops messages are all kept, in order.
	ts := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	h.hub.Publish(stream.TopicOps, stream.NewAgentEvent("dispatch_broker", "a", "", 1, 0, false, ts))
	h.hub.Publish(stream.TopicOps, stream.NewAgentEvent("dispatch_broker", "b", "", 1, 0, false, ts))
	// Not subscribed: never delivered.
	h.hub.Publish(stream.RideTopic("x"), stream.RideUpdate{RideID: "x", Status: "accepted"})

	waitFor(t, func() bool { return h.hub.Pending() == 4 }) // drivers(1) + ops(2) + ride(1)
	h.hub.Flush()

	frames := readFrames(t, conn, 3)
	var drivers, ops []string
	for _, f := range frames {
		switch {
		case strings.HasPrefix(f, `{"topic":"drivers"`):
			drivers = append(drivers, f)
		case strings.HasPrefix(f, `{"topic":"ops"`):
			ops = append(ops, f)
		default:
			t.Fatalf("unexpected frame %s", f)
		}
	}
	if len(drivers) != 1 || !strings.Contains(drivers[0], `"id":"d_2"`) {
		t.Fatalf("drivers frames %v", drivers)
	}
	if len(ops) != 2 || !strings.Contains(ops[0], `"decision":"a"`) || !strings.Contains(ops[1], `"decision":"b"`) {
		t.Fatalf("ops frames %v", ops)
	}
	expectNoFrame(t, conn)
}

func TestHubRideTopicAndSnapshotOnSubscribe(t *testing.T) {
	h := newHarness(t, nil)
	// Published before anyone subscribed: kept as the latest snapshot.
	h.hub.Publish(stream.RideTopic("r9"), stream.RideUpdate{RideID: "r9", Status: "arriving", ETASec: 60})
	waitFor(t, func() bool { return h.hub.Pending() == 1 })
	h.hub.Flush()

	conn := h.dial(t)
	subscribe(t, h.hub, conn, "ride:r9")
	h.hub.Flush()
	frames := readFrames(t, conn, 1)
	if !strings.HasPrefix(frames[0], `{"topic":"ride:r9","data":{"ride_id":"r9","status":"arriving"`) {
		t.Fatalf("snapshot frame %s", frames[0])
	}
	// Nothing new: nothing more is sent.
	h.hub.Flush()
	expectNoFrame(t, conn)
}

func TestHubUnsubscribe(t *testing.T) {
	h := newHarness(t, nil)
	conn := h.dial(t)
	subscribe(t, h.hub, conn, "ops")
	if err := conn.WriteJSON(map[string]any{"action": "unsubscribe", "topics": []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.hub.Subscribers("ops") == 0 })
	h.hub.Publish(stream.TopicOps, stream.NewAgentEvent("x", "y", "", 0, 0, false, time.Now()))
	waitFor(t, func() bool { return h.hub.Pending() == 1 })
	h.hub.Flush()
	expectNoFrame(t, conn)
}

func TestHubPublishLocalSkipsBroker(t *testing.T) {
	b := &recordingBroker{LocalBroker: stream.NewLocalBroker()}
	h := newHarness(t, b)
	conn := h.dial(t)
	subscribe(t, h.hub, conn, "ops")
	data, _ := stream.MetricsData(metricsShape{ActiveDrivers: 1})
	h.hub.PublishLocal(stream.TopicOps, data)
	h.hub.Flush()
	frames := readFrames(t, conn, 1)
	if !strings.Contains(frames[0], `"type":"metrics"`) {
		t.Fatalf("frame %s", frames[0])
	}
	if b.count() != 0 {
		t.Fatalf("PublishLocal went through the broker %d times", b.count())
	}
}

func TestHubFlushLoopRunsEverySecondByDefault(t *testing.T) {
	hub := stream.NewHub(stream.HubOptions{FlushInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)
	srv := httptest.NewServer(hub.Handler(websocket.Upgrader{}))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	subscribe(t, hub, conn, "drivers")
	hub.Publish(stream.TopicDrivers, []stream.Driver{{ID: "d_1"}})
	frames := readFrames(t, conn, 1)
	if !strings.HasPrefix(frames[0], `{"topic":"drivers"`) {
		t.Fatalf("frame %s", frames[0])
	}
	if stream.DefaultFlushInterval != time.Second {
		t.Fatalf("default flush interval %v, want 1s", stream.DefaultFlushInterval)
	}
}

func TestHubInvalidClientMessageKeepsConnection(t *testing.T) {
	h := newHarness(t, nil)
	conn := h.dial(t)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"action":"subscribe","topics":["bogus"]}`)); err != nil {
		t.Fatal(err)
	}
	subscribe(t, h.hub, conn, "drivers")
	if h.hub.Subscribers("bogus") != 0 {
		t.Fatal("invalid topic was subscribed")
	}
}

// ---- Brokers ----

type recordingBroker struct {
	*stream.LocalBroker
	n atomic.Int32
}

func (r *recordingBroker) Publish(ctx context.Context, msg []byte) error {
	r.n.Add(1)
	return r.LocalBroker.Publish(ctx, msg)
}

func (r *recordingBroker) count() int { return int(r.n.Load()) }

func TestLocalBrokerFanOut(t *testing.T) {
	b := stream.NewLocalBroker()
	got := make(chan []byte, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Subscribe(ctx, func(m []byte) { got <- m }) }()
	waitFor(t, func() bool { return b.Subscribers() == 1 })
	if err := b.Publish(ctx, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if string(m) != "hello" {
			t.Fatalf("got %q", m)
		}
	case <-time.After(time.Second):
		t.Fatal("no delivery")
	}
	if b.Kind() != "local" {
		t.Fatalf("kind %q", b.Kind())
	}
}

// Two hubs sharing one broker behave like two gateway instances on Redis:
// a publish on one reaches subscribers of the other.
func TestHubsShareBroker(t *testing.T) {
	shared := stream.NewLocalBroker()
	a := newHarness(t, shared)
	b := newHarness(t, shared)
	waitFor(t, func() bool { return shared.Subscribers() == 2 })
	conn := b.dial(t)
	subscribe(t, b.hub, conn, "ops")
	a.hub.Publish(stream.TopicOps, stream.NewAgentEvent("dispatch_broker", "cross", "", 1, 0, false, time.Now()))
	waitFor(t, func() bool { return b.hub.Pending() == 1 })
	b.hub.Flush()
	frames := readFrames(t, conn, 1)
	if !strings.Contains(frames[0], `"decision":"cross"`) {
		t.Fatalf("frame %s", frames[0])
	}
}

func TestDecodeEnvelopeRoundTrip(t *testing.T) {
	raw, _ := stream.Encode(stream.TopicDrivers, []stream.Driver{{ID: "d_1"}})
	var env struct {
		Topic string            `json:"topic"`
		Data  []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Topic != "drivers" || len(env.Data) != 1 {
		t.Fatalf("round trip: %v %+v", err, env)
	}
}
