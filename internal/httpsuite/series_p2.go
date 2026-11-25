package httpsuite

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// issue asks for series codes for a new device, as identity does at activation.
func issue(t testing.TB, g *Target, tenant, outlet ids.ID, types ...string) []obj {
	t.Helper()
	r := g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", tenant),
		obj{"outlet_id": outlet.String(), "device_id": ids.New().String(), "doc_types": types, "state": "active"})
	want(t, r, 201, "")
	var out []obj
	for _, it := range r.JSON(t)["items"].([]any) {
		out = append(out, it.(obj))
	}
	return out
}

func seriesByID(t testing.TB, g *Target, owner http.Header, id string) obj {
	t.Helper()
	for _, it := range g.Call(t, "GET", "/v1/document-series?limit=200", owner, nil).JSON(t)["items"].([]any) {
		if it.(obj)["id"] == id {
			return it.(obj)
		}
	}
	t.Fatalf("series %s is not in the register", id)
	return nil
}

// secondRegistration gives the organisation a second, verified registration in another state.
func secondRegistration(t testing.TB, g *Target, o created, n int) ids.ID {
	t.Helper()
	owner := g.Owner(o.Org)
	reg := ids.New()
	want(t, g.Call(t, "POST", "/v1/tax-registrations", owner, obj{"id": reg.String(), "legal_name": "Second Registration Pvt Ltd", "regime": "regular",
		"gstin": MakeGSTIN("29", n), "state_code": "29"}), 201, "")
	want(t, g.Call(t, "PUT", org(o.Org)+"/tax-registrations/"+reg.String()+"/verification", allVendor(g), obj{"method": "certificate_seen"}), 200, "")
	return reg
}

// SeriesTail covers the registry's Phase 2 behaviour: tenancy.series.issued and .closed, moving an outlet to
// another registration, the tail fields filled from billing.series.reconciled, and closing unreconciled
// (ADR-0014 A2.5 items 1 and 2).
func SeriesTail(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.SetNow(seed.At)
	spec := newSpecChecker(t)
	o := orgIn(t, g, "Moving Outlets", "33", 1)
	owner := g.Owner(o.Org)
	call := func(method, path string, h http.Header, body any, status int, code string) itest.Response {
		t.Helper()
		r := g.Call(t, method, path, h, body)
		want(t, r, status, code)
		spec.check(t, method, strings.SplitN(path, "?", 2)[0], r.Status, r.Body)
		return r
	}

	issued := issue(t, g, o.Org, o.Outlet, "tax_invoice", "credit_note")
	t.Run("issuing a series publishes it once, ids and codes only", func(t *testing.T) {
		evs := eventsOf(t, g, "tenancy.series.issued")
		if len(evs) != 2 {
			t.Fatalf("one event per series, got %d", len(evs))
		}
		d := dataOf(t, evs[0])
		if d["series_id"] != issued[0]["id"] || d["code"] != issued[0]["code"] || d["outlet_id"] != o.Outlet.String() || d["tax_registration_id"] != o.Reg.String() || d["device_id"] == nil {
			t.Fatalf("%v", d)
		}
		if _, ok := d["gstin"]; ok || strings.Contains(string(evs[0].Data), "33ABCDE") {
			t.Fatalf("no GSTIN in the event: %s", evs[0].Data)
		}
		key := http.Header{"Idempotency-Key": []string{ids.New().String()}}
		body := obj{"outlet_id": o.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"plain_bill", "credit_note"}, "state": "active"}
		first := g.Call(t, "POST", "/internal/v1/document-series-issuances", itest.Merge(g.ServiceHeaders("identity", o.Org), key), body)
		want(t, first, 201, "")
		again := g.Call(t, "POST", "/internal/v1/document-series-issuances", itest.Merge(g.ServiceHeaders("identity", o.Org), key), body)
		if string(again.Body) != string(first.Body) || len(eventsOf(t, g, "tenancy.series.issued")) != 4 {
			t.Fatalf("a replay returns the same codes and publishes nothing")
		}
	})

	spare := secondRegistration(t, g, o, 2)
	unverified := ids.New()
	want(t, g.Call(t, "POST", "/v1/tax-registrations", owner, obj{"id": unverified.String(), "legal_name": "Unverified Pvt Ltd", "regime": "regular",
		"gstin": MakeGSTIN("29", 3), "state_code": "29"}), 201, "")

	t.Run("moving an outlet closes its series under the old registration and publishes both events", func(t *testing.T) {
		path := "/v1/outlets/" + o.Outlet.String() + "/tax-registration"
		move := func(reg ids.ID) obj {
			return obj{"tax_registration_id": reg.String(), "reason": "Moved the outlet to the new GSTIN."}
		}
		call("PUT", path, owner, move(unverified), 409, "registration_not_eligible") // not verified
		call("PUT", path, owner, move(ids.New()), 404, "")                           // not one of ours
		call("PUT", path, owner, obj{"tax_registration_id": spare.String(), "reason": "short"}, 422, "")
		call("PUT", path, g.Owner(seed.Other.Org), move(spare), 404, "") // another organisation's outlet is not found
		tradingDay(t, g, o.Org, o.Outlet, "2026-11-24", "open", 1)
		call("PUT", path, owner, move(spare), 409, "open_trading_day") // one business day has one registration
		tradingDay(t, g, o.Org, o.Outlet, "2026-11-24", "closed", 2)
		before := len(g.Events(t))
		r := call("PUT", path, owner, move(spare), 200, "")
		if r.JSON(t)["tax_registration_id"] != spare.String() {
			t.Fatalf("%s", r.Body)
		}
		var updated, closed int
		for _, e := range g.Events(t)[before:] {
			d := dataOf(t, e)
			switch e.Type {
			case "tenancy.outlet.updated":
				updated++
				if d["tax_registration_id"] != spare.String() || d["tax_regime"] != "regular" || d["series_ids"] != nil {
					t.Fatalf("the outlet event names the new registration and no open series: %v", d)
				}
			case "tenancy.series.closed":
				closed++
				if d["cause"] != "outlet_moved" || d["tax_registration_id"] != o.Reg.String() {
					t.Fatalf("%v", d)
				}
			}
		}
		if updated != 1 || closed != 4 {
			t.Fatalf("one outlet.updated and one series.closed per series: %d %d", updated, closed)
		}
		s := seriesByID(t, g, owner, issued[0]["id"].(string))
		if s["status"] != "closed" || s["closed_at"] == nil || s["tail_confirmed_at"] != nil || s["last_number"] != nil {
			t.Fatalf("closed, awaiting device: %v", s)
		}
		published := len(g.Events(t))
		again := call("PUT", path, owner, move(spare), 200, "") // the same target again returns the outlet
		if string(again.Body) != string(r.Body) || len(g.Events(t)) != published {
			t.Fatalf("a repeat changes nothing")
		}
		// New codes come under the new registration at the next issuance.
		fresh := issue(t, g, o.Org, o.Outlet, "tax_invoice", "credit_note")
		if fresh[0]["tax_registration_id"] != spare.String() {
			t.Fatalf("%v", fresh[0])
		}
	})

	t.Run("billing.series.reconciled fills the tail once; after that only the device's statement counts", func(t *testing.T) {
		id := issued[0]["id"].(string)
		at := seed.At.Add(time.Hour)
		ev := billingEvent("billing.series.reconciled", o.Org, o.Outlet, id, at, obj{"series_id": id, "last_number": 123, "gap_count": 0})
		g.Deliver(t, ev)
		g.Deliver(t, ev) // a redelivered event changes nothing
		g.Deliver(t, billingEvent("billing.series.reconciled", o.Org, o.Outlet, id, at.Add(time.Hour), obj{"series_id": id, "last_number": 999, "gap_count": 0}))
		s := seriesByID(t, g, owner, id)
		if num(s["last_number"]) != 123 || s["tail_confirmed_at"] == nil {
			t.Fatalf("the first statement wins: %v", s)
		}
		call("PUT", "/v1/document-series/"+id+"/closure", owner, obj{"reason": "Device stolen and never reconnected."}, 409, "tail_confirmed")
		// A series that was never confirmed is closed unreconciled by a person, with a reason, and says so in an event.
		other := issued[1]["id"].(string)
		before := len(eventsOf(t, g, "tenancy.series.closed"))
		c := call("PUT", "/v1/document-series/"+other+"/closure", owner, obj{"reason": "Device stolen and never reconnected."}, 200, "").JSON(t)
		if c["status"] != "closed_unreconciled" {
			t.Fatalf("%v", c)
		}
		evs := eventsOf(t, g, "tenancy.series.closed")
		if len(evs) != before+1 || dataOf(t, evs[len(evs)-1])["cause"] != "closed_unreconciled" {
			t.Fatalf("closing unreconciled is published with its cause")
		}
		call("PUT", "/v1/document-series/"+other+"/closure", owner, obj{"reason": "Device stolen and never reconnected."}, 200, "") // idempotent
		if len(eventsOf(t, g, "tenancy.series.closed")) != before+1 {
			t.Fatal("a repeat publishes nothing")
		}
		// Events about series this organisation does not have, or about another organisation's, change nothing.
		g.Deliver(t, billingEvent("billing.series.reconciled", o.Org, o.Outlet, ids.New().String(), at, obj{"series_id": ids.New().String(), "last_number": 5, "gap_count": 0}))
		foreign := seed.Other
		g.Seed(t, foreign)
		g.Deliver(t, billingEvent("billing.series.reconciled", foreign.Org, foreign.Outlet, other, at, obj{"series_id": other, "last_number": 77, "gap_count": 0}))
		if s := seriesByID(t, g, owner, other); s["tail_confirmed_at"] != nil {
			t.Fatalf("another organisation's event cannot fill this organisation's series: %v", s)
		}
	})

	t.Run("identity.device.drained fills the tail of a closed series from the device's statement, once", func(t *testing.T) {
		id := issued[1]["id"].(string)
		at := seed.At.Add(3 * time.Hour)
		g.Deliver(t, drainedEvent(o.Org, o.Outlet, id, 42, at))
		g.Deliver(t, drainedEvent(o.Org, o.Outlet, id, 99, at.Add(time.Hour))) // a later statement does not overwrite the first
		s := seriesByID(t, g, g.Owner(o.Org), id)
		if num(s["last_number"]) != 42 || s["tail_confirmed_at"] == nil {
			t.Fatalf("the drain record fills the tail once: %v", s)
		}
	})

	t.Run("closing a registration publishes series.closed for the series it closes", func(t *testing.T) {
		o2 := orgIn(t, g, "Closing Registration", "33", 4)
		series := issue(t, g, o2.Org, o2.Outlet, "tax_invoice", "credit_note")
		spare2 := secondRegistration(t, g, o2, 5)
		owner2 := g.Owner(o2.Org)
		path := "/v1/tax-registrations/" + o2.Reg.String() + "/closure"
		call("PUT", path, owner2, obj{"reason": "Moved the Bengaluru outlets to a new GSTIN."}, 409, "registration_in_use")
		call("PUT", "/v1/outlets/"+o2.Outlet.String()+"/tax-registration", owner2, obj{"tax_registration_id": spare2.String(), "reason": "Moved the outlet to the new GSTIN."}, 200, "")
		before := len(eventsOf(t, g, "tenancy.series.closed"))
		call("PUT", path, owner2, obj{"reason": "Moved the Bengaluru outlets to a new GSTIN."}, 200, "")
		if len(eventsOf(t, g, "tenancy.series.closed")) != before {
			t.Fatalf("the series were already closed by the move: no second event")
		}
		_ = series
		// A registration whose outlet is inactive closes its open series and says why.
		o3 := orgIn(t, g, "Inactive Outlet", "33", 7)
		issue(t, g, o3.Org, o3.Outlet, "tax_invoice", "credit_note")
		owner3 := g.Owner(o3.Org)
		call("PATCH", "/v1/outlets/"+o3.Outlet.String(), owner3, obj{"status": "inactive"}, 200, "")
		before = len(eventsOf(t, g, "tenancy.series.closed"))
		call("PUT", "/v1/tax-registrations/"+o3.Reg.String()+"/closure", owner3, obj{"reason": "The outlet closed for good."}, 200, "")
		evs := eventsOf(t, g, "tenancy.series.closed")
		if len(evs) != before+2 || dataOf(t, evs[len(evs)-1])["cause"] != "registration_closed" {
			t.Fatalf("one event per series, with the cause: %d", len(evs)-before)
		}
	})
}

// Books covers paper bill books registered as entry-only series (ADR-0014 A1.6 item 4; provisional, P2B-6).
func Books(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.SetNow(seed.At)
	spec := newSpecChecker(t)
	o := orgIn(t, g, "Paper Books", "33", 1)
	other := orgIn(t, g, "Other Books", "33", 2)
	owner := g.Owner(o.Org)
	call := func(method, path string, h http.Header, body any, status int, code string) itest.Response {
		t.Helper()
		r := g.Call(t, method, path, h, body)
		want(t, r, status, code)
		spec.check(t, method, strings.SplitN(path, "?", 2)[0], r.Status, r.Body)
		return r
	}
	book := func(code string) obj {
		return obj{"tax_registration_id": o.Reg.String(), "code": code, "outlet_ids": []string{o.Outlet.String()}}
	}

	t.Run("a book registers once, under a unique code, and the owner lists it", func(t *testing.T) {
		key := http.Header{"Idempotency-Key": []string{ids.New().String()}}
		r := call("POST", "/v1/manual-book-series", itest.Merge(owner, key), book("BKB"), 201, "")
		b := r.JSON(t)
		if b["code"] != "BKB" || b["status"] != "active" || b["tax_registration_id"] != o.Reg.String() {
			t.Fatalf("%s", r.Body)
		}
		again := call("POST", "/v1/manual-book-series", itest.Merge(owner, key), book("BKB"), 201, "")
		if string(again.Body) != string(r.Body) {
			t.Fatalf("a replay returns the same book")
		}
		call("POST", "/v1/manual-book-series", owner, book("BKB"), 409, "series_code_in_use")
		call("POST", "/v1/manual-book-series", owner, book("bkb"), 422, "")
		call("POST", "/v1/manual-book-series", owner, book("TOOLONG1"), 422, "")
		call("POST", "/v1/manual-book-series", owner, obj{"tax_registration_id": o.Reg.String(), "code": "BK2", "outlet_ids": []string{}}, 422, "")
		items := call("GET", "/v1/manual-book-series", owner, nil, 200, "").JSON(t)["items"].([]any)
		if len(items) != 1 || items[0].(obj)["id"] != b["id"] {
			t.Fatalf("%v", items)
		}
	})

	t.Run("a book's code never meets a device series code under the same GSTIN", func(t *testing.T) {
		series := issue(t, g, o.Org, o.Outlet, "tax_invoice", "credit_note")
		taken := series[0]["code"].(string)
		call("POST", "/v1/manual-book-series", owner, book(taken), 409, "series_code_in_use")
		// The allocator skips a code a book owns: reserve the next one, then issue.
		next := model.CodeFor(model.IndexOf(taken) + 2)
		call("POST", "/v1/manual-book-series", owner, book(next), 201, "")
		for _, s := range issue(t, g, o.Org, o.Outlet, "tax_invoice", "credit_note") {
			if s["code"] == next || s["code"] == "BKB" {
				t.Fatalf("the allocator issued a code a book owns: %v", s)
			}
		}
	})

	t.Run("only an owner in a strong session registers; another organisation's registration and outlets are not found", func(t *testing.T) {
		call("POST", "/v1/manual-book-series", itest.Bearer(g.PinToken(o.Org)), book("BK3"), 403, "strong_session_required")
		call("POST", "/v1/manual-book-series", g.Owner(other.Org), book("BK4"), 404, "") // A's registration, B's token
		cross := obj{"tax_registration_id": other.Reg.String(), "code": "BK5", "outlet_ids": []string{o.Outlet.String()}}
		call("POST", "/v1/manual-book-series", g.Owner(other.Org), cross, 404, "") // B's registration, A's outlet
		call("POST", "/v1/manual-book-series", owner, obj{"tax_registration_id": o.Reg.String(), "code": "BK6", "outlet_ids": []string{other.Outlet.String()}}, 404, "")
		items := call("GET", "/v1/manual-book-series", g.Owner(other.Org), nil, 200, "").JSON(t)["items"].([]any)
		if len(items) != 0 {
			t.Fatalf("B sees none of A's books: %v", items)
		}
	})
}

// LinksP2 covers the Phase 2 additions to the link control plane: a replayed proposal withholds the one-time
// code, reissuing an invitation, and the acceptor refreshing its snapshot after counterparty_changed
// (ADR-0003 Note N1 R1 and R3).
func LinksP2(t *testing.T, mk Factory) {
	g := mk(t, true)
	g.Seed(t, seed.Example, seed.Other, seed.Third)
	spec := newSpecChecker(t)
	a, b, c := seed.Example, seed.Other, seed.Third
	ownerA, ownerB, ownerC := g.Owner(a.Org), g.Owner(b.Org), g.Owner(c.Org)
	call := func(method, path string, h http.Header, body any, status int, code string) itest.Response {
		t.Helper()
		r := g.Call(t, method, path, h, body)
		want(t, r, status, code)
		spec.check(t, method, path, r.Status, r.Body)
		return r
	}
	proposal := func() obj {
		return obj{"id": ids.New().String(), "template": "on_premises_outlet", "outlet_ids": []string{a.Outlet.String()},
			"grants": []obj{{"scope": "test.echo.read", "direction": "outbound", "outlet_ids": []string{a.Outlet.String()}}}}
	}

	t.Run("a replayed proposal returns the link without the code and says so", func(t *testing.T) {
		key := http.Header{"Idempotency-Key": []string{ids.New().String()}}
		body := proposal()
		first := call("POST", "/v1/links", itest.Merge(ownerA, key), body, 201, "").JSON(t)
		if first["invitation_code"] == nil || first["secret_withheld"] != nil {
			t.Fatalf("%v", first)
		}
		again := call("POST", "/v1/links", itest.Merge(ownerA, key), body, 201, "").JSON(t)
		if again["invitation_code"] != nil || again["secret_withheld"] != true || again["link"].(obj)["id"] != first["link"].(obj)["id"] {
			t.Fatalf("a replay withholds the code: %v", again)
		}
	})

	t.Run("reissuing an invitation replaces the code, only for the proposer, only while proposed", func(t *testing.T) {
		id, old := proposeLink(t, g, ownerA)
		key := http.Header{"Idempotency-Key": []string{ids.New().String()}}
		r := call("POST", "/v1/links/"+id+"/invitations", itest.Merge(ownerA, key), nil, 200, "").JSON(t)
		fresh, _ := r["invitation_code"].(string)
		if fresh == "" || fresh == old {
			t.Fatalf("a fresh code is shown once: %v", r)
		}
		again := call("POST", "/v1/links/"+id+"/invitations", itest.Merge(ownerA, key), nil, 200, "").JSON(t)
		if again["invitation_code"] != nil || again["secret_withheld"] != true {
			t.Fatalf("a replay never shows the code again: %v", again)
		}
		call("POST", "/v1/link-acceptances", ownerB, obj{"invitation_code": old}, 409, "invitation_code_invalid") // the previous code stops working at once
		call("POST", "/v1/links/"+id+"/invitations", ownerB, nil, 404, "")                                        // not a party
		call("POST", "/v1/links/"+id+"/invitations", ownerC, nil, 404, "")
		call("POST", "/v1/link-acceptances", ownerB, obj{"invitation_code": fresh, "outlet_ids": []string{b.Outlet.String()}}, 201, "")
		call("POST", "/v1/links/"+id+"/invitations", ownerA, nil, 409, "link_not_proposed")
		call("POST", "/v1/links/"+id+"/invitations", ownerB, nil, 404, "") // the acceptor is not the proposer
	})

	t.Run("the acceptor refreshes its snapshot after counterparty_changed and the proposer can then confirm", func(t *testing.T) {
		id, code := proposeLink(t, g, ownerA)
		call("PUT", "/v1/links/"+id+"/re-acceptance", ownerB, nil, 404, "") // not yet a party
		call("POST", "/v1/link-acceptances", ownerB, obj{"invitation_code": code}, 201, "")
		call("PUT", "/v1/links/"+id+"/re-acceptance", ownerA, nil, 404, "") // only the acceptor
		call("PUT", "/v1/links/"+id+"/re-acceptance", ownerC, nil, 404, "")
		// The acceptor declared all its registrations; adding one changes what the proposer was shown.
		reg := ids.New()
		want(t, g.Call(t, "POST", "/v1/tax-registrations", ownerB, obj{"id": reg.String(), "legal_name": "Added After Accepting Pvt Ltd", "regime": "regular",
			"gstin": MakeGSTIN("29", 77), "state_code": "29"}), 201, "")
		confirm := obj{"counterparty_organisation_id": b.Org.String()}
		call("PUT", "/v1/links/"+id+"/confirmation", ownerA, confirm, 409, "counterparty_changed")
		before := len(eventsOf(t, g, "tenancy.link.accepted"))
		r := call("PUT", "/v1/links/"+id+"/re-acceptance", ownerB, nil, 200, "").JSON(t)
		if r["status"] != "accepted" || r["role"] != "acceptor" {
			t.Fatalf("the link stays accepted: %v", r)
		}
		if len(eventsOf(t, g, "tenancy.link.accepted")) != before+1 {
			t.Fatal("tenancy.link.accepted is published again")
		}
		if seen := call("GET", "/v1/links/"+id, ownerA, nil, 200, "").JSON(t); len(seen["counterparty"].(obj)["registrations"].([]any)) != 2 {
			t.Fatalf("the proposer now reviews the new snapshot: %v", seen)
		}
		call("PUT", "/v1/links/"+id+"/confirmation", ownerA, confirm, 200, "")
		call("PUT", "/v1/links/"+id+"/re-acceptance", ownerB, nil, 409, "link_not_accepted") // confirmed: nothing to refresh
	})
}

// PhaseTwoIsolation proves that one organisation can never reach another's Phase 2 data through any new
// tenant-realm operation, and that another organisation's events change nothing (ADR-0003 T2; the database
// level is proved for every new table by the Postgres isolation test).
func PhaseTwoIsolation(t *testing.T, mk Factory) {
	g := mk(t, true)
	g.SetNow(seed.At)
	g.Seed(t, seed.Example, seed.Other)
	a, b := seed.Example, seed.Other
	ownerA, ownerB := g.Owner(a.Org), g.Owner(b.Org)
	setupPlans(t, g)
	startMonthly(t, g, created{Org: a.Org}, "2026-12-01")
	charge := raise(t, g, created{Org: a.Org}, "2026-12-01", "2026-12-31").JSON(t)["id"].(string)
	want(t, g.Call(t, "POST", "/v1/manual-book-series", ownerA, obj{"tax_registration_id": a.Registration.String(), "code": "BKA", "outlet_ids": []string{a.Outlet.String()}}), 201, "")

	t.Run("My plan and the bill books show an organisation its own rows only", func(t *testing.T) {
		want(t, g.Call(t, "GET", "/v1/subscription", ownerB, nil), 404, "") // B has no plan; A's is not B's
		if r := g.Call(t, "GET", "/v1/subscription", ownerA, nil); r.Status != 200 || strings.Contains(string(r.Body), b.Org.String()) {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		if items := g.Call(t, "GET", "/v1/manual-book-series", ownerB, nil).JSON(t)["items"].([]any); len(items) != 0 {
			t.Fatalf("B sees A's book: %v", items)
		}
	})

	t.Run("another organisation's outlet, registration and series are 404", func(t *testing.T) {
		reason := obj{"reason": "Moved the outlet to the new GSTIN."}
		want(t, g.Call(t, "PUT", "/v1/outlets/"+a.Outlet.String()+"/tax-registration", ownerB, obj{"tax_registration_id": b.Registration.String(), "reason": reason["reason"]}), 404, "")
		want(t, g.Call(t, "PUT", "/v1/outlets/"+b.Outlet.String()+"/tax-registration", ownerB, obj{"tax_registration_id": a.Registration.String(), "reason": reason["reason"]}), 404, "")
		want(t, g.Call(t, "POST", "/v1/manual-book-series", ownerB, obj{"tax_registration_id": a.Registration.String(), "code": "BKB", "outlet_ids": []string{b.Outlet.String()}}), 404, "")
		want(t, g.Call(t, "POST", "/v1/manual-book-series", ownerB, obj{"tax_registration_id": b.Registration.String(), "code": "BKB", "outlet_ids": []string{a.Outlet.String()}}), 404, "")
		want(t, g.Call(t, "PUT", "/v1/document-series/"+a.Series.String()+"/closure", ownerB, obj{"reason": "Device stolen and never reconnected."}), 404, "")
	})

	t.Run("a vendor reaches a charge only through the organisation it belongs to", func(t *testing.T) {
		want(t, g.Call(t, "GET", org(b.Org)+"/charges/"+charge, allVendor(g), nil), 404, "")
		want(t, g.Call(t, "GET", org(a.Org)+"/charges/"+charge, allVendor(g), nil), 200, "")
	})

	t.Run("billing events change only the organisation and outlet they name", func(t *testing.T) {
		// A's outlet is open today; B's lifecycle is not held up by it, and an event naming A's outlet under B is ignored.
		tradingDay(t, g, a.Org, a.Outlet, "2026-11-24", "open", 1)
		tradingDay(t, g, b.Org, a.Outlet, "2026-11-24", "open", 2)
		approver := g.VendorHeadersAs(double.ExampleApprover, "admin.org.lifecycle.approve")
		grant := g.MintGrant(double.GrantSpec{Kind: "org_suspend", Org: b.Org.String(), Now: g.Now()})
		hh := itest.Merge(approver)
		hh.Set("Hostry-Vendor-Approval", grant)
		want(t, g.Call(t, "POST", org(b.Org)+"/suspension", hh, obj{"ticket": "SUP-2041", "reason": "Owner asked to pause the outlet group.", "owner_export_offered": false}), 200, "")
		hh2 := itest.Merge(approver)
		hh2.Set("Hostry-Vendor-Approval", g.MintGrant(double.GrantSpec{Kind: "org_suspend", Org: a.Org.String(), Now: g.Now()}))
		want(t, g.Call(t, "POST", org(a.Org)+"/suspension", hh2, obj{"ticket": "SUP-2042", "reason": "Owner asked to pause the outlet group.", "owner_export_offered": false}), 409, "open_trading_day")
	})
}
