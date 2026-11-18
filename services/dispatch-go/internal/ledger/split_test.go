package ledger

import (
	"errors"
	"testing"
)

func TestComputeSplitTable(t *testing.T) {
	rates := DefaultRates()
	cases := []struct {
		name string
		in   SplitInput
		want Split
	}{
		{
			name: "sedan, independent driver, round numbers",
			in:   SplitInput{Tier: "sedan", FarePaise: 25000},
			want: Split{FarePaise: 25000, RiderChargePaise: 25000, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 18750, CityLevyPaise: 500, GSTPaise: 273, PlatformFeePaise: 5477},
		},
		{
			name: "odd fare: floors on every share, remainder to platform",
			in:   SplitInput{Tier: "mini", FarePaise: 12345},
			want: Split{FarePaise: 12345, RiderChargePaise: 12345, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 9258, CityLevyPaise: 246, GSTPaise: 135, PlatformFeePaise: 2706},
		},
		{
			name: "one paisa fare: everything to platform",
			in:   SplitInput{Tier: "auto", FarePaise: 1},
			want: Split{FarePaise: 1, RiderChargePaise: 1, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PlatformFeePaise: 1},
		},
		{
			name: "auto tier",
			in:   SplitInput{Tier: "auto", FarePaise: 9900},
			want: Split{FarePaise: 9900, RiderChargePaise: 9900, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 7425, CityLevyPaise: 198, GSTPaise: 108, PlatformFeePaise: 2169},
		},
		{
			name: "premium tier",
			in:   SplitInput{Tier: "premium", FarePaise: 64000},
			want: Split{FarePaise: 64000, RiderChargePaise: 64000, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 48000, CityLevyPaise: 1280, GSTPaise: 700, PlatformFeePaise: 14020},
		},
		{
			name: "xl tier",
			in:   SplitInput{Tier: "xl", FarePaise: 47700},
			want: Split{FarePaise: 47700, RiderChargePaise: 47700, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 35775, CityLevyPaise: 954, GSTPaise: 522, PlatformFeePaise: 10449},
		},
		{
			name: "AV: fleet operator share",
			in:   SplitInput{Tier: "av", FarePaise: 25000, OperatorID: "hexago_av_fleet"},
			want: Split{FarePaise: 25000, RiderChargePaise: 25000, PayeeType: FleetOperatorPayable, PayeeShareBps: 8000,
				PayeeSharePaise: 20000, CityLevyPaise: 500, GSTPaise: 214, PlatformFeePaise: 4286},
		},
		{
			name: "fleet-owned human car: operator share",
			in:   SplitInput{Tier: "sedan", FarePaise: 33333, OperatorID: "fleet_op_blr"},
			want: Split{FarePaise: 33333, RiderChargePaise: 33333, PayeeType: FleetOperatorPayable, PayeeShareBps: 8000,
				PayeeSharePaise: 26666, CityLevyPaise: 666, GSTPaise: 285, PlatformFeePaise: 5716},
		},
		{
			name: "promo funded from promo_wallet: credits unchanged, rider pays less",
			in:   SplitInput{Tier: "sedan", FarePaise: 25000, PromoPaise: 5000},
			want: Split{FarePaise: 25000, PromoPaise: 5000, RiderChargePaise: 20000, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 18750, CityLevyPaise: 500, GSTPaise: 273, PlatformFeePaise: 5477},
		},
		{
			name: "promo larger than the fare is capped at the fare",
			in:   SplitInput{Tier: "auto", FarePaise: 4000, PromoPaise: 9000},
			want: Split{FarePaise: 4000, PromoPaise: 4000, RiderChargePaise: 0, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 3000, CityLevyPaise: 80, GSTPaise: 43, PlatformFeePaise: 877},
		},
		{
			name: "tip goes entirely to the payee",
			in:   SplitInput{Tier: "sedan", FarePaise: 25000, TipPaise: 2000},
			want: Split{FarePaise: 25000, TipPaise: 2000, RiderChargePaise: 27000, PayeeType: DriverPayable, PayeeShareBps: 7500,
				PayeeSharePaise: 18750, CityLevyPaise: 500, GSTPaise: 273, PlatformFeePaise: 5477},
		},
		{
			name: "promo and tip together on an AV",
			in:   SplitInput{Tier: "av", FarePaise: 18001, PromoPaise: 2500, TipPaise: 1000, OperatorID: "hexago_av_fleet"},
			want: Split{FarePaise: 18001, PromoPaise: 2500, TipPaise: 1000, RiderChargePaise: 16501, PayeeType: FleetOperatorPayable,
				PayeeShareBps: 8000, PayeeSharePaise: 14400, CityLevyPaise: 360, GSTPaise: 154, PlatformFeePaise: 3087},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComputeSplit(tc.in, rates)
			if err != nil {
				t.Fatalf("ComputeSplit: %v", err)
			}
			if got != tc.want {
				t.Fatalf("split\n got %+v\nwant %+v", got, tc.want)
			}
			if err := got.Check(); err != nil {
				t.Fatalf("Check: %v", err)
			}
			if c := got.Credits(); c != got.FarePaise+got.TipPaise {
				t.Fatalf("credits %d != fare+tip %d", c, got.FarePaise+got.TipPaise)
			}
			if got.RiderChargePaise+got.PromoPaise != got.FarePaise+got.TipPaise {
				t.Fatalf("debits do not cover credits: %+v", got)
			}
		})
	}
}

func TestComputeSplitRejectsBadInput(t *testing.T) {
	for _, in := range []SplitInput{
		{Tier: "sedan", FarePaise: 0},
		{Tier: "sedan", FarePaise: -1},
		{Tier: "sedan", FarePaise: 100, PromoPaise: -1},
		{Tier: "sedan", FarePaise: 100, TipPaise: -1},
		{Tier: "sedan", FarePaise: MaxAmountPaise + 1},
		{Tier: "sedan", FarePaise: 100, TipPaise: MaxAmountPaise + 1},
	} {
		if _, err := ComputeSplit(in, DefaultRates()); !errors.Is(err, ErrValidation) {
			t.Errorf("%+v: err = %v, want ErrValidation", in, err)
		}
	}
	bad := DefaultRates()
	bad.DriverShareBps = 9900 // driver + levy > 100%
	if _, err := ComputeSplit(SplitInput{FarePaise: 100}, bad); !errors.Is(err, ErrValidation) {
		t.Errorf("bad rates: err = %v", err)
	}
}

func TestSplitCheckDetectsTampering(t *testing.T) {
	s, _ := ComputeSplit(SplitInput{FarePaise: 25000, PromoPaise: 1000}, DefaultRates())
	s.PlatformFeePaise++
	if err := s.Check(); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("tampered credits: %v", err)
	}
	s, _ = ComputeSplit(SplitInput{FarePaise: 25000, PromoPaise: 1000}, DefaultRates())
	s.RiderChargePaise--
	if err := s.Check(); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("tampered charge: %v", err)
	}
}

func TestFareQuote(t *testing.T) {
	ft := DefaultFares()
	for _, tc := range []struct {
		tier string
		km   float64
		want int64
	}{
		{"auto", 0, 3000},              // minimum fare
		{"auto", 4, 9000},              // 3000 + 4*1500
		{"mini", 5.26, 12400},          // 5000 + 5.26*1400 = 12364 -> rounded to the rupee
		{"sedan", 10, 23000},           // 6000 + 17000
		{"premium", 3, 17500},          // 10000 + 7500
		{"xl", 2, 12400},               // 8000 + 4400
		{"av", 7.5, 19000},             // 7000 + 12000
		{"unknown", 4, 0},              // unknown tiers are quoted as sedan (checked below)
		{"sedan", -3, 8000},            // negative distance clamps to the minimum
		{"sedan", 1e9, MaxAmountPaise}, // clamped to the maximum fare
	} {
		got := ft.Quote(tc.tier, tc.km)
		if tc.tier == "unknown" {
			if got != ft.Quote("sedan", tc.km) {
				t.Errorf("unknown tier = %d, want the sedan quote %d", got, ft.Quote("sedan", tc.km))
			}
			continue
		}
		if got != tc.want {
			t.Errorf("Quote(%s, %.2f) = %d, want %d", tc.tier, tc.km, got, tc.want)
		}
		if got%100 != 0 {
			t.Errorf("Quote(%s) = %d is not a whole rupee", tc.tier, got)
		}
	}
}

func TestAccountTypes(t *testing.T) {
	for _, at := range AccountTypes() {
		if !at.Valid() {
			t.Errorf("%s not valid", at)
		}
	}
	if AccountType("cash").Valid() {
		t.Error("cash is not an account type")
	}
	if !DriverPayable.Guarded() || !RiderWallet.Guarded() || PlatformRevenue.Guarded() || PaymentClearing.Guarded() {
		t.Error("guarded set wrong")
	}
	if !DriverPayable.CreditNormal() || PaymentClearing.CreditNormal() || PromoWallet.CreditNormal() {
		t.Error("normal side wrong")
	}
	a := Acct(DriverPayable, "d_001")
	if a.ID() != "driver_payable:d_001" {
		t.Errorf("ID = %s", a.ID())
	}
	back, err := ParseAccount(a.ID())
	if err != nil || back != a {
		t.Errorf("ParseAccount = %+v, %v", back, err)
	}
	for _, bad := range []string{"", "driver_payable", "nope:x", "driver_payable:"} {
		if _, err := ParseAccount(bad); err == nil {
			t.Errorf("ParseAccount(%q) accepted", bad)
		}
	}
	if NormalBalance(DriverPayable, -500) != 500 || NormalBalance(PaymentClearing, 700) != 700 {
		t.Error("NormalBalance")
	}
}
