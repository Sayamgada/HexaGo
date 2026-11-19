package developer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type receiver struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	codes    []int // status per call; last one repeats
	calls    atomic.Int32
	got      chan struct{}
}

func newReceiver(codes ...int) (*receiver, *httptest.Server) {
	r := &receiver{codes: codes, got: make(chan struct{}, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		n := int(r.calls.Add(1))
		r.mu.Lock()
		r.requests = append(r.requests, req)
		r.bodies = append(r.bodies, b)
		code := r.codes[min(n, len(r.codes))-1]
		r.mu.Unlock()
		w.WriteHeader(code)
		r.got <- struct{}{}
	}))
	return r, srv
}

func (r *receiver) wait(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-r.got:
		case <-time.After(5 * time.Second):
			t.Fatalf("waited for delivery %d of %d", i+1, n)
		}
	}
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func newTestDispatcher(t *testing.T, s Store, opts DispatcherOptions) *Dispatcher {
	t.Helper()
	opts.Store = s
	opts.AllowPrivate = true
	if opts.Backoff == 0 {
		opts.Backoff = time.Millisecond
	}
	opts.Logger = quietLogger()
	d := NewDispatcher(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return d
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSignAndVerify(t *testing.T) {
	sig := Sign("whsec_x", 1700000000, []byte(`{"a":1}`))
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != 7+64 {
		t.Fatal(sig)
	}
	if !Verify("whsec_x", 1700000000, []byte(`{"a":1}`), sig) {
		t.Fatal("valid signature rejected")
	}
	for _, bad := range []func() bool{
		func() bool { return Verify("whsec_y", 1700000000, []byte(`{"a":1}`), sig) },
		func() bool { return Verify("whsec_x", 1700000001, []byte(`{"a":1}`), sig) },
		func() bool { return Verify("whsec_x", 1700000000, []byte(`{"a":2}`), sig) },
	} {
		if bad() {
			t.Fatal("tampered delivery verified")
		}
	}
}

func TestDeliverySignedAndRecorded(t *testing.T) {
	rcv, srv := newReceiver(200)
	defer srv.Close()
	s := NewMemory()
	clock := newClock()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", KeyID: "k", URL: srv.URL, Events: []string{EventRideCompleted}, Secret: "whsec_t", CreatedAt: clock.Now()})
	d := newTestDispatcher(t, s, DispatcherOptions{Now: clock.Now})
	d.Emit(context.Background(), EventRideCompleted, map[string]string{"id": "ride-1"})
	d.Emit(context.Background(), EventIncidentOpened, map[string]string{"id": "nobody listens"})
	rcv.wait(t, 1)

	req, body := rcv.requests[0], rcv.bodies[0]
	ts, _ := strconv.ParseInt(req.Header.Get(HeaderTimestamp), 10, 64)
	if ts != clock.Now().Unix() || !Verify("whsec_t", ts, body, req.Header.Get(HeaderSignature)) {
		t.Fatalf("bad signature headers %v", req.Header)
	}
	if req.Header.Get(HeaderEvent) != EventRideCompleted || req.Header.Get("Content-Type") != "application/json" ||
		!strings.HasPrefix(req.Header.Get(HeaderDelivery), "evt_") {
		t.Fatalf("headers %v", req.Header)
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.Type != EventRideCompleted || ev.ID != req.Header.Get(HeaderDelivery) {
		t.Fatalf("payload %s %v", body, err)
	}
	waitFor(t, func() bool { return len(d.History("wh_1")) == 1 })
	h := d.History("wh_1")[0]
	if !h.OK || h.Attempts != 1 || h.StatusCode != 200 || h.EventID != ev.ID {
		t.Fatalf("history %+v", h)
	}
	if st := d.Stats(); st["sent"] != 1 || st["failed"] != 0 {
		t.Fatalf("stats %v", st)
	}
}

func TestDeliveryRetriesWithBackoff(t *testing.T) {
	rcv, srv := newReceiver(500, 503, 200)
	defer srv.Close()
	s := NewMemory()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", URL: srv.URL, Events: []string{EventPayoutSent}, Secret: "s"})
	d := newTestDispatcher(t, s, DispatcherOptions{Backoff: 20 * time.Millisecond})
	start := time.Now()
	d.Emit(context.Background(), EventPayoutSent, "x")
	rcv.wait(t, 3)
	if el := time.Since(start); el < 60*time.Millisecond { // 20 ms + 40 ms
		t.Fatalf("retries too fast: %v", el)
	}
	waitFor(t, func() bool { return len(d.History("wh_1")) == 1 })
	h := d.History("wh_1")[0]
	if !h.OK || h.Attempts != 3 {
		t.Fatalf("history %+v", h)
	}
	ids := map[string]bool{}
	for _, r := range rcv.requests {
		ids[r.Header.Get(HeaderDelivery)] = true
	}
	if len(ids) != 1 {
		t.Fatalf("retries used different delivery ids: %v", ids)
	}
}

func TestDeliveryGivesUpAfterThreeRetries(t *testing.T) {
	rcv, srv := newReceiver(500)
	defer srv.Close()
	s := NewMemory()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", URL: srv.URL, Events: []string{EventPayoutSent}, Secret: "s"})
	d := newTestDispatcher(t, s, DispatcherOptions{})
	d.Emit(context.Background(), EventPayoutSent, "x")
	rcv.wait(t, 4)
	waitFor(t, func() bool { return len(d.History("wh_1")) == 1 })
	if h := d.History("wh_1")[0]; h.OK || h.Attempts != 4 || h.StatusCode != 500 || h.Error == "" {
		t.Fatalf("history %+v", h)
	}
	time.Sleep(30 * time.Millisecond)
	if n := rcv.calls.Load(); n != 4 {
		t.Fatalf("%d attempts", n)
	}
	if st := d.Stats(); st["failed"] != 1 {
		t.Fatalf("stats %v", st)
	}
}

func TestDeliveryDoesNotRetryClientErrors(t *testing.T) {
	rcv, srv := newReceiver(410)
	defer srv.Close()
	s := NewMemory()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", URL: srv.URL, Events: []string{EventPayoutSent}, Secret: "s"})
	d := newTestDispatcher(t, s, DispatcherOptions{})
	d.Emit(context.Background(), EventPayoutSent, "x")
	rcv.wait(t, 1)
	waitFor(t, func() bool { return len(d.History("wh_1")) == 1 })
	if h := d.History("wh_1")[0]; h.OK || h.Attempts != 1 || h.StatusCode != 410 {
		t.Fatalf("history %+v", h)
	}
}

func TestDeliveryRetriesTooManyRequests(t *testing.T) {
	rcv, srv := newReceiver(429, 200)
	defer srv.Close()
	s := NewMemory()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", URL: srv.URL, Events: []string{EventPayoutSent}, Secret: "s"})
	d := newTestDispatcher(t, s, DispatcherOptions{})
	d.Emit(context.Background(), EventPayoutSent, "x")
	rcv.wait(t, 2)
}

func TestDeliveryNetworkErrorAndBadPayload(t *testing.T) {
	s := NewMemory()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", URL: "http://127.0.0.1:1/nothing", Events: []string{EventPayoutSent}, Secret: "s"})
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_2", URL: "http://bad url", Events: []string{EventIncidentOpened}, Secret: "s"})
	d := newTestDispatcher(t, s, DispatcherOptions{Retries: -1})
	d.Emit(context.Background(), EventPayoutSent, "x")
	d.Emit(context.Background(), EventIncidentOpened, "x")
	d.Emit(context.Background(), EventPayoutSent, func() {}) // cannot be marshalled
	waitFor(t, func() bool { return len(d.History("wh_1")) == 2 && len(d.History("wh_2")) == 1 })
	for _, h := range append(d.History("wh_1"), d.History("wh_2")...) {
		if h.OK || h.Error == "" {
			t.Fatalf("history %+v", h)
		}
	}
}

func TestPingAndHistoryBound(t *testing.T) {
	rcv, srv := newReceiver(204)
	defer srv.Close()
	d := newTestDispatcher(t, NewMemory(), DispatcherOptions{})
	h := Webhook{ID: "wh_p", URL: srv.URL, Secret: "s"}
	for i := 0; i < historyPerHook+5; i++ {
		ev := d.Ping(h)
		if ev.Type != EventPing {
			t.Fatal(ev.Type)
		}
	}
	rcv.wait(t, historyPerHook+5)
	waitFor(t, func() bool { return d.Stats()["sent"] == historyPerHook+5 })
	if n := len(d.History("wh_p")); n != historyPerHook {
		t.Fatalf("history kept %d", n)
	}
}

type failingStore struct{ Store }

func (failingStore) WebhooksFor(context.Context, string) ([]Webhook, error) {
	return nil, errors.New("db down")
}

func TestEmitSurvivesStoreErrorsAndFullQueue(t *testing.T) {
	d := NewDispatcher(DispatcherOptions{Store: failingStore{}, Logger: quietLogger()})
	d.Emit(context.Background(), EventPayoutSent, "x") // logged, nothing queued
	if d.Stats()["queued"] != 0 {
		t.Fatal("queued despite store error")
	}
	s := NewMemory()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", URL: "https://x.example", Events: []string{EventPayoutSent}, Secret: "s"})
	full := NewDispatcher(DispatcherOptions{Store: s, Queue: 1, Logger: quietLogger()}) // not running: nothing drains
	full.Emit(context.Background(), EventPayoutSent, 1)
	full.Emit(context.Background(), EventPayoutSent, 2)
	if st := full.Stats(); st["queued"] != 1 || st["dropped"] != 1 {
		t.Fatalf("stats %v", st)
	}
}

func TestSubscriberCache(t *testing.T) {
	s := NewMemory()
	clock := newClock()
	d := NewDispatcher(DispatcherOptions{Store: s, Now: clock.Now, Logger: quietLogger()})
	ctx := context.Background()
	if subs, _ := d.Subscribers(ctx, EventRideCompleted); len(subs) != 0 {
		t.Fatal("unexpected subscribers")
	}
	_ = s.CreateWebhook(ctx, Webhook{ID: "wh_1", URL: "https://x.example", Events: []string{EventRideCompleted}})
	if subs, _ := d.Subscribers(ctx, EventRideCompleted); len(subs) != 0 {
		t.Fatal("cache not used")
	}
	clock.Add(subscriberTTL)
	if subs, _ := d.Subscribers(ctx, EventRideCompleted); len(subs) != 1 {
		t.Fatal("cache did not expire")
	}
	_ = s.DeleteWebhook(ctx, "", "wh_1")
	d.Invalidate()
	if subs, _ := d.Subscribers(ctx, EventRideCompleted); len(subs) != 0 {
		t.Fatal("Invalidate did not clear the cache")
	}
}

func TestDispatcherDefaults(t *testing.T) {
	d := NewDispatcher(DispatcherOptions{Store: NewMemory()})
	if d.o.Retries != 3 || d.o.Backoff != time.Second || d.o.Timeout != 5*time.Second || d.o.Workers != 4 ||
		d.o.Queue != 1024 || d.client == nil || d.log == nil {
		t.Fatalf("defaults %+v", d.o)
	}
	custom := &http.Client{}
	if NewDispatcher(DispatcherOptions{Store: NewMemory(), Client: custom}).client != custom {
		t.Fatal("custom client ignored")
	}
}

func TestValidateURL(t *testing.T) {
	ok := []string{"https://hooks.example.com/hexago", "http://example.org:8080/x?y=1"}
	for _, u := range ok {
		if err := ValidateURL(u, false); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{"", "ftp://x.example", "/relative", "https://", "https://user:pw@x.example",
		"http://localhost:8080/x", "http://api.localhost/x", "http://metadata.google.internal/x",
		"http://127.0.0.1/x", "http://10.0.0.5/x", "http://192.168.1.1/x", "http://169.254.169.254/latest",
		"http://[::1]/x", "http://0.0.0.0/x", "https://x.example/" + strings.Repeat("a", 2100)}
	for _, u := range bad {
		if err := ValidateURL(u, false); !errors.Is(err, ErrValidation) {
			t.Errorf("%s accepted (%v)", u, err)
		}
	}
	if err := ValidateURL("http://127.0.0.1:9/x", true); err != nil {
		t.Errorf("allowPrivate: %v", err)
	}
}

func TestGuardedClientRefusesPrivateTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	c := guardedClient(time.Second, false)
	if _, err := c.Get(srv.URL); err == nil || !errors.Is(err, ErrPrivateTarget) {
		t.Fatalf("loopback dial allowed: %v", err)
	}
	resp, err := guardedClient(time.Second, true).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowPrivate client: %v", err)
	}
	resp.Body.Close()
}

func TestDispatcherStopsRetryingOnShutdown(t *testing.T) {
	rcv, srv := newReceiver(500)
	defer srv.Close()
	s := NewMemory()
	_ = s.CreateWebhook(context.Background(), Webhook{ID: "wh_1", URL: srv.URL, Events: []string{EventPayoutSent}, Secret: "s"})
	d := NewDispatcher(DispatcherOptions{Store: s, AllowPrivate: true, Backoff: time.Hour, Logger: quietLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	d.Emit(context.Background(), EventPayoutSent, "x")
	rcv.wait(t, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if h := d.History("wh_1"); len(h) != 1 || h[0].OK || h[0].Attempts != 1 {
		t.Fatalf("history %+v", h)
	}
}
