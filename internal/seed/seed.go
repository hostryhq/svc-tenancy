// Package seed builds fixture organisations. Example is the "example world": the organisation, brand,
// outlet, registration and series that the examples of the tenancy OpenAPI document refer to. The
// contract suite sends requests built from those examples, so the double and a real service must be
// seeded to match them (api-specs contracttest). Other is a second, unrelated organisation for
// isolation tests.
package seed

import (
	"context"
	"time"

	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// Ids used by the spec examples.
const (
	Org          ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a60"
	Outlet       ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a61"
	Brand        ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6a"
	DefaultBrand ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6e"
	Registration ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6b"
	Series       ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6c"
	Device       ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e"
	Owner        string = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a64"
	VendorUser   string = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a66"
	// ExampleGSTIN: the spec examples say ...Z5, whose check character is wrong.
	ExampleGSTIN = "33ABCDE1234F1Z7"
)

// At is the instant of the examples.
var At = time.Date(2026, 11, 24, 13, 5, 11, 120_000_000, time.UTC)

// World is one fixture organisation: trial status, a default brand and a second brand, one restaurant
// outlet (plus an optional second one), one verified registration and one active series.
type World struct {
	Org, Outlet, OutletY, DefaultBrand, Brand, Registration, Series, Device ids.ID
	Name, GSTIN                                                             string
}

// Example is the organisation of the spec examples. The example brand is not the default brand, so a
// create with the example id is an idempotent repeat.
var Example = World{Org: Org, Outlet: Outlet, DefaultBrand: DefaultBrand, Brand: Brand, Registration: Registration, Series: Series,
	Device: Device, Name: "Example Hospitality", GSTIN: ExampleGSTIN}

// Other is a second organisation, with a different GSTIN, for isolation tests.
var Other = World{
	Org: "0190b000-0000-7000-8000-000000000b60", Outlet: "0190b000-0000-7000-8000-000000000b61", OutletY: "0190b000-0000-7000-8000-000000000b62",
	DefaultBrand: "0190b000-0000-7000-8000-000000000b6e", Brand: "0190b000-0000-7000-8000-000000000b6a",
	Registration: "0190b000-0000-7000-8000-000000000b6b", Series: "0190b000-0000-7000-8000-000000000b6c", Device: "0190b000-0000-7000-8000-000000000b5e",
	Name: "Other Hotels", GSTIN: "29ABCDE1234F1ZW",
}

// Third is a third organisation, for link tests (a forwarded invitation code accepted by the wrong party).
var Third = World{
	Org: "0190c000-0000-7000-8000-000000000c60", Outlet: "0190c000-0000-7000-8000-000000000c61",
	DefaultBrand: "0190c000-0000-7000-8000-000000000c6e", Brand: "0190c000-0000-7000-8000-000000000c6a",
	Registration: "0190c000-0000-7000-8000-000000000c6b", Series: "0190c000-0000-7000-8000-000000000c6c", Device: "0190c000-0000-7000-8000-000000000c5e",
	Name: "Third Foods", GSTIN: "27AAPFU0939F1ZV",
}

// ExampleWorld stores the example organisation.
func ExampleWorld(ctx context.Context, repo repository.Repository) error { return Example.Apply(ctx, repo) }

// Apply stores the world through the repository, as that organisation.
func (w World) Apply(ctx context.Context, repo repository.Repository) error {
	state := w.GSTIN[:2]
	tctx := tenancy.With(ctx, tenancy.Context{TenantID: w.Org, ActorID: "seed"})
	return repo.InTx(tctx, func(c context.Context, tx repository.Tx) error {
		if err := tx.InsertOrg(c, model.Org{ID: w.Org, Name: w.Name, Status: model.OrgTrial, DataPlacement: "primary", CreatedAt: At, UpdatedAt: At},
			model.Settings{OfflineWindowDays: 7, IdleLockMinutes: 5, UpdatedAt: At}); err != nil {
			return err
		}
		for _, b := range []model.Brand{
			{ID: w.DefaultBrand, Name: w.Name, IsDefault: true, CreatedAt: At},
			{ID: w.Brand, Name: "Example Bakery", CreatedAt: At},
		} {
			if err := tx.InsertBrand(c, b); err != nil {
				return err
			}
		}
		verified := At.Add(5 * time.Minute)
		if err := tx.InsertRegistration(c, model.Registration{ID: w.Registration, LegalName: w.Name + " Pvt Ltd", Regime: model.RegimeRegular,
			GSTIN: w.GSTIN, StateCode: state, Verification: model.Verified, Method: "certificate_seen", VerifiedAt: &verified, VerifiedBy: VendorUser,
			Status: model.RegOpen, CreatedAt: At}); err != nil {
			return err
		}
		for _, o := range []ids.ID{w.Outlet, w.OutletY} {
			if o == "" {
				continue
			}
			if err := tx.InsertOutlet(c, model.Outlet{ID: o, BrandID: w.DefaultBrand, Name: "Main Restaurant", Kind: "restaurant", Status: model.OutletActive,
				StateCode: state, TaxRegistrationID: w.Registration, CreatedAt: At}); err != nil {
				return err
			}
		}
		if err := tx.InsertSeries(c, model.Series{ID: w.Series, TaxRegistrationID: w.Registration, GSTIN: w.GSTIN, Code: model.CodeFor(0), CodeIndex: 0,
			DocType: "tax_invoice", OutletID: w.Outlet, DeviceID: w.Device, Status: model.SeriesActive, IssuedAt: At}); err != nil {
			return err
		}
		return tx.SetCodeIndex(c, w.Registration, 1, At)
	})
}
