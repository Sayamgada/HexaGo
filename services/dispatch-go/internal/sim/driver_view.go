package sim

import (
	"math"

	"github.com/hexago/hexago/services/dispatch-go/internal/fleet"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
)

// DriverSummary is one row of the Driver module's picker (GET /v1/drivers).
type DriverSummary struct {
	ID         string   `json:"id"`
	Tier       string   `json:"tier"`
	Status     string   `json:"status"` // idle | en_route | on_trip | offline | incident | suspended
	AV         bool     `json:"av"`
	Rating     float64  `json:"rating"`
	BatteryPct *float64 `json:"battery_pct,omitempty"`
	Lat        float64  `json:"lat"`
	Lng        float64  `json:"lng"`
	RideID     string   `json:"ride_id,omitempty"`
}

// CurrentRide is the ride a driver is serving right now.
type CurrentRide struct {
	ID       string    `json:"id"`
	RiderID  string    `json:"rider_id"`
	Status   string    `json:"status"` // accepted | arriving | on_trip
	Pickup   geo.Point `json:"pickup"`
	Dropoff  geo.Point `json:"dropoff"`
	ETASec   int       `json:"eta_sec"`  // to the current milestone (pickup or drop-off)
	Progress float64   `json:"progress"` // 0..1 along the current leg's route
}

// AVStatus is the autonomous-vehicle panel.
type AVStatus struct {
	BatteryPct float64 `json:"battery_pct"`
	Health     string  `json:"health"`      // ok | service_due | low_battery
	HealthNote string  `json:"health_note"` // one line for the panel
	Mode       string  `json:"mode"`        // autonomous_to_pickup | autonomous_on_trip | repositioning | charging | minimal_risk_stop | remote_hold | parked
}

// DriverDetail is GET /v1/drivers/{id}.
type DriverDetail struct {
	DriverSummary
	Heading  float64      `json:"heading"`
	SpeedKmh float64      `json:"speed_kmh"`
	Ride     *CurrentRide `json:"ride,omitempty"`
	AV       *AVStatus    `json:"av_status,omitempty"`
}

// Drivers lists every simulated driver, ordered by number.
func (s *Sim) Drivers() []DriverSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DriverSummary, 0, len(s.order))
	for _, d := range s.order {
		if _, live := s.drivers[d.id]; live {
			out = append(out, s.summaryLocked(d))
		}
	}
	return out
}

// Driver returns one simulated driver's live detail.
func (s *Sim) Driver(id string) (DriverDetail, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drivers[id]
	if !ok {
		return DriverDetail{}, false
	}
	v := DriverDetail{DriverSummary: s.summaryLocked(d), Heading: round(math.Mod(d.heading+360, 360), 1), SpeedKmh: round(d.speed, 1)}
	if r := d.ride; r != nil && !r.status.Terminal() {
		v.Ride = &CurrentRide{
			ID: r.id, RiderID: r.rider, Status: string(r.status), Pickup: r.pickup, Dropoff: r.dropoff,
			ETASec: s.etaSec(d), Progress: legProgress(d),
		}
	}
	if d.tier == fleet.TierAV {
		v.AV = avStatus(d)
	}
	return v, true
}

func (s *Sim) summaryLocked(d *simDriver) DriverSummary {
	v := DriverSummary{
		ID: d.id, Tier: string(d.tier), Status: d.wireStatus(), AV: d.tier == fleet.TierAV,
		Rating: round(d.rating, 2), Lat: round(d.pos.Lat, 6), Lng: round(d.pos.Lng, 6),
	}
	if v.AV {
		b := round(d.battery, 1)
		v.BatteryPct = &b
	}
	if d.ride != nil {
		v.RideID = d.ride.id
	}
	return v
}

// legProgress is how far along the current leg's route the driver is (0..1).
func legProgress(d *simDriver) float64 {
	r := d.ride
	if d.route == nil || d.route.total <= 0 {
		return 0
	}
	if (r.status == RideArriving && d.purpose == toPickup) || (r.status == RideOnTrip && d.purpose == toDropoff) {
		return round(math.Min(1, math.Max(0, d.dist/d.route.total)), 3)
	}
	return 0
}

// avStatus derives the AV panel from simulator state (deterministic: the same
// vehicle and state always give the same answer).
func avStatus(d *simDriver) *AVStatus {
	a := &AVStatus{BatteryPct: round(d.battery, 1), Health: "ok", HealthNote: "All systems nominal: lidar, brakes, tyres, drivetrain"}
	switch {
	case d.battery < 20:
		a.Health, a.HealthNote = "low_battery", "Battery below 20 %: returns to charge after this trip"
	case d.n%13 == 0:
		a.Health, a.HealthNote = "service_due", "Tyre rotation due within 500 km; safe to operate"
	}
	switch {
	case d.suspended:
		a.Mode = "remote_hold"
	case d.incident != nil:
		a.Mode = "minimal_risk_stop"
	case d.ride != nil && d.ride.status == RideOnTrip:
		a.Mode = "autonomous_on_trip"
	case d.ride != nil:
		a.Mode = "autonomous_to_pickup"
	case d.offline:
		a.Mode = "parked"
	case d.speed < 0.5 && d.battery < 95:
		a.Mode = "charging"
	default:
		a.Mode = "repositioning"
	}
	return a
}
