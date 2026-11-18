-- Double-entry clearing ledger (dispatch-go internal/ledger).
-- Amounts are BIGINT paise, never floats. A posting's amount_paise > 0 is a
-- debit, < 0 a credit; every journal entry's postings sum to zero (checked by
-- a deferred constraint trigger at commit). journal_entries and postings are
-- append-only: refunds are reversal entries, never edits.
CREATE TABLE IF NOT EXISTS accounts (
    id            TEXT PRIMARY KEY,                -- "<type>:<owner>"
    type          TEXT NOT NULL CHECK (type IN ('rider_wallet', 'driver_payable', 'fleet_operator_payable',
                      'city_levy_payable', 'platform_revenue', 'promo_wallet', 'tax_payable', 'payment_clearing')),
    owner         TEXT NOT NULL,
    guarded       BOOLEAN NOT NULL,                -- wallets and payables: may never go negative
    balance_paise BIGINT NOT NULL DEFAULT 0,       -- signed (debit-positive) sum of postings
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- guarded accounts are credit-normal: a negative balance is a positive signed sum
    CONSTRAINT accounts_guarded_non_negative CHECK (NOT guarded OR balance_paise <= 0)
);
CREATE INDEX IF NOT EXISTS accounts_owner_idx ON accounts (owner);

CREATE TABLE IF NOT EXISTS journal_entries (
    seq             BIGSERIAL UNIQUE,
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL CHECK (kind IN ('ride_settlement', 'payout', 'refund', 'topup')),
    idempotency_key TEXT NOT NULL UNIQUE,         -- "ride:<ride id>" for settlements
    ride_id         TEXT,
    reverses_id     TEXT REFERENCES journal_entries (id),
    description     TEXT NOT NULL DEFAULT '',
    meta            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS journal_entries_ride_idx ON journal_entries (ride_id, seq DESC);
CREATE INDEX IF NOT EXISTS journal_entries_created_idx ON journal_entries (created_at DESC);

CREATE TABLE IF NOT EXISTS postings (
    id           BIGSERIAL PRIMARY KEY,
    entry_id     TEXT NOT NULL REFERENCES journal_entries (id),
    account_id   TEXT NOT NULL REFERENCES accounts (id),
    amount_paise BIGINT NOT NULL CHECK (amount_paise <> 0),
    memo         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS postings_entry_idx ON postings (entry_id);
CREATE INDEX IF NOT EXISTS postings_account_idx ON postings (account_id);

CREATE TABLE IF NOT EXISTS payouts (
    id           TEXT PRIMARY KEY,               -- "po_<ride id>"
    ride_id      TEXT NOT NULL,
    account_id   TEXT NOT NULL REFERENCES accounts (id),
    driver_id    TEXT NOT NULL,
    amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
    status       TEXT NOT NULL CHECK (status IN ('pending', 'sent', 'held_for_review')),
    incident_id  TEXT,
    entry_id     TEXT REFERENCES journal_entries (id),
    created_at   TIMESTAMPTZ NOT NULL,
    held_at      TIMESTAMPTZ,
    released_at  TIMESTAMPTZ,
    sent_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS payouts_status_idx ON payouts (status, created_at DESC);
CREATE INDEX IF NOT EXISTS payouts_incident_idx ON payouts (incident_id) WHERE incident_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS payouts_driver_idx ON payouts (driver_id, created_at DESC);
CREATE INDEX IF NOT EXISTS payouts_sent_idx ON payouts (sent_at) WHERE status = 'sent';

-- Every entry balances: checked once per entry when the transaction commits.
CREATE OR REPLACE FUNCTION ledger_entry_balances() RETURNS trigger AS $$
DECLARE
    total BIGINT;
    legs  INT;
BEGIN
    SELECT COALESCE(SUM(amount_paise), 0), COUNT(*) INTO total, legs FROM postings WHERE entry_id = NEW.id;
    IF total <> 0 OR legs < 2 THEN
        RAISE EXCEPTION 'journal entry % does not balance (sum %, % postings)', NEW.id, total, legs
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS journal_entries_balanced ON journal_entries;
CREATE CONSTRAINT TRIGGER journal_entries_balanced
    AFTER INSERT ON journal_entries DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_entry_balances();

-- Entries and postings are immutable.
CREATE OR REPLACE FUNCTION ledger_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: post a reversal entry instead', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS journal_entries_append_only ON journal_entries;
CREATE TRIGGER journal_entries_append_only BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
DROP TRIGGER IF EXISTS postings_append_only ON postings;
CREATE TRIGGER postings_append_only BEFORE UPDATE OR DELETE ON postings
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
