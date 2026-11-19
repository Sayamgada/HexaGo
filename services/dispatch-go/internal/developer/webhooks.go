package developer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Webhook events.
const (
	EventRideCompleted  = "ride.completed"
	EventIncidentOpened = "incident.opened"
	EventPayoutSent     = "payout.sent"
	EventPing           = "ping" // POST /v1/developer/webhooks/{id}/test only
)

// Events lists the subscribable events.
var Events = []string{EventRideCompleted, EventIncidentOpened, EventPayoutSent}

// ValidEvent reports whether e can be subscribed to.
func ValidEvent(e string) bool {
	for _, v := range Events {
		if v == e {
			return true
		}
	}
	return false
}

// Delivery headers.
const (
	HeaderSignature = "X-HexaGo-Signature" // "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body))
	HeaderTimestamp = "X-HexaGo-Timestamp" // unix seconds
	HeaderEvent     = "X-HexaGo-Event"
	HeaderDelivery  = "X-HexaGo-Delivery" // event id, stable across retries
)

// Sign returns the X-HexaGo-Signature value for body sent at ts.
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature in constant time (what a receiver does).
func Verify(secret string, ts int64, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, ts, body)), []byte(signature))
}

// Event is a webhook payload.
type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      any       `json:"data"`
}

// DeliveryRecord is one finished delivery (all attempts) of an event to a webhook.
type DeliveryRecord struct {
	EventID    string    `json:"event_id"`
	Event      string    `json:"event"`
	WebhookID  string    `json:"webhook_id"`
	Attempts   int       `json:"attempts"`
	StatusCode int       `json:"status_code"` // last attempt; 0 = no response
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
}

// DispatcherOptions configures webhook delivery.
type DispatcherOptions struct {
	Store        Store
	Retries      int           // after the first attempt; default 3
	Backoff      time.Duration // before retry n: Backoff * 2^(n-1); default 1 s
	Timeout      time.Duration // per attempt; default 5 s
	Workers      int           // default 4
	Queue        int           // default 1024
	AllowPrivate bool          // allow loopback/private targets (tests, local dev)
	Client       *http.Client  // overrides the guarded client
	Logger       *log.Logger
	Now          func() time.Time
}

// Dispatcher fans events out to subscribed webhooks.
type Dispatcher struct {
	o      DispatcherOptions
	client *http.Client
	queue  chan job
	log    *log.Logger

	mu      sync.Mutex
	history map[string][]DeliveryRecord // by webhook id, newest last
	subs    map[string]subsEntry        // subscriber cache by event
	stats   struct{ sent, failed, dropped int }

	wg sync.WaitGroup
}

type job struct {
	hook  Webhook
	event Event
}

const historyPerHook = 50

// NewDispatcher returns a dispatcher; call Run to start its workers.
func NewDispatcher(o DispatcherOptions) *Dispatcher {
	if o.Retries == 0 {
		o.Retries = 3
	}
	if o.Retries < 0 {
		o.Retries = 0
	}
	if o.Backoff <= 0 {
		o.Backoff = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Queue <= 0 {
		o.Queue = 1024
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	d := &Dispatcher{o: o, queue: make(chan job, o.Queue), log: o.Logger, history: map[string][]DeliveryRecord{},
		subs: map[string]subsEntry{}}
	d.client = o.Client
	if d.client == nil {
		d.client = guardedClient(o.Timeout, o.AllowPrivate)
	}
	return d
}

// Run delivers queued events until ctx is done.
func (d *Dispatcher) Run(ctx context.Context) {
	for i := 0; i < d.o.Workers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-d.queue:
					d.deliver(ctx, j)
				}
			}
		}()
	}
	d.wg.Wait()
}

type subsEntry struct {
	hooks []Webhook
	at    time.Time
}

// subscriberTTL bounds how stale the subscriber cache may be (another
// instance may have added a webhook); Invalidate clears it locally at once.
const subscriberTTL = 5 * time.Second

// Subscribers returns the webhooks subscribed to typ (cached for a few seconds).
func (d *Dispatcher) Subscribers(ctx context.Context, typ string) ([]Webhook, error) {
	now := d.o.Now()
	d.mu.Lock()
	e, ok := d.subs[typ]
	d.mu.Unlock()
	if ok && now.Sub(e.at) < subscriberTTL && now.Sub(e.at) >= 0 {
		return e.hooks, nil
	}
	hooks, err := d.o.Store.WebhooksFor(ctx, typ)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.subs[typ] = subsEntry{hooks: hooks, at: now}
	d.mu.Unlock()
	return hooks, nil
}

// Invalidate drops the subscriber cache (after a webhook is added or removed).
func (d *Dispatcher) Invalidate() {
	d.mu.Lock()
	d.subs = map[string]subsEntry{}
	d.mu.Unlock()
}

// Emit queues event typ for every webhook subscribed to it. It never blocks:
// when the queue is full the delivery is dropped (and counted).
func (d *Dispatcher) Emit(ctx context.Context, typ string, data any) {
	hooks, err := d.Subscribers(ctx, typ)
	if err != nil {
		d.log.Printf("WARN webhooks: list subscribers of %s: %v", typ, err)
		return
	}
	if len(hooks) == 0 {
		return
	}
	ev := Event{ID: NewSecret("evt_", 10), Type: typ, CreatedAt: d.o.Now().UTC(), Data: data}
	for _, h := range hooks {
		d.enqueue(job{hook: h, event: ev})
	}
}

// Ping queues a test event for one webhook.
func (d *Dispatcher) Ping(h Webhook) Event {
	ev := Event{ID: NewSecret("evt_", 10), Type: EventPing, CreatedAt: d.o.Now().UTC(),
		Data: map[string]any{"webhook_id": h.ID, "message": "HexaGo webhook test"}}
	d.enqueue(job{hook: h, event: ev})
	return ev
}

func (d *Dispatcher) enqueue(j job) {
	select {
	case d.queue <- j:
	default:
		d.mu.Lock()
		d.stats.dropped++
		d.mu.Unlock()
		d.log.Printf("WARN webhooks: queue full, dropped %s for %s", j.event.Type, j.hook.ID)
	}
}

// History returns the recent deliveries to a webhook, newest first.
func (d *Dispatcher) History(webhookID string) []DeliveryRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.history[webhookID]
	out := make([]DeliveryRecord, 0, len(h))
	for i := len(h) - 1; i >= 0; i-- {
		out = append(out, h[i])
	}
	return out
}

// Stats reports sent / failed / dropped delivery counts.
func (d *Dispatcher) Stats() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]int{"sent": d.stats.sent, "failed": d.stats.failed, "dropped": d.stats.dropped, "queued": len(d.queue)}
}

func (d *Dispatcher) deliver(ctx context.Context, j job) {
	body, err := json.Marshal(j.event)
	if err != nil {
		d.record(j, 0, 0, err)
		return
	}
	var code int
	attempts := 0
	for attempt := 0; attempt <= d.o.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				d.record(j, attempts, code, ctx.Err())
				return
			case <-time.After(d.o.Backoff << (attempt - 1)):
			}
		}
		attempts++
		code, err = d.post(ctx, j, body)
		if err == nil {
			d.record(j, attempts, code, nil)
			return
		}
		if code >= 400 && code < 500 && code != http.StatusTooManyRequests && code != http.StatusRequestTimeout {
			break // the receiver rejected it: retrying will not help
		}
	}
	d.record(j, attempts, code, err)
	d.log.Printf("WARN webhooks: %s to %s failed after %d attempt(s): %v", j.event.Type, j.hook.URL, attempts, err)
}

func (d *Dispatcher) post(ctx context.Context, j job, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, d.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.hook.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := d.o.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "HexaGo-Webhooks/1.0")
	req.Header.Set(HeaderEvent, j.event.Type)
	req.Header.Set(HeaderDelivery, j.event.ID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(j.hook.Secret, ts, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (d *Dispatcher) record(j job, attempts, code int, err error) {
	rec := DeliveryRecord{EventID: j.event.ID, Event: j.event.Type, WebhookID: j.hook.ID, Attempts: attempts,
		StatusCode: code, OK: err == nil, At: d.o.Now().UTC()}
	if err != nil {
		rec.Error = err.Error()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if rec.OK {
		d.stats.sent++
	} else {
		d.stats.failed++
	}
	h := append(d.history[j.hook.ID], rec)
	if len(h) > historyPerHook {
		h = h[len(h)-historyPerHook:]
	}
	d.history[j.hook.ID] = h
}

// ErrPrivateTarget is returned for webhook URLs that resolve to internal addresses.
var ErrPrivateTarget = errors.New("webhook target resolves to a private, loopback or link-local address")

func privateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}

// guardedClient refuses to connect to internal addresses (SSRF guard), checked
// on the resolved IP at dial time so DNS tricks do not get around it.
func guardedClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || privateIP(ip) {
				return ErrPrivateTarget
			}
			return nil
		}
	}
	tr := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: timeout,
		ResponseHeaderTimeout: timeout, MaxIdleConnsPerHost: 2, Proxy: nil}
	return &http.Client{Timeout: timeout, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ValidateURL checks a webhook URL at registration.
func ValidateURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%w: url must be an absolute http(s) URL", ErrValidation)
	}
	if len(raw) > 2048 {
		return fmt.Errorf("%w: url longer than 2048 characters", ErrValidation)
	}
	if u.User != nil {
		return fmt.Errorf("%w: url must not contain credentials", ErrValidation)
	}
	if allowPrivate {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("%w: %v", ErrValidation, ErrPrivateTarget)
	}
	if ip := net.ParseIP(host); ip != nil && privateIP(ip) {
		return fmt.Errorf("%w: %v", ErrValidation, ErrPrivateTarget)
	}
	return nil
}
