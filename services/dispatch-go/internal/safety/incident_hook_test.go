package safety

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestIncidentOpenedHookFiresOnSentinelReply(t *testing.T) {
	replies := []string{
		`{"action":"escalate","incident_id":"inc_9","severity":0.9,"reason":"no reply"}`,
		`{"action":"monitor","incident_id":null,"severity":0.2}`, // no incident
		`{"action":"check_in","incident_id":""}`,                 // empty id
		`not json`,
	}
	var mu sync.Mutex
	i := 0
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		body := replies[i%len(replies)]
		i++
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(body))
	}))
	defer agents.Close()

	s := New(Options{AgentsURL: agents.URL, Logger: quiet})
	var got []IncidentOpened
	s.OnIncidentOpened(func(_ context.Context, inc IncidentOpened) {
		mu.Lock()
		got = append(got, inc)
		mu.Unlock()
	})
	sig := sampleSignal()
	for range replies {
		s.Emit(context.Background(), sig)
		s.Wait()
	}
	if len(got) != 1 {
		t.Fatalf("hook fired %d times: %+v", len(got), got)
	}
	inc := got[0]
	if inc.IncidentID != "inc_9" || inc.Action != "escalate" || inc.Severity != 0.9 || inc.Reason != "no reply" ||
		inc.RideID != sig.RideID || inc.DriverID != sig.DriverID || inc.SignalID != sig.ID || inc.At.IsZero() ||
		len(inc.Triggers) != len(sig.Triggers) {
		t.Fatalf("incident %+v", inc)
	}
}

func TestNoIncidentHookWhenDeliveryFails(t *testing.T) {
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"incident_id":"inc_x"}`))
	}))
	defer agents.Close()
	s := New(Options{AgentsURL: agents.URL, Logger: quiet})
	fired := false
	s.OnIncidentOpened(func(context.Context, IncidentOpened) { fired = true })
	s.Emit(context.Background(), sampleSignal())
	s.Wait()
	if fired {
		t.Fatal("hook fired for a failed delivery")
	}
}
