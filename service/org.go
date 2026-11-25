package service

import (
	"context"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/states"
	"github.com/hostryhq/svc-tenancy/repository"
)

// OutletKinds are the allowed outlet kinds (extensible by spec change).
var OutletKinds = map[string]bool{"restaurant": true, "bakery_outlet": true, "production_unit": true, "stall": true, "property": true, "service_point": true}

// GetOrganisation returns the caller's organisation.
func (s *Service) GetOrganisation(ctx context.Context) (model.Org, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Org, error) { return tx.GetOrg(ctx) })
}

// RenameOrganisation changes the display name and publishes tenancy.organisation.updated.
func (s *Service) RenameOrganisation(ctx context.Context, name string) (model.Org, error) {
	name, err := validName("name", name, 120)
	if err != nil {
		return model.Org{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Org, error) {
		o, err := tx.GetOrg(ctx)
		if err != nil {
			return o, err
		}
		if o.Name == name {
			return o, nil
		}
		now := s.clock.Now()
		o, err = tx.UpdateOrgName(ctx, name, now)
		if err != nil {
			return o, err
		}
		return o, s.publish(ctx, tx, tev.OrganisationUpdated(s.newID(), now, actor(ctx), o.ID, o.Status, "name"))
	})
}

// GetSettings returns the organisation settings.
func (s *Service) GetSettings(ctx context.Context) (model.Settings, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Settings, error) { return tx.GetSettings(ctx) })
}

// UpdateSettings replaces the settings. A change publishes tenancy.organisation.updated so identity
// can put the new offline window and idle lock into the next pack.
func (s *Service) UpdateSettings(ctx context.Context, offlineDays, idleMinutes int) (model.Settings, error) {
	if offlineDays < 1 || offlineDays > 30 {
		return model.Settings{}, model.Invalid("offline_window_days", "out_of_range", "1 to 30")
	}
	if idleMinutes < 1 || idleMinutes > 120 {
		return model.Settings{}, model.Invalid("idle_lock_minutes", "out_of_range", "1 to 120")
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Settings, error) {
		cur, err := tx.GetSettings(ctx)
		if err != nil {
			return cur, err
		}
		if cur.OfflineWindowDays == offlineDays && cur.IdleLockMinutes == idleMinutes {
			return cur, nil
		}
		now := s.clock.Now()
		next := model.Settings{OfflineWindowDays: offlineDays, IdleLockMinutes: idleMinutes, UpdatedAt: now}
		if err := tx.PutSettings(ctx, next); err != nil {
			return next, err
		}
		o, err := tx.GetOrg(ctx)
		if err != nil {
			return next, err
		}
		return next, s.publish(ctx, tx, tev.OrganisationUpdated(s.newID(), now, actor(ctx), o.ID, o.Status, "settings"))
	})
}

// ListStates returns the GST state reference table.
func (s *Service) ListStates() []states.State { return states.All }

// ---- brands ----

// ListBrands pages brands.
func (s *Service) ListBrands(ctx context.Context, after ids.ID, limit int) (Page[model.Brand], error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.Brand], error) {
		rows, err := tx.ListBrands(ctx, after, limit+1)
		return pageOf(rows, limit, func(b model.Brand) ids.ID { return b.ID }), err
	})
}

// GetBrand returns one brand.
func (s *Service) GetBrand(ctx context.Context, id ids.ID) (model.Brand, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Brand, error) { return tx.GetBrand(ctx, id) })
}

// CreateBrand creates a brand. The id is client-generated, so a create with an existing id and the
// same content returns the existing brand (offline clients retry); different content is refused.
func (s *Service) CreateBrand(ctx context.Context, id ids.ID, name string) (model.Brand, error) {
	if err := validID("id", id); err != nil {
		return model.Brand{}, err
	}
	name, err := validName("name", name, 120)
	if err != nil {
		return model.Brand{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Brand, error) {
		if cur, err := tx.GetBrand(ctx, id); err == nil {
			if cur.Name == name && !cur.IsDefault {
				return cur, nil
			}
			return cur, model.Invalid("id", "already_exists", "a brand with this id already exists")
		}
		b := model.Brand{ID: id, Name: name, CreatedAt: s.clock.Now()}
		return b, tx.InsertBrand(ctx, b)
	})
}

// ---- outlets ----

// OutletInput is a create request.
type OutletInput struct {
	ID                ids.ID
	BrandID           ids.ID // "" for the default brand
	Name              string
	Kind              string
	StateCode         string
	TaxRegistrationID ids.ID
	LocatedInOutletID ids.ID
}

// ListOutlets pages the outlets the caller may see: an outlet-scoped member lists only granted outlets.
func (s *Service) ListOutlets(ctx context.Context, kind string, after ids.ID, limit int) (Page[model.Outlet], error) {
	if kind != "" && !OutletKinds[kind] {
		return Page[model.Outlet]{}, model.Invalid("kind", "unknown", "unknown outlet kind")
	}
	scope, err := auth.ScopeFor(ctx, "outlet.read")
	if err != nil {
		return Page[model.Outlet]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.Outlet], error) {
		rows, err := tx.ListOutlets(ctx, scope, kind, after, limit+1)
		return pageOf(rows, limit, func(o model.Outlet) ids.ID { return o.ID }), err
	})
}

// GetOutlet returns one outlet; an out-of-scope outlet is 404.
func (s *Service) GetOutlet(ctx context.Context, id ids.ID) (model.Outlet, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Outlet, error) {
		o, err := tx.GetOutlet(ctx, id)
		if err != nil {
			return o, err
		}
		return o, requireOutlet(ctx, "outlet.read", o.ID)
	})
}

// CreateOutlet creates an outlet under an open registration of the same state.
func (s *Service) CreateOutlet(ctx context.Context, in OutletInput) (model.Outlet, error) {
	if err := validID("id", in.ID); err != nil {
		return model.Outlet{}, err
	}
	name, err := validName("name", in.Name, 120)
	if err != nil {
		return model.Outlet{}, err
	}
	if !OutletKinds[in.Kind] {
		return model.Outlet{}, model.Invalid("kind", "unknown", "unknown outlet kind")
	}
	if !states.Valid(in.StateCode) {
		return model.Outlet{}, model.Invalid("state_code", "unknown_state", "state is not in the reference table")
	}
	for f, v := range map[string]ids.ID{"brand_id": in.BrandID, "tax_registration_id": in.TaxRegistrationID, "located_in_outlet_id": in.LocatedInOutletID} {
		if v != "" {
			if err := validID(f, v); err != nil {
				return model.Outlet{}, err
			}
		}
	}
	// Creating an outlet adds to the organisation: it needs an organisation-wide grant.
	scope, err := auth.ScopeFor(ctx, "outlet.create")
	if err != nil {
		return model.Outlet{}, err
	}
	if !scope.All() {
		return model.Outlet{}, apperrDenied()
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Outlet, error) {
		brand := model.Brand{}
		if in.BrandID != "" {
			brand, err = tx.GetBrand(ctx, in.BrandID)
		} else {
			brand, err = tx.DefaultBrand(ctx)
		}
		if err != nil {
			return model.Outlet{}, err
		}
		reg, err := tx.GetRegistration(ctx, in.TaxRegistrationID)
		if err != nil {
			return model.Outlet{}, err
		}
		if cur, err := tx.GetOutlet(ctx, in.ID); err == nil {
			if cur.Name == name && cur.Kind == in.Kind && cur.StateCode == in.StateCode && cur.BrandID == brand.ID &&
				cur.TaxRegistrationID == in.TaxRegistrationID && cur.LocatedInOutletID == in.LocatedInOutletID {
				return cur, nil
			}
			return cur, model.Invalid("id", "already_exists", "an outlet with this id already exists")
		}
		if reg.Status != model.RegOpen {
			return model.Outlet{}, model.Invalid("tax_registration_id", "registration_closed", "the registration is closed")
		}
		if reg.StateCode != in.StateCode {
			return model.Outlet{}, model.Invalid("state_code", "registration_state_mismatch", "the outlet state differs from the registration state")
		}
		if in.LocatedInOutletID != "" {
			host, err := tx.GetOutlet(ctx, in.LocatedInOutletID)
			if err != nil {
				return model.Outlet{}, err
			}
			if host.LocatedInOutletID != "" {
				return model.Outlet{}, model.Invalid("located_in_outlet_id", "too_deep", "an outlet can be located in an outlet that is not itself located in another")
			}
		}
		o := model.Outlet{ID: in.ID, BrandID: brand.ID, Name: name, Kind: in.Kind, Status: model.OutletActive, StateCode: in.StateCode,
			TaxRegistrationID: in.TaxRegistrationID, LocatedInOutletID: in.LocatedInOutletID, CreatedAt: s.clock.Now()}
		if err := tx.InsertOutlet(ctx, o); err != nil {
			return o, err
		}
		tid, _ := tenantOf(ctx)
		return o, s.publishOutlet(ctx, tx, "tenancy.outlet.created", tid, o)
	})
}

// UpdateOutlet renames an outlet or changes its status. A brand_id other than the current one is refused.
func (s *Service) UpdateOutlet(ctx context.Context, id ids.ID, name, status *string, brandID *ids.ID) (model.Outlet, error) {
	if name == nil && status == nil && brandID == nil {
		return model.Outlet{}, model.Invalid("name", "required", "nothing to change")
	}
	var newName string
	if name != nil {
		var err error
		if newName, err = validName("name", *name, 120); err != nil {
			return model.Outlet{}, err
		}
	}
	if status != nil && *status != model.OutletActive && *status != model.OutletInactive {
		return model.Outlet{}, model.Invalid("status", "unknown", "status must be active or inactive")
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Outlet, error) {
		o, err := tx.GetOutlet(ctx, id)
		if err != nil {
			return o, err
		}
		if err := requireOutlet(ctx, "outlet.manage", o.ID); err != nil {
			return o, err
		}
		// An outlet stays in the brand it was created under (ADR-0014 A3.4 item 4).
		if brandID != nil && *brandID != o.BrandID {
			return o, apperr.New(apperr.Conflict, "outlet_brand_fixed", "an outlet cannot change brand")
		}
		changed := false
		if name != nil && o.Name != newName {
			o.Name, changed = newName, true
		}
		if status != nil && o.Status != *status {
			o.Status, changed = *status, true
		}
		if !changed {
			return o, nil
		}
		if err := tx.UpdateOutlet(ctx, o); err != nil {
			return o, err
		}
		tid, _ := tenantOf(ctx)
		return o, s.publishOutlet(ctx, tx, "tenancy.outlet.updated", tid, o)
	})
}
