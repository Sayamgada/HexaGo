// Package cors implements an origin allowlist and CORS middleware for the gateway.
package cors

import (
	"net/http"
	"net/url"
	"strings"
)

// DefaultOrigins allows the hosted web app plus local Flutter dev servers on any port.
const DefaultOrigins = "https://hexago-web.onrender.com,http://localhost:*,http://127.0.0.1:*"

const (
	allowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	allowHeaders = "Accept, Authorization, Content-Type, X-Request-ID"
	maxAge       = "600"
)

type pattern struct {
	scheme string
	host   string
	port   string // empty = no explicit port, "*" = any port
}

// Allowlist matches browser Origin headers against configured patterns.
// A pattern is an exact origin ("https://app.example.com") or an origin whose
// port is "*" ("http://localhost:*"), which matches that host on any port or none.
type Allowlist struct {
	patterns []pattern
}

// Parse builds an Allowlist from a comma-separated list of origin patterns.
// Blank and malformed entries are skipped.
func Parse(list string) *Allowlist {
	a := &Allowlist{}
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "/")
		if raw == "" {
			continue
		}
		anyPort := strings.HasSuffix(raw, ":*")
		if anyPort {
			raw = strings.TrimSuffix(raw, ":*")
		}
		p, ok := split(raw)
		if !ok {
			continue
		}
		if anyPort {
			p.port = "*"
		}
		a.patterns = append(a.patterns, p)
	}
	return a
}

func split(origin string) (pattern, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return pattern{}, false
	}
	return pattern{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   u.Port(),
	}, true
}

// Allowed reports whether the given Origin header value is on the allowlist.
func (a *Allowlist) Allowed(origin string) bool {
	o, ok := split(origin)
	if !ok {
		return false
	}
	for _, p := range a.patterns {
		if p.scheme == o.scheme && p.host == o.host && (p.port == "*" || p.port == o.port) {
			return true
		}
	}
	return false
}

// Middleware adds CORS headers for allowed origins and answers preflight requests.
// Disallowed origins get no CORS headers, so the browser blocks the response.
func (a *Allowlist) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		allowed := a.Allowed(origin)
		if allowed {
			h.Set("Access-Control-Allow-Origin", origin)
		}

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			if allowed {
				h.Set("Access-Control-Allow-Methods", allowMethods)
				h.Set("Access-Control-Allow-Headers", allowHeaders)
				h.Set("Access-Control-Max-Age", maxAge)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// StripUpstream removes CORS headers set by a proxied backend so the gateway's
// own headers are the only ones the browser sees (duplicates make it reject the response).
func StripUpstream(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Access-Control-") {
			h.Del(k)
		}
	}
}
