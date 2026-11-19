package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/scheduler"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
)

func TestSchedulerRoutesAndPricingProxy(t *testing.T) {
	var proxied []string
	upstream := mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		proxied = append(proxied, req.Method+" "+req.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"total":250}`))}, nil
	})
	router := server.NewRouter(testConfig(), server.WithTransport(upstream))

	plan := `{"plan_id":"p1","rider_id":"rider_priya","legs":[{"tier":"sedan","pickup":{"lat":12.97,"lng":77.59},
		"dropoff":{"lat":12.99,"lng":77.6},"pickup_at":"2030-01-01T10:00:00+05:30"}]}`
	for _, tc := range []struct {
		method, path, body string
		code               int
		proxied            bool
	}{
		{"POST", "/v1/scheduled", plan, http.StatusCreated, false},
		{"GET", "/v1/scheduled?rider_id=rider_priya", "", http.StatusOK, false},
		{"GET", "/v1/eta?from_lat=12.97&from_lng=77.59&to_lat=13.2&to_lng=77.7", "", http.StatusOK, false},
		{"GET", "/v1/supply?lat=12.97&lng=77.59", "", http.StatusOK, false},
		{"POST", "/v1/pricing/quote", `{"tier":"mini"}`, http.StatusOK, true},
		{"POST", "/v1/agents/booking/parse", `{"text":"hi"}`, http.StatusOK, true},
		{"POST", "/v1/legal/ingest", `{"text":"surge cap of 1.5x"}`, http.StatusOK, true},
		{"GET", "/v1/legal/jurisdictions", "", http.StatusOK, true},
		{"GET", "/v1/ops/report", "", http.StatusOK, true},
		{"POST", "/v1/ops/report/run", "", http.StatusOK, true},
	} {
		before := len(proxied)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.code {
			t.Errorf("%s %s = %d (%s), want %d", tc.method, tc.path, rec.Code, rec.Body.String(), tc.code)
		}
		if got := len(proxied) > before; got != tc.proxied {
			t.Errorf("%s %s proxied = %v, want %v", tc.method, tc.path, got, tc.proxied)
		}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/scheduled?rider_id=rider_priya", nil))
	if !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Fatalf("scheduled list: %s", rec.Body.String())
	}
}

func TestWithSchedulerUsesTheGivenScheduler(t *testing.T) {
	s := scheduler.New(scheduler.Options{Demo: true})
	router := server.NewRouter(testConfig(), server.WithScheduler(s))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/scheduled", strings.NewReader(
		`{"plan_id":"p2","rider_id":"r","legs":[{"tier":"mini","pickup":{"lat":12.97,"lng":77.59},
		"dropoff":{"lat":12.99,"lng":77.6},"pickup_at":"2030-01-01T10:00:00Z"}]}`)))
	if rec.Code != http.StatusCreated || len(s.Plan("p2")) != 1 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	cfg := testConfig()
	cfg.AgentsURL = "://bad"
	bad := server.NewRouter(cfg)
	rec = httptest.NewRecorder()
	bad.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/pricing/quote", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("pricing without agents proxy = %d", rec.Code)
	}
}
