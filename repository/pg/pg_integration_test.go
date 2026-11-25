//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hostryhq/go-auth/scopetest"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/httpsuite"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/pgtest"
	"github.com/hostryhq/svc-tenancy/internal/scopecheck"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository/pg"
)

var (
	tenantA, tenantB, tenantC = seed.Example.Org, seed.Other.Org, seed.Third.Org
	ctx                       = context.Background()
)

type obj = map[string]any

// stack starts the real service with three seeded organisations and some activity, so that every table
// holds rows of tenants A and B.
func stack(t *testing.T) *itest.Stack {
	t.Helper()
	s := itest.New(t)
	s.Seed(t, seed.Example, seed.Other, seed.Third)
	for _, w := range []seed.World{seed.Example, seed.Other} {
		own := itest.Bearer(s.TenantToken(w.Org))
		if r := itest.Do(t, s.Srv.URL, "PUT", "/v1/organisation-settings", own, obj{"offline_window_days": 10, "idle_lock_minutes": 8}); r.Status != 200 {
			t.Fatalf("settings: %d %s", r.Status, r.Body)
		}
		if r := itest.Do(t, s.Srv.URL, "GET", "/v1/outlets", s.SupportHeaders(w.Org), nil); r.Status != 200 {
			t.Fatalf("support read: %d %s", r.Status, r.Body)
		}
		itest.Do(t, s.Srv.URL, "POST", "/v1/links", own, obj{"id": ids.New().String(), "template": "supplier_buyer", "grants": []obj{{"scope": "test.echo.read", "direction": "outbound"}}})
	}
	return s
}

// inTx runs fn in a transaction on pool with the tenant context set (tenant "" means no context).
func inTx(t *testing.T, db *sql.DB, tenant ids.ID, fn func(tx *sql.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if tenant != "" {
		if err := tenancy.SetDBContext(ctx, tx, tenant); err != nil {
			t.Fatal(err)
		}
	}
	fn(tx)
}

func count(t *testing.T, tx *sql.Tx, q string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func tablesWithColumn(t *testing.T, db *sql.DB, col string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name = $1 ORDER BY table_name`, col)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

var tenantTables = []string{"document_series", "event_keys", "idempotency_records", "organisation_settings", "organisations", "outbox", "outlets",
	"series_allocators", "tax_registrations", "vendor_access_log", "brands"}

// T1 / TST-01: for every tenant-owned table, as the application role: no context sees nothing; context A
// never sees B's rows; writing a row of B or moving a row to B is refused.
func TestDatabaseIsolationOnEveryTenantTable(t *testing.T) {
	s := stack(t)
	got := tablesWithColumn(t, s.DB.Admin, "tenant_id")
	want := append([]string(nil), tenantTables...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tenant-owned tables changed: %v\nupdate this test and the schema lint together", got)
	}
	for _, tbl := range got {
		t.Run(tbl, func(t *testing.T) {
			inTx(t, s.DB.App, "", func(tx *sql.Tx) {
				if n := count(t, tx, `SELECT count(*) FROM `+tbl); n != 0 {
					t.Fatalf("no context sees %d rows", n)
				}
			})
			inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
				mine := count(t, tx, `SELECT count(*) FROM `+tbl)
				if mine == 0 {
					t.Fatalf("tenant A has no rows in %s: the fixture must cover every table", tbl)
				}
				if n := count(t, tx, `SELECT count(*) FROM `+tbl+` WHERE tenant_id::text = $1`, tenantB.String()); n != 0 {
					t.Fatalf("context A sees %d of B's rows", n)
				}
				if n := count(t, tx, `SELECT count(*) FROM `+tbl+` WHERE tenant_id::text = $1`, tenantA.String()); n != mine {
					t.Fatalf("context A: %d rows, %d of them A's", mine, n)
				}
			})
			// Writing as B while the context is A: copy one of A's rows with tenant_id = B (WITH CHECK refuses it).
			inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
				_, err := tx.ExecContext(ctx, `INSERT INTO `+tbl+` SELECT (jsonb_populate_record(NULL::`+tbl+`, to_jsonb(r) || jsonb_build_object('tenant_id', $1::text))).* FROM `+tbl+` r LIMIT 1`, tenantB.String())
				if err == nil {
					t.Fatal("inserting a row of tenant B under context A must be refused")
				}
			})
			inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(ctx, `UPDATE `+tbl+` SET tenant_id = $1::text::`+colType(tbl)+` WHERE tenant_id::text = $2`, tenantB.String(), tenantA.String()); err == nil {
					t.Fatal("moving a row to tenant B must be refused")
				}
			})
			inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE tenant_id::text = $1`, tenantA.String()); err == nil {
					t.Fatal("the application role has no DELETE on tenant tables")
				}
			})
		})
	}
}

func colType(tbl string) string {
	if tbl == "outbox" {
		return "text"
	}
	return "uuid"
}

// T3 / TST-03: a child row in tenant A that names tenant B's parent fails (composite foreign key).
func TestCrossTenantReferenceFails(t *testing.T) {
	s := stack(t)
	inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
		_, err := tx.ExecContext(ctx, `INSERT INTO outlets (tenant_id, outlet_id, brand_id, name, kind, state_code, tax_registration_id, created_at)
			VALUES ($1, $2, $3, 'X', 'restaurant', '29', $4, now())`, tenantA, ids.New(), seed.Example.DefaultBrand, seed.Other.Registration)
		if err == nil || !strings.Contains(err.Error(), "foreign key") {
			t.Fatalf("an outlet of A under B's registration must violate the composite FK, got %v", err)
		}
	})
	inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
		_, err := tx.ExecContext(ctx, `INSERT INTO outlets (tenant_id, outlet_id, brand_id, name, kind, state_code, tax_registration_id, created_at)
			VALUES ($1, $2, $3, 'X', 'restaurant', '33', $4, now())`, tenantA, ids.New(), seed.Other.DefaultBrand, seed.Example.Registration)
		if err == nil {
			t.Fatal("an outlet of A under B's brand must fail")
		}
	})
}

// T4 / TST-04: a transaction under A, then one with no context on the same connection, sees nothing; and a
// session-level setting made by a stray call is cleared when the connection goes back to the pool.
func TestPoolHygiene(t *testing.T) {
	s := stack(t)
	one, err := pg.Open(s.DB.DSN(pgtest.App), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = one.Close() }()
	inTx(t, one, tenantA, func(tx *sql.Tx) {
		if count(t, tx, `SELECT count(*) FROM organisations`) != 1 {
			t.Fatal("context A sees its organisation")
		}
	})
	inTx(t, one, "", func(tx *sql.Tx) {
		if n := count(t, tx, `SELECT count(*) FROM organisations`); n != 0 {
			t.Fatalf("the next transaction on the same connection sees %d rows", n)
		}
	})
	conn, err := one.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantA.String()); err != nil { // the test-only bad call
		t.Fatal(err)
	}
	_ = conn.Close() // back to the pool
	inTx(t, one, "", func(tx *sql.Tx) {
		var v sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT NULLIF(current_setting('app.tenant_id', true), '')`).Scan(&v); err != nil || v.Valid {
			t.Fatalf("a session-level tenant setting survived the pool: %v %v", v, err)
		}
		if n := count(t, tx, `SELECT count(*) FROM organisations`); n != 0 {
			t.Fatalf("leaked context shows %d rows", n)
		}
	})
}

// T5 / TST-05: schema and role lint. Every table with a tenant column has forced RLS with the standard
// policy; every other table is on the reviewed allowlist with its class; runtime roles are not owners,
// superusers or BYPASSRLS; the vendor and relay roles hold exactly their grants.
func TestSchemaAndRoleLint(t *testing.T) {
	s := stack(t)
	db := s.DB.Admin
	allowlist := map[string]string{
		// two-party class (visible to either party; written by the service in the acting party's context)
		"links": "two-party", "link_grants": "two-party", "link_amendments": "two-party", "link_activity": "two-party", "link_invitations": "two-party",
		// vendor realm (directory tables; two policies)
		"directory_organisations": "vendor-realm", "directory_registrations": "vendor-realm",
		// platform class: vendor actions that concern no organisation (ADR-0013 V5 item 3). No tenant column and
		// no row-level security by design; checked below for append-only grants and for holding no tenant data.
		"platform_vendor_access_log": "platform",
		// migrator-only bookkeeping
		"schema_migrations": "migrator-only",
	}
	rows, err := db.QueryContext(ctx, `SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		EXISTS (SELECT 1 FROM information_schema.columns k WHERE k.table_schema = 'public' AND k.table_name = c.relname AND k.column_name = 'tenant_id')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	type tbl struct {
		name          string
		rls, force, t bool
	}
	var all []tbl
	for rows.Next() {
		var x tbl
		if err := rows.Scan(&x.name, &x.rls, &x.force, &x.t); err != nil {
			t.Fatal(err)
		}
		all = append(all, x)
	}
	_ = rows.Close()
	for _, x := range all {
		class, listed := allowlist[x.name]
		switch {
		case x.t:
			if listed {
				t.Errorf("%s has tenant_id but is on the allowlist", x.name)
			}
			if !x.rls || !x.force {
				t.Errorf("%s: row-level security must be enabled and forced", x.name)
			}
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_policies WHERE tablename = $1 AND policyname = 'tenant_isolation'
				AND qual LIKE '%app.tenant_id%' AND with_check LIKE '%app.tenant_id%'`, x.name).Scan(&n); err != nil || n != 1 {
				t.Errorf("%s: the standard policy is missing (%d, %v)", x.name, n, err)
			}
		case !listed:
			t.Errorf("table %s has no tenant_id and is not on the reviewed allowlist", x.name)
		case class != "migrator-only" && class != "platform" && (!x.rls || !x.force):
			t.Errorf("%s (%s): row-level security must be enabled and forced", x.name, class)
		}
	}
	// The directory tables carry exactly their two policies.
	for _, d := range []string{"directory_organisations", "directory_registrations"} {
		var names string
		if err := db.QueryRowContext(ctx, `SELECT string_agg(policyname || ':' || roles::text, ',' ORDER BY policyname) FROM pg_policies WHERE tablename = $1`, d).Scan(&names); err != nil {
			t.Fatal(err)
		}
		if names != "app_own:{tenancy_app},vendor_read:{tenancy_vendor}" {
			t.Errorf("%s policies: %s", d, names)
		}
	}
	// Tenant-owned tables keep exactly one policy (the outbox adds the relay role's).
	var multi string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(string_agg(tablename, ','), '') FROM (SELECT tablename FROM pg_policies WHERE tablename = ANY($1) AND tablename NOT IN ('outbox', 'idempotency_records') GROUP BY tablename HAVING count(*) <> 1) x`, pqArray(tenantTables)).Scan(&multi); err != nil || multi != "" {
		t.Errorf("tenant-owned tables with other than one policy: %q %v", multi, err)
	}
	// Roles.
	for _, role := range []string{pgtest.App, pgtest.Vendor, pgtest.Relay} {
		var super, bypass bool
		var owned int
		if err := db.QueryRowContext(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, role).Scan(&super, &bypass); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = $1`, role).Scan(&owned); err != nil {
			t.Fatal(err)
		}
		if super || bypass || owned != 0 {
			t.Errorf("role %s: superuser=%v bypassrls=%v owns %d tables", role, super, bypass, owned)
		}
	}
	grants := func(role string) string {
		rs, err := db.QueryContext(ctx, `SELECT table_name || ':' || privilege_type FROM information_schema.role_table_grants WHERE grantee = $1 ORDER BY 1`, role)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rs.Close() }()
		var out []string
		for rs.Next() {
			var g string
			_ = rs.Scan(&g)
			out = append(out, g)
		}
		return strings.Join(out, ",")
	}
	if got := grants(pgtest.Vendor); got != "directory_organisations:SELECT,directory_registrations:SELECT" {
		t.Errorf("tenancy_vendor grants: %s", got)
	}
	if got := grants(pgtest.Relay); got != "idempotency_records:DELETE,idempotency_records:SELECT,outbox:DELETE,outbox:SELECT,outbox:UPDATE" {
		t.Errorf("tenancy_relay grants: %s", got)
	}
	if g := grants(pgtest.App); strings.Contains(g, "DELETE") || strings.Contains(g, "TRUNCATE") {
		t.Errorf("tenancy_app must have no DELETE: %s", g)
	}
	for _, appendOnly := range []string{"link_activity", "vendor_access_log", "outbox", "platform_vendor_access_log"} {
		for _, p := range []string{"UPDATE", "DELETE"} {
			if strings.Contains(grants(pgtest.App), appendOnly+":"+p) {
				t.Errorf("tenancy_app can %s %s, which is append-only for it", p, appendOnly)
			}
		}
	}
}

func pqArray(s []string) string { return "{" + strings.Join(s, ",") + "}" }

// TC-01 / TST-07: the process refuses to start with a superuser, a BYPASSRLS role or the migrator.
func TestStartUpRefusesUnsafeRoles(t *testing.T) {
	s := stack(t)
	for name, db := range map[string]*sql.DB{"superuser": s.DB.Admin, "migrator (table owner)": s.DB.Migrator} {
		if err := pg.VerifyRuntimeRole(ctx, db); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if _, err := s.DB.Admin.ExecContext(ctx, `CREATE ROLE tenancy_bypass LOGIN BYPASSRLS PASSWORD 'tenancy-local-test-only'`); err == nil {
		defer func() { _, _ = s.DB.Admin.ExecContext(ctx, `DROP ROLE tenancy_bypass`) }()
		db, _ := pg.Open(s.DB.DSN("tenancy_bypass"), 1)
		defer func() { _ = db.Close() }()
		if err := pg.VerifyRuntimeRole(ctx, db); err == nil {
			t.Error("a BYPASSRLS role must be refused")
		}
	}
	for _, db := range []*sql.DB{s.DB.App, s.DB.Vendor, s.DB.Relay} {
		if err := pg.VerifyRuntimeRole(ctx, db); err != nil {
			t.Errorf("a runtime role was refused: %v", err)
		}
	}
}

// Two-party and vendor-realm classes: a third organisation sees nothing of other parties' links, and
// the invitation is a capability.
func TestTwoPartyAndDirectoryPolicies(t *testing.T) {
	s := stack(t)
	for _, tbl := range []string{"links", "link_grants", "link_activity", "link_invitations"} {
		inTx(t, s.DB.App, tenantC, func(tx *sql.Tx) {
			if n := count(t, tx, `SELECT count(*) FROM `+tbl); n != 0 {
				t.Errorf("tenant C sees %d rows of %s", n, tbl)
			}
		})
		inTx(t, s.DB.App, "", func(tx *sql.Tx) {
			if n := count(t, tx, `SELECT count(*) FROM `+tbl); n != 0 {
				t.Errorf("no context sees %d rows of %s", n, tbl)
			}
		})
		inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
			if count(t, tx, `SELECT count(*) FROM `+tbl) == 0 {
				t.Errorf("tenant A sees none of its %s", tbl)
			}
		})
	}
	inTx(t, s.DB.App, tenantC, func(tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO links (id, party_a_tenant_id, template, status, proposer_party, proposed_at, expires_at)
			VALUES ($1, $2, 'supplier_buyer', 'proposed', '{}', now(), now())`, ids.New(), tenantA); err == nil {
			t.Error("C must not create a link as party A")
		}
	})
	// The invitation is visible to whoever presents its code: a digest set for the transaction.
	var hash string
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT encode(code_hash, 'hex') FROM link_invitations LIMIT 1`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	inTx(t, s.DB.App, tenantC, func(tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.invite_hash', $1, true)`, hash); err != nil {
			t.Fatal(err)
		}
		if n := count(t, tx, `SELECT count(*) FROM link_invitations`); n != 1 {
			t.Errorf("the holder of a code sees its invitation (and nothing else): %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM links`); n != 1 {
			t.Errorf("the holder of a code sees that one link: %d", n)
		}
	})

	// Directory (ADR-0013 V3, AD3).
	inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
		if n := count(t, tx, `SELECT count(*) FROM directory_organisations`); n != 1 {
			t.Errorf("the application role under A sees %d directory rows, want its own 1", n)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE directory_organisations SET name = 'x' WHERE subject_tenant_id = $1`, tenantB); err != nil {
			t.Fatal(err)
		} else if n := count(t, tx, `SELECT count(*) FROM directory_organisations WHERE name = 'x'`); n != 0 {
			t.Error("A changed B's directory row")
		}
	})
	inTx(t, s.DB.Vendor, "", func(tx *sql.Tx) {
		if n := count(t, tx, `SELECT count(*) FROM directory_organisations`); n != 3 {
			t.Errorf("tenancy_vendor reads all 3 directory rows, got %d", n)
		}
	})
	for _, q := range []string{`SELECT count(*) FROM organisations`, `SELECT count(*) FROM tax_registrations`, `SELECT count(*) FROM outbox`,
		`SELECT count(*) FROM vendor_access_log`, `SELECT count(*) FROM links`, `SELECT count(*) FROM idempotency_records`} {
		inTx(t, s.DB.Vendor, "", func(tx *sql.Tx) {
			if _, err := tx.ExecContext(ctx, q); err == nil {
				t.Errorf("tenancy_vendor can run %q", q)
			}
		})
	}
	inTx(t, s.DB.Vendor, "", func(tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `UPDATE directory_organisations SET name = 'x'`); err == nil {
			t.Error("tenancy_vendor can write the directory")
		}
	})
}

// TST-23, AD5: link_activity and vendor_access_log are append-only for the application role.
func TestAppendOnlyTables(t *testing.T) {
	s := stack(t)
	for _, tbl := range []string{"link_activity", "vendor_access_log", "outbox", "platform_vendor_access_log"} {
		for _, stmt := range []string{`UPDATE ` + tbl + ` SET id = id`, `DELETE FROM ` + tbl} {
			inTx(t, s.DB.App, tenantA, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(ctx, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
					t.Errorf("%q: %v", stmt, err)
				}
			})
		}
	}
}

// AD3: a directory row matches its organisation after create, rename, outlet and registration change.
func TestDirectoryMatchesOrganisations(t *testing.T) {
	s := stack(t)
	vendor := s.Vendor()
	r := itest.Do(t, s.Srv.URL, "POST", "/internal/v1/vendor/organisations", vendor, obj{"name": "Dir Check", "outlet": obj{"name": "M", "kind": "restaurant", "state_code": "33"},
		"tax_registration": obj{"legal_name": "Dir Check Pvt Ltd", "regime": "regular", "gstin": httpsuite.MakeGSTIN("33", 900)}})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	org := ids.ID(r.JSON(t)["organisation"].(obj)["id"].(string))
	reg := r.JSON(t)["tax_registration"].(obj)["id"].(string)
	own := itest.Bearer(s.TenantToken(org))
	itest.Do(t, s.Srv.URL, "PATCH", "/v1/organisations/current", own, obj{"name": "Dir Check Renamed"})
	itest.Do(t, s.Srv.URL, "POST", "/v1/outlets", own, obj{"id": ids.New().String(), "name": "Second", "kind": "stall", "state_code": "33", "tax_registration_id": reg})
	itest.Do(t, s.Srv.URL, "PATCH", "/v1/tax-registrations/"+reg, own, obj{"legal_name": "Renamed Pvt Ltd"})
	itest.Do(t, s.Srv.URL, "PUT", "/internal/v1/vendor/organisations/"+org.String()+"/tax-registrations/"+reg+"/verification", vendor, obj{"method": "certificate_seen"})
	var bad int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT o.tenant_id FROM organisations o JOIN directory_organisations d ON d.subject_tenant_id = o.tenant_id
		 WHERE d.name <> o.name OR d.status <> o.status OR d.created_at <> o.created_at
		    OR d.outlet_count <> (SELECT count(*) FROM outlets x WHERE x.tenant_id = o.tenant_id)
		UNION ALL
		SELECT r.tenant_id FROM tax_registrations r LEFT JOIN directory_registrations d ON d.subject_tenant_id = r.tenant_id AND d.registration_id = r.id
		 WHERE d.registration_id IS NULL OR d.legal_name <> r.legal_name OR d.verification_state <> r.verification_state OR d.status <> r.status
		    OR d.gstin_normalised IS DISTINCT FROM r.gstin_normalised
		UNION ALL
		SELECT d.subject_tenant_id FROM directory_organisations d LEFT JOIN organisations o ON o.tenant_id = d.subject_tenant_id WHERE o.tenant_id IS NULL
	) x`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("%d directory rows disagree with their organisations (%v)", bad, err)
	}
	var name string
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT name FROM directory_organisations WHERE subject_tenant_id = $1`, org).Scan(&name); err != nil || name != "Dir Check Renamed" {
		t.Fatalf("directory name %q %v", name, err)
	}
}

// T13 / TST-17: concurrent activations never share a code; the 201st code of an IST day is refused;
// a refused code (the (GSTIN, code) index) makes the allocator jump 200 and retry.
func TestRegistryConcurrencyCapAndCollisionJump(t *testing.T) {
	s := stack(t)
	w := seed.Other
	issue := func(outlet ids.ID) itest.Response {
		return itest.Do(t, s.Srv.URL, "POST", "/internal/v1/document-series-issuances", s.ServiceHeaders("identity", w.Org),
			obj{"outlet_id": outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[string]int{}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outlet := w.Outlet
			if i%2 == 1 {
				outlet = w.OutletY // two outlets on one registration
			}
			r := issue(outlet)
			if r.Status != 201 {
				t.Errorf("activation %d: %d %s", i, r.Status, r.Body)
				return
			}
			mu.Lock()
			codes[r.JSON(t)["items"].([]any)[0].(obj)["code"].(string)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(codes) != 50 {
		t.Fatalf("50 parallel activations produced %d distinct codes", len(codes))
	}
	// 51 codes exist (the seeded one plus 50): 149 more fill the day's 200, the next is refused.
	for i := 0; i < 149; i++ {
		if r := issue(w.Outlet); r.Status != 201 {
			t.Fatalf("code %d: %d %s", 51+i, r.Status, r.Body)
		}
	}
	if r := issue(w.Outlet); r.Status != 429 || r.Code() != "series_cap_reached" {
		t.Fatalf("the 201st code of the day: %d %s", r.Status, r.Body)
	}
	var n int
	_ = s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM document_series WHERE tenant_id = $1`, w.Org).Scan(&n)
	if n != 200 {
		t.Fatalf("%d series rows", n)
	}
	// Collision backstop, on tenant A: plant the next code under the same GSTIN through another tenant's row.
	a := seed.Example
	if _, err := s.DB.Admin.ExecContext(ctx, `INSERT INTO document_series (tenant_id, id, tax_registration_id, gstin_normalised, code, code_index, doc_type, outlet_id, device_id, status, issued_at)
		VALUES ($1, $2, $3, $4, $5, 1, 'plain_bill', $6, $7, 'closed', now())`, seed.Third.Org, ids.New(), seed.Third.Registration, a.GSTIN, model.CodeFor(1), seed.Third.Outlet, ids.New()); err != nil {
		t.Fatal(err)
	}
	r := itest.Do(t, s.Srv.URL, "POST", "/internal/v1/document-series-issuances", s.ServiceHeaders("identity", a.Org),
		obj{"outlet_id": a.Outlet.String(), "device_id": ids.New().String(), "doc_types": []string{"tax_invoice"}, "state": "active"})
	if r.Status != 201 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if got := r.JSON(t)["items"].([]any)[0].(obj)["code"]; got != model.CodeFor(201) {
		t.Fatalf("after a refused code the allocator jumps 200: issued %v, want %s", got, model.CodeFor(201))
	}
}

// TST-11: injected SQL in every string field is data; tenant B's rows stay invisible and nothing 500s.
func TestInjectionInStringFields(t *testing.T) {
	s := stack(t)
	payload := fmt.Sprintf("'); SELECT set_config('app.tenant_id', '%s', true); --", tenantB)
	own := itest.Bearer(s.TenantToken(tenantA))
	brand := ids.New().String()
	steps := []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/v1/organisations/current", obj{"name": payload}},
		{"POST", "/v1/brands", obj{"id": brand, "name": payload}},
		{"PATCH", "/v1/outlets/" + seed.Example.Outlet.String(), obj{"name": payload}},
		{"POST", "/v1/tax-registrations", obj{"id": ids.New().String(), "legal_name": payload, "regime": "unregistered", "state_code": "33"}},
		{"POST", "/v1/tax-registrations", obj{"id": ids.New().String(), "legal_name": "x", "regime": "regular", "gstin": payload, "state_code": "33"}},
		{"PUT", "/v1/document-series/" + seed.Example.Series.String() + "/closure", obj{"reason": payload}},
		{"POST", "/v1/link-acceptances", obj{"invitation_code": payload}},
		{"GET", "/v1/outlets?kind=" + strings.ReplaceAll(payload, " ", "%20"), nil},
	}
	for _, st := range steps {
		if r := itest.Do(t, s.Srv.URL, st.method, st.path, own, st.body); r.Status >= 500 {
			t.Fatalf("%s %s: %d %s", st.method, st.path, r.Status, r.Body)
		}
	}
	vendor := itest.Do(t, s.Srv.URL, "POST", "/internal/v1/vendor/organisations", s.Vendor(), obj{"name": payload, "outlet": obj{"name": payload, "kind": "restaurant", "state_code": "33"},
		"tax_registration": obj{"legal_name": payload, "regime": "unregistered"}})
	if vendor.Status != 201 {
		t.Fatalf("%d %s", vendor.Status, vendor.Body)
	}
	if got := itest.Do(t, s.Srv.URL, "GET", "/v1/brands/"+brand, own, nil).JSON(t)["name"]; got != payload {
		t.Fatalf("the payload must be stored as plain data: %v", got)
	}
	for _, path := range []string{"/v1/brands", "/v1/outlets", "/v1/tax-registrations", "/v1/document-series"} {
		for _, id := range []string{seed.Other.Brand.String(), seed.Other.Outlet.String(), seed.Other.Registration.String(), seed.Other.Series.String()} {
			if strings.Contains(string(itest.Do(t, s.Srv.URL, "GET", path, own, nil).Body), id) {
				t.Fatalf("%s shows tenant B's %s", path, id)
			}
		}
	}
	if got := itest.Do(t, s.Srv.URL, "GET", "/v1/organisations/current", own, nil).JSON(t); got["id"] != tenantA.String() {
		t.Fatalf("the tenant context changed: %v", got)
	}
}

// Idempotent replay is exact on the real database, and concurrent requests with one key run the effect once.
func TestConcurrentRetriesWithOneKeyRunOnce(t *testing.T) {
	s := stack(t)
	own := itest.Bearer(s.TenantToken(tenantA))
	own.Set("Idempotency-Key", "retry-storm")
	id := ids.New().String()
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = itest.Do(t, s.Srv.URL, "POST", "/v1/brands", own, obj{"id": id, "name": "Once"}).Status
		}()
	}
	wg.Wait()
	for _, st := range statuses {
		if st != 201 {
			t.Fatalf("statuses %v", statuses)
		}
	}
	var n int
	_ = s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM brands WHERE tenant_id = $1 AND name = 'Once'`, tenantA).Scan(&n)
	if n != 1 {
		t.Fatalf("%d brands created by 10 retries", n)
	}
}

// T14 (ADR-0003 A1.4) on Postgres: the same generated test as the unit run, through the real repository.
func TestOutletScopingT14OnPostgres(t *testing.T) {
	s := itest.New(t)
	s.Seed(t, scopecheck.World)
	cat, err := gen.Catalogue()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range scopecheck.Resources(s.App.Service) {
		t.Run(r.Name, func(t *testing.T) {
			if bad := scopetest.Check(ctx, cat, r); len(bad) > 0 {
				t.Fatalf("T14 failed:\n%s", strings.Join(bad, "\n"))
			}
		})
	}
}
