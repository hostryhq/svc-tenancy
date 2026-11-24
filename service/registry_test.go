package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/gstin"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/service"
)

func TestSeriesCodesAreUniqueThreeCharacterCodes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < model.CodeSpace; i++ {
		c := model.CodeFor(i)
		if len(c) != 3 || seen[c] || model.IndexOf(c) != i {
			t.Fatalf("index %d -> %q is not a unique 3-character code that round-trips", i, c)
		}
		for _, r := range c {
			if r == 'I' || r == 'O' || r == '0' || r == '1' {
				t.Fatalf("code %q uses a forbidden symbol", c)
			}
		}
		seen[c] = true
	}
	if len(seen) != 32768 {
		t.Fatalf("%d codes, want 32768 (ADR-0002 S7)", len(seen))
	}
}

func TestCreateRegistrationRules(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	create := func(in service.RegistrationInput) error {
		in.ID = f.newID()
		_, err := f.svc.CreateRegistration(ctx, in)
		return err
	}
	tests := []struct {
		name  string
		in    service.RegistrationInput
		field string
		code  string
	}{
		{"unregistered takes no GSTIN", service.RegistrationInput{LegalName: "X", Regime: "unregistered", GSTIN: "27AAPFU0939F1ZV", StateCode: "27"}, "gstin", "gstin_forbidden"},
		{"regular needs a GSTIN", service.RegistrationInput{LegalName: "X", Regime: "regular", StateCode: "27"}, "gstin", "gstin_required"},
		{"bad check character", service.RegistrationInput{LegalName: "X", Regime: "regular", GSTIN: "27AAPFU0939F1ZX", StateCode: "27"}, "gstin", gstin.CodeCheckChar},
		{"bad format", service.RegistrationInput{LegalName: "X", Regime: "regular", GSTIN: "27AAPFU", StateCode: "27"}, "gstin", gstin.CodeFormat},
		{"state differs from the GSTIN prefix", service.RegistrationInput{LegalName: "X", Regime: "regular", GSTIN: "27AAPFU0939F1ZV", StateCode: "33"}, "gstin", "gstin_state_mismatch"},
		{"state not in the reference table", service.RegistrationInput{LegalName: "X", Regime: "unregistered", StateCode: "99"}, "state_code", "unknown_state"},
		{"empty legal name", service.RegistrationInput{LegalName: " ", Regime: "unregistered", StateCode: "33"}, "legal_name", "length"},
		{"unknown regime", service.RegistrationInput{LegalName: "X", Regime: "weird", StateCode: "33"}, "regime", "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantField(t, create(tc.in), tc.field, tc.code) })
	}

	t.Run("a GSTIN already held in the organisation is refused, whatever its spelling", func(t *testing.T) {
		for _, spelling := range []string{"33ABCDE1234F1Z7", "33abcde1234f1z7", " 33ABCDE 1234F1Z7 "} {
			wantField(t, create(service.RegistrationInput{LegalName: "Again", Regime: "regular", GSTIN: spelling, StateCode: "33"}), "gstin", "gstin_duplicate")
		}
	})
	t.Run("unregistered registrations are exempt from uniqueness", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if err := create(service.RegistrationInput{LegalName: "Stall", Regime: "unregistered", StateCode: "33"}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("a GSTIN claimed by another organisation is accepted unverified with the same response shape", func(t *testing.T) {
		// Other holds 29ABCDE1234F1ZW verified. The example organisation adds it: nothing differs from any new registration.
		mine, err := f.svc.CreateRegistration(ctx, service.RegistrationInput{ID: f.newID(), LegalName: "Mine", Regime: "regular", GSTIN: seed.Other.GSTIN, StateCode: "29"})
		if err != nil {
			t.Fatalf("a tenant must not learn that the GSTIN is held elsewhere: %v", err)
		}
		fresh, err := f.svc.CreateRegistration(ctx, service.RegistrationInput{ID: f.newID(), LegalName: "Mine", Regime: "regular", GSTIN: "27AAPFU0939F1ZV", StateCode: "27"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range []model.Registration{mine, fresh} {
			if r.Verification != model.Unverified {
				t.Fatalf("registration %s must start unverified, is %s", r.ID, r.Verification)
			}
		}
		mine.ID, fresh.ID, mine.GSTIN, fresh.GSTIN, mine.StateCode, fresh.StateCode = "", "", "", "", "", ""
		if marshal(t, mine) != marshal(t, fresh) {
			t.Fatalf("responses differ:\n%s\n%s", marshal(t, mine), marshal(t, fresh))
		}
	})
	t.Run("the same id with the same content is an idempotent repeat, other content is refused", func(t *testing.T) {
		in := service.RegistrationInput{ID: f.newID(), LegalName: "Repeat", Regime: "unregistered", StateCode: "33"}
		a, err := f.svc.CreateRegistration(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		b, err := f.svc.CreateRegistration(ctx, in)
		if err != nil || a.ID != b.ID {
			t.Fatalf("repeat: %v %v", b, err)
		}
		in.LegalName = "Changed"
		wantField(t, func() error { _, err := f.svc.CreateRegistration(ctx, in); return err }(), "id", "already_exists")
	})
}

func TestVerifiedRegistrationIsFixed(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	name := "Renamed Pvt Ltd"
	if _, err := f.svc.UpdateRegistration(ctx, seed.Registration, &name, nil); err == nil {
		t.Fatal("a verified registration must not change")
	} else {
		wantCode(t, err, "registration_fixed")
	}
	same := "Example Hospitality Pvt Ltd"
	if _, err := f.svc.UpdateRegistration(ctx, seed.Registration, &same, nil); err != nil {
		t.Fatalf("a request that changes nothing must succeed: %v", err)
	}
	// An unverified one can be corrected, including the GSTIN, which stays unique within the organisation.
	r, err := f.svc.CreateRegistration(ctx, service.RegistrationInput{ID: f.newID(), LegalName: "Typo", Regime: "regular", GSTIN: "27AAPFU0939F1ZV", StateCode: "27"})
	if err != nil {
		t.Fatal(err)
	}
	fix := "Fixed"
	got, err := f.svc.UpdateRegistration(ctx, r.ID, &fix, nil)
	if err != nil || got.LegalName != "Fixed" {
		t.Fatalf("correcting an unverified registration: %v %v", got, err)
	}
	r3, err := f.svc.CreateRegistration(ctx, service.RegistrationInput{ID: f.newID(), LegalName: "Second", Regime: "regular", GSTIN: "33AAAAA0000A1Z9", StateCode: "33"})
	if err != nil {
		t.Fatal(err)
	}
	dup := " " + seed.Example.GSTIN + " "
	wantField(t, func() error { _, err := f.svc.UpdateRegistration(ctx, r3.ID, nil, &dup); return err }(), "gstin", "gstin_duplicate")
}

func issue(f *fixture, tenant, outlet, device ids.ID, state string, types ...string) ([]model.Series, error) {
	if len(types) == 0 {
		types = []string{"tax_invoice"}
	}
	return f.svc.IssueSeries(system(tenant), service.IssueInput{OutletID: outlet, DeviceID: device, DocTypes: types, State: state})
}

func TestIssueSeriesNeedsAVerifiedRegistration(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	reg, err := f.svc.CreateRegistration(ctx, service.RegistrationInput{ID: f.newID(), LegalName: "New", Regime: "regular", GSTIN: "27AAPFU0939F1ZV", StateCode: "27"})
	if err != nil {
		t.Fatal(err)
	}
	outlet, err := f.svc.CreateOutlet(ctx, service.OutletInput{ID: f.newID(), Name: "Mumbai", Kind: "restaurant", StateCode: "27", TaxRegistrationID: reg.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issue(f, seed.Example.Org, outlet.ID, f.newID(), "active"); err == nil {
		t.Fatal("no code may be issued under an unverified registration")
	} else {
		wantCode(t, err, "registration_unverified")
	}
	// A kind-unregistered registration needs no verification.
	un, _ := f.svc.CreateRegistration(ctx, service.RegistrationInput{ID: f.newID(), LegalName: "Stall", Regime: "unregistered", StateCode: "33"})
	stall, err := f.svc.CreateOutlet(ctx, service.OutletInput{ID: f.newID(), Name: "Stall", Kind: "stall", StateCode: "33", TaxRegistrationID: un.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := issue(f, seed.Example.Org, stall.ID, f.newID(), "active"); err != nil || len(rows) != 1 || rows[0].GSTIN != "" {
		t.Fatalf("unregistered issuance: %v %v", rows, err)
	}
}

func TestIssueSeriesIsPerRegistrationAndRepeatable(t *testing.T) {
	f := newFixture(t)
	d1, d2 := f.newID(), f.newID()
	a, err := issue(f, seed.Example.Org, seed.Outlet, d1, "active", "tax_invoice", "credit_note")
	if err != nil || len(a) != 2 {
		t.Fatalf("issue: %v %v", a, err)
	}
	again, err := issue(f, seed.Example.Org, seed.Outlet, d1, "active", "tax_invoice", "credit_note")
	if err != nil || marshal(t, a) != marshal(t, again) {
		t.Fatalf("a repeat for the same device and outlet must return the same codes: %v %v", again, err)
	}
	b, err := issue(f, seed.Example.Org, seed.Outlet, d2, "active", "tax_invoice")
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{model.CodeFor(0): true} // the seeded series holds the first code
	for _, s := range append(a, b...) {
		if codes[s.Code] {
			t.Fatalf("code %s issued twice under one registration", s.Code)
		}
		codes[s.Code] = true
	}
	// Two registrations (two organisations here) may each hold the same code once.
	other, err := issue(f, seed.Other.Org, seed.Other.Outlet, f.newID(), "active")
	if err != nil || other[0].Code != model.CodeFor(1) {
		t.Fatalf("another registration has its own code space: %v %v", other, err)
	}
	// A waiter's dormant series become active when billing is granted later.
	w := f.newID()
	dormant, _ := issue(f, seed.Example.Org, seed.Outlet, w, "dormant")
	active, err := issue(f, seed.Example.Org, seed.Outlet, w, "active")
	if err != nil || dormant[0].Status != "dormant" || active[0].Status != "active" || active[0].Code != dormant[0].Code {
		t.Fatalf("dormant -> active keeps the code: %v %v %v", dormant, active, err)
	}
}

func TestIssueSeriesIsBoundByTenant(t *testing.T) {
	f := newFixture(t)
	// The service token names tenant A but the outlet is tenant B's: 404, and nothing is issued for B.
	_, err := issue(f, seed.Example.Org, seed.Other.Outlet, f.newID(), "active")
	wantCode(t, err, "not_found")
}

func TestIssueSeriesDailyCapPerRegistration(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < service.DailyCap-1; i++ { // the seeded series was issued on the same IST day
		if _, err := issue(f, seed.Example.Org, seed.Outlet, f.newID(), "active"); err != nil {
			t.Fatalf("code %d: %v", i+1, err)
		}
	}
	_, err := issue(f, seed.Example.Org, seed.Outlet, f.newID(), "active")
	wantCode(t, err, "series_cap_reached")
	f.clk.Advance(24 * time.Hour)
	if _, err := issue(f, seed.Example.Org, seed.Outlet, f.newID(), "active"); err != nil {
		t.Fatalf("the next IST day starts a new allowance: %v", err)
	}
}

func TestDeviceSeriesClosureAndReissue(t *testing.T) {
	f := newFixture(t)
	d := f.newID()
	first, _ := issue(f, seed.Example.Org, seed.Outlet, d, "active", "tax_invoice", "credit_note")
	n, err := f.svc.CloseDeviceSeries(system(seed.Example.Org), d, "reinstalled")
	if err != nil || n != 2 {
		t.Fatalf("closed %d, %v", n, err)
	}
	again, err := issue(f, seed.Example.Org, seed.Outlet, d, "active", "tax_invoice")
	if err != nil || again[0].Code == first[0].Code || again[0].Code == first[1].Code {
		t.Fatalf("re-activation must get new codes: %v %v", again, err)
	}
	_, err = f.svc.CloseDeviceSeries(system(seed.Example.Org), f.newID(), "revoked")
	wantCode(t, err, "not_found")
	_, err = f.svc.CloseDeviceSeries(system(seed.Other.Org), d, "revoked")
	wantCode(t, err, "not_found") // another tenant's device
}

func TestCloseSeriesUnreconciledNeedsReasonAndKeepsRecord(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	if _, err := f.svc.CloseSeriesUnreconciled(ctx, seed.Series, "short"); err == nil {
		t.Fatal("a reason of 10 to 500 characters is required")
	}
	got, err := f.svc.CloseSeriesUnreconciled(ctx, seed.Series, "Device stolen and never reconnected.")
	if err != nil || got.Status != "closed_unreconciled" || got.ClosedAt == nil {
		t.Fatalf("close: %v %v", got, err)
	}
	// Even a series closed by a revocation can be marked unreconciled afterwards.
	d := f.newID()
	issue(f, seed.Example.Org, seed.Outlet, d, "active")
	f.svc.CloseDeviceSeries(system(seed.Example.Org), d, "revoked")
	rows, _ := f.svc.ListSeries(ctx, "", "closed", "", 50)
	if len(rows.Items) != 1 {
		t.Fatalf("closed series: %v", rows.Items)
	}
	if got, err := f.svc.CloseSeriesUnreconciled(ctx, rows.Items[0].ID, "Revoked device never uploaded its tail."); err != nil || got.Status != "closed_unreconciled" {
		t.Fatalf("closed -> closed_unreconciled: %v %v", got, err)
	}
}

// T13: after a simulated restore that forgets issued codes, the next code is beyond every forgotten one.
func TestRestoreJumpMovesEveryAllocatorBeyondForgottenCodes(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		issue(f, seed.Example.Org, seed.Outlet, f.newID(), "active")
	}
	last, _ := issue(f, seed.Example.Org, seed.Outlet, f.newID(), "active")
	forgotten := model.IndexOf(last[0].Code)
	// The lost window is 2 whole days: the allocator jumps by 200 x (2 + 1).
	if _, err := f.svc.AdvanceAllocatorsAfterRestore(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	after, err := issue(f, seed.Example.Org, seed.Outlet, f.newID(), "active")
	if err != nil {
		t.Fatal(err)
	}
	if got := model.IndexOf(after[0].Code); got < forgotten+600 {
		t.Fatalf("next code index %d is not beyond the forgotten window (%d + 600)", got, forgotten)
	}
}
