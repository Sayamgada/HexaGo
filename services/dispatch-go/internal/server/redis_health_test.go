package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/hexago/hexago/services/dispatch-go/internal/redisdiag"
	"github.com/hexago/hexago/services/dispatch-go/internal/server"
)

func healthRedis(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	r, ok := body["redis"].(map[string]any)
	if !ok {
		t.Fatalf("no redis block in %s", rec.Body.String())
	}
	return r
}

func TestHealthzRedisNotConfigured(t *testing.T) {
	r := healthRedis(t, server.NewRouter(testConfig()), "/healthz")
	if r["configured"] != false || r["ok"] != false {
		t.Errorf("redis = %v", r)
	}
}

func TestHealthzRedisMonitor(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("hunter2")
	m := redisdiag.NewMonitor("redis://:hunter2@"+mr.Addr()+"/0", time.Hour, time.Second)
	h := server.NewRouter(testConfig(), server.WithRedisMonitor(m))

	// Before any check the block says so rather than blocking on Redis.
	if r := healthRedis(t, h, "/healthz"); r["configured"] != true || r["checked_at"] != nil {
		t.Errorf("before first check: %v", r)
	}
	m.CheckNow(context.Background())
	r := healthRedis(t, h, "/healthz")
	if r["ok"] != true || r["scheme"] != "redis" || r["host"] != "127.0.0.1" || r["pubsub_roundtrip_ms"] == nil {
		t.Errorf("after check: %v", r)
	}
	if u, _ := r["url"].(string); strings.Contains(u, "hunter2") || !strings.Contains(u, "****") {
		t.Errorf("url not masked: %q", u)
	}

	mr.Close()
	if r := healthRedis(t, h, "/healthz"); r["ok"] != true {
		t.Errorf("plain /healthz serves the cached result: %v", r)
	}
	r = healthRedis(t, h, "/healthz?redis=fresh")
	if r["ok"] != false || r["failed_stage"] != "ping" || r["hint"] == "" {
		t.Errorf("fresh check after shutdown: %v", r)
	}
}
