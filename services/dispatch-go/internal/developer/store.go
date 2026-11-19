package developer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors.
var (
	ErrNotFound   = errors.New("developer: not found")
	ErrValidation = errors.New("developer: invalid request")
)

// KeyPrefix starts every API key, so keys are easy to recognise (and to scan for in leaks).
const KeyPrefix = "hxg_"

// Key is an API key's stored record. The key itself is never stored, only its SHA-256.
type Key struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Prefix          string     `json:"prefix"` // first characters of the key, for display
	Hash            string     `json:"-"`
	Scopes          []string   `json:"scopes"`
	RateLimitPerMin int        `json:"rate_limit_per_min"`
	CreatedAt       time.Time  `json:"created_at"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}

// HasScope reports whether the key grants scope.
func (k Key) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Webhook is a subscription of one key to some events.
type Webhook struct {
	ID        string    `json:"id"`
	KeyID     string    `json:"key_id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Secret    string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// Subscribed reports whether the webhook wants event.
func (w Webhook) Subscribed(event string) bool {
	for _, e := range w.Events {
		if e == event {
			return true
		}
	}
	return false
}

// Store persists keys and webhooks.
type Store interface {
	Kind() string
	CreateKey(ctx context.Context, k Key) error
	KeyByHash(ctx context.Context, hash string) (Key, error)
	RevokeKey(ctx context.Context, id string, at time.Time) error
	CreateWebhook(ctx context.Context, w Webhook) error
	Webhooks(ctx context.Context, keyID string) ([]Webhook, error)
	WebhooksFor(ctx context.Context, event string) ([]Webhook, error)
	DeleteWebhook(ctx context.Context, keyID, id string) error
}

// HashKey returns the stored form of an API key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewSecret returns a random token: prefix + 2*n hex characters.
func NewSecret(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return prefix + hex.EncodeToString(b)
}

// LooksLikeKey reports whether s has the shape of a HexaGo API key.
func LooksLikeKey(s string) bool {
	return strings.HasPrefix(s, KeyPrefix) && len(s) > len(KeyPrefix)+8 && len(s) <= 128
}

// Memory is an in-process Store (no DATABASE_URL, tests).
type Memory struct {
	mu       sync.RWMutex
	keys     map[string]Key // by hash
	webhooks map[string]Webhook
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{keys: map[string]Key{}, webhooks: map[string]Webhook{}}
}

// Kind implements Store.
func (m *Memory) Kind() string { return "memory" }

// CreateKey implements Store.
func (m *Memory) CreateKey(_ context.Context, k Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k.Scopes = append([]string(nil), k.Scopes...)
	m.keys[k.Hash] = k
	return nil
}

// KeyByHash implements Store.
func (m *Memory) KeyByHash(_ context.Context, hash string) (Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[hash]
	if !ok {
		return Key{}, ErrNotFound
	}
	return k, nil
}

// RevokeKey implements Store.
func (m *Memory) RevokeKey(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, k := range m.keys {
		if k.ID == id && k.RevokedAt == nil {
			k.RevokedAt = &at
			m.keys[h] = k
			for wid, w := range m.webhooks {
				if w.KeyID == id {
					delete(m.webhooks, wid)
				}
			}
			return nil
		}
	}
	return ErrNotFound
}

// CreateWebhook implements Store.
func (m *Memory) CreateWebhook(_ context.Context, w Webhook) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Events = append([]string(nil), w.Events...)
	m.webhooks[w.ID] = w
	return nil
}

func (m *Memory) list(match func(Webhook) bool) []Webhook {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Webhook{}
	for _, w := range m.webhooks {
		if match(w) {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt) || (out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID)
	})
	return out
}

// Webhooks implements Store.
func (m *Memory) Webhooks(_ context.Context, keyID string) ([]Webhook, error) {
	return m.list(func(w Webhook) bool { return w.KeyID == keyID }), nil
}

// WebhooksFor implements Store.
func (m *Memory) WebhooksFor(_ context.Context, event string) ([]Webhook, error) {
	return m.list(func(w Webhook) bool { return w.Subscribed(event) }), nil
}

// DeleteWebhook implements Store.
func (m *Memory) DeleteWebhook(_ context.Context, keyID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.webhooks[id]
	if !ok || w.KeyID != keyID {
		return ErrNotFound
	}
	delete(m.webhooks, id)
	return nil
}
