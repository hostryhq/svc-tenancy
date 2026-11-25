package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"regexp"
	"strconv"
	"strings"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// The ledger Hostry keeps for its own customers (ADR-0008 S2): charges, payments recorded by hand,
// staff work lists and the CSV export. The panel records the invoice number only; invoices come from the
// founder's books (P2A-6). Nothing is sent and nothing is switched off (BR-A1-50, BR-A1-60).

var (
	invoiceNumberPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/._-]{0,39}$`)
	methods              = map[string]bool{"upi": true, "bank_transfer": true, "cash": true, "cheque": true}
)

// GST computes the tax on a taxable amount in integer paise (BR-A1-32). When Hostry is registered, a customer
// in Hostry's state gets CGST and SGST, each half the rate rounded half up and equal; any other customer gets
// IGST at the full rate rounded half up. When Hostry is not registered no tax is added. There is no round-off:
// the total is exact (the books decide round-off, BR-A1-41). The rate is a configuration input, never code.
func GST(taxable int64, rateBP int, registered, sameState bool) (cgst, sgst, igst int64) {
	if !registered || rateBP <= 0 || taxable <= 0 {
		return 0, 0, 0
	}
	if sameState {
		half := (taxable*int64(rateBP) + 10000) / 20000 // round half up of taxable*rate/2
		return half, half, 0
	}
	return 0, 0, (taxable*int64(rateBP) + 5000) / 10000
}

// RaiseChargeInput is a charge for a period, or an extra charge with a reason.
type RaiseChargeInput struct {
	Kind        string
	PeriodStart string
	PeriodEnd   string
	TaxableP    *int64 // extra charges only; a period charge takes the subscription's agreed price and add-ons
	Reason      string
	Note        string
}

// RaiseCharge raises a charge (BR-A1-31). A period that already has a live charge is refused 409
// duplicate_period unless this is an extra charge with a reason. GST comes from the billing configuration in
// force for the period and is stored on the charge (BR-A1-30). The due date is the later of the raising date
// and the period start plus the payment terms for the first charge, and the period start afterwards (BR-A1-33).
func (s *Service) RaiseCharge(ctx context.Context, orgID ids.ID, in RaiseChargeInput) (ChargeView, error) {
	if in.Kind != model.ChargePeriod && in.Kind != model.ChargeExtra {
		return ChargeView{}, model.Invalid("kind", "unknown", "kind is period or extra")
	}
	ps, err := parseDate("period_start", in.PeriodStart)
	if err != nil {
		return ChargeView{}, err
	}
	pe, err := parseDate("period_end", in.PeriodEnd)
	if err != nil {
		return ChargeView{}, err
	}
	if pe.Before(ps) {
		return ChargeView{}, model.Invalid("period_end", "before_start", "the period ends before it starts")
	}
	if len([]rune(in.Note)) > 200 {
		return ChargeView{}, model.Invalid("note", "length", "at most 200 characters")
	}
	if in.Kind == model.ChargeExtra {
		if n := len([]rune(strings.TrimSpace(in.Reason))); n < 10 || n > 500 {
			return ChargeView{}, model.Invalid("reason", "required", "an extra charge needs a reason of 10 to 500 characters")
		}
		if in.TaxableP == nil || *in.TaxableP <= 0 || *in.TaxableP > maxMoney {
			return ChargeView{}, model.Invalid("taxable", "required", "an extra charge needs an amount")
		}
	} else if in.TaxableP != nil {
		return ChargeView{}, model.Invalid("taxable", "not_allowed", "a period charge takes the agreed price")
	}
	ctx, err = auth.VendorTenant(ctx, "admin.subscription.manage", orgID)
	if err != nil {
		return ChargeView{}, err
	}
	auth.SetAuditTarget(ctx, orgID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (ChargeView, error) {
		if _, err := tx.GetOrg(ctx); err != nil {
			return ChargeView{}, err
		}
		sub, has, err := tx.CurrentSubscription(ctx)
		if err != nil {
			return ChargeView{}, err
		}
		if !has || effectiveState(sub, s.today()) == model.SubCancelled {
			return ChargeView{}, apperr.New(apperr.Conflict, "no_active_subscription", "the organisation has no current subscription")
		}
		if sub.Cycle == model.CycleNone {
			return ChargeView{}, apperr.New(apperr.Conflict, "subscription_has_no_charges", "a pilot has no charges")
		}
		taxable := int64(0)
		if in.Kind == model.ChargePeriod {
			if in.PeriodStart < sub.StartDate {
				return ChargeView{}, model.Invalid("period_start", "before_start", "the period starts before the subscription")
			}
			if sub.CancelAtPeriodEnd && in.PeriodStart > sub.PaidThrough {
				return ChargeView{}, apperr.New(apperr.Conflict, "subscription_ending", "no charge is raised after paid_through of a cancelled subscription")
			}
			overlap, err := tx.LiveChargesOverlapping(ctx, in.PeriodStart, in.PeriodEnd)
			if err != nil {
				return ChargeView{}, err
			}
			if len(overlap) > 0 {
				return ChargeView{}, model.ErrDuplicatePeriod
			}
			taxable = sub.AgreedPaise
			for _, a := range sub.Addons {
				taxable += int64(a.Quantity) * a.AgreedPaise
			}
		} else {
			taxable = *in.TaxableP
		}
		cfg, found, err := tx.BillingConfigFor(ctx, in.PeriodStart)
		if err != nil {
			return ChargeView{}, err
		}
		if !found {
			return ChargeView{}, apperr.New(apperr.Conflict, "billing_configuration_missing", "no GST configuration is effective for this period")
		}
		state, err := customerState(ctx, tx)
		if err != nil {
			return ChargeView{}, err
		}
		cgst, sgst, igst := GST(taxable, cfg.GSTRateBP, cfg.GSTRegistered, state != "" && state == cfg.HostryStateCode)
		// A period charge is due payment_terms_days after its period starts, under the configuration in effect at
		// that date (provisional contract rule, api-specs round 4); an extra charge is due that long after today.
		due := addDaysStr(in.PeriodStart, cfg.PaymentTermsDays)
		if in.Kind == model.ChargeExtra {
			due = addDaysStr(s.today(), cfg.PaymentTermsDays)
		}
		c := model.Charge{ID: s.newID(), SubscriptionID: sub.ID, Kind: in.Kind, PeriodStart: in.PeriodStart, PeriodEnd: in.PeriodEnd, TaxableP: taxable,
			CGSTP: cgst, SGSTP: sgst, IGSTP: igst, TotalP: taxable + cgst + sgst + igst, TolerancePaise: cfg.PaymentToleranceP, DueDate: due,
			Status: model.ChargeOpen, Note: in.Note, Reason: strings.TrimSpace(in.Reason), CreatedBy: p.VendorUser(), CreatedAt: s.clock.Now()}
		if err := tx.InsertCharge(ctx, c); err != nil {
			return ChargeView{}, err
		}
		if in.Kind == model.ChargePeriod && in.PeriodEnd > sub.PaidThrough {
			sub.PaidThrough = in.PeriodEnd
			if err := tx.UpdateSubscription(ctx, sub); err != nil {
				return ChargeView{}, err
			}
		}
		s.log.InfoContext(ctx, "charge raised", "organisation_id", orgID, "charge_id", c.ID, "kind", c.Kind)
		return chargeView(c, 0), nil
	})
}

// customerState is the state of the organisation's first open registration: its GSTIN state, or for an
// unregistered registration the state of its outlets (BR-A1-32). It decides CGST and SGST against IGST.
func customerState(ctx context.Context, tx repository.Tx) (string, error) {
	regs, err := tx.ListRegistrations(ctx, "", 200)
	if err != nil {
		return "", err
	}
	for _, r := range regs {
		if r.Status == model.RegOpen {
			return r.StateCode, nil
		}
	}
	return "", nil
}

func addDaysStr(date string, n int) string { return mustDate(date).AddDate(0, 0, n).Format(dateLayout) }

// ChargePage is one page of charges.
type ChargePage struct {
	Items []ChargeView
	Next  ids.ID
}

// ListCharges pages an organisation's charges, newest period first (a directory-class read). The paid
// status is derived, so a filter on open or paid reads the stored open rows and filters on the derived status.
func (s *Service) ListCharges(ctx context.Context, orgID ids.ID, status string, after ids.ID, limit int) (ChargePage, error) {
	switch status {
	case "", model.ChargeOpen, model.ChargePaid, model.ChargeWaived, model.ChargeCancelled:
	default:
		return ChargePage{}, model.Invalid("status", "unknown", "unknown status")
	}
	ctx, err := auth.VendorTenant(ctx, "admin.subscription.read", orgID)
	if err != nil {
		return ChargePage{}, err
	}
	auth.SetAuditTarget(ctx, orgID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (ChargePage, error) {
		if _, err := tx.GetOrg(ctx); err != nil {
			return ChargePage{}, err
		}
		net, err := tx.NetPaid(ctx)
		if err != nil {
			return ChargePage{}, err
		}
		stored := status
		if status == model.ChargePaid {
			stored = model.ChargeOpen
		}
		var out []ChargeView
		cursor := after
		for len(out) <= limit {
			rows, err := tx.ListCharges(ctx, stored, cursor, limit+1)
			if err != nil {
				return ChargePage{}, err
			}
			for _, c := range rows {
				v := chargeView(c, net[c.ID])
				if status == "" || v.Status == status || (status == model.ChargeOpen && v.Status == model.ChargeOpen) {
					out = append(out, v)
				}
			}
			if len(rows) < limit+1 {
				break
			}
			cursor = rows[len(rows)-1].ID
		}
		items, more := paginate(out, limit)
		pg := ChargePage{Items: items}
		if more {
			pg.Next = items[len(items)-1].ID
		}
		return pg, nil
	})
}

// GetCharge reads one charge; the read is audited (an operational read).
func (s *Service) GetCharge(ctx context.Context, orgID, chargeID ids.ID) (ChargeView, error) {
	ctx, err := auth.VendorTenant(ctx, "admin.subscription.read", orgID)
	if err != nil {
		return ChargeView{}, err
	}
	auth.SetAuditTarget(ctx, chargeID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (ChargeView, error) {
		c, err := tx.GetCharge(ctx, chargeID)
		if err != nil {
			return ChargeView{}, err
		}
		return s.view(ctx, tx, c)
	})
}

func (s *Service) view(ctx context.Context, tx repository.Tx, c model.Charge) (ChargeView, error) {
	net, err := tx.NetPaid(ctx)
	if err != nil {
		return ChargeView{}, err
	}
	return chargeView(c, net[c.ID]), nil
}

func checkAdjustReason(reason string) (string, error) {
	r := strings.TrimSpace(reason)
	if n := len([]rune(r)); n < 10 || n > 500 {
		return "", model.Invalid("reason", "length", "10 to 500 characters")
	}
	return r, nil
}

// CancelCharge cancels a charge raised in error (BR-A1-35, admin.subscription.adjust, reason). A charge with
// recorded payments is refused 409 charge_has_payments: reverse them first. Repeating it changes nothing.
func (s *Service) CancelCharge(ctx context.Context, orgID, chargeID ids.ID, reason string) (ChargeView, error) {
	r, err := checkAdjustReason(reason)
	if err != nil {
		return ChargeView{}, err
	}
	ctx, err = auth.VendorTenant(ctx, "admin.subscription.adjust", orgID)
	if err != nil {
		return ChargeView{}, err
	}
	auth.SetAuditTarget(ctx, chargeID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (ChargeView, error) {
		c, err := tx.GetCharge(ctx, chargeID)
		if err != nil {
			return ChargeView{}, err
		}
		if c.Status == model.ChargeCancelled {
			return s.view(ctx, tx, c)
		}
		if c.Status != model.ChargeOpen {
			return ChargeView{}, apperr.New(apperr.Conflict, "charge_not_open", "the charge is "+c.Status)
		}
		net, err := tx.NetPaid(ctx)
		if err != nil {
			return ChargeView{}, err
		}
		if net[c.ID] != 0 {
			return ChargeView{}, apperr.New(apperr.Conflict, "charge_has_payments", "reverse the payments first")
		}
		c.Status, c.Reason = model.ChargeCancelled, r
		if err := tx.UpdateCharge(ctx, c); err != nil {
			return ChargeView{}, err
		}
		return s.view(ctx, tx, c)
	})
}

// WaiveCharge waives the outstanding amount of an open charge (the amount is not collected; BR-A1-35).
func (s *Service) WaiveCharge(ctx context.Context, orgID, chargeID ids.ID, reason string) (ChargeView, error) {
	r, err := checkAdjustReason(reason)
	if err != nil {
		return ChargeView{}, err
	}
	ctx, err = auth.VendorTenant(ctx, "admin.subscription.adjust", orgID)
	if err != nil {
		return ChargeView{}, err
	}
	auth.SetAuditTarget(ctx, chargeID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (ChargeView, error) {
		c, err := tx.GetCharge(ctx, chargeID)
		if err != nil {
			return ChargeView{}, err
		}
		if c.Status == model.ChargeWaived {
			return s.view(ctx, tx, c)
		}
		v, err := s.view(ctx, tx, c)
		if err != nil {
			return ChargeView{}, err
		}
		if v.Status != model.ChargeOpen {
			return ChargeView{}, apperr.New(apperr.Conflict, "charge_not_open", "the charge is "+v.Status)
		}
		c.Status, c.Reason = model.ChargeWaived, r
		if err := tx.UpdateCharge(ctx, c); err != nil {
			return ChargeView{}, err
		}
		return s.view(ctx, tx, c)
	})
}

// SetChargeInvoiceReference records the number and date of the invoice raised in the founder's books. It is set
// once: a second reference is refused 409 invoice_reference_set, and a mistyped one is fixed by cancelling the
// charge and raising it again (A1-Q8). The panel does not check it against the books (BR-A1-34).
func (s *Service) SetChargeInvoiceReference(ctx context.Context, orgID, chargeID ids.ID, number, date string) (ChargeView, error) {
	if !invoiceNumberPattern.MatchString(number) {
		return ChargeView{}, model.Invalid("invoice_reference.number", "pattern", "1 to 40 letters, digits and / . _ -")
	}
	if _, err := parseDate("invoice_reference.date", date); err != nil {
		return ChargeView{}, err
	}
	ctx, err := auth.VendorTenant(ctx, "admin.subscription.manage", orgID)
	if err != nil {
		return ChargeView{}, err
	}
	auth.SetAuditTarget(ctx, chargeID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (ChargeView, error) {
		c, err := tx.GetCharge(ctx, chargeID)
		if err != nil {
			return ChargeView{}, err
		}
		if c.InvoiceNumber != "" {
			return ChargeView{}, model.ErrInvoiceReferenceSet
		}
		c.InvoiceNumber, c.InvoiceDate = number, date
		if err := tx.UpdateCharge(ctx, c); err != nil {
			return ChargeView{}, err
		}
		return s.view(ctx, tx, c)
	})
}

// PaymentInput is a payment staff saw arrive (BR-A1-43).
type PaymentInput struct {
	AmountP    int64
	Method     string
	Reference  string
	ReceivedOn string
}

// RecordPayment records a payment by hand. Partial payments are allowed; a payment above the outstanding
// amount is refused 422 overpayment (the panel keeps no unapplied credit); a duplicate reference is a
// warning only (BR-A1-45). The row is append-only and idempotent by key.
func (s *Service) RecordPayment(ctx context.Context, orgID, chargeID ids.ID, in PaymentInput) (model.Payment, error) {
	if in.AmountP <= 0 || in.AmountP > maxMoney {
		return model.Payment{}, model.Invalid("amount", "range", "must be more than zero")
	}
	if !methods[in.Method] {
		return model.Payment{}, model.Invalid("method", "unknown", "upi, bank_transfer, cash or cheque")
	}
	if len([]rune(in.Reference)) > 40 {
		return model.Payment{}, model.Invalid("reference", "length", "at most 40 characters")
	}
	rec, err := parseDate("received_on", in.ReceivedOn)
	if err != nil {
		return model.Payment{}, err
	}
	if rec.Format(dateLayout) > s.today() {
		return model.Payment{}, model.Invalid("received_on", "future", "the date received is not in the future")
	}
	ctx, err = auth.VendorTenant(ctx, "admin.subscription.manage", orgID)
	if err != nil {
		return model.Payment{}, err
	}
	auth.SetAuditTarget(ctx, chargeID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Payment, error) {
		c, err := tx.GetCharge(ctx, chargeID)
		if err != nil {
			return model.Payment{}, err
		}
		if c.Status != model.ChargeOpen {
			return model.Payment{}, apperr.New(apperr.Conflict, "charge_not_open", "the charge is "+c.Status)
		}
		net, err := tx.NetPaid(ctx)
		if err != nil {
			return model.Payment{}, err
		}
		if in.AmountP > max(c.TotalP-net[c.ID], 0) {
			return model.Payment{}, model.ErrOverpayment
		}
		if in.Reference != "" {
			if n, err := tx.CountPaymentRef(ctx, in.Reference); err == nil && n > 0 {
				s.log.WarnContext(ctx, "payment reference already recorded for another payment", "organisation_id", orgID, "charge_id", chargeID, "earlier", n)
			}
		}
		pay := model.Payment{ID: s.newID(), ChargeID: chargeID, AmountP: in.AmountP, Method: in.Method, Reference: in.Reference, ReceivedOn: in.ReceivedOn,
			RecordedBy: p.VendorUser(), CreatedAt: s.clock.Now()}
		return pay, tx.InsertPayment(ctx, pay)
	})
}

// ReversePayment adds a reversing row for a payment that did not happen (a bounced cheque): never an edit or a
// delete (BR-A1-44). A payment is reversed once, and a reversing row cannot be reversed.
func (s *Service) ReversePayment(ctx context.Context, orgID, chargeID, paymentID ids.ID, reason string) (model.Payment, error) {
	r, err := checkAdjustReason(reason)
	if err != nil {
		return model.Payment{}, err
	}
	ctx, err = auth.VendorTenant(ctx, "admin.subscription.adjust", orgID)
	if err != nil {
		return model.Payment{}, err
	}
	auth.SetAuditTarget(ctx, chargeID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Payment, error) {
		orig, err := tx.GetPayment(ctx, chargeID, paymentID)
		if err != nil {
			return model.Payment{}, err
		}
		if orig.ReversalOf != "" {
			return model.Payment{}, apperr.New(apperr.Conflict, "payment_is_reversal", "a reversing row cannot be reversed")
		}
		all, err := tx.AllPayments(ctx)
		if err != nil {
			return model.Payment{}, err
		}
		for _, e := range all {
			if e.ReversalOf == paymentID {
				return model.Payment{}, apperr.New(apperr.Conflict, "payment_reversed", "the payment is already reversed")
			}
		}
		rev := model.Payment{ID: s.newID(), ChargeID: chargeID, AmountP: orig.AmountP, Method: orig.Method, Reference: orig.Reference, ReceivedOn: orig.ReceivedOn,
			RecordedBy: p.VendorUser(), ReversalOf: paymentID, Reason: r, CreatedAt: s.clock.Now()}
		return rev, tx.InsertPayment(ctx, rev)
	})
}

// PaymentPage is one page of payment rows.
type PaymentPage struct {
	Items []model.Payment
	Next  ids.ID
}

// ListChargePayments pages the payment rows of a charge, oldest first (a directory-class read).
func (s *Service) ListChargePayments(ctx context.Context, orgID, chargeID, after ids.ID, limit int) (PaymentPage, error) {
	ctx, err := auth.VendorTenant(ctx, "admin.subscription.read", orgID)
	if err != nil {
		return PaymentPage{}, err
	}
	auth.SetAuditTarget(ctx, chargeID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (PaymentPage, error) {
		if _, err := tx.GetCharge(ctx, chargeID); err != nil {
			return PaymentPage{}, err
		}
		rows, err := tx.ListPayments(ctx, chargeID, after, limit+1)
		if err != nil {
			return PaymentPage{}, err
		}
		items, more := paginate(rows, limit)
		pg := PaymentPage{Items: items}
		if more {
			pg.Next = items[len(items)-1].ID
		}
		return pg, nil
	})
}

// ---- work lists and export (cross-organisation, read from the vendor directory) ----

// ListKinds are the staff work lists (BR-A1-50).
var ListKinds = map[string]bool{"overdue_charges": true, "charges_to_raise": true, "charges_without_invoice_reference": true,
	"pilots_ending": true, "retired_version_subscriptions": true, "outlets_over_limit": true}

// LedgerPage is one page of a work list; the cursor is (NextDue, NextID).
type LedgerPage struct {
	Rows    []model.LedgerRow
	NextDue string
	NextID  ids.ID
	HasMore bool
}

// ListLedger derives one work list on read: there are no scheduled tasks and nothing is sent. Rows are
// oldest first.
func (s *Service) ListLedger(ctx context.Context, kind, afterDue string, afterID ids.ID, limit int) (LedgerPage, error) {
	if !ListKinds[kind] {
		return LedgerPage{}, model.ErrNotFound
	}
	rows, err := s.repo.Directory().LedgerList(ctx, kind, s.today(), afterDue, afterID, limit+1)
	if err != nil {
		return LedgerPage{}, err
	}
	items, more := paginate(rows, limit)
	pg := LedgerPage{Rows: items, HasMore: more}
	if more {
		last := items[len(items)-1]
		pg.NextDue, pg.NextID = last.DueDate, last.SubjectID
	}
	return pg, nil
}

var csvHeader = []string{"organisation_id", "organisation_name", "period_start", "period_end", "taxable_paise", "total_paise", "due_date",
	"invoice_number", "status", "outstanding_paise"}

// ExportLedger renders the ledger as CSV for charges due in [from, to]: organisation ids and names only, no
// GSTIN, phone or email (BR-A1-38). The export is audited. A name that a spreadsheet would read as a formula is
// prefixed with an apostrophe.
func (s *Service) ExportLedger(ctx context.Context, from, to string) ([]byte, error) {
	for f, v := range map[string]string{"from": from, "to": to} {
		if v != "" {
			if _, err := parseDate(f, v); err != nil {
				return nil, err
			}
		}
	}
	if from != "" && to != "" && to < from {
		return nil, model.Invalid("to", "before_from", "the end is before the start")
	}
	rows, err := s.repo.Directory().LedgerExport(ctx, from, to)
	if err != nil {
		return nil, err
	}
	// The audit row commits in a platform transaction of its own around the work (the read above is the work).
	if err := s.repo.InPlatformOnlyTx(ctx, func(context.Context, repository.Tx) error { return nil }); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write(csvHeader)
	for _, r := range rows {
		_ = w.Write([]string{r.OrganisationID.String(), safeCell(r.OrganisationName), r.PeriodStart, r.PeriodEnd, strconv.FormatInt(r.TaxableP, 10),
			strconv.FormatInt(r.TotalP, 10), r.DueDate, safeCell(r.InvoiceNumber), r.Status, strconv.FormatInt(r.OutstandingP, 10)})
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

func safeCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// ---- the owner's "My plan" (tenant realm, read only) ----

// MyPlan is what the owner sees of their own subscription (BR-A1-90): no vendor user ids, no notes.
type MyPlan struct {
	Sub      SubView
	NameKey  string
	Charges  []ChargeView
	Payments []model.Payment
}

// GetMyPlan returns the caller's own organisation's plan, charges and payments. Overdue is a label only; it
// gates nothing (ADR-0008 S5). An organisation with no subscription is 404.
func (s *Service) GetMyPlan(ctx context.Context) (MyPlan, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (MyPlan, error) {
		var out MyPlan
		sub, has, err := tx.CurrentSubscription(ctx)
		if err != nil {
			return out, err
		}
		if !has {
			return out, model.ErrNotFound
		}
		if out.Sub, err = s.subView(ctx, tx, sub); err != nil {
			return out, err
		}
		plan, err := tx.GetPlan(ctx, sub.PlanKey)
		if err != nil {
			return out, err
		}
		out.NameKey = plan.NameKey
		charges, net, err := allCharges(ctx, tx)
		if err != nil {
			return out, err
		}
		for _, c := range charges {
			out.Charges = append(out.Charges, chargeView(c, net[c.ID]))
		}
		pays, err := tx.AllPayments(ctx)
		if err != nil {
			return out, err
		}
		reversed := map[ids.ID]bool{}
		for _, p := range pays {
			if p.ReversalOf != "" {
				reversed[p.ReversalOf] = true
			}
		}
		for i := len(pays) - 1; i >= 0; i-- { // newest first
			if pays[i].ReversalOf == "" && !reversed[pays[i].ID] {
				out.Payments = append(out.Payments, pays[i])
			}
		}
		return out, nil
	})
}
