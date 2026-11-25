package service

import (
	"context"
	"errors"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// Series registry constants (ADR-0003 section 1a, ADR-0002 S7).
const (
	// DailyCap is the most codes issued per registration per IST day.
	DailyCap = 200
	// CodeSpace is 32^3: three characters of a 32-symbol alphabet (capitals and digits without I, O, 0, 1).
	CodeSpace = model.CodeSpace
	// collisionJump is how far the allocator moves when the (GSTIN, code) index refuses a code.
	collisionJump = 200
)

// IssueInput is the identity-only series issuance request.
type IssueInput struct {
	OutletID ids.ID
	DeviceID ids.ID
	DocTypes []string
	State    string // active (pos) or dormant (waiter)
}

func istDay(now time.Time) (from, to time.Time) {
	n := now.In(clock.IST)
	from = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, clock.IST)
	return from.UTC(), from.AddDate(0, 0, 1).UTC()
}

// IssueSeries issues series codes for a device under the registration of the stored outlet. The tenant
// is the one named by the service token (identity is the only caller); an outlet of another tenant is
// 404. Codes exist only for a verified registration (kind unregistered needs none). A repeat for the same
// device and outlet returns the same codes. The allocator, the unique indexes and the daily cap all
// sit in one transaction, so concurrent activations never share a code.
func (s *Service) IssueSeries(ctx context.Context, in IssueInput) ([]model.Series, error) {
	if err := validID("outlet_id", in.OutletID); err != nil {
		return nil, err
	}
	if err := validID("device_id", in.DeviceID); err != nil {
		return nil, err
	}
	if in.State != model.SeriesActive && in.State != model.SeriesDormant {
		return nil, model.Invalid("state", "unknown", "state must be active or dormant")
	}
	if len(in.DocTypes) < 1 || len(in.DocTypes) > len(model.DocTypes) {
		return nil, model.Invalid("doc_types", "length", "1 to 4 document types")
	}
	seen := map[string]bool{}
	for _, d := range in.DocTypes {
		known := false
		for _, k := range model.DocTypes {
			known = known || k == d
		}
		if !known || seen[d] {
			return nil, model.Invalid("doc_types", "invalid", "unknown or repeated document type")
		}
		seen[d] = true
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) ([]model.Series, error) {
		o, err := tx.GetOutlet(ctx, in.OutletID)
		if err != nil {
			return nil, err
		}
		reg, err := tx.GetRegistration(ctx, o.TaxRegistrationID)
		if err != nil {
			return nil, err
		}
		if reg.Status != model.RegOpen || (reg.Regime != model.RegimeUnregistered && reg.Verification != model.Verified) {
			return nil, model.ErrRegUnverified
		}
		now := s.clock.Now()
		existing, err := tx.SeriesForDevice(ctx, o.ID, in.DeviceID)
		if err != nil {
			return nil, err
		}
		byType := map[string]model.Series{}
		for _, e := range existing {
			if e.Status == model.SeriesActive || e.Status == model.SeriesDormant {
				byType[e.DocType] = e
			}
		}
		var missing []string
		for _, d := range in.DocTypes {
			if _, ok := byType[d]; !ok {
				missing = append(missing, d)
			}
		}
		if len(missing) > 0 {
			from, to := istDay(now)
			used, err := tx.CountIssued(ctx, reg.ID, from, to)
			if err != nil {
				return nil, err
			}
			if used+len(missing) > DailyCap {
				return nil, model.ErrSeriesCap
			}
			if before, after := used*2/DailyCap, (used+len(missing))*2/DailyCap; after > before {
				s.log.WarnContext(ctx, "series daily cap at 50 percent", "registration_id", reg.ID, "issued_today", used+len(missing))
			}
			next, err := tx.NextCodeIndex(ctx, reg.ID)
			if err != nil {
				return nil, err
			}
			for _, d := range missing {
				sr := model.Series{ID: s.newID(), TaxRegistrationID: reg.ID, GSTIN: reg.GSTIN, DocType: d, OutletID: o.ID,
					DeviceID: in.DeviceID, Status: in.State, IssuedAt: now}
				for attempt := 0; ; attempt++ {
					if next >= CodeSpace {
						return nil, model.ErrSeriesCap
					}
					sr.Code, sr.CodeIndex = model.CodeFor(next), next
					err := tx.InsertSeries(ctx, sr)
					if err == nil {
						next++
						break
					}
					if !errors.Is(err, model.ErrSeriesCollision) || attempt >= 5 {
						return nil, err
					}
					s.log.ErrorContext(ctx, "series code collision; allocator jumps", "registration_id", reg.ID, "alert", true)
					next += collisionJump
				}
				byType[d] = sr
				if err := s.publishSeriesIssued(ctx, tx, sr); err != nil {
					return nil, err
				}
			}
			if err := tx.SetCodeIndex(ctx, reg.ID, next, now); err != nil {
				return nil, err
			}
			if next*4 >= CodeSpace*3 {
				s.log.WarnContext(ctx, "series code space at 75 percent", "registration_id", reg.ID, "next_index", next)
			}
		}
		var out []model.Series
		for _, d := range in.DocTypes {
			sr := byType[d]
			if in.State == model.SeriesActive && sr.Status == model.SeriesDormant { // billing granted later
				sr.Status = model.SeriesActive
				if err := tx.UpdateSeries(ctx, sr); err != nil {
					return nil, err
				}
			}
			out = append(out, sr)
		}
		return out, nil
	})
}

// CloseDeviceSeries closes a device's open series (re-bound, reinstalled, restored or revoked); the
// device gets new codes when it is activated again. It returns the number closed. A device with no
// series in the tenant is 404.
func (s *Service) CloseDeviceSeries(ctx context.Context, deviceID ids.ID, reason string) (int, error) {
	if err := validID("device_id", deviceID); err != nil {
		return 0, err
	}
	switch reason {
	case "rebound", "reinstalled", "restored", "revoked":
	default:
		return 0, model.Invalid("reason", "unknown", "unknown reason")
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (int, error) {
		closed, total, err := tx.CloseDeviceSeries(ctx, deviceID, s.clock.Now())
		if err != nil {
			return 0, err
		}
		if total == 0 {
			return 0, model.ErrNotFound
		}
		return closed, nil
	})
}

// ListSeries pages the series register inside the caller's outlet scope; an outlet filter is intersected
// with the scope and can never widen it.
func (s *Service) ListSeries(ctx context.Context, outletFilter ids.ID, status string, after ids.ID, limit int) (Page[model.Series], error) {
	switch status {
	case "", model.SeriesDormant, model.SeriesActive, model.SeriesClosed, model.SeriesClosedUnreconciled:
	default:
		return Page[model.Series]{}, model.Invalid("status", "unknown", "unknown status")
	}
	scope, err := auth.ScopeFor(ctx, "series.read")
	if err != nil {
		return Page[model.Series]{}, err
	}
	if outletFilter != "" {
		scope = scope.Intersect([]string{outletFilter.String()})
		if scope.Empty() {
			return Page[model.Series]{}, nil
		}
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.Series], error) {
		rows, err := tx.ListSeries(ctx, scope, status, after, limit+1)
		return pageOf(rows, limit, func(x model.Series) ids.ID { return x.ID }), err
	})
}

// GetSeries returns one series of the register; one outside the caller's outlet scope is 404.
func (s *Service) GetSeries(ctx context.Context, id ids.ID) (model.Series, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Series, error) {
		sr, err := tx.GetSeries(ctx, id)
		if err != nil {
			return sr, err
		}
		return sr, requireOutlet(ctx, "series.read", sr.OutletID)
	})
}

// CloseSeriesUnreconciled marks a series closed_unreconciled with a reason, for a device that never
// uploaded its tail. Allowed from any state; repeating it changes nothing.
func (s *Service) CloseSeriesUnreconciled(ctx context.Context, id ids.ID, reason string) (model.Series, error) {
	if n := len([]rune(reason)); n < 10 || n > 500 {
		return model.Series{}, model.Invalid("reason", "length", "10 to 500 characters")
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Series, error) {
		sr, err := tx.GetSeries(ctx, id)
		if err != nil {
			return sr, err
		}
		if err := requireOutlet(ctx, "series.close_unreconciled", sr.OutletID); err != nil {
			return sr, err
		}
		if sr.Status == model.SeriesClosedUnreconciled {
			return sr, nil
		}
		// Offered only while no tail is confirmed: once a device stated its last number the series is
		// reconciled, not abandoned (ADR-0014 A2.5 item 2).
		if sr.TailConfirmedAt != nil {
			return sr, model.ErrTailConfirmed
		}
		now := s.clock.Now()
		sr.Status, sr.ClosedReason = model.SeriesClosedUnreconciled, reason
		if sr.ClosedAt == nil {
			sr.ClosedAt = &now
		}
		if err := tx.UpdateSeries(ctx, sr); err != nil {
			return sr, err
		}
		return sr, s.publishSeriesClosed(ctx, tx, []model.Series{sr}, "closed_unreconciled")
	})
}

func seriesData(sr model.Series) tev.SeriesData {
	return tev.SeriesData{SeriesID: sr.ID, Code: sr.Code, DocType: sr.DocType, TaxRegistrationID: sr.TaxRegistrationID, OutletID: sr.OutletID, DeviceID: sr.DeviceID}
}

// publishSeriesIssued tells billing which codes to expect (ids and codes only).
func (s *Service) publishSeriesIssued(ctx context.Context, tx repository.Tx, sr model.Series) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	return s.publish(ctx, tx, tev.SeriesIssued(s.newID(), s.clock.Now(), actor(ctx), tid, seriesData(sr)))
}

// publishSeriesClosed publishes tenancy.series.closed for each series with its cause.
func (s *Service) publishSeriesClosed(ctx context.Context, tx repository.Tx, rows []model.Series, cause string) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	for _, sr := range rows {
		d := seriesData(sr)
		d.Cause = cause
		if err := s.publish(ctx, tx, tev.SeriesClosed(s.newID(), s.clock.Now(), actor(ctx), tid, d)); err != nil {
			return err
		}
	}
	return nil
}

// AdvanceAllocatorsAfterRestore moves every registration's allocator forward by 200 x (whole days in
// the lost window + 1), so a code forgotten by a restore is never issued again (ADR-0003 section 1a).
// It runs per tenant under that tenant's context and does not bypass row-level security.
func (s *Service) AdvanceAllocatorsAfterRestore(ctx context.Context, lostDays int) (tenants int, err error) {
	if lostDays < 0 {
		return 0, model.Invalid("lost_days", "out_of_range", "must not be negative")
	}
	var after ids.ID
	for {
		batch, err := s.repo.Directory().TenantIDs(ctx, after, 100)
		if err != nil {
			return tenants, err
		}
		for _, t := range batch {
			if err := s.internalTx(t, func(c context.Context, tx repository.Tx) error {
				return tx.ForceAdvanceAllocators(c, DailyCap*(lostDays+1), s.clock.Now())
			}); err != nil {
				return tenants, err
			}
			tenants++
		}
		if len(batch) < 100 {
			return tenants, nil
		}
		after = batch[len(batch)-1]
	}
}
