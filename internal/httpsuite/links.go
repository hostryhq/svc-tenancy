package httpsuite

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/contractenv"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// specChecker validates responses against the tenancy OpenAPI document.
type specChecker struct{ router routers.Router }

func newSpecChecker(t testing.TB) *specChecker {
	t.Helper()
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile(contractenv.SpecPath(t))
	if err != nil {
		t.Fatal(err)
	}
	doc.Servers = nil
	r, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &specChecker{r}
}

// check fails the test when a response violates the contract (undeclared status, media type or body shape).
func (s *specChecker) check(t testing.TB, method, path string, status int, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://spec"+path, nil)
	route, params, err := s.router.FindRoute(req)
	if err != nil {
		t.Fatalf("%s %s is not in the spec: %v", method, path, err)
	}
	ct := "application/json"
	if status >= 400 {
		ct = "application/problem+json"
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 status, Header: http.Header{"Content-Type": []string{ct}}, Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		t.Fatalf("%s %s -> %d violates the contract: %v\n%s", method, path, status, err, body)
	}
}

// Links drives the whole link control plane over HTTP with the test-only scopes (T6 control-plane half, T9,
// T17; TST-19 to TST-23) and checks every response against the OpenAPI document.
func Links(t *testing.T, mk Factory) {
	g := mk(t, true)
	g.Seed(t, seed.Example, seed.Other, seed.Third)
	spec := newSpecChecker(t)
	a, b, c := seed.Example, seed.Other, seed.Third
	ownerA, ownerB, ownerC := g.Owner(a.Org), g.Owner(b.Org), g.Owner(c.Org)
	call := func(method, path string, h http.Header, body any, status int) itest.Response {
		t.Helper()
		r := g.Call(t, method, path, h, body)
		if r.Status != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.Status, status, r.Body)
		}
		spec.check(t, method, strings.SplitN(path, "?", 2)[0], r.Status, r.Body)
		return r
	}
	decision := func(h http.Header, link, scope string) obj {
		return call("GET", "/internal/v1/link-decisions?link_id="+link+"&scope="+scope, h, nil, 200).JSON(t)
	}
	idA, idB := g.ServiceHeaders("billing", a.Org), g.ServiceHeaders("billing", b.Org)

	linkID, code := proposeLink(t, g, ownerA)
	got := call("GET", "/v1/links/"+linkID, ownerA, nil, 200).JSON(t)
	if got["status"] != "proposed" || got["role"] != "proposer" || got["counterparty"] != nil || got["counterparty_organisation_id"] != nil {
		t.Fatalf("the inviter learns nothing before acceptance: %v", got)
	}
	call("GET", "/v1/links?status=proposed", ownerA, nil, 200)
	call("GET", "/v1/links/"+linkID, ownerB, nil, 404)

	t.Run("a malformed, unknown or own code is one answer", func(t *testing.T) {
		for _, bad := range []string{"7QK3M9XD7QK3M9XD", "SHORT", strings.Repeat("I", 16)} {
			r := call("POST", "/v1/link-acceptances", ownerB, obj{"invitation_code": bad}, 409)
			if r.Code() != "invitation_code_invalid" {
				t.Fatalf("%s", r.Body)
			}
		}
		call("POST", "/v1/link-acceptances", ownerA, obj{"invitation_code": code}, 409) // the proposer cannot accept its own link
	})

	t.Run("accepted: nothing is active until the proposer confirms who accepted", func(t *testing.T) {
		acc := call("POST", "/v1/link-acceptances", ownerB, obj{"invitation_code": code, "outlet_ids": []string{b.Outlet.String()}}, 201).JSON(t)
		if acc["status"] != "accepted" || acc["role"] != "acceptor" || acc["counterparty"].(obj)["display_name"] != a.Name {
			t.Fatalf("the acceptor is told whom it accepted: %v", acc)
		}
		call("POST", "/v1/link-acceptances", ownerB, obj{"invitation_code": code}, 409) // single use (a replay with the key returns the first answer)
		for _, h := range []http.Header{idA, idB} {
			if d := decision(h, linkID, "test.echo.read"); d["allowed"] != false || d["reason"] != "link_not_active" {
				t.Fatalf("an accepted link authorises nothing: %v", d)
			}
		}
		seen := call("GET", "/v1/links/"+linkID, ownerA, nil, 200).JSON(t)
		reg := seen["counterparty"].(obj)["registrations"].([]any)[0].(obj)
		if seen["counterparty_organisation_id"] != b.Org.String() || reg["legal_name"] != b.Name+" Pvt Ltd" || reg["gstin"] != b.GSTIN || reg["gstin_verified"] != true {
			t.Fatalf("the proposer sees the accepting organisation before confirming: %v", seen)
		}
		call("PUT", "/v1/links/"+linkID+"/confirmation", ownerA, obj{"counterparty_organisation_id": c.Org.String()}, 422) // a swap is refused
		call("PUT", "/v1/links/"+linkID+"/confirmation", ownerB, obj{"counterparty_organisation_id": b.Org.String()}, 409)
		act := call("PUT", "/v1/links/"+linkID+"/confirmation", ownerA, obj{"counterparty_organisation_id": b.Org.String()}, 200).JSON(t)
		if act["status"] != "active" || act["confirmed_at"] == nil {
			t.Fatalf("%v", act)
		}
	})

	t.Run("an active link authorises exactly its grants (decision lookup for go-auth)", func(t *testing.T) {
		if d := decision(idB, linkID, "test.echo.read"); d["allowed"] != true || d["data_owner_organisation_id"] != a.Org.String() || d["cache_seconds"].(float64) > 30 {
			t.Fatalf("grantee: %v", d)
		}
		if d := decision(idA, linkID, "test.echo.read"); d["allowed"] != false || d["reason"] != "scope_not_granted" {
			t.Fatalf("the grantor is not the grantee: %v", d)
		}
		if d := decision(idA, linkID, "test.echo.post"); d["allowed"] != true || d["data_owner_organisation_id"] != b.Org.String() {
			t.Fatalf("reverse direction: %v", d)
		}
		call("GET", "/internal/v1/link-decisions?link_id="+linkID+"&scope=test.echo.read", g.ServiceHeaders("billing", c.Org), nil, 404) // another pair's link
	})

	t.Run("suspend, resume, amend and remove a grant take effect at once", func(t *testing.T) {
		call("PUT", "/v1/links/"+linkID+"/suspension", ownerB, obj{"reason": "under review"}, 200)
		if d := decision(idB, linkID, "test.echo.read"); d["allowed"] != false {
			t.Fatalf("suspended: %v", d)
		}
		call("DELETE", "/v1/links/"+linkID+"/suspension", ownerA, nil, 409) // only the suspending party resumes
		call("DELETE", "/v1/links/"+linkID+"/suspension", ownerB, nil, 200)
		amID := ids.New().String()
		call("POST", "/v1/links/"+linkID+"/amendments", ownerA, obj{"id": amID, "add_grants": []obj{{"scope": "test.echo.read", "direction": "inbound"}}}, 201)
		call("PUT", "/v1/links/"+linkID+"/amendments/"+amID+"/decision", ownerA, obj{"decision": "accept"}, 409) // not its own
		call("PUT", "/v1/links/"+linkID+"/amendments/"+amID+"/decision", ownerB, obj{"decision": "accept"}, 200)
		if d := decision(idA, linkID, "test.echo.read"); d["allowed"] != true {
			t.Fatalf("accepted amendment: %v", d)
		}
		var grantID string
		for _, gr := range call("GET", "/v1/links/"+linkID, ownerA, nil, 200).JSON(t)["grants"].([]any) {
			if gr.(obj)["scope"] == "test.echo.read" && gr.(obj)["direction"] == "outbound" {
				grantID = gr.(obj)["id"].(string)
			}
		}
		call("DELETE", "/v1/links/"+linkID+"/grants/"+grantID, ownerA, nil, 204)
		if d := decision(idB, linkID, "test.echo.read"); d["allowed"] != false || d["reason"] != "scope_not_granted" {
			t.Fatalf("grant removed: %v", d)
		}
		call("DELETE", "/v1/links/"+linkID+"/grants/"+ids.New().String(), ownerA, nil, 404)
	})

	t.Run("revocation is terminal; the log is append-only and readable by both parties", func(t *testing.T) {
		call("PUT", "/v1/links/"+linkID+"/revocation", ownerA, obj{"reason": "contract ended"}, 200)
		call("PUT", "/v1/links/"+linkID+"/revocation", ownerB, obj{"reason": "contract ended"}, 200) // idempotent
		if d := decision(idA, linkID, "test.echo.post"); d["allowed"] != false || d["reason"] != "link_not_active" {
			t.Fatalf("revoked: %v", d)
		}
		call("DELETE", "/v1/links/"+linkID+"/suspension", ownerB, nil, 409)
		for _, h := range []http.Header{ownerA, ownerB} {
			items := call("GET", "/v1/links/"+linkID+"/activity", h, nil, 200).JSON(t)["items"].([]any)
			var actions []string
			for _, i := range items {
				row := i.(obj)
				actions = append(actions, row["action"].(string))
				for k := range row {
					if k == "payload" || k == "body" || k == "reason" {
						t.Fatalf("activity carries %q", k)
					}
				}
			}
			if len(actions) < 8 || actions[0] != "link.proposed" || actions[1] != "link.accepted" || actions[2] != "link.confirmed" {
				t.Fatalf("activity: %v", actions)
			}
		}
		call("GET", "/v1/links/"+linkID+"/activity", ownerC, nil, 404)
		call("GET", "/v1/links?status=revoked", ownerA, nil, 200)
	})

	t.Run("a forwarded code accepted by a third organisation activates nothing; rejecting ends the link", func(t *testing.T) {
		id2, code2 := proposeLink(t, g, ownerA)
		call("POST", "/v1/link-acceptances", g.Owner(c.Org), obj{"invitation_code": code2}, 201)
		if d := decision(g.ServiceHeaders("billing", c.Org), id2, "test.echo.read"); d["allowed"] != false {
			t.Fatalf("%v", d)
		}
		call("POST", "/v1/link-acceptances", ownerB, obj{"invitation_code": code2}, 409) // the intended acceptor holds a used code
		call("PUT", "/v1/links/"+id2+"/declination", ownerA, obj{"reason": "not our partner"}, 200)
		call("PUT", "/v1/links/"+id2+"/confirmation", ownerA, obj{"counterparty_organisation_id": c.Org.String()}, 409)
	})

	t.Run("link changes are published with the counterparty (ADR-0011 B7)", func(t *testing.T) {
		verbs := map[string]bool{}
		for _, e := range g.Events(t) {
			if strings.HasPrefix(e.Type, "tenancy.link.") {
				verbs[strings.TrimPrefix(e.Type, "tenancy.link.")] = true
				if e.Type != "tenancy.link.proposed" && (e.CounterpartyTenantID == "" || e.LinkID == "") {
					t.Fatalf("%s is not link-addressed: %+v", e.Type, e)
				}
			}
		}
		for _, v := range []string{"proposed", "accepted", "confirmed", "suspended", "resumed", "amended", "revoked", "declined"} {
			if !verbs[v] {
				t.Fatalf("no tenancy.link.%s event (have %v)", v, verbs)
			}
		}
	})
	// Last, because the limit is per address as well as per tenant and every test client shares one address.
	t.Run("redemption is rate limited per tenant and address", func(t *testing.T) {
		var last itest.Response
		for i := 0; i < 12; i++ {
			last = g.Call(t, "POST", "/v1/link-acceptances", ownerC, obj{"invitation_code": "7QK3M9XD7QK3M9XD"})
		}
		if last.Status != 429 || last.Header.Get("Retry-After") == "" {
			t.Fatalf("expected 429 with Retry-After, got %d %v", last.Status, last.Header)
		}
		spec.check(t, "POST", "/v1/link-acceptances", last.Status, last.Body)
	})
}
