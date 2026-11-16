// Package stream implements the /v1/stream WebSocket: topic subscriptions,
// 1-second batched fan-out, and cross-instance delivery over Redis pub/sub
// (or in-process when Redis is not configured).
//
// Wire contract (server → client), one JSON object per frame:
//
//	{"topic":"drivers","data":[{"id","lat","lng","heading","speed_kmh","status","tier","battery"}]}
//	{"topic":"ride:<id>","data":{"ride_id","status","driver":{...},"eta_sec"}}
//	{"topic":"ops","data":{"type":"agent_event","agent","decision","reasoning","confidence","latency_ms","used_fallback","ts"}}
//	{"topic":"ops","data":{"type":"metrics", ...GET /v1/metrics/dispatch fields}}
//
// Client → server: {"action":"subscribe"|"unsubscribe","topics":["drivers","ops","ride:<id>"]}
package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Topics.
const (
	TopicDrivers    = "drivers"
	TopicOps        = "ops"
	RideTopicPrefix = "ride:"
	maxRideIDLen    = 64
	maxTopics       = 32
)

// RideTopic returns the topic carrying updates for one ride.
func RideTopic(rideID string) string { return RideTopicPrefix + rideID }

// ValidTopic reports whether t is a topic clients may subscribe to.
func ValidTopic(t string) bool {
	switch {
	case t == TopicDrivers || t == TopicOps:
		return true
	case strings.HasPrefix(t, RideTopicPrefix):
		id := t[len(RideTopicPrefix):]
		return id != "" && len(id) <= maxRideIDLen && !strings.ContainsAny(id, " \t\r\n")
	}
	return false
}

// latestOnly reports whether only the newest message per flush window matters
// (position snapshots) as opposed to every message (ops events).
func latestOnly(topic string) bool { return topic != TopicOps }

// Publisher publishes a message to every subscriber of topic on every instance.
type Publisher interface {
	Publish(topic string, data any)
}

// Driver is one vehicle in the drivers topic. Battery is null except for AVs.
type Driver struct {
	ID       string   `json:"id"`
	Lat      float64  `json:"lat"`
	Lng      float64  `json:"lng"`
	Heading  float64  `json:"heading"`
	SpeedKmh float64  `json:"speed_kmh"`
	Status   string   `json:"status"`
	Tier     string   `json:"tier"`
	Battery  *float64 `json:"battery"`
}

// RideUpdate is the ride:<id> payload.
type RideUpdate struct {
	RideID string `json:"ride_id"`
	Status string `json:"status"`
	Driver Driver `json:"driver"`
	ETASec int    `json:"eta_sec"`
}

// AgentEvent is an ops agent decision.
type AgentEvent struct {
	Type         string  `json:"type"` // always "agent_event"
	Agent        string  `json:"agent"`
	Decision     string  `json:"decision"`
	Reasoning    string  `json:"reasoning"`
	Confidence   float64 `json:"confidence"`
	LatencyMs    float64 `json:"latency_ms"`
	UsedFallback bool    `json:"used_fallback"`
	TS           string  `json:"ts"` // RFC 3339, UTC
}

// NewAgentEvent builds an ops agent_event payload.
func NewAgentEvent(agent, decision, reasoning string, confidence, latencyMs float64, usedFallback bool, ts time.Time) AgentEvent {
	r3 := func(v float64) float64 { return math.Round(v*1000) / 1000 }
	return AgentEvent{
		Type: "agent_event", Agent: agent, Decision: decision, Reasoning: reasoning,
		Confidence: r3(confidence), LatencyMs: r3(latencyMs), UsedFallback: usedFallback,
		TS: ts.UTC().Format(time.RFC3339),
	}
}

// MetricsData turns a metrics struct (the GET /v1/metrics/dispatch body) into
// the ops metrics payload: the same object with "type":"metrics" first.
func MetricsData(m any) (json.RawMessage, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) < 2 || b[0] != '{' {
		return nil, errors.New("stream: metrics must encode to a JSON object")
	}
	out := make([]byte, 0, len(b)+18)
	out = append(out, `{"type":"metrics"`...)
	if rest := b[1:]; len(rest) > 1 { // not "{}"
		out = append(out, ',')
		out = append(out, rest...)
	} else {
		out = append(out, '}')
	}
	return out, nil
}

type envelope struct {
	Topic string `json:"topic"`
	Data  any    `json:"data"`
}

// Encode renders one server → client frame.
func Encode(topic string, data any) ([]byte, error) {
	if ds, ok := data.([]Driver); ok && ds == nil {
		data = []Driver{}
	}
	return json.Marshal(envelope{Topic: topic, Data: data})
}

// ClientMessage is a client → server control message.
type ClientMessage struct {
	Action string   `json:"action"`
	Topics []string `json:"topics"`
}

// ParseClientMessage decodes and validates a control message.
func ParseClientMessage(b []byte) (ClientMessage, error) {
	var m ClientMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("stream: invalid message: %w", err)
	}
	if m.Action != "subscribe" && m.Action != "unsubscribe" {
		return m, fmt.Errorf("stream: unknown action %q", m.Action)
	}
	if len(m.Topics) == 0 || len(m.Topics) > maxTopics {
		return m, fmt.Errorf("stream: need 1..%d topics", maxTopics)
	}
	for _, t := range m.Topics {
		if !ValidTopic(t) {
			return m, fmt.Errorf("stream: invalid topic %q", t)
		}
	}
	return m, nil
}
