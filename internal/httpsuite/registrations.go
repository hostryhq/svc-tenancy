package httpsuite

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

const rejectReason = "The certificate shown does not match the legal name."

// Registrations covers the end of a registration's life (ADR-0003 A1.1 items 4 and 6; AC-TEN-20):
// vendor rejection of an unverified registration, and tenant closure of a registration with its series.
func Registrations(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.Seed(t, seed.Example, seed.Other)
	create := func(name string, n int, verify bool) created {
		body := orgBody(name, MakeGSTIN("33", n))
		if verify {
			body["verification"] = obj{"method": "certificate_seen"}
		}
		r := g.Call(t, "POST", "/internal/v1/vendor/organisations", g.Vendor(), body)
		want(t, r, 201, "")
		return parseCreated(t, r)
	}
	rejectPath := func(c created) string {
		return "/internal/v1/vendor/organisations/" + c.Org.String() + "/tax-registrations/" + c.Reg.String() + "/rejection"
	}
	closePath := func(reg ids.ID) string { return "/v1/tax-registrations/" + reg.String() + "/closure" }
	reason := obj{"reason": "Moved the outlets to a new GSTIN."}
	state := func(owner http.Header, reg ids.ID) (map[string]any, string) {
		m := g.Call(t, "GET", "/v1/tax-registrations/"+reg.String(), owner, nil).JSON(t)
		return m["verification"].(obj), m["status"].(string)
	}

	t.Run("rejecting an unverified registration records it, is idempotent and keeps the reason out of the body", func(t *testing.T) {
		c := create("Reject Foods", 300, false)
		want(t, g.Call(t, "PUT", rejectPath(c), g.Vendor("admin.org.read"), obj{"reason": rejectReason}), 403, "")
		want(t, g.Call(t, "PUT", rejectPath(c), g.Vendor(), obj{"reason": "too short"}), 422, "validation")
		r := g.Call(t, "PUT", rejectPath(c), g.Vendor(), obj{"reason": rejectReason})
		want(t, r, 200, "")
		v := r.JSON(t)["verification"].(obj)
		if v["state"] != "rejected" || v["rejected_at"] == nil || v["verified_at"] != nil {
			t.Fatalf("%s", r.Body)
		}
		if strings.Contains(string(r.Body), "certificate shown") {
			t.Fatalf("the reason leaked into the registration: %s", r.Body)
		}
		again := g.Call(t, "PUT", rejectPath(c), g.Vendor(), obj{"reason": rejectReason})
		want(t, again, 200, "")
		if string(again.Body) != string(r.Body) {
			t.Fatalf("a repeat changed the registration:\n%s\n%s", r.Body, again.Body)
		}
		if v, _ := state(g.Owner(c.Org), c.Reg); v["state"] != "rejected" {
			t.Fatalf("the tenant sees %v", v)
		}
		// A rejected registration can get no series.
		issue := obj{"outlet_id": c.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice", "credit_note"}, "state": "active"}
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", c.Org), issue), 409, "registration_unverified")
	})

	t.Run("a verified registration cannot be rejected; another organisation's path is 404", func(t *testing.T) {
		c := create("Verified Foods", 301, true)
		want(t, g.Call(t, "PUT", rejectPath(c), g.Vendor(), obj{"reason": rejectReason}), 409, "registration_verified")
		if v, _ := state(g.Owner(c.Org), c.Reg); v["state"] != "verified" {
			t.Fatalf("a refused rejection changed the registration: %v", v)
		}
		bad := "/internal/v1/vendor/organisations/" + seed.Other.Org.String() + "/tax-registrations/" + c.Reg.String() + "/rejection"
		want(t, g.Call(t, "PUT", bad, g.Vendor(), obj{"reason": rejectReason}), 404, "not_found")
	})

	t.Run("a rejected registration's GSTIN can be corrected, then verified", func(t *testing.T) {
		c := create("Correct Foods", 302, false)
		want(t, g.Call(t, "PUT", rejectPath(c), g.Vendor(), obj{"reason": rejectReason}), 200, "")
		owner := g.Owner(c.Org)
		want(t, g.Call(t, "PATCH", "/v1/tax-registrations/"+c.Reg.String(), owner, obj{"gstin": MakeGSTIN("33", 303)}), 200, "")
		verify := "/internal/v1/vendor/organisations/" + c.Org.String() + "/tax-registrations/" + c.Reg.String() + "/verification"
		want(t, g.Call(t, "PUT", verify, g.Vendor(), obj{"method": "certificate_seen"}), 200, "")
	})

	t.Run("closing is refused while an active outlet bills under the registration, then closes it with its series", func(t *testing.T) {
		c := create("Close Foods", 304, true)
		owner := g.Owner(c.Org)
		issue := obj{"outlet_id": c.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice", "credit_note"}, "state": "active"}
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", c.Org), issue), 201, "")
		want(t, g.Call(t, "PUT", closePath(c.Reg), owner, obj{"reason": "short"}), 422, "validation")
		want(t, g.Call(t, "PUT", closePath(c.Reg), owner, reason), 409, "registration_in_use")
		if _, st := state(owner, c.Reg); st != "open" {
			t.Fatalf("a refused closure changed the registration: %s", st)
		}
		want(t, g.Call(t, "PATCH", "/v1/outlets/"+c.Outlet.String(), owner, obj{"status": "inactive"}), 200, "")
		r := g.Call(t, "PUT", closePath(c.Reg), owner, reason)
		want(t, r, 200, "")
		if r.JSON(t)["status"] != "closed" || strings.Contains(string(r.Body), "Moved the outlets") {
			t.Fatalf("%s", r.Body)
		}
		ser := g.Call(t, "GET", "/v1/document-series", owner, nil).JSON(t)["items"].([]any)
		if len(ser) != 2 {
			t.Fatalf("series: %v", ser)
		}
		for _, s := range ser {
			if s.(obj)["status"] != "closed" || s.(obj)["closed_at"] == nil {
				t.Fatalf("a series of a closed registration is still open: %v", s)
			}
		}
		again := g.Call(t, "PUT", closePath(c.Reg), owner, reason)
		want(t, again, 200, "")
		if string(again.Body) != string(r.Body) {
			t.Fatalf("a repeat changed the registration:\n%s\n%s", r.Body, again.Body)
		}
		// A closed registration cannot be corrected, and no series are issued under it.
		want(t, g.Call(t, "PATCH", "/v1/tax-registrations/"+c.Reg.String(), owner, obj{"legal_name": "Renamed"}), 409, "registration_fixed")
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", c.Org), issue), 409, "")
	})

	t.Run("closing a registration frees its GSTIN for another organisation to verify", func(t *testing.T) {
		c := create("Hold Foods", 305, true)
		gs := MakeGSTIN("33", 305)
		squatter := g.Call(t, "POST", "/v1/tax-registrations", g.Owner(seed.Other.Org), obj{"id": ids.New().String(), "legal_name": "Next Holder", "regime": "regular", "gstin": gs, "state_code": "33"})
		want(t, squatter, 201, "")
		verify := "/internal/v1/vendor/organisations/" + seed.Other.Org.String() + "/tax-registrations/" + squatter.JSON(t)["id"].(string) + "/verification"
		want(t, g.Call(t, "PUT", verify, g.Vendor(), obj{"method": "certificate_seen"}), 409, "gstin_in_use")
		owner := g.Owner(c.Org)
		want(t, g.Call(t, "PATCH", "/v1/outlets/"+c.Outlet.String(), owner, obj{"status": "inactive"}), 200, "")
		want(t, g.Call(t, "PUT", closePath(c.Reg), owner, reason), 200, "")
		want(t, g.Call(t, "PUT", verify, g.Vendor(), obj{"method": "certificate_seen"}), 200, "")
	})

	t.Run("closing needs tax_registration.manage, and an unknown or foreign id is 404", func(t *testing.T) {
		a := seed.Example
		reader := itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Tenant: a.Org, Grants: authtest.Grants(nil, "tax_registration.read")}))
		want(t, g.Call(t, "PUT", closePath(a.Registration), reader, reason), 403, "")
		want(t, g.Call(t, "PUT", closePath(ids.New()), g.Owner(a.Org), reason), 404, "not_found")
		if _, st := state(g.Owner(a.Org), a.Registration); st != "open" {
			t.Fatalf("status %s", st)
		}
	})
}
