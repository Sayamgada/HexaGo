package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowed(t *testing.T) {
	a := Parse(DefaultOrigins)
	cases := map[string]bool{
		"https://hexago-web.onrender.com":      true,
		"https://HEXAGO-WEB.onrender.com":      true,
		"http://hexago-web.onrender.com":       false, // wrong scheme
		"https://hexago-web.onrender.com:8443": false, // explicit port not allowed
		"https://evil.onrender.com":            false,
		"http://localhost:5173":                true,
		"http://localhost":                     true,
		"http://127.0.0.1:61234":               true,
		"https://localhost:3000":               false, // only http localhost is listed
		"http://localhost.evil.com":            false,
		"null":                                 false,
		"":                                     false,
	}
	for origin, want := range cases {
		if got := a.Allowed(origin); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestParseSkipsBlankAndMalformed(t *testing.T) {
	a := Parse(" https://a.example.com/ , ,not-a-url, https://b.example.com/path ")
	if len(a.patterns) != 1 {
		t.Fatalf("want 1 pattern, got %d: %+v", len(a.patterns), a.patterns)
	}
	if !a.Allowed("https://a.example.com") {
		t.Error("trailing slash entry should match its origin")
	}
}

func TestMiddleware(t *testing.T) {
	a := Parse(DefaultOrigins)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := a.Middleware(next)

	t.Run("allowed simple request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Origin", "https://hexago-web.onrender.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://hexago-web.onrender.com" {
			t.Errorf("ACAO = %q", got)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d", rec.Code)
		}
	})

	t.Run("disallowed origin gets no ACAO", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("ACAO = %q, want empty", got)
		}
	})

	t.Run("preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/x", nil)
		req.Header.Set("Origin", "http://localhost:5173")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Methods") == "" || rec.Header().Get("Access-Control-Allow-Headers") == "" {
			t.Errorf("missing preflight headers: %v", rec.Header())
		}
	})

	t.Run("no origin passes through untouched", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if len(rec.Header().Values("Vary")) != 0 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("unexpected CORS headers: %v", rec.Header())
		}
	})
}

func TestStripUpstream(t *testing.T) {
	h := http.Header{}
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Content-Type", "application/json")
	StripUpstream(h)
	if len(h) != 1 || h.Get("Content-Type") == "" {
		t.Errorf("unexpected headers after strip: %v", h)
	}
}
