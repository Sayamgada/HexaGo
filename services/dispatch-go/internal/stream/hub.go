package stream

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultFlushInterval is how often batched messages are written to clients.
const DefaultFlushInterval = time.Second

const (
	defaultMaxOps   = 256              // ops messages kept per flush window
	snapshotTTL     = 10 * time.Minute // how long a ride's last update is replayed to new subscribers
	writeWait       = 10 * time.Second
	pongWait        = 75 * time.Second
	pingPeriod      = 30 * time.Second
	maxClientMsg    = 4096
	clientQueueSize = 4
	outboxSize      = 512
)

// HubOptions configures a Hub.
type HubOptions struct {
	FlushInterval time.Duration // default 1 s
	// Broker carries messages to other instances. nil = this process only.
	Broker Broker
	Logger *log.Logger
	MaxOps int // ops messages kept per window (default 256)
}

type snapshot struct {
	frame []byte
	at    time.Time
}

// Hub fans topic messages out to WebSocket clients in 1 s batches.
type Hub struct {
	interval time.Duration
	broker   Broker
	log      *log.Logger
	maxOps   int
	outbox   chan []byte

	mu       sync.Mutex
	pending  map[string][][]byte
	npending int
	latest   map[string]snapshot
	clients  map[*client]struct{}
	subs     map[string]int

	metricsMu sync.RWMutex
	metricsFn func(context.Context) (any, error)

	dropped atomic.Uint64
}

// NewHub returns a Hub. Call Run to start flushing and broker delivery.
func NewHub(o HubOptions) *Hub {
	if o.FlushInterval <= 0 {
		o.FlushInterval = DefaultFlushInterval
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.MaxOps <= 0 {
		o.MaxOps = defaultMaxOps
	}
	return &Hub{
		interval: o.FlushInterval,
		broker:   o.Broker,
		log:      o.Logger,
		maxOps:   o.MaxOps,
		outbox:   make(chan []byte, outboxSize),
		pending:  map[string][][]byte{},
		latest:   map[string]snapshot{},
		clients:  map[*client]struct{}{},
		subs:     map[string]int{},
	}
}

// BrokerKind names the cross-instance transport ("redis", "local" or "none").
func (h *Hub) BrokerKind() string {
	if h.broker == nil {
		return "none"
	}
	return h.broker.Kind()
}

// SetMetricsSource registers fn, which is polled every flush while this
// instance has ops subscribers and published locally as an ops metrics message.
func (h *Hub) SetMetricsSource(fn func(context.Context) (any, error)) {
	h.metricsMu.Lock()
	h.metricsFn = fn
	h.metricsMu.Unlock()
}

// Run subscribes to the broker and flushes every interval until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	if h.broker != nil {
		go func() {
			if err := h.broker.Subscribe(ctx, h.receive); err != nil && ctx.Err() == nil {
				h.log.Printf("WARN stream: broker subscription ended: %v", err)
			}
		}()
		go h.publishLoop(ctx)
	}
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			h.closeAll()
			return
		case <-t.C:
			h.pollMetrics(ctx)
			h.Flush()
		}
	}
}

func (h *Hub) pollMetrics(ctx context.Context) {
	h.metricsMu.RLock()
	fn := h.metricsFn
	h.metricsMu.RUnlock()
	if fn == nil || h.Subscribers(TopicOps) == 0 {
		return
	}
	m, err := fn(ctx)
	if err != nil {
		h.log.Printf("WARN stream: metrics: %v", err)
		return
	}
	data, err := MetricsData(m)
	if err != nil {
		h.log.Printf("WARN stream: metrics encode: %v", err)
		return
	}
	h.PublishLocal(TopicOps, data)
}

// Publish sends data on topic to subscribers on every instance.
func (h *Hub) Publish(topic string, data any) {
	frame, err := Encode(topic, data)
	if err != nil {
		h.log.Printf("WARN stream: encode %s: %v", topic, err)
		return
	}
	if h.broker == nil {
		h.deliver(topic, frame)
		return
	}
	msg := make([]byte, 0, len(topic)+1+len(frame))
	msg = append(append(append(msg, topic...), '\n'), frame...)
	select {
	case h.outbox <- msg:
	default:
		h.dropped.Add(1) // broker backlog: deliver locally rather than lose it
		h.deliver(topic, frame)
	}
}

// PublishLocal sends data on topic to this instance's subscribers only.
func (h *Hub) PublishLocal(topic string, data any) {
	frame, err := Encode(topic, data)
	if err != nil {
		h.log.Printf("WARN stream: encode %s: %v", topic, err)
		return
	}
	h.deliver(topic, frame)
}

func (h *Hub) publishLoop(ctx context.Context) {
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-h.outbox:
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := h.broker.Publish(pctx, msg)
			cancel()
			if err != nil {
				if !failing {
					h.log.Printf("WARN stream: broker publish failed, delivering locally: %v", err)
					failing = true
				}
				h.receive(msg)
			} else if failing {
				h.log.Printf("INFO stream: broker publish recovered")
				failing = false
			}
		}
	}
}

// receive handles a broker message: "<topic>\n<frame>".
func (h *Hub) receive(msg []byte) {
	i := bytes.IndexByte(msg, '\n')
	if i <= 0 {
		return
	}
	topic := string(msg[:i])
	if !ValidTopic(topic) {
		return
	}
	h.deliver(topic, append([]byte(nil), msg[i+1:]...))
}

func (h *Hub) deliver(topic string, frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if latestOnly(topic) {
		h.npending += 1 - len(h.pending[topic])
		h.pending[topic] = [][]byte{frame}
		h.latest[topic] = snapshot{frame: frame, at: time.Now()}
		return
	}
	q := h.pending[topic]
	if len(q) >= h.maxOps {
		q = q[1:]
		h.npending--
		h.dropped.Add(1)
	}
	h.pending[topic] = append(q, frame)
	h.npending++
}

// Pending returns the number of frames waiting for the next flush.
func (h *Hub) Pending() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.npending
}

// Subscribers returns how many local clients subscribe to topic.
func (h *Hub) Subscribers(topic string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subs[topic]
}

// Clients returns the number of connected local clients.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Flush writes everything pending to subscribed clients now.
func (h *Hub) Flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	pending := h.pending
	h.pending = map[string][][]byte{}
	h.npending = 0

	cutoff := time.Now().Add(-snapshotTTL)
	for t, s := range h.latest {
		if strings.HasPrefix(t, RideTopicPrefix) && s.at.Before(cutoff) {
			delete(h.latest, t)
		}
	}

	for c := range h.clients {
		var frames [][]byte
		for t := range c.topics {
			if q, ok := pending[t]; ok {
				frames = append(frames, q...)
			} else if c.fresh[t] {
				if s, ok := h.latest[t]; ok {
					frames = append(frames, s.frame)
				}
			}
		}
		clear(c.fresh)
		if len(frames) == 0 {
			continue
		}
		select {
		case c.send <- frames:
		default:
			h.dropped.Add(uint64(len(frames))) // slow client: skip this window
		}
	}
}

// Dropped returns how many frames were discarded because of backpressure.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }

func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.close()
	}
}

type client struct {
	conn   *websocket.Conn
	send   chan [][]byte
	done   chan struct{}
	once   sync.Once
	topics map[string]bool // guarded by Hub.mu
	fresh  map[string]bool // subscribed since the last flush (guarded by Hub.mu)
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (h *Hub) apply(c *client, m ClientMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range m.Topics {
		switch {
		case m.Action == "subscribe" && !c.topics[t]:
			if len(c.topics) >= maxTopics {
				continue
			}
			c.topics[t] = true
			c.fresh[t] = true
			h.subs[t]++
		case m.Action == "unsubscribe" && c.topics[t]:
			delete(c.topics, t)
			delete(c.fresh, t)
			h.unsub(t)
		}
	}
}

func (h *Hub) unsub(t string) {
	if h.subs[t]--; h.subs[t] <= 0 {
		delete(h.subs, t)
	}
}

// Handler serves /v1/stream. up.CheckOrigin decides which browser origins may connect.
func (h *Hub) Handler(up websocket.Upgrader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade already wrote the HTTP error
		}
		c := &client{
			conn: conn, send: make(chan [][]byte, clientQueueSize), done: make(chan struct{}),
			topics: map[string]bool{}, fresh: map[string]bool{},
		}
		h.mu.Lock()
		h.clients[c] = struct{}{}
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.clients, c)
			for t := range c.topics {
				h.unsub(t)
			}
			h.mu.Unlock()
			c.close()
		}()
		go h.writeLoop(c)
		h.readLoop(c)
	})
}

func (h *Hub) readLoop(c *client) {
	c.conn.SetReadLimit(maxClientMsg)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { return c.conn.SetReadDeadline(time.Now().Add(pongWait)) })
	for {
		_, b, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		m, err := ParseClientMessage(b)
		if err != nil {
			continue // ignore malformed control messages; keep the connection
		}
		h.apply(c, m)
	}
}

func (h *Hub) writeLoop(c *client) {
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	defer c.close()
	for {
		select {
		case <-c.done:
			return
		case frames := <-c.send:
			for _, f := range frames {
				_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := c.conn.WriteMessage(websocket.TextMessage, f); err != nil {
					return
				}
			}
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

// compile-time check that Hub satisfies Publisher.
var _ Publisher = (*Hub)(nil)
