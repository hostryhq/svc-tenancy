//go:build integration

package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/httpsuite"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// pgFactory starts the real service on a fresh Postgres database as a suite target: the same suites that
// run against the in-memory double (double/suite_test.go) run here against the roles and policies of the
// real schema.
func pgFactory(t *testing.T, links bool) *httpsuite.Target {
	t.Helper()
	s := itest.NewWith(t, links)
	exec := func(t testing.TB, q string, args ...any) {
		t.Helper()
		if _, err := s.DB.Admin.ExecContext(context.Background(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	return &httpsuite.Target{
		Base: s.Srv.URL, Tokens: s.Tokens, LinksEnabled: links,
		Seed: func(t testing.TB, worlds ...seed.World) {
			for _, w := range worlds {
				if err := w.Apply(context.Background(), s.App.Repo); err != nil {
					t.Fatal(err)
				}
			}
		},
		Deliver: func(t testing.TB, e events.Envelope) {
			if err := s.App.Service.HandleEvent(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		},
		SetNow: s.Clock.Set,
		Now:    time.Now,
		Events: func(t testing.TB) []events.Envelope {
			rows, err := s.DB.Admin.QueryContext(context.Background(), `SELECT payload FROM outbox ORDER BY seq`)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var out []events.Envelope
			for rows.Next() {
				var b []byte
				if err := rows.Scan(&b); err != nil {
					t.Fatal(err)
				}
				e, err := events.Unmarshal(b)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, e)
			}
			return out
		},
		AuditLog: func(t testing.TB, tenant ids.ID) []httpsuite.AuditRow {
			rows, err := s.DB.Admin.QueryContext(context.Background(), `SELECT action_key, outcome, COALESCE(target_id, ''), token_type FROM vendor_access_log WHERE tenant_id = $1 ORDER BY id`, tenant)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var out []httpsuite.AuditRow
			for rows.Next() {
				var r httpsuite.AuditRow
				if err := rows.Scan(&r.ActionKey, &r.Outcome, &r.TargetID, &r.TokenType); err != nil {
					t.Fatal(err)
				}
				out = append(out, r)
			}
			return out
		},
		PlatformAuditLog: func(t testing.TB) []httpsuite.AuditRow {
			rows, err := s.DB.Admin.QueryContext(context.Background(), `SELECT action_key, outcome, COALESCE(target_id, ''), token_type FROM platform_vendor_access_log ORDER BY occurred_at, id`)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var out []httpsuite.AuditRow
			for rows.Next() {
				var r httpsuite.AuditRow
				if err := rows.Scan(&r.ActionKey, &r.Outcome, &r.TargetID, &r.TokenType); err != nil {
					t.Fatal(err)
				}
				out = append(out, r)
			}
			return out
		},
		SetAuditWritable: func(t testing.TB, ok bool) {
			for _, tbl := range []string{"vendor_access_log", "platform_vendor_access_log"} {
				if ok {
					exec(t, `GRANT INSERT ON `+tbl+` TO tenancy_app`)
				} else {
					exec(t, `REVOKE INSERT ON `+tbl+` FROM tenancy_app`)
				}
			}
		},
		CloseRegistration: func(t testing.TB, tenant, reg ids.ID) {
			exec(t, fmt.Sprintf(`UPDATE tax_registrations SET status = 'closed' WHERE tenant_id = '%s' AND id = '%s'`, tenant, reg))
			exec(t, fmt.Sprintf(`UPDATE directory_registrations SET status = 'closed' WHERE subject_tenant_id = '%s' AND registration_id = '%s'`, tenant, reg))
		},
	}
}

func TestPostgresIsolation(t *testing.T)         { httpsuite.Isolation(t, pgFactory) }
func TestPostgresOutletScope(t *testing.T)       { httpsuite.OutletScope(t, pgFactory) }
func TestPostgresRealms(t *testing.T)            { httpsuite.Realms(t, pgFactory) }
func TestPostgresLinksOff(t *testing.T)          { httpsuite.LinksOff(t, pgFactory) }
func TestPostgresVendor(t *testing.T)            { httpsuite.Vendor(t, pgFactory) }
func TestPostgresDirectory(t *testing.T)         { httpsuite.Directory(t, pgFactory) }
func TestPostgresAudit(t *testing.T)             { httpsuite.Audit(t, pgFactory) }
func TestPostgresLinks(t *testing.T)             { httpsuite.Links(t, pgFactory) }
func TestPostgresRegistrations(t *testing.T)     { httpsuite.Registrations(t, pgFactory) }
func TestPostgresCommercial(t *testing.T)        { httpsuite.Commercial(t, pgFactory) }
func TestPostgresLifecycle(t *testing.T)         { httpsuite.Lifecycle(t, pgFactory) }
func TestPostgresSeriesTail(t *testing.T)        { httpsuite.SeriesTail(t, pgFactory) }
func TestPostgresBooks(t *testing.T)             { httpsuite.Books(t, pgFactory) }
func TestPostgresGateway(t *testing.T)           { httpsuite.Gateway(t, pgFactory) }
func TestPostgresLinksP2(t *testing.T)           { httpsuite.LinksP2(t, pgFactory) }
func TestPostgresPhaseTwoIsolation(t *testing.T) { httpsuite.PhaseTwoIsolation(t, pgFactory) }
