package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
)

type mockRoundTripper func(req *http.Request) (*http.Response, error)

func (m mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m(req)
}

func testConfig() *config.Config {
	return &config.Config{
		Port:          8080,
		AgentsURL:     "http://mock-agents:8000",
		DatabaseURL:   "postgres://hexago:hexago@localhost:5432/hexago?sslmode=disable",
		RedisURL:      "redis://localhost:6379/0",
		DemoMode:      true,
		CityCenterLat: 12.9716,
		CityCenterLng: 77.5946,
		CORSOrigins:   "https://hexago-web.onrender.com,http://localhost:*",
	}
}

func TestHealthz(t *testing.T) {
	cfg := testConfig()
	router := server.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Failed to parse JSON body: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("Expected status 'ok', got %v", body["status"])
	}
	if body["service"] != "dispatch-go" {
		t.Errorf("Expected service 'dispatch-go', got %v", body["service"])
	}
}

func TestPing(t *testing.T) {
	cfg := testConfig()
	router := server.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Failed to parse JSON body: %v", err)
	}

	if body["message"] != "pong" {
		t.Errorf("Expected message 'pong', got %v", body["message"])
	}
	if body["service"] != "dispatch-go" {
		t.Errorf("Expected service 'dispatch-go', got %v", body["service"])
	}
}

func TestMetrics(t *testing.T) {
	cfg := testConfig()
	router := server.NewRouter(cfg)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}

	content := rec.Body.String()
	if !strings.Contains(content, "promhttp_") && !strings.Contains(content, "go_") {
		t.Errorf("Expected metrics response to contain promhttp or go metrics, got: %s", content[:min(200, len(content))])
	}
}

func TestAgentsReverseProxy(t *testing.T) {
	cfg := testConfig()

	// In-memory mock transport for reverse proxy
	mockTransport := mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v1/agents/ping":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"service":"agents-py","status":"ok"}`)),
			}, nil
		case "/v1/agents/echo":
			bodyBytes, _ := io.ReadAll(req.Body)
			hdr := http.Header{"Content-Type": []string{"application/json"}}
			if val := req.Header.Get("X-Custom-Test"); val != "" {
				hdr.Set("X-Custom-Echo", val)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     hdr,
				Body:       io.NopCloser(bytes.NewReader(bodyBytes)),
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		}
	})

	router := server.NewRouter(cfg, server.WithTransport(mockTransport))

	// 1. Test GET /v1/agents/ping
	getReq := httptest.NewRequest(http.MethodGet, "/v1/agents/ping", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Errorf("Expected status 200 from proxied GET, got %d", getRec.Code)
	}

	var pingBody map[string]interface{}
	if err := json.Unmarshal(getRec.Body.Bytes(), &pingBody); err != nil {
		t.Fatalf("Failed to decode proxied response: %v", err)
	}
	if pingBody["service"] != "agents-py" {
		t.Errorf("Expected service 'agents-py', got %v", pingBody["service"])
	}

	// 2. Test POST /v1/agents/echo
	reqBody := `{"query":"book a ride to Indiranagar"}`
	postReq := httptest.NewRequest(http.MethodPost, "/v1/agents/echo", strings.NewReader(reqBody))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("X-Custom-Test", "test-header-val")
	postRec := httptest.NewRecorder()
	router.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusOK {
		t.Errorf("Expected status 200 from proxied POST, got %d", postRec.Code)
	}

	if postRec.Header().Get("X-Custom-Echo") != "test-header-val" {
		t.Errorf("Expected custom header to be forwarded, got %s", postRec.Header().Get("X-Custom-Echo"))
	}

	if postRec.Body.String() != reqBody {
		t.Errorf("Expected body %s, got %s", reqBody, postRec.Body.String())
	}
}

func TestCORSOnProxiedAgents(t *testing.T) {
	cfg := testConfig()
	// Upstream sets its own CORS header; the gateway must replace it, not duplicate it.
	upstream := mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":                []string{"application/json"},
				"Access-Control-Allow-Origin": []string{"*"},
			},
			Body: io.NopCloser(strings.NewReader(`{"message":"pong"}`)),
		}, nil
	})
	router := server.NewRouter(cfg, server.WithTransport(upstream))

	req := httptest.NewRequest(http.MethodGet, "/v1/agents/ping", nil)
	req.Header.Set("Origin", "https://hexago-web.onrender.com")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "https://hexago-web.onrender.com" {
		t.Errorf("Access-Control-Allow-Origin = %v, want exactly the web origin", got)
	}

	// Preflight is answered by the gateway without reaching agents-py.
	pre := httptest.NewRequest(http.MethodOptions, "/v1/agents/parse-intent", nil)
	pre.Header.Set("Origin", "http://localhost:5173")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Access-Control-Request-Headers", "content-type")
	preRec := httptest.NewRecorder()
	router.ServeHTTP(preRec, pre)
	if preRec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", preRec.Code)
	}
	if preRec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("preflight ACAO = %q", preRec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
