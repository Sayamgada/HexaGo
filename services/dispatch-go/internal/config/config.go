package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/hexago/hexago/services/dispatch-go/internal/cors"
)

// Config represents runtime configuration loaded from environment variables.
type Config struct {
	Port          int
	AgentsURL     string
	DatabaseURL   string
	RedisURL      string
	DemoMode      bool
	CityCenterLat float64
	CityCenterLng float64
	CORSOrigins   string
	// H3Resolution is the driver index resolution (H3_RESOLUTION, 0..15, default 9).
	H3Resolution int
	// CitySpeedFactor scales driving speed for congestion in ETAs (CITY_SPEED_FACTOR, (0,1], default 0.6).
	CitySpeedFactor float64
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	port, err := strconv.Atoi(getEnv("PORT", "8080"))
	if err != nil {
		port = 8080
	}

	lat, err := strconv.ParseFloat(getEnv("CITY_CENTER_LAT", "12.9716"), 64)
	if err != nil {
		lat = 12.9716
	}

	lng, err := strconv.ParseFloat(getEnv("CITY_CENTER_LNG", "77.5946"), 64)
	if err != nil {
		lng = 77.5946
	}

	demoMode := getEnv("DEMO_MODE", "true") == "true"

	res, err := strconv.Atoi(getEnv("H3_RESOLUTION", "9"))
	if err != nil || res < 0 || res > 15 {
		res = 9
	}

	speed, err := strconv.ParseFloat(getEnv("CITY_SPEED_FACTOR", "0.6"), 64)
	if err != nil || speed <= 0 || speed > 1 {
		speed = 0.6
	}

	return &Config{
		Port:      port,
		AgentsURL: withScheme(getEnv("AGENTS_URL", "http://localhost:8000")),
		// Empty DATABASE_URL / REDIS_URL mean "run in memory" (local dev, tests).
		DatabaseURL:     getEnv("DATABASE_URL", ""),
		RedisURL:        getEnv("REDIS_URL", ""),
		DemoMode:        demoMode,
		CityCenterLat:   lat,
		CityCenterLng:   lng,
		CORSOrigins:     getEnv("CORS_ALLOWED_ORIGINS", cors.DefaultOrigins),
		H3Resolution:    res,
		CitySpeedFactor: speed,
	}
}

// withScheme prefixes http:// when the URL has no scheme, e.g. Render's
// fromService "hostport" value ("agents-py-xyz:8000").
func withScheme(u string) string {
	if strings.Contains(u, "://") {
		return u
	}
	return "http://" + u
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}
