package httpsuite

import (
	"net/http"
	"testing"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

func hdr(h ...http.Header) http.Header { return itest.Merge(h...) }

// Realms covers realm separation, service callers, PIN sessions and step-up (T12, TST-14, TST-19, TST-27).
func Realms(t *testing.T, mk Factory) {
	g := mk(t, true)
	g.Seed(t, seed.Example, seed.Other)
	a := seed.Example
	owner := g.Owner(a.Org)

	t.Run("a token of the wrong realm or without the second credential is the same 401", func(t *testing.T) {
		cases := []struct {
			name, method, path string
			h                  http.Header
		}{
			{"no token on a tenant route", "GET", "/v1/organisations/current", http.Header{}},
			{"vendor token on a tenant route", "GET", "/v1/organisations/current", itest.Bearer(g.VendorIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Grants: authtest.OwnerGrants()}))},
			{"tenant token on a vendor route", "GET", "/internal/v1/vendor/organisations", hdr(g.Owner(a.Org), g.ServiceHeaders("admin", a.Org))},
			{"vendor token without the admin service token", "GET", "/internal/v1/vendor/organisations", itest.Bearer(g.VendorIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Grants: authtest.OwnerGrants()}))},
			{"admin service token without a vendor token", "GET", "/internal/v1/vendor/organisations", g.ServiceHeaders("admin", "")},
			{"tenant token on an internal route", "POST", "/internal/v1/document-series-issuances", owner},
			{"support token without the admin service token", "GET", "/v1/organisations/current", itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeSupport, Tenant: a.Org, Grants: authtest.Grants(nil, "organisation.read")}))},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var body any
				if tc.method == "POST" {
					body = obj{"outlet_id": a.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"}
				}
				want(t, g.Call(t, tc.method, tc.path, tc.h, body), 401, "auth.invalid_token")
			})
		}
	})

	t.Run("internal operations accept only the callers they declare", func(t *testing.T) {
		issue := obj{"outlet_id": a.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"}
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("admin", a.Org), issue), 403, "auth.caller_not_allowed")
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", a.Org), issue), 201, "")
		// admin is the only caller of the vendor operations; a vendor token with identity's service token is refused.
		viaIdentity := g.Vendor()
		viaIdentity.Set(auth.ServiceTokenHeader, g.ServiceHeaders("identity", "").Get(auth.ServiceTokenHeader))
		want(t, g.Call(t, "GET", "/internal/v1/vendor/organisations", viaIdentity, nil), 403, "auth.caller_not_allowed")
		// The link decision lookup is for every service ("*").
		want(t, g.Call(t, "GET", "/internal/v1/link-decisions?link_id="+ids.New().String()+"&scope=test.echo.read", g.ServiceHeaders("admin", a.Org), nil), 404, "not_found")
		// A service token with no tenant cannot act on tenant data.
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", ""), issue), 401, "")
	})

	t.Run("a support session reads its one tenant and writes nothing (view mode)", func(t *testing.T) {
		sup := g.SupportHeaders(a.Org)
		org := g.Call(t, "GET", "/v1/organisations/current", sup, nil)
		want(t, org, 200, "")
		if org.JSON(t)["id"] != a.Org.String() {
			t.Fatalf("a support session sees only its tenant: %v", org.JSON(t))
		}
		want(t, g.Call(t, "GET", "/v1/document-series", sup, nil), 200, "")
		want(t, g.Call(t, "GET", "/v1/organisation-settings", sup, nil), 200, "")
		want(t, g.Call(t, "PATCH", "/v1/organisations/current", sup, obj{"name": "Hijack"}), 403, "")
		want(t, g.Call(t, "POST", "/v1/brands", sup, obj{"id": ids.New().String(), "name": "Hijack"}), 403, "")
		want(t, g.Call(t, "GET", "/v1/links", sup, nil), 403, "") // link.read is not support-viewable
		// It cannot read another tenant's resource: the token's tenant is the only context.
		want(t, g.Call(t, "GET", "/v1/outlets/"+seed.Other.Outlet.String(), sup, nil), 404, "not_found")
	})

	t.Run("a PIN session never exercises a strong-only permission (TST-14)", func(t *testing.T) {
		pin := itest.Bearer(g.PinToken(a.Org))
		want(t, g.Call(t, "POST", "/v1/outlets", pin, obj{"id": ids.New().String(), "name": "N", "kind": "stall", "state_code": "33", "tax_registration_id": a.Registration.String()}), 403, "")
		want(t, g.Call(t, "PUT", "/v1/document-series/"+a.Series.String()+"/closure", pin, obj{"reason": "Device stolen and never reconnected."}), 403, "")
		want(t, g.Call(t, "POST", "/v1/links", pin, obj{"id": ids.New().String(), "template": "supplier_buyer", "grants": []obj{{"scope": "test.echo.read", "direction": "outbound"}}}), 403, "")
		want(t, g.Call(t, "POST", "/v1/link-acceptances", pin, obj{"invitation_code": "7QK3M9XD7QK3M9XD"}), 403, "")
		want(t, g.Call(t, "PUT", "/v1/organisation-settings", pin, obj{"offline_window_days": 7, "idle_lock_minutes": 5}), 403, "")
		// Reads that are not strong-only stay available to a PIN session.
		want(t, g.Call(t, "GET", "/v1/outlets", pin, nil), 200, "")
	})

	t.Run("a member without the permission is refused (403, not 404)", func(t *testing.T) {
		noPerm := itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Tenant: a.Org, Grants: authtest.Grants(nil, "brand.read")}))
		want(t, g.Call(t, "PUT", "/v1/document-series/"+a.Series.String()+"/closure", noPerm, obj{"reason": "Device stolen and never reconnected."}), 403, "")
		want(t, g.Call(t, "GET", "/v1/tax-registrations", noPerm, nil), 403, "")
	})

	t.Run("step-up: operations marked x-step-up need a fresh one-time code", func(t *testing.T) {
		stale := itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Tenant: a.Org, Grants: authtest.OwnerGrants(), AuthTime: time.Now().Add(-20 * time.Minute)}))
		reg := obj{"id": ids.New().String(), "legal_name": "S", "regime": "unregistered", "state_code": "33"}
		cases := []struct {
			name, method, path string
			body               any
		}{
			{"create registration", "POST", "/v1/tax-registrations", reg},
			{"update registration", "PATCH", "/v1/tax-registrations/" + a.Registration.String(), obj{"legal_name": "Z"}},
			{"propose link", "POST", "/v1/links", obj{"id": ids.New().String(), "template": "supplier_buyer", "grants": []obj{{"scope": "test.echo.read", "direction": "outbound"}}}},
			{"accept link", "POST", "/v1/link-acceptances", obj{"invitation_code": "7QK3M9XD7QK3M9XD"}},
			{"confirm link", "PUT", "/v1/links/" + ids.New().String() + "/confirmation", obj{"counterparty_organisation_id": seed.Other.Org.String()}},
			{"decline link", "PUT", "/v1/links/" + ids.New().String() + "/declination", obj{"reason": "no"}},
			{"suspend link", "PUT", "/v1/links/" + ids.New().String() + "/suspension", obj{"reason": "pause"}},
			{"revoke link", "PUT", "/v1/links/" + ids.New().String() + "/revocation", obj{"reason": "ended"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				want(t, g.Call(t, tc.method, tc.path, stale, tc.body), 403, "auth.step_up_required")
			})
		}
		want(t, g.Call(t, "POST", "/v1/tax-registrations", owner, reg), 201, "")
		// A vendor-assisted code never satisfies step-up.
		vc := itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Tenant: a.Org, Grants: authtest.OwnerGrants(), AMR: []string{auth.AMRVendorCode}}))
		want(t, g.Call(t, "POST", "/v1/tax-registrations", vc, obj{"id": ids.New().String(), "legal_name": "S2", "regime": "unregistered", "state_code": "33"}), 403, "auth.step_up_required")
	})

	t.Run("a new device under caution needs an approval token for strong-only operations", func(t *testing.T) {
		caution := itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Tenant: a.Org, Grants: authtest.OwnerGrants(), CautionUntil: time.Now().Add(time.Hour)}))
		want(t, g.Call(t, "PUT", "/v1/organisation-settings", caution, obj{"offline_window_days": 7, "idle_lock_minutes": 5}), 403, "approval_required")
		want(t, g.Call(t, "GET", "/v1/organisation-settings", caution, nil), 200, "")
	})
}

// LinksOff proves that the tenant-facing link routes do not exist while LINKS_ENABLED is false (AC-TEN-24).
func LinksOff(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.Seed(t, seed.Example)
	owner := g.Owner(seed.Example.Org)
	id := ids.New().String()
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/links"}, {"POST", "/v1/links"}, {"GET", "/v1/links/" + id}, {"POST", "/v1/link-acceptances"},
		{"PUT", "/v1/links/" + id + "/confirmation"}, {"PUT", "/v1/links/" + id + "/revocation"}, {"GET", "/v1/links/" + id + "/activity"},
	} {
		r := g.Call(t, c.method, c.path, owner, obj{})
		want(t, r, 404, "not_found")
	}
	want(t, g.Call(t, "GET", "/internal/v1/link-decisions?link_id="+id+"&scope=test.echo.read", g.ServiceHeaders("identity", seed.Example.Org), nil), 404, "not_found")
	want(t, g.Call(t, "GET", "/v1/outlets", owner, nil), 200, "") // everything else is unaffected
}
