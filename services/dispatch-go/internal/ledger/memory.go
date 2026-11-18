package ledger

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Memory is the in-process Store used when DATABASE_URL is absent. One mutex
// makes every Apply atomic and serialisable.
type Memory struct {
	mu       sync.RWMutex
	entries  []Entry          // seq order
	byKey    map[string]int   // idempotency key -> index
	balances map[string]int64 // account id -> signed balance
	accounts map[string]Account
	payouts  map[string]Payout
	postings int64
	rides    int64
}

// NewMemory returns an empty in-memory ledger store.
func NewMemory() *Memory {
	return &Memory{byKey: map[string]int{}, balances: map[string]int64{}, accounts: map[string]Account{}, payouts: map[string]Payout{}}
}

// Kind implements Store.
func (m *Memory) Kind() string { return "memory" }

// Apply implements Store.
func (m *Memory) Apply(_ context.Context, c Commit) (Applied, error) {
	if err := checkCommit(c); err != nil {
		return Applied{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if c.Entry != nil {
		if i, ok := m.byKey[c.Entry.IdempotencyKey]; ok {
			e := m.entries[i]
			out := Applied{Entry: &e, Duplicate: true}
			if c.Payout != nil {
				if p, ok := m.payouts[c.Payout.ID]; ok {
					out.Payout = &p
				}
			}
			return out, nil
		}
	}
	if p := c.Payout; p != nil {
		cur, exists := m.payouts[p.ID]
		switch {
		case c.From == "" && exists:
			return Applied{Payout: &cur, Duplicate: true}, nil
		case c.From != "" && (!exists || cur.Status != c.From):
			return Applied{}, fmt.Errorf("%w: payout %s is %q, want %q", ErrPayoutState, p.ID, cur.Status, c.From)
		}
	}

	var out Applied
	if c.Entry != nil {
		e := *c.Entry
		next := map[string]int64{}
		for _, p := range e.Postings {
			if _, ok := next[p.Account]; !ok {
				next[p.Account] = m.balances[p.Account]
			}
			next[p.Account] += p.AmountPaise
		}
		for id, bal := range next {
			a, _ := ParseAccount(id) // validated by checkCommit
			if a.Type.Guarded() && NormalBalance(a.Type, bal) < 0 {
				return Applied{}, fmt.Errorf("%w: %s would be %d paise", ErrNegativeBalance, id, NormalBalance(a.Type, bal))
			}
		}
		for id, bal := range next {
			m.balances[id] = bal
			if _, ok := m.accounts[id]; !ok {
				m.accounts[id], _ = ParseAccount(id)
			}
		}
		e.Seq = int64(len(m.entries) + 1)
		e.Postings = append([]Posting(nil), e.Postings...)
		m.byKey[e.IdempotencyKey] = len(m.entries)
		m.entries = append(m.entries, e)
		m.postings += int64(len(e.Postings))
		if e.Kind == KindRide {
			m.rides++
		}
		out.Entry = &e
	}
	if c.Payout != nil {
		p := *c.Payout
		m.payouts[p.ID] = p
		out.Payout = &p
	}
	return out, nil
}

// EntryByKey implements Store.
func (m *Memory) EntryByKey(_ context.Context, key string) (Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.byKey[key]
	if !ok {
		return Entry{}, fmt.Errorf("%w: entry %q", ErrNotFound, key)
	}
	return m.entries[i], nil
}

// Entries implements Store.
func (m *Memory) Entries(_ context.Context, f EntryFilter) ([]Entry, error) {
	limit := clampLimit(f.Limit)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Entry{}
	for i := len(m.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := m.entries[i]
		if (f.RideID != "" && e.RideID != f.RideID) || (f.Kind != "" && e.Kind != f.Kind) ||
			(!f.Since.IsZero() && e.CreatedAt.Before(f.Since)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Balance implements Store.
func (m *Memory) Balance(_ context.Context, account string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.balances[account], nil
}

// OwnerBalances implements Store.
func (m *Memory) OwnerBalances(_ context.Context, owner string) ([]AccountBalance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []AccountBalance{}
	for id, a := range m.accounts {
		if a.Owner == owner {
			s := m.balances[id]
			out = append(out, AccountBalance{Account: id, Type: a.Type, Owner: owner, SignedPaise: s, BalancePaise: NormalBalance(a.Type, s)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

// TypeTotals implements Store.
func (m *Memory) TypeTotals(context.Context) ([]TypeTotal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byType := map[AccountType]*TypeTotal{}
	for id, a := range m.accounts {
		t := byType[a.Type]
		if t == nil {
			t = &TypeTotal{Type: a.Type}
			byType[a.Type] = t
		}
		t.Accounts++
		t.SignedPaise += m.balances[id]
	}
	return finishTotals(byType), nil
}

func finishTotals(byType map[AccountType]*TypeTotal) []TypeTotal {
	out := make([]TypeTotal, 0, len(accountTypes))
	for _, at := range accountTypes {
		t := TypeTotal{Type: at}
		if v := byType[at]; v != nil {
			t = *v
		}
		t.BalancePaise = NormalBalance(at, t.SignedPaise)
		out = append(out, t)
	}
	return out
}

// Counts implements Store.
func (m *Memory) Counts(context.Context) (Counts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Counts{Entries: int64(len(m.entries)), Postings: m.postings, Rides: m.rides}, nil
}

// Payout implements Store.
func (m *Memory) Payout(_ context.Context, id string) (Payout, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.payouts[id]
	if !ok {
		return Payout{}, fmt.Errorf("%w: payout %q", ErrNotFound, id)
	}
	return p, nil
}

// Payouts implements Store.
func (m *Memory) Payouts(_ context.Context, f PayoutFilter) ([]Payout, error) {
	limit := clampLimit(f.Limit)
	m.mu.RLock()
	all := make([]Payout, 0, len(m.payouts))
	for _, p := range m.payouts {
		if (f.Status != "" && p.Status != f.Status) || (f.IncidentID != "" && p.IncidentID != f.IncidentID) ||
			(f.Owner != "" && p.Owner != f.Owner) || (f.DriverID != "" && p.DriverID != f.DriverID) || (f.RideID != "" && p.RideID != f.RideID) ||
			(!f.Since.IsZero() && p.CreatedAt.Before(f.Since)) {
			continue
		}
		all = append(all, p)
	}
	m.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// PayoutTotals implements Store.
func (m *Memory) PayoutTotals(_ context.Context, since time.Time) (PayoutTotals, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var t PayoutTotals
	for _, p := range m.payouts {
		switch p.Status {
		case PayoutHeld:
			t.HeldCount++
			t.HeldPaise += p.AmountPaise
		case PayoutPending:
			t.PendingCount++
		case PayoutSent:
			if p.SentAt != nil && !p.SentAt.Before(since) {
				t.SentCount++
				t.SentPaise += p.AmountPaise
			}
		}
	}
	return t, nil
}
