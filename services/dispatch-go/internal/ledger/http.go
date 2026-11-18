package ledger

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Routes mounts the ledger API:
//
//	GET  /v1/wallets/{owner}                 balances + recent payouts of a rider, driver or operator
//	POST /v1/wallets/{owner}/topup           {"amount_paise", "idempotency_key"?} (test mode only)
//	GET  /v1/ledger/entries?ride_id=&kind=&since=&limit=
//	GET  /v1/ledger/trial-balance
//	GET  /v1/ledger/summary                  the Ops "Money" tile
//	GET  /v1/ledger/payouts?status=&driver_id=&incident_id=&since=&limit=
//	POST /v1/ledger/refunds                  {"ride_id", "amount_paise", "reason", "idempotency_key"?}
func (l *Ledger) Routes(r chi.Router) {
	r.Get("/v1/wallets/{owner}", func(w http.ResponseWriter, r *http.Request) {
		v, err := l.Wallet(r.Context(), chi.URLParam(r, "owner"))
		reply(w, http.StatusOK, v, err)
	})
	r.Post("/v1/wallets/{owner}/topup", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AmountPaise    int64  `json:"amount_paise"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if !decode(w, r, &in) {
			return
		}
		e, err := l.Topup(r.Context(), chi.URLParam(r, "owner"), in.AmountPaise, in.IdempotencyKey)
		if err != nil {
			reply(w, 0, nil, err)
			return
		}
		wal, err := l.Wallet(r.Context(), chi.URLParam(r, "owner"))
		reply(w, http.StatusCreated, map[string]any{"entry": e, "wallet": wal}, err)
	})
	r.Get("/v1/ledger/entries", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since, limit, ok := sinceLimit(w, q.Get("since"), q.Get("limit"))
		if !ok {
			return
		}
		if id := strings.TrimSpace(q.Get("ride_id")); id != "" {
			v, err := l.Ride(r.Context(), id)
			if errors.Is(err, ErrNotFound) {
				v, err = RideView{RideID: id, Entries: []Entry{}}, nil
			}
			reply(w, http.StatusOK, v, err)
			return
		}
		es, err := l.Entries(r.Context(), EntryFilter{Kind: EntryKind(q.Get("kind")), Since: since, Limit: limit})
		reply(w, http.StatusOK, map[string]any{"entries": es}, err)
	})
	r.Get("/v1/ledger/trial-balance", func(w http.ResponseWriter, r *http.Request) {
		tb, err := l.TrialBalance(r.Context())
		reply(w, http.StatusOK, tb, err)
	})
	r.Get("/v1/ledger/summary", func(w http.ResponseWriter, r *http.Request) {
		s, err := l.Summary(r.Context())
		reply(w, http.StatusOK, s, err)
	})
	r.Get("/v1/ledger/payouts", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since, limit, ok := sinceLimit(w, q.Get("since"), q.Get("limit"))
		if !ok {
			return
		}
		ps, err := l.Payouts(r.Context(), PayoutFilter{Status: PayoutStatus(q.Get("status")), DriverID: q.Get("driver_id"),
			IncidentID: q.Get("incident_id"), RideID: q.Get("ride_id"), Since: since, Limit: limit})
		reply(w, http.StatusOK, map[string]any{"payouts": ps, "store": l.StoreKind(), "rates": l.Rates()}, err)
	})
	r.Post("/v1/ledger/refunds", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RefundInput
			Amount int64 `json:"amount"` // alias of amount_paise (paise)
		}
		if !decode(w, r, &in) {
			return
		}
		if in.AmountPaise == 0 {
			in.AmountPaise = in.Amount
		}
		e, err := l.Refund(r.Context(), in.RefundInput)
		reply(w, http.StatusCreated, e, err)
	})
}

func sinceLimit(w http.ResponseWriter, s, n string) (time.Time, int, bool) {
	var since time.Time
	var limit int
	var err error
	if s != "" {
		if since, err = time.Parse(time.RFC3339, s); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC 3339"})
			return since, 0, false
		}
	}
	if n != "" {
		if limit, err = strconv.Atoi(n); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be an integer"})
			return since, 0, false
		}
	}
	return since, limit, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

// Status maps a ledger error to an HTTP status.
func Status(err error) int {
	switch {
	case errors.Is(err, ErrValidation), errors.Is(err, ErrUnbalanced):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrNegativeBalance), errors.Is(err, ErrPayoutState):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func reply(w http.ResponseWriter, code int, v any, err error) {
	if err != nil {
		writeJSON(w, Status(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, code, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
