package config

import (
	"os"
	"strconv"
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

	return &Config{
		Port:          port,
		AgentsURL:     getEnv("AGENTS_URL", "http://localhost:8000"),
		DatabaseURL:   getEnv("DATABASE_URL", "postgres://hexago:hexago@localhost:5432/hexago?sslmode=disable"),
		RedisURL:      getEnv("REDIS_URL", "redis://localhost:6379/0"),
		DemoMode:      demoMode,
		CityCenterLat: lat,
		CityCenterLng: lng,
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}

