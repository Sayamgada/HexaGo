// Package redisdiag checks a REDIS_URL end to end (connect, PING, pub/sub
// round-trip) and explains failures, for /healthz. Passwords are never
// reported: URLs are shown as scheme://user:****@host:port/db.
package redisdiag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// URLInfo describes a Redis URL without its password.
type URLInfo struct {
	URL      string `json:"url"` // masked
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	DB       int    `json:"db"`
	TLS      bool   `json:"tls"`
	Internal bool   `json:"render_internal_host"` // red-xxxx: resolves only inside Render's private network
}

// Describe parses raw and masks its password.
func Describe(raw string) (URLInfo, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return URLInfo{}, fmt.Errorf("parse REDIS_URL: %w", errors.Unwrap(err))
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return URLInfo{}, fmt.Errorf("REDIS_URL scheme %q: want redis:// or rediss://", u.Scheme)
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "6379"
	}
	db := 0
	if p := strings.Trim(u.Path, "/"); p != "" {
		db, _ = strconv.Atoi(p)
	}
	masked := *u
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			masked.User = url.UserPassword(u.User.Username(), "____")
		}
	}
	return URLInfo{
		URL:      strings.Replace(masked.String(), "____", "****", 1),
		Scheme:   u.Scheme,
		Host:     host,
		Port:     port,
		DB:       db,
		TLS:      u.Scheme == "rediss",
		Internal: strings.HasPrefix(host, "red-") && !strings.Contains(host, "."),
	}, nil
}

// Result is one end-to-end check.
type Result struct {
	Configured bool `json:"configured"`
	URLInfo
	OK          bool    `json:"ok"`
	PingMs      float64 `json:"ping_ms"`
	PubSubMs    float64 `json:"pubsub_roundtrip_ms"`
	FailedStage string  `json:"failed_stage,omitempty"` // parse | ping | subscribe | publish | receive
	Error       string  `json:"error,omitempty"`
	Hint        string  `json:"hint,omitempty"`
	CheckedAt   string  `json:"checked_at,omitempty"`
}

// Check connects to raw, PINGs, and publishes a message on a throwaway
// channel it is subscribed to, each step bounded by timeout.
func Check(ctx context.Context, raw string, timeout time.Duration) Result {
	res := Result{Configured: raw != "", CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if raw == "" {
		return res
	}
	fail := func(stage string, err error) Result {
		res.FailedStage, res.Error, res.Hint = stage, err.Error(), Hint(err)
		return res
	}
	info, err := Describe(raw)
	if err != nil {
		return fail("parse", err)
	}
	res.URLInfo = info
	opt, err := redis.ParseURL(raw)
	if err != nil {
		return fail("parse", err)
	}
	opt.DialTimeout, opt.ReadTimeout, opt.WriteTimeout = timeout, timeout, timeout
	opt.MaxRetries = 0
	c := redis.NewClient(opt)
	defer c.Close()

	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.Ping(pctx).Err(); err != nil {
		return fail("ping", err)
	}
	res.PingMs = ms(time.Since(start))

	var b [6]byte
	_, _ = rand.Read(b[:])
	channel := "hexago:diag:" + hex.EncodeToString(b[:])
	start = time.Now()
	sctx, scancel := context.WithTimeout(ctx, timeout)
	defer scancel()
	ps := c.Subscribe(sctx, channel)
	defer ps.Close()
	if _, err := ps.ReceiveTimeout(sctx, timeout); err != nil { // subscription confirmation
		return fail("subscribe", err)
	}
	if err := c.Publish(sctx, channel, "ping").Err(); err != nil {
		return fail("publish", err)
	}
	msg, err := ps.ReceiveTimeout(sctx, timeout)
	if err != nil {
		return fail("receive", err)
	}
	if m, ok := msg.(*redis.Message); !ok || m.Payload != "ping" {
		return fail("receive", fmt.Errorf("unexpected pub/sub reply %T", msg))
	}
	res.PubSubMs = ms(time.Since(start))
	res.OK = true
	return res
}

// Hint explains a Redis connection error in terms of the likely Render setting.
func Hint(err error) string {
	if err == nil {
		return ""
	}
	e := strings.ToLower(err.Error())
	switch {
	case strings.Contains(e, "no such host") || strings.Contains(e, "lookup"):
		return "DNS lookup failed. An internal Render Key Value URL (redis://red-xxxx:6379) only resolves from services " +
			"in the same region and workspace as the Key Value instance; otherwise use its external rediss:// URL."
	case strings.Contains(e, "x509") || strings.Contains(e, "certificate"):
		return "TLS certificate not accepted for this host; use the exact hostname from the Key Value's external URL."
	case strings.Contains(e, "tls") || strings.Contains(e, "handshake"):
		return "TLS mismatch: rediss:// was used against a plain-text endpoint. Internal Render URLs use redis://."
	case strings.Contains(e, "wrongpass") || strings.Contains(e, "noauth") || strings.Contains(e, "invalid password") ||
		strings.Contains(e, "invalid username-password"):
		return "Authentication failed: the password in REDIS_URL is wrong or missing. Copy the URL again from the Key Value page."
	case strings.Contains(e, "refused"):
		return "Connection refused: the host is reachable but nothing accepts connections on that port (wrong host/port)."
	case e == "eof" || strings.Contains(e, "connection reset") || strings.Contains(e, "broken pipe"):
		return "Server closed the connection. An external Render Key Value URL needs TLS (rediss://) and this service's " +
			"outbound IP in the instance's allow list."
	case strings.Contains(e, "timeout") || strings.Contains(e, "deadline"):
		return "Timed out: the host is not reachable from this service (different region, or external access blocked " +
			"by the Key Value allow list)."
	}
	return ""
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Monitor re-checks a URL periodically so /healthz can report without
// blocking on Redis.
type Monitor struct {
	url      string
	interval time.Duration
	timeout  time.Duration
	mu       sync.RWMutex
	last     Result
	lastOK   *bool
}

// NewMonitor checks url every interval, each check bounded by timeout.
func NewMonitor(url string, interval, timeout time.Duration) *Monitor {
	return &Monitor{url: url, interval: interval, timeout: timeout}
}

// Last returns the most recent result. Before the first check it reports only
// whether a URL is configured and what it points at.
func (m *Monitor) Last() Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.last.CheckedAt == "" {
		pending := Result{Configured: m.url != ""}
		pending.URLInfo, _ = Describe(m.url)
		return pending
	}
	return m.last
}

// CheckNow runs a check, stores and returns it, and logs state changes.
func (m *Monitor) CheckNow(ctx context.Context) Result {
	res := Check(ctx, m.url, m.timeout)
	m.mu.Lock()
	changed := m.lastOK == nil || *m.lastOK != res.OK
	ok := res.OK
	m.last, m.lastOK = res, &ok
	m.mu.Unlock()
	if changed && res.Configured {
		if res.OK {
			log.Printf("INFO redis check: %s ok (ping %.1f ms, pub/sub %.1f ms)", res.URL, res.PingMs, res.PubSubMs)
		} else {
			log.Printf("ERROR redis check: %s failed at %s: %s. %s", res.URL, res.FailedStage, res.Error, res.Hint)
		}
	}
	return res
}

// Run checks immediately and then every interval until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		m.CheckNow(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
