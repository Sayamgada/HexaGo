package dispatch

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

func TestFeedLimiter(t *testing.T) {
	f := feedLimiter{interval: 5 * time.Second}
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	steps := []struct {
		at      time.Duration
		ok      bool
		skipped int
	}{
		{0, true, 0},
		{time.Second, false, 0},
		{4999 * time.Millisecond, false, 0},
		{5 * time.Second, true, 2},
		{6 * time.Second, false, 0},
		{11 * time.Second, true, 1},
		{30 * time.Second, true, 0},
	}
	for i, s := range steps {
		ok, skipped := f.allow(t0.Add(s.at))
		if ok != s.ok || skipped != s.skipped {
			t.Errorf("step %d (+%s): ok=%v skipped=%d, want %v/%d", i, s.at, ok, skipped, s.ok, s.skipped)
		}
	}
	unlimited := feedLimiter{}
	for i := 0; i < 3; i++ {
		if ok, _ := unlimited.allow(t0); !ok {
			t.Fatal("interval 0 must not limit")
		}
	}
}

// Match events reach the Ops feed at most once per 5 s; every one is still in agent_events.
func TestMatchEventsRateLimitedOnFeedButAllPersisted(t *testing.T) {
	mem := store.NewMemory()
	pub := &pubRecorder{}
	svc, err := New(Options{H3Resolution: 9, Center: center, Store: mem, Telemetry: mem, Publisher: pub,
		Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return clk }
	ride := func() {
		t.Helper()
		putDriver(t, svc, "d_"+clk.Format("150405.000"), center, fleet.TierMini)
		if _, err := svc.CreateRide(context.Background(), CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "mini"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ { // four matches within 3 s
		ride()
		clk = clk.Add(time.Second)
	}
	clk = clk.Add(3 * time.Second) // 7 s after the first
	ride()

	evs := pub.agentEvents()
	if len(evs) != 2 {
		t.Fatalf("published %d match events, want 2: %+v", len(evs), evs)
	}
	if !strings.Contains(evs[1].Reasoning, "+3 more dispatch decisions") {
		t.Errorf("second event should mention the 3 suppressed decisions: %q", evs[1].Reasoning)
	}
	if m, _ := mem.EventCountsSince(AgentName, "match", time.Time{}); m != 5 {
		t.Errorf("agent_events holds %d matches, want all 5", m)
	}
}

func TestSetMatchFeedIntervalDisablesLimit(t *testing.T) {
	svc, _, pub := newStreamHarness(t)
	svc.SetMatchFeedInterval(0)
	for i := 0; i < 3; i++ {
		putDriver(t, svc, "d_"+string(rune('a'+i)), center, fleet.TierMini)
		if _, err := svc.CreateRide(context.Background(), CreateRideInput{RiderID: "r", Pickup: &center, Dropoff: &dropoff, Tier: "mini"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(pub.agentEvents()); n != 3 {
		t.Fatalf("published %d, want 3", n)
	}
}
