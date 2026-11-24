package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/states"
	"github.com/hostryhq/svc-tenancy/repository"
)

// internalTx runs fn for a tenant on behalf of the service itself (cross-tenant loops, floors for the
// GSTIN allocator). It carries no principal, so no vendor audit row is written, and it sets the tenant
// context for each tenant: it never bypasses row-level security.
func (s *Service) internalTx(tenant ids.ID, fn func(ctx context.Context, tx repository.Tx) error) error {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c = tenancy.With(c, tenancy.Context{TenantID: tenant, ActorID: "tenancy"})
	return s.repo.InTx(c, fn)
}

// DirectoryQuery is a vendor directory request.
type DirectoryQuery struct {
	Q               string
	Status          string
	OrganisationIDs []ids.ID
	After           ids.ID
	Limit           int
}

// ListDirectory lists and searches the vendor directory (newest first, stable under inserts). It reads
// the directory tables through the tenancy_vendor role; a partial GSTIN never matches.
func (s *Service) ListDirectory(ctx context.Context, q DirectoryQuery) (Page[model.DirOrg], error) {
	if len(q.Q) > 200 {
		return Page[model.DirOrg]{}, model.Invalid("q", "length", "at most 200 characters")
	}
	switch q.Status {
	case "", "trial", "active", "suspended", "closed":
	default:
		return Page[model.DirOrg]{}, model.Invalid("status", "unknown", "unknown status")
	}
	if len(q.OrganisationIDs) > 100 {
		return Page[model.DirOrg]{}, model.Invalid("organisation_ids", "too_many", "at most 100 ids")
	}
	rows, err := s.repo.Directory().List(ctx, repository.DirectoryFilter{
		Q: strings.TrimSpace(q.Q), Status: q.Status, OrganisationIDs: q.OrganisationIDs, After: q.After, Limit: q.Limit + 1})
	if err != nil {
		return Page[model.DirOrg]{}, err
	}
	return pageOf(rows, q.Limit, func(o model.DirOrg) ids.ID { return o.ID }), nil
}

// OrgDetail is the structure-only directory detail of one organisation.
type OrgDetail struct {
	Org           model.DirOrg
	Brands        []model.Brand
	Outlets       []model.Outlet
	Registrations []model.Registration
}

// GetOrgDetail returns an organisation's structure for a vendor user: no members, devices or settings.
// It runs as the ordinary application role under that organisation's context (ADR-0013 V3 item 4).
func (s *Service) GetOrgDetail(ctx context.Context, orgID ids.ID) (OrgDetail, error) {
	ctx, err := auth.VendorTenant(ctx, "admin.org.read", orgID)
	if err != nil {
		return OrgDetail{}, err
	}
	auth.SetAuditTarget(ctx, orgID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (OrgDetail, error) {
		return loadDetail(ctx, tx)
	})
}

func loadDetail(ctx context.Context, tx repository.Tx) (OrgDetail, error) {
	var d OrgDetail
	o, err := tx.GetOrg(ctx)
	if err != nil {
		return d, err
	}
	if d.Brands, err = tx.ListBrands(ctx, "", 1000); err != nil {
		return d, err
	}
	if d.Outlets, err = tx.ListOutlets(ctx, auth.AllOutlets(), "", "", 1000); err != nil {
		return d, err
	}
	if d.Registrations, err = tx.ListRegistrations(ctx, "", 1000); err != nil {
		return d, err
	}
	d.Org = model.DirOrg{ID: o.ID, Name: o.Name, Status: o.Status, OutletCount: len(d.Outlets), CreatedAt: o.CreatedAt}
	return d, nil
}

// CreateOrgInput is the vendor onboarding request. Key and Fingerprint come from the Idempotency-Key.
type CreateOrgInput struct {
	Name         string
	OutletName   string
	OutletKind   string
	OutletState  string
	LegalName    string
	Regime       string
	GSTIN        string
	VerifyMethod string // "" leaves the registration unverified
	VerifyNote   string
	Key          string
	Fingerprint  string
}

// OrgCreated is what creation returns and a replay returns again.
type OrgCreated struct {
	Org          model.Org
	Brand        model.Brand
	Outlet       model.Outlet
	Registration model.Registration
}

// CreateOrganisation creates the organisation, its hidden default brand, the first outlet, the tax
// registration the outlet bills under and the directory rows, in one transaction (ADR-0013 V4). It is
// idempotent on the vendor's key: the same key and body return the same result and create nothing; a
// different body is a 422. No series, entitlements or owner exist afterwards.
func (s *Service) CreateOrganisation(ctx context.Context, in CreateOrgInput) (OrgCreated, error) {
	orgName, err := validName("name", in.Name, 120)
	if err != nil {
		return OrgCreated{}, err
	}
	outName, err := validName("outlet.name", in.OutletName, 120)
	if err != nil {
		return OrgCreated{}, err
	}
	legal, err := validName("tax_registration.legal_name", in.LegalName, 200)
	if err != nil {
		return OrgCreated{}, err
	}
	if !OutletKinds[in.OutletKind] {
		return OrgCreated{}, model.Invalid("outlet.kind", "unknown", "unknown outlet kind")
	}
	if !states.Valid(in.OutletState) {
		return OrgCreated{}, model.Invalid("outlet.state_code", "unknown_state", "state is not in the reference table")
	}
	n, err := validateRegistration("tax_registration.", in.Regime, in.GSTIN, in.OutletState)
	if err != nil {
		return OrgCreated{}, err
	}
	switch in.VerifyMethod {
	case "", "certificate_seen", "gst_portal_lookup":
	case "other":
		if strings.TrimSpace(in.VerifyNote) == "" {
			return OrgCreated{}, model.Invalid("verification.note", "required", "a note is required when the method is other")
		}
	default:
		return OrgCreated{}, model.Invalid("verification.method", "unknown", "unknown verification method")
	}
	if in.VerifyMethod != "" {
		if in.Regime == model.RegimeUnregistered {
			return OrgCreated{}, model.Invalid("verification", "no_gstin", "an unregistered registration has no GSTIN to verify")
		}
		if _, err := auth.ScopeFor(ctx, "admin.registration.verify"); err != nil {
			return OrgCreated{}, err // verification in the same submit needs the permission
		}
	}
	p, _ := auth.PrincipalFrom(ctx)
	sum := sha256.Sum256([]byte(p.VendorUser() + "\x00" + in.Key))
	keyHash := hex.EncodeToString(sum[:])

	for attempt := 0; attempt < 2; attempt++ {
		if id, fp, found, err := s.repo.Directory().FindCreation(ctx, keyHash); err != nil {
			return OrgCreated{}, err
		} else if found {
			if fp != in.Fingerprint {
				return OrgCreated{}, idempotency.ErrMismatch
			}
			return s.replayCreation(ctx, id)
		}
		var floor = -1
		if n != "" {
			holders, err := s.repo.Directory().Holders(ctx, n)
			if err != nil {
				return OrgCreated{}, err
			}
			for _, h := range holders {
				if h.Verified && !h.Closed {
					return OrgCreated{}, model.ErrGSTINInUse
				}
				if in.VerifyMethod != "" {
					m, err := s.maxCodeIndex(ctx, h.TenantID, n)
					if err != nil {
						return OrgCreated{}, err
					}
					floor = max(floor, m)
				}
			}
		}
		tid := s.newID()
		tctx, err := auth.VendorTenant(ctx, "admin.org.create", tid)
		if err != nil {
			return OrgCreated{}, err
		}
		auth.SetAuditTarget(tctx, tid.String())
		var out OrgCreated
		err = s.repo.InTx(tctx, func(c context.Context, tx repository.Tx) error {
			now := s.clock.Now()
			out.Org = model.Org{ID: tid, Name: orgName, Status: model.OrgTrial, DataPlacement: "primary", CreatedAt: now, UpdatedAt: now,
				CreateKeyHash: keyHash, CreateFingerprint: in.Fingerprint}
			if err := tx.InsertOrg(c, out.Org, model.Settings{OfflineWindowDays: 7, IdleLockMinutes: 5, UpdatedAt: now}); err != nil {
				return err
			}
			out.Brand = model.Brand{ID: s.newID(), Name: orgName, IsDefault: true, CreatedAt: now}
			if err := tx.InsertBrand(c, out.Brand); err != nil {
				return err
			}
			out.Registration = model.Registration{ID: s.newID(), LegalName: legal, Regime: in.Regime, GSTIN: n, StateCode: in.OutletState,
				Verification: model.Unverified, Status: model.RegOpen, CreatedAt: now}
			if in.VerifyMethod != "" {
				out.Registration.Verification, out.Registration.Method = model.Verified, in.VerifyMethod
				out.Registration.VerifiedAt, out.Registration.VerifiedBy = &now, p.VendorUser()
			}
			if err := tx.InsertRegistration(c, out.Registration); err != nil {
				if errors.Is(err, model.ErrVerifiedElsewhere) {
					return model.ErrGSTINInUse
				}
				return err
			}
			if floor >= 0 {
				if err := tx.SetCodeIndex(c, out.Registration.ID, floor+1, now); err != nil {
					return err
				}
			}
			out.Outlet = model.Outlet{ID: s.newID(), BrandID: out.Brand.ID, Name: outName, Kind: in.OutletKind, Status: model.OutletActive,
				StateCode: in.OutletState, TaxRegistrationID: out.Registration.ID, CreatedAt: now}
			if err := tx.InsertOutlet(c, out.Outlet); err != nil {
				return err
			}
			if err := s.publish(c, tx, tev.OrganisationCreated(s.newID(), now, p.VendorUser(), tid, out.Org.Status)); err != nil {
				return err
			}
			return s.publish(c, tx, outletEvent(s, "tenancy.outlet.created", c, tid, out.Outlet))
		})
		if errors.Is(err, model.ErrCreateKeyTaken) {
			continue // a concurrent request with the same key won; the next pass replays it
		}
		if err != nil {
			return OrgCreated{}, err
		}
		s.log.InfoContext(ctx, "organisation created", "organisation_id", tid, "outlet_id", out.Outlet.ID, "registration_id", out.Registration.ID, "vendor_user", p.VendorUser())
		return out, nil
	}
	return OrgCreated{}, idempotency.ErrInProgress
}

func (s *Service) replayCreation(ctx context.Context, orgID ids.ID) (OrgCreated, error) {
	tctx, err := auth.VendorTenant(ctx, "admin.org.create", orgID)
	if err != nil {
		return OrgCreated{}, err
	}
	auth.SetAuditTarget(tctx, orgID.String())
	var out OrgCreated
	err = s.repo.InTx(tctx, func(c context.Context, tx repository.Tx) error {
		d, err := loadDetail(c, tx)
		if err != nil {
			return err
		}
		if out.Org, err = tx.GetOrg(c); err != nil {
			return err
		}
		if out.Brand, err = tx.DefaultBrand(c); err != nil {
			return err
		}
		if len(d.Outlets) == 0 || len(d.Registrations) == 0 {
			return model.ErrNotFound
		}
		out.Outlet, out.Registration = d.Outlets[0], d.Registrations[0]
		for _, r := range d.Registrations {
			if r.ID == out.Outlet.TaxRegistrationID {
				out.Registration = r
			}
		}
		return nil
	})
	return out, err
}
