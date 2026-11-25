package api

import (
	"net/http"
	"strings"

	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/page"
	"github.com/hostryhq/svc-tenancy/service"
)

func nextCursor(next ids.ID) *string {
	if next == "" {
		return nil
	}
	c := page.Encode(next.String())
	return &c
}

func deref[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}

// ---- organisation and settings ----

func (s *Server) GetCurrentOrganisation(w http.ResponseWriter, r *http.Request) {
	o, err := s.svc.GetOrganisation(r.Context())
	s.reply(w, r, http.StatusOK, orgDTO(o), err)
}

func (s *Server) UpdateCurrentOrganisation(w http.ResponseWriter, r *http.Request, _ gen.UpdateCurrentOrganisationParams) {
	var in gen.UpdateOrganisation
	ctx, ok := s.begin(w, r, "updateCurrentOrganisation", &in)
	if !ok {
		return
	}
	o, err := s.svc.RenameOrganisation(ctx, in.Name)
	s.reply(w, r, http.StatusOK, orgDTO(o), err)
}

func (s *Server) GetOrganisationSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetSettings(r.Context())
	s.reply(w, r, http.StatusOK, settingsDTO(v), err)
}

func (s *Server) UpdateOrganisationSettings(w http.ResponseWriter, r *http.Request, _ gen.UpdateOrganisationSettingsParams) {
	var in gen.UpdateOrganisationSettings
	ctx, ok := s.begin(w, r, "updateOrganisationSettings", &in)
	if !ok {
		return
	}
	v, err := s.svc.UpdateSettings(ctx, in.OfflineWindowDays, in.IdleLockMinutes)
	s.reply(w, r, http.StatusOK, settingsDTO(v), err)
}

func (s *Server) ListStates(w http.ResponseWriter, r *http.Request) {
	out := gen.StateList{Items: []gen.StateEntry{}}
	for _, st := range s.svc.ListStates() {
		out.Items = append(out.Items, gen.StateEntry{Code: st.Code, Name: st.Name})
	}
	s.reply(w, r, http.StatusOK, out, nil)
}

// ---- brands ----

func (s *Server) ListBrands(w http.ResponseWriter, r *http.Request, p gen.ListBrandsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListBrands(r.Context(), after, n)
	out := gen.BrandPage{Items: []gen.Brand{}, NextCursor: nextCursor(pg.Next)}
	for _, b := range pg.Items {
		out.Items = append(out.Items, brandDTO(b))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) CreateBrand(w http.ResponseWriter, r *http.Request, _ gen.CreateBrandParams) {
	var in gen.CreateBrand
	ctx, ok := s.begin(w, r, "createBrand", &in)
	if !ok {
		return
	}
	b, err := s.svc.CreateBrand(ctx, ids.ID(in.Id), in.Name)
	s.reply(w, r, http.StatusCreated, brandDTO(b), err)
}

func (s *Server) GetBrand(w http.ResponseWriter, r *http.Request, brandID gen.Id) {
	id, ok := s.pathID(w, r, brandID)
	if !ok {
		return
	}
	b, err := s.svc.GetBrand(r.Context(), id)
	s.reply(w, r, http.StatusOK, brandDTO(b), err)
}

// ---- outlets ----

func (s *Server) ListOutlets(w http.ResponseWriter, r *http.Request, p gen.ListOutletsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListOutlets(r.Context(), string(deref(p.Kind)), after, n)
	out := gen.OutletPage{Items: []gen.Outlet{}, NextCursor: nextCursor(pg.Next)}
	for _, o := range pg.Items {
		out.Items = append(out.Items, outletDTO(o))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) CreateOutlet(w http.ResponseWriter, r *http.Request, _ gen.CreateOutletParams) {
	var in gen.CreateOutlet
	ctx, ok := s.begin(w, r, "createOutlet", &in)
	if !ok {
		return
	}
	o, err := s.svc.CreateOutlet(ctx, service.OutletInput{ID: ids.ID(in.Id), BrandID: ids.ID(deref(in.BrandId)), Name: in.Name, Kind: string(in.Kind),
		StateCode: in.StateCode, TaxRegistrationID: ids.ID(in.TaxRegistrationId), LocatedInOutletID: ids.ID(deref(in.LocatedInOutletId))})
	s.reply(w, r, http.StatusCreated, outletDTO(o), err)
}

func (s *Server) GetOutlet(w http.ResponseWriter, r *http.Request, outletID gen.Id) {
	id, ok := s.pathID(w, r, outletID)
	if !ok {
		return
	}
	o, err := s.svc.GetOutlet(r.Context(), id)
	s.reply(w, r, http.StatusOK, outletDTO(o), err)
}

func (s *Server) UpdateOutlet(w http.ResponseWriter, r *http.Request, outletID gen.Id, _ gen.UpdateOutletParams) {
	id, ok := s.pathID(w, r, outletID)
	if !ok {
		return
	}
	var in gen.UpdateOutlet
	ctx, ok := s.begin(w, r, "updateOutlet", &in)
	if !ok {
		return
	}
	var status *string
	if in.Status != nil {
		v := string(*in.Status)
		status = &v
	}
	o, err := s.svc.UpdateOutlet(ctx, id, in.Name, status)
	s.reply(w, r, http.StatusOK, outletDTO(o), err)
}

// ---- tax registrations ----

func (s *Server) ListTaxRegistrations(w http.ResponseWriter, r *http.Request, p gen.ListTaxRegistrationsParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListRegistrations(r.Context(), after, n)
	out := gen.TaxRegistrationPage{Items: []gen.TaxRegistration{}, NextCursor: nextCursor(pg.Next)}
	for _, x := range pg.Items {
		out.Items = append(out.Items, regDTO(x))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) CreateTaxRegistration(w http.ResponseWriter, r *http.Request, _ gen.CreateTaxRegistrationParams) {
	var in gen.CreateTaxRegistration
	ctx, ok := s.begin(w, r, "createTaxRegistration", &in)
	if !ok {
		return
	}
	x, err := s.svc.CreateRegistration(ctx, service.RegistrationInput{ID: ids.ID(in.Id), LegalName: in.LegalName, Regime: string(in.Regime),
		GSTIN: deref(in.Gstin), StateCode: in.StateCode})
	s.reply(w, r, http.StatusCreated, regDTO(x), err)
}

func (s *Server) GetTaxRegistration(w http.ResponseWriter, r *http.Request, registrationID gen.Id) {
	id, ok := s.pathID(w, r, registrationID)
	if !ok {
		return
	}
	x, err := s.svc.GetRegistration(r.Context(), id)
	s.reply(w, r, http.StatusOK, regDTO(x), err)
}

func (s *Server) UpdateTaxRegistration(w http.ResponseWriter, r *http.Request, registrationID gen.Id, _ gen.UpdateTaxRegistrationParams) {
	id, ok := s.pathID(w, r, registrationID)
	if !ok {
		return
	}
	var in gen.UpdateTaxRegistration
	ctx, ok := s.begin(w, r, "updateTaxRegistration", &in)
	if !ok {
		return
	}
	x, err := s.svc.UpdateRegistration(ctx, id, in.LegalName, in.Gstin)
	s.reply(w, r, http.StatusOK, regDTO(x), err)
}

func (s *Server) CloseTaxRegistration(w http.ResponseWriter, r *http.Request, registrationID gen.Id, _ gen.CloseTaxRegistrationParams) {
	id, ok := s.pathID(w, r, registrationID)
	if !ok {
		return
	}
	var in gen.CloseRegistration
	ctx, ok := s.begin(w, r, "closeTaxRegistration", &in)
	if !ok {
		return
	}
	x, err := s.svc.CloseRegistration(ctx, id, in.Reason)
	s.reply(w, r, http.StatusOK, regDTO(x), err)
}

// ---- document-series registry ----

func (s *Server) ListDocumentSeries(w http.ResponseWriter, r *http.Request, p gen.ListDocumentSeriesParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	filter := ids.ID(deref(p.OutletId))
	if filter != "" {
		if _, err := ids.Parse(filter.String()); err != nil { // an unparseable id names nothing: empty result
			s.reply(w, r, http.StatusOK, gen.DocumentSeriesPage{Items: []gen.DocumentSeries{}}, nil)
			return
		}
	}
	pg, err := s.svc.ListSeries(r.Context(), filter, string(deref(p.Status)), after, n)
	out := gen.DocumentSeriesPage{Items: []gen.DocumentSeries{}, NextCursor: nextCursor(pg.Next)}
	for _, x := range pg.Items {
		out.Items = append(out.Items, seriesDTO(x))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) CloseDocumentSeriesUnreconciled(w http.ResponseWriter, r *http.Request, seriesID gen.Id, _ gen.CloseDocumentSeriesUnreconciledParams) {
	id, ok := s.pathID(w, r, seriesID)
	if !ok {
		return
	}
	var in gen.CloseSeries
	ctx, ok := s.begin(w, r, "closeDocumentSeriesUnreconciled", &in)
	if !ok {
		return
	}
	x, err := s.svc.CloseSeriesUnreconciled(ctx, id, in.Reason)
	s.reply(w, r, http.StatusOK, seriesDTO(x), err)
}

func (s *Server) IssueDocumentSeries(w http.ResponseWriter, r *http.Request, _ gen.IssueDocumentSeriesParams) {
	var in gen.IssueSeries
	ctx, ok := s.begin(w, r, "issueDocumentSeries", &in)
	if !ok {
		return
	}
	types := make([]string, len(in.DocTypes))
	for i, d := range in.DocTypes {
		types[i] = string(d)
	}
	rows, err := s.svc.IssueSeries(ctx, service.IssueInput{OutletID: ids.ID(in.OutletId), DeviceID: ids.ID(in.DeviceId), DocTypes: types, State: string(in.State)})
	out := gen.SeriesIssuance{Items: []gen.DocumentSeries{}}
	for _, x := range rows {
		out.Items = append(out.Items, seriesDTO(x))
	}
	s.reply(w, r, http.StatusCreated, out, err)
}

func (s *Server) CloseDeviceSeries(w http.ResponseWriter, r *http.Request, deviceID gen.Id, _ gen.CloseDeviceSeriesParams) {
	id, ok := s.pathID(w, r, deviceID)
	if !ok {
		return
	}
	var in gen.CloseDeviceSeries
	ctx, ok := s.begin(w, r, "closeDeviceSeries", &in)
	if !ok {
		return
	}
	n, err := s.svc.CloseDeviceSeries(ctx, id, string(in.Reason))
	s.reply(w, r, http.StatusOK, gen.SeriesClosure{Closed: n}, err)
}

// ---- links (registered only when LINKS_ENABLED) ----

func viewer(r *http.Request) ids.ID {
	tc, _ := tenancy.From(r.Context())
	return tc.TenantID
}

func grantsIn(in []gen.GrantInput) []service.GrantIn {
	out := make([]service.GrantIn, len(in))
	for i, g := range in {
		out[i] = service.GrantIn{Scope: g.Scope, Direction: string(g.Direction), OutletIDs: toIDs(g.OutletIds)}
	}
	return out
}

func (s *Server) ListLinks(w http.ResponseWriter, r *http.Request, p gen.ListLinksParams) {
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListLinks(r.Context(), string(deref(p.Status)), after, n)
	out := gen.LinkPage{Items: []gen.Link{}, NextCursor: nextCursor(pg.Next)}
	for _, l := range pg.Items {
		out.Items = append(out.Items, linkDTO(l, viewer(r), s.clk.Now()))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) ProposeLink(w http.ResponseWriter, r *http.Request, _ gen.ProposeLinkParams) {
	var in gen.ProposeLink
	ctx, ok := s.begin(w, r, "proposeLink", &in)
	if !ok {
		return
	}
	p, err := s.svc.ProposeLink(ctx, service.ProposeInput{ID: ids.ID(in.Id), Template: string(in.Template), Grants: grantsIn(in.Grants), OutletIDs: toIDs(in.OutletIds)})
	s.reply(w, r, http.StatusCreated, proposalDTO(p, viewer(r), s.clk.Now()), err)
}

func (s *Server) GetLink(w http.ResponseWriter, r *http.Request, linkID gen.Id) {
	id, ok := s.pathID(w, r, linkID)
	if !ok {
		return
	}
	l, err := s.svc.GetLink(r.Context(), id)
	s.reply(w, r, http.StatusOK, linkDTO(l, viewer(r), s.clk.Now()), err)
}

func (s *Server) AcceptLink(w http.ResponseWriter, r *http.Request, _ gen.AcceptLinkParams) {
	var in gen.AcceptLink
	ctx, ok := s.begin(w, r, "acceptLink", &in)
	if !ok {
		return
	}
	l, err := s.svc.AcceptLink(ctx, strings.ToUpper(in.InvitationCode), toIDs(in.OutletIds))
	s.reply(w, r, http.StatusCreated, linkDTO(l, viewer(r), s.clk.Now()), err)
}

// linkAction runs one of the single-link transitions that return the link.
func (s *Server) linkAction(w http.ResponseWriter, r *http.Request, op string, linkID gen.Id, body any, do func(ctx ctxT, id ids.ID) (model.Link, error)) {
	id, ok := s.pathID(w, r, linkID)
	if !ok {
		return
	}
	ctx, ok := s.begin(w, r, op, body)
	if !ok {
		return
	}
	l, err := do(ctx, id)
	s.reply(w, r, http.StatusOK, linkDTO(l, viewer(r), s.clk.Now()), err)
}

func (s *Server) ConfirmLink(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.ConfirmLinkParams) {
	var in gen.ConfirmLink
	s.linkAction(w, r, "confirmLink", linkID, &in, func(ctx ctxT, id ids.ID) (model.Link, error) {
		return s.svc.ConfirmLink(ctx, id, ids.ID(in.CounterpartyOrganisationId))
	})
}

func (s *Server) DeclineLink(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.DeclineLinkParams) {
	var in gen.LinkReason
	s.linkAction(w, r, "declineLink", linkID, &in, func(ctx ctxT, id ids.ID) (model.Link, error) { return s.svc.DeclineLink(ctx, id, in.Reason) })
}

func (s *Server) SuspendLink(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.SuspendLinkParams) {
	var in gen.LinkReason
	s.linkAction(w, r, "suspendLink", linkID, &in, func(ctx ctxT, id ids.ID) (model.Link, error) { return s.svc.SuspendLink(ctx, id, in.Reason) })
}

func (s *Server) ResumeLink(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.ResumeLinkParams) {
	s.linkAction(w, r, "resumeLink", linkID, nil, func(ctx ctxT, id ids.ID) (model.Link, error) { return s.svc.ResumeLink(ctx, id) })
}

func (s *Server) RevokeLink(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.RevokeLinkParams) {
	var in gen.LinkReason
	s.linkAction(w, r, "revokeLink", linkID, &in, func(ctx ctxT, id ids.ID) (model.Link, error) { return s.svc.RevokeLink(ctx, id, in.Reason) })
}

func (s *Server) ProposeLinkAmendment(w http.ResponseWriter, r *http.Request, linkID gen.Id, _ gen.ProposeLinkAmendmentParams) {
	id, ok := s.pathID(w, r, linkID)
	if !ok {
		return
	}
	var in gen.LinkAmendmentInput
	ctx, ok := s.begin(w, r, "proposeLinkAmendment", &in)
	if !ok {
		return
	}
	a, err := s.svc.ProposeAmendment(ctx, id, ids.ID(in.Id), grantsIn(in.AddGrants))
	s.reply(w, r, http.StatusCreated, amendmentDTO(a, a.ProposedBySide), err)
}

func (s *Server) DecideLinkAmendment(w http.ResponseWriter, r *http.Request, linkID, amendmentID gen.Id, _ gen.DecideLinkAmendmentParams) {
	lid, ok := s.pathID(w, r, linkID)
	if !ok {
		return
	}
	aid, ok := s.pathID(w, r, amendmentID)
	if !ok {
		return
	}
	var in gen.DecideAmendment
	ctx, ok := s.begin(w, r, "decideLinkAmendment", &in)
	if !ok {
		return
	}
	if in.Decision != "accept" && in.Decision != "decline" {
		s.fail(w, r, model.Invalid("decision", "unknown", "decision must be accept or decline"))
		return
	}
	a, err := s.svc.DecideAmendment(ctx, lid, aid, in.Decision == "accept")
	s.reply(w, r, http.StatusOK, amendmentDTO(a, otherSide(a.ProposedBySide)), err)
}

func (s *Server) RemoveLinkGrant(w http.ResponseWriter, r *http.Request, linkID, grantID gen.Id, _ gen.RemoveLinkGrantParams) {
	lid, ok := s.pathID(w, r, linkID)
	if !ok {
		return
	}
	gid, ok := s.pathID(w, r, grantID)
	if !ok {
		return
	}
	ctx, ok := s.begin(w, r, "removeLinkGrant", nil)
	if !ok {
		return
	}
	if err := s.svc.RemoveGrant(ctx, lid, gid); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ListLinkActivity(w http.ResponseWriter, r *http.Request, linkID gen.Id, p gen.ListLinkActivityParams) {
	id, ok := s.pathID(w, r, linkID)
	if !ok {
		return
	}
	after, n, err := pageParams(p.Cursor, p.Limit, 50, 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg, err := s.svc.ListActivity(r.Context(), id, after, n)
	out := gen.LinkActivityPage{Items: []gen.LinkActivity{}, NextCursor: nextCursor(pg.Next)}
	for _, a := range pg.Items {
		out.Items = append(out.Items, activityDTO(a))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) GetLinkDecision(w http.ResponseWriter, r *http.Request, p gen.GetLinkDecisionParams) {
	id, ok := s.pathID(w, r, p.LinkId)
	if !ok {
		return
	}
	d, err := s.svc.Decide(r.Context(), id, p.Scope)
	out := gen.LinkDecision{Allowed: d.Allowed, CacheSeconds: d.CacheSeconds, OutletIds: idStrings(d.OutletIDs)}
	if d.Reason != "" {
		rs := gen.LinkDecisionReason(d.Reason)
		out.Reason = &rs
	}
	if d.DataOwner != "" {
		o := d.DataOwner.String()
		out.DataOwnerOrganisationId = &o
	}
	s.reply(w, r, http.StatusOK, out, err)
}

// ---- vendor operations (admin service only) ----

func (s *Server) ListOrganisationDirectory(w http.ResponseWriter, r *http.Request, p gen.ListOrganisationDirectoryParams) {
	after, n, err := pageParams(p.Cursor, (*int)(p.Limit), 25, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := service.DirectoryQuery{Q: deref(p.Q), Status: string(deref(p.Status)), After: after, Limit: n}
	if raw := deref(p.OrganisationIds); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, err := ids.Parse(strings.TrimSpace(part))
			if err != nil {
				s.fail(w, r, model.Invalid("organisation_ids", "invalid", "not a list of organisation ids"))
				return
			}
			q.OrganisationIDs = append(q.OrganisationIDs, id)
		}
	}
	pg, err := s.svc.ListDirectory(r.Context(), q)
	out := gen.DirectoryPage{Items: []gen.DirectoryOrganisation{}, NextCursor: nextCursor(pg.Next)}
	for _, o := range pg.Items {
		out.Items = append(out.Items, dirOrgDTO(o))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) GetOrganisationDirectoryEntry(w http.ResponseWriter, r *http.Request, organisationID gen.Id, _ gen.GetOrganisationDirectoryEntryParams) {
	id, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	d, err := s.svc.GetOrgDetail(r.Context(), id)
	s.reply(w, r, http.StatusOK, detailDTO(d), err)
}

func (s *Server) CreateOrganisation(w http.ResponseWriter, r *http.Request, _ gen.CreateOrganisationParams) {
	var in gen.CreateOrganisation
	ctx, ok := s.begin(w, r, "createOrganisation", &in)
	if !ok {
		return
	}
	im, _ := service.IdemFrom(ctx)
	ci := service.CreateOrgInput{Name: in.Name, OutletName: in.Outlet.Name, OutletKind: string(in.Outlet.Kind), OutletState: in.Outlet.StateCode,
		LegalName: in.TaxRegistration.LegalName, Regime: string(in.TaxRegistration.Regime), GSTIN: deref(in.TaxRegistration.Gstin),
		Key: im.Key, Fingerprint: im.Fingerprint}
	if in.Verification != nil {
		ci.VerifyMethod, ci.VerifyNote = string(in.Verification.Method), deref(in.Verification.Note)
	}
	c, err := s.svc.CreateOrganisation(ctx, ci)
	s.reply(w, r, http.StatusCreated, gen.OrganisationCreated{Organisation: orgDTO(c.Org), Brand: brandDTO(c.Brand), Outlet: outletDTO(c.Outlet),
		TaxRegistration: regDTO(c.Registration)}, err)
}

func (s *Server) VerifyTaxRegistration(w http.ResponseWriter, r *http.Request, organisationID, registrationID gen.Id, _ gen.VerifyTaxRegistrationParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	rid, ok := s.pathID(w, r, registrationID)
	if !ok {
		return
	}
	var in gen.VerifyRegistration
	ctx, ok := s.begin(w, r, "verifyTaxRegistration", &in)
	if !ok {
		return
	}
	x, err := s.svc.VerifyRegistration(ctx, oid, rid, service.VerifyInput{Method: string(in.Method), Note: deref(in.Note)})
	s.reply(w, r, http.StatusOK, regDTO(x), err)
}

func (s *Server) RejectTaxRegistration(w http.ResponseWriter, r *http.Request, organisationID, registrationID gen.Id, _ gen.RejectTaxRegistrationParams) {
	oid, ok := s.pathID(w, r, organisationID)
	if !ok {
		return
	}
	rid, ok := s.pathID(w, r, registrationID)
	if !ok {
		return
	}
	var in gen.RejectRegistration
	ctx, ok := s.begin(w, r, "rejectTaxRegistration", &in)
	if !ok {
		return
	}
	x, err := s.svc.RejectRegistration(ctx, oid, rid, in.Reason)
	s.reply(w, r, http.StatusOK, regDTO(x), err)
}

func otherSide(side string) string {
	if side == model.SideProposer {
		return model.SideAcceptor
	}
	return model.SideProposer
}

func (s *Server) ListDeviceSeries(w http.ResponseWriter, r *http.Request, deviceID gen.Id) {
	id, ok := s.pathID(w, r, deviceID)
	if !ok {
		return
	}
	rows, err := s.svc.ListDeviceSeries(r.Context(), id)
	out := gen.SeriesIssuance{Items: []gen.DocumentSeries{}}
	for _, x := range rows {
		out.Items = append(out.Items, seriesDTO(x))
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) GetStreamOwner(w http.ResponseWriter, r *http.Request, streamID gen.Id) {
	id, ok := s.pathID(w, r, streamID)
	if !ok {
		return
	}
	o, err := s.svc.GetStreamOwner(r.Context(), id)
	out := gen.StreamOwner{StreamId: o.StreamID.String(), Kind: gen.StreamOwnerKind(o.Kind), OrganisationId: o.OrganisationID.String()}
	if o.BrandID != "" {
		b := o.BrandID.String()
		out.BrandId = &b
	}
	s.reply(w, r, http.StatusOK, out, err)
}
