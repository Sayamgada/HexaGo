package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/cors"
)

// DefaultAgentsWakeTimeout is how long a proxied request waits for a sleeping
// agents-py (Render free tier cold start) before answering 503 agents_waking.
const DefaultAgentsWakeTimeout = 60 * time.Second

// AgentsWakingBody is the JSON answer once the wake window runs out.
const AgentsWakingBody = `{"error":"agents_waking","retry_after_s":5}`

const (
	firstBackoff  = 250 * time.Millisecond
	maxBackoff    = 4 * time.Second
	maxReplayBody = 4 << 20 // request bodies above this are not buffered for retries
)

// WithAgentsWakeTimeout overrides DefaultAgentsWakeTimeout (tests).
func WithAgentsWakeTimeout(d time.Duration) Option {
	return func(o *RouterOptions) { o.AgentsWake = d }
}

// wakeTransport retries a request while agents-py is unreachable or its
// platform proxy says it is starting, then gives up with a JSON 503.
//
// Retried: dial errors (connection refused / reset, DNS, timeouts before a
// response) and 502/503/504 whose body is not JSON - Render's own pages while
// the instance boots. A JSON 5xx comes from agents-py itself and is passed on.
type wakeTransport struct {
	base    http.RoundTripper
	timeout time.Duration
	sleep   func(ctx context.Context, d time.Duration) error
	logf    func(format string, args ...any)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryableErr reports whether err means agents-py is not (yet) accepting
// connections. For requests that are not idempotent only failures before the
// request could have reached agents-py (refused dial, DNS) are retried.
func retryableErr(err error, idempotent bool) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	if !idempotent {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || op != nil {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// wakingResponse reports whether resp is the hosting platform's "starting" page.
func wakingResponse(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return false
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return mt != "application/json" && !strings.HasSuffix(mt, "+json")
}

func wakingAnswer(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusServiceUnavailable,
		Status:        "503 Service Unavailable",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}, "Retry-After": {"5"}},
		Body:          io.NopCloser(strings.NewReader(AgentsWakingBody)),
		ContentLength: int64(len(AgentsWakingBody)),
		Request:       req,
	}
}

func (t *wakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(req.Body, maxReplayBody+1))
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > maxReplayBody { // too big to replay: one attempt only
			req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), req.Body))
			return t.base.RoundTrip(req)
		}
		body = b
	}
	deadline := time.Now().Add(t.timeout)
	backoff := firstBackoff
	for attempt := 1; ; attempt++ {
		try := req.Clone(req.Context())
		if body != nil {
			try.Body = io.NopCloser(bytes.NewReader(body))
			try.ContentLength = int64(len(body))
		}
		resp, err := t.base.RoundTrip(try)
		switch {
		case err != nil:
			if !retryableErr(err, idempotent(req.Method)) {
				return nil, err
			}
		case wakingResponse(resp):
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		default:
			if attempt > 1 {
				t.logf("agents proxy: %s %s answered after %d attempts", req.Method, req.URL.Path, attempt)
			}
			return resp, nil
		}
		wait := backoff
		if remaining := time.Until(deadline); remaining <= 0 {
			t.logf("agents proxy: %s %s: agents-py still unavailable after %s (%d attempts)", req.Method, req.URL.Path, t.timeout, attempt)
			return wakingAnswer(req), nil
		} else if wait > remaining {
			wait = remaining
		}
		if err := t.sleep(req.Context(), wait); err != nil {
			return nil, err
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// newAgentsProxy reverse-proxies to target with the wake-up retries above.
func newAgentsProxy(target *url.URL, base http.RoundTripper, wake time.Duration) http.Handler {
	if base == nil {
		base = http.DefaultTransport
	}
	if wake <= 0 {
		wake = DefaultAgentsWakeTimeout
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &wakeTransport{base: base, timeout: wake, sleep: sleepCtx, logf: log.Printf}
	proxy.ModifyResponse = func(resp *http.Response) error {
		cors.StripUpstream(resp.Header)
		return nil
	}
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, context.Canceled) {
			return // the client went away
		}
		log.Printf("agents proxy: %s %s: %v", r.Method, r.URL.Path, err)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, AgentsWakingBody)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server's 15 s WriteTimeout would cut a request that waits for a cold start.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wake + 15*time.Second))
		proxy.ServeHTTP(w, r)
	})
}
