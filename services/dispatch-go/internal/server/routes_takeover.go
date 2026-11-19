package server

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/hexago/hexago/services/dispatch-go/internal/config"
	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/sim"
)

// mountTakeover registers take-over mode (a person driving a simulated car,
// see sim/takeover.go) and proxies the Demand Forecast to agents-py:
//
//	GET    /v1/drivers/{id}/takeover            take-over state and pending offer
//	POST   /v1/drivers/{id}/takeover            take over / renew (TTL 10 min)
//	DELETE /v1/drivers/{id}/takeover            back to simulator control
//	POST   /v1/drivers/{id}/send-ride           DEMO_MODE: a ride request near that driver, assigned to it
//	POST   /v1/rides/{id}/offer/{accept|decline}
//	GET    /v1/route?from_lat&from_lng&to_lat&to_lng  road route for "Move to hotspot" (straight line without the simulator)
//	GET    /v1/forecast/*                       -> agents-py (Demand Forecast heatmap)
//
// TAKEOVER_ENABLED=false (or no simulator) answers 503 on the take-over routes.
func mountTakeover(r chi.Router, s *sim.Sim, cfg *config.Config, agents http.Handler) {
	if agents != nil {
		r.Handle("/v1/forecast/*", agents)
	}
	r.Get("/v1/route", func(w http.ResponseWriter, r *http.Request) {
		var vals [4]float64
		for i, k := range []string{"from_lat", "from_lng", "to_lat", "to_lng"} {
			v, err := strconv.ParseFloat(r.URL.Query().Get(k), 64)
			if err != nil {
				writeDriverJSON(w, http.StatusBadRequest, map[string]string{"error": k + " must be a number"})
				return
			}
			vals[i] = v
		}
		a, b := geo.Point{Lat: vals[0], Lng: vals[1]}, geo.Point{Lat: vals[2], Lng: vals[3]}
		if a.Validate() != nil || b.Validate() != nil {
			writeDriverJSON(w, http.StatusBadRequest, map[string]string{"error": "coordinates out of range"})
			return
		}
		pts, src := []geo.Point{a, b}, "straight_line"
		if s != nil {
			pts, src = s.RouteFor(r.Context(), a, b)
		}
		km := 0.0
		out := make([][2]float64, len(pts))
		for i, p := range pts {
			out[i] = [2]float64{p.Lat, p.Lng}
			if i > 0 {
				km += geo.HaversineKm(pts[i-1], p)
			}
		}
		writeDriverJSON(w, http.StatusOK, map[string]any{"points": out, "source": src, "distance_km": math.Round(km*100) / 100})
	})
	if !cfg.TakeoverEnabled || s == nil {
		off := func(w http.ResponseWriter, _ *http.Request) {
			msg := "take-over mode is disabled (TAKEOVER_ENABLED=false)"
			if s == nil {
				msg = "take-over mode needs the fleet simulator (DEMO_MODE=true)"
			}
			writeDriverJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg})
		}
		r.Get("/v1/drivers/{id}/takeover", off)
		r.Post("/v1/drivers/{id}/takeover", off)
		r.Delete("/v1/drivers/{id}/takeover", off)
		r.Post("/v1/drivers/{id}/send-ride", off)
		r.Post("/v1/rides/{id}/offer/{decision}", off)
		return
	}
	view := func(w http.ResponseWriter, v sim.TakeoverView, err error) {
		if err != nil {
			takeoverError(w, err)
			return
		}
		writeDriverJSON(w, http.StatusOK, v)
	}
	r.Get("/v1/drivers/{id}/takeover", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Takeover(chi.URLParam(r, "id"))
		view(w, v, err)
	})
	r.Post("/v1/drivers/{id}/takeover", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.TakeOver(r.Context(), chi.URLParam(r, "id"))
		view(w, v, err)
	})
	r.Delete("/v1/drivers/{id}/takeover", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Release(r.Context(), chi.URLParam(r, "id"))
		view(w, v, err)
	})
	r.Post("/v1/drivers/{id}/send-ride", func(w http.ResponseWriter, r *http.Request) {
		if !cfg.DemoMode {
			writeDriverJSON(w, http.StatusForbidden, map[string]string{"error": "send-ride is a DEMO_MODE tool"})
			return
		}
		v, err := s.SendRide(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			takeoverError(w, err)
			return
		}
		writeDriverJSON(w, http.StatusCreated, v)
	})
	r.Post("/v1/rides/{id}/offer/{decision}", func(w http.ResponseWriter, r *http.Request) {
		var accept bool
		switch chi.URLParam(r, "decision") {
		case "accept":
			accept = true
		case "decline":
		default:
			writeDriverJSON(w, http.StatusBadRequest, map[string]string{"error": "decision must be accept or decline"})
			return
		}
		res, err := s.RespondOffer(r.Context(), chi.URLParam(r, "id"), accept)
		if err != nil {
			takeoverError(w, err)
			return
		}
		writeDriverJSON(w, http.StatusOK, res)
	})
}

func takeoverError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, sim.ErrUnknownDriver), errors.Is(err, sim.ErrNoOffer), errors.Is(err, dispatch.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, sim.ErrNotTakenOver), errors.Is(err, sim.ErrDriverBusy), errors.Is(err, dispatch.ErrConflict):
		code = http.StatusConflict
	case errors.Is(err, dispatch.ErrValidation):
		code = http.StatusBadRequest
	}
	writeDriverJSON(w, code, map[string]string{"error": err.Error()})
}
