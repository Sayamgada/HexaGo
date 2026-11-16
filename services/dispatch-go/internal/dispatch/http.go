package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

const maxBodyBytes = 64 << 10

// Routes mounts the dispatch API on r.
func (s *Service) Routes(r chi.Router) {
	r.Post("/v1/rides", s.handleCreateRide)
	r.Get("/v1/rides/{id}", s.handleGetRide)
	r.Post("/v1/rides/{id}/cancel", s.handleCancelRide)
	r.Post("/v1/drivers/{id}/status", s.handleDriverStatus)
	r.Get("/v1/metrics/dispatch", s.handleMetrics)
}

func (s *Service) handleCreateRide(w http.ResponseWriter, r *http.Request) {
	var in CreateRideInput
	if err := decode(r, &in, false); err != nil {
		writeError(w, err)
		return
	}
	view, err := s.CreateRide(r.Context(), in)
	if err != nil {
		writeError(w, err)
		return
	}
	code := http.StatusCreated
	if view.Match == nil {
		code = http.StatusAccepted // stored as an open request
	}
	writeJSON(w, code, view)
}

func (s *Service) handleGetRide(w http.ResponseWriter, r *http.Request) {
	view, err := s.GetRide(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Service) handleCancelRide(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &in, true); err != nil {
		writeError(w, err)
		return
	}
	view, err := s.CancelRide(r.Context(), chi.URLParam(r, "id"), in.Reason)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Service) handleDriverStatus(w http.ResponseWriter, r *http.Request) {
	var in DriverStatusInput
	if err := decode(r, &in, false); err != nil {
		writeError(w, err)
		return
	}
	view, err := s.UpdateDriverStatus(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Service) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m, err := s.Metrics(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// decode reads a JSON body strictly. With allowEmpty, an empty body leaves v unchanged.
func decode(r *http.Request, v any, allowEmpty bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("%w: invalid JSON body: %v", ErrValidation, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrValidation):
		code = http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrConflict):
		code = http.StatusConflict
	}
	msg := err.Error()
	if code == http.StatusInternalServerError {
		msg = "internal error"
	}
	writeJSON(w, code, map[string]string{"error": msg})
}
