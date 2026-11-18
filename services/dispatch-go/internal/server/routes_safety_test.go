package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
)

func TestSafetyRoutesSplitBetweenGatewayAndAgents(t *testing.T) {
	var proxied []string
	upstream := mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		proxied = append(proxied, req.Method+" "+req.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"incidents":[]}`))}, nil
	})
	svc := safety.New(safety.Options{Config: safety.DefaultConfig(true)})
	router := server.NewRouter(testConfig(), server.WithTransport(upstream), server.WithSafety(svc))

	for _, tc := range []struct {
		method, path string
		local        bool
		code         int
	}{
		{"GET", "/v1/safety/incidents", false, 200},
		{"POST", "/v1/safety/signal", false, 200},
		{"POST", "/v1/safety/checkins/c1/reply", false, 200},
		{"POST", "/v1/safety/incidents/i1/resolve", false, 200},
		{"GET", "/v1/safety/signals", true, 200},
		{"GET", "/v1/safety/drivers/d_001", true, 200},
		{"GET", "/track/unknown", true, 404},
	} {
		before := len(proxied)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))
		if rec.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
		if wasProxied := len(proxied) > before; wasProxied == tc.local {
			t.Fatalf("%s %s: proxied=%v, want local=%v", tc.method, tc.path, wasProxied, tc.local)
		}
	}
}

func TestSafetyRoutesWithoutAgentsURL(t *testing.T) {
	cfg := testConfig()
	cfg.AgentsURL = "://bad"
	router := server.NewRouter(cfg) // builds its own safety service
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/safety/incidents", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/safety/signals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("local route %d", rec.Code)
	}
}
