package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
)

func setupTestServer(t *testing.T, agentsURL string) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Port:          8080,
		AgentsURL:     agentsURL,
		DatabaseURL:   "postgres://hexago:hexago@localhost:5432/hexago?sslmode=disable",
		RedisURL:      "redis://localhost:6379/0",
		DemoMode:      true,
		CityCenterLat: 12.9716,
		CityCenterLng: 77.5946,
	}

	router := server.NewRouter(cfg)
	return httptest.NewServer(router)
}

func TestHealthz(t *testing.T) {
	ts := setupTestServer(t, "http://localhost:8000")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("Failed to call /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
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
	ts := setupTestServer(t, "http://localhost:8000")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ping")
	if err != nil {
		t.Fatalf("Failed to call /v1/ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
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
	ts := setupTestServer(t, "http://localhost:8000")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("Failed to call /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "promhttp_") && !strings.Contains(content, "go_") {
		t.Errorf("Expected metrics response to contain promhttp or go metrics, got: %s", content[:min(200, len(content))])
	}
}

func TestAgentsReverseProxy(t *testing.T) {
	// Mock agents-py server
	mockAgents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/ping":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"service":"agents-py","status":"ok"}`))
		case "/v1/agents/echo":
			bodyBytes, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Custom-Echo", r.Header.Get("X-Custom-Test"))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bodyBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockAgents.Close()

	ts := setupTestServer(t, mockAgents.URL)
	defer ts.Close()

	// 1. Test GET /v1/agents/ping
	resp, err := http.Get(ts.URL + "/v1/agents/ping")
	if err != nil {
		t.Fatalf("Failed to GET /v1/agents/ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 from proxied route, got %d", resp.StatusCode)
	}

	var pingBody map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&pingBody); err != nil {
		t.Fatalf("Failed to decode proxied response: %v", err)
	}
	if pingBody["service"] != "agents-py" {
		t.Errorf("Expected service 'agents-py', got %v", pingBody["service"])
	}

	// 2. Test POST /v1/agents/echo
	reqBody := `{"query":"book a ride to Indiranagar"}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/agents/echo", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("Failed to build POST request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Test", "test-header-val")

	postResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to POST /v1/agents/echo: %v", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 from proxied POST, got %d", postResp.StatusCode)
	}

	if postResp.Header.Get("X-Custom-Echo") != "test-header-val" {
		t.Errorf("Expected custom header to be forwarded, got %s", postResp.Header.Get("X-Custom-Echo"))
	}

	respBytes, _ := io.ReadAll(postResp.Body)
	if string(respBytes) != reqBody {
		t.Errorf("Expected body %s, got %s", reqBody, string(respBytes))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

