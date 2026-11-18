package ledger

import (
	"context"
	"errors"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

// RideInfoFromStore converts a dispatched ride (priced from the fare table).
func RideInfoFromStore(r store.Ride) RideInfo {
	return RideInfo{ID: r.ID, RiderID: r.RiderID, DriverID: r.DriverID, Tier: r.Tier, DistanceKm: r.DistanceKm}
}

// incidentKey is the hold key of a suspension (a manual one has no incident id).
func incidentKey(sp safety.Suspension) string {
	if sp.IncidentID != "" {
		return sp.IncidentID
	}
	return "suspension:" + sp.DriverID
}

// Attach hooks the ledger into the ride state machine and the safety service:
// every completed ride is settled; a safety escalation (driver suspension)
// settles the interrupted ride with its payout held and holds the driver's
// pending payouts; the Ops-confirmed release sends them. sf may be nil.
func Attach(l *Ledger, svc *dispatch.Service, sf *safety.Service) {
	l.mu.Lock()
	l.rides = func(ctx context.Context, id string) (RideInfo, error) {
		v, err := svc.GetRide(ctx, id)
		if errors.Is(err, dispatch.ErrNotFound) {
			return RideInfo{}, ErrNotFound
		}
		if err != nil {
			return RideInfo{}, err
		}
		return RideInfoFromStore(v.Ride), nil
	}
	l.mu.Unlock()
	svc.OnRideCompleted(func(ctx context.Context, r store.Ride) {
		if _, err := l.SettleRide(ctx, RideInfoFromStore(r)); err != nil {
			l.log.Printf("ERROR ledger: settle ride %s: %v", r.ID, err)
		}
	})
	if sf == nil {
		return
	}
	sf.OnSuspension(func(ctx context.Context, sp safety.Suspension) {
		if _, err := l.HoldForIncident(ctx, Hold{IncidentID: incidentKey(sp), DriverID: sp.DriverID, RideID: sp.Result.InterruptedRideID}); err != nil {
			l.log.Printf("ERROR ledger: hold payouts for %s: %v", sp.DriverID, err)
		}
	}, func(ctx context.Context, sp safety.Suspension) {
		if _, err := l.ReleaseIncident(ctx, incidentKey(sp)); err != nil {
			l.log.Printf("ERROR ledger: release payouts for %s: %v", sp.DriverID, err)
		}
	})
}
