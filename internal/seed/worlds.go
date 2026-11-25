package seed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// The example worlds of the tenancy OpenAPI document (info.x-example-worlds). The shared contract suite
// asks the target to be put into an operation's world before the call (contracttest Options.SeedWorld);
// ApplyWorld does that for the double and for the real service over the same repository interface.
const (
	WorldBase                        = "base"
	WorldEmpty                       = "empty"
	WorldRegistrationVerified        = "registration-verified"
	WorldSeriesIssued                = "series-issued"
	WorldRegistrationSpare           = "registration-spare-verified"
	WorldLinkNone                    = "link-none"
	WorldLinkProposedToCaller        = "link-proposed-to-caller"
	WorldLinkAccepted                = "link-accepted"
	WorldLinkActive                  = "link-active"
	WorldLinkSuspended               = "link-suspended"
	WorldLinkAmendmentPending        = "link-amendment-pending"
	invitationCode                   = "7QK3M9XD7QK3M9XD"
	spareRegistration         ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a93"
	CreditNoteSeries          ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a95"
	LinkID                    ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a62"
	GrantID                   ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a72"
	AmendmentID               ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a73"
	Restaurants               ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a63" // the second organisation of the link worlds
	RestaurantsOutlet         ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a94"
	restaurantsBrand          ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aa0"
	restaurantsReg            ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aa1"
	activityProposed          ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a96"
	activityAccepted          ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a97"
	activityConfirmed         ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a98"
	activitySuspended         ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a99"
	amendmentGrantID          ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4aa2"
	exampleOrgName                   = "Example Hospitality"
	restaurantsGSTIN                 = "33AAAAA0000A1Z9"
	karnatakaGSTIN                   = "29ABCDE1234F1ZW"
)

// WorldCaller is the organisation a tenant-realm caller acts as in the world. Only the link world in which
// the caller is the invited party differs from the example organisation.
func WorldCaller(world string) ids.ID {
	switch world {
	case WorldLinkProposedToCaller, WorldLinkAmendmentPending:
		return Restaurants
	}
	return Org
}

// KnownWorld reports whether the world is declared.
func KnownWorld(world string) bool {
	switch world {
	case WorldBase, WorldEmpty, WorldRegistrationVerified, WorldSeriesIssued, WorldRegistrationSpare, WorldLinkNone,
		WorldLinkProposedToCaller, WorldLinkAccepted, WorldLinkActive, WorldLinkSuspended, WorldLinkAmendmentPending:
		return true
	}
	return false
}

type worldSpec struct {
	verified bool // registration 4a6b verified
	series   bool // tax_invoice and credit_note series issued
	spare    bool // a second, verified, unreferenced registration
	others   bool // the second organisation exists
	link     string
}

var worldSpecs = map[string]worldSpec{
	WorldBase:                 {},
	WorldEmpty:                {},
	WorldRegistrationVerified: {verified: true},
	WorldSeriesIssued:         {verified: true, series: true},
	WorldRegistrationSpare:    {spare: true},
	WorldLinkNone:             {others: true},
	WorldLinkProposedToCaller: {verified: true, others: true, link: "proposed"},
	WorldLinkAccepted:         {verified: true, others: true, link: "accepted"},
	WorldLinkActive:           {verified: true, others: true, link: "active"},
	WorldLinkSuspended:        {verified: true, others: true, link: "suspended"},
	WorldLinkAmendmentPending: {verified: true, others: true, link: "amendment"},
}

func asTenant(ctx context.Context, t ids.ID) context.Context {
	return tenancy.With(ctx, tenancy.Context{TenantID: t, ActorID: "seed"})
}

// ApplyWorld stores the named example world into an empty repository. The caller empties the target
// first (the double's Store.Reset; a TRUNCATE for Postgres).
func ApplyWorld(ctx context.Context, repo repository.Repository, world string) error {
	spec, ok := worldSpecs[world]
	if !ok {
		return fmt.Errorf("seed: unknown example world %q", world)
	}
	if world == WorldEmpty {
		return nil
	}
	if spec.others {
		if err := applyRestaurants(ctx, repo); err != nil {
			return err
		}
	}
	if err := applyHospitality(ctx, repo, spec); err != nil {
		return err
	}
	if spec.link != "" {
		return applyLink(ctx, repo, spec.link)
	}
	return nil
}

// applyHospitality stores organisation 4a60: trial, default brand 4a6a, outlet 4a61 and registration 4a6b.
func applyHospitality(ctx context.Context, repo repository.Repository, spec worldSpec) error {
	return repo.InTx(asTenant(ctx, Org), func(c context.Context, tx repository.Tx) error {
		if err := tx.InsertOrg(c, model.Org{ID: Org, Name: exampleOrgName, Status: model.OrgTrial, DataPlacement: "primary", CreatedAt: At, UpdatedAt: At},
			model.Settings{OfflineWindowDays: 7, IdleLockMinutes: 5, UpdatedAt: At}); err != nil {
			return err
		}
		if err := tx.InsertBrand(c, model.Brand{ID: Brand, Name: exampleOrgName, IsDefault: true, CreatedAt: At}); err != nil {
			return err
		}
		reg := model.Registration{ID: Registration, LegalName: exampleOrgName + " Pvt Ltd", Regime: model.RegimeRegular, GSTIN: ExampleGSTIN, StateCode: "33",
			Verification: model.Unverified, Status: model.RegOpen, CreatedAt: At}
		verifiedAt := At.Add(5 * time.Minute)
		if spec.verified {
			reg.Verification, reg.Method, reg.VerifiedAt, reg.VerifiedBy = model.Verified, "certificate_seen", &verifiedAt, VendorUser
		}
		if err := tx.InsertRegistration(c, reg); err != nil {
			return err
		}
		if err := tx.InsertOutlet(c, model.Outlet{ID: Outlet, BrandID: Brand, Name: "Main Restaurant", Kind: "restaurant", Status: model.OutletActive,
			StateCode: "33", TaxRegistrationID: Registration, CreatedAt: At}); err != nil {
			return err
		}
		if spec.spare {
			if err := tx.InsertRegistration(c, model.Registration{ID: spareRegistration, LegalName: exampleOrgName + " Karnataka Pvt Ltd", Regime: model.RegimeRegular,
				GSTIN: karnatakaGSTIN, StateCode: "29", Verification: model.Verified, Method: "certificate_seen", VerifiedAt: &verifiedAt, VerifiedBy: VendorUser,
				Status: model.RegOpen, CreatedAt: At}); err != nil {
				return err
			}
		}
		if !spec.series {
			return nil
		}
		issued := At.Add(10 * time.Minute)
		next := 0
		for _, s := range []struct {
			id      ids.ID
			code    string
			docType string
		}{{Series, "A7K", "tax_invoice"}, {CreditNoteSeries, "A7M", "credit_note"}} {
			idx := model.IndexOf(s.code)
			if err := tx.InsertSeries(c, model.Series{ID: s.id, TaxRegistrationID: Registration, GSTIN: ExampleGSTIN, Code: s.code, CodeIndex: idx,
				DocType: s.docType, OutletID: Outlet, DeviceID: Device, Status: model.SeriesActive, IssuedAt: issued}); err != nil {
				return err
			}
			next = max(next, idx+1)
		}
		return tx.SetCodeIndex(c, Registration, next, issued)
	})
}

// applyRestaurants stores organisation 4a63, the other party of the link worlds.
func applyRestaurants(ctx context.Context, repo repository.Repository) error {
	return repo.InTx(asTenant(ctx, Restaurants), func(c context.Context, tx repository.Tx) error {
		if err := tx.InsertOrg(c, model.Org{ID: Restaurants, Name: "Example Restaurants", Status: model.OrgTrial, DataPlacement: "primary", CreatedAt: At, UpdatedAt: At},
			model.Settings{OfflineWindowDays: 7, IdleLockMinutes: 5, UpdatedAt: At}); err != nil {
			return err
		}
		if err := tx.InsertBrand(c, model.Brand{ID: restaurantsBrand, Name: "Example Restaurants", IsDefault: true, CreatedAt: At}); err != nil {
			return err
		}
		verified := At.Add(5 * time.Minute)
		if err := tx.InsertRegistration(c, model.Registration{ID: restaurantsReg, LegalName: "Example Restaurants LLP", Regime: model.RegimeRegular, GSTIN: restaurantsGSTIN,
			StateCode: "33", Verification: model.Verified, Method: "certificate_seen", VerifiedAt: &verified, VerifiedBy: VendorUser, Status: model.RegOpen, CreatedAt: At}); err != nil {
			return err
		}
		return tx.InsertOutlet(c, model.Outlet{ID: RestaurantsOutlet, BrandID: restaurantsBrand, Name: "Riverside", Kind: "restaurant", Status: model.OutletActive,
			StateCode: "33", TaxRegistrationID: restaurantsReg, CreatedAt: At})
	})
}

// applyLink stores link 4a62 from the proposer 4a60 to the acceptor 4a63 in the named state: proposed
// (invitation unused), accepted, active, suspended (by the proposer) or active with a pending amendment.
func applyLink(ctx context.Context, repo repository.Repository, state string) error {
	accepted, confirmed := At.Add(5*time.Minute), At.Add(15*time.Minute)
	l := model.Link{ID: LinkID, Template: "on_premises_outlet", Status: model.LinkProposed, ProposerID: Org, ProposerOutlets: []ids.ID{Outlet},
		ProposerParty: model.Party{DisplayName: exampleOrgName, Registrations: []model.PartyRegistration{{LegalName: exampleOrgName + " Pvt Ltd", GSTIN: ExampleGSTIN, StateCode: "33", GSTINVerified: true}}},
		ProposedAt:    At, ExpiresAt: At.Add(14 * 24 * time.Hour),
		Grants: []model.Grant{{ID: GrantID, LinkID: LinkID, Scope: "test.echo.read", GrantorSide: model.SideProposer, OutletIDs: []ids.ID{Outlet}, Status: model.GrantPending}}}
	sum := sha256.Sum256([]byte(invitationCode))
	hash := sum[:]
	acts := []model.Activity{{ID: activityProposed, LinkID: LinkID, OccurredAt: At, Side: model.SideProposer, ActorID: Owner, Action: "link.proposed", Outcome: "succeeded"}}
	if state != "proposed" {
		l.Status, l.AcceptorID, l.AcceptorOutlets, l.AcceptedAt = model.LinkAccepted, Restaurants, []ids.ID{RestaurantsOutlet}, &accepted
		l.AcceptorParty = model.Party{DisplayName: "Example Restaurants", Registrations: []model.PartyRegistration{{LegalName: "Example Restaurants LLP", GSTIN: restaurantsGSTIN, StateCode: "33", GSTINVerified: true}}}
		acts = append(acts, model.Activity{ID: activityAccepted, LinkID: LinkID, OccurredAt: accepted, Side: model.SideAcceptor, ActorID: Owner, Action: "link.accepted", Outcome: "succeeded"})
	}
	if state != "proposed" && state != "accepted" {
		l.Status, l.ConfirmedAt = model.LinkActive, &confirmed
		l.Grants[0].Status = model.GrantActive
		acts = append(acts, model.Activity{ID: activityConfirmed, LinkID: LinkID, OccurredAt: confirmed, Side: model.SideProposer, ActorID: Owner, Action: "link.confirmed", Outcome: "succeeded"})
	}
	if state == "suspended" {
		l.Status, l.SuspendedBy = model.LinkSuspended, Org
		acts = append(acts, model.Activity{ID: activitySuspended, LinkID: LinkID, OccurredAt: confirmed.Add(10 * time.Minute), Side: model.SideProposer, ActorID: Owner, Action: "link.suspended", Outcome: "succeeded"})
	}
	return repo.InTx(asTenant(ctx, Org), func(c context.Context, tx repository.Tx) error {
		if err := tx.InsertLink(c, l, hash, l.ExpiresAt); err != nil {
			return err
		}
		if state != "proposed" {
			if err := tx.SaveLink(c, l); err != nil {
				return err
			}
			if used, err := tx.UseInvite(c, hash, accepted); err != nil || !used {
				return fmt.Errorf("seed: invitation not used: %v", err)
			}
		}
		for _, a := range acts {
			if err := tx.AppendActivity(c, a); err != nil {
				return err
			}
		}
		if state == "amendment" {
			return tx.InsertAmendment(c, model.Amendment{ID: AmendmentID, LinkID: LinkID, ProposedBySide: model.SideProposer, Status: "pending", CreatedAt: At,
				Add: []model.Grant{{ID: amendmentGrantID, LinkID: LinkID, Scope: "test.echo.post", GrantorSide: model.SideProposer, Status: model.GrantPending}}})
		}
		return nil
	})
}
