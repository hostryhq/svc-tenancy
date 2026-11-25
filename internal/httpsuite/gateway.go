package httpsuite

import (
	"net/http"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// Gateway covers what the sync gateway asks tenancy (ADR-0005 A3.2 row 8) and the issuance pair (soak
// finding F5): the series of a device by device id, who owns a stream, the paper books of an outlet, and the
// credit_note series that every billing device is issued with its invoice series.
func Gateway(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.SetNow(seed.At)
	a := orgIn(t, g, "Gateway A", "33", 1)
	b := orgIn(t, g, "Gateway B", "33", 2)
	sync := func(tenant ids.ID) http.Header { return g.ServiceHeaders("sync", tenant) }
	get := func(h http.Header, path string) itest.Response { return g.Call(t, "GET", path, h, nil) }

	t.Run("a billing device is issued its invoice series with a credit_note series, or nothing", func(t *testing.T) {
		body := func(types ...string) obj {
			return obj{"outlet_id": a.Outlet.String(), "device_id": ids.New().String(), "doc_types": types, "state": "active"}
		}
		identity := g.ServiceHeaders("identity", a.Org)
		for _, types := range [][]string{{"tax_invoice"}, {"bill_of_supply"}, {"plain_bill", "bill_of_supply"}} {
			want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", identity, body(types...)), 422, "series.credit_note_required")
		}
		if n := len(eventsOf(t, g, "tenancy.series.issued")); n != 0 {
			t.Fatalf("a refused request issues nothing: %d series.issued events", n)
		}
		d := body("tax_invoice", "credit_note")
		pair := g.Call(t, "POST", "/internal/v1/document-series-issuances", identity, d)
		want(t, pair, 201, "")
		items := pair.JSON(t)["items"].([]any)
		if len(items) != 2 || items[0].(obj)["issued_at"] != items[1].(obj)["issued_at"] || items[0].(obj)["status"] != items[1].(obj)["status"] {
			t.Fatalf("the pair shares its state and instant: %v", items)
		}
		// A credit_note alone is accepted for a device that already holds its invoice series.
		d["doc_types"] = []string{"credit_note"}
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", identity, d), 201, "")
	})

	var device string
	t.Run("the series of a device are answered by device id, closed ones included, and only to the gateway", func(t *testing.T) {
		device = ids.New().String()
		issue := obj{"outlet_id": a.Outlet.String(), "device_id": device, "doc_types": []string{"tax_invoice", "credit_note"}, "state": "active"}
		want(t, g.Call(t, "POST", "/internal/v1/document-series-issuances", g.ServiceHeaders("identity", a.Org), issue), 201, "")
		path := "/internal/v1/devices/" + device + "/series"
		r := get(sync(a.Org), path)
		want(t, r, 200, "")
		if items := r.JSON(t)["items"].([]any); len(items) != 2 || items[0].(obj)["doc_type"] != "tax_invoice" || items[1].(obj)["doc_type"] != "credit_note" {
			t.Fatalf("the pair: %s", r.Body)
		}
		want(t, g.Call(t, "PUT", "/internal/v1/devices/"+device+"/series-closure", g.ServiceHeaders("identity", a.Org), obj{"reason": "revoked"}), 200, "")
		r = get(sync(a.Org), path)
		items := r.JSON(t)["items"].([]any)
		if len(items) != 2 || items[0].(obj)["status"] != "closed" {
			t.Fatalf("a closed series is still answered: %s", r.Body)
		}
		// Another organisation's token sees nothing of it; an unknown device is an empty list, not an error.
		if got := get(sync(b.Org), path).JSON(t)["items"].([]any); len(got) != 0 {
			t.Fatalf("tenant B reads tenant A's device series: %v", got)
		}
		if got := get(sync(a.Org), "/internal/v1/devices/"+ids.New().String()+"/series").JSON(t)["items"].([]any); len(got) != 0 {
			t.Fatalf("an unknown device has no series: %v", got)
		}
		want(t, get(g.ServiceHeaders("identity", a.Org), path), 403, "auth.caller_not_allowed")
		want(t, get(g.ServiceHeaders("admin", a.Org), path), 403, "auth.caller_not_allowed")
	})

	t.Run("a stream owner is an outlet or a brand of the token's organisation, and nothing else", func(t *testing.T) {
		brands := get(g.Owner(a.Org), "/v1/brands").JSON(t)["items"].([]any)
		brand := brands[0].(obj)["id"].(string)
		r := get(sync(a.Org), "/internal/v1/stream-owners/"+a.Outlet.String())
		want(t, r, 200, "")
		if m := r.JSON(t); m["kind"] != "outlet" || m["organisation_id"] != a.Org.String() || m["brand_id"] != brand {
			t.Fatalf("outlet owner: %s", r.Body)
		}
		r = get(sync(a.Org), "/internal/v1/stream-owners/"+brand)
		want(t, r, 200, "")
		if m := r.JSON(t); m["kind"] != "brand" || m["organisation_id"] != a.Org.String() || m["brand_id"] != brand {
			t.Fatalf("brand owner: %s", r.Body)
		}
		want(t, get(sync(b.Org), "/internal/v1/stream-owners/"+a.Outlet.String()), 404, "not_found")
		want(t, get(sync(b.Org), "/internal/v1/stream-owners/"+brand), 404, "not_found")
		want(t, get(sync(a.Org), "/internal/v1/stream-owners/"+ids.New().String()), 404, "not_found")
		want(t, get(g.ServiceHeaders("orders", a.Org), "/internal/v1/stream-owners/"+a.Outlet.String()), 403, "auth.caller_not_allowed")
	})

	t.Run("the paper books of an outlet are answered to the gateway and identity, and registering one is published", func(t *testing.T) {
		owner := g.Owner(a.Org)
		book := g.Call(t, "POST", "/v1/manual-book-series", owner, obj{"tax_registration_id": a.Reg.String(), "code": "BKG", "outlet_ids": []string{a.Outlet.String()}})
		want(t, book, 201, "")
		path := "/internal/v1/outlets/" + a.Outlet.String() + "/manual-book-series"
		r := get(sync(a.Org), path)
		want(t, r, 200, "")
		if items := r.JSON(t)["items"].([]any); len(items) != 1 || items[0].(obj)["code"] != "BKG" || items[0].(obj)["id"] != book.JSON(t)["id"] {
			t.Fatalf("the books of the outlet: %s", r.Body)
		}
		want(t, get(g.ServiceHeaders("identity", a.Org), path), 200, "")
		want(t, get(sync(b.Org), path), 404, "not_found") // another organisation's outlet
		if items := get(sync(b.Org), "/internal/v1/outlets/"+b.Outlet.String()+"/manual-book-series").JSON(t)["items"].([]any); len(items) != 0 {
			t.Fatalf("a book of tenant A is not in tenant B's outlet answer: %v", items)
		}
		evs := eventsOf(t, g, "tenancy.manual_book.registered")
		if len(evs) != 1 || dataOf(t, evs[0])["code"] != "BKG" || dataOf(t, evs[0])["status"] != "active" || evs[0].OutletID != a.Outlet {
			t.Fatalf("one manual_book.registered event with ids and codes: %v", evs)
		}
	})
}
