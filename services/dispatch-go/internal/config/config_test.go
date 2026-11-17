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

func TestLoadDispatchSettings(t *testing.T) {
	tests := []struct {
		res, speed string
		wantRes    int
		wantSpeed  float64
	}{
		{"", "", 9, 0.6},
		{"8", "0.5", 8, 0.5},
		{"16", "1.5", 9, 0.6},
		{"abc", "-1", 9, 0.6},
		{"0", "1", 0, 1},
	}
	for _, tc := range tests {
		t.Setenv("H3_RESOLUTION", tc.res)
		t.Setenv("CITY_SPEED_FACTOR", tc.speed)
		c := Load()
		if c.H3Resolution != tc.wantRes || c.CitySpeedFactor != tc.wantSpeed {
			t.Errorf("H3_RESOLUTION=%q CITY_SPEED_FACTOR=%q: got %d/%v", tc.res, tc.speed, c.H3Resolution, c.CitySpeedFactor)
		}
	}
}

func TestLoadStorageURLsDefaultEmpty(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_URL", "")
	if c := Load(); c.DatabaseURL != "" || c.RedisURL != "" {
		t.Errorf("unset storage URLs should stay empty (in-memory mode): %q %q", c.DatabaseURL, c.RedisURL)
	}
}

func TestLoadMaxPickupKm(t *testing.T) {
	for in, want := range map[string]float64{"": 5, "3.5": 3.5, "0": 5, "-2": 5, "abc": 5, "80": 5, "25": 25} {
		t.Setenv("MAX_PICKUP_KM", in)
		if got := Load().MaxPickupKm; got != want {
			t.Errorf("MAX_PICKUP_KM=%q: got %v, want %v", in, got, want)
		}
	}
}
