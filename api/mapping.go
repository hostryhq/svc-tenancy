package api

import (
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/service"
)

func orgDTO(o model.Org) gen.Organisation {
	return gen.Organisation{Id: o.ID.String(), Name: o.Name, Status: gen.OrganisationStatus(o.Status), CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
}

func settingsDTO(s model.Settings) gen.OrganisationSettings {
	return gen.OrganisationSettings{OfflineWindowDays: s.OfflineWindowDays, IdleLockMinutes: s.IdleLockMinutes, UpdatedAt: s.UpdatedAt}
}

func brandDTO(b model.Brand) gen.Brand {
	return gen.Brand{Id: b.ID.String(), Name: b.Name, IsDefault: b.IsDefault, CreatedAt: b.CreatedAt}
}

func outletDTO(o model.Outlet) gen.Outlet {
	d := gen.Outlet{Id: o.ID.String(), BrandId: o.BrandID.String(), Name: o.Name, Kind: gen.OutletKind(o.Kind), Status: gen.OutletStatus(o.Status),
		StateCode: o.StateCode, TaxRegistrationId: o.TaxRegistrationID.String(), Timezone: "Asia/Kolkata", CreatedAt: o.CreatedAt}
	if o.LocatedInOutletID != "" {
		v := o.LocatedInOutletID.String()
		d.LocatedInOutletId = &v
	}
	return d
}

func regDTO(r model.Registration) gen.TaxRegistration {
	d := gen.TaxRegistration{Id: r.ID.String(), LegalName: r.LegalName, Regime: gen.TaxRegime(r.Regime), StateCode: r.StateCode,
		Status: gen.TaxRegistrationStatus(r.Status), CreatedAt: r.CreatedAt,
		Verification: gen.Verification{State: gen.VerificationState(r.Verification)}}
	if r.GSTIN != "" {
		g := r.GSTIN
		d.Gstin = &g
	}
	if r.Method != "" {
		m := gen.VerificationMethod(r.Method)
		d.Verification.Method = &m
	}
	d.Verification.VerifiedAt = r.VerifiedAt
	d.Verification.RejectedAt = r.RejectedAt
	return d
}

func seriesDTO(s model.Series) gen.DocumentSeries {
	return gen.DocumentSeries{Id: s.ID.String(), TaxRegistrationId: s.TaxRegistrationID.String(), Code: s.Code, DocType: gen.DocType(s.DocType),
		OutletId: s.OutletID.String(), DeviceId: s.DeviceID.String(), Status: gen.DocumentSeriesStatus(s.Status), IssuedAt: s.IssuedAt, ClosedAt: s.ClosedAt}
}

func idStrings(in []ids.ID) *[]string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = v.String()
	}
	return &out
}

func toIDs(in *[]string) []ids.ID {
	if in == nil {
		return nil
	}
	out := make([]ids.ID, len(*in))
	for i, v := range *in {
		out[i] = ids.ID(v)
	}
	return out
}

// linkDTO renders a link as seen by viewer: role, grant directions and the counterparty are relative to it.
func linkDTO(l model.Link, viewer ids.ID, now time.Time) gen.Link {
	role, other, otherParty := gen.LinkRoleProposer, l.AcceptorID, l.AcceptorParty
	mySide := model.SideProposer
	if viewer == l.AcceptorID && viewer != l.ProposerID {
		role, other, otherParty = gen.LinkRoleAcceptor, l.ProposerID, l.ProposerParty
		mySide = model.SideAcceptor
	}
	d := gen.Link{Id: l.ID.String(), Template: gen.LinkTemplate(l.Template), Status: gen.LinkStatus(service.EffectiveStatus(l, now)), Role: role,
		Grants: []gen.LinkGrant{}, ProposedAt: l.ProposedAt, ExpiresAt: l.ExpiresAt, AcceptedAt: l.AcceptedAt, ConfirmedAt: l.ConfirmedAt}
	if other != "" {
		o := other.String()
		d.CounterpartyOrganisationId = &o
		cp := gen.LinkCounterparty{DisplayName: otherParty.DisplayName, Registrations: []gen.CounterpartyRegistration{}}
		for _, r := range otherParty.Registrations {
			cr := gen.CounterpartyRegistration{LegalName: r.LegalName, StateCode: r.StateCode, GstinVerified: r.GSTINVerified}
			if r.GSTIN != "" {
				g := r.GSTIN
				cr.Gstin = &g
			}
			cp.Registrations = append(cp.Registrations, cr)
		}
		d.Counterparty = &cp
	}
	for _, g := range l.Grants {
		d.Grants = append(d.Grants, gen.LinkGrant{Id: g.ID.String(), Scope: g.Scope, Direction: direction(g.GrantorSide, mySide),
			OutletIds: idStrings(g.OutletIDs), Status: gen.LinkGrantStatus(g.Status)})
	}
	return d
}

func direction(grantorSide, mySide string) gen.LinkGrantDirection {
	if grantorSide == mySide {
		return gen.LinkGrantDirectionOutbound
	}
	return gen.LinkGrantDirectionInbound
}

func amendmentDTO(a model.Amendment, mySide string) gen.LinkAmendment {
	d := gen.LinkAmendment{Id: a.ID.String(), LinkId: a.LinkID.String(), Status: gen.LinkAmendmentStatus(a.Status), CreatedAt: a.CreatedAt,
		AddGrants: []gen.GrantInput{}}
	for _, g := range a.Add {
		dir := gen.GrantInputDirectionInbound
		if g.GrantorSide == mySide {
			dir = gen.GrantInputDirectionOutbound
		}
		d.AddGrants = append(d.AddGrants, gen.GrantInput{Scope: g.Scope, Direction: dir, OutletIds: idStrings(g.OutletIDs)})
	}
	return d
}

func activityDTO(a model.Activity) gen.LinkActivity {
	d := gen.LinkActivity{Id: a.ID.String(), LinkId: a.LinkID.String(), OccurredAt: a.OccurredAt, Side: gen.LinkActivitySide(a.Side), Action: a.Action,
		Outcome: gen.LinkActivityOutcome(a.Outcome)}
	if a.Scope != "" {
		s := a.Scope
		d.Scope = &s
	}
	if a.ObjectRef != "" {
		r := a.ObjectRef
		d.ObjectRef = &r
	}
	return d
}

func dirOrgDTO(o model.DirOrg) gen.DirectoryOrganisation {
	return gen.DirectoryOrganisation{Id: o.ID.String(), Name: o.Name, Status: gen.OrganisationStatus(o.Status), OutletCount: o.OutletCount, CreatedAt: o.CreatedAt}
}

func detailDTO(d service.OrgDetail) gen.OrganisationDetail {
	out := gen.OrganisationDetail{Organisation: dirOrgDTO(d.Org), Brands: []gen.DirectoryBrand{}, Outlets: []gen.DirectoryOutlet{}, Registrations: []gen.DirectoryRegistration{}}
	for _, b := range d.Brands {
		out.Brands = append(out.Brands, gen.DirectoryBrand{Id: b.ID.String(), Name: b.Name, IsDefault: b.IsDefault})
	}
	for _, o := range d.Outlets {
		out.Outlets = append(out.Outlets, gen.DirectoryOutlet{Id: o.ID.String(), Name: o.Name, Kind: gen.OutletKind(o.Kind), StateCode: o.StateCode, Status: gen.OutletStatus(o.Status)})
	}
	for _, r := range d.Registrations {
		dr := gen.DirectoryRegistration{Id: r.ID.String(), LegalName: r.LegalName, Regime: gen.TaxRegime(r.Regime), StateCode: r.StateCode,
			VerificationState: gen.DirectoryRegistrationVerificationState(r.Verification), Status: gen.DirectoryRegistrationStatus(r.Status)}
		if r.GSTIN != "" {
			g := r.GSTIN
			dr.Gstin = &g
		}
		out.Registrations = append(out.Registrations, dr)
	}
	return out
}
