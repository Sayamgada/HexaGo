package developer

import "testing"

func TestRequiredScope(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"GET", "/healthz", ""},
		{"GET", "/metrics", ""},
		{"GET", "/openapi.json", ""},
		{"GET", "/docs", ""},
		{"GET", "/v1/ping", ""},
		{"GET", "/track/abc", ""},
		{"POST", "/v1/developer/keys", ""},
		{"GET", "/v1/stream", ScopeStreamRead},
		{"GET", "/v1/ws", ScopeStreamRead},
		{"GET", "/v1/pricing/tiers", ScopePricingRead},
		{"POST", "/v1/pricing/quote", ScopePricingRead},
		{"GET", "/v1/pricing/surge-map", ScopePricingRead},
		{"GET", "/v1/agents", ScopeAgentsInvoke},
		{"POST", "/v1/agents/booking/parse", ScopeAgentsInvoke},
		{"GET", "/v1/ledger/summary", ScopeLedgerRead},
		{"GET", "/v1/wallets/r1", ScopeLedgerRead},
		{"POST", "/v1/ledger/refunds", ScopeRidesWrite},
		{"POST", "/v1/wallets/r1/topup", ScopeRidesWrite},
		{"GET", "/v1/safety/incidents", ScopeRidesRead},
		{"POST", "/v1/safety/checkins/c1/reply", ScopeRidesWrite},
		{"POST", "/v1/safety/incidents/i1/resolve", ScopeAgentsInvoke},
		{"POST", "/v1/safety/drivers/d1/suspend", ScopeAgentsInvoke},
		{"GET", "/v1/rides/r1", ScopeRidesRead},
		{"POST", "/v1/rides", ScopeRidesWrite},
		{"POST", "/v1/rides/r1/cancel", ScopeRidesWrite},
		{"POST", "/v1/drivers/d1/status", ScopeRidesWrite},
		{"GET", "/v1/scheduled", ScopeRidesRead},
		{"POST", "/v1/scheduled", ScopeRidesWrite},
		{"GET", "/v1/eta", ScopeRidesRead},
		{"GET", "/v1/supply", ScopeRidesRead},
		{"GET", "/v1/metrics/dispatch", ScopeRidesRead},
		{"GET", "/v1/sim/config", ScopeRidesRead},
		{"POST", "/v1/sim/config", ScopeAgentsInvoke},
		{"POST", "/v1/sim/scenario/rush_hour", ScopeAgentsInvoke},
		{"GET", "/v1/unknown", ScopeRidesRead},
		{"DELETE", "/v1/unknown", ScopeRidesWrite},
		{"GET", "/v1/ridesharing", ScopeRidesRead}, // prefix match is per path segment
	}
	for _, c := range cases {
		if got := RequiredScope(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestScopeCatalogue(t *testing.T) {
	all := AllScopes()
	if len(all) != 6 || all[0] != ScopeAgentsInvoke {
		t.Fatalf("AllScopes = %v", all)
	}
	for _, s := range all {
		if !ValidScope(s) || ScopeInfo[s] == "" {
			t.Errorf("scope %s undocumented", s)
		}
	}
	if ValidScope("admin") {
		t.Fatal("admin is not a scope")
	}
	for _, e := range Events {
		if !ValidEvent(e) {
			t.Errorf("event %s", e)
		}
	}
	if ValidEvent(EventPing) {
		t.Fatal("ping is not subscribable")
	}
}
