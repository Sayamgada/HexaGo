package developer

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Routes mounts the developer API:
//
//	GET    /v1/developer/scopes                     scopes and webhook events
//	POST   /v1/developer/keys                       {name?, scopes?, rate_limit_per_min?} -> key (shown once)
//	GET    /v1/developer/keys/me                    the calling key            (key required)
//	DELETE /v1/developer/keys/me                    revoke the calling key      (key required)
//	POST   /v1/developer/webhooks                   {url, events}  -> webhook + signing secret (shown once)
//	GET    /v1/developer/webhooks                   the key's webhooks
//	DELETE /v1/developer/webhooks/{id}
//	POST   /v1/developer/webhooks/{id}/test         queue a "ping" delivery
//	GET    /v1/developer/webhooks/{id}/deliveries   recent deliveries
func (s *Service) Routes(r chi.Router) {
	r.Get("/v1/developer/scopes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"scopes": ScopeInfo, "events": Events,
			"default_rate_limit_per_min": DefaultRateLimitPerMin,
			"signature": map[string]string{"header": HeaderSignature, "timestamp_header": HeaderTimestamp,
				"algorithm": "sha256=hex(HMAC-SHA256(secret, timestamp + \".\" + raw_body))"}})
	})
	r.Post("/v1/developer/keys", s.handleCreateKey)
	r.Get("/v1/developer/keys/me", s.withKey(func(w http.ResponseWriter, _ *http.Request, k Key) {
		writeJSON(w, http.StatusOK, k)
	}))
	r.Delete("/v1/developer/keys/me", s.withKey(func(w http.ResponseWriter, r *http.Request, k Key) {
		if err := s.store.RevokeKey(r.Context(), k.ID, s.now().UTC()); err != nil {
			writeErr(w, err)
			return
		}
		s.forget(k.Hash)
		s.hooks.Invalidate()
		writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "id": k.ID})
	}))
	r.Post("/v1/developer/webhooks", s.withKey(func(w http.ResponseWriter, r *http.Request, k Key) {
		var in CreateWebhookInput
		if err := decodeBody(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		wh, err := s.CreateWebhook(r.Context(), k, in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, wh)
	}))
	r.Get("/v1/developer/webhooks", s.withKey(func(w http.ResponseWriter, r *http.Request, k Key) {
		hs, err := s.store.Webhooks(r.Context(), k.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"webhooks": hs, "count": len(hs)})
	}))
	r.Delete("/v1/developer/webhooks/{id}", s.withKey(func(w http.ResponseWriter, r *http.Request, k Key) {
		if err := s.store.DeleteWebhook(r.Context(), k.ID, chi.URLParam(r, "id")); err != nil {
			writeErr(w, err)
			return
		}
		s.hooks.Invalidate()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": chi.URLParam(r, "id")})
	}))
	r.Post("/v1/developer/webhooks/{id}/test", s.withHook(func(w http.ResponseWriter, _ *http.Request, h Webhook) {
		ev := s.hooks.Ping(h)
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "event": ev})
	}))
	r.Get("/v1/developer/webhooks/{id}/deliveries", s.withHook(func(w http.ResponseWriter, _ *http.Request, h Webhook) {
		ds := s.hooks.History(h.ID)
		writeJSON(w, http.StatusOK, map[string]any{"deliveries": ds, "count": len(ds)})
	}))
}

func (s *Service) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	d, err := s.limiter.Allow(r.Context(), "keys-ip:"+clientIP(r), s.perIP)
	if err != nil {
		s.log.Printf("WARN developer: key-creation limiter: %v", err)
	}
	SetRateHeaders(w.Header(), d)
	if !d.Allowed {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many keys created from this address; try again shortly"})
		return
	}
	var in CreateKeyInput
	if r.ContentLength != 0 {
		if err := decodeBody(r, &in); err != nil {
			writeErr(w, err)
			return
		}
	}
	k, err := s.CreateKey(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, k)
}

func (s *Service) withKey(fn func(http.ResponseWriter, *http.Request, Key)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k, ok := KeyFrom(r.Context())
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hexago"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "send your API key as 'Authorization: Bearer hxg_...' (create one with POST /v1/developer/keys)"})
			return
		}
		fn(w, r, k)
	}
}

func (s *Service) withHook(fn func(http.ResponseWriter, *http.Request, Webhook)) http.HandlerFunc {
	return s.withKey(func(w http.ResponseWriter, r *http.Request, k Key) {
		hs, err := s.store.Webhooks(r.Context(), k.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		id := chi.URLParam(r, "id")
		for _, h := range hs {
			if h.ID == id {
				fn(w, r, h)
				return
			}
		}
		writeErr(w, ErrNotFound)
	})
}
