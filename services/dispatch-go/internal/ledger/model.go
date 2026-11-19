package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// EntryKind classifies a journal entry.
type EntryKind string

// Entry kinds.
const (
	KindRide   EntryKind = "ride_settlement"
	KindPayout EntryKind = "payout"
	KindRefund EntryKind = "refund"
	KindTopup  EntryKind = "topup"
)

// Posting memos (the role of a leg inside an entry).
const (
	MemoRiderCharge = "rider_charge"
	MemoWallet      = "rider_wallet"
	MemoCard        = "card"
	MemoPromo       = "promo"
	MemoPayeeShare  = "payee_share"
	MemoTip         = "tip"
	MemoCityLevy    = "city_levy"
	MemoGST         = "gst"
	MemoPlatformFee = "platform_fee"
	MemoPayout      = "payout"
	MemoRefund      = "refund"
	MemoTopup       = "topup"
)

// Posting is one leg of an entry. AmountPaise > 0 is a debit, < 0 a credit.
type Posting struct {
	Account     string      `json:"account"`
	AccountType AccountType `json:"account_type"`
	Owner       string      `json:"owner"`
	AmountPaise int64       `json:"amount_paise"`
	Memo        string      `json:"memo"`
}

// Post builds a posting on a.
func Post(a Account, amount int64, memo string) Posting {
	return Posting{Account: a.ID(), AccountType: a.Type, Owner: a.Owner, AmountPaise: amount, Memo: memo}
}

// Entry is one immutable, balanced journal entry.
type Entry struct {
	ID             string    `json:"id"`
	Seq            int64     `json:"seq"`
	Kind           EntryKind `json:"kind"`
	IdempotencyKey string    `json:"idempotency_key"`
	RideID         string    `json:"ride_id,omitempty"`
	ReversesID     string    `json:"reverses_id,omitempty"` // refunds: the ride entry being reversed
	Description    string    `json:"description"`
	Meta           EntryMeta `json:"meta"`
	Postings       []Posting `json:"postings"`
	CreatedAt      time.Time `json:"created_at"`
}

// EntryMeta is the structured context stored with an entry.
type EntryMeta struct {
	Tier        string `json:"tier,omitempty"`
	DriverID    string `json:"driver_id,omitempty"`
	RiderID     string `json:"rider_id,omitempty"`
	OperatorID  string `json:"operator_id,omitempty"`
	Split       *Split `json:"split,omitempty"`  // ride settlements
	Rates       *Rates `json:"rates,omitempty"`  // rates the split used
	Reason      string `json:"reason,omitempty"` // refunds, holds
	PayoutID    string `json:"payout_id,omitempty"`
	IncidentID  string `json:"incident_id,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"` // the ride ended in a safety escalation
}

// Validate checks the entry's invariants: at least two postings on valid
// accounts, every amount within bounds and non-zero, and a zero sum.
func (e Entry) Validate() error {
	if e.IdempotencyKey == "" {
		return fmt.Errorf("%w: entry needs an idempotency key", ErrValidation)
	}
	if len(e.Postings) < 2 {
		return fmt.Errorf("%w: entry needs at least two postings", ErrUnbalanced)
	}
	var sum int64
	for _, p := range e.Postings {
		a, err := ParseAccount(p.Account)
		if err != nil {
			return err
		}
		if a.Type != p.AccountType || a.Owner != p.Owner {
			return fmt.Errorf("%w: posting account %s mismatches type/owner", ErrValidation, p.Account)
		}
		if p.AmountPaise == 0 || p.AmountPaise > MaxAmountPaise || p.AmountPaise < -MaxAmountPaise {
			return fmt.Errorf("%w: posting amount %d on %s", ErrValidation, p.AmountPaise, p.Account)
		}
		sum += p.AmountPaise
	}
	if sum != 0 {
		return fmt.Errorf("%w: postings sum to %d paise", ErrUnbalanced, sum)
	}
	return nil
}

// PayoutStatus is a payout's state.
type PayoutStatus string

// Payout statuses.
const (
	PayoutPending PayoutStatus = "pending"         // queued, sent within PayoutDelay
	PayoutSent    PayoutStatus = "sent"            // posted payable → payment_clearing
	PayoutHeld    PayoutStatus = "held_for_review" // ride ended in a safety escalation
)

// Payout moves a ride's earnings from a payable to the payment gateway.
type Payout struct {
	ID          string       `json:"id"`
	RideID      string       `json:"ride_id"`
	Account     string       `json:"account"`
	AccountType AccountType  `json:"account_type"`
	Owner       string       `json:"owner"`
	AmountPaise int64        `json:"amount_paise"`
	Status      PayoutStatus `json:"status"`
	IncidentID  string       `json:"incident_id,omitempty"`
	DriverID    string       `json:"driver_id"`
	EntryID     string       `json:"entry_id,omitempty"` // the payout entry, once sent
	CreatedAt   time.Time    `json:"created_at"`
	HeldAt      *time.Time   `json:"held_at,omitempty"`
	ReleasedAt  *time.Time   `json:"released_at,omitempty"`
	SentAt      *time.Time   `json:"sent_at,omitempty"`
}

// Commit is one atomic store write: an entry, a payout change, or both.
type Commit struct {
	Entry *Entry
	// Payout is inserted when From is "" (a no-op duplicate if its id exists)
	// or replaces the stored payout only if that one is in status From.
	Payout *Payout
	From   PayoutStatus
}

// Applied reports what a Commit did.
type Applied struct {
	Entry     *Entry
	Payout    *Payout
	Duplicate bool // the entry's idempotency key (or the payout id) already existed: nothing was written
}

// EntryFilter selects entries (newest first).
type EntryFilter struct {
	RideID string
	Kind   EntryKind
	Since  time.Time
	Limit  int // 0 = 100, max 1000
}

// PayoutFilter selects payouts (newest first).
type PayoutFilter struct {
	Status     PayoutStatus
	IncidentID string
	Owner      string
	DriverID   string
	RideID     string
	Since      time.Time
	Limit      int // 0 = 100, max 1000
}

// AccountBalance is an account and its balance.
type AccountBalance struct {
	Account      string      `json:"account"`
	Type         AccountType `json:"type"`
	Owner        string      `json:"owner"`
	SignedPaise  int64       `json:"signed_paise"`  // debit-positive sum of postings
	BalancePaise int64       `json:"balance_paise"` // on the account's normal side
}

// TypeTotal sums every account of one type.
type TypeTotal struct {
	Type         AccountType `json:"type"`
	Accounts     int         `json:"accounts"`
	SignedPaise  int64       `json:"signed_paise"`
	BalancePaise int64       `json:"balance_paise"`
}

// PayoutTotals aggregates payouts since a point in time.
type PayoutTotals struct {
	SentCount    int   `json:"sent_count"`
	SentPaise    int64 `json:"sent_paise"`
	HeldCount    int   `json:"held_count"` // currently held (any age)
	HeldPaise    int64 `json:"held_paise"`
	PendingCount int   `json:"pending_count"`
}

// Store persists the ledger. Apply is atomic: either everything in the
// commit is written (entry, postings, balance updates, payout) or nothing.
type Store interface {
	Kind() string
	Apply(ctx context.Context, c Commit) (Applied, error)
	EntryByKey(ctx context.Context, key string) (Entry, error)
	Entries(ctx context.Context, f EntryFilter) ([]Entry, error)
	Balance(ctx context.Context, account string) (int64, error) // signed; 0 for unknown accounts
	OwnerBalances(ctx context.Context, owner string) ([]AccountBalance, error)
	TypeTotals(ctx context.Context) ([]TypeTotal, error)
	Counts(ctx context.Context) (Counts, error)
	Payout(ctx context.Context, id string) (Payout, error)
	Payouts(ctx context.Context, f PayoutFilter) ([]Payout, error)
	PayoutTotals(ctx context.Context, since time.Time) (PayoutTotals, error)
	// RidesSettledSince counts distinct rides with a ride_settlement entry created at or after since.
	RidesSettledSince(ctx context.Context, since time.Time) (int64, error)
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 100
	case n > 1000:
		return 1000
	}
	return n
}

func newID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// checkCommit validates a commit before any store touches it.
func checkCommit(c Commit) error {
	if c.Entry == nil && c.Payout == nil {
		return fmt.Errorf("%w: empty commit", ErrValidation)
	}
	if c.Entry != nil {
		if err := c.Entry.Validate(); err != nil {
			return err
		}
	}
	if p := c.Payout; p != nil {
		if p.ID == "" || p.AmountPaise < 0 || !p.AccountType.Valid() {
			return fmt.Errorf("%w: payout %+v", ErrValidation, *p)
		}
	}
	return nil
}

// Counts sizes the ledger.
type Counts struct {
	Entries  int64 `json:"entries"`
	Postings int64 `json:"postings"`
	Rides    int64 `json:"rides"` // ride settlement entries
}
