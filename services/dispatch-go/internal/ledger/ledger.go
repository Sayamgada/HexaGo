package ledger

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexago/hexago/services/dispatch-go/internal/stream"
)

// Agent is the agent name ledger events are published under.
const Agent = "settlement"

// Defaults and limits.
const (
	DefaultPayoutDelay = 300 * time.Millisecond // simulated bank transfer; always < 1 s
	MaxTopupPaise      = 10_000_000             // ₹1 lakh per test top-up
	UnusualTipPaise    = 50_000                 // ₹500: the simulator's rare large tip
	AVOperator         = "hexago_av_fleet"
	FleetOperator      = "fleet_op_blr"
	payoutTimeout      = 5 * time.Second
)

// Event kinds published on the ops topic.
const (
	EvEntryPosted    = "ledger_entry"
	EvPayoutSent     = "payout_sent"
	EvPayoutHeld     = "payout_held"
	EvPayoutReleased = "payout_released"
	EvRefund         = "refund"
)

// RideInfo is a finished ride to settle.
type RideInfo struct {
	ID          string
	RiderID     string
	DriverID    string
	Tier        string
	DistanceKm  float64
	FarePaise   int64 // 0 = quote from the fare table (and apply Extras)
	PromoPaise  int64
	TipPaise    int64
	OperatorID  string // "" = Options.Operator decides
	IncidentID  string // set: hold the payout for review
	Interrupted bool   // ended by a safety escalation
}

// Options configures a Ledger.
type Options struct {
	Store       Store            // nil = NewMemory()
	Rates       Rates            // zero = DefaultRates()
	Fares       FareTable        // nil = DefaultFares()
	Publisher   stream.Publisher // ops events; nil = none
	Now         func() time.Time
	PayoutDelay time.Duration // 0 = DefaultPayoutDelay
	TestMode    bool          // enables wallet top-ups
	// Operator returns the fleet operator paid for a driver's rides ("" = the driver). nil = DefaultOperator.
	Operator func(driverID, tier string) string
	// Extras returns the promo and tip of a ride priced from the fare table (nil = none).
	Extras func(rideID string) (promoPaise, tipPaise int64)
	// Rides looks up a ride a safety escalation interrupted, so it can be settled (nil = cannot).
	Rides  func(ctx context.Context, id string) (RideInfo, error)
	Logger *log.Logger
}

// Ledger settles rides, pays out earnings and serves balances.
type Ledger struct {
	store    Store
	rates    Rates
	fares    FareTable
	pub      stream.Publisher
	now      func() time.Time
	delay    time.Duration
	testMode bool
	operator func(string, string) string
	extras   func(string) (int64, int64)
	rides    func(context.Context, string) (RideInfo, error)
	log      *log.Logger

	mu     sync.Mutex
	holds  map[string]string // driver id -> incident id while an escalation is open
	stop   chan struct{}
	closed bool
	once   sync.Once
	wg     sync.WaitGroup
}

// New builds a Ledger.
func New(o Options) *Ledger {
	if o.Store == nil {
		o.Store = NewMemory()
	}
	if o.Rates == (Rates{}) {
		o.Rates = DefaultRates()
	}
	if o.Fares == nil {
		o.Fares = DefaultFares()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PayoutDelay <= 0 {
		o.PayoutDelay = DefaultPayoutDelay
	}
	if o.Operator == nil {
		o.Operator = DefaultOperator
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return &Ledger{
		store: o.Store, rates: o.Rates, fares: o.Fares, pub: o.Publisher, now: func() time.Time { return o.Now().UTC() },
		delay: o.PayoutDelay, testMode: o.TestMode, operator: o.Operator, extras: o.Extras, rides: o.Rides,
		log: o.Logger, holds: map[string]string{}, stop: make(chan struct{}),
	}
}

// StoreKind reports the backing store ("memory" or "postgres").
func (l *Ledger) StoreKind() string { return l.store.Kind() }

// Rates returns the settlement rates.
func (l *Ledger) Rates() Rates { return l.rates }

// Close sends every queued payout now and waits for them.
func (l *Ledger) Close() {
	l.once.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.stop)
		l.mu.Unlock()
	})
	l.wg.Wait()
}

// Wait blocks until every queued payout has been attempted (tests).
func (l *Ledger) Wait() { l.wg.Wait() }

// DefaultOperator: AVs belong to the HexaGo AV fleet; every eighth simulated
// car (d_008, d_016, ...) is fleet-owned; everyone else is independent.
func DefaultOperator(driverID, tier string) string {
	if strings.EqualFold(tier, "av") {
		return AVOperator
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(driverID, "d_")); err == nil && strings.HasPrefix(driverID, "d_") && n%8 == 0 {
		return FleetOperator
	}
	return ""
}

// SimExtras derives a deterministic promo and tip from the ride id for
// simulated rides: about 10 % carry a ₹25 promo, about 14 % a tip, and
// roughly one in a thousand an unusually large tip.
func SimExtras(rideID string) (promoPaise, tipPaise int64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(rideID))
	v := h.Sum64()
	if v%10 == 0 {
		promoPaise = 2500
	}
	switch {
	case (v/10)%997 == 0:
		tipPaise = UnusualTipPaise
	case (v/10)%7 == 0:
		tipPaise = []int64{1000, 2000, 3000, 5000}[(v/7000)%4]
	}
	return promoPaise, tipPaise
}

// Rupees formats paise as "₹1234.56".
func Rupees(p int64) string {
	sign := ""
	if p < 0 {
		sign, p = "-", -p
	}
	return fmt.Sprintf("%s₹%d.%02d", sign, p/100, p%100)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (l *Ledger) publish(kind, rideID, driverID, decision, reasoning string, started time.Time) {
	if l.pub == nil {
		return
	}
	ev := stream.NewAgentEvent(Agent, decision, reasoning, 1, float64(time.Since(started).Microseconds())/1000, false, l.now())
	ev.Kind, ev.RideID, ev.DriverID = kind, rideID, driverID
	l.pub.Publish(stream.TopicOps, ev)
}

// ---- ride settlement ----

// Settlement is the result of settling one ride.
type Settlement struct {
	Entry     Entry  `json:"entry"`
	Payout    Payout `json:"payout"`
	Duplicate bool   `json:"duplicate"` // the ride was already settled: nothing was written
}

func rideKey(id string) string { return "ride:" + id }

// maxBalanceRetries bounds how often an operation re-reads balances after a
// concurrent write moved a guarded account under it (ErrNegativeBalance): with
// many settlements and top-ups of one rider in flight, a single retry can lose
// the race again.
const maxBalanceRetries = 8

// retryBalance reports whether the failed attempt (0-based) should be retried,
// after a short jittered backoff so contending writers spread out.
func retryBalance(ctx context.Context, err error, attempt int) bool {
	if !errors.Is(err, ErrNegativeBalance) || attempt >= maxBalanceRetries {
		return false
	}
	if attempt == 0 {
		return true // the common case: retry at once
	}
	backoff := time.Duration(1+rand.IntN(1<<min(attempt, 5))) * time.Millisecond
	select {
	case <-ctx.Done():
		return false
	case <-time.After(backoff):
		return true
	}
}

// SettleRide posts the ride's balanced journal entry (idempotency key = ride
// id) and its payout atomically, then pays out within PayoutDelay unless the
// driver is under a safety hold. Replaying a ride is a no-op.
func (l *Ledger) SettleRide(ctx context.Context, r RideInfo) (Settlement, error) {
	started := time.Now()
	if strings.TrimSpace(r.ID) == "" || r.RiderID == "" || r.DriverID == "" {
		return Settlement{}, fmt.Errorf("%w: ride needs id, rider and driver", ErrValidation)
	}
	in := SplitInput{Tier: r.Tier, FarePaise: r.FarePaise, PromoPaise: r.PromoPaise, TipPaise: r.TipPaise, OperatorID: r.OperatorID}
	if in.FarePaise == 0 {
		in.FarePaise = l.fares.Quote(r.Tier, r.DistanceKm)
		if l.extras != nil {
			in.PromoPaise, in.TipPaise = l.extras(r.ID)
		}
	}
	if in.OperatorID == "" {
		in.OperatorID = l.operator(r.DriverID, r.Tier)
	}
	split, err := ComputeSplit(in, l.rates)
	if err != nil {
		return Settlement{}, err
	}
	incident := r.IncidentID
	if incident == "" {
		l.mu.Lock()
		incident = l.holds[r.DriverID]
		l.mu.Unlock()
	}
	payee := Acct(split.PayeeType, r.DriverID)
	if in.OperatorID != "" {
		payee = Acct(FleetOperatorPayable, in.OperatorID)
	}

	var applied Applied
	for attempt := 0; ; attempt++ {
		c, err := l.rideCommit(ctx, r, in.OperatorID, split, payee, incident)
		if err != nil {
			return Settlement{}, err
		}
		applied, err = l.store.Apply(ctx, c)
		if retryBalance(ctx, err, attempt) {
			continue // the rider's wallet moved under us: re-split wallet vs card
		}
		if err != nil {
			return Settlement{}, err
		}
		break
	}
	out := Settlement{Duplicate: applied.Duplicate}
	if applied.Entry != nil {
		out.Entry = *applied.Entry
	}
	if applied.Payout != nil {
		out.Payout = *applied.Payout
	}
	if applied.Duplicate {
		return out, nil
	}
	l.publish(EvEntryPosted, r.ID, r.DriverID,
		fmt.Sprintf("ride %s settled: fare %s, %s %s, levy %s, GST %s, platform %s", short(r.ID), Rupees(split.FarePaise),
			payeeLabel(split.PayeeType), Rupees(split.PayeePaise()), Rupees(split.CityLevyPaise), Rupees(split.GSTPaise), Rupees(split.PlatformFeePaise)),
		fmt.Sprintf("Balanced entry %s (%d postings, idempotency key %s); rider charged %s, promo %s, tip %s.",
			out.Entry.ID, len(out.Entry.Postings), out.Entry.IdempotencyKey, Rupees(split.RiderChargePaise), Rupees(split.PromoPaise), Rupees(split.TipPaise)),
		started)
	switch {
	case applied.Payout == nil:
	case out.Payout.Status == PayoutHeld:
		l.publish(EvPayoutHeld, r.ID, r.DriverID,
			fmt.Sprintf("payout %s held for review (incident %s)", Rupees(out.Payout.AmountPaise), incident),
			fmt.Sprintf("Ride %s ended in a safety escalation; the fare is settled but %s stays in %s until the incident is resolved.",
				short(r.ID), Rupees(out.Payout.AmountPaise), payee.ID()), started)
	default:
		l.schedule(out.Payout.ID)
	}
	return out, nil
}

func payeeLabel(t AccountType) string {
	if t == FleetOperatorPayable {
		return "operator"
	}
	return "driver"
}

func (l *Ledger) rideCommit(ctx context.Context, r RideInfo, operator string, s Split, payee Account, incident string) (Commit, error) {
	wallet := Acct(RiderWallet, r.RiderID)
	signed, err := l.store.Balance(ctx, wallet.ID())
	if err != nil {
		return Commit{}, err
	}
	fromWallet := min(max(NormalBalance(RiderWallet, signed), 0), s.RiderChargePaise)
	ps := make([]Posting, 0, 9)
	add := func(a Account, amt int64, memo string) {
		if amt != 0 {
			ps = append(ps, Post(a, amt, memo))
		}
	}
	add(wallet, fromWallet, MemoWallet)
	add(Acct(PaymentClearing, GatewayOwner), s.RiderChargePaise-fromWallet, MemoCard)
	add(Acct(PromoWallet, PlatformOwner), s.PromoPaise, MemoPromo)
	add(payee, -s.PayeeSharePaise, MemoPayeeShare)
	add(payee, -s.TipPaise, MemoTip)
	add(Acct(CityLevyPayable, CityOwner), -s.CityLevyPaise, MemoCityLevy)
	add(Acct(TaxPayable, TaxOwner), -s.GSTPaise, MemoGST)
	add(Acct(PlatformRevenue, PlatformOwner), -s.PlatformFeePaise, MemoPlatformFee)

	now := l.now()
	split, rates := s, l.rates
	e := &Entry{
		ID: newID("je_"), Kind: KindRide, IdempotencyKey: rideKey(r.ID), RideID: r.ID, CreatedAt: now,
		Description: fmt.Sprintf("%s ride %s: fare %s", r.Tier, r.ID, Rupees(s.FarePaise)),
		Meta: EntryMeta{Tier: r.Tier, DriverID: r.DriverID, RiderID: r.RiderID, OperatorID: operator, Split: &split, Rates: &rates,
			IncidentID: incident, Interrupted: r.Interrupted, PayoutID: "po_" + r.ID},
		Postings: ps,
	}
	c := Commit{Entry: e}
	if amt := s.PayeePaise(); amt > 0 {
		p := &Payout{ID: "po_" + r.ID, RideID: r.ID, Account: payee.ID(), AccountType: payee.Type, Owner: payee.Owner,
			DriverID: r.DriverID, AmountPaise: amt, Status: PayoutPending, CreatedAt: now}
		if incident != "" {
			p.Status, p.IncidentID, p.HeldAt = PayoutHeld, incident, &now
		}
		c.Payout = p
	}
	return c, nil
}

// ---- payouts ----

func (l *Ledger) schedule(id string) {
	l.mu.Lock()
	closed := l.closed
	if !closed {
		l.wg.Add(1)
	}
	l.mu.Unlock()
	if closed {
		l.sendNow(id)
		return
	}
	go func() {
		defer l.wg.Done()
		t := time.NewTimer(l.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-l.stop:
		}
		l.sendNow(id)
	}()
}

func (l *Ledger) sendNow(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), payoutTimeout)
	defer cancel()
	p, err := l.store.Payout(ctx, id)
	if err != nil {
		l.log.Printf("WARN ledger: payout %s: %v", id, err)
		return
	}
	if p.Status != PayoutPending {
		return // held meanwhile, or already sent
	}
	if _, err := l.send(ctx, p, false); err != nil && !errors.Is(err, ErrPayoutState) {
		l.log.Printf("WARN ledger: send payout %s: %v", id, err)
	}
}

// send moves a pending (or held, on release) payout's amount from the payable
// to payment_clearing and marks it sent, atomically. The amount is capped at
// what the payable holds (a refund may have reversed part of the share).
func (l *Ledger) send(ctx context.Context, p Payout, released bool) (Payout, error) {
	started := time.Now()
	from := p.Status
	for attempt := 0; ; attempt++ {
		signed, err := l.store.Balance(ctx, p.Account)
		if err != nil {
			return Payout{}, err
		}
		amount := min(p.AmountPaise, max(NormalBalance(p.AccountType, signed), 0))
		now := l.now()
		next := p
		next.Status, next.SentAt, next.AmountPaise = PayoutSent, &now, amount
		if released {
			next.ReleasedAt = &now
		}
		c := Commit{Payout: &next, From: from}
		if amount > 0 {
			payable := Acct(p.AccountType, p.Owner)
			e := &Entry{
				ID: newID("je_"), Kind: KindPayout, IdempotencyKey: "payout:" + p.ID, RideID: p.RideID, CreatedAt: now,
				Description: fmt.Sprintf("instant payout %s to %s", Rupees(amount), p.Owner),
				Meta:        EntryMeta{DriverID: p.DriverID, PayoutID: p.ID, IncidentID: p.IncidentID},
				Postings:    []Posting{Post(payable, amount, MemoPayout), Post(Acct(PaymentClearing, GatewayOwner), -amount, MemoPayout)},
			}
			next.EntryID = e.ID
			c.Entry = e
		}
		applied, err := l.store.Apply(ctx, c)
		if retryBalance(ctx, err, attempt) {
			continue
		}
		if err != nil {
			return Payout{}, err
		}
		if applied.Duplicate {
			return *applied.Payout, nil
		}
		out := *applied.Payout
		kind, verb := EvPayoutSent, "sent"
		if released {
			kind, verb = EvPayoutReleased, "released and sent"
		}
		l.publish(kind, p.RideID, p.DriverID,
			fmt.Sprintf("payout %s %s to %s", Rupees(amount), verb, p.Owner),
			fmt.Sprintf("Simulated instant transfer for ride %s: %s → payment_clearing (%s after settlement).",
				short(p.RideID), p.Account, now.Sub(p.CreatedAt).Round(time.Millisecond)), started)
		return out, nil
	}
}

// Hold describes a safety escalation.
type Hold struct {
	IncidentID string
	DriverID   string
	RideID     string // the ride the escalation interrupted ("" = none)
}

// HoldResult reports what a hold did.
type HoldResult struct {
	Settled     bool     `json:"settled"` // the interrupted ride was settled now
	Held        []Payout `json:"held"`
	AlreadySent []string `json:"already_sent,omitempty"` // payouts that had left before the escalation
}

// HoldForIncident applies a safety escalation: the interrupted ride is
// settled (the rider still pays the fare) with its payout held for review,
// every pending payout of the driver is held, and the driver's future rides
// are held until ReleaseIncident.
func (l *Ledger) HoldForIncident(ctx context.Context, h Hold) (HoldResult, error) {
	started := time.Now()
	var res HoldResult
	if h.IncidentID == "" {
		return res, fmt.Errorf("%w: hold needs an incident id", ErrValidation)
	}
	l.mu.Lock()
	if h.DriverID != "" {
		l.holds[h.DriverID] = h.IncidentID
	}
	lookup := l.rides
	l.mu.Unlock()
	if h.RideID != "" {
		if _, err := l.store.EntryByKey(ctx, rideKey(h.RideID)); errors.Is(err, ErrNotFound) && lookup != nil {
			info, err := lookup(ctx, h.RideID)
			if err == nil {
				info.IncidentID, info.Interrupted = h.IncidentID, true
				st, err := l.SettleRide(ctx, info)
				if err != nil {
					return res, err
				}
				res.Settled = !st.Duplicate
				if st.Payout.Status == PayoutHeld {
					res.Held = append(res.Held, st.Payout)
				}
			} else if !errors.Is(err, ErrNotFound) {
				l.log.Printf("WARN ledger: look up interrupted ride %s: %v", h.RideID, err)
			}
		}
	}
	var candidates []Payout
	if h.DriverID != "" {
		ps, err := l.store.Payouts(ctx, PayoutFilter{DriverID: h.DriverID, Status: PayoutPending, Limit: 1000})
		if err != nil {
			return res, err
		}
		candidates = ps
	}
	if h.RideID != "" {
		if p, err := l.store.Payout(ctx, "po_"+h.RideID); err == nil {
			switch p.Status {
			case PayoutSent:
				res.AlreadySent = append(res.AlreadySent, p.ID)
			case PayoutPending:
				if p.DriverID != h.DriverID {
					candidates = append(candidates, p)
				}
			}
		}
	}
	for _, p := range candidates {
		now := l.now()
		next := p
		next.Status, next.IncidentID, next.HeldAt = PayoutHeld, h.IncidentID, &now
		applied, err := l.store.Apply(ctx, Commit{Payout: &next, From: PayoutPending})
		if errors.Is(err, ErrPayoutState) {
			continue // sent meanwhile
		}
		if err != nil {
			return res, err
		}
		res.Held = append(res.Held, *applied.Payout)
		l.publish(EvPayoutHeld, p.RideID, p.DriverID,
			fmt.Sprintf("payout %s held for review (incident %s)", Rupees(p.AmountPaise), h.IncidentID),
			fmt.Sprintf("Safety escalation on driver %s: the payout stays in %s until the incident is resolved.", p.DriverID, p.Account), started)
	}
	return res, nil
}

// ReleaseIncident lifts the holds of a resolved incident: every payout held
// for it is sent, and the driver's future rides pay out normally again.
func (l *Ledger) ReleaseIncident(ctx context.Context, incidentID string) ([]Payout, error) {
	if incidentID == "" {
		return nil, fmt.Errorf("%w: release needs an incident id", ErrValidation)
	}
	l.mu.Lock()
	for d, inc := range l.holds {
		if inc == incidentID {
			delete(l.holds, d)
		}
	}
	l.mu.Unlock()
	held, err := l.store.Payouts(ctx, PayoutFilter{IncidentID: incidentID, Status: PayoutHeld, Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := []Payout{}
	for _, p := range held {
		sent, err := l.send(ctx, p, true)
		if errors.Is(err, ErrPayoutState) {
			continue
		}
		if err != nil {
			return out, err
		}
		out = append(out, sent)
	}
	return out, nil
}

// ---- refunds and top-ups ----

// RefundInput is a refund request.
type RefundInput struct {
	RideID         string `json:"ride_id"`
	AmountPaise    int64  `json:"amount_paise"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// Refund posts a reversal entry crediting the rider's wallet. The ride's
// credit legs are reversed pro rata (floors, remainder from the platform);
// the part of the payee's share already paid out is absorbed by the platform
// so the payable never goes negative. Refunds never exceed what the rider paid.
func (l *Ledger) Refund(ctx context.Context, in RefundInput) (Entry, error) {
	started := time.Now()
	if in.RideID == "" || in.AmountPaise <= 0 {
		return Entry{}, fmt.Errorf("%w: refund needs a ride id and a positive amount", ErrValidation)
	}
	orig, err := l.store.EntryByKey(ctx, rideKey(in.RideID))
	if err != nil {
		return Entry{}, err
	}
	s := orig.Meta.Split
	if s == nil {
		return Entry{}, fmt.Errorf("%w: ride entry %s has no split", ErrValidation, orig.ID)
	}
	prior, err := l.store.Entries(ctx, EntryFilter{RideID: in.RideID, Kind: KindRefund, Limit: 1000})
	if err != nil {
		return Entry{}, err
	}
	key := "refund:" + in.RideID + ":" + strconv.Itoa(len(prior)+1)
	if in.IdempotencyKey != "" {
		key = "refund:" + in.RideID + ":" + in.IdempotencyKey
	}
	if e, err := l.store.EntryByKey(ctx, key); err == nil {
		return e, nil
	}
	var refunded int64
	for _, e := range prior {
		refunded += refundAmount(e)
	}
	if left := s.RiderChargePaise - refunded; in.AmountPaise > left {
		return Entry{}, fmt.Errorf("%w: refund %s exceeds the refundable %s", ErrValidation, Rupees(in.AmountPaise), Rupees(left))
	}
	var payee Account
	for _, p := range orig.Postings {
		if p.Memo == MemoPayeeShare || p.Memo == MemoTip {
			payee = Acct(p.AccountType, p.Owner)
		}
	}
	basis := s.Credits()
	revPayee := in.AmountPaise * s.PayeePaise() / basis
	revLevy := in.AmountPaise * s.CityLevyPaise / basis
	revGST := in.AmountPaise * s.GSTPaise / basis
	rider := Acct(RiderWallet, orig.Meta.RiderID)

	for attempt := 0; ; attempt++ {
		pay := revPayee
		if pay > 0 {
			signed, err := l.store.Balance(ctx, payee.ID())
			if err != nil {
				return Entry{}, err
			}
			pay = min(pay, max(NormalBalance(payee.Type, signed), 0))
		}
		plat := in.AmountPaise - pay - revLevy - revGST
		ps := []Posting{Post(rider, -in.AmountPaise, MemoRefund)}
		for _, leg := range []struct {
			a    Account
			amt  int64
			memo string
		}{{payee, pay, MemoPayeeShare}, {Acct(CityLevyPayable, CityOwner), revLevy, MemoCityLevy},
			{Acct(TaxPayable, TaxOwner), revGST, MemoGST}, {Acct(PlatformRevenue, PlatformOwner), plat, MemoPlatformFee}} {
			if leg.amt != 0 {
				ps = append(ps, Post(leg.a, leg.amt, leg.memo))
			}
		}
		e := &Entry{
			ID: newID("je_"), Kind: KindRefund, IdempotencyKey: key, RideID: in.RideID, ReversesID: orig.ID, CreatedAt: l.now(),
			Description: fmt.Sprintf("refund %s on ride %s", Rupees(in.AmountPaise), in.RideID),
			Meta:        EntryMeta{RiderID: orig.Meta.RiderID, DriverID: orig.Meta.DriverID, Tier: orig.Meta.Tier, Reason: strings.TrimSpace(in.Reason)},
			Postings:    ps,
		}
		applied, err := l.store.Apply(ctx, Commit{Entry: e})
		if retryBalance(ctx, err, attempt) {
			continue
		}
		if err != nil {
			return Entry{}, err
		}
		if !applied.Duplicate {
			l.publish(EvRefund, in.RideID, orig.Meta.DriverID,
				fmt.Sprintf("refund %s on ride %s posted as a reversal entry", Rupees(in.AmountPaise), short(in.RideID)),
				fmt.Sprintf("Reverses %s pro rata (payee %s, levy %s, GST %s, platform %s); reason: %s.",
					orig.ID, Rupees(pay), Rupees(revLevy), Rupees(revGST), Rupees(plat), orDash(e.Meta.Reason)), started)
		}
		return *applied.Entry, nil
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func refundAmount(e Entry) int64 {
	var n int64
	for _, p := range e.Postings {
		if p.Memo == MemoRefund {
			n -= p.AmountPaise
		}
	}
	return n
}

// Topup credits a rider wallet from payment_clearing (test mode only).
func (l *Ledger) Topup(ctx context.Context, owner string, amountPaise int64, key string) (Entry, error) {
	if !l.testMode {
		return Entry{}, fmt.Errorf("%w: wallet top-ups are only available in test mode", ErrForbidden)
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || amountPaise <= 0 || amountPaise > MaxTopupPaise {
		return Entry{}, fmt.Errorf("%w: top-up needs an owner and 1..%d paise", ErrValidation, MaxTopupPaise)
	}
	if key == "" {
		key = newID("")
	}
	e := &Entry{
		ID: newID("je_"), Kind: KindTopup, IdempotencyKey: "topup:" + owner + ":" + key, CreatedAt: l.now(),
		Description: fmt.Sprintf("test top-up %s for %s", Rupees(amountPaise), owner), Meta: EntryMeta{RiderID: owner},
		Postings: []Posting{Post(Acct(PaymentClearing, GatewayOwner), amountPaise, MemoTopup), Post(Acct(RiderWallet, owner), -amountPaise, MemoTopup)},
	}
	applied, err := l.store.Apply(ctx, Commit{Entry: e})
	if err != nil {
		return Entry{}, err
	}
	return *applied.Entry, nil
}

// ---- views ----

// Entries lists journal entries (newest first).
func (l *Ledger) Entries(ctx context.Context, f EntryFilter) ([]Entry, error) {
	return l.store.Entries(ctx, f)
}

// Payouts lists payouts (newest first).
func (l *Ledger) Payouts(ctx context.Context, f PayoutFilter) ([]Payout, error) {
	return l.store.Payouts(ctx, f)
}

// TrialBalance is the sum of every account; it is always zero.
type TrialBalance struct {
	TotalPaise int64       `json:"total_paise"`
	Balanced   bool        `json:"balanced"`
	Types      []TypeTotal `json:"types"`
	Entries    int64       `json:"entries"`
	Postings   int64       `json:"postings"`
	AsOf       time.Time   `json:"as_of"`
}

// TrialBalance sums every account by type.
func (l *Ledger) TrialBalance(ctx context.Context) (TrialBalance, error) {
	types, err := l.store.TypeTotals(ctx)
	if err != nil {
		return TrialBalance{}, err
	}
	n, err := l.store.Counts(ctx)
	if err != nil {
		return TrialBalance{}, err
	}
	tb := TrialBalance{Types: types, Entries: n.Entries, Postings: n.Postings, AsOf: l.now()}
	for _, t := range types {
		tb.TotalPaise += t.SignedPaise
	}
	tb.Balanced = tb.TotalPaise == 0
	return tb, nil
}

// Summary is the Ops "Money" tile.
type Summary struct {
	TrialBalancePaise    int64        `json:"trial_balance_paise"`
	Balanced             bool         `json:"balanced"`
	PayoutsToday         PayoutTotals `json:"payouts_today"` // sent since midnight IST; held/pending at any age
	PlatformRevenuePaise int64        `json:"platform_revenue_paise"`
	Rides                int64        `json:"rides_settled"`
	RidesToday           int64        `json:"rides_settled_today"` // distinct rides settled since midnight IST
	Entries              int64        `json:"entries"`
	Store                string       `json:"store"`
	AsOf                 time.Time    `json:"as_of"`
}

var ist = time.FixedZone("IST", 5*3600+1800)

// Summary returns the Money tile figures.
func (l *Ledger) Summary(ctx context.Context) (Summary, error) {
	tb, err := l.TrialBalance(ctx)
	if err != nil {
		return Summary{}, err
	}
	now := l.now().In(ist)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ist)
	pt, err := l.store.PayoutTotals(ctx, midnight)
	if err != nil {
		return Summary{}, err
	}
	n, err := l.store.Counts(ctx)
	if err != nil {
		return Summary{}, err
	}
	today, err := l.store.RidesSettledSince(ctx, midnight)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{TrialBalancePaise: tb.TotalPaise, Balanced: tb.Balanced, PayoutsToday: pt, Rides: n.Rides, RidesToday: today,
		Entries: n.Entries, Store: l.store.Kind(), AsOf: tb.AsOf}
	for _, t := range tb.Types {
		if t.Type == PlatformRevenue {
			s.PlatformRevenuePaise = t.BalancePaise
		}
	}
	return s, nil
}

// RidesSettledSince counts distinct rides settled at or after since (the Ops counters).
func (l *Ledger) RidesSettledSince(ctx context.Context, since time.Time) (int64, error) {
	return l.store.RidesSettledSince(ctx, since)
}

// Wallet is an owner's accounts and recent payouts.
type Wallet struct {
	Owner        string           `json:"owner"`
	BalancePaise int64            `json:"balance_paise"` // sum of the owner's accounts on their normal side
	Accounts     []AccountBalance `json:"accounts"`
	Payouts      []Payout         `json:"payouts"`
	PaidOutPaise int64            `json:"paid_out_paise"` // across the listed payouts
	HeldPaise    int64            `json:"held_paise"`
}

// Wallet returns an owner's balances (rider wallet, driver or operator payable).
func (l *Ledger) Wallet(ctx context.Context, owner string) (Wallet, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Wallet{}, fmt.Errorf("%w: owner", ErrValidation)
	}
	accts, err := l.store.OwnerBalances(ctx, owner)
	if err != nil {
		return Wallet{}, err
	}
	ps, err := l.store.Payouts(ctx, PayoutFilter{Owner: owner, Limit: 50})
	if err != nil {
		return Wallet{}, err
	}
	w := Wallet{Owner: owner, Accounts: accts, Payouts: ps}
	for _, a := range accts {
		w.BalancePaise += a.BalancePaise
	}
	for _, p := range ps {
		switch p.Status {
		case PayoutSent:
			w.PaidOutPaise += p.AmountPaise
		case PayoutHeld:
			w.HeldPaise += p.AmountPaise
		}
	}
	return w, nil
}

// Breakdown is one ride's fare, role by role.
type Breakdown struct {
	FarePaise        int64        `json:"fare_paise"`
	PromoPaise       int64        `json:"promo_paise"`
	TipPaise         int64        `json:"tip_paise"`
	RiderPaidPaise   int64        `json:"rider_paid_paise"`
	WalletPaise      int64        `json:"wallet_paise"`
	CardPaise        int64        `json:"card_paise"`
	PayeeType        AccountType  `json:"payee_type"`
	PayeeOwner       string       `json:"payee_owner"`
	PayeePaise       int64        `json:"payee_paise"` // share + tip
	CityLevyPaise    int64        `json:"city_levy_paise"`
	GSTPaise         int64        `json:"gst_paise"`
	PlatformFeePaise int64        `json:"platform_fee_paise"`
	RefundedPaise    int64        `json:"refunded_paise"`
	PayoutStatus     PayoutStatus `json:"payout_status,omitempty"`
	Interrupted      bool         `json:"interrupted"`
	IncidentID       string       `json:"incident_id,omitempty"`
}

// RideView is every entry of one ride plus its breakdown.
type RideView struct {
	RideID    string     `json:"ride_id"`
	Entries   []Entry    `json:"entries"`
	Breakdown *Breakdown `json:"breakdown"`
	Payout    *Payout    `json:"payout,omitempty"`
}

// Ride returns a ride's entries and fare breakdown.
func (l *Ledger) Ride(ctx context.Context, rideID string) (RideView, error) {
	es, err := l.store.Entries(ctx, EntryFilter{RideID: rideID, Limit: 1000})
	if err != nil {
		return RideView{}, err
	}
	if len(es) == 0 {
		return RideView{}, fmt.Errorf("%w: no entries for ride %q", ErrNotFound, rideID)
	}
	v := RideView{RideID: rideID, Entries: es}
	if p, err := l.store.Payout(ctx, "po_"+rideID); err == nil {
		v.Payout = &p
	}
	for _, e := range es {
		switch e.Kind {
		case KindRide:
			b := &Breakdown{Interrupted: e.Meta.Interrupted, IncidentID: e.Meta.IncidentID}
			if s := e.Meta.Split; s != nil {
				b.FarePaise, b.PromoPaise, b.TipPaise, b.RiderPaidPaise, b.PayeeType = s.FarePaise, s.PromoPaise, s.TipPaise, s.RiderChargePaise, s.PayeeType
			}
			for _, p := range e.Postings {
				switch p.Memo {
				case MemoWallet:
					b.WalletPaise += p.AmountPaise
				case MemoCard:
					b.CardPaise += p.AmountPaise
				case MemoPayeeShare, MemoTip:
					b.PayeePaise -= p.AmountPaise
					b.PayeeOwner = p.Owner
				case MemoCityLevy:
					b.CityLevyPaise -= p.AmountPaise
				case MemoGST:
					b.GSTPaise -= p.AmountPaise
				case MemoPlatformFee:
					b.PlatformFeePaise -= p.AmountPaise
				}
			}
			if v.Payout != nil {
				b.PayoutStatus = v.Payout.Status
			}
			v.Breakdown = b
		}
	}
	if v.Breakdown != nil {
		for _, e := range es {
			if e.Kind == KindRefund {
				v.Breakdown.RefundedPaise += refundAmount(e)
			}
		}
	}
	return v, nil
}
