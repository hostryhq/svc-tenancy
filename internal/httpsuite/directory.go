package httpsuite

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// Directory covers the vendor directory (AC-TEN-7, 8, 16; ADR-0013 AD3): stable paging newest first,
// search by name, id and exact GSTIN, closed schema, and no lifecycle operation.
func Directory(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.Seed(t, seed.Example, seed.Other)
	var all []string
	for i := 0; i < 60; i++ {
		r := g.Call(t, "POST", "/internal/v1/vendor/organisations", g.Vendor(), orgBody(fmt.Sprintf("Directory Org %02d", i), MakeGSTIN("33", 100+i)))
		want(t, r, 201, "")
		all = append(all, parseCreated(t, r).Org.String())
	}
	page := func(cursor string, limit int) (ids []string, next string) {
		q := fmt.Sprintf("?q=Directory+Org&limit=%d", limit)
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		r := g.Call(t, "GET", "/internal/v1/vendor/organisations"+q, g.Vendor(), nil)
		want(t, r, 200, "")
		m := r.JSON(t)
		for _, i := range m["items"].([]any) {
			ids = append(ids, i.(obj)["id"].(string))
		}
		if n, ok := m["next_cursor"].(string); ok {
			next = n
		}
		return
	}

	t.Run("pages are newest first, stable under concurrent inserts, at most 100 per page", func(t *testing.T) {
		seen := map[string]bool{}
		var order []string
		cursor := ""
		for p := 0; ; p++ {
			got, next := page(cursor, 25)
			if len(got) > 25 {
				t.Fatalf("page of %d", len(got))
			}
			for _, id := range got {
				if seen[id] {
					t.Fatalf("%s listed twice", id)
				}
				seen[id] = true
				order = append(order, id)
			}
			if p == 0 { // an insert between pages must not shift the later pages
				want(t, g.Call(t, "POST", "/internal/v1/vendor/organisations", g.Vendor(), orgBody("Directory Org Late", MakeGSTIN("33", 400))), 201, "")
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if len(order) != 60 {
			t.Fatalf("%d organisations listed, want 60 (the late one is newer than page one)", len(order))
		}
		if !sort.SliceIsSorted(order, func(i, j int) bool { return order[i] > order[j] }) {
			t.Fatal("not newest first")
		}
		if ids, _ := page("", 100); len(ids) != 61 {
			t.Fatalf("limit 100 returned %d, want 61", len(ids))
		}
		want(t, g.Call(t, "GET", "/internal/v1/vendor/organisations?limit=101", g.Vendor(), nil), 422, "validation")
		want(t, g.Call(t, "GET", "/internal/v1/vendor/organisations?limit=0", g.Vendor(), nil), 422, "validation")
	})

	t.Run("search by name fragment, id and exact GSTIN; a partial GSTIN matches nothing", func(t *testing.T) {
		if n := len(directory(t, g, "?q=directory+org+07")); n != 1 {
			t.Fatalf("case-insensitive name fragment: %d", n)
		}
		if n := len(directory(t, g, "?q="+all[3])); n != 1 {
			t.Fatalf("by id: %d", n)
		}
		if n := len(directory(t, g, "?q="+MakeGSTIN("33", 105))); n != 1 {
			t.Fatalf("by exact GSTIN: %d", n)
		}
		if n := len(directory(t, g, "?q="+MakeGSTIN("33", 105)[:10])); n != 0 {
			t.Fatalf("a partial GSTIN must not match: %d", n)
		}
		if n := len(directory(t, g, "?q=%25")); n != 0 { // LIKE metacharacters are literal
			t.Fatalf("a percent sign must not act as a wildcard: %d", n)
		}
		if n := len(directory(t, g, "?status=closed")); n != 0 {
			t.Fatalf("status filter: %d", n)
		}
		if n := len(directory(t, g, "?organisation_ids="+all[0]+","+all[1])); n != 2 {
			t.Fatalf("organisation_ids filter: %d", n)
		}
		want(t, g.Call(t, "GET", "/internal/v1/vendor/organisations?organisation_ids=nonsense", g.Vendor(), nil), 422, "validation")
	})

	t.Run("responses hold structure only: no member, device, phone or email data", func(t *testing.T) {
		rows := directory(t, g, "?q=Directory+Org+01")
		keys := func(o obj) string {
			var k []string
			for key := range o {
				k = append(k, key)
			}
			sort.Strings(k)
			return strings.Join(k, ",")
		}
		if keys(rows[0]) != "created_at,id,name,outlet_count,status" {
			t.Fatalf("directory row keys %s", keys(rows[0]))
		}
		d := g.Call(t, "GET", "/internal/v1/vendor/organisations/"+all[1], g.Vendor(), nil)
		want(t, d, 200, "")
		m := d.JSON(t)
		if keys(m) != "brands,organisation,outlets,registrations" {
			t.Fatalf("detail keys %s", keys(m))
		}
		body := strings.ToLower(string(d.Body))
		for _, deny := range []string{"phone", "mobile", "email", "member", "device", "pin", "settings"} {
			if strings.Contains(body, `"`+deny) {
				t.Fatalf("detail carries %q: %s", deny, d.Body)
			}
		}
		want(t, g.Call(t, "GET", "/internal/v1/vendor/organisations/"+ids.New().String(), g.Vendor(), nil), 404, "not_found")
	})

	t.Run("no lifecycle operation exists in A0 (AC-TEN-12)", func(t *testing.T) {
		for _, m := range []string{"PUT", "PATCH", "DELETE", "POST"} {
			r := g.Call(t, m, "/internal/v1/vendor/organisations/"+all[0]+"/status", g.Vendor(), obj{"status": "suspended"})
			if r.Status != 404 && r.Status != 405 {
				t.Fatalf("%s: %d", m, r.Status)
			}
		}
		if got := directory(t, g, "?q="+all[0])[0]["status"]; got != "trial" {
			t.Fatalf("status %v", got)
		}
	})

	t.Run("a directory read leaves no audit row", func(t *testing.T) {
		for _, row := range g.AuditLog(t, ids.ID(all[1])) {
			if row.ActionKey != "vendor.organisation.created" {
				t.Fatalf("directory reads are x-vendor-audit: none, got %+v", row)
			}
		}
	})
}

var actionKeyRe = regexp.MustCompile(`^[a-z]+(\.[a-z_]+)+$`)
var ticketRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,63}$`)

// Audit covers the vendor access log (AC-TEN-17, 18; ADR-0013 AD5, AD6): exactly one row per vendor or
// support action, in the action's own transaction, fail-closed, and published without free text.
func Audit(t *testing.T, mk Factory) {
	g := mk(t, false)
	g.Seed(t, seed.Example)
	a := seed.Example

	t.Run("each audited vendor action leaves exactly one row, and a failed one none", func(t *testing.T) {
		r := g.Call(t, "POST", "/internal/v1/vendor/organisations", g.Vendor(), orgBody("Audited Foods", MakeGSTIN("33", 200)))
		want(t, r, 201, "")
		c := parseCreated(t, r)
		rows := g.AuditLog(t, c.Org)
		if len(rows) != 1 || rows[0].ActionKey != "vendor.organisation.created" || rows[0].Outcome != "ok" || rows[0].TargetID != c.Org.String() {
			t.Fatalf("creation rows: %+v", rows)
		}
		path := "/internal/v1/vendor/organisations/" + c.Org.String() + "/tax-registrations/" + c.Reg.String() + "/verification"
		want(t, g.Call(t, "PUT", path, g.Vendor(), obj{"method": "other"}), 422, "validation") // handler error: no row
		if n := len(g.AuditLog(t, c.Org)); n != 1 {
			t.Fatalf("a failed action must leave no row, rows: %d", n)
		}
		want(t, g.Call(t, "PUT", path, g.Vendor(), obj{"method": "certificate_seen"}), 200, "")
		rows = g.AuditLog(t, c.Org)
		if len(rows) != 2 || rows[1].ActionKey != "vendor.registration.verified" || rows[1].TargetID != c.Reg.String() {
			t.Fatalf("verification rows: %+v", rows)
		}
	})

	t.Run("a support read is recorded; a support write refusal is recorded as refused", func(t *testing.T) {
		before := len(g.AuditLog(t, a.Org))
		sup := g.SupportHeaders(a.Org)
		want(t, g.Call(t, "GET", "/v1/outlets", sup, nil), 200, "")
		want(t, g.Call(t, "PATCH", "/v1/organisations/current", sup, obj{"name": "Hijack"}), 403, "")
		rows := g.AuditLog(t, a.Org)[before:]
		if len(rows) != 2 || rows[0].ActionKey != "support.read.outlets" || rows[0].Outcome != "ok" || rows[0].TokenType != "support" || rows[1].Outcome != "refused" {
			t.Fatalf("support rows: %+v", rows)
		}
	})

	t.Run("fail-closed: with the log unwritable, vendor actions fail and change nothing", func(t *testing.T) {
		before := len(directory(t, g, ""))
		g.SetAuditWritable(t, false)
		defer g.SetAuditWritable(t, true)
		r := g.Call(t, "POST", "/internal/v1/vendor/organisations", g.Vendor(), orgBody("Unaudited Foods", MakeGSTIN("33", 201)))
		if r.Status != 500 {
			t.Fatalf("status %d, want 500: %s", r.Status, r.Body)
		}
		if got := g.Call(t, "GET", "/v1/outlets", g.SupportHeaders(a.Org), nil); got.Status != 500 {
			t.Fatalf("a support read with an unwritable log must fail, got %d", got.Status)
		}
		g.SetAuditWritable(t, true)
		if after := len(directory(t, g, "")); after != before {
			t.Fatalf("an unaudited creation left an organisation: %d -> %d", before, after)
		}
	})

	t.Run("each row is published as tenancy.vendor_access.recorded with ids and fixed-pattern fields only (AD6)", func(t *testing.T) {
		var n int
		for _, e := range g.Events(t) {
			if e.Type != "tenancy.vendor_access.recorded" {
				continue
			}
			n++
			var d map[string]any
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			allowed := map[string]bool{"entry_id": true, "vendor_user_id": true, "token_type": true, "support_session_id": true, "action_key": true,
				"target_id": true, "ticket_reference": true, "outcome": true, "result_count": true, "request_id": true}
			for k := range d {
				if !allowed[k] {
					t.Fatalf("event carries field %q: %s", k, e.Data)
				}
			}
			if !actionKeyRe.MatchString(d["action_key"].(string)) {
				t.Fatalf("action key %q", d["action_key"])
			}
			if tk, ok := d["ticket_reference"].(string); ok && !ticketRe.MatchString(tk) {
				t.Fatalf("ticket %q", tk)
			}
			if e.Key != d["entry_id"] || e.TenantID == "" || e.Source != "tenancy" {
				t.Fatalf("envelope %+v", e)
			}
			for _, secret := range []string{"Audited Foods", "33ABCDE", "Pvt Ltd"} {
				if strings.Contains(string(e.Data), secret) {
					t.Fatalf("event carries %q", secret)
				}
			}
		}
		if n < 3 {
			t.Fatalf("only %d vendor_access.recorded events", n)
		}
	})

	t.Run("no event of any type carries a GSTIN (A1.1 item 8)", func(t *testing.T) {
		for _, e := range g.Events(t) {
			if strings.Contains(string(e.Data), "33ABCDE") || strings.Contains(string(e.Data), seed.ExampleGSTIN) {
				t.Fatalf("%s carries a GSTIN: %s", e.Type, e.Data)
			}
		}
	})
}

var _ = http.StatusOK
var _ = itest.Bearer
