package httpsuite

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

type obj = map[string]any

func want(t testing.TB, r itest.Response, status int, code string) {
	t.Helper()
	if r.Status != status || (code != "" && r.Code() != code) {
		t.Fatalf("status %d code %q, want %d %q: %s", r.Status, r.Code(), status, code, r.Body)
	}
}

func itemIDs(t testing.TB, r itest.Response) []string {
	t.Helper()
	want(t, r, 200, "")
	var p struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range p.Items {
		out = append(out, i.ID)
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// proposeLink makes A propose a link and returns its id and code.
func proposeLink(t testing.TB, g *Target, h http.Header) (id, code string) {
	t.Helper()
	id = ids.New().String()
	r := g.Call(t, "POST", "/v1/links", h, obj{"id": id, "template": "on_premises_outlet", "outlet_ids": []string{seed.Outlet.String()},
		"grants": []obj{{"scope": "test.echo.read", "direction": "outbound", "outlet_ids": []string{seed.Outlet.String()}}, {"scope": "test.echo.post", "direction": "inbound"}}})
	want(t, r, 201, "")
	return id, r.JSON(t)["invitation_code"].(string)
}

// Isolation proves that tenant A can never read or write tenant B's data through any operation
// (ADR-0003 T2, T3, T10; TST-02, TST-08, TST-09, TST-10).
func Isolation(t *testing.T, mk Factory) {
	g := mk(t, true)
	g.Seed(t, seed.Example, seed.Other)
	a, b := seed.Example, seed.Other
	ownerA, ownerB := g.Owner(a.Org), g.Owner(b.Org)
	linkID, _ := proposeLink(t, g, ownerA)

	t.Run("tenant B gets 404 for every operation on tenant A's resource", func(t *testing.T) {
		series := "/v1/document-series/" + a.Series.String() + "/closure"
		reason := obj{"reason": "Device stolen and never reconnected."}
		cases := []struct {
			name, method, path string
			body               any
		}{
			{"get brand", "GET", "/v1/brands/" + a.Brand.String(), nil},
			{"get outlet", "GET", "/v1/outlets/" + a.Outlet.String(), nil},
			{"update outlet", "PATCH", "/v1/outlets/" + a.Outlet.String(), obj{"name": "Hijacked"}},
			{"get registration", "GET", "/v1/tax-registrations/" + a.Registration.String(), nil},
			{"update registration", "PATCH", "/v1/tax-registrations/" + a.Registration.String(), obj{"legal_name": "Hijacked"}},
			{"close registration", "PUT", "/v1/tax-registrations/" + a.Registration.String() + "/closure", reason},
			{"close series", "PUT", series, reason},
			{"get link", "GET", "/v1/links/" + linkID, nil},
			{"link activity", "GET", "/v1/links/" + linkID + "/activity", nil},
			{"confirm link", "PUT", "/v1/links/" + linkID + "/confirmation", obj{"counterparty_organisation_id": b.Org.String()}},
			{"decline link", "PUT", "/v1/links/" + linkID + "/declination", obj{"reason": "not ours"}},
			{"suspend link", "PUT", "/v1/links/" + linkID + "/suspension", obj{"reason": "not ours"}},
			{"resume link", "DELETE", "/v1/links/" + linkID + "/suspension", nil},
			{"revoke link", "PUT", "/v1/links/" + linkID + "/revocation", obj{"reason": "not ours"}},
			{"amend link", "POST", "/v1/links/" + linkID + "/amendments", obj{"id": ids.New().String(), "add_grants": []obj{{"scope": "test.echo.read", "direction": "inbound"}}}},
			{"remove grant", "DELETE", "/v1/links/" + linkID + "/grants/" + ids.New().String(), nil},
			{"outlet of tenant A as brand parent in an outlet create", "POST", "/v1/outlets", obj{"id": ids.New().String(), "name": "X", "kind": "restaurant", "state_code": "29", "tax_registration_id": a.Registration.String()}},
			{"outlet create with tenant A's brand", "POST", "/v1/outlets", obj{"id": ids.New().String(), "brand_id": a.Brand.String(), "name": "X", "kind": "restaurant", "state_code": "29", "tax_registration_id": b.Registration.String()}},
		}
		var seenBodies []string
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := g.Call(t, tc.method, tc.path, ownerB, tc.body)
				want(t, r, 404, "not_found")
				seenBodies = append(seenBodies, string(r.Body))
			})
		}
		_ = seenBodies
	})

	t.Run("internal operations bound to the tenant named by the service token", func(t *testing.T) {
		// Identity names tenant B but the outlet and the device are tenant A's.
		h := g.ServiceHeaders("identity", b.Org)
		r := g.Call(t, "POST", "/internal/v1/document-series-issuances", h, obj{"outlet_id": a.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice", "credit_note"}, "state": "active"})
		want(t, r, 404, "not_found")
		r = g.Call(t, "PUT", "/internal/v1/devices/"+a.Device.String()+"/series-closure", h, obj{"reason": "revoked"})
		want(t, r, 404, "not_found")
		r = g.Call(t, "GET", "/internal/v1/link-decisions?link_id="+linkID+"&scope=test.echo.read", h, nil)
		want(t, r, 404, "not_found")
		// A's series is still active.
		rows := itemIDs(t, g.Call(t, "GET", "/v1/document-series?status=active", ownerA, nil))
		if !contains(rows, a.Series.String()) {
			t.Fatal("tenant B's call changed tenant A's series")
		}
	})

	t.Run("lists never contain the other tenant's rows", func(t *testing.T) {
		for _, c := range []struct{ path, mine, theirs string }{
			{"/v1/brands", b.Brand.String(), a.Brand.String()},
			{"/v1/outlets", b.Outlet.String(), a.Outlet.String()},
			{"/v1/tax-registrations", b.Registration.String(), a.Registration.String()},
			{"/v1/document-series", b.Series.String(), a.Series.String()},
		} {
			got := itemIDs(t, g.Call(t, "GET", c.path, ownerB, nil))
			if !contains(got, c.mine) || contains(got, c.theirs) {
				t.Fatalf("%s as B: %v", c.path, got)
			}
		}
		if got := itemIDs(t, g.Call(t, "GET", "/v1/links", ownerB, nil)); contains(got, linkID) {
			t.Fatal("B lists A's link")
		}
		org := g.Call(t, "GET", "/v1/organisations/current", ownerB, nil).JSON(t)
		if org["id"] != b.Org.String() || org["name"] != b.Name {
			t.Fatalf("organisations/current as B: %v", org)
		}
	})

	t.Run("a tenant id in the body, query or header is rejected or ignored (TST-08)", func(t *testing.T) {
		for _, field := range []string{"tenant_id", "organisation_id", "tid"} {
			r := g.Call(t, "POST", "/v1/brands", ownerB, obj{"id": ids.New().String(), "name": "Injected", field: a.Org.String()})
			want(t, r, 422, "validation")
		}
		h := g.Owner(b.Org)
		h.Set("X-Tenant-Id", a.Org.String())
		id := ids.New().String()
		want(t, g.Call(t, "POST", "/v1/brands?tenant_id="+a.Org.String(), h, obj{"id": id, "name": "Mine"}), 201, "")
		if contains(itemIDs(t, g.Call(t, "GET", "/v1/brands", ownerA, nil)), id) {
			t.Fatal("a header or query tenant id moved the row into tenant A")
		}
		if !contains(itemIDs(t, g.Call(t, "GET", "/v1/brands", ownerB, nil)), id) {
			t.Fatal("the row must be stored under the token's tenant")
		}
	})

	t.Run("the same client-generated id in two tenants succeeds independently (TST-09)", func(t *testing.T) {
		same := ids.New().String()
		want(t, g.Call(t, "POST", "/v1/brands", ownerA, obj{"id": same, "name": "A's"}), 201, "")
		want(t, g.Call(t, "POST", "/v1/brands", ownerB, obj{"id": same, "name": "B's"}), 201, "")
		if got := g.Call(t, "GET", "/v1/brands/"+same, ownerA, nil).JSON(t)["name"]; got != "A's" {
			t.Fatalf("A reads %v", got)
		}
		if got := g.Call(t, "GET", "/v1/brands/"+same, ownerB, nil).JSON(t)["name"]; got != "B's" {
			t.Fatalf("B reads %v", got)
		}
		// The same Idempotency-Key in two tenants is two requests.
		h1, h2 := g.Owner(a.Org), g.Owner(b.Org)
		h1.Set("Idempotency-Key", "shared-key")
		h2.Set("Idempotency-Key", "shared-key")
		id := ids.New().String()
		want(t, g.Call(t, "POST", "/v1/brands", h1, obj{"id": id, "name": "K"}), 201, "")
		want(t, g.Call(t, "POST", "/v1/brands", h2, obj{"id": id, "name": "K"}), 201, "")
	})

	t.Run("a foreign id and a random id give the same 404 (TST-10)", func(t *testing.T) {
		strip := func(r itest.Response) string {
			m := r.JSON(t)
			delete(m, "request_id")
			delete(m, "instance")
			b, _ := json.Marshal(m)
			return string(b)
		}
		var foreign, random []time.Duration
		var fb, rb string
		for i := 0; i < 30; i++ {
			t0 := time.Now()
			f := g.Call(t, "GET", "/v1/outlets/"+a.Outlet.String(), ownerB, nil)
			foreign = append(foreign, time.Since(t0))
			t0 = time.Now()
			r := g.Call(t, "GET", "/v1/outlets/"+ids.New().String(), ownerB, nil)
			random = append(random, time.Since(t0))
			fb, rb = strip(f), strip(r)
			if f.Status != 404 || r.Status != 404 || fb != rb {
				t.Fatalf("foreign %d %s / random %d %s", f.Status, fb, r.Status, rb)
			}
		}
		med := func(d []time.Duration) time.Duration {
			sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
			return d[len(d)/2]
		}
		if diff := med(foreign) - med(random); diff > 25*time.Millisecond || diff < -25*time.Millisecond {
			t.Fatalf("median latency differs by %v (foreign %v, random %v)", diff, med(foreign), med(random))
		}
	})

	t.Run("an attacker's writes left tenant A's data untouched", func(t *testing.T) {
		o := g.Call(t, "GET", "/v1/outlets/"+a.Outlet.String(), ownerA, nil).JSON(t)
		if o["name"] != "Main Restaurant" {
			t.Fatalf("outlet renamed: %v", o)
		}
		r := g.Call(t, "GET", "/v1/tax-registrations/"+a.Registration.String(), ownerA, nil).JSON(t)
		if r["legal_name"] != a.Name+" Pvt Ltd" {
			t.Fatalf("registration renamed: %v", r)
		}
		l := g.Call(t, "GET", "/v1/links/"+linkID, ownerA, nil).JSON(t)
		if l["status"] != "proposed" {
			t.Fatalf("link changed: %v", l["status"])
		}
	})
}

// OutletScope is T14 over HTTP (TST-06): a member granted outlet X only gets nothing of outlet Y from
// lists, filters and reads, and a changed Y resource is 404; an organisation-wide member sees both.
func OutletScope(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.Seed(t, seed.Other)
	w := seed.Other
	tok := func(outlets ...string) http.Header {
		perms := []string{"outlet.read", "outlet.manage", "series.read", "series.close_unreconciled"}
		return itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Tenant: w.Org, Grants: authtest.Grants(outlets, perms...)}))
	}
	x, y := w.Outlet.String(), w.OutletY.String()
	// A second series, at outlet Y, so each outlet has one.
	r := g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", w.Org),
		obj{"outlet_id": y, "device_id": ids.New().String(), "doc_types": []string{"tax_invoice", "credit_note"}, "state": "active"})
	want(t, r, 201, "")
	seriesY := r.JSON(t)["items"].([]any)[0].(obj)["id"].(string)

	scoped, orgWide := tok(x), tok()
	if got := itemIDs(t, g.Call(t, "GET", "/v1/outlets", scoped, nil)); len(got) != 1 || got[0] != x {
		t.Fatalf("outlets listed for X-only member: %v", got)
	}
	want(t, g.Call(t, "GET", "/v1/outlets/"+y, scoped, nil), 404, "not_found")
	want(t, g.Call(t, "PATCH", "/v1/outlets/"+y, scoped, obj{"name": "Nope"}), 404, "not_found")
	want(t, g.Call(t, "GET", "/v1/outlets/"+x, scoped, nil), 200, "")
	if got := itemIDs(t, g.Call(t, "GET", "/v1/document-series", scoped, nil)); contains(got, seriesY) || len(got) == 0 {
		t.Fatalf("series listed for X-only member: %v", got)
	}
	if got := itemIDs(t, g.Call(t, "GET", "/v1/document-series?outlet_id="+y, scoped, nil)); len(got) != 0 {
		t.Fatalf("an outlet_id filter must not widen the scope: %v", got)
	}
	want(t, g.Call(t, "PUT", "/v1/document-series/"+seriesY+"/closure", scoped, obj{"reason": "Device stolen and never reconnected."}), 404, "not_found")
	if got := itemIDs(t, g.Call(t, "GET", "/v1/outlets", orgWide, nil)); !contains(got, x) || !contains(got, y) {
		t.Fatalf("an organisation-wide member sees both outlets: %v", got)
	}
	if got := itemIDs(t, g.Call(t, "GET", "/v1/document-series?outlet_id="+y, orgWide, nil)); len(got) != 2 || !contains(got, seriesY) {
		t.Fatalf("filter for an organisation-wide member: %v", got)
	}
	want(t, g.Call(t, "PUT", "/v1/document-series/"+seriesY+"/closure", orgWide, obj{"reason": "Device stolen and never reconnected."}), 200, "")
	// A member with no grant of the permission at all is refused, not shown an empty list.
	none := itest.Bearer(g.TenantIssuer.Mint(authtest.Spec{Tenant: w.Org, Grants: authtest.Grants([]string{x}, "brand.read")}))
	want(t, g.Call(t, "GET", "/v1/outlets", none, nil), 403, "")
}
