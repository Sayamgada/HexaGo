package developer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configures the developer platform.
type Options struct {
	Store   Store             // default: in-memory
	Limiter Limiter           // default: in-memory
	Origin  func(string) bool // first-party browser origins (the CORS allow-list)
	// KeyCreationPerMin caps self-serve key creation per client IP (default 5).
	KeyCreationPerMin int
	Webhooks          DispatcherOptions // Store is filled in from Store
	Logger            *log.Logger
	Now               func() time.Time
}

// Service holds the key store, limiter and webhook dispatcher.
type Service struct {
	store    Store
	limiter  Limiter
	origin   func(string) bool
	perIP    int
	hooks    *Dispatcher
	log      *log.Logger
	now      func() time.Time
	allowPvt bool

	cacheMu sync.Mutex
	cache   map[string]cachedKey // by hash
}

type cachedKey struct {
	key Key
	ok  bool // false = unknown key (negative cache)
	at  time.Time
}

const keyCacheTTL = 30 * time.Second

// New builds the service; call Run to start webhook delivery.
func New(o Options) *Service {
	if o.Store == nil {
		o.Store = NewMemory()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Limiter == nil {
		o.Limiter = NewMemoryLimiter(o.Now)
	}
	if o.Origin == nil {
		o.Origin = func(string) bool { return false }
	}
	if o.KeyCreationPerMin <= 0 {
		o.KeyCreationPerMin = 5
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	o.Webhooks.Store = o.Store
	if o.Webhooks.Now == nil {
		o.Webhooks.Now = o.Now
	}
	if o.Webhooks.Logger == nil {
		o.Webhooks.Logger = o.Logger
	}
	return &Service{store: o.Store, limiter: o.Limiter, origin: o.Origin, perIP: o.KeyCreationPerMin,
		hooks: NewDispatcher(o.Webhooks), log: o.Logger, now: o.Now, allowPvt: o.Webhooks.AllowPrivate,
		cache: map[string]cachedKey{}}
}

// Run delivers webhooks until ctx is done.
func (s *Service) Run(ctx context.Context) { s.hooks.Run(ctx) }

// Webhooks returns the dispatcher (event sources call Emit on it).
func (s *Service) Webhooks() *Dispatcher { return s.hooks }

// StoreKind and LimiterKind describe the backends (for /healthz).
func (s *Service) StoreKind() string   { return s.store.Kind() }
func (s *Service) LimiterKind() string { return s.limiter.Kind() }

type ctxKey struct{}

// KeyFrom returns the API key that authenticated the request, if any.
func KeyFrom(ctx context.Context) (Key, bool) {
	k, ok := ctx.Value(ctxKey{}).(Key)
	return k, ok
}

// presentedKey extracts a key from Authorization: Bearer, X-API-Key or, for
// WebSocket clients that cannot set headers, the api_key query parameter.
func presentedKey(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if h := strings.TrimSpace(r.Header.Get("X-API-Key")); h != "" {
		return h
	}
	return r.URL.Query().Get("api_key")
}

func (s *Service) lookup(ctx context.Context, raw string) (Key, bool, error) {
	if !LooksLikeKey(raw) {
		return Key{}, false, nil
	}
	hash := HashKey(raw)
	now := s.now()
	s.cacheMu.Lock()
	c, hit := s.cache[hash]
	s.cacheMu.Unlock()
	if hit && now.Sub(c.at) < keyCacheTTL {
		return c.key, c.ok, nil
	}
	k, err := s.store.KeyByHash(ctx, hash)
	ok := err == nil && k.RevokedAt == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Key{}, false, err
	}
	s.cacheMu.Lock()
	if len(s.cache) > 10000 {
		s.cache = map[string]cachedKey{}
	}
	s.cache[hash] = cachedKey{key: k, ok: ok, at: now}
	s.cacheMu.Unlock()
	return k, ok, nil
}

func (s *Service) forget(hash string) {
	s.cacheMu.Lock()
	delete(s.cache, hash)
	s.cacheMu.Unlock()
}

// sameOrigin reports whether a browser Origin is the gateway itself (Swagger UI at /docs).
func sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// SetRateHeaders writes the X-RateLimit-* headers for d.
func SetRateHeaders(h http.Header, d Decision) {
	h.Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(max(d.Remaining, 0)))
	h.Set("X-RateLimit-Reset", strconv.Itoa(int(math.Ceil(d.Reset.Seconds()))))
	if !d.Allowed {
		h.Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(d.RetryAfter.Seconds())))))
	}
}

// Middleware authenticates API keys, enforces scopes and per-key rate limits.
// Keyless requests pass through unless they come from a browser origin that is
// not first-party.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		raw := presentedKey(r)
		if raw == "" {
			origin := r.Header.Get("Origin")
			if origin != "" && !Public(r.URL.Path) && !s.origin(origin) && !sameOrigin(r, origin) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "an API key is required for requests from " + origin + "; create one with POST /v1/developer/keys"})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		key, ok, err := s.lookup(r.Context(), raw)
		if err != nil {
			s.log.Printf("WARN developer: key lookup: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "key store unavailable"})
			return
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hexago"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or revoked API key"})
			return
		}
		if scope := RequiredScope(r.Method, r.URL.Path); scope != "" && !key.HasScope(scope) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": fmt.Sprintf("this key lacks the %s scope", scope), "required_scope": scope, "scopes": key.Scopes})
			return
		}
		d, err := s.limiter.Allow(r.Context(), "key:"+key.ID, key.RateLimitPerMin)
		if err != nil {
			s.log.Printf("WARN developer: rate limiter (%s) failed, used in-memory bucket: %v", s.limiter.Kind(), err)
		}
		SetRateHeaders(w.Header(), d)
		if !d.Allowed {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": fmt.Sprintf("rate limit of %d requests/min exceeded", d.Limit), "retry_after_s": math.Ceil(d.RetryAfter.Seconds())})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, key)))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), ErrValidation.Error()+": ")})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: invalid JSON body: %v", ErrValidation, err)
	}
	return nil
}

// CreateKeyInput is the POST /v1/developer/keys body.
type CreateKeyInput struct {
	Name            string   `json:"name"`
	Scopes          []string `json:"scopes"`             // default: every scope
	RateLimitPerMin int      `json:"rate_limit_per_min"` // default 60, max 600
}

// CreatedKey is returned once, at creation: the only time the key is shown.
type CreatedKey struct {
	Key
	APIKey  string `json:"api_key"`
	Warning string `json:"warning"`
}

// CreateKey issues a new key.
func (s *Service) CreateKey(ctx context.Context, in CreateKeyInput) (CreatedKey, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "demo key"
	}
	if len(name) > 80 {
		return CreatedKey{}, fmt.Errorf("%w: name longer than 80 characters", ErrValidation)
	}
	scopes := AllScopes()
	if len(in.Scopes) > 0 {
		seen := map[string]bool{}
		scopes = scopes[:0:0]
		for _, sc := range in.Scopes {
			if !ValidScope(sc) {
				return CreatedKey{}, fmt.Errorf("%w: unknown scope %q (valid: %s)", ErrValidation, sc, strings.Join(AllScopes(), ", "))
			}
			if !seen[sc] {
				seen[sc] = true
				scopes = append(scopes, sc)
			}
		}
	}
	limit := in.RateLimitPerMin
	if limit == 0 {
		limit = DefaultRateLimitPerMin
	}
	if limit < 1 || limit > 600 {
		return CreatedKey{}, fmt.Errorf("%w: rate_limit_per_min must be 1..600", ErrValidation)
	}
	raw := NewSecret(KeyPrefix, 24)
	k := Key{ID: NewSecret("key_", 8), Name: name, Prefix: raw[:len(KeyPrefix)+6], Hash: HashKey(raw),
		Scopes: scopes, RateLimitPerMin: limit, CreatedAt: s.now().UTC().Truncate(time.Microsecond)}
	if err := s.store.CreateKey(ctx, k); err != nil {
		return CreatedKey{}, err
	}
	s.forget(k.Hash)
	return CreatedKey{Key: k, APIKey: raw, Warning: "Store this key now: it is shown only once (HexaGo keeps only its SHA-256)."}, nil
}

// CreateWebhookInput is the POST /v1/developer/webhooks body.
type CreateWebhookInput struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

// CreatedWebhook includes the signing secret, shown only at creation.
type CreatedWebhook struct {
	Webhook
	Secret string `json:"secret"`
}

const maxWebhooksPerKey = 10

// CreateWebhook registers a webhook for key.
func (s *Service) CreateWebhook(ctx context.Context, key Key, in CreateWebhookInput) (CreatedWebhook, error) {
	if err := ValidateURL(in.URL, s.allowPvt); err != nil {
		return CreatedWebhook{}, err
	}
	if len(in.Events) == 0 {
		return CreatedWebhook{}, fmt.Errorf("%w: events must list at least one of %s", ErrValidation, strings.Join(Events, ", "))
	}
	seen := map[string]bool{}
	var events []string
	for _, e := range in.Events {
		if !ValidEvent(e) {
			return CreatedWebhook{}, fmt.Errorf("%w: unknown event %q (valid: %s)", ErrValidation, e, strings.Join(Events, ", "))
		}
		if !seen[e] {
			seen[e] = true
			events = append(events, e)
		}
	}
	existing, err := s.store.Webhooks(ctx, key.ID)
	if err != nil {
		return CreatedWebhook{}, err
	}
	if len(existing) >= maxWebhooksPerKey {
		return CreatedWebhook{}, fmt.Errorf("%w: at most %d webhooks per key", ErrValidation, maxWebhooksPerKey)
	}
	w := Webhook{ID: NewSecret("wh_", 8), KeyID: key.ID, URL: strings.TrimSpace(in.URL), Events: events,
		Secret: NewSecret("whsec_", 24), CreatedAt: s.now().UTC().Truncate(time.Microsecond)}
	if err := s.store.CreateWebhook(ctx, w); err != nil {
		return CreatedWebhook{}, err
	}
	s.hooks.Invalidate()
	return CreatedWebhook{Webhook: w, Secret: w.Secret}, nil
}
