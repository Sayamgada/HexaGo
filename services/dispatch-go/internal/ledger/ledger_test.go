package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// capture records published ops events.
type capture struct {
	mu  sync.Mutex
	evs []stream.AgentEvent
}

func (c *capture) Publish(topic string, data any) {
	if topic != stream.TopicOps {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, data.(stream.AgentEvent))
}

func (c *capture) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.evs))
	for i, e := range c.evs {
		out[i] = e.Kind
	}
	return out
}

func (c *capture) count(kind string) int {
	n := 0
	for _, k := range c.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

type fixture struct {
	l   *Ledger
	s   Store
	pub *capture
	now time.Time
}

func newFixture(t *testing.T, s Store, mut ...func(*Options)) *fixture {
	t.Helper()
	if s == nil {
		s = NewMemory()
	}
	f := &fixture{s: s, pub: &capture{}, now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	o := Options{Store: s, Publisher: f.pub, PayoutDelay: time.Millisecond, TestMode: true,
		Now: func() time.Time { return f.now }}
	for _, m := range mut {
		m(&o)
	}
	f.l = New(o)
	t.Cleanup(f.l.Close)
	return f
}

func ride(id string) RideInfo {
	return RideInfo{ID: id, RiderID: "r_" + id, DriverID: "d_001", Tier: "sedan", FarePaise: 25000}
}

func mustTrialZero(t *testing.T, l *Ledger) TrialBalance {
	t.Helper()
	tb, err := l.TrialBalance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tb.TotalPaise != 0 || !tb.Balanced {
		t.Fatalf("trial balance %d (balanced=%v): %+v", tb.TotalPaise, tb.Balanced, tb.Types)
	}
	return tb
}

func balance(t *testing.T, s Store, a Account) int64 {
	t.Helper()
	b, err := s.Balance(context.Background(), a.ID())
	if err != nil {
		t.Fatal(err)
	}
	return NormalBalance(a.Type, b)
}

func testSettleRidePostsBalancedEntryAndPaysOut(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	st, err := f.l.SettleRide(ctx, ride("ride-1"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Duplicate || st.Entry.Kind != KindRide || st.Entry.RideID != "ride-1" || st.Entry.IdempotencyKey != "ride:ride-1" {
		t.Fatalf("entry %+v", st.Entry)
	}
	if err := st.Entry.Validate(); err != nil {
		t.Fatal(err)
	}
	if st.Payout.Status != PayoutPending || st.Payout.AmountPaise != 18750 || st.Payout.DriverID != "d_001" {
		t.Fatalf("payout %+v", st.Payout)
	}
	f.l.Wait()
	p, err := f.s.Payout(ctx, st.Payout.ID)
	if err != nil || p.Status != PayoutSent || p.EntryID == "" || p.SentAt == nil {
		t.Fatalf("payout after wait %+v %v", p, err)
	}
	if got := balance(t, f.s, Acct(DriverPayable, "d_001")); got != 0 {
		t.Fatalf("driver payable after payout = %d", got)
	}
	if got := balance(t, f.s, Acct(PlatformRevenue, PlatformOwner)); got != 5477 {
		t.Fatalf("platform revenue = %d", got)
	}
	if got := balance(t, f.s, Acct(CityLevyPayable, CityOwner)); got != 500 {
		t.Fatalf("levy = %d", got)
	}
	if got := balance(t, f.s, Acct(TaxPayable, TaxOwner)); got != 273 {
		t.Fatalf("gst = %d", got)
	}
	mustTrialZero(t, f.l)
	if k := f.pub.kinds(); len(k) != 2 || k[0] != "ledger_entry" || k[1] != "payout_sent" {
		t.Fatalf("events %v", k)
	}
}

func testPayoutWithinOneSecond(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t), func(o *Options) { o.PayoutDelay = 0; o.Now = nil }) // default delay, real clock
	ctx := context.Background()
	start := time.Now()
	st, err := f.l.SettleRide(ctx, ride("fast"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p, _ := f.s.Payout(ctx, st.Payout.ID)
		if p.Status == PayoutSent {
			if lag := p.SentAt.Sub(start); lag > time.Second {
				t.Fatalf("payout lag %s", lag)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("payout not sent within 1 s")
}

func testReplayIsNoOp(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	first, err := f.l.SettleRide(ctx, ride("dup"))
	if err != nil {
		t.Fatal(err)
	}
	f.l.Wait()
	before, _ := f.l.TrialBalance(ctx)
	for i := 0; i < 5; i++ {
		again, err := f.l.SettleRide(ctx, ride("dup"))
		if err != nil {
			t.Fatal(err)
		}
		if !again.Duplicate || again.Entry.ID != first.Entry.ID {
			t.Fatalf("replay %d not a no-op: %+v", i, again)
		}
	}
	f.l.Wait()
	after := mustTrialZero(t, f.l)
	if after.Entries != before.Entries || after.Postings != before.Postings {
		t.Fatalf("replay wrote: %d/%d -> %d/%d", before.Entries, before.Postings, after.Entries, after.Postings)
	}
	es, _ := f.l.Entries(ctx, EntryFilter{RideID: "dup"})
	if len(es) != 2 { // the ride entry and its payout
		t.Fatalf("entries for ride = %d", len(es))
	}
	if n := f.pub.count("ledger_entry"); n != 1 {
		t.Fatalf("ledger_entry events = %d", n)
	}
}

func testSettleUsesWalletThenCard(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	if _, err := f.l.Topup(ctx, "r_w", 10000, ""); err != nil {
		t.Fatal(err)
	}
	r := ride("w")
	r.RiderID = "r_w"
	st, err := f.l.SettleRide(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var wallet, card int64
	for _, p := range st.Entry.Postings {
		switch p.Memo {
		case MemoWallet:
			wallet += p.AmountPaise
		case MemoCard:
			card += p.AmountPaise
		}
	}
	if wallet != 10000 || card != 15000 {
		t.Fatalf("wallet %d card %d", wallet, card)
	}
	if got := balance(t, f.s, Acct(RiderWallet, "r_w")); got != 0 {
		t.Fatalf("wallet left %d", got)
	}
	f.l.Wait()
	mustTrialZero(t, f.l)
}

func testSettleQuotesFareAndAppliesExtrasAndOperators(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t), func(o *Options) {
		o.Extras = func(id string) (int64, int64) { return 2500, 1000 }
	})
	ctx := context.Background()
	st, err := f.l.SettleRide(ctx, RideInfo{ID: "av-1", RiderID: "r1", DriverID: "d_007", Tier: "av", DistanceKm: 7.5})
	if err != nil {
		t.Fatal(err)
	}
	sp := st.Entry.Meta.Split
	if sp == nil || sp.FarePaise != 19000 || sp.PromoPaise != 2500 || sp.TipPaise != 1000 || sp.PayeeType != FleetOperatorPayable {
		t.Fatalf("split %+v", sp)
	}
	if st.Payout.Owner != AVOperator || st.Payout.AccountType != FleetOperatorPayable || st.Payout.DriverID != "d_007" {
		t.Fatalf("payout %+v", st.Payout)
	}
	if got := DefaultOperator("d_016", "sedan"); got != FleetOperator {
		t.Fatalf("d_016 operator = %q", got)
	}
	if got := DefaultOperator("d_017", "sedan"); got != "" {
		t.Fatalf("d_017 operator = %q", got)
	}
	if got := DefaultOperator("driver-x", "mini"); got != "" {
		t.Fatalf("non-numeric operator = %q", got)
	}
	f.l.Wait()
	if got := balance(t, f.s, Acct(PromoWallet, PlatformOwner)); got != 2500 {
		t.Fatalf("promo spend = %d", got)
	}
	mustTrialZero(t, f.l)
}

func TestSimExtrasAreDeterministic(t *testing.T) {
	promos, tips, big := 0, 0, 0
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("ride-%d", i)
		p, tip := SimExtras(id)
		p2, tip2 := SimExtras(id)
		if p != p2 || tip != tip2 {
			t.Fatal("not deterministic")
		}
		if p > 0 {
			promos++
		}
		if tip > 0 {
			tips++
		}
		if tip >= UnusualTipPaise {
			big++
		}
	}
	if promos < 300 || promos > 700 || tips < 500 || tips > 1000 || big == 0 || big > 20 {
		t.Fatalf("promos %d tips %d big %d", promos, tips, big)
	}
}

func testSettleRejectsBadRides(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	for _, r := range []RideInfo{{}, {ID: "x", DriverID: "d"}, {ID: "x", RiderID: "r"}, {ID: "x", RiderID: "r", DriverID: "d", FarePaise: -5}} {
		if _, err := f.l.SettleRide(ctx, r); !errors.Is(err, ErrValidation) {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

func testHeldPayoutReleasedOnlyWhenIncidentResolved(t *testing.T, mk storeMaker) {
	rides := map[string]RideInfo{"esc-1": {ID: "esc-1", RiderID: "r9", DriverID: "d_009", Tier: "mini", FarePaise: 12345}}
	f := newFixture(t, mk(t), func(o *Options) {
		o.Rides = func(_ context.Context, id string) (RideInfo, error) {
			r, ok := rides[id]
			if !ok {
				return RideInfo{}, ErrNotFound
			}
			return r, nil
		}
	})
	ctx := context.Background()
	res, err := f.l.HoldForIncident(ctx, Hold{IncidentID: "inc-1", DriverID: "d_009", RideID: "esc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Held) != 1 || res.Held[0].Status != PayoutHeld || res.Held[0].IncidentID != "inc-1" || res.Held[0].HeldAt == nil {
		t.Fatalf("held %+v", res)
	}
	f.l.Wait()
	// The fare is settled: the rider was charged, the driver payable holds the share.
	es, _ := f.l.Entries(ctx, EntryFilter{RideID: "esc-1"})
	if len(es) != 1 || !es[0].Meta.Interrupted || es[0].Meta.IncidentID != "inc-1" {
		t.Fatalf("entries %+v", es)
	}
	if got := balance(t, f.s, Acct(DriverPayable, "d_009")); got != 9258 {
		t.Fatalf("payable while held = %d", got)
	}
	// Further rides of a held driver are held too.
	r2 := RideInfo{ID: "esc-2", RiderID: "r9", DriverID: "d_009", Tier: "mini", FarePaise: 5000}
	st2, err := f.l.SettleRide(ctx, r2)
	if err != nil || st2.Payout.Status != PayoutHeld {
		t.Fatalf("second ride %+v %v", st2.Payout, err)
	}
	// A different incident does not release it.
	if rel, err := f.l.ReleaseIncident(ctx, "inc-other"); err != nil || len(rel) != 0 {
		t.Fatalf("other release %v %v", rel, err)
	}
	if f.pub.count("payout_sent") != 0 || f.pub.count("payout_held") != 2 {
		t.Fatalf("events %v", f.pub.kinds())
	}
	tot, _ := f.s.PayoutTotals(ctx, time.Time{})
	if tot.HeldCount != 2 || tot.HeldPaise != 9258+3750 {
		t.Fatalf("totals %+v", tot)
	}
	f.now = f.now.Add(time.Minute)
	rel, err := f.l.ReleaseIncident(ctx, "inc-1")
	if err != nil || len(rel) != 2 {
		t.Fatalf("release %+v %v", rel, err)
	}
	for _, p := range rel {
		if p.Status != PayoutSent || p.ReleasedAt == nil || p.SentAt == nil {
			t.Fatalf("released %+v", p)
		}
	}
	if got := balance(t, f.s, Acct(DriverPayable, "d_009")); got != 0 {
		t.Fatalf("payable after release = %d", got)
	}
	// Released twice: nothing more.
	if rel, err := f.l.ReleaseIncident(ctx, "inc-1"); err != nil || len(rel) != 0 {
		t.Fatalf("second release %v %v", rel, err)
	}
	// The hold is lifted: new rides pay out normally.
	st3, _ := f.l.SettleRide(ctx, RideInfo{ID: "esc-3", RiderID: "r9", DriverID: "d_009", Tier: "mini", FarePaise: 5000})
	if st3.Payout.Status != PayoutPending {
		t.Fatalf("after release %+v", st3.Payout)
	}
	f.l.Wait()
	mustTrialZero(t, f.l)
	if f.pub.count("payout_released") != 2 {
		t.Fatalf("events %v", f.pub.kinds())
	}
}

func testHoldAfterSettlementHoldsPendingPayout(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t), func(o *Options) { o.PayoutDelay = time.Hour })
	ctx := context.Background()
	st, _ := f.l.SettleRide(ctx, ride("late"))
	res, err := f.l.HoldForIncident(ctx, Hold{IncidentID: "inc-2", DriverID: "d_001", RideID: "late"})
	if err != nil || len(res.Held) != 1 || res.Held[0].ID != st.Payout.ID {
		t.Fatalf("hold %+v %v", res, err)
	}
	f.l.Close() // flushes the timer: the pending send finds the payout held and skips it
	p, _ := f.s.Payout(ctx, st.Payout.ID)
	if p.Status != PayoutHeld {
		t.Fatalf("payout %+v", p)
	}
	// Holding a ride whose payout was already sent reports it as too late.
	g := newFixture(t, mk(t))
	st2, _ := g.l.SettleRide(ctx, ride("sent"))
	g.l.Wait()
	res2, err := g.l.HoldForIncident(ctx, Hold{IncidentID: "inc-3", DriverID: "d_001", RideID: "sent"})
	if err != nil || len(res2.Held) != 0 || len(res2.AlreadySent) != 1 || res2.AlreadySent[0] != st2.Payout.ID {
		t.Fatalf("hold sent %+v %v", res2, err)
	}
	// Unknown rides without a lookup only hold the driver.
	res3, err := g.l.HoldForIncident(ctx, Hold{IncidentID: "inc-4", DriverID: "d_002", RideID: "nope"})
	if err != nil || len(res3.Held) != 0 || res3.Settled {
		t.Fatalf("hold unknown %+v %v", res3, err)
	}
	if _, err := g.l.HoldForIncident(ctx, Hold{DriverID: "d_002"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("hold without incident: %v", err)
	}
}

func testRefundIsReversalEntry(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	st, _ := f.l.SettleRide(ctx, RideInfo{ID: "rf", RiderID: "r1", DriverID: "d_001", Tier: "sedan", FarePaise: 25000, TipPaise: 2000})
	f.l.Wait() // payout sent: the driver payable is empty
	e, err := f.l.Refund(ctx, RefundInput{RideID: "rf", AmountPaise: 5400, Reason: "route dispute"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindRefund || e.ReversesID != st.Entry.ID || e.Meta.Reason != "route dispute" {
		t.Fatalf("refund %+v", e)
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	// The original entry is untouched.
	orig, _ := f.s.EntryByKey(ctx, "ride:rf")
	if len(orig.Postings) != len(st.Entry.Postings) {
		t.Fatal("original entry edited")
	}
	// The rider is credited; the driver share the payable no longer holds is absorbed by the platform.
	if got := balance(t, f.s, Acct(RiderWallet, "r1")); got != 5400 {
		t.Fatalf("wallet %d", got)
	}
	if got := balance(t, f.s, Acct(DriverPayable, "d_001")); got != 0 {
		t.Fatalf("payable %d", got)
	}
	mustTrialZero(t, f.l)
	// Refunds are capped at what the rider paid.
	if _, err := f.l.Refund(ctx, RefundInput{RideID: "rf", AmountPaise: 27000 - 5400 + 1}); !errors.Is(err, ErrValidation) {
		t.Fatalf("over-refund: %v", err)
	}
	if _, err := f.l.Refund(ctx, RefundInput{RideID: "rf", AmountPaise: 27000 - 5400, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	// Same key: no-op.
	again, err := f.l.Refund(ctx, RefundInput{RideID: "rf", AmountPaise: 27000 - 5400, IdempotencyKey: "k1"})
	if err != nil || again.IdempotencyKey != "refund:rf:k1" {
		t.Fatalf("replayed refund %+v %v", again, err)
	}
	if _, err := f.l.Refund(ctx, RefundInput{RideID: "rf", AmountPaise: 1}); !errors.Is(err, ErrValidation) {
		t.Fatalf("fully refunded ride accepted more: %v", err)
	}
	for _, bad := range []RefundInput{{RideID: "nope", AmountPaise: 1}, {RideID: "rf", AmountPaise: 0}, {AmountPaise: 5}} {
		if _, err := f.l.Refund(ctx, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	mustTrialZero(t, f.l)
	view, err := f.l.Ride(ctx, "rf")
	if err != nil || view.Breakdown == nil || view.Breakdown.RefundedPaise != 27000 || len(view.Entries) != 4 {
		t.Fatalf("ride view %+v %v", view.Breakdown, err)
	}
}

func testRefundBeforePayoutReversesDriverShare(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t), func(o *Options) { o.PayoutDelay = time.Hour })
	ctx := context.Background()
	_, _ = f.l.SettleRide(ctx, ride("pre"))
	if _, err := f.l.Refund(ctx, RefundInput{RideID: "pre", AmountPaise: 25000}); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, f.s, Acct(DriverPayable, "d_001")); got != 0 {
		t.Fatalf("payable after full refund = %d", got)
	}
	if got := balance(t, f.s, Acct(PlatformRevenue, PlatformOwner)); got != 0 {
		t.Fatalf("platform after full refund = %d", got)
	}
	f.l.Close() // the payout finds nothing to pay
	p, _ := f.s.Payout(ctx, "po_pre")
	if p.Status != PayoutSent || p.AmountPaise != 0 || p.EntryID != "" {
		t.Fatalf("payout %+v", p)
	}
	mustTrialZero(t, f.l)
}

func testTopup(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	e, err := f.l.Topup(ctx, "r1", 50000, "t1")
	if err != nil || e.Kind != KindTopup {
		t.Fatalf("%+v %v", e, err)
	}
	if again, _ := f.l.Topup(ctx, "r1", 50000, "t1"); again.ID != e.ID {
		t.Fatal("topup replay wrote a new entry")
	}
	w, err := f.l.Wallet(ctx, "r1")
	if err != nil || len(w.Accounts) != 1 || w.Accounts[0].BalancePaise != 50000 || w.BalancePaise != 50000 {
		t.Fatalf("wallet %+v %v", w, err)
	}
	for _, amt := range []int64{0, -1, MaxTopupPaise + 1} {
		if _, err := f.l.Topup(ctx, "r1", amt, ""); !errors.Is(err, ErrValidation) {
			t.Errorf("topup %d: %v", amt, err)
		}
	}
	if _, err := f.l.Topup(ctx, "", 100, ""); !errors.Is(err, ErrValidation) {
		t.Error("empty owner accepted")
	}
	g := newFixture(t, mk(t), func(o *Options) { o.TestMode = false })
	if _, err := g.l.Topup(ctx, "r1", 100, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("topup outside test mode: %v", err)
	}
	mustTrialZero(t, f.l)
}

func testSummaryAndWalletViews(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _ = f.l.SettleRide(ctx, ride(fmt.Sprintf("s%d", i)))
	}
	f.l.Wait()
	_, _ = f.l.HoldForIncident(ctx, Hold{IncidentID: "i", DriverID: "d_002"})
	_, _ = f.l.SettleRide(ctx, RideInfo{ID: "h", RiderID: "r", DriverID: "d_002", Tier: "sedan", FarePaise: 10000})
	s, err := f.l.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Balanced || s.TrialBalancePaise != 0 || s.PayoutsToday.SentCount != 3 || s.PayoutsToday.SentPaise != 3*18750 ||
		s.PayoutsToday.HeldCount != 1 || s.PlatformRevenuePaise != 3*5477+2191 || s.Store != f.s.Kind() || s.Rides != 4 {
		t.Fatalf("summary %+v", s)
	}
	w, err := f.l.Wallet(ctx, "d_001")
	if err != nil || len(w.Payouts) != 3 || w.BalancePaise != 0 || w.PaidOutPaise != 3*18750 {
		t.Fatalf("driver wallet %+v %v", w, err)
	}
	w2, _ := f.l.Wallet(ctx, "d_002")
	if w2.HeldPaise != 7500 || w2.BalancePaise != 7500 {
		t.Fatalf("held wallet %+v", w2)
	}
	if _, err := f.l.Wallet(ctx, " "); !errors.Is(err, ErrValidation) {
		t.Fatal("blank owner accepted")
	}
	v, err := f.l.Ride(ctx, "s0")
	if err != nil || v.Breakdown == nil || v.Payout == nil || v.Breakdown.PayeePaise != 18750 || v.Breakdown.CardPaise != 25000 {
		t.Fatalf("ride view %+v %v", v, err)
	}
	if _, err := f.l.Ride(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ride: %v", err)
	}
	if !strings.Contains(Rupees(123456), "1234.56") || Rupees(-5) != "-₹0.05" {
		t.Fatalf("Rupees %q %q", Rupees(123456), Rupees(-5))
	}
}

// 100 rides settle concurrently, each replayed twice, while a few riders top up.
func testConcurrentSettlements(t *testing.T, mk storeMaker) {
	f := newFixture(t, mk(t))
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for i := 0; i < 100; i++ {
		for rep := 0; rep < 3; rep++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := RideInfo{ID: fmt.Sprintf("c%03d", i), RiderID: fmt.Sprintf("r%d", i%7), DriverID: fmt.Sprintf("d_%03d", i%10),
					Tier: "sedan", FarePaise: int64(5000 + i*37), TipPaise: int64(i % 3 * 100)}
				if _, err := f.l.SettleRide(ctx, r); err != nil {
					errs <- err
				}
			}(i)
		}
		if i%10 == 0 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := f.l.Topup(ctx, fmt.Sprintf("r%d", i%7), 3000, ""); err != nil {
					errs <- err
				}
			}(i)
		}
	}
	wg.Wait()
	f.l.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	tb := mustTrialZero(t, f.l)
	rides, _ := f.l.Entries(ctx, EntryFilter{Kind: KindRide, Limit: 1000})
	payouts, _ := f.l.Entries(ctx, EntryFilter{Kind: KindPayout, Limit: 1000})
	if len(rides) != 100 || len(payouts) != 100 {
		t.Fatalf("rides %d payouts %d (entries %d)", len(rides), len(payouts), tb.Entries)
	}
	for i := 0; i < 10; i++ {
		if b := balance(t, f.s, Acct(DriverPayable, fmt.Sprintf("d_%03d", i))); b != 0 {
			t.Fatalf("driver %d payable %d", i, b)
		}
	}
}
