package service

import (
	"context"
	"errors"
	"strings"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/gstin"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/states"
	"github.com/hostryhq/svc-tenancy/repository"
)

// RegistrationInput is a create request. GSTIN is as typed; the service normalises it.
type RegistrationInput struct {
	ID        ids.ID
	LegalName string
	Regime    string
	GSTIN     string
	StateCode string
}

// validateRegistration checks the GSTIN rules (ADR-0003 A1.1 item 1) and returns the normalised GSTIN.
func validateRegistration(field string, regime, rawGSTIN, state string) (string, error) {
	switch regime {
	case model.RegimeRegular, model.RegimeComposition, model.RegimeUnregistered:
	default:
		return "", model.Invalid(field+"regime", "unknown", "regime must be regular, composition or unregistered")
	}
	if !states.Valid(state) {
		return "", model.Invalid(field+"state_code", "unknown_state", "state is not in the reference table")
	}
	n := gstin.Normalise(rawGSTIN)
	if regime == model.RegimeUnregistered {
		if n != "" {
			return "", model.Invalid(field+"gstin", "gstin_forbidden", "an unregistered registration takes no GSTIN")
		}
		return "", nil
	}
	if n == "" {
		return "", model.Invalid(field+"gstin", "gstin_required", "a GSTIN is required")
	}
	if code := gstin.Validate(n); code != "" {
		return "", model.Invalid(field+"gstin", code, "the GSTIN is not valid")
	}
	if gstin.State(n) != state {
		return "", model.Invalid(field+"gstin", "gstin_state_mismatch", "state code does not match the GSTIN")
	}
	return n, nil
}

// ListRegistrations pages registrations with their verification state.
func (s *Service) ListRegistrations(ctx context.Context, after ids.ID, limit int) (Page[model.Registration], error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.Registration], error) {
		rows, err := tx.ListRegistrations(ctx, after, limit+1)
		return pageOf(rows, limit, func(r model.Registration) ids.ID { return r.ID }), err
	})
}

// GetRegistration returns one registration.
func (s *Service) GetRegistration(ctx context.Context, id ids.ID) (model.Registration, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Registration, error) {
		return tx.GetRegistration(ctx, id)
	})
}

// CreateRegistration creates an unverified registration. A GSTIN claimed by another organisation is
// accepted with the same response as any new one: the tenant never learns who else holds it (A1.1 item 5).
// Only a duplicate inside the organisation is refused.
func (s *Service) CreateRegistration(ctx context.Context, in RegistrationInput) (model.Registration, error) {
	if err := validID("id", in.ID); err != nil {
		return model.Registration{}, err
	}
	name, err := validName("legal_name", in.LegalName, 200)
	if err != nil {
		return model.Registration{}, err
	}
	n, err := validateRegistration("", in.Regime, in.GSTIN, in.StateCode)
	if err != nil {
		return model.Registration{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Registration, error) {
		if cur, err := tx.GetRegistration(ctx, in.ID); err == nil {
			if cur.LegalName == name && cur.Regime == in.Regime && cur.GSTIN == n && cur.StateCode == in.StateCode {
				return cur, nil
			}
			return cur, model.Invalid("id", "already_exists", "a registration with this id already exists")
		}
		r := model.Registration{ID: in.ID, LegalName: name, Regime: in.Regime, GSTIN: n, StateCode: in.StateCode,
			Verification: model.Unverified, Status: model.RegOpen, CreatedAt: s.clock.Now()}
		if err := tx.InsertRegistration(ctx, r); err != nil {
			return r, mapGSTINErr(err)
		}
		s.log.InfoContext(ctx, "tax registration created", "registration_id", r.ID, "regime", r.Regime) // no GSTIN in logs
		return r, nil
	})
}

// mapGSTINErr turns a repository uniqueness error into the tenant-facing validation error.
func mapGSTINErr(err error) error {
	if errors.Is(err, model.ErrDuplicateGSTIN) {
		return model.Invalid("gstin", "gstin_duplicate", "this organisation already holds a registration with this GSTIN")
	}
	return err
}

// UpdateRegistration corrects the legal name or GSTIN while the registration is unverified. After
// verification the legal identity is fixed: a different GSTIN is a new registration (A1.1 item 6).
// A request that changes nothing returns the registration unchanged.
func (s *Service) UpdateRegistration(ctx context.Context, id ids.ID, legalName, rawGSTIN *string) (model.Registration, error) {
	if legalName == nil && rawGSTIN == nil {
		return model.Registration{}, model.Invalid("legal_name", "required", "nothing to change")
	}
	var name string
	if legalName != nil {
		var err error
		if name, err = validName("legal_name", *legalName, 200); err != nil {
			return model.Registration{}, err
		}
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Registration, error) {
		r, err := tx.GetRegistration(ctx, id)
		if err != nil {
			return r, err
		}
		next := r
		if legalName != nil {
			next.LegalName = name
		}
		if rawGSTIN != nil {
			n, err := validateRegistration("", r.Regime, *rawGSTIN, r.StateCode)
			if err != nil {
				return r, err
			}
			next.GSTIN = n
		}
		if next == r {
			return r, nil
		}
		if r.Verification == model.Verified || r.Status != model.RegOpen {
			return r, apperr.New(apperr.Conflict, "registration_fixed", "a verified registration cannot be changed; a different GSTIN is a new registration")
		}
		if err := tx.UpdateRegistration(ctx, next); err != nil {
			return r, mapGSTINErr(err)
		}
		return next, nil
	})
}

// VerifyInput is a vendor verification request.
type VerifyInput struct {
	Method string
	Note   string
}

// VerifyRegistration marks a registration verified (vendor operation, ADR-0003 A1.1 item 4). It is
// idempotent. A GSTIN has at most one verified holder: a second one is 409 gstin_in_use. When earlier
// records held the GSTIN, the allocator starts beyond every code ever issued under it (item 7).
func (s *Service) VerifyRegistration(ctx context.Context, orgID, regID ids.ID, in VerifyInput) (model.Registration, error) {
	switch in.Method {
	case "certificate_seen", "gst_portal_lookup":
	case "other":
		if strings.TrimSpace(in.Note) == "" {
			return model.Registration{}, model.Invalid("note", "required", "a note is required when the method is other")
		}
	default:
		return model.Registration{}, model.Invalid("method", "unknown", "unknown verification method")
	}
	ctx, err := auth.VendorTenant(ctx, "admin.registration.verify", orgID)
	if err != nil {
		return model.Registration{}, err
	}
	auth.SetAuditTarget(ctx, regID.String())
	p, _ := auth.PrincipalFrom(ctx)

	// Find the registration first (to learn its GSTIN) and the floor beyond earlier holders' codes.
	var reg model.Registration
	// The pre-read is service-internal (no audit row): the one audit row belongs to the action below.
	if err := s.internalTx(orgID, func(c context.Context, tx repository.Tx) error {
		var err error
		reg, err = tx.GetRegistration(c, regID)
		return err
	}); err != nil {
		return reg, err
	}
	floor := -1
	if reg.GSTIN != "" && reg.Verification != model.Verified {
		holders, err := s.repo.Directory().Holders(ctx, reg.GSTIN)
		if err != nil {
			return reg, err
		}
		for _, h := range holders {
			if h.TenantID == orgID && h.RegistrationID == regID {
				continue
			}
			m, err := s.maxCodeIndex(ctx, h.TenantID, reg.GSTIN)
			if err != nil {
				return reg, err
			}
			if m > floor {
				floor = m
			}
		}
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Registration, error) {
		r, err := tx.GetRegistration(ctx, regID)
		if err != nil {
			return r, err
		}
		if r.Regime == model.RegimeUnregistered {
			return r, model.Invalid("registration_id", "no_gstin", "an unregistered registration has no GSTIN to verify")
		}
		if r.Verification == model.Verified {
			return r, nil
		}
		if r.Status != model.RegOpen {
			return r, apperr.New(apperr.Conflict, "registration_closed", "the registration is closed")
		}
		now := s.clock.Now()
		r.Verification, r.Method, r.VerifiedAt, r.VerifiedBy = model.Verified, in.Method, &now, p.VendorUser()
		r.RejectedAt, r.RejectedBy, r.RejectedReason = nil, "", "" // a rejected registration whose GSTIN was corrected can be verified
		if err := tx.UpdateRegistration(ctx, r); err != nil {
			if errors.Is(err, model.ErrVerifiedElsewhere) {
				return r, model.ErrGSTINInUse
			}
			return r, err
		}
		if floor >= 0 {
			if err := tx.SetCodeIndex(ctx, r.ID, floor+1, now); err != nil {
				return r, err
			}
		}
		s.log.InfoContext(ctx, "tax registration verified", "registration_id", r.ID, "organisation_id", orgID, "method", in.Method)
		return r, nil
	})
}

// maxCodeIndex reads the highest allocator index issued under gstin in one earlier holder's organisation,
// under that organisation's own context and without the vendor audit hook (it is a service-internal read).
func (s *Service) maxCodeIndex(ctx context.Context, tenant ids.ID, gstin string) (int, error) {
	max := -1
	err := s.internalTx(tenant, func(c context.Context, tx repository.Tx) error {
		var err error
		max, err = tx.MaxCodeIndexForGSTIN(c, gstin)
		return err
	})
	return max, err
}

// checkClosingReason validates the free-text reason of a rejection or closure (10 to 500 characters).
func checkClosingReason(reason string) error {
	if n := len([]rune(strings.TrimSpace(reason))); n < 10 || n > 500 {
		return model.Invalid("reason", "length", "10 to 500 characters")
	}
	return nil
}

// RejectRegistration marks an unverified registration rejected (vendor operation, ADR-0003 A1.1 item 4,
// AC-TEN-20). Only an unverified registration can be rejected: a verified one is 409, as is a closed
// one. Repeating it changes nothing. A rejected registration can get no series; the tenant corrects the
// GSTIN or closes it. The reason stays with the registration row and the vendor audit trail, never in the
// API body or an event.
func (s *Service) RejectRegistration(ctx context.Context, orgID, regID ids.ID, reason string) (model.Registration, error) {
	if err := checkClosingReason(reason); err != nil {
		return model.Registration{}, err
	}
	ctx, err := auth.VendorTenant(ctx, "admin.registration.verify", orgID)
	if err != nil {
		return model.Registration{}, err
	}
	auth.SetAuditTarget(ctx, regID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Registration, error) {
		r, err := tx.GetRegistration(ctx, regID)
		if err != nil {
			return r, err
		}
		switch {
		case r.Verification == model.Rejected:
			return r, nil
		case r.Regime == model.RegimeUnregistered:
			return r, model.Invalid("registration_id", "no_gstin", "an unregistered registration has no GSTIN to reject")
		case r.Verification == model.Verified:
			return r, apperr.New(apperr.Conflict, "registration_verified", "a verified registration cannot be rejected")
		case r.Status != model.RegOpen:
			return r, apperr.New(apperr.Conflict, "registration_closed", "the registration is closed")
		}
		now := s.clock.Now()
		r.Verification, r.RejectedAt, r.RejectedBy, r.RejectedReason = model.Rejected, &now, p.VendorUser(), strings.TrimSpace(reason)
		if err := tx.UpdateRegistration(ctx, r); err != nil {
			return r, err
		}
		s.log.InfoContext(ctx, "tax registration rejected", "registration_id", r.ID, "organisation_id", orgID)
		return r, nil
	})
}

// CloseRegistration closes a registration and every dormant or active series under it (ADR-0003 A1.1
// item 6: a verified GSTIN is fixed, so a different GSTIN is a new registration and the old one is closed).
// While an active outlet still bills under it the answer is 409 registration_in_use. Closing is terminal
// and idempotent. Series are closed, not erased: their codes stay taken, so the GSTIN's allocator floor
// survives a later registration of the same GSTIN. Tenancy keeps no record of a device's unreconciled
// tail (sync does), so every open series closes as plain closed; closeDocumentSeriesUnreconciled marks
// the ones the tenant knows never reconciled.
func (s *Service) CloseRegistration(ctx context.Context, id ids.ID, reason string) (model.Registration, error) {
	if err := checkClosingReason(reason); err != nil {
		return model.Registration{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Registration, error) {
		r, err := tx.GetRegistration(ctx, id)
		if err != nil {
			return r, err
		}
		if r.Status == model.RegClosed {
			return r, nil
		}
		var after ids.ID
		for {
			outlets, err := tx.ListOutlets(ctx, auth.AllOutlets(), "", after, 500)
			if err != nil {
				return r, err
			}
			for _, o := range outlets {
				if o.TaxRegistrationID == id && o.Status == model.OutletActive {
					return r, apperr.New(apperr.Conflict, "registration_in_use", "an active outlet still bills under this registration")
				}
			}
			if len(outlets) < 500 {
				break
			}
			after = outlets[len(outlets)-1].ID
		}
		now := s.clock.Now()
		if _, err := tx.CloseRegistrationSeries(ctx, id, now); err != nil {
			return r, err
		}
		r.Status, r.ClosedAt, r.ClosedReason = model.RegClosed, &now, strings.TrimSpace(reason)
		if err := tx.UpdateRegistration(ctx, r); err != nil {
			return r, err
		}
		s.log.InfoContext(ctx, "tax registration closed", "registration_id", r.ID)
		return r, nil
	})
}
