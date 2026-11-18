package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"testing/quick"
	"time"
)

// Random sequences of rides (any tier, fleet/AV, promos, tips, replays),
// refunds, top-ups, safety holds and releases, and payouts must always keep
// the trial balance at zero, every guarded account non-negative, and every
// entry balanced.
func TestPropertyLedgerInvariants(t *testing.T) {
	check := func(seed uint64) bool {
		if err := runRandomOps(t, NewMemory(), seed, 120); err != nil {
			t.Logf("seed %d: %v", seed, err)
			return false
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

func runRandomOps(t *testing.T, s Store, seed uint64, steps int) error {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	now := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	l := New(Options{Store: s, TestMode: true, PayoutDelay: time.Hour, Now: func() time.Time { return now }})
	defer l.Close()
	tiers := []string{"auto", "mini", "sedan", "premium", "xl", "av"}
	var settled []string
	incidents := []string{}
	pending := []string{}

	for i := 0; i < steps; i++ {
		now = now.Add(time.Duration(rng.IntN(5000)) * time.Millisecond)
		switch op := rng.IntN(10); {
		case op < 4: // settle a new ride (or replay an old one)
			id := fmt.Sprintf("r%d-%d", seed, i)
			if len(settled) > 0 && rng.IntN(4) == 0 {
				id = settled[rng.IntN(len(settled))]
			}
			r := RideInfo{ID: id, RiderID: fmt.Sprintf("rider%d", rng.IntN(5)), DriverID: fmt.Sprintf("d_%03d", rng.IntN(12)),
				Tier: tiers[rng.IntN(len(tiers))], DistanceKm: rng.Float64() * 30}
			if rng.IntN(2) == 0 {
				r.FarePaise = 1 + rng.Int64N(200_000)
				r.PromoPaise = rng.Int64N(3) * rng.Int64N(r.FarePaise+5000)
				r.TipPaise = rng.Int64N(3) * rng.Int64N(10_000)
			}
			st, err := l.SettleRide(ctx, r)
			if err != nil {
				return fmt.Errorf("settle %s: %w", id, err)
			}
			if !st.Duplicate {
				settled = append(settled, id)
				if st.Payout.Status == PayoutPending {
					pending = append(pending, st.Payout.ID)
				}
			}
		case op < 5 && len(pending) > 0: // a payout fires
			j := rng.IntN(len(pending))
			l.sendNow(pending[j])
			pending = append(pending[:j], pending[j+1:]...)
		case op < 7 && len(settled) > 0: // refund (possibly too much)
			id := settled[rng.IntN(len(settled))]
			_, err := l.Refund(ctx, RefundInput{RideID: id, AmountPaise: 1 + rng.Int64N(60_000), Reason: "property"})
			if err != nil && !errors.Is(err, ErrValidation) {
				return fmt.Errorf("refund %s: %w", id, err)
			}
		case op < 8: // top-up
			if _, err := l.Topup(ctx, fmt.Sprintf("rider%d", rng.IntN(5)), 1+rng.Int64N(50_000), ""); err != nil {
				return fmt.Errorf("topup: %w", err)
			}
		case op < 9: // safety escalation on a driver (with or without a ride)
			inc := fmt.Sprintf("inc-%d", i)
			h := Hold{IncidentID: inc, DriverID: fmt.Sprintf("d_%03d", rng.IntN(12))}
			if len(settled) > 0 && rng.IntN(2) == 0 {
				h.RideID = settled[rng.IntN(len(settled))]
			}
			if _, err := l.HoldForIncident(ctx, h); err != nil {
				return fmt.Errorf("hold: %w", err)
			}
			incidents = append(incidents, inc)
		default: // an incident is resolved
			if len(incidents) == 0 {
				continue
			}
			j := rng.IntN(len(incidents))
			if _, err := l.ReleaseIncident(ctx, incidents[j]); err != nil {
				return fmt.Errorf("release: %w", err)
			}
			incidents = append(incidents[:j], incidents[j+1:]...)
		}
		if err := checkInvariants(ctx, l, s); err != nil {
			return fmt.Errorf("after step %d: %w", i, err)
		}
	}
	// Drain: every queued payout fires, every incident resolves.
	for _, id := range pending {
		l.sendNow(id)
	}
	for _, inc := range incidents {
		if _, err := l.ReleaseIncident(ctx, inc); err != nil {
			return err
		}
	}
	if err := checkInvariants(ctx, l, s); err != nil {
		return fmt.Errorf("after drain: %w", err)
	}
	// Nothing is owed to drivers or operators once every payout has gone out.
	tt, _ := s.TypeTotals(ctx)
	for _, x := range tt {
		if (x.Type == DriverPayable || x.Type == FleetOperatorPayable) && x.BalancePaise != 0 {
			return fmt.Errorf("%s still holds %d after drain", x.Type, x.BalancePaise)
		}
	}
	return nil
}

func checkInvariants(ctx context.Context, l *Ledger, s Store) error {
	tb, err := l.TrialBalance(ctx)
	if err != nil {
		return err
	}
	if tb.TotalPaise != 0 {
		return fmt.Errorf("trial balance %d", tb.TotalPaise)
	}
	es, err := s.Entries(ctx, EntryFilter{Limit: 1000})
	if err != nil {
		return err
	}
	owners := map[string]bool{}
	for _, e := range es {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("entry %s: %w", e.ID, err)
		}
		for _, p := range e.Postings {
			owners[p.Owner] = true
		}
	}
	for o := range owners {
		bs, err := s.OwnerBalances(ctx, o)
		if err != nil {
			return err
		}
		for _, b := range bs {
			if b.Type.Guarded() && b.BalancePaise < 0 {
				return fmt.Errorf("%s is negative: %d", b.Account, b.BalancePaise)
			}
		}
	}
	return nil
}
