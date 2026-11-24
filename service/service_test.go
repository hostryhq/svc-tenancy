package service_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/scopetest"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/scopecheck"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository/mem"
	"github.com/hostryhq/svc-tenancy/service"
)

// An Idempotency-Key makes a retry safe: the effect and the key commit together, a replay returns the
// stored result, the same key with a different request is refused, and a failed attempt stores nothing.
func TestIdempotencyKey(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	id := f.newID()
	with := func(key, fp string) context.Context {
		return service.WithIdem(ctx, service.Idem{Op: "createBrand", Key: key, Fingerprint: fp})
	}
	first, err := f.svc.CreateBrand(with("k1", "fp-a"), id, "Bakery")
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(1) // a repeat that really ran again would stamp a different time
	again, err := f.svc.CreateBrand(with("k1", "fp-a"), id, "Bakery")
	if err != nil || marshal(t, first) != marshal(t, again) {
		t.Fatalf("replay must return the original result: %v %v", again, err)
	}
	_, err = f.svc.CreateBrand(with("k1", "fp-other"), id, "Bakery")
	wantCode(t, err, idempotency.ErrMismatch.Code)

	t.Run("a failed request does not burn the key", func(t *testing.T) {
		if _, err := f.svc.CreateBrand(with("k2", "fp"), "not-a-uuid", "X"); err == nil {
			t.Fatal("expected validation error")
		}
		if _, err := f.svc.CreateBrand(with("k2", "fp"), f.newID(), "X"); err != nil {
			t.Fatalf("the retry must run: %v", err)
		}
	})
	t.Run("the same key in another operation or tenant is a different request", func(t *testing.T) {
		if _, err := f.svc.RenameOrganisation(service.WithIdem(ctx, service.Idem{Op: "updateCurrentOrganisation", Key: "k1", Fingerprint: "x"}), "Renamed"); err != nil {
			t.Fatal(err)
		}
		other := service.WithIdem(f.as(seed.Other.Org), service.Idem{Op: "createBrand", Key: "k1", Fingerprint: "fp-a"})
		if _, err := f.svc.CreateBrand(other, id, "Bakery"); err != nil { // TST-09: the same id and key in tenant B
			t.Fatalf("keys and ids are scoped by tenant: %v", err)
		}
	})
}

func TestSettingsAndRenamePublishOrganisationUpdatedOnlyOnChange(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	count := func() (n int, last string) {
		for _, e := range f.store.Events() {
			if e.Type == "tenancy.organisation.updated" {
				n, last = n+1, string(e.Data)
			}
		}
		return
	}
	if _, err := f.svc.UpdateSettings(ctx, 7, 5); err != nil { // the seeded values: nothing changes
		t.Fatal(err)
	}
	if n, _ := count(); n != 0 {
		t.Fatalf("an unchanged PUT must not publish, got %d events", n)
	}
	got, err := f.svc.UpdateSettings(ctx, 14, 10)
	if err != nil || got.OfflineWindowDays != 14 || got.IdleLockMinutes != 10 {
		t.Fatalf("%+v %v", got, err)
	}
	n, data := count()
	if n != 1 || !strings.Contains(data, `"changed":["settings"]`) || !strings.Contains(data, `"status":"trial"`) {
		t.Fatalf("settings change: %d %s", n, data)
	}
	if _, err := f.svc.RenameOrganisation(ctx, "Renamed Hospitality"); err != nil {
		t.Fatal(err)
	}
	if n, data = count(); n != 2 || !strings.Contains(data, `"changed":["name"]`) || strings.Contains(data, "Renamed") {
		t.Fatalf("rename event must say what changed, not the new name: %d %s", n, data)
	}
	for _, tc := range []struct{ days, idle int }{{0, 5}, {31, 5}, {7, 0}, {7, 121}} {
		if _, err := f.svc.UpdateSettings(ctx, tc.days, tc.idle); err == nil {
			t.Fatalf("%+v must be refused", tc)
		}
	}
}

// ADR-0003 A1.1 item 8: no GSTIN in any event payload; ids only.
func TestEventsNeverCarryAGSTIN(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	reg, err := f.svc.CreateRegistration(ctx, service.RegistrationInput{ID: f.newID(), LegalName: "New", Regime: "regular", GSTIN: "27AAPFU0939F1ZV", StateCode: "27"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.svc.CreateOutlet(ctx, service.OutletInput{ID: f.newID(), Name: "Mumbai", Kind: "restaurant", StateCode: "27", TaxRegistrationID: reg.ID})
	if err != nil {
		t.Fatal(err)
	}
	name := "Renamed"
	if _, err := f.svc.UpdateOutlet(ctx, o.ID, &name, nil); err != nil {
		t.Fatal(err)
	}
	evs := f.store.Events()
	types := map[string]bool{}
	for _, e := range evs {
		types[e.Type] = true
		if err := e.Validate(); err != nil {
			t.Fatalf("%s: %v", e.Type, err)
		}
		for _, secret := range []string{"27AAPFU0939F1ZV", seed.ExampleGSTIN, "Mumbai", "Renamed"} {
			if strings.Contains(string(e.Data), secret) {
				t.Fatalf("event %s carries %q", e.Type, secret)
			}
		}
	}
	if !types["tenancy.outlet.created"] || !types["tenancy.outlet.updated"] {
		t.Fatalf("outlet events missing: %v", types)
	}
}

func TestOutletRules(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(seed.Example.Org)
	in := func(mut func(*service.OutletInput)) error {
		o := service.OutletInput{ID: f.newID(), Name: "Annexe", Kind: "restaurant", StateCode: "33", TaxRegistrationID: seed.Registration}
		mut(&o)
		_, err := f.svc.CreateOutlet(ctx, o)
		return err
	}
	wantField(t, in(func(o *service.OutletInput) { o.Kind = "palace" }), "kind", "unknown")
	wantField(t, in(func(o *service.OutletInput) { o.StateCode = "27" }), "state_code", "registration_state_mismatch")
	wantField(t, in(func(o *service.OutletInput) { o.StateCode = "99" }), "state_code", "unknown_state")
	wantCode(t, in(func(o *service.OutletInput) { o.TaxRegistrationID = seed.Other.Registration }), "not_found") // another tenant's registration
	wantCode(t, in(func(o *service.OutletInput) { o.BrandID = seed.Other.Brand }), "not_found")
	if err := in(func(o *service.OutletInput) { o.LocatedInOutletID = seed.Outlet }); err != nil {
		t.Fatalf("an in-house restaurant inside the organisation's own property: %v", err)
	}
	host := f.newID()
	_, _ = f.svc.CreateOutlet(ctx, service.OutletInput{ID: host, Name: "Host", Kind: "property", StateCode: "33", TaxRegistrationID: seed.Registration, LocatedInOutletID: seed.Outlet})
	wantField(t, in(func(o *service.OutletInput) { o.LocatedInOutletID = host }), "located_in_outlet_id", "too_deep")
	// Creating an outlet takes an organisation-wide grant, even for a member who holds the permission at one outlet.
	scoped := f.asGrants(seed.Example.Org, []auth.Grant{{Outlets: []string{seed.Outlet.String()}, Perms: []string{"outlet.create"}}})
	_, err := f.svc.CreateOutlet(scoped, service.OutletInput{ID: f.newID(), Name: "Z", Kind: "stall", StateCode: "33", TaxRegistrationID: seed.Registration})
	wantCode(t, err, "auth.permission_denied")
}

// T14 (ADR-0003 A1.4), generated from go-auth/scopetest: a member granted outlet X only gets nothing of
// outlet Y from lists and reads (404); an organisation-wide member sees both; the outlet filter never widens.
func TestOutletScopingT14(t *testing.T) {
	cat, err := gen.Catalogue()
	if err != nil {
		t.Fatal(err)
	}
	clk := testkit.NewClock()
	clk.Set(seed.At)
	gens := clk.IDs()
	store := mem.New(gens)
	svc := service.New(store, service.Options{Clock: clk, IDs: gens, Log: slog.New(slog.DiscardHandler)})
	if err := scopecheck.World.Apply(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, r := range scopecheck.Resources(svc) {
		t.Run(r.Name, func(t *testing.T) {
			if bad := scopetest.Check(context.Background(), cat, r); len(bad) > 0 {
				t.Fatalf("T14 failed:\n%s", strings.Join(bad, "\n"))
			}
		})
	}
}
