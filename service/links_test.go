package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/service"
)

var (
	orgA = seed.Example.Org // proposer
	orgB = seed.Other.Org   // the intended acceptor
	orgC = seed.Third.Org   // a stranger holding a forwarded code
)

// proposed returns a link proposed by A (A grants test.echo.read outbound; B grants test.echo.post inbound).
func (f *fixture) proposed() (model.Link, string) {
	f.t.Helper()
	p, err := f.svc.ProposeLink(f.as(orgA), service.ProposeInput{ID: f.newID(), Template: "on_premises_outlet", OutletIDs: []ids.ID{seed.Outlet},
		Grants: []service.GrantIn{
			{Scope: "test.echo.read", Direction: "outbound", OutletIDs: []ids.ID{seed.Outlet}},
			{Scope: "test.echo.post", Direction: "inbound"},
		}})
	if err != nil {
		f.t.Fatal(err)
	}
	return p.Link, p.Code
}

func (f *fixture) active() model.Link {
	f.t.Helper()
	l, code := f.proposed()
	if _, err := f.svc.AcceptLink(f.as(orgB), code, []ids.ID{seed.Other.Outlet}); err != nil {
		f.t.Fatal(err)
	}
	l, err := f.svc.ConfirmLink(f.as(orgA), l.ID, orgB)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *fixture) decide(tenant ids.ID, link model.Link, scope string) service.Decision {
	f.t.Helper()
	d, err := f.svc.Decide(f.as(tenant), link.ID, scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

// T17 / TST-19: the proposer confirms who accepted; only then does anything become active.
func TestAcceptedLinkAuthorisesNothingUntilTheProposerConfirms(t *testing.T) {
	f := newFixture(t)
	l, code := f.proposed()
	if got := f.decide(orgA, l, "test.echo.read"); got.Allowed || got.Reason != "link_not_active" {
		t.Fatalf("a proposed link authorises nothing: %+v", got)
	}
	acc, err := f.svc.AcceptLink(f.as(orgB), code, []ids.ID{seed.Other.Outlet})
	if err != nil || acc.Status != model.LinkAccepted {
		t.Fatalf("accept: %+v %v", acc, err)
	}
	for _, who := range []ids.ID{orgA, orgB} {
		if got := f.decide(who, l, "test.echo.read"); got.Allowed {
			t.Fatalf("an accepted link must authorise nothing for %s: %+v", who, got)
		}
	}
	for _, g := range acc.Grants {
		if g.Status != model.GrantPending {
			t.Fatalf("no grant is live while accepted: %+v", g)
		}
	}
	// The proposer first sees who accepted: name, legal name, GSTIN, state, verified.
	seen, err := f.svc.GetLink(f.as(orgA), l.ID)
	if err != nil || seen.AcceptorID != orgB || seen.AcceptorParty.DisplayName != seed.Other.Name ||
		len(seen.AcceptorParty.Registrations) != 1 || seen.AcceptorParty.Registrations[0].GSTIN != seed.Other.GSTIN || !seen.AcceptorParty.Registrations[0].GSTINVerified {
		t.Fatalf("the proposer must see the accepting organisation: %+v %v", seen.AcceptorParty, err)
	}
	// A swap between viewing and confirming is refused; the acceptor cannot confirm for the proposer.
	if _, err := f.svc.ConfirmLink(f.as(orgA), l.ID, orgC); err == nil {
		t.Fatal("confirming a different organisation than the one shown must fail")
	} else {
		wantField(t, err, "counterparty_organisation_id", "counterparty_mismatch")
	}
	if _, err := f.svc.ConfirmLink(f.as(orgB), l.ID, orgB); err == nil {
		t.Fatal("the acceptor cannot confirm itself")
	} else {
		wantCode(t, err, "link_state_conflict")
	}
	act, err := f.svc.ConfirmLink(f.as(orgA), l.ID, orgB)
	if err != nil || act.Status != model.LinkActive || act.ConfirmedAt == nil {
		t.Fatalf("confirm: %+v %v", act, err)
	}
}

// T6 control-plane half (TST-20): allowed only for the exact grant in the grantee's favour.
func TestLinkDecisionMatrix(t *testing.T) {
	f := newFixture(t)
	l := f.active()
	tests := []struct {
		name    string
		tenant  ids.ID
		scope   string
		allowed bool
		reason  string
		owner   ids.ID
	}{
		{"grantee of the outbound grant is allowed, owner is the proposer", orgB, "test.echo.read", true, "", orgA},
		{"grantor of that grant is not its grantee", orgA, "test.echo.read", false, "scope_not_granted", ""},
		{"the reverse-direction grant allows the proposer, owner is the acceptor", orgA, "test.echo.post", true, "", orgB},
		{"a scope nobody granted", orgB, "test.echo.nothing", false, "scope_not_granted", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := f.decide(tc.tenant, l, tc.scope)
			if got.Allowed != tc.allowed || got.Reason != tc.reason || got.DataOwner != tc.owner || got.CacheSeconds > 30 {
				t.Fatalf("decision %+v", got)
			}
		})
	}
	t.Run("a third organisation using another pair's link id gets 404", func(t *testing.T) {
		_, err := f.svc.Decide(f.as(orgC), l.ID, "test.echo.read")
		wantCode(t, err, "not_found")
	})
	t.Run("the outlet constraint of the grant is returned", func(t *testing.T) {
		got := f.decide(orgB, l, "test.echo.read")
		if len(got.OutletIDs) != 1 || got.OutletIDs[0] != seed.Outlet {
			t.Fatalf("outlets %v", got.OutletIDs)
		}
	})
}

// T9 control-plane half (TST-21): after suspend, revoke or scope removal the decision refuses at once.
func TestSuspendResumeRevokeAndGrantRemovalTakeEffectAtOnce(t *testing.T) {
	f := newFixture(t)
	l := f.active()
	if _, err := f.svc.SuspendLink(f.as(orgB), l.ID, "under review"); err != nil {
		t.Fatal(err)
	}
	if got := f.decide(orgB, l, "test.echo.read"); got.Allowed || got.Reason != "link_not_active" {
		t.Fatalf("suspended: %+v", got)
	}
	if _, err := f.svc.ResumeLink(f.as(orgA), l.ID); err == nil {
		t.Fatal("only the suspending party may resume")
	} else {
		wantCode(t, err, "link_state_conflict")
	}
	if _, err := f.svc.ResumeLink(f.as(orgB), l.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.decide(orgB, l, "test.echo.read"); !got.Allowed {
		t.Fatalf("resumed: %+v", got)
	}
	// Removing one grant is unilateral and immediate, and leaves the other grant alone.
	var readGrant ids.ID
	cur, _ := f.svc.GetLink(f.as(orgA), l.ID)
	for _, g := range cur.Grants {
		if g.Scope == "test.echo.read" {
			readGrant = g.ID
		}
	}
	if err := f.svc.RemoveGrant(f.as(orgA), l.ID, readGrant); err != nil {
		t.Fatal(err)
	}
	if got := f.decide(orgB, l, "test.echo.read"); got.Allowed || got.Reason != "scope_not_granted" {
		t.Fatalf("grant removed: %+v", got)
	}
	if got := f.decide(orgA, l, "test.echo.post"); !got.Allowed {
		t.Fatalf("the other grant stays: %+v", got)
	}
	if err := f.svc.RemoveGrant(f.as(orgC), l.ID, readGrant); err == nil {
		t.Fatal("a third organisation cannot touch the link")
	}
	// Revocation is terminal and idempotent.
	for i := 0; i < 2; i++ {
		if rv, err := f.svc.RevokeLink(f.as(orgA), l.ID, "contract ended"); err != nil || rv.Status != model.LinkRevoked {
			t.Fatalf("revoke #%d: %+v %v", i, rv, err)
		}
	}
	if got := f.decide(orgA, l, "test.echo.post"); got.Allowed {
		t.Fatalf("revoked: %+v", got)
	}
	if _, err := f.svc.ResumeLink(f.as(orgA), l.ID); err == nil {
		t.Fatal("a revoked link cannot be resumed")
	}
}

// T17: a forwarded code accepted by a third organisation activates nothing until confirmed, and rejection ends the link.
func TestForwardedCodeAcceptedByTheWrongOrganisation(t *testing.T) {
	f := newFixture(t)
	l, code := f.proposed()
	acc, err := f.svc.AcceptLink(f.as(orgC), code, nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.AcceptorParty.DisplayName != seed.Third.Name || acc.Status != model.LinkAccepted {
		t.Fatalf("%+v", acc)
	}
	// The intended acceptor now holds a used code.
	if _, err := f.svc.AcceptLink(f.as(orgB), code, nil); err == nil {
		t.Fatal("the code is single use")
	} else {
		wantCode(t, err, "invitation_code_invalid")
	}
	if got := f.decide(orgA, l, "test.echo.read"); got.Allowed {
		t.Fatalf("nothing is active until the proposer confirms: %+v", got)
	}
	rej, err := f.svc.DeclineLink(f.as(orgA), l.ID, "not our partner")
	if err != nil || rej.Status != model.LinkDeclined {
		t.Fatalf("the proposer rejects the accepted link: %+v %v", rej, err)
	}
	if _, err := f.svc.ConfirmLink(f.as(orgA), l.ID, orgC); err == nil {
		t.Fatal("a rejected link cannot be confirmed")
	}
}

func TestInvitationCodeRulesAreOneAnswerForEverything(t *testing.T) {
	f := newFixture(t)
	l, code := f.proposed()
	tests := []struct{ name, code string }{
		{"unknown code", "7QK3M9XD7QK3M9XD"},
		{"malformed code", "short"},
		{"forbidden symbols", "IIIIIIIIIIIIIIII"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.AcceptLink(f.as(orgB), tc.code, nil)
			wantCode(t, err, "invitation_code_invalid")
		})
	}
	t.Run("the proposer cannot accept its own link", func(t *testing.T) {
		_, err := f.svc.AcceptLink(f.as(orgA), code, nil)
		wantCode(t, err, "invitation_code_invalid")
	})
	t.Run("expired after 14 days", func(t *testing.T) {
		f.clk.Advance(service.InviteTTL + time.Second)
		_, err := f.svc.AcceptLink(f.as(orgB), code, nil)
		wantCode(t, err, "invitation_code_invalid")
		got, _ := f.svc.GetLink(f.as(orgA), l.ID)
		if service.EffectiveStatus(got, f.clk.Now()) != model.LinkExpired {
			t.Fatalf("status %s", service.EffectiveStatus(got, f.clk.Now()))
		}
		n, err := f.svc.ExpireDueLinks(f.as(orgA))
		if err != nil || n != 1 {
			t.Fatalf("sweep expired %d, %v", n, err)
		}
	})
	t.Run("the code has at least 80 bits: 16 symbols of a 32-symbol alphabet, never repeated", func(t *testing.T) {
		real := service.New(f.store, service.Options{Clock: f.clk, IDs: f.gen}) // crypto/rand
		seen := map[string]bool{}
		for i := 0; i < 50; i++ {
			p, err := real.ProposeLink(f.as(orgA), service.ProposeInput{ID: f.newID(), Template: "supplier_buyer", Grants: []service.GrantIn{{Scope: "test.echo.read", Direction: "outbound"}}})
			if err != nil || len(p.Code) != 16 || seen[p.Code] {
				t.Fatalf("code %q: %v", p.Code, err)
			}
			seen[p.Code] = true
		}
	})
}

func TestRedemptionIsRateLimitedPerTenant(t *testing.T) {
	f := newFixture(t)
	f.proposed()
	var last error
	for i := 0; i < 11; i++ {
		_, last = f.svc.AcceptLink(f.as(orgC), "7QK3M9XD7QK3M9XD", nil)
	}
	var ra *service.RetryAfter
	if !errors.As(last, &ra) || ra.Seconds < 1 {
		t.Fatalf("the 11th attempt must be rate limited with a Retry-After, got %v", last)
	}
	wantCode(t, last, "rate_limited")
	// Another tenant is not affected by C's attempts.
	_, err := f.svc.AcceptLink(f.as(orgB), "7QK3M9XD7QK3M9XD", nil)
	wantCode(t, err, "invitation_code_invalid")
	f.clk.Advance(11 * time.Minute)
	_, err = f.svc.AcceptLink(f.as(orgC), "7QK3M9XD7QK3M9XD", nil)
	wantCode(t, err, "invitation_code_invalid")
}

// TST-22: a grantee cannot add or widen a grant; an amendment needs the other party.
func TestAmendmentsNeedTheOtherParty(t *testing.T) {
	f := newFixture(t)
	l := f.active()
	add := []service.GrantIn{{Scope: "test.echo.read", Direction: "inbound"}} // B would grant read to A
	am, err := f.svc.ProposeAmendment(f.as(orgA), l.ID, f.newID(), add)
	if err != nil || am.Status != model.AmendPending {
		t.Fatalf("propose: %+v %v", am, err)
	}
	if got := f.decide(orgA, l, "test.echo.read"); got.Allowed {
		t.Fatalf("a proposed amendment is not active: %+v", got)
	}
	if _, err := f.svc.DecideAmendment(f.as(orgA), l.ID, am.ID, true); err == nil {
		t.Fatal("a party cannot accept its own amendment")
	}
	if _, err := f.svc.DecideAmendment(f.as(orgC), l.ID, am.ID, true); err == nil {
		t.Fatal("a third organisation cannot decide")
	}
	done, err := f.svc.DecideAmendment(f.as(orgB), l.ID, am.ID, true)
	if err != nil || done.Status != model.AmendAccepted {
		t.Fatalf("accept: %+v %v", done, err)
	}
	if got := f.decide(orgA, l, "test.echo.read"); !got.Allowed || got.DataOwner != orgB {
		t.Fatalf("the accepted grant is active, with the acceptor as the data owner: %+v", got)
	}
	// Repeating a scope and direction that already exists is refused; a decline adds nothing.
	if _, err := f.svc.ProposeAmendment(f.as(orgA), l.ID, f.newID(), add); err == nil {
		t.Fatal("duplicate grant")
	}
	b2, _ := f.svc.ProposeAmendment(f.as(orgB), l.ID, f.newID(), []service.GrantIn{{Scope: "test.echo.post", Direction: "inbound"}})
	if declined, err := f.svc.DecideAmendment(f.as(orgA), l.ID, b2.ID, false); err != nil || declined.Status != model.AmendDeclined {
		t.Fatalf("decline: %+v %v", declined, err)
	}
	if got := f.decide(orgB, l, "test.echo.post"); got.Allowed {
		t.Fatalf("a declined amendment adds nothing: %+v", got)
	}
	// An amendment only exists on an active link.
	f.svc.SuspendLink(f.as(orgA), l.ID, "pause")
	if _, err := f.svc.ProposeAmendment(f.as(orgA), l.ID, f.newID(), []service.GrantIn{{Scope: "test.echo.post", Direction: "outbound"}}); err == nil {
		t.Fatal("no amendment on a suspended link")
	}
}

func TestProposalValidation(t *testing.T) {
	f := newFixture(t)
	base := service.ProposeInput{Template: "on_premises_outlet", Grants: []service.GrantIn{{Scope: "test.echo.read", Direction: "outbound"}}}
	tests := []struct {
		name   string
		mutate func(in *service.ProposeInput)
		field  string
		code   string
	}{
		{"a scope outside the platform catalogue", func(in *service.ProposeInput) { in.Grants[0].Scope = "tenant.invented.scope" }, "grants", "unknown_scope"},
		{"unknown template", func(in *service.ProposeInput) { in.Template = "weird" }, "template", "unknown"},
		{"no grants", func(in *service.ProposeInput) { in.Grants = nil }, "grants", "length"},
		{"duplicate grant", func(in *service.ProposeInput) { in.Grants = append(in.Grants, in.Grants[0]) }, "grants", "duplicate"},
		{"an outlet the proposer does not have", func(in *service.ProposeInput) { in.OutletIDs = []ids.ID{seed.Other.Outlet} }, "outlet_ids", "unknown_outlet"},
		{"id is not a UUIDv7", func(in *service.ProposeInput) { in.ID = "01ARZ3NDEKTSV4RRFFQ69G5FAV" }, "id", "uuidv7_required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.ID = f.newID()
			in.Grants = append([]service.GrantIn(nil), base.Grants...)
			tc.mutate(&in)
			_, err := f.svc.ProposeLink(f.as(orgA), in)
			wantField(t, err, tc.field, tc.code)
		})
	}
}

// TST-23 / TC-17: every state change writes one link_activity row; no payload; both parties can read.
func TestEveryStateChangeWritesOneActivityRowReadableByBothParties(t *testing.T) {
	f := newFixture(t)
	l := f.active()
	f.svc.SuspendLink(f.as(orgA), l.ID, "pause")
	f.svc.ResumeLink(f.as(orgA), l.ID)
	f.svc.RevokeLink(f.as(orgB), l.ID, "contract ended")
	want := []string{"link.proposed", "link.accepted", "link.confirmed", "link.suspended", "link.resumed", "link.revoked"}
	for _, who := range []ids.ID{orgA, orgB} {
		pg, err := f.svc.ListActivity(f.as(who), l.ID, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, a := range pg.Items {
			got = append(got, a.Action)
			if a.Outcome != "succeeded" {
				t.Fatalf("%+v", a)
			}
		}
		if marshal(t, got) != marshal(t, want) {
			t.Fatalf("%s sees %v, want %v", who, got, want)
		}
	}
	if _, err := f.svc.ListActivity(f.as(orgC), l.ID, "", 50); err == nil {
		t.Fatal("a third organisation cannot read the log")
	}
}

// Link events are addressed: they carry the counterparty and the link once the counterparty is known.
func TestLinkEventsAreAddressedToTheCounterparty(t *testing.T) {
	f := newFixture(t)
	l := f.active()
	f.svc.SuspendLink(f.as(orgB), l.ID, "pause")
	var proposed, suspended bool
	for _, e := range f.store.Events() {
		switch e.Type {
		case "tenancy.link.proposed":
			proposed = e.TenantID == orgA && e.CounterpartyTenantID == "" && e.LinkID == ""
		case "tenancy.link.suspended":
			suspended = e.TenantID == orgB && e.CounterpartyTenantID == orgA && e.LinkID == l.ID.String()
		}
	}
	if !proposed || !suspended {
		t.Fatalf("proposed=%v suspended=%v events=%d", proposed, suspended, len(f.store.Events()))
	}
}
