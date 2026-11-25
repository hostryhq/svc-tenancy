package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// Grant kinds of the three lifecycle actions executed here (ADR-0014 A1.3, A2.6).
const (
	KindSuspend   = "org_suspend"
	KindReinstate = "org_reinstate"
	KindClose     = "org_close"
)

var ticketPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,63}$`)

// istDates returns today and yesterday in IST as business dates: "in progress" means a day open with one of
// these dates (ADR-0014 A1.2 item 3); a day left open longer protects nothing.
func (s *Service) istDates() (yesterday, today string) {
	n := s.clock.Now().In(clock.IST)
	return n.AddDate(0, 0, -1).Format("2006-01-02"), n.Format("2006-01-02")
}

// guardTradingDay refuses with 409 open_trading_day while a day is in progress at the outlet (or at any
// outlet of the organisation when outletID is empty). The count of outlets is in the message.
func (s *Service) guardTradingDay(ctx context.Context, tx repository.Tx, outletID ids.ID) error {
	from, to := s.istDates()
	n, err := tx.OutletsInProgress(ctx, from, to, outletID)
	if err != nil {
		return err
	}
	if n > 0 {
		return apperr.New(apperr.Conflict, "open_trading_day", fmt.Sprintf("%d outlet(s) have a trading day in progress", n))
	}
	return nil
}

// ChangeOutletTaxRegistration moves an outlet to another registration of the organisation (ADR-0014 A2.5
// item 1, ADR-0003 section 1a). The target must be open and verified (or of regime unregistered). Refused 409
// open_trading_day while the outlet has a day in progress, so one business day has one registration. In one
// transaction the outlet points at the target and every series of the outlet under the old registration is
// closed (it then reads closed, awaiting device); tenancy.outlet.updated and tenancy.series.closed follow.
// The same target again returns the outlet.
func (s *Service) ChangeOutletTaxRegistration(ctx context.Context, outletID, registrationID ids.ID, reason string) (model.Outlet, error) {
	if err := validID("tax_registration_id", registrationID); err != nil {
		return model.Outlet{}, err
	}
	if err := checkClosingReason(reason); err != nil {
		return model.Outlet{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Outlet, error) {
		o, err := tx.GetOutlet(ctx, outletID)
		if err != nil {
			return o, err
		}
		if err := requireOutlet(ctx, "tax_registration.manage", o.ID); err != nil {
			return o, err
		}
		target, err := tx.GetRegistration(ctx, registrationID)
		if err != nil {
			return o, err
		}
		if o.TaxRegistrationID == registrationID {
			return o, nil
		}
		if target.Status != model.RegOpen || (target.Regime != model.RegimeUnregistered && target.Verification != model.Verified) {
			return o, model.ErrNotEligible
		}
		if err := s.guardTradingDay(ctx, tx, o.ID); err != nil {
			return o, err
		}
		old := o.TaxRegistrationID
		o.TaxRegistrationID = registrationID
		if err := tx.UpdateOutlet(ctx, o); err != nil {
			return o, err
		}
		closed, err := tx.CloseOutletSeries(ctx, o.ID, old, s.clock.Now())
		if err != nil {
			return o, err
		}
		tid, err := tenantOf(ctx)
		if err != nil {
			return o, err
		}
		if err := s.publishOutlet(ctx, tx, "tenancy.outlet.updated", tid, o); err != nil {
			return o, err
		}
		if err := s.publishSeriesClosed(ctx, tx, closed, "outlet_moved"); err != nil {
			return o, err
		}
		s.log.InfoContext(ctx, "outlet moved to another tax registration", "outlet_id", o.ID, "registration_id", registrationID, "series_closed", len(closed))
		return o, nil
	})
}

// LifecycleInput is the execution of an approved suspend, reinstate or close.
type LifecycleInput struct {
	Kind           string // org_suspend, org_reinstate or org_close
	OrganisationID ids.ID
	Grant          string // the Hostry-Vendor-Approval header
	Ticket         string
	Reason         string
	ExportOffered  bool
}

// LifecycleResult is the organisation after the action.
type LifecycleResult struct {
	OrganisationID ids.ID
	Status         string
	ChangedAt      time.Time
}

type transition struct {
	target string
	from   []string
	guard  bool
}

var transitions = map[string]transition{
	KindSuspend:   {target: model.OrgSuspended, from: []string{model.OrgTrial, model.OrgActive}, guard: true},
	KindReinstate: {target: model.OrgActive, from: []string{model.OrgSuspended}},
	KindClose:     {target: model.OrgClosed, from: []string{model.OrgTrial, model.OrgActive, model.OrgSuspended}, guard: true},
}

// ChangeOrganisationStatus executes a lifecycle action with a vendor approval grant (ADR-0014 A1.3, A2.6):
// the grant is verified (signature, audience, kind, organisation, approver, then the subject hash recomputed
// from the path and the target status), its id is recorded as used in this transaction, the open-trading-day
// guard is applied (suspend and close only), the status and the directory row change, a tenant audit entry
// with the "show to owner" marker is written and tenancy.organisation.status_changed is published. If the
// guard refuses, the transaction rolls back and the grant is not consumed, so the approval stays approved
// and a fresh grant may be issued.
func (s *Service) ChangeOrganisationStatus(ctx context.Context, in LifecycleInput) (LifecycleResult, error) {
	tr, ok := transitions[in.Kind]
	if !ok {
		return LifecycleResult{}, model.Invalid("kind", "unknown", "unknown lifecycle action")
	}
	if !ticketPattern.MatchString(in.Ticket) {
		return LifecycleResult{}, model.Invalid("ticket", "pattern", "not a ticket reference")
	}
	if n := len([]rune(strings.TrimSpace(in.Reason))); n < 10 || n > 500 {
		return LifecycleResult{}, model.Invalid("reason", "length", "10 to 500 characters")
	}
	if in.Kind == KindClose && !in.ExportOffered {
		return LifecycleResult{}, model.Invalid("owner_export_offered", "required", "closing needs the recorded confirmation that the owner was offered an export")
	}
	ctx, err := auth.VendorTenant(ctx, "admin.org.lifecycle.approve", in.OrganisationID)
	if err != nil {
		return LifecycleResult{}, err
	}
	auth.SetAuditTarget(ctx, in.OrganisationID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (LifecycleResult, error) {
		org, err := tx.GetOrg(ctx)
		if err != nil {
			return LifecycleResult{}, err
		}
		claims, err := s.grants.Verify(ctx, in.Grant, auth.GrantWant{Kind: in.Kind, Org: in.OrganisationID.String(), Approver: p.VendorUser(),
			SubjectHash: func() (string, error) {
				return auth.SubjectHash(in.Kind, in.OrganisationID.String(), tr.target), nil
			}})
		if err != nil {
			return LifecycleResult{}, err
		}
		now := s.clock.Now()
		first, err := tx.MarkGrantUsed(ctx, claims.JTI, claims.ApprovalID, in.Kind, now)
		if err != nil {
			return LifecycleResult{}, err
		}
		if !first {
			return LifecycleResult{}, model.ErrGrantUsed
		}
		allowed := false
		for _, f := range tr.from {
			allowed = allowed || f == org.Status
		}
		if !allowed {
			return LifecycleResult{}, apperr.New(apperr.Conflict, "organisation_status_conflict", "the organisation is "+org.Status)
		}
		if tr.guard {
			if err := s.guardTradingDay(ctx, tx, ""); err != nil {
				return LifecycleResult{}, err
			}
		}
		if err := tx.SetOrgStatus(ctx, tr.target, now); err != nil {
			return LifecycleResult{}, err
		}
		if err := tx.AppendLifecycle(ctx, model.LifecycleEntry{ID: s.newID(), Kind: in.Kind, ApprovalID: claims.ApprovalID, Requester: claims.Requester,
			Approver: claims.Approver, From: org.Status, To: tr.target, Ticket: in.Ticket, Reason: strings.TrimSpace(in.Reason), ExportOffer: in.ExportOffered,
			OccurredAt: now, ShowToOwner: true}); err != nil {
			return LifecycleResult{}, err
		}
		if err := s.publish(ctx, tx, tev.OrganisationStatusChanged(s.newID(), now, p.VendorUser(), in.OrganisationID, tr.target, org.Status, claims.ApprovalID)); err != nil {
			return LifecycleResult{}, err
		}
		s.log.InfoContext(ctx, "organisation status changed", "organisation_id", in.OrganisationID, "from", org.Status, "to", tr.target, "approval_id", claims.ApprovalID)
		return LifecycleResult{OrganisationID: in.OrganisationID, Status: tr.target, ChangedAt: now}, nil
	})
}

// ---- events consumed from the bus (ADR-0011 B5) ----

type tradingDayData struct {
	OutletID     ids.ID    `json:"outlet_id"`
	BusinessDate string    `json:"business_date"`
	State        string    `json:"state"`
	ChangedAt    time.Time `json:"changed_at"`
}

type reconciledData struct {
	SeriesID   ids.ID `json:"series_id"`
	LastNumber *int   `json:"last_number"`
}

type drainedData struct {
	DrainedAt time.Time `json:"drained_at"`
	Series    []struct {
		SeriesID   ids.ID `json:"series_id"`
		LastNumber int    `json:"last_number"`
	} `json:"series"`
}

// ApplyEvent applies one event inside tx (the tenant context is that of the envelope). It is idempotent and
// tolerates out-of-order delivery: the trading state keeps the latest business date and change, and a series
// tail is filled once. Events of other types are ignored.
//
//	billing.trading_day.changed  -> outlet_trading_state (the open-trading-day guard, ADR-0014 A1.2)
//	billing.series.reconciled    -> tail_confirmed_at and last_number of a closed series (A2.5 item 2)
//	identity.device.drained      -> the same from the revoked device's own statement of its last numbers (the
//	                                spec lists tenancy as subscriber). The first confirmation wins; a series
//	                                not yet closed here is left alone.
func (s *Service) ApplyEvent(ctx context.Context, tx repository.Tx, e events.Envelope) error {
	switch e.Type {
	case "billing.trading_day.changed":
		var d tradingDayData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("trading_day.changed: %w", err)
		}
		if _, err := time.Parse("2006-01-02", d.BusinessDate); err != nil || (d.State != "open" && d.State != "closed") || d.OutletID == "" {
			return fmt.Errorf("trading_day.changed: invalid payload")
		}
		at := d.ChangedAt
		if at.IsZero() {
			at = e.OccurredAt
		}
		_, err := tx.UpsertTradingState(ctx, model.TradingState{OutletID: d.OutletID, Source: e.Source, BusinessDate: d.BusinessDate, State: d.State, ChangedAt: at.UTC()})
		return err
	case "billing.series.reconciled":
		var d reconciledData
		if err := json.Unmarshal(e.Data, &d); err != nil || d.SeriesID == "" || d.LastNumber == nil || *d.LastNumber < 0 {
			return fmt.Errorf("series.reconciled: invalid payload")
		}
		_, err := tx.ConfirmTail(ctx, d.SeriesID, e.OccurredAt.UTC(), *d.LastNumber)
		return err
	case "identity.device.drained":
		var d drainedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("device.drained: %w", err)
		}
		at := d.DrainedAt
		if at.IsZero() {
			at = e.OccurredAt
		}
		for _, sr := range d.Series {
			if sr.SeriesID == "" || sr.LastNumber < 0 {
				return fmt.Errorf("device.drained: invalid payload")
			}
			if _, err := tx.ConfirmTail(ctx, sr.SeriesID, at.UTC(), sr.LastNumber); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

// HandleEvent applies an event in a transaction of its own, for callers that have no transaction to
// join (the test double). The bus consumer uses ApplyEvent inside the inbox transaction instead.
func (s *Service) HandleEvent(ctx context.Context, e events.Envelope) error {
	return s.repo.InTx(e.TenantContext(ctx), func(ctx context.Context, tx repository.Tx) error { return s.ApplyEvent(ctx, tx, e) })
}
