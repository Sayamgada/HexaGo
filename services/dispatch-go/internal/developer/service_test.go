package developer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

type harness struct {
	t     *testing.T
	svc   *Service
	srv   *httptest.Server
	clock *fakeClock
}

func newHarness(t *testing.T, o Options) *harness {
	t.Helper()
	clock := newClock()
	if o.Now == nil {
		o.Now = clock.Now
	}
	o.Logger = quietLogger()
	o.Webhooks.AllowPrivate = true
	o.Webhooks.Backoff = time.Millisecond
	if o.Origin == nil {
		o.Origin = func(origin string) bool { return origin == "https://hexago-web.onrender.com" }
	}
	s := New(o)
	r := chi.NewRouter()
	r.Use(s.Middleware)
	s.Routes(r)
	r.Get("/v1/rides/{id}", func(w http.ResponseWriter, r *http.Request) {
		k, ok := KeyFrom(r.Context())
		writeJSON(w, 200, map[string]any{"id": chi.URLParam(r, "id"), "keyed": ok, "key_id": k.ID})
	})
	r.Post("/v1/rides", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 201, map[string]any{"ok": true}) })
	r.Get("/v1/stream", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"stream": true}) })
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	srv := httptest.NewServer(r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })
	return &harness{t: t, svc: s, srv: srv, clock: clock}
}

func (h *harness) do(method, path, key string, body any, hdr ...string) (*http.Response, map[string]any) {
	h.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *harness) newKey(in CreateKeyInput) string {
	h.t.Helper()
	resp, out := h.do("POST", "/v1/developer/keys", "", in)
	if resp.StatusCode != 201 {
		h.t.Fatalf("create key: %d %v", resp.StatusCode, out)
	}
	return out["api_key"].(string)
}

func TestCreateKeyAndUseIt(t *testing.T) {
	h := newHarness(t, Options{})
	resp, out := h.do("POST", "/v1/developer/keys", "", map[string]any{"name": "my app", "scopes": []string{"rides:read", "rides:read"}})
	if resp.StatusCode != 201 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	key := out["api_key"].(string)
	if !strings.HasPrefix(key, KeyPrefix) || out["prefix"] != key[:len(KeyPrefix)+6] || out["warning"] == "" ||
		out["rate_limit_per_min"].(float64) != 60 || len(out["scopes"].([]any)) != 1 || out["name"] != "my app" {
		t.Fatalf("created %v", out)
	}
	if _, leaked := out["hash"]; leaked {
		t.Fatal("hash leaked")
	}
	// The store has only the hash.
	stored, err := h.svc.store.KeyByHash(context.Background(), HashKey(key))
	if err != nil || stored.Hash == key {
		t.Fatalf("stored %+v %v", stored, err)
	}

	resp, out = h.do("GET", "/v1/rides/r1", key, nil)
	if resp.StatusCode != 200 || out["keyed"] != true || out["key_id"] != stored.ID {
		t.Fatalf("keyed read: %d %v", resp.StatusCode, out)
	}
	if resp.Header.Get("X-RateLimit-Limit") != "60" || resp.Header.Get("X-RateLimit-Remaining") != "59" ||
		resp.Header.Get("X-RateLimit-Reset") != "1" {
		t.Fatalf("rate headers %v", resp.Header)
	}
	// Scope check.
	resp, out = h.do("POST", "/v1/rides", key, map[string]any{})
	if resp.StatusCode != 403 || out["required_scope"] != ScopeRidesWrite {
		t.Fatalf("missing scope: %d %v", resp.StatusCode, out)
	}
	// X-API-Key and ?api_key= work too.
	if resp, _ := h.do("GET", "/v1/rides/r1", "", nil, "X-API-Key", key); resp.StatusCode != 200 {
		t.Fatalf("X-API-Key: %d", resp.StatusCode)
	}
	if resp, out := h.do("GET", "/v1/stream?api_key="+key, "", nil); resp.StatusCode != 403 || out["required_scope"] != ScopeStreamRead {
		t.Fatalf("query key: %d %v", resp.StatusCode, out)
	}
	// /v1/developer/keys/me
	resp, out = h.do("GET", "/v1/developer/keys/me", key, nil)
	if resp.StatusCode != 200 || out["id"] != stored.ID {
		t.Fatalf("me: %d %v", resp.StatusCode, out)
	}
}

func TestDefaultKeyHasEveryScope(t *testing.T) {
	h := newHarness(t, Options{})
	resp, out := h.do("POST", "/v1/developer/keys", "", nil)
	if resp.StatusCode != 201 || len(out["scopes"].([]any)) != 6 || out["name"] != "demo key" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
}

func TestCreateKeyValidation(t *testing.T) {
	h := newHarness(t, Options{KeyCreationPerMin: 100})
	for name, body := range map[string]any{
		"unknown scope": map[string]any{"scopes": []string{"admin"}},
		"long name":     map[string]any{"name": strings.Repeat("x", 81)},
		"limit too big": map[string]any{"rate_limit_per_min": 601},
		"limit < 0":     map[string]any{"rate_limit_per_min": -1},
		"unknown field": map[string]any{"role": "admin"},
	} {
		if resp, out := h.do("POST", "/v1/developer/keys", "", body); resp.StatusCode != 400 || out["error"] == "" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, out)
		}
	}
}

func TestKeyCreationIsRateLimitedPerIP(t *testing.T) {
	h := newHarness(t, Options{KeyCreationPerMin: 2})
	h.newKey(CreateKeyInput{})
	h.newKey(CreateKeyInput{})
	resp, _ := h.do("POST", "/v1/developer/keys", "", nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third key: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestPerKeyRateLimit(t *testing.T) {
	h := newHarness(t, Options{})
	key := h.newKey(CreateKeyInput{RateLimitPerMin: 3})
	for i := 0; i < 3; i++ {
		if resp, _ := h.do("GET", "/v1/rides/r1", key, nil); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	resp, out := h.do("GET", "/v1/rides/r1", key, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "20" || resp.Header.Get("X-RateLimit-Remaining") != "0" ||
		out["retry_after_s"].(float64) != 20 {
		t.Fatalf("over limit: %d %v %v", resp.StatusCode, resp.Header, out)
	}
	h.clock.Add(20 * time.Second)
	if resp, _ := h.do("GET", "/v1/rides/r1", key, nil); resp.StatusCode != 200 {
		t.Fatalf("after refill: %d", resp.StatusCode)
	}
	// Another key is unaffected.
	other := h.newKey(CreateKeyInput{})
	if resp, _ := h.do("GET", "/v1/rides/r1", other, nil); resp.StatusCode != 200 {
		t.Fatalf("other key: %d", resp.StatusCode)
	}
}

func TestInvalidAndRevokedKeys(t *testing.T) {
	h := newHarness(t, Options{})
	for _, k := range []string{"hxg_" + strings.Repeat("0", 48), "not-a-key"} {
		resp, out := h.do("GET", "/v1/rides/r1", k, nil)
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" || out["error"] == "" {
			t.Fatalf("%s: %d %v", k, resp.StatusCode, out)
		}
	}
	key := h.newKey(CreateKeyInput{})
	if resp, _ := h.do("GET", "/v1/rides/r1", key, nil); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	resp, out := h.do("DELETE", "/v1/developer/keys/me", key, nil)
	if resp.StatusCode != 200 || out["revoked"] != true {
		t.Fatalf("revoke: %d %v", resp.StatusCode, out)
	}
	if resp, _ := h.do("GET", "/v1/rides/r1", key, nil); resp.StatusCode != 401 {
		t.Fatalf("revoked key still works: %d", resp.StatusCode)
	}
}

func TestKeylessRequests(t *testing.T) {
	h := newHarness(t, Options{})
	// Native / server-to-server (no Origin): allowed.
	if resp, out := h.do("GET", "/v1/rides/r1", "", nil); resp.StatusCode != 200 || out["keyed"] != false {
		t.Fatalf("no origin: %d %v", resp.StatusCode, out)
	}
	// First-party web app: allowed.
	if resp, _ := h.do("POST", "/v1/rides", "", map[string]any{}, "Origin", "https://hexago-web.onrender.com"); resp.StatusCode != 201 {
		t.Fatalf("first-party: %d", resp.StatusCode)
	}
	// Same origin (Swagger UI on the gateway): allowed.
	if resp, _ := h.do("GET", "/v1/rides/r1", "", nil, "Origin", h.srv.URL); resp.StatusCode != 200 {
		t.Fatalf("same origin: %d", resp.StatusCode)
	}
	// Third-party origin without a key: 401; public routes stay open.
	if resp, out := h.do("GET", "/v1/rides/r1", "", nil, "Origin", "https://evil.example"); resp.StatusCode != 401 ||
		!strings.Contains(out["error"].(string), "/v1/developer/keys") {
		t.Fatalf("third party: %d %v", resp.StatusCode, out)
	}
	if resp, _ := h.do("GET", "/healthz", "", nil, "Origin", "https://evil.example"); resp.StatusCode != 200 {
		t.Fatalf("public route: %d", resp.StatusCode)
	}
	if resp, _ := h.do("POST", "/v1/developer/keys", "", nil, "Origin", "https://evil.example"); resp.StatusCode != 201 {
		t.Fatalf("key creation from third party: %d", resp.StatusCode)
	}
	// Preflights are left to the CORS middleware.
	if resp, _ := h.do("OPTIONS", "/v1/rides/r1", "", nil, "Origin", "https://evil.example"); resp.StatusCode == 401 {
		t.Fatal("preflight rejected")
	}
	// Keyed developer routes need a key.
	for _, p := range []string{"/v1/developer/keys/me", "/v1/developer/webhooks"} {
		if resp, _ := h.do("GET", p, "", nil); resp.StatusCode != 401 {
			t.Fatalf("%s without key: %d", p, resp.StatusCode)
		}
	}
}

func TestScopesEndpoint(t *testing.T) {
	h := newHarness(t, Options{})
	resp, out := h.do("GET", "/v1/developer/scopes", "", nil)
	if resp.StatusCode != 200 || len(out["scopes"].(map[string]any)) != 6 || len(out["events"].([]any)) != 3 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
}

func TestWebhookLifecycle(t *testing.T) {
	rcv, target := newReceiver(200)
	defer target.Close()
	h := newHarness(t, Options{})
	key := h.newKey(CreateKeyInput{})
	other := h.newKey(CreateKeyInput{})

	resp, out := h.do("POST", "/v1/developer/webhooks", key, map[string]any{"url": target.URL, "events": []string{"ride.completed", "payout.sent", "ride.completed"}})
	if resp.StatusCode != 201 || !strings.HasPrefix(out["secret"].(string), "whsec_") || len(out["events"].([]any)) != 2 {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	id, secret := out["id"].(string), out["secret"].(string)

	resp, out = h.do("GET", "/v1/developer/webhooks", key, nil)
	if resp.StatusCode != 200 || out["count"].(float64) != 1 {
		t.Fatalf("list: %d %v", resp.StatusCode, out)
	}
	if strings.Contains(mustJSON(out), secret) {
		t.Fatal("secret listed")
	}
	if _, out := h.do("GET", "/v1/developer/webhooks", other, nil); out["count"].(float64) != 0 {
		t.Fatal("other key sees the webhook")
	}

	// A real event reaches the receiver, signed with the secret.
	h.svc.Webhooks().Emit(context.Background(), EventPayoutSent, map[string]any{"amount_paise": 100})
	rcv.wait(t, 1)
	req := rcv.requests[0]
	if !Verify(secret, mustInt(req.Header.Get(HeaderTimestamp)), rcv.bodies[0], req.Header.Get(HeaderSignature)) {
		t.Fatal("signature does not verify with the returned secret")
	}

	// Test ping and delivery log.
	resp, out = h.do("POST", "/v1/developer/webhooks/"+id+"/test", key, nil)
	if resp.StatusCode != 202 || out["queued"] != true {
		t.Fatalf("test: %d %v", resp.StatusCode, out)
	}
	rcv.wait(t, 1)
	waitFor(t, func() bool { return len(h.svc.Webhooks().History(id)) == 2 })
	resp, out = h.do("GET", "/v1/developer/webhooks/"+id+"/deliveries", key, nil)
	if resp.StatusCode != 200 || out["count"].(float64) != 2 {
		t.Fatalf("deliveries: %d %v", resp.StatusCode, out)
	}
	if resp, _ := h.do("GET", "/v1/developer/webhooks/"+id+"/deliveries", other, nil); resp.StatusCode != 404 {
		t.Fatalf("other key reads deliveries: %d", resp.StatusCode)
	}

	// Delete.
	if resp, _ := h.do("DELETE", "/v1/developer/webhooks/"+id, other, nil); resp.StatusCode != 404 {
		t.Fatalf("other key deletes: %d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/v1/developer/webhooks/"+id, key, nil); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if subs, _ := h.svc.Webhooks().Subscribers(context.Background(), EventPayoutSent); len(subs) != 0 {
		t.Fatal("subscriber cache not invalidated")
	}
}

func TestWebhookValidation(t *testing.T) {
	h := newHarness(t, Options{})
	key := h.newKey(CreateKeyInput{})
	for name, body := range map[string]any{
		"no events":     map[string]any{"url": "https://x.example"},
		"bad event":     map[string]any{"url": "https://x.example", "events": []string{"ride.exploded"}},
		"bad url":       map[string]any{"url": "ftp://x", "events": []string{"ride.completed"}},
		"unknown field": map[string]any{"url": "https://x.example", "events": []string{"ride.completed"}, "secret": "mine"},
	} {
		if resp, out := h.do("POST", "/v1/developer/webhooks", key, body); resp.StatusCode != 400 {
			t.Errorf("%s: %d %v", name, resp.StatusCode, out)
		}
	}
	for i := 0; i < maxWebhooksPerKey; i++ {
		if resp, _ := h.do("POST", "/v1/developer/webhooks", key, map[string]any{"url": "https://x.example", "events": []string{"ride.completed"}}); resp.StatusCode != 201 {
			t.Fatalf("webhook %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := h.do("POST", "/v1/developer/webhooks", key, map[string]any{"url": "https://x.example", "events": []string{"ride.completed"}}); resp.StatusCode != 400 {
		t.Fatalf("over the cap: %d", resp.StatusCode)
	}
	if resp, _ := h.do("POST", "/v1/developer/webhooks/wh_nope/test", key, nil); resp.StatusCode != 404 {
		t.Fatalf("unknown webhook: %d", resp.StatusCode)
	}
}

// brokenStore fails every call.
type brokenStore struct{ Memory }

var errBroken = errors.New("store down")

func (*brokenStore) KeyByHash(context.Context, string) (Key, error)         { return Key{}, errBroken }
func (*brokenStore) CreateKey(context.Context, Key) error                   { return errBroken }
func (*brokenStore) Webhooks(context.Context, string) ([]Webhook, error)    { return nil, errBroken }
func (*brokenStore) RevokeKey(context.Context, string, time.Time) error     { return errBroken }
func (*brokenStore) DeleteWebhook(context.Context, string, string) error    { return errBroken }
func (*brokenStore) CreateWebhook(context.Context, Webhook) error           { return errBroken }
func (*brokenStore) WebhooksFor(context.Context, string) ([]Webhook, error) { return nil, errBroken }

func TestStoreFailures(t *testing.T) {
	h := newHarness(t, Options{Store: &brokenStore{}})
	if resp, _ := h.do("POST", "/v1/developer/keys", "", nil); resp.StatusCode != 500 {
		t.Fatalf("create with broken store: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/rides/r1", "hxg_"+strings.Repeat("1", 48), nil); resp.StatusCode != 503 {
		t.Fatalf("lookup with broken store: %d", resp.StatusCode)
	}

	// Handlers that run after authentication: authenticate through a working
	// store, then break it.
	mem := NewMemory()
	sw := &switchStore{Store: mem}
	h2 := newHarness(t, Options{Store: sw})
	key := h2.newKey(CreateKeyInput{})
	_, _ = h2.do("GET", "/v1/developer/keys/me", key, nil) // warm the key cache
	sw.Store = &brokenStore{}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/developer/webhooks"}, {"DELETE", "/v1/developer/keys/me"}, {"DELETE", "/v1/developer/webhooks/x"},
		{"GET", "/v1/developer/webhooks/x/deliveries"},
	} {
		if resp, _ := h2.do(c.method, c.path, key, nil); resp.StatusCode != 500 {
			t.Errorf("%s %s: %d", c.method, c.path, resp.StatusCode)
		}
	}
	if resp, _ := h2.do("POST", "/v1/developer/webhooks", key, map[string]any{"url": "https://x.example", "events": []string{"ride.completed"}}); resp.StatusCode != 500 {
		t.Errorf("create webhook: %d", resp.StatusCode)
	}
	sw.Store = &createFails{Memory: mem}
	if resp, _ := h2.do("POST", "/v1/developer/webhooks", key, map[string]any{"url": "https://x.example", "events": []string{"ride.completed"}}); resp.StatusCode != 500 {
		t.Errorf("create webhook (insert fails): %d", resp.StatusCode)
	}
}

type switchStore struct{ Store }

type createFails struct{ *Memory }

func (*createFails) CreateWebhook(context.Context, Webhook) error { return errBroken }

func TestKeyCacheExpiresAndBounds(t *testing.T) {
	h := newHarness(t, Options{})
	key := h.newKey(CreateKeyInput{})
	if _, ok, _ := h.svc.lookup(context.Background(), key); !ok {
		t.Fatal("lookup")
	}
	// Revoked directly in the store: the cache hides it until the TTL passes.
	k, _ := h.svc.store.KeyByHash(context.Background(), HashKey(key))
	_ = h.svc.store.RevokeKey(context.Background(), k.ID, h.clock.Now())
	if _, ok, _ := h.svc.lookup(context.Background(), key); !ok {
		t.Fatal("cache not used")
	}
	h.clock.Add(keyCacheTTL)
	if _, ok, _ := h.svc.lookup(context.Background(), key); ok {
		t.Fatal("cache did not expire")
	}
	for i := 0; i < 10001; i++ {
		h.svc.cache[string(rune(i))] = cachedKey{}
	}
	_, _, _ = h.svc.lookup(context.Background(), "hxg_"+strings.Repeat("2", 48))
	if len(h.svc.cache) != 1 {
		t.Fatalf("cache not reset: %d", len(h.svc.cache))
	}
}

func TestHelpers(t *testing.T) {
	s := New(Options{})
	if s.StoreKind() != "memory" || s.LimiterKind() != "memory" || s.origin("x") || s.log == nil || s.perIP != 5 {
		t.Fatal("defaults")
	}
	if _, ok := KeyFrom(context.Background()); ok {
		t.Fatal("KeyFrom on empty context")
	}
	if clientIP(&http.Request{RemoteAddr: "garbage"}) != "garbage" {
		t.Fatal("clientIP fallback")
	}
	rec := httptest.NewRecorder()
	SetRateHeaders(rec.Header(), Decision{Allowed: false, Limit: 5, Remaining: -1, RetryAfter: 10 * time.Millisecond})
	if rec.Header().Get("X-RateLimit-Remaining") != "0" || rec.Header().Get("Retry-After") != "1" {
		t.Fatal(rec.Header())
	}
	if sameOrigin(&http.Request{Host: "a"}, "::bad::") {
		t.Fatal("bad origin")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Basic abc")
	if presentedKey(r) != "" {
		t.Fatal("basic auth taken as key")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func mustInt(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}
