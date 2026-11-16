// Package store persists rides, drivers, telemetry and agent events.
//
// Store covers transactional data (Postgres in production, Memory when
// DATABASE_URL is unset). TelemetryStore is deliberately separate so the
// high-volume location stream can move to ScyllaDB without touching rides.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// Errors returned by stores.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: already exists")
)

// RideStatus is a ride's lifecycle state.
type RideStatus string

// Ride lifecycle: requested → matched → in_progress → completed, or → cancelled.
const (
	RideRequested  RideStatus = "requested"   // open, waiting for a driver
	RideMatched    RideStatus = "matched"     // driver assigned and en route to pickup
	RideInProgress RideStatus = "in_progress" // rider on board
	RideCompleted  RideStatus = "completed"
	RideCancelled  RideStatus = "cancelled"
)

// Active reports whether a driver is currently committed to the ride.
func (s RideStatus) Active() bool { return s == RideMatched || s == RideInProgress }

// Terminal reports whether the ride is finished.
func (s RideStatus) Terminal() bool { return s == RideCompleted || s == RideCancelled }

// Leg kinds.
const (
	LegPickup = "pickup" // driver → pickup
	LegTrip   = "trip"   // pickup → dropoff
)

// RideLeg is one segment of a ride.
type RideLeg struct {
	Seq        int       `json:"seq"`
	Kind       string    `json:"kind"`
	From       geo.Point `json:"from"`
	To         geo.Point `json:"to"`
	DistanceKm float64   `json:"distance_km"`
	ETASeconds float64   `json:"eta_seconds"`
}

// Ride is a rider's trip request and its dispatch outcome.
type Ride struct {
	ID           string     `json:"id"`
	RiderID      string     `json:"rider_id"`
	DriverID     string     `json:"driver_id,omitempty"`
	Tier         string     `json:"tier"`
	Status       RideStatus `json:"status"`
	Pickup       geo.Point  `json:"pickup"`
	Dropoff      geo.Point  `json:"dropoff"`
	PickupH3     string     `json:"pickup_h3"`
	ETASeconds   float64    `json:"eta_seconds,omitempty"` // driver → pickup
	DistanceKm   float64    `json:"distance_km"`           // pickup → dropoff (road estimate)
	MatchScore   float64    `json:"match_score,omitempty"`
	CancelReason string     `json:"cancel_reason,omitempty"`
	Legs         []RideLeg  `json:"legs"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (r Ride) clone() Ride {
	r.Legs = append([]RideLeg(nil), r.Legs...)
	if r.Legs == nil {
		r.Legs = []RideLeg{}
	}
	return r
}

// DriverRecord is the persisted driver + vehicle profile.
type DriverRecord struct {
	ID         string
	Status     string
	Tier       string
	Rating     float64
	BatteryPct *float64 // AVs only
}

// RideCounts summarises non-terminal rides.
type RideCounts struct {
	Open       int // requested
	InProgress int // matched + in_progress
}

// AgentEvent is one row of the append-only agent audit trail.
type AgentEvent struct {
	ID           int64           `json:"id"`
	Agent        string          `json:"agent"`
	EventType    string          `json:"event_type"`
	RideID       string          `json:"ride_id,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Decision     json.RawMessage `json:"decision,omitempty"`
	Confidence   float64         `json:"confidence"`
	FallbackUsed bool            `json:"fallback_used"`
	TokenCost    float64         `json:"token_cost"`
	LatencyMs    float64         `json:"latency_ms"`
	CreatedAt    time.Time       `json:"created_at"`
}

// TelemetryPoint is one driver location ping.
type TelemetryPoint struct {
	DriverID   string    `json:"driver_id"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	Heading    float64   `json:"heading"`
	SpeedKmh   float64   `json:"speed_kmh"`
	Status     string    `json:"status"`
	H3Cell     string    `json:"h3_cell"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Store persists transactional dispatch data.
type Store interface {
	Kind() string
	Ping(ctx context.Context) error
	Close()

	UpsertDriver(ctx context.Context, d DriverRecord) error
	// CreateRide inserts a ride and its legs, creating the rider (and driver,
	// if set) rows on demand. Returns ErrConflict if the ID exists.
	CreateRide(ctx context.Context, r Ride) error
	// UpdateRide updates status, driver, ETA, score, cancel reason and
	// updated_at. Legs are immutable after creation.
	UpdateRide(ctx context.Context, r Ride) error
	GetRide(ctx context.Context, id string) (Ride, error)
	// ActiveRideForDriver returns the driver's matched/in-progress ride.
	ActiveRideForDriver(ctx context.Context, driverID string) (Ride, error)
	RideCounts(ctx context.Context) (RideCounts, error)

	// CloseStaleRides cancels non-terminal rides last updated before `before`
	// (e.g. left over from a previous process whose drivers no longer exist).
	CloseStaleRides(ctx context.Context, before time.Time, reason string, now time.Time) (int64, error)

	RecordEvent(ctx context.Context, e AgentEvent) error
	// RecentEvents returns the newest events first.
	RecentEvents(ctx context.Context, limit int) ([]AgentEvent, error)
}

// TelemetryStore persists the driver location stream.
type TelemetryStore interface {
	AppendTelemetry(ctx context.Context, p TelemetryPoint) error
	// RecentTelemetry returns up to limit points for a driver, newest first (limit <= 0: all retained).
	RecentTelemetry(ctx context.Context, driverID string, limit int) ([]TelemetryPoint, error)
}

// BatchTelemetryStore can write many points in one round trip.
type BatchTelemetryStore interface {
	AppendTelemetryBatch(ctx context.Context, ps []TelemetryPoint) error
}

// TelemetryPruner deletes points recorded before a cutoff.
type TelemetryPruner interface {
	PruneTelemetry(ctx context.Context, before time.Time) (int64, error)
}
