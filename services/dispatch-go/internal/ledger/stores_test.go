package ledger

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hexago/hexago/services/dispatch-go/internal/store"
)

type storeMaker func(t *testing.T) Store

// behaviours run against every Store implementation.
var behaviours = map[string]func(*testing.T, storeMaker){
	"SettleRidePostsBalancedEntryAndPaysOut":       testSettleRidePostsBalancedEntryAndPaysOut,
	"PayoutWithinOneSecond":                        testPayoutWithinOneSecond,
	"ReplayIsNoOp":                                 testReplayIsNoOp,
	"SettleUsesWalletThenCard":                     testSettleUsesWalletThenCard,
	"SettleQuotesFareAndAppliesExtrasAndOperators": testSettleQuotesFareAndAppliesExtrasAndOperators,
	"SettleRejectsBadRides":                        testSettleRejectsBadRides,
	"HeldPayoutReleasedOnlyWhenIncidentResolved":   testHeldPayoutReleasedOnlyWhenIncidentResolved,
	"HoldAfterSettlementHoldsPendingPayout":        testHoldAfterSettlementHoldsPendingPayout,
	"RefundIsReversalEntry":                        testRefundIsReversalEntry,
	"RefundBeforePayoutReversesDriverShare":        testRefundBeforePayoutReversesDriverShare,
	"Topup":                                        testTopup,
	"SummaryAndWalletViews":                        testSummaryAndWalletViews,
	"ConcurrentSettlements":                        testConcurrentSettlements,
	"StoreContract":                                testStoreContract,
}

func TestMemoryStore(t *testing.T) {
	for name, fn := range behaviours {
		t.Run(name, func(t *testing.T) { fn(t, func(*testing.T) Store { return NewMemory() }) })
	}
}

// TestPostgresStore runs the same behaviours against a real Postgres when
// TEST_DATABASE_URL is set; every store gets a throwaway schema.
func TestPostgresStore(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres ledger tests")
	}
	for name, fn := range behaviours {
		t.Run(name, func(t *testing.T) { fn(t, pgStore) })
	}
	t.Run("PropertyInvariants", func(t *testing.T) {
		for seed := uint64(1); seed <= 4; seed++ {
			if err := runRandomOps(t, pgStore(t), seed, 80); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
		}
	})
	t.Run("DatabaseEnforcesInvariants", testDatabaseInvariants)
}

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := fmt.Sprintf("ledger_test_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func pgStore(t *testing.T) Store { return NewPostgres(pgPool(t)) }
