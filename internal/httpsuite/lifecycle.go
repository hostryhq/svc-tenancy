package httpsuite

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// billingEvent is an event as billing publishes it, for the bus consumer to apply.
func billingEvent(typ string, tenant, outlet ids.ID, key string, at time.Time, data any) events.Envelope {
	b, _ := json.Marshal(data)
	return events.Envelope{ID: ids.New(), Type: typ, V: 1, Source: "billing", TenantID: tenant, OutletID: outlet, Key: key, KeySeq: 1, OccurredAt: at, Data: b}
}

// drainedEvent is identity.device.drained for one series, as identity publishes it.
func drainedEvent(tenant, outlet ids.ID, series string, last int, at time.Time) events.Envelope {
	b, _ := json.Marshal(obj{"device_id": ids.New().String(), "drained_at": at.Format(time.RFC3339Nano), "series": []obj{{"series_id": series, "last_number": last}}})
	return events.Envelope{ID: ids.New(), Type: "identity.device.drained", V: 1, Source: "identity", TenantID: tenant, OutletID: outlet, Key: series, KeySeq: 1, OccurredAt: at, Data: b}
}

// tradingDay delivers billing.trading_day.changed for an outlet; seq orders two changes of one date.
func tradingDay(t testing.TB, g *Target, tenant, outlet ids.ID, date, state string, seq int) {
	t.Helper()
	at := seed.At.Add(time.Duration(seq) * time.Minute)
	g.Deliver(t, billingEvent("billing.trading_day.changed", tenant, outlet, outlet.String()+":"+date, at,
		obj{"outlet_id": outlet, "business_date": date, "state": state, "changed_at": at.Format(time.RFC3339Nano)}))
}

func eventsOf(t testing.TB, g *Target, typ string) []events.Envelope {
	t.Helper()
	var out []events.Envelope
	for _, e := range g.Events(t) {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func dataOf(t testing.TB, e events.Envelope) obj {
	t.Helper()
	var d obj
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Lifecycle covers suspend, reinstate and close under a vendor approval grant (ADR-0014 A1.2, A1.3, A2.6):
// the grant checks in their order, single use in the action's own transaction, the open-trading-day guard
// fed by billing's events, and the events and audit that follow.
func Lifecycle(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.SetNow(seed.At)
	spec := newSpecChecker(t)
	approver := g.VendorHeadersAs(double.ExampleApprover, "admin.org.lifecycle.approve", "admin.org.read")
	// A grant for kind and organisation, minted as identity would at the instant the service judges it at.
	mint := func(kind string, orgID ids.ID, edit ...func(*double.GrantSpec)) string {
		gs := double.GrantSpec{Kind: kind, Org: orgID.String(), Now: g.Now()}
		for _, e := range edit {
			e(&gs)
		}
		return g.MintGrant(gs)
	}
	path := map[string]string{"org_suspend": "/suspension", "org_reinstate": "/reinstatement", "org_close": "/closure"}
	do := func(kind string, orgID ids.ID, grant string, body obj, h http.Header, status int, code string) itest.Response {
		t.Helper()
		hh := itest.Merge(h)
		if grant != "" {
			hh.Set("Hostry-Vendor-Approval", grant)
		}
		p := org(orgID) + path[kind]
		r := g.Call(t, "POST", p, hh, body)
		want(t, r, status, code)
		spec.check(t, "POST", p, r.Status, r.Body)
		return r
	}
	req := func() obj {
		return obj{"ticket": "SUP-2041", "reason": "Owner asked to close the outlet group.", "owner_export_offered": true}
	}

	t.Run("the grant is verified in order: required, kind, organisation, approver, signature, expiry, subject, then use", func(t *testing.T) {
		o, other := orgIn(t, g, "Grant Checks", "33", 1), orgIn(t, g, "Grant Others", "33", 2)
		do("org_suspend", o.Org, "", req(), approver, 403, "grant.required")
		do("org_suspend", o.Org, "not-a-jws", req(), approver, 403, "grant.invalid")
		do("org_suspend", o.Org, mint("org_close", o.Org), req(), approver, 403, "grant.invalid")                                                                  // a grant for another kind
		do("org_suspend", o.Org, mint("org_suspend", other.Org), req(), approver, 403, "grant.invalid")                                                            // another organisation
		do("org_suspend", o.Org, mint("org_suspend", o.Org, func(s *double.GrantSpec) { s.Approver = ids.New().String() }), req(), approver, 403, "grant.invalid") // approver is not the bearer
		do("org_suspend", o.Org, mint("org_suspend", o.Org, func(s *double.GrantSpec) { s.Requester = double.ExampleApprover }), req(), approver, 403, "grant.invalid")
		do("org_suspend", o.Org, mint("org_suspend", o.Org, func(s *double.GrantSpec) { s.Now = g.Now().Add(-time.Hour) }), req(), approver, 403, "grant.expired")
		do("org_suspend", o.Org, mint("org_suspend", o.Org, func(s *double.GrantSpec) { s.SubjectHash = strings.Repeat("a", 64) }), req(), approver, 403, "grant.subject_mismatch")
		other2, err := double.NewEnv()
		if err != nil {
			t.Fatal(err)
		}
		do("org_suspend", o.Org, other2.MintGrant(double.GrantSpec{Kind: "org_suspend", Org: o.Org.String(), Now: g.Now()}), req(), approver, 403, "grant.invalid") // another key
		do("org_suspend", o.Org, mint("org_suspend", o.Org), req(), g.VendorHeadersAs(double.ExampleApprover, "admin.org.read"), 403, "")                           // lacks the approve permission
		do("org_suspend", o.Org, mint("org_suspend", o.Org), req(), approver, 200, "")
		if d := g.Call(t, "GET", org(o.Org), approver, nil).JSON(t); d["organisation"].(obj)["status"] != "suspended" {
			t.Fatalf("%v", d)
		}
	})

	t.Run("a grant is single use, a replay of the request returns the same result, and a refused guard consumes nothing", func(t *testing.T) {
		o := orgIn(t, g, "Single Use", "33", 3)
		grant := mint("org_suspend", o.Org)
		key := http.Header{"Idempotency-Key": []string{ids.New().String()}}
		first := do("org_suspend", o.Org, grant, req(), itest.Merge(approver, key), 200, "")
		again := do("org_suspend", o.Org, grant, req(), itest.Merge(approver, key), 200, "") // the same request: nothing runs twice
		if string(first.Body) != string(again.Body) {
			t.Fatalf("a replay returns the same result:\n%s\n%s", first.Body, again.Body)
		}
		do("org_suspend", o.Org, grant, req(), approver, 403, "grant.used") // another request with a used grant
		do("org_reinstate", o.Org, mint("org_reinstate", o.Org), req(), approver, 200, "")

		// The open-trading-day guard: in progress means open, dated today or yesterday in IST (today is 2026-11-24).
		g2 := orgIn(t, g, "Busy Outlet", "33", 4)
		tradingDay(t, g, g2.Org, g2.Outlet, "2026-11-20", "open", 1) // a day left open for days protects nothing
		do("org_suspend", g2.Org, mint("org_suspend", g2.Org), req(), approver, 200, "")
		do("org_reinstate", g2.Org, mint("org_reinstate", g2.Org), req(), approver, 200, "")
		tradingDay(t, g, g2.Org, g2.Outlet, "2026-11-23", "open", 2) // yesterday still counts
		held := mint("org_suspend", g2.Org)
		busy := do("org_suspend", g2.Org, held, req(), approver, 409, "open_trading_day")
		if !strings.Contains(string(busy.Body), "1 outlet") {
			t.Fatalf("the refusal names the number of outlets: %s", busy.Body)
		}
		do("org_close", g2.Org, mint("org_close", g2.Org), req(), approver, 409, "open_trading_day") // the guard covers closing too
		tradingDay(t, g, g2.Org, g2.Outlet, "2026-11-23", "closed", 3)
		do("org_suspend", g2.Org, held, req(), approver, 200, "") // the approval stayed usable: the refused attempt did not consume the grant
		if r := do("org_suspend", g2.Org, mint("org_suspend", g2.Org), req(), approver, 409, "organisation_status_conflict"); r.Status != 409 {
			t.Fatal("already suspended")
		}
		// Events for another organisation's outlet say nothing about this one.
		tradingDay(t, g, g2.Org, ids.New(), "2026-11-24", "open", 4)
	})

	t.Run("reinstating is not guarded, closing needs the export confirmation and is terminal", func(t *testing.T) {
		o := orgIn(t, g, "Closing Down", "33", 5)
		do("org_reinstate", o.Org, mint("org_reinstate", o.Org), req(), approver, 409, "organisation_status_conflict") // nothing to reinstate
		do("org_suspend", o.Org, mint("org_suspend", o.Org), req(), approver, 200, "")
		tradingDay(t, g, o.Org, o.Outlet, "2026-11-24", "open", 1)
		do("org_reinstate", o.Org, mint("org_reinstate", o.Org), req(), approver, 200, "") // an open day does not stop reinstatement
		tradingDay(t, g, o.Org, o.Outlet, "2026-11-24", "closed", 2)
		noExport := req()
		delete(noExport, "owner_export_offered")
		do("org_close", o.Org, mint("org_close", o.Org), noExport, approver, 422, "validation")
		bad := req()
		bad["ticket"] = "!"
		do("org_close", o.Org, mint("org_close", o.Org), bad, approver, 422, "validation")
		short := req()
		short["reason"] = "short"
		do("org_close", o.Org, mint("org_close", o.Org), short, approver, 422, "validation")
		r := do("org_close", o.Org, mint("org_close", o.Org), req(), approver, 200, "")
		if m := r.JSON(t); m["status"] != "closed" || m["organisation_id"] != o.Org.String() {
			t.Fatalf("%s", r.Body)
		}
		do("org_reinstate", o.Org, mint("org_reinstate", o.Org), req(), approver, 409, "organisation_status_conflict") // closed is terminal
		do("org_suspend", o.Org, mint("org_suspend", o.Org), req(), approver, 409, "organisation_status_conflict")
		do("org_close", ids.New(), mint("org_close", ids.New()), req(), approver, 404, "")
		// Suspending and closing never touches the outlets or the series: the devices give the guarantee.
		if d := g.Call(t, "GET", org(o.Org), approver, nil).JSON(t); len(d["outlets"].([]any)) != 1 || d["organisation"].(obj)["status"] != "closed" {
			t.Fatalf("%v", d)
		}
	})

	t.Run("every action is audited with its target, and publishes the status change with the approval id", func(t *testing.T) {
		o := orgIn(t, g, "Audit Trail", "33", 6)
		before := len(eventsOf(t, g, "tenancy.organisation.status_changed"))
		n := len(g.AuditLog(t, o.Org))
		approvalID := ids.New().String()
		do("org_suspend", o.Org, mint("org_suspend", o.Org, func(s *double.GrantSpec) { s.ApprovalID = approvalID }), req(), approver, 200, "")
		evs := eventsOf(t, g, "tenancy.organisation.status_changed")
		if len(evs) != before+1 {
			t.Fatalf("one status event per action, got %d", len(evs)-before)
		}
		d := dataOf(t, evs[len(evs)-1])
		if d["status"] != "suspended" || d["previous_status"] != "trial" || d["approval_id"] != approvalID {
			t.Fatalf("%v", d)
		}
		if strings.Contains(string(evs[len(evs)-1].Data), "Owner asked") || strings.Contains(string(evs[len(evs)-1].Data), "SUP-2041") {
			t.Fatalf("no free text in the event: %s", evs[len(evs)-1].Data)
		}
		rows := g.AuditLog(t, o.Org)
		if len(rows) != n+1 || rows[len(rows)-1].Outcome != "ok" || rows[len(rows)-1].TargetID != o.Org.String() {
			t.Fatalf("a vendor audit row names the organisation: %v", rows)
		}
		// A refused action leaves no status event and no change.
		do("org_suspend", o.Org, mint("org_suspend", o.Org), req(), approver, 409, "organisation_status_conflict")
		if len(eventsOf(t, g, "tenancy.organisation.status_changed")) != before+1 {
			t.Fatal("a refused action publishes nothing")
		}
	})
}
