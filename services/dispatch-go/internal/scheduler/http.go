package scheduler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

const maxBody = 64 << 10

// Routes mounts:
//
//	POST /v1/scheduled              store a confirmed plan's legs (agents-py booking confirm)
//	GET  /v1/scheduled?rider_id=    a rider's scheduled legs (&plan_id= for one plan)
//	GET  /v1/eta?from_lat&from_lng&to_lat&to_lng   trip estimate
//	GET  /v1/supply?lat&lng[&rings=1]              idle vs busy cars around the pickup cell
func (s *Scheduler) Routes(r chi.Router) {
	r.Post("/v1/scheduled", s.handleSchedule)
	r.Get("/v1/scheduled", s.handleList)
	r.Get("/v1/eta", handleETA)
	r.Get("/v1/supply", s.handleSupply)
}

func (s *Scheduler) handleSchedule(w http.ResponseWriter, r *http.Request) {
	var in PlanInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	legs, err := s.Schedule(r.Context(), in)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrValidation) {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"plan_id": legs[0].PlanID, "rider_id": legs[0].RiderID, "demo_mode": s.o.Demo, "legs": legs,
	})
}

func (s *Scheduler) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var legs []Leg
	if plan := q.Get("plan_id"); plan != "" {
		legs = s.Plan(plan)
	} else {
		legs = s.List(q.Get("rider_id"))
	}
	if legs == nil {
		legs = []Leg{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"legs": legs, "count": len(legs), "demo_mode": s.o.Demo, "server_time": s.o.Now().UTC().Format(time.RFC3339),
	})
}

func floats(q map[string][]string, keys ...string) ([]float64, bool) {
	out := make([]float64, len(keys))
	for i, k := range keys {
		v, ok := q[k]
		if !ok || len(v) == 0 {
			return nil, false
		}
		f, err := strconv.ParseFloat(v[0], 64)
		if err != nil {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

func handleETA(w http.ResponseWriter, r *http.Request) {
	v, ok := floats(r.URL.Query(), "from_lat", "from_lng", "to_lat", "to_lng")
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from_lat, from_lng, to_lat and to_lng are required"})
		return
	}
	a, b := geo.Point{Lat: v[0], Lng: v[1]}, geo.Point{Lat: v[2], Lng: v[3]}
	if a.Validate() != nil || b.Validate() != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "coordinates out of range"})
		return
	}
	t := EstimateTrip(a, b)
	writeJSON(w, http.StatusOK, map[string]any{
		"distance_km": t.DistanceKm, "duration_s": t.DurationS, "duration_min": t.DurationS / 60, "source": "dispatch-go",
	})
}

func (s *Scheduler) handleSupply(w http.ResponseWriter, r *http.Request) {
	v, ok := floats(r.URL.Query(), "lat", "lng")
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lat and lng are required"})
		return
	}
	if s.o.Geo == nil || s.o.Fleet == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "fleet index not available"})
		return
	}
	rings := 1
	if raw := r.URL.Query().Get("rings"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 3 {
			rings = n
		}
	}
	load, err := Load(s.o.Geo, s.o.Fleet, geo.Point{Lat: v[0], Lng: v[1]}, rings)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, load)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
