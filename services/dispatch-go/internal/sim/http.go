package sim

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Routes mounts the simulator controls:
//
//	GET  /v1/sim/config            current state
//	POST /v1/sim/config            {"drivers","demand_per_min","spike_lat","spike_lng"} (all optional)
//	POST /v1/sim/scenario/{name}   rush_hour | rain_storm | concert_exit | safety_incident
func (s *Sim) Routes(r chi.Router) {
	r.Get("/v1/sim/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Status())
	})
	r.Post("/v1/sim/config", func(w http.ResponseWriter, r *http.Request) {
		var in ConfigInput
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
			return
		}
		v, err := s.Configure(r.Context(), in)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v)
	})
	r.Post("/v1/sim/scenario/{name}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.TriggerScenario(r.Context(), chi.URLParam(r, "name"))
		switch {
		case errors.Is(err, ErrUnknownScenario):
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error(), "scenarios": Scenarios})
		case errors.Is(err, ErrNoOnTripVehicle):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		default:
			writeJSON(w, http.StatusOK, v)
		}
	})
}

// DisabledRoutes mounts the simulator paths answering 503 (DEMO_MODE off).
func DisabledRoutes(r chi.Router) {
	off := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "fleet simulator is not running (set DEMO_MODE=true)"})
	}
	r.Get("/v1/sim/config", off)
	r.Post("/v1/sim/config", off)
	r.Post("/v1/sim/scenario/{name}", off)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
