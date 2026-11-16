package config

import (
	"strings"
	"testing"
)

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

func TestLoadCORSOrigins(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	if got := Load().CORSOrigins; !strings.Contains(got, "https://hexago-web.onrender.com") || !strings.Contains(got, "http://localhost:*") {
		t.Errorf("default CORSOrigins = %q", got)
	}
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://example.com")
	if got := Load().CORSOrigins; got != "https://example.com" {
		t.Errorf("override CORSOrigins = %q", got)
	}
}
