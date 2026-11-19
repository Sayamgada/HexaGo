// Package developer is HexaGo's public API platform: self-serve API keys
// (stored hashed), scopes, per-key token-bucket rate limiting (Redis, with an
// in-memory fallback) and signed webhooks.
//
// Requests without a key keep working: they are the first-party path (the web
// app, whose origin is on the CORS allow-list, the native apps and
// service-to-service calls, which send no Origin). A keyless browser request
// from an origin that is not on the allow-list is rejected with 401.
package developer

import (
	"net/http"
	"sort"
	"strings"
)

// Scopes.
const (
	ScopeRidesRead    = "rides:read"
	ScopeRidesWrite   = "rides:write"
	ScopePricingRead  = "pricing:read"
	ScopeStreamRead   = "stream:read"
	ScopeAgentsInvoke = "agents:invoke"
	ScopeLedgerRead   = "ledger:read"
)

// ScopeInfo describes every scope (served by GET /v1/developer/scopes).
var ScopeInfo = map[string]string{
	ScopeRidesRead:    "Read rides, scheduled legs, ETAs, cell supply, dispatch metrics, safety incidents and the simulator config.",
	ScopeRidesWrite:   "Request and cancel rides, update driver status, schedule legs, reply to safety check-ins, top up wallets and issue refunds.",
	ScopePricingRead:  "Fare quotes, the rate card and surge data (/v1/pricing/*).",
	ScopeStreamRead:   "Open the live WebSocket stream (/v1/stream, /v1/ws).",
	ScopeAgentsInvoke: "Call the agent swarm (/v1/agents/*: booking, roster, events, budget, settlement) and the safety/simulator controls.",
	ScopeLedgerRead:   "Read wallets, journal entries, payouts, the trial balance and the ledger summary.",
}

// AllScopes returns every scope, sorted.
func AllScopes() []string {
	out := make([]string, 0, len(ScopeInfo))
	for s := range ScopeInfo {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ValidScope reports whether s is a known scope.
func ValidScope(s string) bool { _, ok := ScopeInfo[s]; return ok }

// Public reports whether path needs no key and no scope at all (probes,
// documentation, developer self-service and the public tracking page).
func Public(path string) bool {
	switch path {
	case "/healthz", "/metrics", "/openapi.json", "/docs", "/docs/", "/v1/ping":
		return true
	}
	return strings.HasPrefix(path, "/track/") || strings.HasPrefix(path, "/v1/developer/")
}

// RequiredScope returns the scope a keyed request to method+path needs, or ""
// when the route is public (see Public) or unknown (it then 404s anyway).
func RequiredScope(method, path string) string {
	if Public(path) {
		return ""
	}
	read := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
	has := func(prefix string) bool { return path == prefix || strings.HasPrefix(path, prefix+"/") }
	switch {
	case path == "/v1/stream" || path == "/v1/ws":
		return ScopeStreamRead
	case has("/v1/pricing"):
		return ScopePricingRead
	case has("/v1/agents"), has("/v1/sim") && !read:
		return ScopeAgentsInvoke
	case has("/v1/ledger") || has("/v1/wallets"):
		if read {
			return ScopeLedgerRead
		}
		return ScopeRidesWrite // refunds and demo top-ups move rider money
	case has("/v1/safety"):
		if read {
			return ScopeRidesRead
		}
		if strings.HasPrefix(path, "/v1/safety/checkins/") && strings.HasSuffix(path, "/reply") {
			return ScopeRidesWrite // the rider answering a safety call
		}
		return ScopeAgentsInvoke // suspensions, emergency actions, resolve: agent/ops controls
	case has("/v1/rides"), has("/v1/drivers"), has("/v1/scheduled"), has("/v1/eta"), has("/v1/supply"),
		has("/v1/metrics"), has("/v1/sim"):
		if read {
			return ScopeRidesRead
		}
		return ScopeRidesWrite
	}
	if read {
		return ScopeRidesRead
	}
	return ScopeRidesWrite
}
