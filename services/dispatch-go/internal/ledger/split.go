// Package ledger is HexaGo's double-entry, real-time clearing ledger.
//
// Every money movement is one immutable journal entry whose postings sum to
// zero (amounts are int64 paise; a positive amount is a debit, a negative one
// a credit). Ride settlement, payouts, refunds and wallet top-ups are entries;
// nothing is ever edited (a refund is a reversal entry). Guarded accounts
// (wallets and payables) can never go negative, a ride settles at most once
// (idempotency key = ride id), and a driver's payout is held for review while
// a safety incident on that ride is open.
package ledger

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Errors.
var (
	ErrValidation      = errors.New("ledger: invalid input")
	ErrUnbalanced      = errors.New("ledger: entry does not balance")
	ErrNegativeBalance = errors.New("ledger: guarded account would go negative")
	ErrNotFound        = errors.New("ledger: not found")
	ErrPayoutState     = errors.New("ledger: payout is not in the expected state")
	ErrForbidden       = errors.New("ledger: not allowed")
)

// MaxAmountPaise bounds any single amount (₹1 crore) so products of two amounts never overflow int64.
const MaxAmountPaise int64 = 1_000_000_000

// AccountType is one of the ledger's account classes.
type AccountType string

// Account types.
const (
	RiderWallet          AccountType = "rider_wallet"
	DriverPayable        AccountType = "driver_payable"
	FleetOperatorPayable AccountType = "fleet_operator_payable"
	CityLevyPayable      AccountType = "city_levy_payable"
	PlatformRevenue      AccountType = "platform_revenue"
	PromoWallet          AccountType = "promo_wallet"
	TaxPayable           AccountType = "tax_payable"
	PaymentClearing      AccountType = "payment_clearing"
)

var accountTypes = []AccountType{RiderWallet, DriverPayable, FleetOperatorPayable, CityLevyPayable,
	PlatformRevenue, PromoWallet, TaxPayable, PaymentClearing}

// AccountTypes lists every account type.
func AccountTypes() []AccountType { return append([]AccountType(nil), accountTypes...) }

// Valid reports whether t is a known account type.
func (t AccountType) Valid() bool {
	for _, k := range accountTypes {
		if k == t {
			return true
		}
	}
	return false
}

// CreditNormal reports whether the account's natural balance is a credit
// (liabilities and revenue). payment_clearing (cash at the gateway) and
// promo_wallet (promo spend funded by the platform) are debit-normal.
func (t AccountType) CreditNormal() bool { return t != PaymentClearing && t != PromoWallet }

// Guarded reports whether the account may never go negative: rider wallets
// and every payable.
func (t AccountType) Guarded() bool {
	switch t {
	case RiderWallet, DriverPayable, FleetOperatorPayable, CityLevyPayable, TaxPayable:
		return true
	}
	return false
}

// NormalBalance converts a signed (debit-positive) sum into the balance on the
// account's normal side.
func NormalBalance(t AccountType, signed int64) int64 {
	if t.CreditNormal() {
		return -signed
	}
	return signed
}

// Owners of the singleton accounts.
const (
	PlatformOwner = "hexago"
	CityOwner     = "bengaluru"
	TaxOwner      = "gst"
	GatewayOwner  = "gateway"
)

// Account identifies one ledger account: a type and an owner.
type Account struct {
	Type  AccountType
	Owner string
}

// Acct builds an Account.
func Acct(t AccountType, owner string) Account { return Account{Type: t, Owner: owner} }

// ID is the account's stable identifier, "type:owner".
func (a Account) ID() string { return string(a.Type) + ":" + a.Owner }

// ParseAccount parses "type:owner".
func ParseAccount(id string) (Account, error) {
	t, owner, ok := strings.Cut(id, ":")
	if !ok || owner == "" || !AccountType(t).Valid() {
		return Account{}, fmt.Errorf("%w: account %q", ErrValidation, id)
	}
	return Account{Type: AccountType(t), Owner: owner}, nil
}

// Rates are the settlement percentages, in basis points.
type Rates struct {
	DriverShareBps        int64 `json:"driver_share_bps"`         // independent driver's share of the fare
	FleetOperatorShareBps int64 `json:"fleet_operator_share_bps"` // fleet / AV operator's share of the fare
	CityLevyBps           int64 `json:"city_levy_bps"`            // municipal levy on the fare
	GSTBps                int64 `json:"gst_bps"`                  // GST on the platform fee
}

// DefaultRates: driver 75 %, fleet operator 80 %, city levy 2 %, GST 5 %.
func DefaultRates() Rates {
	return Rates{DriverShareBps: 7500, FleetOperatorShareBps: 8000, CityLevyBps: 200, GSTBps: 500}
}

// SplitInput is what a completed ride owes.
type SplitInput struct {
	Tier       string
	OperatorID string // set for fleet-owned cars and AVs: the operator is paid instead of the driver
	FarePaise  int64
	PromoPaise int64 // funded by promo_wallet, capped at the fare
	TipPaise   int64 // paid by the rider, 100 % to the payee
}

// Split is how one ride's money moves. Credits (payee share + tip, levy,
// GST, platform fee) always equal debits (rider charge + promo).
type Split struct {
	FarePaise        int64       `json:"fare_paise"`
	PromoPaise       int64       `json:"promo_paise"`
	TipPaise         int64       `json:"tip_paise"`
	RiderChargePaise int64       `json:"rider_charge_paise"` // fare - promo + tip
	PayeeType        AccountType `json:"payee_type"`         // driver_payable | fleet_operator_payable
	PayeeShareBps    int64       `json:"payee_share_bps"`
	PayeeSharePaise  int64       `json:"payee_share_paise"` // excluding the tip
	CityLevyPaise    int64       `json:"city_levy_paise"`
	GSTPaise         int64       `json:"gst_paise"`
	PlatformFeePaise int64       `json:"platform_fee_paise"` // the remainder, net of GST
}

// PayeePaise is what the payee earns from the ride (share + tip).
func (s Split) PayeePaise() int64 { return s.PayeeSharePaise + s.TipPaise }

// Credits is the sum of every credit leg.
func (s Split) Credits() int64 {
	return s.PayeePaise() + s.CityLevyPaise + s.GSTPaise + s.PlatformFeePaise
}

// Check verifies that the split balances.
func (s Split) Check() error {
	if debits := s.RiderChargePaise + s.PromoPaise; debits != s.Credits() || s.Credits() != s.FarePaise+s.TipPaise {
		return fmt.Errorf("%w: debits %d, credits %d, fare+tip %d", ErrUnbalanced, debits, s.Credits(), s.FarePaise+s.TipPaise)
	}
	return nil
}

func bps(amount, rate int64) int64 { return amount * rate / 10_000 } // floor for non-negative amounts

// ComputeSplit is the one pure settlement function. Every share is floored;
// the platform fee is whatever remains, so rounding remainders always land in
// platform_revenue and the split balances exactly.
//
//	payee    = fare × driver (or fleet operator) share
//	levy     = fare × city levy
//	R        = fare − payee − levy          (platform fee incl. GST)
//	GST      = R × gst / (1 + gst)           (5 % on the net platform fee)
//	platform = R − GST
func ComputeSplit(in SplitInput, r Rates) (Split, error) {
	switch {
	case in.FarePaise <= 0 || in.FarePaise > MaxAmountPaise:
		return Split{}, fmt.Errorf("%w: fare %d paise", ErrValidation, in.FarePaise)
	case in.PromoPaise < 0 || in.TipPaise < 0 || in.PromoPaise > MaxAmountPaise || in.TipPaise > MaxAmountPaise:
		return Split{}, fmt.Errorf("%w: promo %d / tip %d paise", ErrValidation, in.PromoPaise, in.TipPaise)
	}
	s := Split{FarePaise: in.FarePaise, TipPaise: in.TipPaise, PayeeType: DriverPayable, PayeeShareBps: r.DriverShareBps}
	if in.OperatorID != "" {
		s.PayeeType, s.PayeeShareBps = FleetOperatorPayable, r.FleetOperatorShareBps
	}
	if s.PayeeShareBps < 0 || r.CityLevyBps < 0 || r.GSTBps < 0 || s.PayeeShareBps+r.CityLevyBps > 10_000 {
		return Split{}, fmt.Errorf("%w: rates %+v", ErrValidation, r)
	}
	s.PromoPaise = min(in.PromoPaise, in.FarePaise)
	s.RiderChargePaise = in.FarePaise - s.PromoPaise + in.TipPaise
	s.PayeeSharePaise = bps(in.FarePaise, s.PayeeShareBps)
	s.CityLevyPaise = bps(in.FarePaise, r.CityLevyBps)
	rest := in.FarePaise - s.PayeeSharePaise - s.CityLevyPaise
	s.GSTPaise = rest * r.GSTBps / (10_000 + r.GSTBps)
	s.PlatformFeePaise = rest - s.GSTPaise
	return s, nil
}

// TierFare is one tier's tariff, in paise.
type TierFare struct {
	BasePaise  int64 `json:"base_paise"`
	PerKmPaise int64 `json:"per_km_paise"`
	MinPaise   int64 `json:"min_paise"`
}

// FareTable quotes a fare from tier and trip distance. It is the ledger's
// deterministic tariff for rides that carry no quoted fare.
type FareTable map[string]TierFare

// DefaultFares is the Bengaluru demo tariff.
func DefaultFares() FareTable {
	return FareTable{
		"auto":    {BasePaise: 3000, PerKmPaise: 1500, MinPaise: 3000},
		"mini":    {BasePaise: 5000, PerKmPaise: 1400, MinPaise: 6000},
		"sedan":   {BasePaise: 6000, PerKmPaise: 1700, MinPaise: 8000},
		"premium": {BasePaise: 10000, PerKmPaise: 2500, MinPaise: 15000},
		"xl":      {BasePaise: 8000, PerKmPaise: 2200, MinPaise: 12000},
		"av":      {BasePaise: 7000, PerKmPaise: 1600, MinPaise: 8000},
	}
}

// Quote returns the fare for km on tier, rounded to the rupee (unknown tiers
// are quoted as sedan).
func (ft FareTable) Quote(tier string, km float64) int64 {
	f, ok := ft[strings.ToLower(tier)]
	if !ok {
		f = ft["sedan"]
	}
	if km < 0 || math.IsNaN(km) {
		km = 0
	}
	raw := float64(f.BasePaise) + km*float64(f.PerKmPaise)
	if raw > float64(MaxAmountPaise) {
		return MaxAmountPaise
	}
	fare := int64(math.Round(raw/100)) * 100
	return max(fare, f.MinPaise)
}
