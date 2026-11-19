package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hexago/hexago/services/dispatch-go/internal/dispatch"
	"github.com/hexago/hexago/services/dispatch-go/internal/geo"
	"github.com/hexago/hexago/services/dispatch-go/internal/safety"
	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

var errBoom = errors.New("boom")

// faulty wraps a Store and fails the named methods (each fails `n` times, -1 = always).
type faulty struct {
	Store
	fail map[string]int
	skip map[string]int // calls that succeed before failing starts
}

func (f *faulty) hit(name string) error {
	if f.skip[name] > 0 {
		f.skip[name]--
		return nil
	}
	n, ok := f.fail[name]
	if !ok || n == 0 {
		return nil
	}
	if n > 0 {
		f.fail[name] = n - 1
	}
	return errBoom
}

func (f *faulty) Apply(ctx context.Context, c Commit) (Applied, error) {
	if err := f.hit("Apply"); err != nil {
		return Applied{}, err
	}
	if c.Entry != nil {
		if err := f.hit("ApplyNegative"); err != nil {
			return Applied{}, ErrNegativeBalance
		}
	}
	return f.Store.Apply(ctx, c)
}
func (f *faulty) EntryByKey(ctx context.Context, k string) (Entry, error) {
	if err := f.hit("EntryByKey"); err != nil {
		return Entry{}, err
	}
	return f.Store.EntryByKey(ctx, k)
}
func (f *faulty) Entries(ctx context.Context, x EntryFilter) ([]Entry, error) {
	if err := f.hit("Entries"); err != nil {
		return nil, err
	}
	return f.Store.Entries(ctx, x)
}
func (f *faulty) Balance(ctx context.Context, a string) (int64, error) {
	if err := f.hit("Balance"); err != nil {
		return 0, err
	}
	return f.Store.Balance(ctx, a)
}
func (f *faulty) OwnerBalances(ctx context.Context, o string) ([]AccountBalance, error) {
	if err := f.hit("OwnerBalances"); err != nil {
		return nil, err
	}
	return f.Store.OwnerBalances(ctx, o)
}
func (f *faulty) TypeTotals(ctx context.Context) ([]TypeTotal, error) {
	if err := f.hit("TypeTotals"); err != nil {
		return nil, err
	}
	return f.Store.TypeTotals(ctx)
}
func (f *faulty) Counts(ctx context.Context) (Counts, error) {
	if err := f.hit("Counts"); err != nil {
		return Counts{}, err
	}
	return f.Store.Counts(ctx)
}
func (f *faulty) Payout(ctx context.Context, id string) (Payout, error) {
	if err := f.hit("Payout"); err != nil {
		return Payout{}, err
	}
	return f.Store.Payout(ctx, id)
}
func (f *faulty) Payouts(ctx context.Context, x PayoutFilter) ([]Payout, error) {
	if err := f.hit("Payouts"); err != nil {
		return nil, err
	}
	return f.Store.Payouts(ctx, x)
}
func (f *faulty) PayoutTotals(ctx context.Context, since time.Time) (PayoutTotals, error) {
	if err := f.hit("PayoutTotals"); err != nil {
		return PayoutTotals{}, err
	}
	return f.Store.PayoutTotals(ctx, since)
}

func (f *faulty) RidesSettledSince(ctx context.Context, since time.Time) (int64, error) {
	if err := f.hit("RidesSettledSince"); err != nil {
		return 0, err
	}
	return f.Store.RidesSettledSince(ctx, since)
}

func quietLogger() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}

func faultyLedger(t *testing.T, fail map[string]int) (*Ledger, *faulty, *bytes.Buffer) {
	t.Helper()
	f := &faulty{Store: NewMemory(), fail: fail}
	lg, buf := quietLogger()
	l := New(Options{Store: f, TestMode: true, PayoutDelay: time.Hour, Logger: lg})
	t.Cleanup(l.Close)
	return l, f, buf
}

func TestStoreErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	views := map[string]func(l *Ledger) error{
		"TrialBalance/TypeTotals": func(l *Ledger) error { _, err := l.TrialBalance(ctx); return err },
		"Summary/PayoutTotals":    func(l *Ledger) error { _, err := l.Summary(ctx); return err },
		"Wallet/OwnerBalances":    func(l *Ledger) error { _, err := l.Wallet(ctx, "x"); return err },
		"Wallet/Payouts":          func(l *Ledger) error { _, err := l.Wallet(ctx, "x"); return err },
		"Ride/Entries":            func(l *Ledger) error { _, err := l.Ride(ctx, "x"); return err },
		"Topup/Apply":             func(l *Ledger) error { _, err := l.Topup(ctx, "x", 5, ""); return err },
		"Settle/Balance":          func(l *Ledger) error { _, err := l.SettleRide(ctx, ride("x")); return err },
		"Settle/Apply":            func(l *Ledger) error { _, err := l.SettleRide(ctx, ride("x")); return err },
		"Refund/EntryByKey":       func(l *Ledger) error { _, err := l.Refund(ctx, RefundInput{RideID: "x", AmountPaise: 1}); return err },
		"Release/Payouts":         func(l *Ledger) error { _, err := l.ReleaseIncident(ctx, "i"); return err },
		"Hold/Payouts": func(l *Ledger) error {
			_, err := l.HoldForIncident(ctx, Hold{IncidentID: "i", DriverID: "d"})
			return err
		},
	}
	for name, fn := range views {
		method := name[strings.Index(name, "/")+1:]
		l, _, _ := faultyLedger(t, map[string]int{method: -1})
		if err := fn(l); !errors.Is(err, errBoom) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Counts fails after TypeTotals succeeded (trial balance), and in Summary after its trial balance.
	l, _, _ := faultyLedger(t, map[string]int{"Counts": -1})
	if _, err := l.TrialBalance(ctx); !errors.Is(err, errBoom) {
		t.Errorf("trial balance counts: %v", err)
	}
	l, f, _ := faultyLedger(t, map[string]int{"Counts": -1})
	f.skip = map[string]int{"Counts": 1}
	if _, err := l.Summary(ctx); !errors.Is(err, errBoom) {
		t.Errorf("summary counts: %v", err)
	}
}

func TestRetriesAndLateFailures(t *testing.T) {
	ctx := context.Background()
	// Negative-balance races are retried (re-reading balances) until they clear.
	l, f, _ := faultyLedger(t, map[string]int{"ApplyNegative": 3})
	if _, err := l.SettleRide(ctx, ride("retry")); err != nil {
		t.Fatalf("settle retry: %v", err)
	}
	f.fail["ApplyNegative"] = 3
	if _, err := l.Refund(ctx, RefundInput{RideID: "retry", AmountPaise: 100}); err != nil {
		t.Fatalf("refund retry: %v", err)
	}
	f.fail["ApplyNegative"] = 3
	p, _ := f.Store.Payout(ctx, "po_retry")
	if _, err := l.send(ctx, p, false); err != nil {
		t.Fatalf("send retry: %v", err)
	}
	// ...but not forever: a balance that never clears surfaces the error.
	f.fail["ApplyNegative"] = -1
	if _, err := l.SettleRide(ctx, ride("never")); !errors.Is(err, ErrNegativeBalance) {
		t.Fatalf("persistent negative balance: %v", err)
	}
	// A cancelled context stops the backoff.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if retryBalance(cctx, ErrNegativeBalance, 1) {
		t.Fatal("retried after cancellation")
	}
	if retryBalance(ctx, errBoom, 0) || !retryBalance(ctx, ErrNegativeBalance, 0) || retryBalance(ctx, ErrNegativeBalance, maxBalanceRetries) {
		t.Fatal("retryBalance policy")
	}
	f.fail["ApplyNegative"] = 0
	// Refund failures after the original was found.
	for _, method := range []string{"Entries", "Balance", "Apply"} {
		l, f, _ := faultyLedger(t, map[string]int{})
		_, _ = l.SettleRide(ctx, ride("rf"))
		f.fail[method] = -1
		if _, err := l.Refund(ctx, RefundInput{RideID: "rf", AmountPaise: 100}); !errors.Is(err, errBoom) {
			t.Errorf("refund with %s failing: %v", method, err)
		}
	}
	// A ride entry without a split cannot be refunded.
	m := NewMemory()
	_, _ = m.Apply(ctx, Commit{Entry: &Entry{ID: "e", Kind: KindRide, IdempotencyKey: "ride:bare", RideID: "bare",
		Postings: []Posting{Post(Acct(PaymentClearing, GatewayOwner), 5, "x"), Post(Acct(PlatformRevenue, PlatformOwner), -5, "x")}}})
	if _, err := New(Options{Store: m}).Refund(ctx, RefundInput{RideID: "bare", AmountPaise: 1}); !errors.Is(err, ErrValidation) {
		t.Errorf("bare refund: %v", err)
	}
	// send: balance and apply failures; sendNow logs them.
	for _, method := range []string{"Balance", "Apply"} {
		l, f, buf := faultyLedger(t, map[string]int{})
		_, _ = l.SettleRide(ctx, ride("snd"))
		f.fail[method] = -1
		p, _ := f.Store.Payout(ctx, "po_snd")
		if _, err := l.send(ctx, p, false); !errors.Is(err, errBoom) {
			t.Errorf("send with %s failing: %v", method, err)
		}
		l.sendNow("po_snd")
		if !strings.Contains(buf.String(), "send payout") {
			t.Errorf("sendNow did not log: %q", buf.String())
		}
	}
	l, _, buf := faultyLedger(t, map[string]int{"Payout": -1})
	l.sendNow("po_none")
	if !strings.Contains(buf.String(), "payout po_none") {
		t.Errorf("sendNow lookup failure not logged: %q", buf.String())
	}
	// A duplicate payout send returns the stored payout.
	l2 := New(Options{PayoutDelay: time.Hour})
	_, _ = l2.SettleRide(ctx, ride("twice"))
	p1, _ := l2.store.Payout(ctx, "po_twice")
	if _, err := l2.send(ctx, p1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.send(ctx, p1, false); !errors.Is(err, ErrPayoutState) {
		t.Fatalf("second send: %v", err)
	}
	l2.Close()
	// After Close, scheduling sends immediately.
	_, _ = l2.SettleRide(ctx, ride("closed"))
	if p, _ := l2.store.Payout(ctx, "po_closed"); p.Status != PayoutSent {
		t.Fatalf("post-close payout %+v", p)
	}
	// Topup replay with a failing duplicate path is still a no-op.
	if _, err := l2.Topup(ctx, "r", 5, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("topup not in test mode: %v", err)
	}
}

func TestHoldAndReleaseFailures(t *testing.T) {
	ctx := context.Background()
	// The interrupted-ride lookup fails (not "not found"): logged, driver still held.
	lg, buf := quietLogger()
	l := New(Options{Logger: lg, Rides: func(context.Context, string) (RideInfo, error) { return RideInfo{}, errBoom }})
	if res, err := l.HoldForIncident(ctx, Hold{IncidentID: "i", DriverID: "d", RideID: "r"}); err != nil || res.Settled {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(buf.String(), "look up interrupted ride") {
		t.Fatalf("not logged: %q", buf.String())
	}
	// The lookup returns an unsettleable ride.
	l = New(Options{Rides: func(context.Context, string) (RideInfo, error) { return RideInfo{ID: "r"}, nil }})
	if _, err := l.HoldForIncident(ctx, Hold{IncidentID: "i", DriverID: "d", RideID: "r"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad ride: %v", err)
	}
	// Holding a pending payout fails in the store; or it was sent meanwhile.
	for _, tc := range []struct {
		fail map[string]int
		want error
	}{{map[string]int{"Apply": -1}, errBoom}, {map[string]int{}, nil}} {
		l, f, _ := faultyLedger(t, map[string]int{})
		_, _ = l.SettleRide(ctx, ride("h"))
		if tc.want == nil { // sent between the listing and the hold
			p, _ := f.Store.Payout(ctx, "po_h")
			_, _ = l.send(ctx, p, false)
			f.Store = &stalePayouts{Store: f.Store, p: p}
		}
		for k, v := range tc.fail {
			f.fail[k] = v
		}
		res, err := l.HoldForIncident(ctx, Hold{IncidentID: "i", DriverID: "d_001"})
		if !errors.Is(err, tc.want) || (tc.want == nil && len(res.Held) != 0) {
			t.Fatalf("hold %+v %v", res, err)
		}
	}
	// Release: send fails, or the payout changed state meanwhile.
	l2, f2, _ := faultyLedger(t, map[string]int{})
	_, _ = l2.HoldForIncident(ctx, Hold{IncidentID: "i", DriverID: "d_001"})
	_, _ = l2.SettleRide(ctx, ride("rel"))
	f2.fail["Balance"] = -1
	if _, err := l2.ReleaseIncident(ctx, "i"); !errors.Is(err, errBoom) {
		t.Fatalf("release send failure: %v", err)
	}
	f2.fail["Balance"] = 0
	p, _ := f2.Store.Payout(ctx, "po_rel")
	f2.Store = &stalePayouts{Store: f2.Store, p: Payout{}}
	moved := p
	moved.Status = PayoutPending
	_, _ = f2.Store.(*stalePayouts).Store.Apply(ctx, Commit{Payout: &moved, From: PayoutHeld})
	f2.Store.(*stalePayouts).p = p // listing still says held
	if out, err := l2.ReleaseIncident(ctx, "i"); err != nil || len(out) != 0 {
		t.Fatalf("stale release %+v %v", out, err)
	}
	if _, err := l2.ReleaseIncident(ctx, ""); !errors.Is(err, ErrValidation) {
		t.Fatal("empty incident released")
	}
}

// stalePayouts returns a stale snapshot of one payout from Payouts.
type stalePayouts struct {
	Store
	p Payout
}

func (s *stalePayouts) Payouts(context.Context, PayoutFilter) ([]Payout, error) {
	return []Payout{s.p}, nil
}

func TestStatusMapping(t *testing.T) {
	for err, code := range map[error]int{
		ErrValidation: http.StatusBadRequest, ErrUnbalanced: http.StatusBadRequest, ErrNotFound: http.StatusNotFound,
		ErrForbidden: http.StatusForbidden, ErrNegativeBalance: http.StatusConflict, ErrPayoutState: http.StatusConflict,
		errBoom: http.StatusInternalServerError,
	} {
		if got := Status(fmt.Errorf("wrapped: %w", err)); got != code {
			t.Errorf("%v -> %d, want %d", err, got, code)
		}
	}
}

func TestAttachFailuresAreLogged(t *testing.T) {
	ctx := context.Background()
	svc, err := dispatch.New(dispatch.Options{Center: geo.Point{Lat: 12.97, Lng: 77.59}, Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	sf := safety.New(safety.Options{Positions: svc.Fleet(), Logger: log.New(os.Stderr, "", 0)})
	lg, buf := quietLogger()
	l, f := New(Options{Logger: lg, TestMode: true}), (*faulty)(nil)
	_ = f
	fs := &faulty{Store: NewMemory(), fail: map[string]int{"Apply": -1, "Payouts": -1}}
	l.store = fs
	Attach(l, svc, sf)
	Attach(New(Options{}), svc, nil) // nil safety service: rides only

	// A completed ride whose settlement fails is logged, not lost silently.
	if _, err := svc.UpdateDriverStatus(ctx, "d_001", dispatch.DriverStatusInput{Status: sptr("idle"), Lat: ptr(12.97), Lng: ptr(77.59), Tier: sptr("sedan")}); err != nil {
		t.Fatal(err)
	}
	p, d := geo.Point{Lat: 12.971, Lng: 77.591}, geo.Point{Lat: 12.99, Lng: 77.61}
	v, err := svc.CreateRide(ctx, dispatch.CreateRideInput{RiderID: "r", Pickup: &p, Dropoff: &d, Tier: "sedan"})
	if err != nil || v.Match == nil {
		t.Fatalf("%+v %v", v, err)
	}
	_, _ = svc.AdvanceRide(ctx, v.Ride.ID, store.RideInProgress)
	if _, err := svc.AdvanceRide(ctx, v.Ride.ID, store.RideCompleted); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "settle ride") {
		t.Fatalf("settle failure not logged: %q", buf.String())
	}
	// Ride lookup: found, not found.
	if info, err := l.rides(ctx, v.Ride.ID); err != nil || info.DriverID != "d_001" {
		t.Fatalf("lookup %+v %v", info, err)
	}
	if _, err := l.rides(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup missing: %v", err)
	}
	// A manual suspension (no incident id) holds under a derived key; failures are logged.
	if _, err := sf.Suspend(ctx, "d_001", "", "manual", "ops"); err != nil {
		t.Fatal(err)
	}
	if err := sf.Unsuspend(ctx, "d_001", safety.ConfirmingAgent); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "hold payouts for d_001") || !strings.Contains(buf.String(), "release payouts for d_001") {
		t.Fatalf("hold/release failures not logged: %q", buf.String())
	}
	if incidentKey(safety.Suspension{DriverID: "d"}) != "suspension:d" || incidentKey(safety.Suspension{IncidentID: "i"}) != "i" {
		t.Fatal("incidentKey")
	}
}

func ptr(v float64) *float64 { return &v }
func sptr(v string) *string  { return &v }

// A closed pool makes every Postgres read and write fail cleanly.
func TestPostgresStoreErrors(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool := pgPool(t)
	s := NewPostgres(pool)
	ctx := context.Background()
	_, _ = s.Apply(ctx, Commit{Entry: entry("seed", Post(Acct(PaymentClearing, GatewayOwner), 5, "x"), Post(Acct(RiderWallet, "r"), -5, "x"))})
	pool.Close()
	if _, err := s.Apply(ctx, Commit{Entry: entry("k", Post(Acct(PaymentClearing, GatewayOwner), 5, "x"), Post(Acct(RiderWallet, "r"), -5, "x"))}); err == nil {
		t.Error("Apply")
	}
	if _, err := s.EntryByKey(ctx, "seed"); err == nil || errors.Is(err, ErrNotFound) {
		t.Error("EntryByKey")
	}
	if _, err := s.Entries(ctx, EntryFilter{}); err == nil {
		t.Error("Entries")
	}
	if _, err := s.Balance(ctx, "x"); err == nil {
		t.Error("Balance")
	}
	if _, err := s.OwnerBalances(ctx, "r"); err == nil {
		t.Error("OwnerBalances")
	}
	if _, err := s.TypeTotals(ctx); err == nil {
		t.Error("TypeTotals")
	}
	if _, err := s.Counts(ctx); err == nil {
		t.Error("Counts")
	}
	if _, err := s.Payout(ctx, "p"); err == nil || errors.Is(err, ErrNotFound) {
		t.Error("Payout")
	}
	if _, err := s.Payouts(ctx, PayoutFilter{}); err == nil {
		t.Error("Payouts")
	}
	if _, err := s.PayoutTotals(ctx, time.Time{}); err == nil {
		t.Error("PayoutTotals")
	}
	if err := s.attachPostings(ctx, []Entry{{ID: "x"}}); err == nil {
		t.Error("attachPostings")
	}
}
