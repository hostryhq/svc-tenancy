package httpsuite

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/gstin"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// MakeGSTIN builds a valid GSTIN for a state and a serial number.
func MakeGSTIN(state string, n int) string {
	first14 := fmt.Sprintf("%sABCDE%04dF1Z", state, n)
	return first14 + string(gstin.CheckChar(first14))
}

func orgBody(name, g string) obj {
	reg := obj{"legal_name": name + " Pvt Ltd", "regime": "regular", "gstin": g}
	if g == "" {
		reg = obj{"legal_name": name + " Pvt Ltd", "regime": "unregistered"}
	}
	return obj{"name": name, "outlet": obj{"name": "Main", "kind": "restaurant", "state_code": "33"}, "tax_registration": reg}
}

type created struct{ Org, Outlet, Reg ids.ID }

func parseCreated(t testing.TB, r itest.Response) created {
	t.Helper()
	m := r.JSON(t)
	return created{
		Org:    ids.ID(m["organisation"].(obj)["id"].(string)),
		Outlet: ids.ID(m["outlet"].(obj)["id"].(string)),
		Reg:    ids.ID(m["tax_registration"].(obj)["id"].(string)),
	}
}

func directory(t testing.TB, g *Target, query string) []obj {
	t.Helper()
	r := g.Call(t, "GET", "/internal/v1/vendor/organisations"+query, g.Vendor(), nil)
	want(t, r, 200, "")
	var out []obj
	for _, i := range r.JSON(t)["items"].([]any) {
		out = append(out, i.(obj))
	}
	return out
}

// Vendor covers organisation onboarding, GSTIN verification and the GSTIN rules (T15; AC-TEN-1 to 6, 19 to 22).
func Vendor(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.Seed(t, seed.Example, seed.Other)
	create := func(h http.Header, body any) itest.Response {
		return g.Call(t, "POST", "/internal/v1/vendor/organisations", h, body)
	}

	t.Run("creation makes the tenant, default brand, first outlet, registration and nothing else", func(t *testing.T) {
		r := create(g.Vendor(), orgBody("Fresh Foods", MakeGSTIN("33", 1)))
		want(t, r, 201, "")
		c := parseCreated(t, r)
		m := r.JSON(t)
		if m["organisation"].(obj)["status"] != "trial" || m["brand"].(obj)["is_default"] != true || m["tax_registration"].(obj)["verification"].(obj)["state"] != "unverified" {
			t.Fatalf("%s", r.Body)
		}
		owner := g.Owner(c.Org)
		if got := itemIDs(t, g.Call(t, "GET", "/v1/document-series", owner, nil)); len(got) != 0 {
			t.Fatalf("no document series exist at creation: %v", got)
		}
		s := g.Call(t, "GET", "/v1/organisation-settings", owner, nil).JSON(t)
		if s["offline_window_days"] != float64(7) || s["idle_lock_minutes"] != float64(5) {
			t.Fatalf("default settings: %v", s)
		}
		// No series without verification (TST-16, AC-TEN-6).
		issue := obj{"outlet_id": c.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"}
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", c.Org), issue), 409, "registration_unverified")
	})

	t.Run("the same key and body return the same organisation and create nothing more; another body is refused", func(t *testing.T) {
		h := g.Vendor()
		h.Set("Idempotency-Key", "onboard-1")
		body := orgBody("Replay Foods", MakeGSTIN("33", 2))
		first := create(h, body)
		want(t, first, 201, "")
		second := create(h, body)
		want(t, second, 201, "")
		if string(first.Body) != string(second.Body) {
			t.Fatalf("replay differs:\n%s\n%s", first.Body, second.Body)
		}
		if n := len(directory(t, g, "?q=Replay+Foods")); n != 1 {
			t.Fatalf("%d organisations named Replay Foods", n)
		}
		want(t, create(h, orgBody("Replay Foods Two", MakeGSTIN("33", 3))), 422, "idempotency.mismatch")
	})

	t.Run("concurrent requests with one key create one organisation", func(t *testing.T) {
		h := g.Vendor()
		h.Set("Idempotency-Key", "onboard-parallel")
		body := orgBody("Parallel Foods", MakeGSTIN("33", 4))
		var wg sync.WaitGroup
		results := make([]itest.Response, 8)
		for i := range results {
			wg.Add(1)
			go func() { defer wg.Done(); results[i] = create(h, body) }()
		}
		wg.Wait()
		var org string
		for _, r := range results {
			if r.Status != 201 && !(r.Status == 409 && r.Code() == "idempotency.in_progress") {
				t.Fatalf("%d %s", r.Status, r.Body)
			}
			if r.Status == 201 {
				o := r.JSON(t)["organisation"].(obj)["id"].(string)
				if org != "" && org != o {
					t.Fatalf("two organisations from one key: %s %s", org, o)
				}
				org = o
			}
		}
		if n := len(directory(t, g, "?q=Parallel+Foods")); n != 1 {
			t.Fatalf("%d organisations named Parallel Foods", n)
		}
	})

	t.Run("invalid input is a 422 naming the field and creates nothing", func(t *testing.T) {
		before := len(directory(t, g, ""))
		bad := func(mut func(b obj)) obj { b := orgBody("Invalid Foods", MakeGSTIN("33", 5)); mut(b); return b }
		cases := []struct {
			name  string
			body  obj
			field string
		}{
			{"unknown outlet kind", bad(func(b obj) { b["outlet"].(obj)["kind"] = "palace" }), "outlet.kind"},
			{"state not in the table", bad(func(b obj) { b["outlet"].(obj)["state_code"] = "99" }), "outlet.state_code"},
			{"bad GSTIN format", bad(func(b obj) { b["tax_registration"].(obj)["gstin"] = "33ABC" }), "tax_registration.gstin"},
			{"bad check character", bad(func(b obj) { b["tax_registration"].(obj)["gstin"] = "33ABCDE0006F1ZX" }), "tax_registration.gstin"},
			{"GSTIN state differs from the outlet state", bad(func(b obj) { b["outlet"].(obj)["state_code"] = "29" }), "tax_registration.gstin"},
			{"unregistered with a GSTIN", bad(func(b obj) { b["tax_registration"].(obj)["regime"] = "unregistered" }), "tax_registration.gstin"},
			{"regular without a GSTIN", bad(func(b obj) { delete(b["tax_registration"].(obj), "gstin") }), "tax_registration.gstin"},
			{"empty organisation name", bad(func(b obj) { b["name"] = " " }), "name"},
			{"empty outlet name", bad(func(b obj) { b["outlet"].(obj)["name"] = "" }), "outlet.name"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := create(g.Vendor(), tc.body)
				want(t, r, 422, "validation")
				if !strings.Contains(string(r.Body), `"field":"`+tc.field+`"`) {
					t.Fatalf("field %s not named: %s", tc.field, r.Body)
				}
			})
		}
		if after := len(directory(t, g, "")); after != before {
			t.Fatalf("invalid requests created organisations: %d -> %d", before, after)
		}
	})

	t.Run("T15: a verified holder blocks vendor creation (409 gstin_in_use) and tenant-side duplicates stay neutral", func(t *testing.T) {
		before := len(directory(t, g, ""))
		want(t, create(g.Vendor(), orgBody("Clash", seed.ExampleGSTIN)), 409, "gstin_in_use")
		if len(directory(t, g, "")) != before {
			t.Fatal("a refused creation must create nothing")
		}
		// Organisation B adds the GSTIN that Example holds verified: accepted, unverified, same response as any new one.
		ownerB := g.Owner(seed.Other.Org)
		dup := g.Call(t, "POST", "/v1/tax-registrations", ownerB, obj{"id": ids.New().String(), "legal_name": "Squatter", "regime": "regular", "gstin": seed.ExampleGSTIN, "state_code": "33"})
		fresh := g.Call(t, "POST", "/v1/tax-registrations", ownerB, obj{"id": ids.New().String(), "legal_name": "Squatter", "regime": "regular", "gstin": MakeGSTIN("33", 60), "state_code": "33"})
		want(t, dup, 201, "")
		want(t, fresh, 201, "")
		shape := func(r itest.Response) string {
			m := r.JSON(t)
			for _, k := range []string{"id", "gstin"} {
				delete(m, k)
			}
			b, _ := json.Marshal(m)
			return string(b)
		}
		if shape(dup) != shape(fresh) || dup.JSON(t)["verification"].(obj)["state"] != "unverified" {
			t.Fatalf("the response must not reveal the other holder:\n%s\n%s", dup.Body, fresh.Body)
		}
		// Vendor verification of the squatter is refused while Example holds the GSTIN, and changes nothing.
		squatter := dup.JSON(t)["id"].(string)
		r := g.Call(t, "PUT", "/internal/v1/vendor/organisations/"+seed.Other.Org.String()+"/tax-registrations/"+squatter+"/verification", g.Vendor(), obj{"method": "certificate_seen"})
		want(t, r, 409, "gstin_in_use")
		if got := g.Call(t, "GET", "/v1/tax-registrations/"+squatter, ownerB, nil).JSON(t)["verification"].(obj)["state"]; got != "unverified" {
			t.Fatalf("state %v", got)
		}
		// Same organisation, same GSTIN, any spelling: refused.
		same := g.Call(t, "POST", "/v1/tax-registrations", ownerB, obj{"id": ids.New().String(), "legal_name": "Again", "regime": "regular", "gstin": seed.ExampleGSTIN, "state_code": "33"})
		want(t, same, 422, "validation")
		if !strings.Contains(string(same.Body), "gstin_duplicate") {
			t.Fatalf("%s", same.Body)
		}
	})

	t.Run("verification is idempotent, needs its permission and fixes the GSTIN", func(t *testing.T) {
		c := parseCreated(t, create(g.Vendor(), orgBody("Verify Foods", MakeGSTIN("33", 7))))
		path := "/internal/v1/vendor/organisations/" + c.Org.String() + "/tax-registrations/" + c.Reg.String() + "/verification"
		want(t, g.Call(t, "PUT", path, g.Vendor("admin.org.read"), obj{"method": "certificate_seen"}), 403, "")
		want(t, g.Call(t, "PUT", path, g.Vendor(), obj{"method": "other"}), 422, "validation") // a note is required
		r := g.Call(t, "PUT", path, g.Vendor(), obj{"method": "certificate_seen", "note": "seen"})
		want(t, r, 200, "")
		v := r.JSON(t)["verification"].(obj)
		if v["state"] != "verified" || v["method"] != "certificate_seen" || v["verified_at"] == nil {
			t.Fatalf("%v", v)
		}
		want(t, g.Call(t, "PUT", path, g.Vendor(), obj{"method": "certificate_seen"}), 200, "")
		// Another organisation's path and a foreign registration are 404.
		bad := "/internal/v1/vendor/organisations/" + seed.Other.Org.String() + "/tax-registrations/" + c.Reg.String() + "/verification"
		want(t, g.Call(t, "PUT", bad, g.Vendor(), obj{"method": "certificate_seen"}), 404, "not_found")
		// The GSTIN is fixed once verified; series can now be issued.
		owner := g.Owner(c.Org)
		fix := g.Call(t, "PATCH", "/v1/tax-registrations/"+c.Reg.String(), owner, obj{"gstin": MakeGSTIN("33", 8)})
		want(t, fix, 409, "registration_fixed")
		issue := obj{"outlet_id": c.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"}
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", c.Org), issue), 201, "")
	})

	t.Run("verification in the same submit needs admin.registration.verify", func(t *testing.T) {
		body := orgBody("Inline Foods", MakeGSTIN("33", 9))
		body["verification"] = obj{"method": "gst_portal_lookup"}
		want(t, create(g.Vendor("admin.org.create", "admin.org.read"), body), 403, "")
		r := create(g.Vendor(), body)
		want(t, r, 201, "")
		if r.JSON(t)["tax_registration"].(obj)["verification"].(obj)["state"] != "verified" {
			t.Fatalf("%s", r.Body)
		}
	})

	t.Run("a GSTIN that comes back gets codes beyond every code issued under it (AC-TEN-21)", func(t *testing.T) {
		gs := MakeGSTIN("33", 10)
		body := orgBody("First Holder", gs)
		body["verification"] = obj{"method": "certificate_seen"}
		x := parseCreated(t, create(g.Vendor(), body))
		var maxIdx int
		code := func(r itest.Response) int {
			c := r.JSON(t)["items"].([]any)[0].(obj)["code"].(string)
			i := strings.Index("ABCDEFGHJKLMNPQRSTUVWXYZ23456789", string(c[0]))*1024 + strings.Index("ABCDEFGHJKLMNPQRSTUVWXYZ23456789", string(c[1]))*32 + strings.Index("ABCDEFGHJKLMNPQRSTUVWXYZ23456789", string(c[2]))
			return i
		}
		for i := 0; i < 6; i++ {
			r := g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", x.Org),
				obj{"outlet_id": x.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"})
			want(t, r, 201, "")
			maxIdx = max(maxIdx, code(r))
		}
		g.CloseRegistration(t, x.Org, x.Reg)
		y := parseCreated(t, create(g.Vendor(), orgBody("Second Holder", gs)))
		want(t, g.Call(t, "PUT", "/internal/v1/vendor/organisations/"+y.Org.String()+"/tax-registrations/"+y.Reg.String()+"/verification", g.Vendor(), obj{"method": "certificate_seen"}), 200, "")
		r := g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", y.Org),
			obj{"outlet_id": y.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"})
		want(t, r, 201, "")
		if got := code(r); got <= maxIdx {
			t.Fatalf("the returning GSTIN's first code index %d is not beyond the earlier holder's %d", got, maxIdx)
		}
	})
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
