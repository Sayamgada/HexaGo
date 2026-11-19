package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/sim"
)

// driverRow is a picker row plus the Safety Sentinel's suspension, if any.
type driverRow struct {
	sim.DriverSummary
	Suspended  bool   `json:"suspended"`
	IncidentID string `json:"incident_id,omitempty"`
}

type driverDetail struct {
	sim.DriverDetail
	Suspended  bool               `json:"suspended"`
	IncidentID string             `json:"incident_id,omitempty"`
	Suspension *safety.Suspension `json:"suspension,omitempty"`
}

// mountDrivers registers the Driver module's read-only endpoints:
//
//	GET /v1/drivers?tier=&q=      every simulated driver (q = id substring)
//	GET /v1/drivers/{id}          live state, current ride, AV panel, suspension
//
// With the simulator off they fall back to the dispatch fleet index (no ride detail).
func mountDrivers(r chi.Router, s *sim.Sim, svc *dispatch.Service, sf *safety.Service) {
	suspend := func(id string) (*safety.Suspension, bool) {
		if sf == nil {
			return nil, false
		}
		sp, ok := sf.Suspended(id)
		return &sp, ok
	}
	r.Get("/v1/drivers", func(w http.ResponseWriter, r *http.Request) {
		tier := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tier")))
		q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		var all []sim.DriverSummary
		if s != nil {
			all = s.Drivers()
		} else {
			for _, d := range svc.Fleet().Snapshot() {
				all = append(all, fleetSummary(d))
			}
			sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
		}
		rows := make([]driverRow, 0, len(all))
		for _, d := range all {
			if (tier != "" && d.Tier != tier) || (q != "" && !strings.Contains(strings.ToLower(d.ID), q)) {
				continue
			}
			row := driverRow{DriverSummary: d}
			if sp, ok := suspend(d.ID); ok {
				row.Suspended, row.IncidentID, row.Status = true, sp.IncidentID, "suspended"
			}
			rows = append(rows, row)
		}
		writeDriverJSON(w, http.StatusOK, map[string]any{"drivers": rows, "count": len(rows), "simulated": s != nil})
	})
	r.Get("/v1/drivers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var v sim.DriverDetail
		found := false
		if s != nil {
			v, found = s.Driver(id)
		}
		if !found {
			if d, ok := svc.Fleet().Get(id); ok {
				v, found = sim.DriverDetail{DriverSummary: fleetSummary(d), Heading: d.Heading, SpeedKmh: d.SpeedKmh}, true
			}
		}
		if !found {
			writeDriverJSON(w, http.StatusNotFound, map[string]string{"error": "driver not found: " + id})
			return
		}
		out := driverDetail{DriverDetail: v}
		if sp, ok := suspend(id); ok {
			out.Suspended, out.IncidentID, out.Suspension, out.Status = true, sp.IncidentID, sp, "suspended"
		}
		writeDriverJSON(w, http.StatusOK, out)
	})
}

func fleetSummary(d fleet.Driver) sim.DriverSummary {
	v := sim.DriverSummary{ID: d.ID, Tier: string(d.Tier), Status: string(d.Status), AV: d.Tier == fleet.TierAV,
		Rating: d.Rating, Lat: d.Lat, Lng: d.Lng}
	if v.AV {
		b := d.BatteryPct
		v.BatteryPct = &b
	}
	return v
}

func writeDriverJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
