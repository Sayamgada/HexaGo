package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func entry(key string, ps ...Posting) *Entry {
	return &Entry{ID: newID("je_"), Kind: KindTopup, IdempotencyKey: key, CreatedAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), Postings: ps}
}

// testStoreContract pins Store semantics every implementation must share.
func testStoreContract(t *testing.T, mk storeMaker) {
	s := mk(t)
	ctx := context.Background()
	wallet, clearing := Acct(RiderWallet, "r1"), Acct(PaymentClearing, GatewayOwner)

	if _, err := s.Apply(ctx, Commit{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty commit: %v", err)
	}
	for _, bad := range []*Entry{
		entry("k", Post(wallet, -5, "x")),                         // one leg
		entry("k", Post(wallet, -5, "x"), Post(clearing, 4, "x")), // unbalanced
		entry("k", Post(wallet, 0, "x"), Post(clearing, 0, "x")),  // zero legs
		entry("", Post(wallet, -5, "x"), Post(clearing, 5, "x")),  // no key
		entry("k", Posting{Account: "bogus:x", AccountType: "bogus", Owner: "x", AmountPaise: 5}, Post(clearing, -5, "x")),
		entry("k", Posting{Account: wallet.ID(), AccountType: DriverPayable, Owner: "r1", AmountPaise: 5}, Post(clearing, -5, "x")),
		entry("k", Post(wallet, -MaxAmountPaise-1, "x"), Post(clearing, MaxAmountPaise+1, "x")),
	} {
		if _, err := s.Apply(ctx, Commit{Entry: bad}); !errors.Is(err, ErrValidation) && !errors.Is(err, ErrUnbalanced) {
			t.Fatalf("bad entry %+v: %v", bad.Postings, err)
		}
	}
	if _, err := s.Apply(ctx, Commit{Payout: &Payout{}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad payout: %v", err)
	}

	// Guarded accounts never go negative; the whole commit is rejected.
	if _, err := s.Apply(ctx, Commit{Entry: entry("neg", Post(wallet, 100, "x"), Post(clearing, -100, "x"))}); !errors.Is(err, ErrNegativeBalance) {
		t.Fatalf("negative wallet: %v", err)
	}
	if b, _ := s.Balance(ctx, wallet.ID()); b != 0 {
		t.Fatalf("rejected commit left balance %d", b)
	}
	if _, err := s.EntryByKey(ctx, "neg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected commit left entry: %v", err)
	}

	a, err := s.Apply(ctx, Commit{Entry: entry("t1", Post(clearing, 500, MemoTopup), Post(wallet, -500, MemoTopup))})
	if err != nil || a.Duplicate || a.Entry.Seq == 0 {
		t.Fatalf("topup %+v %v", a, err)
	}
	dup, err := s.Apply(ctx, Commit{Entry: entry("t1", Post(clearing, 999, MemoTopup), Post(wallet, -999, MemoTopup))})
	if err != nil || !dup.Duplicate || dup.Entry.ID != a.Entry.ID || dup.Entry.Postings[0].AmountPaise != 500 {
		t.Fatalf("duplicate %+v %v", dup, err)
	}
	if b, _ := s.Balance(ctx, wallet.ID()); NormalBalance(RiderWallet, b) != 500 {
		t.Fatalf("wallet %d", b)
	}
	if b, _ := s.Balance(ctx, "rider_wallet:nobody"); b != 0 {
		t.Fatalf("unknown account %d", b)
	}

	// Payout lifecycle: insert, duplicate insert, guarded transition.
	now := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	po := Payout{ID: "po_x", RideID: "x", Account: wallet.ID(), AccountType: RiderWallet, Owner: "r1", DriverID: "d1",
		AmountPaise: 100, Status: PayoutPending, CreatedAt: now}
	if _, err := s.Apply(ctx, Commit{Payout: &po}); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Apply(ctx, Commit{Payout: &po}); err != nil || !d.Duplicate {
		t.Fatalf("dup payout %+v %v", d, err)
	}
	held := po
	held.Status, held.IncidentID, held.HeldAt = PayoutHeld, "inc", &now
	if _, err := s.Apply(ctx, Commit{Payout: &held, From: PayoutSent}); !errors.Is(err, ErrPayoutState) {
		t.Fatalf("wrong from: %v", err)
	}
	if _, err := s.Apply(ctx, Commit{Payout: &held, From: PayoutPending}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Payout(ctx, "po_x")
	if err != nil || got.Status != PayoutHeld || got.IncidentID != "inc" || got.HeldAt == nil || !got.HeldAt.Equal(now) || got.Owner != "r1" {
		t.Fatalf("payout %+v %v", got, err)
	}
	if _, err := s.Payout(ctx, "po_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing payout: %v", err)
	}
	for _, f := range []PayoutFilter{{Status: PayoutHeld}, {IncidentID: "inc"}, {Owner: "r1"}, {DriverID: "d1"}, {RideID: "x"}, {Since: now}} {
		if ps, err := s.Payouts(ctx, f); err != nil || len(ps) != 1 {
			t.Fatalf("filter %+v: %d %v", f, len(ps), err)
		}
	}
	if ps, _ := s.Payouts(ctx, PayoutFilter{Status: PayoutSent}); len(ps) != 0 {
		t.Fatal("sent filter")
	}
	if ps, _ := s.Payouts(ctx, PayoutFilter{Since: now.Add(time.Hour)}); len(ps) != 0 {
		t.Fatal("since filter")
	}

	// A commit whose payout transition fails writes no entry either.
	sent := held
	sent.Status = PayoutSent
	e := entry("payout:po_x", Post(clearing, 1, "x"), Post(wallet, -1, "x"))
	if _, err := s.Apply(ctx, Commit{Entry: e, Payout: &sent, From: PayoutPending}); !errors.Is(err, ErrPayoutState) {
		t.Fatalf("mixed commit: %v", err)
	}
	if _, err := s.EntryByKey(ctx, "payout:po_x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("entry of a failed commit was written")
	}

	es, err := s.Entries(ctx, EntryFilter{Kind: KindTopup})
	if err != nil || len(es) != 1 || len(es[0].Postings) != 2 {
		t.Fatalf("entries %+v %v", es, err)
	}
	if es, _ := s.Entries(ctx, EntryFilter{Since: now.Add(48 * time.Hour)}); len(es) != 0 {
		t.Fatal("since filter on entries")
	}
	if es, _ := s.Entries(ctx, EntryFilter{RideID: "none"}); len(es) != 0 {
		t.Fatal("ride filter on entries")
	}
	bs, err := s.OwnerBalances(ctx, "r1")
	if err != nil || len(bs) != 1 || bs[0].BalancePaise != 500 || bs[0].SignedPaise != -500 {
		t.Fatalf("owner balances %+v %v", bs, err)
	}
	tt, err := s.TypeTotals(ctx)
	if err != nil || len(tt) != len(AccountTypes()) {
		t.Fatalf("type totals %+v %v", tt, err)
	}
	var sum int64
	for _, x := range tt {
		sum += x.SignedPaise
	}
	if sum != 0 {
		t.Fatalf("type totals sum %d", sum)
	}
	n, err := s.Counts(ctx)
	if err != nil || n.Entries != 1 || n.Postings != 2 || n.Rides != 0 {
		t.Fatalf("counts %+v %v", n, err)
	}
	pt, err := s.PayoutTotals(ctx, time.Time{})
	if err != nil || pt.HeldCount != 1 || pt.HeldPaise != 100 {
		t.Fatalf("payout totals %+v %v", pt, err)
	}
	if clampLimit(5000) != 1000 || clampLimit(-1) != 100 || clampLimit(7) != 7 {
		t.Fatal("clampLimit")
	}
	if s.Kind() == "" {
		t.Fatal("kind")
	}
}

// testDatabaseInvariants: Postgres refuses edits, unbalanced entries and
// negative guarded balances even when the Go checks are bypassed.
func testDatabaseInvariants(t *testing.T) {
	pool := pgPool(t)
	s := NewPostgres(pool)
	ctx := context.Background()
	if _, err := s.Apply(ctx, Commit{Entry: entry("t", Post(Acct(PaymentClearing, GatewayOwner), 500, "x"), Post(Acct(RiderWallet, "r"), -500, "x"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE postings SET amount_paise = 1`); err == nil {
		t.Fatal("postings UPDATE allowed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM journal_entries`); err == nil {
		t.Fatal("journal_entries DELETE allowed")
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, kind, idempotency_key, created_at) VALUES ('bad', 'topup', 'bad', now())`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO postings (entry_id, account_id, amount_paise) VALUES ('bad', 'rider_wallet:r', -7)`)
		return err
	})
	if !errors.Is(mapPgError(err), ErrUnbalanced) {
		t.Fatalf("unbalanced raw entry: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE accounts SET balance_paise = 1 WHERE id = 'rider_wallet:r'`)
	if !errors.Is(mapPgError(err), ErrNegativeBalance) {
		t.Fatalf("negative guarded balance: %v", err)
	}
	if mapPgError(errors.New("x")).Error() != "x" {
		t.Fatal("mapPgError passthrough")
	}
}
