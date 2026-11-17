package config

import (
	"testing"
	"time"
)

func TestLoadSimDefaults(t *testing.T) {
	for _, k := range []string{"SIM_DRIVERS", "SIM_DEMAND_PER_MIN", "OSRM_URL", "OSRM_RPS", "TELEMETRY_FLUSH_INTERVAL", "TELEMETRY_RETENTION"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.SimDrivers != 300 {
		t.Errorf("SimDrivers %d", c.SimDrivers)
	}
	if c.SimDemandPerMin != 0 { // 0 = auto (sized to the fleet)
		t.Errorf("SimDemandPerMin %v", c.SimDemandPerMin)
	}
	if c.OSRMURL != "https://router.project-osrm.org" {
		t.Errorf("OSRMURL %q", c.OSRMURL)
	}
	if c.OSRMRPS != 1 {
		t.Errorf("OSRMRPS %v", c.OSRMRPS)
	}
	if c.TelemetryFlush != 5*time.Second || c.TelemetryRetention != 2*time.Hour {
		t.Errorf("telemetry %v / %v", c.TelemetryFlush, c.TelemetryRetention)
	}
}

func TestLoadSimOverridesAndBadValues(t *testing.T) {
	t.Setenv("SIM_DRIVERS", "50")
	t.Setenv("SIM_DEMAND_PER_MIN", "2.5")
	t.Setenv("OSRM_URL", "off")
	t.Setenv("OSRM_RPS", "0.5")
	t.Setenv("TELEMETRY_FLUSH_INTERVAL", "10s")
	t.Setenv("TELEMETRY_RETENTION", "30m")
	c := Load()
	if c.SimDrivers != 50 || c.SimDemandPerMin != 2.5 || c.OSRMURL != "" || c.OSRMRPS != 0.5 ||
		c.TelemetryFlush != 10*time.Second || c.TelemetryRetention != 30*time.Minute {
		t.Fatalf("overrides not applied: %+v", c)
	}

	t.Setenv("SIM_DRIVERS", "99999")
	t.Setenv("SIM_DEMAND_PER_MIN", "-1")
	t.Setenv("OSRM_RPS", "zero")
	t.Setenv("TELEMETRY_FLUSH_INTERVAL", "soon")
	t.Setenv("TELEMETRY_RETENTION", "-5m")
	c = Load()
	if c.SimDrivers != 300 || c.SimDemandPerMin != 0 || c.OSRMRPS != 1 ||
		c.TelemetryFlush != 5*time.Second || c.TelemetryRetention != 2*time.Hour {
		t.Fatalf("bad values not defaulted: %+v", c)
	}
}

func TestLoadSimDemandAuto(t *testing.T) {
	t.Setenv("SIM_DEMAND_PER_MIN", "auto")
	if c := Load(); c.SimDemandPerMin != 0 {
		t.Fatalf("auto -> %v, want 0", c.SimDemandPerMin)
	}
}
