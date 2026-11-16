package dispatch

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

type pubRecorder struct {
	mu   sync.Mutex
	msgs []struct {
		topic string
		data  any
	}
}

func (p *pubRecorder) Publish(topic string, data any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, struct {
		topic string
		data  any
	}{topic, data})
}

func (p *pubRecorder) agentEvents() []stream.AgentEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []stream.AgentEvent
	for _, m := range p.msgs {
		if ev, ok := m.data.(stream.AgentEvent); ok && m.topic == stream.TopicOps {
			out = append(out, ev)
		}
	}
	return out
}

func newStreamHarness(t *testing.T) (*Service, *store.Memory, *pubRecorder) {
	t.Helper()
	mem := store.NewMemory()
	pub := &pubRecorder{}
	svc, err := New(Options{
		H3Resolution: 9, Center: center, Store: mem, Telemetry: mem,
		Registry: prometheus.NewRegistry(), Logger: log.New(io.Discard, "", 0),
		Publisher: pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, mem, pub
}

func putDriver(t *testing.T, svc *Service, id string, p geo.Point, tier fleet.Tier) {
	t.Helper()
	if _, err := svc.Fleet().Upsert(fleet.Driver{ID: id, Lat: p.Lat, Lng: p.Lng, Status: fleet.StatusIdle, Tier: tier, Rating: 4.8}); err != nil {
		t.Fatal(err)
	}
}

var dropoff = geo.Point{Lat: 12.9784, Lng: 77.6408}

func TestMatchPublishesAgentEventAndPersistsIt(t *testing.T) {
	svc, mem, pub := newStreamHarness(t)
	putDriver(t, svc, "d_001", center, fleet.TierSedan)
	view, err := svc.CreateRide(context.Background(), CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "sedan"})
	if err != nil || view.Match == nil {
		t.Fatalf("create: %v %+v", err, view)
	}
	evs := pub.agentEvents()
	if len(evs) != 1 {
		t.Fatalf("published %d agent events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Type != "agent_event" || ev.Agent != "dispatch_broker" || ev.UsedFallback {
		t.Fatalf("event %+v", ev)
	}
	if !strings.Contains(ev.Decision, "d_001") || !strings.Contains(ev.Decision, view.Ride.ID) {
		t.Fatalf("decision %q should name the driver and ride", ev.Decision)
	}
	if ev.Reasoning == "" || ev.Confidence <= 0 || ev.Confidence > 1 {
		t.Fatalf("reasoning/confidence %+v", ev)
	}
	if _, err := time.Parse(time.RFC3339, ev.TS); err != nil {
		t.Fatalf("ts %q: %v", ev.TS, err)
	}
	stored, _ := mem.RecentEvents(context.Background(), 10)
	if len(stored) != 1 || stored[0].Agent != AgentName || stored[0].EventType != "match" {
		t.Fatalf("agent_events rows %+v", stored)
	}
}

func TestNoMatchAlsoPublishes(t *testing.T) {
	svc, _, pub := newStreamHarness(t)
	if _, err := svc.CreateRide(context.Background(), CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "sedan"}); err != nil {
		t.Fatal(err)
	}
	evs := pub.agentEvents()
	if len(evs) != 1 || !strings.Contains(evs[0].Decision, "no driver") {
		t.Fatalf("events %+v", evs)
	}
}

func TestRideObserverSeesMatchAndCancel(t *testing.T) {
	svc, _, _ := newStreamHarness(t)
	var mu sync.Mutex
	var seen []store.RideStatus
	svc.SetRideObserver(func(r store.Ride) {
		mu.Lock()
		seen = append(seen, r.Status)
		mu.Unlock()
	})
	putDriver(t, svc, "d_001", center, fleet.TierSedan)
	view, err := svc.CreateRide(context.Background(), CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "sedan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelRide(context.Background(), view.Ride.ID, ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != store.RideMatched || seen[1] != store.RideCancelled {
		t.Fatalf("observer saw %v", seen)
	}
}

func TestAdvanceRide(t *testing.T) {
	svc, mem, _ := newStreamHarness(t)
	ctx := context.Background()
	putDriver(t, svc, "d_001", center, fleet.TierSedan)
	view, err := svc.CreateRide(ctx, CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "sedan"})
	if err != nil {
		t.Fatal(err)
	}
	id := view.Ride.ID

	// matched → completed skips a step.
	if _, err := svc.AdvanceRide(ctx, id, store.RideCompleted); !errors.Is(err, ErrConflict) {
		t.Fatalf("skip to completed: %v", err)
	}
	r, err := svc.AdvanceRide(ctx, id, store.RideInProgress)
	if err != nil || r.Status != store.RideInProgress {
		t.Fatalf("start trip: %v %+v", err, r)
	}
	if d, _ := svc.Fleet().Get("d_001"); d.Status != fleet.StatusOnTrip {
		t.Fatalf("driver %s, want on_trip", d.Status)
	}
	r, err = svc.AdvanceRide(ctx, id, store.RideCompleted)
	if err != nil || r.Status != store.RideCompleted {
		t.Fatalf("complete: %v %+v", err, r)
	}
	if d, _ := svc.Fleet().Get("d_001"); d.Status != fleet.StatusIdle {
		t.Fatalf("driver %s, want idle", d.Status)
	}
	got, _ := mem.GetRide(ctx, id)
	if got.Status != store.RideCompleted {
		t.Fatalf("stored %s", got.Status)
	}
	if _, err := svc.AdvanceRide(ctx, "nope", store.RideInProgress); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ride: %v", err)
	}
	if _, err := svc.AdvanceRide(ctx, id, store.RideRequested); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad target: %v", err)
	}
}

func TestRecordAgentEventPublishesAndPersists(t *testing.T) {
	svc, mem, pub := newStreamHarness(t)
	svc.RecordAgentEvent(context.Background(), AgentEventInput{
		Agent: "safety_sentinel", EventType: "route_deviation", RideID: "r1",
		Decision: "flagged d_1", Reasoning: "off route", Confidence: 0.9, LatencyMs: 0.2, UsedFallback: true,
	})
	evs := pub.agentEvents()
	if len(evs) != 1 || evs[0].Agent != "safety_sentinel" || !evs[0].UsedFallback {
		t.Fatalf("published %+v", evs)
	}
	stored, _ := mem.RecentEvents(context.Background(), 10)
	if len(stored) != 1 || stored[0].EventType != "route_deviation" || !stored[0].FallbackUsed {
		t.Fatalf("stored %+v", stored)
	}
}

func TestCloseStaleRides(t *testing.T) {
	svc, mem, _ := newStreamHarness(t)
	ctx := context.Background()
	if _, err := svc.CreateRide(ctx, CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "sedan"}); err != nil {
		t.Fatal(err)
	}
	n, err := svc.CloseStaleRides(ctx, time.Now().Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("closed %d, %v", n, err)
	}
	c, _ := mem.RideCounts(ctx)
	if c.Open != 0 || c.InProgress != 0 {
		t.Fatalf("counts %+v", c)
	}
}
