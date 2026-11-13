package config

import "testing"

func TestLoadAgentsURLNormalizesScheme(t *testing.T) {
	cases := map[string]string{
		"":                           "http://localhost:8000",
		"agents-py-abc:8000":         "http://agents-py-abc:8000",
		"http://agents-py:8000":      "http://agents-py:8000",
		"https://agents.example.com": "https://agents.example.com",
	}
	for in, want := range cases {
		t.Setenv("AGENTS_URL", in)
		if got := Load().AgentsURL; got != want {
			t.Errorf("AGENTS_URL=%q: got %q, want %q", in, got, want)
		}
	}
}
