package service

import (
	"context"
	"regexp"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

var bookCodePattern = regexp.MustCompile(`^[A-Z0-9]{1,6}$`)

// RegisterBookInput is a paper bill book to register as an entry-only series.
type RegisterBookInput struct {
	TaxRegistrationID ids.ID
	Code              string
	OutletIDs         []ids.ID
}

// RegisterManualBook registers a paper bill book as an entry-only series of kind manual_book (ADR-0014
// A1.6 item 4, provisional: P2B-6). The series is issued, so the foreign-series rule of ADR-0003 A1.5 holds
// unchanged; billing devices of the named outlets receive it entry-only, and a number typed from the book is
// never allocated by Hostry. The code must be unique under the GSTIN against device series codes (409
// series_code_in_use); the allocator never issues a code a book owns. Strong sessions only (owner).
func (s *Service) RegisterManualBook(ctx context.Context, in RegisterBookInput) (model.ManualBook, error) {
	if err := validID("tax_registration_id", in.TaxRegistrationID); err != nil {
		return model.ManualBook{}, err
	}
	if !bookCodePattern.MatchString(in.Code) {
		return model.ManualBook{}, model.Invalid("code", "pattern", "1 to 6 capitals or digits")
	}
	if n := len(in.OutletIDs); n < 1 || n > 50 {
		return model.ManualBook{}, model.Invalid("outlet_ids", "length", "1 to 50 outlets")
	}
	seen := map[ids.ID]bool{}
	for _, o := range in.OutletIDs {
		if err := validID("outlet_ids", o); err != nil {
			return model.ManualBook{}, err
		}
		if seen[o] {
			return model.ManualBook{}, model.Invalid("outlet_ids", "repeated", "an outlet is listed twice")
		}
		seen[o] = true
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.ManualBook, error) {
		reg, err := tx.GetRegistration(ctx, in.TaxRegistrationID)
		if err != nil {
			return model.ManualBook{}, err
		}
		// A paper book is typed, never allocated by Hostry, so it needs an open registration but not a verified
		// one (the contract's own example registers a book under the unverified registration of the base world).
		if reg.Status != model.RegOpen || reg.Verification == model.Rejected {
			return model.ManualBook{}, model.ErrRegUnverified
		}
		for _, id := range in.OutletIDs {
			o, err := tx.GetOutlet(ctx, id)
			if err != nil {
				return model.ManualBook{}, err
			}
			if err := requireOutlet(ctx, "series.manual_book.register", o.ID); err != nil {
				return model.ManualBook{}, err
			}
			if o.TaxRegistrationID != reg.ID {
				return model.ManualBook{}, model.Invalid("outlet_ids", "outlet_registration_mismatch", "an outlet bills under another registration")
			}
		}
		// Serialise with series issuance: both read and extend the registration's code space.
		if _, err := tx.NextCodeIndex(ctx, reg.ID); err != nil {
			return model.ManualBook{}, err
		}
		b := model.ManualBook{ID: s.newID(), TaxRegistrationID: reg.ID, GSTIN: reg.GSTIN, Code: in.Code, OutletIDs: in.OutletIDs, Status: model.BookActive, CreatedAt: s.clock.Now()}
		if err := tx.InsertManualBook(ctx, b); err != nil {
			return model.ManualBook{}, err
		}
		return b, nil
	})
}

// ListManualBooks pages the registered bill books that serve at least one outlet in the caller's scope.
func (s *Service) ListManualBooks(ctx context.Context, after ids.ID, limit int) (Page[model.ManualBook], error) {
	scope, err := auth.ScopeFor(ctx, "series.read")
	if err != nil {
		return Page[model.ManualBook]{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.ManualBook], error) {
		var out []model.ManualBook
		cursor := after
		for len(out) < limit+1 {
			rows, err := tx.ListManualBooks(ctx, cursor, limit+1)
			if err != nil {
				return Page[model.ManualBook]{}, err
			}
			for _, b := range rows {
				if scope.All() || anyInScope(scope, b.OutletIDs) {
					out = append(out, b)
				}
			}
			if len(rows) < limit+1 {
				break
			}
			cursor = rows[len(rows)-1].ID
		}
		return pageOf(out, limit, func(b model.ManualBook) ids.ID { return b.ID }), nil
	})
}

func anyInScope(scope auth.OutletScope, outlets []ids.ID) bool {
	for _, o := range outlets {
		if scope.Contains(o.String()) {
			return true
		}
	}
	return false
}
