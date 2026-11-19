package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hexago/hexago/services/dispatch-go/internal/server"
)

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/stream"
}

func TestStreamAcceptsWebAppOrigin(t *testing.T) {
	srv := httptest.NewServer(server.NewRouter(testConfig()))
	defer srv.Close()

	allowed := []string{"https://hexago-web.onrender.com", "http://localhost:5173", ""}
	for _, origin := range allowed {
		h := http.Header{}
		if origin != "" {
			h.Set("Origin", origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), h)
		if err != nil {
			t.Fatalf("origin %q rejected: %v (%v)", origin, err, resp)
		}
		conn.Close()
	}

	h := http.Header{"Origin": []string{"https://evil.example.com"}}
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), h)
	if err == nil {
		t.Fatal("foreign origin was allowed to open the stream")
	}
	// Keyless third-party origins are stopped by the API-key middleware (401)
	// before the upgrader's origin check (403).
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("foreign origin: %v", resp)
	}
}

func TestStreamFromForeignOriginWithKey(t *testing.T) {
	srv := httptest.NewServer(server.NewRouter(testConfig()))
	defer srv.Close()
	key := createKey(t, srv.URL, `{"scopes":["stream:read"]}`)
	h := http.Header{"Origin": []string{"https://partner.example.com"}}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv)+"?api_key="+key, h)
	if err != nil {
		t.Fatalf("keyed foreign origin rejected: %v (%v)", err, resp)
	}
	conn.Close()
	// A key without stream:read is refused.
	other := createKey(t, srv.URL, `{"scopes":["rides:read"]}`)
	_, resp, err = websocket.DefaultDialer.Dial(wsURL(srv)+"?api_key="+other, nil)
	if err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("key without stream:read: %v %v", err, resp)
	}
}

func TestStreamPushesOpsMetrics(t *testing.T) {
	srv := httptest.NewServer(server.NewRouter(testConfig()))
	defer srv.Close()
	h := http.Header{"Origin": []string{"https://hexago-web.onrender.com"}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv), h)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"action": "subscribe", "topics": []string{"drivers", "ops"}}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("no metrics frame: %v", err)
		}
		s := string(b)
		if strings.HasPrefix(s, `{"topic":"ops","data":{"type":"metrics","p50_ms":`) &&
			strings.Contains(s, `"rides_in_progress":`) {
			return
		}
	}
}

func TestSimRoutesWithoutSimulator(t *testing.T) {
	router := server.NewRouter(testConfig())
	for _, path := range []string{"/v1/sim/scenario/rush_hour", "/v1/sim/config"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
}
