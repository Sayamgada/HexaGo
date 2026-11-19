package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the production Store (tables from migration 0200_ledger_core.sql,
// applied by store.OpenPostgres). Each Apply is one transaction: accounts are
// locked in id order (no deadlocks), the guarded-balance CHECK and the
// deferred balanced-entry trigger back up the checks made in Go.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Kind implements Store.
func (p *Postgres) Kind() string { return "postgres" }

var errDuplicate = errors.New("ledger: duplicate")

// Apply implements Store.
func (p *Postgres) Apply(ctx context.Context, c Commit) (Applied, error) {
	if err := checkCommit(c); err != nil {
		return Applied{}, err
	}
	var out Applied
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if e := c.Entry; e != nil {
			meta, err := json.Marshal(e.Meta)
			if err != nil {
				return err
			}
			var seq int64
			err = tx.QueryRow(ctx, `
				INSERT INTO journal_entries (id, kind, idempotency_key, ride_id, reverses_id, description, meta, created_at)
				VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8)
				ON CONFLICT (idempotency_key) DO NOTHING RETURNING seq`,
				e.ID, string(e.Kind), e.IdempotencyKey, e.RideID, e.ReversesID, e.Description, meta, e.CreatedAt).Scan(&seq)
			if errors.Is(err, pgx.ErrNoRows) {
				return errDuplicate
			}
			if err != nil {
				return err
			}
			deltas := map[string]int64{}
			types := map[string]Account{}
			for _, ps := range e.Postings {
				deltas[ps.Account] += ps.AmountPaise
				types[ps.Account] = Acct(ps.AccountType, ps.Owner)
			}
			ids := make([]string, 0, len(deltas))
			for id := range deltas {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				a := types[id]
				if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, type, owner, guarded) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
					id, string(a.Type), a.Owner, a.Type.Guarded()); err != nil {
					return err
				}
			}
			for _, id := range ids {
				if deltas[id] == 0 {
					continue
				}
				if _, err := tx.Exec(ctx, `UPDATE accounts SET balance_paise = balance_paise + $2, updated_at = now() WHERE id = $1`,
					id, deltas[id]); err != nil {
					return err
				}
			}
			b := &pgx.Batch{}
			for _, ps := range e.Postings {
				b.Queue(`INSERT INTO postings (entry_id, account_id, amount_paise, memo) VALUES ($1, $2, $3, $4)`,
					e.ID, ps.Account, ps.AmountPaise, ps.Memo)
			}
			if err := tx.SendBatch(ctx, b).Close(); err != nil {
				return err
			}
			cp := *e
			cp.Seq = seq
			cp.Postings = append([]Posting(nil), e.Postings...)
			out.Entry = &cp
		}
		if po := c.Payout; po != nil {
			var tag pgconn.CommandTag
			var err error
			if c.From == "" {
				tag, err = tx.Exec(ctx, `
					INSERT INTO payouts (id, ride_id, account_id, driver_id, amount_paise, status, incident_id, entry_id,
						created_at, held_at, released_at, sent_at)
					VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), $9, $10, $11, $12)
					ON CONFLICT (id) DO NOTHING`,
					po.ID, po.RideID, po.Account, po.DriverID, po.AmountPaise, string(po.Status), po.IncidentID, po.EntryID,
					po.CreatedAt, po.HeldAt, po.ReleasedAt, po.SentAt)
				if err == nil && tag.RowsAffected() == 0 {
					return errDuplicate
				}
			} else {
				tag, err = tx.Exec(ctx, `
					UPDATE payouts SET amount_paise = $2, status = $3, incident_id = NULLIF($4, ''), entry_id = NULLIF($5, ''),
						held_at = $6, released_at = $7, sent_at = $8
					WHERE id = $1 AND status = $9`,
					po.ID, po.AmountPaise, string(po.Status), po.IncidentID, po.EntryID, po.HeldAt, po.ReleasedAt, po.SentAt, string(c.From))
				if err == nil && tag.RowsAffected() == 0 {
					return fmt.Errorf("%w: payout %s is not %q", ErrPayoutState, po.ID, c.From)
				}
			}
			if err != nil {
				return err
			}
			cp := *po
			out.Payout = &cp
		}
		return nil
	})
	switch {
	case errors.Is(err, errDuplicate):
		dup := Applied{Duplicate: true}
		if c.Entry != nil {
			if e, err := p.EntryByKey(ctx, c.Entry.IdempotencyKey); err == nil {
				dup.Entry = &e
			}
		}
		if c.Payout != nil {
			if po, err := p.Payout(ctx, c.Payout.ID); err == nil {
				dup.Payout = &po
			}
		}
		return dup, nil
	case err != nil:
		return Applied{}, mapPgError(err)
	}
	return out, nil
}

func mapPgError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23514" { // check_violation
		if pe.ConstraintName == "accounts_guarded_non_negative" {
			return fmt.Errorf("%w: %s", ErrNegativeBalance, pe.Message)
		}
		return fmt.Errorf("%w: %s", ErrUnbalanced, pe.Message)
	}
	return err
}

const entryCols = `id, seq, kind, idempotency_key, COALESCE(ride_id, ''), COALESCE(reverses_id, ''), description, meta, created_at`

func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	var kind string
	var meta []byte
	if err := row.Scan(&e.ID, &e.Seq, &kind, &e.IdempotencyKey, &e.RideID, &e.ReversesID, &e.Description, &meta, &e.CreatedAt); err != nil {
		return Entry{}, err
	}
	e.Kind = EntryKind(kind)
	e.CreatedAt = e.CreatedAt.UTC()
	if err := json.Unmarshal(meta, &e.Meta); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (p *Postgres) attachPostings(ctx context.Context, es []Entry) error {
	if len(es) == 0 {
		return nil
	}
	ids := make([]string, len(es))
	idx := make(map[string]int, len(es))
	for i, e := range es {
		ids[i] = e.ID
		idx[e.ID] = i
		es[i].Postings = []Posting{}
	}
	rows, err := p.pool.Query(ctx, `
		SELECT p.entry_id, p.account_id, a.type, a.owner, p.amount_paise, p.memo
		FROM postings p JOIN accounts a ON a.id = p.account_id
		WHERE p.entry_id = ANY($1) ORDER BY p.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var entryID, typ string
		var ps Posting
		if err := rows.Scan(&entryID, &ps.Account, &typ, &ps.Owner, &ps.AmountPaise, &ps.Memo); err != nil {
			return err
		}
		ps.AccountType = AccountType(typ)
		i := idx[entryID]
		es[i].Postings = append(es[i].Postings, ps)
	}
	return rows.Err()
}

// EntryByKey implements Store.
func (p *Postgres) EntryByKey(ctx context.Context, key string) (Entry, error) {
	e, err := scanEntry(p.pool.QueryRow(ctx, `SELECT `+entryCols+` FROM journal_entries WHERE idempotency_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: entry %q", ErrNotFound, key)
	}
	if err != nil {
		return Entry{}, err
	}
	es := []Entry{e}
	if err := p.attachPostings(ctx, es); err != nil {
		return Entry{}, err
	}
	return es[0], nil
}

// Entries implements Store.
func (p *Postgres) Entries(ctx context.Context, f EntryFilter) ([]Entry, error) {
	var where []string
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if f.RideID != "" {
		where = append(where, "ride_id = "+arg(f.RideID))
	}
	if f.Kind != "" {
		where = append(where, "kind = "+arg(string(f.Kind)))
	}
	if !f.Since.IsZero() {
		where = append(where, "created_at >= "+arg(f.Since))
	}
	q := `SELECT ` + entryCols + ` FROM journal_entries`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY seq DESC LIMIT " + arg(clampLimit(f.Limit))
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, p.attachPostings(ctx, out)
}

// Balance implements Store.
func (p *Postgres) Balance(ctx context.Context, account string) (int64, error) {
	var b int64
	err := p.pool.QueryRow(ctx, `SELECT balance_paise FROM accounts WHERE id = $1`, account).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return b, err
}

// OwnerBalances implements Store.
func (p *Postgres) OwnerBalances(ctx context.Context, owner string) ([]AccountBalance, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, type, balance_paise FROM accounts WHERE owner = $1 ORDER BY id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountBalance{}
	for rows.Next() {
		var b AccountBalance
		var typ string
		if err := rows.Scan(&b.Account, &typ, &b.SignedPaise); err != nil {
			return nil, err
		}
		b.Type, b.Owner = AccountType(typ), owner
		b.BalancePaise = NormalBalance(b.Type, b.SignedPaise)
		out = append(out, b)
	}
	return out, rows.Err()
}

// TypeTotals implements Store.
func (p *Postgres) TypeTotals(ctx context.Context) ([]TypeTotal, error) {
	rows, err := p.pool.Query(ctx, `SELECT type, count(*), COALESCE(sum(balance_paise), 0)::BIGINT FROM accounts GROUP BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byType := map[AccountType]*TypeTotal{}
	for rows.Next() {
		var typ string
		t := &TypeTotal{}
		if err := rows.Scan(&typ, &t.Accounts, &t.SignedPaise); err != nil {
			return nil, err
		}
		t.Type = AccountType(typ)
		byType[t.Type] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return finishTotals(byType), nil
}

// Counts implements Store.
func (p *Postgres) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := p.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries), (SELECT count(*) FROM postings),
		(SELECT count(*) FROM journal_entries WHERE kind = 'ride_settlement')`).Scan(&c.Entries, &c.Postings, &c.Rides)
	return c, err
}

const payoutCols = `p.id, p.ride_id, p.account_id, a.type, a.owner, p.driver_id, p.amount_paise, p.status,
	COALESCE(p.incident_id, ''), COALESCE(p.entry_id, ''), p.created_at, p.held_at, p.released_at, p.sent_at`

func scanPayout(row pgx.Row) (Payout, error) {
	var po Payout
	var typ, status string
	if err := row.Scan(&po.ID, &po.RideID, &po.Account, &typ, &po.Owner, &po.DriverID, &po.AmountPaise, &status,
		&po.IncidentID, &po.EntryID, &po.CreatedAt, &po.HeldAt, &po.ReleasedAt, &po.SentAt); err != nil {
		return Payout{}, err
	}
	po.AccountType, po.Status = AccountType(typ), PayoutStatus(status)
	po.CreatedAt = po.CreatedAt.UTC()
	for _, t := range []*time.Time{po.HeldAt, po.ReleasedAt, po.SentAt} {
		if t != nil {
			*t = t.UTC()
		}
	}
	return po, nil
}

// Payout implements Store.
func (p *Postgres) Payout(ctx context.Context, id string) (Payout, error) {
	po, err := scanPayout(p.pool.QueryRow(ctx, `SELECT `+payoutCols+` FROM payouts p JOIN accounts a ON a.id = p.account_id WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payout{}, fmt.Errorf("%w: payout %q", ErrNotFound, id)
	}
	return po, err
}

// Payouts implements Store.
func (p *Postgres) Payouts(ctx context.Context, f PayoutFilter) ([]Payout, error) {
	var where []string
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	for col, v := range map[string]string{"p.status": string(f.Status), "p.incident_id": f.IncidentID, "a.owner": f.Owner,
		"p.driver_id": f.DriverID, "p.ride_id": f.RideID} {
		if v != "" {
			where = append(where, col+" = "+arg(v))
		}
	}
	if !f.Since.IsZero() {
		where = append(where, "p.created_at >= "+arg(f.Since))
	}
	q := `SELECT ` + payoutCols + ` FROM payouts p JOIN accounts a ON a.id = p.account_id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY p.created_at DESC, p.id DESC LIMIT " + arg(clampLimit(f.Limit))
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payout{}
	for rows.Next() {
		po, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, po)
	}
	return out, rows.Err()
}

// PayoutTotals implements Store.
func (p *Postgres) PayoutTotals(ctx context.Context, since time.Time) (PayoutTotals, error) {
	var t PayoutTotals
	err := p.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status = 'sent' AND sent_at >= $1),
		COALESCE(sum(amount_paise) FILTER (WHERE status = 'sent' AND sent_at >= $1), 0)::BIGINT,
		count(*) FILTER (WHERE status = 'held_for_review'),
		COALESCE(sum(amount_paise) FILTER (WHERE status = 'held_for_review'), 0)::BIGINT,
		count(*) FILTER (WHERE status = 'pending')
		FROM payouts`, since).Scan(&t.SentCount, &t.SentPaise, &t.HeldCount, &t.HeldPaise, &t.PendingCount)
	return t, err
}

// RidesSettledSince implements Store.
func (p *Postgres) RidesSettledSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT count(DISTINCT ride_id) FROM journal_entries
		WHERE kind = 'ride_settlement' AND created_at >= $1`, since).Scan(&n)
	return n, err
}
