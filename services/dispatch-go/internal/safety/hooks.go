package safety

import (
	"context"
	"encoding/json"
	"time"
)

// SuspensionHook observes a suspension (escalation) or its Ops-confirmed release.
type SuspensionHook func(ctx context.Context, sp Suspension)

// OnSuspension registers hooks run synchronously after a driver is suspended
// (the ledger holds that driver's payouts) and after an ops_supervisor-
// confirmed release (the ledger releases them). Either may be nil.
func (s *Service) OnSuspension(suspended, released SuspensionHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if suspended != nil {
		s.onSuspend = append(s.onSuspend, suspended)
	}
	if released != nil {
		s.onRelease = append(s.onRelease, released)
	}
}

// IncidentOpened is the Safety Sentinel opening an incident (check-in or
// escalation) for a signal dispatch-go sent it.
type IncidentOpened struct {
	IncidentID string    `json:"incident_id"`
	Action     string    `json:"action"` // check_in | escalate
	Severity   float64   `json:"severity"`
	Reason     string    `json:"reason"`
	CheckinID  string    `json:"checkin_id,omitempty"`
	SignalID   string    `json:"signal_id"`
	RideID     string    `json:"ride_id"`
	DriverID   string    `json:"driver_id"`
	RiderID    string    `json:"rider_id,omitempty"`
	Triggers   []Trigger `json:"triggers"`
	At         time.Time `json:"opened_at"`
}

// OnIncidentOpened registers fn to run (on the signal's delivery goroutine)
// whenever the sentinel's reply to a signal names a new incident.
func (s *Service) OnIncidentOpened(fn func(context.Context, IncidentOpened)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onIncident = append(s.onIncident, fn)
}

// incidentFromReply fires the incident hooks when the sentinel opened one.
func (s *Service) incidentFromReply(ctx context.Context, sig Signal, reply []byte) {
	var out struct {
		IncidentID *string `json:"incident_id"`
		Action     string  `json:"action"`
		Severity   float64 `json:"severity"`
		Reason     string  `json:"reason"`
		CheckinID  string  `json:"checkin_id"`
	}
	if json.Unmarshal(reply, &out) != nil || out.IncidentID == nil || *out.IncidentID == "" {
		return
	}
	s.mu.Lock()
	hs := append([]func(context.Context, IncidentOpened){}, s.onIncident...)
	s.mu.Unlock()
	ev := IncidentOpened{IncidentID: *out.IncidentID, Action: out.Action, Severity: out.Severity, Reason: out.Reason,
		CheckinID: out.CheckinID, SignalID: sig.ID, RideID: sig.RideID, DriverID: sig.DriverID, RiderID: sig.RiderID, Triggers: sig.Triggers, At: s.now().UTC()}
	for _, h := range hs {
		h(ctx, ev)
	}
}

func (s *Service) fire(ctx context.Context, release bool, sp Suspension) {
	s.mu.Lock()
	hooks := s.onSuspend
	if release {
		hooks = s.onRelease
	}
	hs := append([]SuspensionHook{}, hooks...)
	s.mu.Unlock()
	for _, h := range hs {
		h(ctx, sp)
	}
}
