package config

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"

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
	// MaxPickupKm caps the dispatch search radius (MAX_PICKUP_KM, (0,50], default 5).
	MaxPickupKm float64

	// Fleet simulator (runs when DemoMode is on).
	SimDrivers      int     // SIM_DRIVERS, 0..2000, default 300
	SimDemandPerMin float64 // SIM_DEMAND_PER_MIN, mean ride requests per minute; 0 or "auto" (default) sizes it to the fleet
	OSRMURL         string  // OSRM_URL, default public demo server; "off" = straight lines only
	OSRMRPS         float64 // OSRM_RPS, request rate limit, default 1 (public server policy)

	// Take-over mode: a person drives a simulated car from the Driver screen (TAKEOVER_ENABLED, default true).
	TakeoverEnabled bool

	// Telemetry persistence.
	TelemetryFlush     time.Duration // TELEMETRY_FLUSH_INTERVAL, default 5s
	TelemetryRetention time.Duration // TELEMETRY_RETENTION, default 2h (0 keeps everything)
}

// DefaultOSRMURL is the public OSRM demo server.
const DefaultOSRMURL = "https://router.project-osrm.org"

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

	pickup, err := strconv.ParseFloat(getEnv("MAX_PICKUP_KM", "5"), 64)
	if err != nil || pickup <= 0 || pickup > 50 {
		pickup = 5
	}

	simDrivers, err := strconv.Atoi(getEnv("SIM_DRIVERS", "300"))
	if err != nil || simDrivers < 0 || simDrivers > 2000 {
		simDrivers = 300
	}
	demand, err := strconv.ParseFloat(getEnv("SIM_DEMAND_PER_MIN", "auto"), 64)
	if err != nil || demand < 0 || math.IsNaN(demand) {
		demand = 0 // auto: main sizes demand to the fleet (sim.AutoDemandPerMin)
	}
	osrmURL := strings.TrimRight(getEnv("OSRM_URL", DefaultOSRMURL), "/")
	if strings.EqualFold(osrmURL, "off") {
		osrmURL = ""
	}
	rps, err := strconv.ParseFloat(getEnv("OSRM_RPS", "1"), 64)
	if err != nil || rps <= 0 {
		rps = 1
	}

	return &Config{
		TakeoverEnabled:    getEnv("TAKEOVER_ENABLED", "true") == "true",
		SimDrivers:         simDrivers,
		SimDemandPerMin:    demand,
		OSRMURL:            osrmURL,
		OSRMRPS:            rps,
		TelemetryFlush:     durationEnv("TELEMETRY_FLUSH_INTERVAL", 5*time.Second, false),
		TelemetryRetention: durationEnv("TELEMETRY_RETENTION", 2*time.Hour, true),

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
		MaxPickupKm:     pickup,
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

// durationEnv parses a Go duration; invalid or negative values (or zero unless
// allowZero) fall back to def.
func durationEnv(key string, def time.Duration, allowZero bool) time.Duration {
	d, err := time.ParseDuration(getEnv(key, def.String()))
	if err != nil || d < 0 || (d == 0 && !allowZero) {
		return def
	}
	return d
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}
