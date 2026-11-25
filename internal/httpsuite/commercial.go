package httpsuite

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// The commercial module (ADR-0008 S2, Admin A1): plans, Hostry's GST configuration, subscriptions, charges,
// payments recorded by hand, work lists, the CSV export and the owner's "My plan". The suite runs against the
// real service and the double and checks the responses it can against the OpenAPI document.

func num(v any) int64 { return int64(v.(float64)) }

func paise(m any) int64 { return num(m.(obj)["amount_paise"]) }

func inr(p int64) obj { return obj{"amount_paise": p, "currency": "INR"} }

func allVendor(g *Target) http.Header { return g.VendorHeadersAs(vendorUser, double.AllVendorPerms...) }

// orgIn creates a verified organisation whose outlet and registration are in state (so GST is CGST and SGST for
// Hostry's state 33 and IGST for any other).
func orgIn(t testing.TB, g *Target, name, state string, n int) created {
	t.Helper()
	body := obj{"name": name, "outlet": obj{"name": "Main", "kind": "restaurant", "state_code": state},
		"tax_registration": obj{"legal_name": name + " Pvt Ltd", "regime": "regular", "gstin": MakeGSTIN(state, n)},
		"verification":     obj{"method": "certificate_seen"}}
	r := g.Call(t, "POST", "/internal/v1/vendor/organisations", allVendor(g), body)
	want(t, r, 201, "")
	return parseCreated(t, r)
}

func org(id ids.ID) string { return "/internal/v1/vendor/organisations/" + id.String() }

var tableFeatures = []string{"restaurant.core", "restaurant.kds", "restaurant.captain", "restaurant.reports.core"}

func versionBody(listMonthly, listAnnual int64, outlets int) obj {
	prices := []obj{{"cycle": "monthly", "list_price": inr(listMonthly)}}
	if listAnnual > 0 {
		prices = append(prices, obj{"cycle": "annual", "list_price": inr(listAnnual)})
	}
	return obj{"feature_keys": tableFeatures, "included_outlets": outlets, "prices": prices}
}

// setupPlans creates and activates the plans the suites sell, and Hostry's GST configuration.
func setupPlans(t testing.TB, g *Target) {
	t.Helper()
	admin := allVendor(g)
	for _, p := range []struct {
		key    string
		prices []obj
	}{
		{"table_service", []obj{{"cycle": "monthly", "list_price": inr(129900)}, {"cycle": "annual", "list_price": inr(1299000)}}},
		{"pilot", []obj{{"cycle": "none", "list_price": inr(0)}}},
	} {
		// On Postgres the migration has seeded these plans as drafts (BR-A1-12): they exist, and activating
		// version 1 is what makes them sellable.
		r := g.Call(t, "POST", "/internal/v1/vendor/plans", admin, obj{"key": p.key, "name_key": "plan." + p.key,
			"version": obj{"feature_keys": tableFeatures, "included_outlets": 1, "prices": p.prices}})
		if r.Status != 201 && r.Code() != "plan_exists" {
			t.Fatalf("plan %s: %d %s", p.key, r.Status, r.Body)
		}
		want(t, g.Call(t, "POST", "/internal/v1/vendor/plans/"+p.key+"/versions/1/activation", admin, nil), 200, "")
	}
	want(t, g.Call(t, "POST", "/internal/v1/vendor/billing-configurations", admin, obj{"effective_from": "2026-12-01", "hostry_state_code": "33", "sac": "997331",
		"gst_rate_bp": 1800, "gst_registered": true, "payment_tolerance_paise": 99, "payment_terms_days": 7}), 201, "")
}

func startMonthly(t testing.TB, g *Target, o created, start string) obj {
	t.Helper()
	r := g.Call(t, "POST", org(o.Org)+"/subscription", allVendor(g), obj{"plan_key": "table_service", "plan_version": 1, "cycle": "monthly", "start_date": start})
	want(t, r, 201, "")
	return r.JSON(t)
}

func raise(t testing.TB, g *Target, o created, start, end string) itest.Response {
	return g.Call(t, "POST", org(o.Org)+"/charges", allVendor(g), obj{"kind": "period", "period_start": start, "period_end": end})
}

func at(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 10, 0, 0, 0, time.UTC) }

// Commercial covers plans, the GST configuration, subscriptions, charges, payments, lists and "My plan".
func Commercial(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.SetNow(seed.At)
	spec := newSpecChecker(t)
	admin := allVendor(g)
	onboarding := g.VendorHeadersAs(vendorUser, "admin.org.read", "admin.subscription.read", "admin.subscription.start")
	billing := g.VendorHeadersAs(vendorUser, "admin.org.read", "admin.subscription.read", "admin.subscription.start", "admin.subscription.manage", "admin.report.ledger")
	call := func(method, path string, h http.Header, body any, status int) itest.Response {
		t.Helper()
		r := g.Call(t, method, path, h, body)
		if r.Status != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.Status, status, r.Body)
		}
		spec.check(t, method, strings.SplitN(path, "?", 2)[0], r.Status, r.Body)
		return r
	}

	t.Run("plans and versions are data: create, version, activate, retire, and onboarding cannot change plans", func(t *testing.T) {
		body := obj{"key": "bakery_plan", "name_key": "plan.bakery_plan", "version": versionBody(49900, 0, 1)}
		call("POST", "/internal/v1/vendor/plans", onboarding, body, 403)
		r := call("POST", "/internal/v1/vendor/plans", admin, body, 201)
		if v := r.JSON(t)["versions"].([]any)[0].(obj); v["state"] != "draft" || num(v["version"]) != 1 {
			t.Fatalf("a new plan starts as draft version 1: %s", r.Body)
		}
		call("POST", "/internal/v1/vendor/plans", admin, obj{"key": "bakery_plan", "name_key": "plan.bakery_plan", "version": versionBody(1, 0, 1)}, 409)
		call("POST", "/internal/v1/vendor/plans", admin, obj{"key": "Bad Key", "name_key": "plan.x", "version": versionBody(1, 0, 1)}, 422)
		v2 := call("POST", "/internal/v1/vendor/plans/bakery_plan/versions", admin, versionBody(59900, 0, 2), 201).JSON(t)
		if num(v2["version"]) != 2 || v2["state"] != "draft" {
			t.Fatalf("%v", v2)
		}
		call("POST", "/internal/v1/vendor/plans/bakery_plan/versions/2/retirement", admin, nil, 409) // a draft is not retired
		act := call("POST", "/internal/v1/vendor/plans/bakery_plan/versions/2/activation", admin, nil, 200).JSON(t)
		if act["state"] != "active" || act["activated_at"] == nil {
			t.Fatalf("%v", act)
		}
		call("POST", "/internal/v1/vendor/plans/bakery_plan/versions/2/activation", admin, nil, 409) // immutable once active
		ret := call("POST", "/internal/v1/vendor/plans/bakery_plan/versions/2/retirement", admin, nil, 200).JSON(t)
		if ret["state"] != "retired" || ret["retired_at"] == nil {
			t.Fatalf("%v", ret)
		}
		call("POST", "/internal/v1/vendor/plans/nosuchplan/versions", admin, versionBody(1, 0, 1), 404)
		got := call("GET", "/internal/v1/vendor/plans/bakery_plan", onboarding, nil, 200).JSON(t)
		if vs := got["versions"].([]any); len(vs) != 2 || num(vs[0].(obj)["version"]) != 2 {
			t.Fatalf("versions are listed newest first: %v", got)
		}
		call("GET", "/internal/v1/vendor/plans", onboarding, nil, 200)
		call("GET", "/internal/v1/vendor/plans/nosuchplan", onboarding, nil, 404)
	})

	setupPlans(t, g)

	t.Run("the GST configuration is versioned and effective dated", func(t *testing.T) {
		body := obj{"effective_from": "2027-04-01", "hostry_state_code": "33", "sac": "997331", "gst_rate_bp": 1800, "gst_registered": true,
			"payment_tolerance_paise": 99, "payment_terms_days": 7}
		call("POST", "/internal/v1/vendor/billing-configurations", admin, body, 201)
		call("POST", "/internal/v1/vendor/billing-configurations", admin, body, 409) // one version per date
		bad := obj{"effective_from": "2027-05-01", "hostry_state_code": "99", "sac": "997331", "gst_rate_bp": 1800, "gst_registered": true,
			"payment_tolerance_paise": 99, "payment_terms_days": 7}
		call("POST", "/internal/v1/vendor/billing-configurations", admin, bad, 422)
		items := call("GET", "/internal/v1/vendor/billing-configurations", onboarding, nil, 200).JSON(t)["items"].([]any)
		if len(items) != 2 || items[0].(obj)["effective_from"] != "2027-04-01" {
			t.Fatalf("newest first: %v", items)
		}
	})

	t.Run("starting a plan writes the subscription, publishes it and moves the trial to active", func(t *testing.T) {
		o := orgIn(t, g, "Plan Starters", "33", 1)
		before := len(g.Events(t))
		body := obj{"plan_key": "table_service", "plan_version": 1, "cycle": "monthly", "start_date": "2026-12-01"}
		key := http.Header{"Idempotency-Key": []string{ids.New().String()}}
		r := call("POST", org(o.Org)+"/subscription", itest.Merge(onboarding, key), body, 201)
		sub := r.JSON(t)
		if sub["state"] != "active" || sub["paid_through"] != "2026-12-31" || paise(sub["agreed_price"]) != 129900 || sub["overdue"] != false {
			t.Fatalf("%s", r.Body)
		}
		again := call("POST", org(o.Org)+"/subscription", itest.Merge(onboarding, key), body, 201)
		if string(again.Body) != string(r.Body) {
			t.Fatalf("a replay returns the same row:\n%s\n%s", r.Body, again.Body)
		}
		call("POST", org(o.Org)+"/subscription", onboarding, body, 409) // one current subscription per organisation
		var gotSub, gotStatus bool
		for _, e := range g.Events(t)[before:] {
			switch e.Type {
			case "tenancy.subscription.changed":
				gotSub = true
				if strings.Contains(string(e.Data), "amount") || strings.Contains(string(e.Data), "price") {
					t.Fatalf("no amounts in the subscription event: %s", e.Data)
				}
			case "tenancy.organisation.updated":
				gotStatus = strings.Contains(string(e.Data), `"active"`)
			}
		}
		if !gotSub || !gotStatus {
			t.Fatalf("subscription.changed %v, organisation.updated %v", gotSub, gotStatus)
		}
		if d := call("GET", org(o.Org), onboarding, nil, 200).JSON(t); d["organisation"].(obj)["status"] != "active" {
			t.Fatalf("the first plan moves trial to active: %v", d)
		}
		// Pricing rules: below list needs adjust and a reason; above list or add-ons need manage.
		o2 := orgIn(t, g, "Price Rules", "33", 2)
		below := obj{"plan_key": "table_service", "plan_version": 1, "cycle": "monthly", "start_date": "2026-12-01", "agreed_price": inr(100000)}
		call("POST", org(o2.Org)+"/subscription", billing, below, 403)
		below["reason"] = "Founding customer discount agreed."
		call("POST", org(o2.Org)+"/subscription", billing, below, 403) // billing has manage but not adjust
		delete(below, "reason")
		call("POST", org(o2.Org)+"/subscription", admin, below, 422)
		below["reason"] = "Founding customer discount agreed."
		if r := call("POST", org(o2.Org)+"/subscription", admin, below, 201); paise(r.JSON(t)["agreed_price"]) != 100000 {
			t.Fatalf("%s", r.Body)
		}
		// A version that is not active cannot be sold.
		o3 := orgIn(t, g, "Draft Buyers", "33", 3)
		call("POST", "/internal/v1/vendor/plans/pilot/versions", admin, obj{"feature_keys": tableFeatures, "included_outlets": 1, "prices": []obj{{"cycle": "none", "list_price": inr(0)}}}, 201)
		call("POST", org(o3.Org)+"/subscription", admin, obj{"plan_key": "pilot", "plan_version": 2, "cycle": "none", "start_date": "2026-12-01"}, 409)
	})

	t.Run("a plan change is a new row from the effective date; cancelling is recorded once", func(t *testing.T) {
		o := orgIn(t, g, "Changers", "33", 4)
		startMonthly(t, g, o, "2026-12-01")
		call("PUT", org(o.Org)+"/subscription", onboarding, obj{"plan_key": "table_service", "plan_version": 1, "cycle": "annual", "effective_from": "2027-01-01"}, 403)
		ch := call("PUT", org(o.Org)+"/subscription", billing, obj{"plan_key": "table_service", "plan_version": 1, "cycle": "annual", "effective_from": "2027-01-01"}, 200).JSON(t)
		if ch["cycle"] != "annual" || ch["start_date"] != "2027-01-01" {
			t.Fatalf("%v", ch)
		}
		hist := call("GET", org(o.Org)+"/subscription", onboarding, nil, 200).JSON(t)
		if len(hist["history"].([]any)) != 1 || hist["current"].(obj)["cycle"] != "annual" {
			t.Fatalf("one earlier row stays as history: %v", hist)
		}
		call("PUT", org(o.Org)+"/subscription", billing, obj{"plan_key": "table_service", "plan_version": 1, "cycle": "annual", "effective_from": "2026-12-15"}, 422) // not after the current start
		c1 := call("POST", org(o.Org)+"/subscription/cancellation", billing, obj{"reason": "Owner asked to stop after the period."}, 200)
		if c1.JSON(t)["cancel_at_period_end"] != true {
			t.Fatalf("%s", c1.Body)
		}
		call("POST", org(o.Org)+"/subscription/cancellation", billing, obj{}, 200) // a repeat changes nothing
		call("POST", org(orgIn(t, g, "Nobody Subscribed", "33", 5).Org)+"/subscription/cancellation", billing, obj{}, 409)
	})

	t.Run("charges take GST by state, refuse a duplicate period and carry their own due dates", func(t *testing.T) {
		in := orgIn(t, g, "Home State", "33", 6)
		startMonthly(t, g, in, "2026-12-01")
		r := call("POST", org(in.Org)+"/charges", billing, obj{"kind": "period", "period_start": "2026-12-01", "period_end": "2026-12-31"}, 201)
		c := r.JSON(t)
		if paise(c["taxable"]) != 129900 || paise(c["cgst"]) != 11691 || paise(c["sgst"]) != 11691 || paise(c["igst"]) != 0 || paise(c["total"]) != 153282 ||
			c["due_date"] != "2026-12-08" || c["status"] != "open" || paise(c["outstanding"]) != 153282 {
			t.Fatalf("home-state charge: %s", r.Body)
		}
		call("POST", org(in.Org)+"/charges", billing, obj{"kind": "period", "period_start": "2026-12-01", "period_end": "2026-12-31"}, 409)
		call("POST", org(in.Org)+"/charges", billing, obj{"kind": "extra", "period_start": "2026-12-01", "period_end": "2026-12-31", "taxable": inr(50000)}, 422) // needs a reason
		x := call("POST", org(in.Org)+"/charges", billing, obj{"kind": "extra", "period_start": "2026-12-01", "period_end": "2026-12-31", "taxable": inr(50000),
			"reason": "Extra training day, agreed by phone."}, 201).JSON(t)
		if paise(x["cgst"]) != 4500 || paise(x["total"]) != 59000 || x["kind"] != "extra" {
			t.Fatalf("extra charge: %v", x)
		}
		next := call("POST", org(in.Org)+"/charges", billing, obj{"kind": "period", "period_start": "2027-01-01", "period_end": "2027-01-31"}, 201).JSON(t)
		if next["due_date"] != "2027-01-01" {
			t.Fatalf("a later charge is due on its first day: %v", next)
		}
		out := orgIn(t, g, "Other State", "29", 7)
		startMonthly(t, g, out, "2026-12-01")
		ig := call("POST", org(out.Org)+"/charges", billing, obj{"kind": "period", "period_start": "2026-12-01", "period_end": "2026-12-31"}, 201).JSON(t)
		if paise(ig["igst"]) != 23382 || paise(ig["cgst"]) != 0 || paise(ig["total"]) != 153282 {
			t.Fatalf("other-state charge takes IGST: %v", ig)
		}
		call("POST", org(in.Org)+"/charges", onboarding, obj{"kind": "period", "period_start": "2027-02-01", "period_end": "2027-02-28"}, 403)
		// A pilot has no charges; a cancelled subscription raises none after paid_through.
		p := orgIn(t, g, "Pilot Only", "33", 8)
		call("POST", org(p.Org)+"/subscription", admin, obj{"plan_key": "pilot", "plan_version": 1, "cycle": "none", "start_date": "2026-12-01"}, 201)
		call("POST", org(p.Org)+"/charges", billing, obj{"kind": "period", "period_start": "2026-12-01", "period_end": "2026-12-31"}, 409)
		call("POST", org(in.Org)+"/subscription/cancellation", billing, obj{}, 200)
		call("POST", org(in.Org)+"/charges", billing, obj{"kind": "period", "period_start": "2027-03-01", "period_end": "2027-03-31"}, 409)
	})

	t.Run("payments are recorded by hand, within the tolerance a charge is paid, and corrections are new rows", func(t *testing.T) {
		g.SetNow(at(2026, 12, 2))
		defer g.SetNow(seed.At)
		o := orgIn(t, g, "Payers", "33", 9)
		startMonthly(t, g, o, "2026-12-01")
		charge := raise(t, g, o, "2026-12-01", "2026-12-31").JSON(t)["id"].(string)
		base := org(o.Org) + "/charges/" + charge
		pay := func(amount int64, method, on string, status int) itest.Response {
			return call("POST", base+"/payments", billing, obj{"amount": inr(amount), "method": method, "reference": "UTR1234567890", "received_on": on}, status)
		}
		state := func() obj { return call("GET", base, billing, nil, 200).JSON(t) }
		p1 := pay(100000, "upi", "2026-12-02", 201).JSON(t)
		if s := state(); s["status"] != "open" || paise(s["outstanding"]) != 53282 {
			t.Fatalf("a partial payment leaves the rest outstanding: %v", s)
		}
		pay(53283, "upi", "2026-12-02", 422)        // above the outstanding amount: no unapplied credit
		pay(100, "upi", "2026-12-03", 422)          // not in the future
		pay(100, "gateway_link", "2026-12-02", 422) // reserved, not available in A1
		pay(0, "upi", "2026-12-02", 422)            // nothing
		p2 := pay(53200, "bank_transfer", "2026-12-02", 201).JSON(t)
		if s := state(); s["status"] != "paid" {
			t.Fatalf("within the tolerance of 99 paise the charge counts as paid: %v", s)
		}
		pay(1, "cash", "2026-12-02", 201) // a small remaining balance can still be recorded
		rev := call("POST", base+"/payments/"+p2["id"].(string)+"/reversal", admin, obj{"reason": "The cheque bounced at the bank."}, 201).JSON(t)
		if rev["reversal_of"] != p2["id"] || paise(rev["amount"]) != 53200 {
			t.Fatalf("a reversing row references the payment: %v", rev)
		}
		if s := state(); s["status"] != "open" {
			t.Fatalf("a reversal reopens the charge: %v", s)
		}
		call("POST", base+"/payments/"+p2["id"].(string)+"/reversal", admin, obj{"reason": "Reversing the same payment twice."}, 409)
		call("POST", base+"/payments/"+rev["id"].(string)+"/reversal", admin, obj{"reason": "Reversing a reversing row again."}, 409)
		call("POST", base+"/payments/"+p1["id"].(string)+"/reversal", billing, obj{"reason": "Billing cannot reverse payments."}, 403)
		rows := call("GET", base+"/payments", billing, nil, 200).JSON(t)["items"].([]any)
		if len(rows) != 4 {
			t.Fatalf("append-only: 3 payments and 1 reversal, got %d", len(rows))
		}

		// The invoice reference is set once and never edited.
		call("PUT", base+"/invoice-reference", billing, obj{"invoice_reference": obj{"number": "bad number!", "date": "2026-12-01"}}, 422)
		ref := call("PUT", base+"/invoice-reference", billing, obj{"invoice_reference": obj{"number": "HQ/2627/0042", "date": "2026-12-01"}}, 200).JSON(t)
		if ref["invoice_reference"].(obj)["number"] != "HQ/2627/0042" {
			t.Fatalf("%v", ref)
		}
		call("PUT", base+"/invoice-reference", billing, obj{"invoice_reference": obj{"number": "HQ/2627/0043", "date": "2026-12-01"}}, 409)

		// Waive and cancel are adjustments with a reason, for admins only; neither deletes anything.
		call("POST", base+"/waiver", billing, obj{"reason": "Goodwill after the outage."}, 403)
		call("POST", base+"/cancellation", admin, obj{"reason": "Raised in error, please cancel."}, 409) // it has payments
		w := call("POST", base+"/waiver", admin, obj{"reason": "Goodwill after the outage."}, 200).JSON(t)
		if w["status"] != "waived" || paise(w["outstanding"]) != 0 || w["reason"] == nil {
			t.Fatalf("%v", w)
		}
		call("POST", base+"/waiver", admin, obj{"reason": "Goodwill after the outage."}, 200) // repeat: nothing changes
		pay(1, "cash", "2026-12-02", 409)
		call("POST", base+"/cancellation", admin, obj{"reason": "Raised in error, please cancel."}, 409)

		// A charge raised in error with no payment is cancelled and keeps its row.
		x := call("POST", org(o.Org)+"/charges", billing, obj{"kind": "extra", "period_start": "2026-12-01", "period_end": "2026-12-31", "taxable": inr(1000),
			"reason": "Raised against the wrong customer."}, 201).JSON(t)["id"].(string)
		c := call("POST", org(o.Org)+"/charges/"+x+"/cancellation", admin, obj{"reason": "Raised against the wrong customer."}, 200).JSON(t)
		if c["status"] != "cancelled" || paise(c["outstanding"]) != 0 {
			t.Fatalf("%v", c)
		}
		if n := len(call("GET", org(o.Org)+"/charges?status=cancelled", billing, nil, 200).JSON(t)["items"].([]any)); n != 1 {
			t.Fatalf("the cancelled charge is listed: %d", n)
		}
		if n := len(call("GET", org(o.Org)+"/charges", billing, nil, 200).JSON(t)["items"].([]any)); n != 2 {
			t.Fatalf("both rows stay: %d", n)
		}
	})

	t.Run("overdue is a derived label, the work lists are derived on read and the export holds ids and names only", func(t *testing.T) {
		g.SetNow(at(2026, 12, 1))
		defer g.SetNow(seed.At)
		o := orgIn(t, g, "Late Payers", "33", 10)
		startMonthly(t, g, o, "2026-12-01")
		charge := raise(t, g, o, "2026-12-01", "2026-12-31").JSON(t)["id"].(string) // due 2026-12-08
		g.SetNow(at(2026, 12, 11))
		sub := func() obj { return call("GET", org(o.Org)+"/subscription", billing, nil, 200).JSON(t)["current"].(obj) }
		if sub()["overdue"] != true {
			t.Fatalf("an open charge three days past due makes the subscription overdue")
		}
		row := func(kind string, of ids.ID) obj {
			for _, it := range call("GET", "/internal/v1/vendor/ledger-lists/"+kind+"?limit=100", billing, nil, 200).JSON(t)["items"].([]any) {
				if it.(obj)["organisation_id"] == of.String() {
					return it.(obj)
				}
			}
			return nil
		}
		if r := row("overdue_charges", o.Org); r == nil || num(r["age_days"]) != 3 || r["subject_id"] != charge || paise(r["amount"]) != 153282 || r["organisation_name"] != "Late Payers" {
			t.Fatalf("overdue row: %v", r)
		}
		if row("charges_without_invoice_reference", o.Org) == nil {
			t.Fatal("a charge with no invoice reference is listed")
		}
		call("PUT", org(o.Org)+"/charges/"+charge+"/invoice-reference", billing, obj{"invoice_reference": obj{"number": "HQ/2627/0050", "date": "2026-12-01"}}, 200)
		if row("charges_without_invoice_reference", o.Org) != nil {
			t.Fatal("a charge with a reference leaves the list")
		}
		call("POST", org(o.Org)+"/charges/"+charge+"/payments", billing, obj{"amount": inr(153282), "method": "upi", "received_on": "2026-12-10"}, 201)
		if sub()["overdue"] != false || row("overdue_charges", o.Org) != nil {
			t.Fatal("recording the payment clears the label and the list at once")
		}
		// A subscription whose next period starts within a week and has no charge yet.
		nx := orgIn(t, g, "Next Raise", "33", 18)
		startMonthly(t, g, nx, "2026-12-14")
		if r := row("charges_to_raise", nx.Org); r == nil || r["due_date"] != "2026-12-14" {
			t.Fatalf("charges to raise: %v", r)
		}
		if row("charges_to_raise", o.Org) != nil {
			t.Fatal("January is more than a week away")
		}
		// A pilot whose review date is within 14 days.
		p := orgIn(t, g, "Pilot Ending", "33", 11)
		call("POST", org(p.Org)+"/subscription", admin, obj{"plan_key": "pilot", "plan_version": 1, "cycle": "none", "start_date": "2026-11-20", "review_date": "2026-12-20"}, 201)
		if r := row("pilots_ending", p.Org); r == nil || r["due_date"] != "2026-12-20" {
			t.Fatalf("pilots ending: %v", r)
		}
		// One outlet is included in the plan; a second one is a notice, not a refusal (A1-Q9).
		two := orgIn(t, g, "Two Outlets", "33", 12)
		startMonthly(t, g, two, "2026-12-01")
		if row("outlets_over_limit", two.Org) != nil {
			t.Fatal("one outlet is within the plan")
		}
		call("POST", "/v1/outlets", g.Owner(two.Org), obj{"id": ids.New().String(), "name": "Second", "kind": "restaurant", "state_code": "33", "tax_registration_id": two.Reg.String()}, 201)
		if row("outlets_over_limit", two.Org) == nil {
			t.Fatal("two outlets on a one-outlet plan are listed")
		}
		if sub := call("GET", org(two.Org)+"/subscription", billing, nil, 200).JSON(t)["current"].(obj); sub["outlets_over_limit"] != true {
			t.Fatalf("the soft limit notice: %v", sub)
		}
		call("GET", "/internal/v1/vendor/ledger-lists/nonsense", billing, nil, 404)
		call("GET", "/internal/v1/vendor/ledger-lists/overdue_charges", onboarding, nil, 200) // read permission suffices

		// The CSV carries ids and names, never a GSTIN, phone number or email (BR-A1-38).
		r := g.Call(t, "GET", "/internal/v1/vendor/ledger-exports", billing, nil)
		want(t, r, 200, "")
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
			t.Fatalf("content type %q", ct)
		}
		body := string(r.Body)
		if !strings.HasPrefix(body, "organisation_id,organisation_name,period_start,period_end,taxable_paise,total_paise,due_date,invoice_number,status,outstanding_paise\n") ||
			!strings.Contains(body, "Late Payers") || !strings.Contains(body, "HQ/2627/0050") || !strings.Contains(body, ",paid,") {
			t.Fatalf("export:\n%s", body)
		}
		if strings.Contains(body, MakeGSTIN("33", 10)) || strings.Contains(body, "@") || strings.Contains(body, "+91") {
			t.Fatalf("the export leaks a contact or tax id:\n%s", body)
		}
		g.Call(t, "GET", "/internal/v1/vendor/ledger-exports", onboarding, nil)
		want(t, g.Call(t, "GET", "/internal/v1/vendor/ledger-exports", onboarding, nil), 403, "")
		want(t, g.Call(t, "GET", "/internal/v1/vendor/ledger-exports?from=garbage", billing, nil), 422, "")
		// The export is audited as an operational read; the lists are directory reads and are not.
		if n := len(g.PlatformAuditLog(t)); n == 0 {
			t.Fatal("the ledger export is recorded in the platform audit log")
		}
	})

	t.Run("My plan shows an owner their own plan and nobody else's", func(t *testing.T) {
		g.SetNow(at(2026, 12, 2))
		defer g.SetNow(seed.At)
		a, b := orgIn(t, g, "Owner A Plans", "33", 13), orgIn(t, g, "Owner B Plans", "33", 14)
		startMonthly(t, g, a, "2026-12-01")
		call("POST", org(b.Org)+"/subscription", admin, obj{"plan_key": "pilot", "plan_version": 1, "cycle": "none", "start_date": "2026-12-01"}, 201)
		charge := raise(t, g, a, "2026-12-01", "2026-12-31").JSON(t)["id"].(string)
		call("PUT", org(a.Org)+"/charges/"+charge+"/invoice-reference", billing, obj{"invoice_reference": obj{"number": "HQ/2627/0099", "date": "2026-12-01"}}, 200)
		pay := call("POST", org(a.Org)+"/charges/"+charge+"/payments", billing, obj{"amount": inr(153282), "method": "upi", "received_on": "2026-12-02"}, 201).JSON(t)
		mine := call("GET", "/v1/subscription", g.Owner(a.Org), nil, 200)
		m := mine.JSON(t)
		if m["plan_key"] != "table_service" || m["plan_name_key"] != "plan.table_service" || len(m["charges"].([]any)) != 1 || len(m["payments"].([]any)) != 1 ||
			m["charges"].([]any)[0].(obj)["invoice_number"] != "HQ/2627/0099" || m["charges"].([]any)[0].(obj)["status"] != "paid" {
			t.Fatalf("%s", mine.Body)
		}
		if strings.Contains(string(mine.Body), "recorded_by") || strings.Contains(string(mine.Body), vendorUser) || strings.Contains(string(mine.Body), pay["id"].(string)) {
			t.Fatalf("no vendor user ids or internal ids for the owner: %s", mine.Body)
		}
		theirs := call("GET", "/v1/subscription", g.Owner(b.Org), nil, 200).JSON(t)
		if theirs["plan_key"] != "pilot" || len(theirs["charges"].([]any)) != 0 || len(theirs["payments"].([]any)) != 0 {
			t.Fatalf("B sees its own plan only: %v", theirs)
		}
		call("GET", "/v1/subscription", g.Owner(orgIn(t, g, "Owner C No Plan", "33", 15).Org), nil, 404)
		call("GET", "/v1/subscription", itest.Bearer(g.PinToken(a.Org)), nil, 200) // a read needs no strong session
	})

	t.Run("a vendor read of a charge is audited and a list is not; an unknown charge is 404", func(t *testing.T) {
		o := orgIn(t, g, "Audited Ledger", "33", 16)
		startMonthly(t, g, o, "2026-12-01")
		charge := raise(t, g, o, "2026-12-01", "2026-12-31").JSON(t)["id"].(string)
		n := len(g.AuditLog(t, o.Org))
		call("GET", org(o.Org)+"/charges", onboarding, nil, 200)
		call("GET", org(o.Org)+"/subscription", onboarding, nil, 200)
		if len(g.AuditLog(t, o.Org)) != n {
			t.Fatal("list reads write no audit row")
		}
		call("GET", org(o.Org)+"/charges/"+charge, onboarding, nil, 200)
		rows := g.AuditLog(t, o.Org)
		if len(rows) != n+1 || rows[len(rows)-1].TargetID != charge {
			t.Fatalf("a charge detail read is audited with its target: %v", rows)
		}
		call("GET", org(o.Org)+"/charges/"+ids.New().String(), onboarding, nil, 404)
		other := orgIn(t, g, "Other Ledger", "33", 17)
		call("GET", org(other.Org)+"/charges/"+charge, onboarding, nil, 404) // a charge belongs to one organisation
		call("POST", org(other.Org)+"/charges/"+charge+"/payments", billing, obj{"amount": inr(100), "method": "upi", "received_on": "2026-11-24"}, 404)
		call("GET", org(ids.New())+"/subscription", onboarding, nil, 404)
		call("GET", org(o.Org)+"/charges", g.VendorHeadersAs(vendorUser, "admin.org.read"), nil, 403)
	})
}
