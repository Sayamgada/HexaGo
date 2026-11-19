package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
)

// scripted answers each round trip with the next step (the last one repeats).
type scripted struct {
	mu     sync.Mutex
	steps  []func(*http.Request) (*http.Response, error)
	calls  int
	bodies []string
}

func (s *scripted) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	i := s.calls
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.calls++
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(b))
	}
	step := s.steps[i]
	s.mu.Unlock()
	return step(r)
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func refused(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func reply(code int, ctype, body string) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {ctype}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

func newWake(base http.RoundTripper, timeout time.Duration) *wakeTransport {
	return &wakeTransport{base: base, timeout: timeout, sleep: noSleep, logf: func(string, ...any) {}}
}

func TestWakeTransportRetriesRefusedThenSucceeds(t *testing.T) {
	base := &scripted{steps: []func(*http.Request) (*http.Response, error){refused, refused, reply(200, "application/json", `{"ok":true}`)}}
	req := httptest.NewRequest(http.MethodPost, "http://agents/v1/agents/x", strings.NewReader(`{"q":"hi"}`))
	resp, err := newWake(base, time.Minute).RoundTrip(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if base.count() != 3 {
		t.Errorf("attempts = %d, want 3", base.count())
	}
	for i, b := range base.bodies {
		if b != `{"q":"hi"}` {
			t.Errorf("attempt %d body %q: the POST body must be replayed", i+1, b)
		}
	}
}

func TestWakeTransportRetriesPlatformPagesNotAgentErrors(t *testing.T) {
	base := &scripted{steps: []func(*http.Request) (*http.Response, error){
		reply(503, "text/html", "<h1>Service waking up</h1>"),
		reply(502, "text/html", "bad gateway"),
		reply(200, "application/json", `{}`),
	}}
	resp, err := newWake(base, time.Minute).RoundTrip(httptest.NewRequest(http.MethodGet, "http://agents/v1/agents", nil))
	if err != nil || resp.StatusCode != 200 || base.count() != 3 {
		t.Fatalf("resp %v err %v attempts %d", resp, err, base.count())
	}

	// A JSON 502 is agents-py's own answer: passed through, no retry.
	own := &scripted{steps: []func(*http.Request) (*http.Response, error){reply(502, "application/json", `{"error":"could not schedule"}`)}}
	resp, err = newWake(own, time.Minute).RoundTrip(httptest.NewRequest(http.MethodPost, "http://agents/v1/booking/confirm", strings.NewReader("{}")))
	if err != nil || resp.StatusCode != 502 || own.count() != 1 {
		t.Fatalf("agent JSON 502: resp %v err %v attempts %d", resp, err, own.count())
	}
}

func TestWakeTransportGivesUpWithAgentsWaking(t *testing.T) {
	base := &scripted{steps: []func(*http.Request) (*http.Response, error){refused}}
	w := newWake(base, 30*time.Millisecond)
	w.sleep = sleepCtx // real (short) waits
	resp, err := w.RoundTrip(httptest.NewRequest(http.MethodGet, "http://agents/v1/agents", nil))
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("resp %v err %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != AgentsWakingBody || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("body %s / %v", b, resp.Header)
	}
	if base.count() < 2 {
		t.Errorf("attempts = %d, want retries before giving up", base.count())
	}
}

func TestWakeTransportNonIdempotentNotRetriedAfterReachingAgents(t *testing.T) {
	reset := func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	}
	base := &scripted{steps: []func(*http.Request) (*http.Response, error){reset, reply(200, "application/json", "{}")}}
	if _, err := newWake(base, time.Minute).RoundTrip(httptest.NewRequest(http.MethodPost, "http://agents/x", strings.NewReader("{}"))); err == nil {
		t.Fatal("a POST reset mid-request must not be replayed")
	}
	if base.count() != 1 {
		t.Errorf("attempts = %d", base.count())
	}
	get := &scripted{steps: []func(*http.Request) (*http.Response, error){reset, reply(200, "application/json", "{}")}}
	if resp, err := newWake(get, time.Minute).RoundTrip(httptest.NewRequest(http.MethodGet, "http://agents/x", nil)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("a GET may be retried: %v %v", resp, err)
	}
}

func TestWakeTransportStopsWhenClientLeaves(t *testing.T) {
	base := &scripted{steps: []func(*http.Request) (*http.Response, error){refused}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWake(base, time.Minute)
	w.sleep = sleepCtx
	req := httptest.NewRequest(http.MethodGet, "http://agents/x", nil).WithContext(ctx)
	if _, err := w.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRetryableErr(t *testing.T) {
	cases := []struct {
		err        error
		idempotent bool
		want       bool
	}{
		{nil, true, false},
		{context.Canceled, true, false},
		{syscall.ECONNREFUSED, false, true},
		{&net.DNSError{Err: "no such host", Name: "agents"}, false, true},
		{&net.OpError{Op: "dial", Err: errors.New("x")}, false, true},
		{io.EOF, false, false},
		{io.EOF, true, true},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, true, true},
		{errors.New("tls: bad certificate"), true, false},
	}
	for i, c := range cases {
		if got := retryableErr(c.err, c.idempotent); got != c.want {
			t.Errorf("case %d (%v, idempotent=%v) = %v", i, c.err, c.idempotent, got)
		}
	}
}

// End to end: a gateway whose AGENTS_URL points at a closed port answers JSON
// 503 agents_waking (never a bare 502), and recovers once agents-py is up.
func TestGatewayAgentsWakingThenRecovers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens: connection refused

	cfg := config.Load()
	cfg.AgentsURL = "http://" + addr
	r := NewRouter(cfg, WithAgentsWakeTimeout(300*time.Millisecond))
	gw := httptest.NewServer(r)
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), `"error":"agents_waking"`) {
		t.Fatalf("asleep: %d %s", resp.StatusCode, b)
	}

	// agents-py comes up on that address while a request is waiting.
	go func() {
		time.Sleep(100 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"agents":[]}`)
		})}
		t.Cleanup(func() { _ = srv.Close() })
		_ = srv.Serve(l)
	}()
	u, _ := url.Parse(gw.URL + "/v1/agents")
	resp, err = http.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != `{"agents":[]}` {
		t.Fatalf("after wake: %d %s", resp.StatusCode, b)
	}
}
