package api

import (
	"net/http"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/page"
	"github.com/hostryhq/svc-tenancy/service"
)

// Controllers of the Phase 2 operations: the commercial module (ADR-0008 S2), the organisation lifecycle
// under a vendor approval grant (ADR-0014 A1.3, A2.6), the outlet's tax registration, bill books and
// "My plan". They validate the transport, call the service and map to the generated types; the rules live in
// service/.

const dayLayout = "2006-01-02"

func money(p int64) gen.Money { return gen.Money{AmountPaise: p, Currency: gen.INR} }

func dateOf(s string) openapi_types.Date {
	t, _ := time.Parse(dayLayout, s)
	return openapi_types.Date{Time: t}
}

func datePtr(s string) *openapi_types.Date {
	if s == "" {
		return nil
	}
	d := dateOf(s)
	return &d
}

func dayStr(d openapi_types.Date) string { return d.Time.Format(dayLayout) }

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---- plans and the GST configuration ----

func planVersionDTO(v model.PlanVersion) gen.PlanVersion {
	d := gen.PlanVersion{Version: v.Version, State: gen.PlanVersionState(v.State), FeatureKeys: append([]string{}, v.FeatureKeys...), IncludedOutlets: v.IncludedOutlets,
		Prices: []gen.PlanPrice{}, CreatedAt: v.CreatedAt, ActivatedAt: v.ActivatedAt, RetiredAt: v.RetiredAt}
	for _, p := range v.Prices {
		d.Prices = append(d.Prices, gen.PlanPrice{Cycle: gen.Cycle(p.Cycle), ListPrice: money(p.ListPaise)})
	}
	return d
}

func planDTO(p model.Plan) gen.Plan {
	d := gen.Plan{Key: p.Key, NameKey: p.NameKey, CreatedAt: p.CreatedAt, Versions: []gen.PlanVersion{}}
	for _, v := range p.Versions {
		d.Versions = append(d.Versions, planVersionDTO(v))
	}
	return d
}

func versionInput(d gen.PlanVersionDraft) service.VersionInput {
	in := service.VersionInput{FeatureKeys: d.FeatureKeys, IncludedOutlets: d.IncludedOutlets}
	for _, p := range d.Prices {
		in.Prices = append(in.Prices, model.PlanPrice{Cycle: string(p.Cycle), ListPaise: p.ListPrice.AmountPaise})
	}
	return in
}

func (s *Server) ListPlans(w http.ResponseWriter, r *http.Request, p gen.ListPlansParams) {
	after, err := page.Decode(p.Cursor)
	if err == nil {
		var n int
		if n, err = page.Limit((*int)(p.Limit), 25, 100); err == nil {
			var pg service.PlanPage
			pg, err = s.svc.ListPlans(r.Context(), after, n)
			out := gen.PlanPage{Items: []gen.Plan{}}
			for _, x := range pg.Items {
				out.Items = append(out.Items, planDTO(x))
			}
			if pg.NextKey != "" {
				c := page.Encode(pg.NextKey)
				out.NextCursor = &c
			}
			s.reply(w, r, http.StatusOK, out, err)
			return
		}
	}
	s.fail(w, r, err)
}

func (s *Server) CreatePlan(w http.ResponseWriter, r *http.Request, _ gen.CreatePlanParams) {
	var in gen.CreatePlan
	ctx, ok := s.begin(w, r, "createPlan", &in)
	if !ok {
		return
	}
	p, err := s.svc.CreatePlan(ctx, in.Key, in.NameKey, versionInput(in.Version))
	s.reply(w, r, http.StatusCreated, planDTO(p), err)
}

func (s *Server) GetPlan(w http.ResponseWriter, r *http.Request, planKey string, _ gen.GetPlanParams) {
	p, err := s.svc.GetPlan(r.Context(), planKey)
	s.reply(w, r, http.StatusOK, planDTO(p), err)
}

func (s *Server) CreatePlanVersion(w http.ResponseWriter, r *http.Request, planKey string, _ gen.CreatePlanVersionParams) {
	var in gen.PlanVersionDraft
	ctx, ok := s.begin(w, r, "createPlanVersion", &in)
	if !ok {
		return
	}
	v, err := s.svc.CreatePlanVersion(ctx, planKey, versionInput(in))
	s.reply(w, r, http.StatusCreated, planVersionDTO(v), err)
}

func (s *Server) ActivatePlanVersion(w http.ResponseWriter, r *http.Request, planKey string, version int, _ gen.ActivatePlanVersionParams) {
	ctx, ok := s.begin(w, r, "activatePlanVersion", nil)
	if !ok {
		return
	}
	v, err := s.svc.ActivatePlanVersion(ctx, planKey, version)
	s.reply(w, r, http.StatusOK, planVersionDTO(v), err)
}

func (s *Server) RetirePlanVersion(w http.ResponseWriter, r *http.Request, planKey string, version int, _ gen.RetirePlanVersionParams) {
	ctx, ok := s.begin(w, r, "retirePlanVersion", nil)
	if !ok {
		return
	}
	v, err := s.svc.RetirePlanVersion(ctx, planKey, version)
	s.reply(w, r, http.StatusOK, planVersionDTO(v), err)
}

func configDTO(c model.BillingConfig) gen.BillingConfiguration {
	return gen.BillingConfiguration{Id: c.ID.String(), EffectiveFrom: dateOf(c.EffectiveFrom), HostryStateCode: c.HostryStateCode, Sac: c.SAC, GstRateBp: c.GSTRateBP,
		GstRegistered: c.GSTRegistered, PaymentTolerancePaise: int(c.PaymentToleranceP), PaymentTermsDays: c.PaymentTermsDays, CreatedAt: c.CreatedAt}
}

func (s *Server) ListBillingConfigurations(w http.ResponseWriter, r *http.Request, p gen.ListBillingConfigurationsParams) {
	after, n, err := pageParams(p.Cursor, (*int)(p.Limit), 25, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListBillingConfigs(r.Context(), after, n)
	out := gen.BillingConfigurationPage{Items: []gen.BillingConfiguration{}, NextCursor: nextCursor(pg.Next)}
	for _, c := range pg.Items {
		out.Items = append(out.Items, configDTO(c))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) CreateBillingConfiguration(w http.ResponseWriter, r *http.Request, _ gen.CreateBillingConfigurationParams) {
	var in gen.CreateBillingConfiguration
	ctx, ok := s.begin(w, r, "createBillingConfiguration", &in)
	if !ok {
		return
	}
	c, err := s.svc.CreateBillingConfig(ctx, service.BillingConfigInput{EffectiveFrom: dayStr(in.EffectiveFrom), HostryStateCode: in.HostryStateCode, SAC: in.Sac,
		GSTRateBP: in.GstRateBp, GSTRegistered: in.GstRegistered, PaymentToleranceP: int64(in.PaymentTolerancePaise), PaymentTermsDays: in.PaymentTermsDays})
	s.reply(w, r, http.StatusCreated, configDTO(c), err)
}

// ---- subscription ----

func addonsIn(in *[]gen.Addon) []model.Addon {
	if in == nil {
		return nil
	}
	out := make([]model.Addon, len(*in))
	for i, a := range *in {
		out[i] = model.Addon{Name: a.Name, Quantity: a.Quantity, AgreedPaise: a.AgreedPrice.AmountPaise}
	}
	return out
}

func subDTO(orgID ids.ID, v service.SubView) gen.Subscription {
	d := gen.Subscription{Id: v.ID.String(), OrganisationId: orgID.String(), PlanKey: v.PlanKey, PlanVersion: v.PlanVersion, Cycle: gen.Cycle(v.Cycle),
		AgreedPrice: money(v.AgreedPaise), Addons: []gen.Addon{}, StartDate: dateOf(v.StartDate), PaidThrough: datePtr(v.PaidThrough),
		State: gen.SubscriptionState(v.State), Overdue: v.Overdue, CancelAtPeriodEnd: v.CancelAtPeriodEnd, CreatedAt: v.CreatedAt, SupersededAt: v.SupersededAt}
	over := v.OutletsOverLimit
	d.OutletsOverLimit = &over
	for _, a := range v.Addons {
		d.Addons = append(d.Addons, gen.Addon{Name: a.Name, Quantity: a.Quantity, AgreedPrice: money(a.AgreedPaise)})
	}
	return d
}

func agreedIn(m *gen.Money) *int64 {
	if m == nil {
		return nil
	}
	v := m.AmountPaise
	return &v
}

func (s *Server) StartSubscription(w http.ResponseWriter, r *http.Request, organisationID gen.Id, _ gen.StartSubscriptionParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	var in gen.StartSubscription
	ctx, ok := s.begin(w, r, "startSubscription", &in)
	if !ok {
		return
	}
	v, err := s.svc.StartSubscription(ctx, oid, service.StartInput{PlanKey: in.PlanKey, Version: in.PlanVersion, Cycle: string(in.Cycle), StartDate: dayStr(in.StartDate),
		AgreedPaise: agreedIn(in.AgreedPrice), Addons: addonsIn(in.Addons), ReviewDate: optDate(in.ReviewDate), Reason: deref(in.Reason)})
	s.reply(w, r, http.StatusCreated, subDTO(oid, v), err)
}

func optDate(d *openapi_types.Date) string {
	if d == nil {
		return ""
	}
	return dayStr(*d)
}

func (s *Server) ChangeSubscription(w http.ResponseWriter, r *http.Request, organisationID gen.Id, _ gen.ChangeSubscriptionParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	var in gen.ChangeSubscription
	ctx, ok := s.begin(w, r, "changeSubscription", &in)
	if !ok {
		return
	}
	v, err := s.svc.ChangeSubscription(ctx, oid, service.ChangeInput{PlanKey: in.PlanKey, Version: in.PlanVersion, Cycle: string(in.Cycle), EffectiveFrom: dayStr(in.EffectiveFrom),
		AgreedPaise: agreedIn(in.AgreedPrice), Addons: addonsIn(in.Addons), Reason: deref(in.Reason)})
	s.reply(w, r, http.StatusOK, subDTO(oid, v), err)
}

func (s *Server) CancelSubscription(w http.ResponseWriter, r *http.Request, organisationID gen.Id, _ gen.CancelSubscriptionParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	var in gen.CancelSubscription
	ctx, ok := s.begin(w, r, "cancelSubscription", &in)
	if !ok {
		return
	}
	v, err := s.svc.CancelSubscription(ctx, oid)
	s.reply(w, r, http.StatusOK, subDTO(oid, v), err)
}

func (s *Server) GetOrganisationSubscription(w http.ResponseWriter, r *http.Request, organisationID gen.Id, _ gen.GetOrganisationSubscriptionParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	h, err := s.svc.GetOrganisationSubscription(r.Context(), oid)
	out := gen.SubscriptionHistory{History: []gen.Subscription{}}
	if h.Current != nil {
		c := subDTO(oid, *h.Current)
		out.Current = &c
	}
	for _, x := range h.History {
		out.History = append(out.History, subDTO(oid, x))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) GetMyPlan(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.GetMyPlan(r.Context())
	out := gen.MyPlan{PlanKey: m.Sub.PlanKey, PlanNameKey: m.NameKey, PlanVersion: m.Sub.PlanVersion, Cycle: gen.Cycle(m.Sub.Cycle), AgreedPrice: money(m.Sub.AgreedPaise),
		StartDate: dateOf(m.Sub.StartDate), PaidThrough: datePtr(m.Sub.PaidThrough), State: gen.SubscriptionState(m.Sub.State), Overdue: m.Sub.Overdue}
	out.Charges = make([]struct {
		DueDate       openapi_types.Date `json:"due_date"`
		InvoiceNumber *string            `json:"invoice_number,omitempty"`
		PeriodEnd     openapi_types.Date `json:"period_end"`
		PeriodStart   openapi_types.Date `json:"period_start"`
		Status        gen.ChargeStatus   `json:"status"`
		Total         gen.Money          `json:"total"`
	}, 0, len(m.Charges))
	for _, c := range m.Charges {
		out.Charges = append(out.Charges, struct {
			DueDate       openapi_types.Date `json:"due_date"`
			InvoiceNumber *string            `json:"invoice_number,omitempty"`
			PeriodEnd     openapi_types.Date `json:"period_end"`
			PeriodStart   openapi_types.Date `json:"period_start"`
			Status        gen.ChargeStatus   `json:"status"`
			Total         gen.Money          `json:"total"`
		}{DueDate: dateOf(c.DueDate), InvoiceNumber: strPtr(c.InvoiceNumber), PeriodEnd: dateOf(c.PeriodEnd), PeriodStart: dateOf(c.PeriodStart),
			Status: gen.ChargeStatus(c.Status), Total: money(c.TotalP)})
	}
	out.Payments = make([]struct {
		Amount     gen.Money          `json:"amount"`
		Method     gen.PaymentMethod  `json:"method"`
		ReceivedOn openapi_types.Date `json:"received_on"`
	}, 0, len(m.Payments))
	for _, p := range m.Payments {
		out.Payments = append(out.Payments, struct {
			Amount     gen.Money          `json:"amount"`
			Method     gen.PaymentMethod  `json:"method"`
			ReceivedOn openapi_types.Date `json:"received_on"`
		}{Amount: money(p.AmountP), Method: gen.PaymentMethod(p.Method), ReceivedOn: dateOf(p.ReceivedOn)})
	}
	s.reply(w, r, http.StatusOK, out, err)
}

// ---- charges and payments ----

func chargeDTO(orgID ids.ID, c service.ChargeView) gen.Charge {
	d := gen.Charge{Id: c.ID.String(), OrganisationId: orgID.String(), SubscriptionId: c.SubscriptionID.String(), Kind: gen.ChargeKind(c.Kind),
		PeriodStart: dateOf(c.PeriodStart), PeriodEnd: dateOf(c.PeriodEnd), Taxable: money(c.TaxableP), Cgst: money(c.CGSTP), Sgst: money(c.SGSTP), Igst: money(c.IGSTP),
		Total: money(c.TotalP), DueDate: dateOf(c.DueDate), Status: gen.ChargeStatus(c.Status), Outstanding: money(c.Outstanding), CreatedAt: c.CreatedAt,
		Note: strPtr(c.Note), Reason: strPtr(c.Reason)}
	if c.InvoiceNumber != "" {
		d.InvoiceReference = &gen.InvoiceReference{Number: c.InvoiceNumber, Date: dateOf(c.InvoiceDate)}
	}
	return d
}

func paymentDTO(p model.Payment) gen.LedgerPayment {
	d := gen.LedgerPayment{Id: p.ID.String(), ChargeId: p.ChargeID.String(), Amount: money(p.AmountP), Method: gen.PaymentMethod(p.Method), Reference: strPtr(p.Reference),
		ReceivedOn: dateOf(p.ReceivedOn), RecordedBy: p.RecordedBy, Reason: strPtr(p.Reason), CreatedAt: p.CreatedAt}
	if p.ReversalOf != "" {
		v := p.ReversalOf.String()
		d.ReversalOf = &v
	}
	return d
}

func (s *Server) RaiseCharge(w http.ResponseWriter, r *http.Request, organisationID gen.Id, _ gen.RaiseChargeParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	var in gen.RaiseCharge
	ctx, ok := s.begin(w, r, "raiseCharge", &in)
	if !ok {
		return
	}
	c, err := s.svc.RaiseCharge(ctx, oid, service.RaiseChargeInput{Kind: string(in.Kind), PeriodStart: dayStr(in.PeriodStart), PeriodEnd: dayStr(in.PeriodEnd),
		TaxableP: agreedIn(in.Taxable), Reason: deref(in.Reason), Note: deref(in.Note)})
	s.reply(w, r, http.StatusCreated, chargeDTO(oid, c), err)
}

func (s *Server) ListCharges(w http.ResponseWriter, r *http.Request, organisationID gen.Id, p gen.ListChargesParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	after, n, err := pageParams(p.Cursor, (*int)(p.Limit), 25, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListCharges(r.Context(), oid, string(deref(p.Status)), after, n)
	out := gen.ChargePage{Items: []gen.Charge{}, NextCursor: nextCursor(pg.Next)}
	for _, c := range pg.Items {
		out.Items = append(out.Items, chargeDTO(oid, c))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) GetCharge(w http.ResponseWriter, r *http.Request, organisationID, chargeID gen.Id, _ gen.GetChargeParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	cid, ok := s.pathID(w, r, chargeID)
	if !ok {
		return
	}
	c, err := s.svc.GetCharge(r.Context(), oid, cid)
	s.reply(w, r, http.StatusOK, chargeDTO(oid, c), err)
}

// chargeAction runs a charge adjustment that takes a reason and returns the charge.
func (s *Server) chargeAction(w http.ResponseWriter, r *http.Request, op string, organisationID, chargeID gen.Id, do func(ctx ctxT, oid, cid ids.ID, reason string) (service.ChargeView, error)) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	cid, ok := s.pathID(w, r, chargeID)
	if !ok {
		return
	}
	var in gen.ReasonBody
	ctx, ok := s.begin(w, r, op, &in)
	if !ok {
		return
	}
	c, err := do(ctx, oid, cid, in.Reason)
	s.reply(w, r, http.StatusOK, chargeDTO(oid, c), err)
}

func (s *Server) CancelCharge(w http.ResponseWriter, r *http.Request, organisationID, chargeID gen.Id, _ gen.CancelChargeParams) {
	s.chargeAction(w, r, "cancelCharge", organisationID, chargeID, s.svc.CancelCharge)
}

func (s *Server) WaiveCharge(w http.ResponseWriter, r *http.Request, organisationID, chargeID gen.Id, _ gen.WaiveChargeParams) {
	s.chargeAction(w, r, "waiveCharge", organisationID, chargeID, s.svc.WaiveCharge)
}

func (s *Server) SetChargeInvoiceReference(w http.ResponseWriter, r *http.Request, organisationID, chargeID gen.Id, _ gen.SetChargeInvoiceReferenceParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	cid, ok := s.pathID(w, r, chargeID)
	if !ok {
		return
	}
	var in gen.SetInvoiceReference
	ctx, ok := s.begin(w, r, "setChargeInvoiceReference", &in)
	if !ok {
		return
	}
	c, err := s.svc.SetChargeInvoiceReference(ctx, oid, cid, in.InvoiceReference.Number, dayStr(in.InvoiceReference.Date))
	s.reply(w, r, http.StatusOK, chargeDTO(oid, c), err)
}

func (s *Server) RecordPayment(w http.ResponseWriter, r *http.Request, organisationID, chargeID gen.Id, _ gen.RecordPaymentParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	cid, ok := s.pathID(w, r, chargeID)
	if !ok {
		return
	}
	var in gen.RecordPayment
	ctx, ok := s.begin(w, r, "recordPayment", &in)
	if !ok {
		return
	}
	p, err := s.svc.RecordPayment(ctx, oid, cid, service.PaymentInput{AmountP: in.Amount.AmountPaise, Method: string(in.Method), Reference: deref(in.Reference), ReceivedOn: dayStr(in.ReceivedOn)})
	s.reply(w, r, http.StatusCreated, paymentDTO(p), err)
}

func (s *Server) ReversePayment(w http.ResponseWriter, r *http.Request, organisationID, chargeID, paymentID gen.Id, _ gen.ReversePaymentParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	cid, ok := s.pathID(w, r, chargeID)
	if !ok {
		return
	}
	pid, ok := s.pathID(w, r, paymentID)
	if !ok {
		return
	}
	var in gen.ReasonBody
	ctx, ok := s.begin(w, r, "reversePayment", &in)
	if !ok {
		return
	}
	p, err := s.svc.ReversePayment(ctx, oid, cid, pid, in.Reason)
	s.reply(w, r, http.StatusCreated, paymentDTO(p), err)
}

func (s *Server) ListChargePayments(w http.ResponseWriter, r *http.Request, organisationID, chargeID gen.Id, p gen.ListChargePaymentsParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	cid, ok := s.pathID(w, r, chargeID)
	if !ok {
		return
	}
	after, n, err := pageParams(p.Cursor, (*int)(p.Limit), 25, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListChargePayments(r.Context(), oid, cid, after, n)
	out := gen.LedgerPaymentPage{Items: []gen.LedgerPayment{}, NextCursor: nextCursor(pg.Next)}
	for _, x := range pg.Items {
		out.Items = append(out.Items, paymentDTO(x))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

// ---- work lists and export ----

func (s *Server) ListLedgerEntries(w http.ResponseWriter, r *http.Request, listKind gen.LedgerListKind, p gen.ListLedgerEntriesParams) {
	c, err := page.Decode(p.Cursor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var afterDue string
	var afterID ids.ID
	if c != "" {
		// The cursor is "<due date>|<subject id>"; a bare id (no date) continues from the earliest date.
		due, id, found := strings.Cut(c, "|")
		if !found {
			due, id = "", c
		}
		parsed, perr := ids.Parse(id)
		if perr != nil {
			s.fail(w, r, model.Invalid("cursor", "invalid", "invalid cursor"))
			return
		}
		afterDue, afterID = due, parsed
	}
	n, err := page.Limit((*int)(p.Limit), 25, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListLedger(r.Context(), string(listKind), afterDue, afterID, n)
	out := gen.LedgerListRowPage{Items: []gen.LedgerListRow{}}
	for _, row := range pg.Rows {
		d := gen.LedgerListRow{OrganisationId: row.OrganisationID.String(), OrganisationName: row.OrganisationName, DueDate: datePtr(row.DueDate), AgeDays: row.AgeDays}
		if row.SubjectID != "" {
			sid := row.SubjectID.String()
			d.SubjectId = &sid
		}
		if row.AmountP != nil {
			m := money(*row.AmountP)
			d.Amount = &m
		}
		out.Items = append(out.Items, d)
	}
	if pg.HasMore {
		cur := page.Encode(pg.NextDue + "|" + pg.NextID.String())
		out.NextCursor = &cur
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) ExportLedger(w http.ResponseWriter, r *http.Request, p gen.ExportLedgerParams) {
	b, err := s.svc.ExportLedger(r.Context(), optDate(p.From), optDate(p.To))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ledger.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ---- lifecycle under a vendor approval grant ----

func (s *Server) lifecycle(w http.ResponseWriter, r *http.Request, op, kind string, organisationID gen.Id, grant string) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	var in gen.LifecycleRequest
	ctx, ok := s.begin(w, r, op, &in)
	if !ok {
		return
	}
	res, err := s.svc.ChangeOrganisationStatus(ctx, service.LifecycleInput{Kind: kind, OrganisationID: oid, Grant: grant, Ticket: in.Ticket, Reason: in.Reason,
		ExportOffered: deref(in.OwnerExportOffered)})
	s.reply(w, r, http.StatusOK, gen.LifecycleResult{OrganisationId: res.OrganisationID.String(), Status: gen.OrganisationStatus(res.Status), ChangedAt: res.ChangedAt}, err)
}

func (s *Server) SuspendOrganisation(w http.ResponseWriter, r *http.Request, organisationID gen.Id, p gen.SuspendOrganisationParams) {
	s.lifecycle(w, r, "suspendOrganisation", service.KindSuspend, organisationID, p.HostryVendorApproval)
}

func (s *Server) ReinstateOrganisation(w http.ResponseWriter, r *http.Request, organisationID gen.Id, p gen.ReinstateOrganisationParams) {
	s.lifecycle(w, r, "reinstateOrganisation", service.KindReinstate, organisationID, p.HostryVendorApproval)
}

func (s *Server) CloseOrganisation(w http.ResponseWriter, r *http.Request, organisationID gen.Id, p gen.CloseOrganisationParams) {
	s.lifecycle(w, r, "closeOrganisation", service.KindClose, organisationID, p.HostryVendorApproval)
}

// ---- outlet registration change, bill books, link invitations ----

func (s *Server) ChangeOutletTaxRegistration(w http.ResponseWriter, r *http.Request, outletID gen.Id, _ gen.ChangeOutletTaxRegistrationParams) {
	id, ok := s.pathID(w, r, outletID)
	if !ok {
		return
	}
	var in gen.ChangeOutletTaxRegistration
	ctx, ok := s.begin(w, r, "changeOutletTaxRegistration", &in)
	if !ok {
		return
	}
	o, err := s.svc.ChangeOutletTaxRegistration(ctx, id, ids.ID(in.TaxRegistrationId), in.Reason)
	s.reply(w, r, http.StatusOK, outletDTO(o), err)
}

func bookDTO(b model.ManualBook) gen.ManualBookSeries {
	d := gen.ManualBookSeries{Id: b.ID.String(), TaxRegistrationId: b.TaxRegistrationID.String(), Code: b.Code, OutletIds: []string{}, Status: gen.ManualBookSeriesStatus(b.Status), CreatedAt: b.CreatedAt}
	for _, o := range b.OutletIDs {
		d.OutletIds = append(d.OutletIds, o.String())
	}
	return d
}

func (s *Server) ListManualBookSeries(w http.ResponseWriter, r *http.Request, p gen.ListManualBookSeriesParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListManualBooks(r.Context(), after, n)
	out := gen.ManualBookSeriesPage{Items: []gen.ManualBookSeries{}, NextCursor: nextCursor(pg.Next)}
	for _, b := range pg.Items {
		out.Items = append(out.Items, bookDTO(b))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) RegisterManualBookSeries(w http.ResponseWriter, r *http.Request, _ gen.RegisterManualBookSeriesParams) {
	var in gen.RegisterManualBook
	ctx, ok := s.begin(w, r, "registerManualBookSeries", &in)
	if !ok {
		return
	}
	b, err := s.svc.RegisterManualBook(ctx, service.RegisterBookInput{TaxRegistrationID: ids.ID(in.TaxRegistrationId), Code: in.Code, OutletIDs: toIDs(&in.OutletIds)})
	s.reply(w, r, http.StatusCreated, bookDTO(b), err)
}

func (s *Server) ReissueLinkInvitation(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.ReissueLinkInvitationParams) {
	id, ok := s.pathID(w, r, linkID)
	if !ok {
		return
	}
	ctx, ok := s.begin(w, r, "reissueLinkInvitation", nil)
	if !ok {
		return
	}
	p, err := s.svc.ReissueInvitation(ctx, id)
	s.reply(w, r, http.StatusOK, proposalDTO(p, viewer(r), s.clk.Now()), err)
}

func (s *Server) ReacceptLink(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.ReacceptLinkParams) {
	s.linkAction(w, r, "reacceptLink", linkID, nil, func(ctx ctxT, id ids.ID) (model.Link, error) { return s.svc.ReacceptLink(ctx, id) })
}

// proposalDTO renders a proposed link and its one-time code. On an idempotent replay the code is gone (it is
// never stored) and secret_withheld says so.
func proposalDTO(p service.Proposal, viewer ids.ID, now time.Time) gen.LinkProposal {
	out := gen.LinkProposal{Link: linkDTO(p.Link, viewer, now)}
	if p.Code != "" {
		c := p.Code
		out.InvitationCode = &c
	} else {
		t := true
		out.SecretWithheld = &t
	}
	return out
}

func (s *Server) ListOutletManualBookSeries(w http.ResponseWriter, r *http.Request, outletID gen.Id) {
	id, ok := s.pathID(w, r, outletID)
	if !ok {
		return
	}
	rows, err := s.svc.ListOutletManualBooks(r.Context(), id)
	out := gen.ManualBookSeriesPage{Items: []gen.ManualBookSeries{}}
	for _, x := range rows {
		out.Items = append(out.Items, bookDTO(x))
	}
	s.reply(w, r, http.StatusOK, out, err)
}
