package safety

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

const maxBody = 64 << 10

// Routes mounts the dispatch-go side of the safety API (everything else
// under /v1/safety/* is the Safety Sentinel in agents-py, proxied by the gateway):
//
//	GET  /v1/safety/signals                      recent detector signals + thresholds
//	GET  /v1/safety/drivers/{id}                 suspension state
//	POST /v1/safety/drivers/{id}/suspend         {"incident_id","reason","actor"}
//	POST /v1/safety/drivers/{id}/unsuspend       {"incident_id","confirmed_by":"ops_supervisor"}
//	POST /v1/safety/track                        {"driver_id","ride_id","incident_id"} -> {"token","url"}
//	GET  /track/{token}                          live location behind a share link
//	GET  /v1/safety/vehicle-path/{driverID}      telemetry, oldest first (?limit=, ?since=RFC3339)
//	POST /v1/safety/publish                      {"topic":"ops"|"ride:<id>","data":{"type":"safety_call"|...}}
//	POST /v1/safety/emergency/dispatch           simulated emergency services (payload is logged)
//	POST /v1/safety/emergency/notify-contact     simulated emergency-contact SMS (payload is logged)
//	GET  /v1/safety/emergency/log                the simulated emergency records
func (s *Service) Routes(r chi.Router) {
	r.Get("/v1/safety/signals", func(w http.ResponseWriter, _ *http.Request) {
		posted, failed := s.Delivery()
		writeJSON(w, http.StatusOK, map[string]any{
			"signals": s.Signals(), "config": s.det.Config(), "tracked_rides": s.det.Tracked(),
			"delivered": posted, "delivery_errors": failed, "agents_url_set": s.agents != "",
		})
	})
	r.Get("/v1/safety/drivers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		sp, ok := s.Suspended(id)
		out := map[string]any{"driver_id": id, "suspended": ok}
		if ok {
			out["suspension"] = sp
		}
		writeJSON(w, http.StatusOK, out)
	})
	r.Post("/v1/safety/drivers/{id}/suspend", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			IncidentID string `json:"incident_id"`
			Reason     string `json:"reason"`
			Actor      string `json:"actor"`
		}
		if !decode(w, r, &in) {
			return
		}
		sp, err := s.Suspend(r.Context(), chi.URLParam(r, "id"), in.IncidentID, in.Reason, in.Actor)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sp)
	})
	r.Post("/v1/safety/drivers/{id}/unsuspend", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			IncidentID  string `json:"incident_id"`
			ConfirmedBy string `json:"confirmed_by"`
		}
		if !decode(w, r, &in) {
			return
		}
		id := chi.URLParam(r, "id")
		was, err := s.ReleaseIncident(r.Context(), id, in.IncidentID, in.ConfirmedBy)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"driver_id": id, "suspended": false, "was_suspended": was,
			"confirmed_by": in.ConfirmedBy})
	})
	r.Post("/v1/safety/track", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			DriverID   string `json:"driver_id"`
			RideID     string `json:"ride_id"`
			IncidentID string `json:"incident_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		l, err := s.TrackLink(in.DriverID, in.RideID, in.IncidentID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": l.Token, "path": "/track/" + l.Token, "url": baseURL(r) + "/track/" + l.Token, "expires_at": l.ExpiresAt,
		})
	})
	r.Get("/track/{token}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.Track(chi.URLParam(r, "token"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown or expired link"})
			return
		}
		writeJSON(w, http.StatusOK, v)
	})
	r.Get("/v1/safety/vehicle-path/{driverID}", s.handlePath)
	r.Post("/v1/safety/publish", s.handlePublish)
	r.Post("/v1/safety/emergency/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		if !decode(w, r, &in) {
			return
		}
		rec := s.recordEmergency("dispatch", "ems-", in)
		writeJSON(w, http.StatusOK, map[string]any{
			"reference": rec.Reference, "status": "dispatched", "eta_min": 7, "simulated": true, "at": rec.At,
		})
	})
	r.Post("/v1/safety/emergency/notify-contact", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		if !decode(w, r, &in) {
			return
		}
		rec := s.recordEmergency("contact", "sms-", in)
		writeJSON(w, http.StatusOK, map[string]any{
			"reference": rec.Reference, "status": "sent", "channel": "sms", "simulated": true, "at": rec.At,
		})
	})
	r.Get("/v1/safety/emergency/log", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"records": s.EmergencyLog()})
	})
}

// PathPoint is one telemetry sample of a vehicle path.
type PathPoint struct {
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	SpeedKmh   float64   `json:"speed_kmh"`
	Status     string    `json:"status"`
	RecordedAt time.Time `json:"recorded_at"`
}

func (s *Service) handlePath(w http.ResponseWriter, r *http.Request) {
	if s.tel == nil {
		writeJSON(w, http.StatusOK, map[string]any{"points": []PathPoint{}, "source": "none"})
		return
	}
	limit := 600
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 5000)
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC 3339"})
			return
		}
		since = t
	}
	pts, err := s.tel.RecentTelemetry(r.Context(), chi.URLParam(r, "driverID"), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "telemetry unavailable"})
		return
	}
	out := make([]PathPoint, 0, len(pts))
	for _, p := range pts {
		if p.RecordedAt.Before(since) {
			continue
		}
		out = append(out, PathPoint{Lat: p.Lat, Lng: p.Lng, SpeedKmh: p.SpeedKmh, Status: p.Status, RecordedAt: p.RecordedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordedAt.Before(out[j].RecordedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"points": out, "source": "telemetry"})
}

// publishable lists the frame types agents may push onto the live stream.
var publishable = map[string]bool{"safety_call": true, "safety_update": true}

func (s *Service) handlePublish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Topic string         `json:"topic"`
		Data  map[string]any `json:"data"`
	}
	if !decode(w, r, &in) {
		return
	}
	typ, _ := in.Data["type"].(string)
	switch {
	case in.Topic != stream.TopicOps && !(strings.HasPrefix(in.Topic, stream.RideTopicPrefix) && stream.ValidTopic(in.Topic)):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic must be ops or ride:<id>"})
		return
	case !publishable[typ]:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "data.type must be safety_call or safety_update"})
		return
	case s.stream == nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "stream unavailable"})
		return
	}
	s.stream.Publish(in.Topic, in.Data)
	writeJSON(w, http.StatusAccepted, map[string]any{"published": true, "topic": in.Topic})
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrUnknownDriver):
		code = http.StatusNotFound
	case errors.Is(err, ErrNotSuspended):
		code = http.StatusConflict
	case errors.Is(err, ErrForbidden):
		code = http.StatusForbidden
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- Redis ----

// RedisClient publishes safety signals on Redis.
type RedisClient struct{ c *redis.Client }

// NewRedis connects to url and verifies it with PING.
func NewRedis(ctx context.Context, url string) (*RedisClient, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("safety: parse REDIS_URL: %w", err)
	}
	opt.DialTimeout = 2 * time.Second
	c := redis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.Ping(pctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("safety: redis ping: %w", err)
	}
	return &RedisClient{c: c}, nil
}

// Publish implements RedisPublisher.
func (r *RedisClient) Publish(ctx context.Context, channel, message string) error {
	return r.c.Publish(ctx, channel, message).Err()
}

// Close closes the connection.
func (r *RedisClient) Close() error { return r.c.Close() }
