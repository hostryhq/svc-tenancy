package double_test

import (
	"context"
	"testing"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/httpsuite"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository"
)

// factory starts a double as a suite target.
func factory(t *testing.T, links bool) *httpsuite.Target {
	t.Helper()
	env, err := double.NewEnvLinks(links)
	if err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	t.Cleanup(srv.Close)
	return &httpsuite.Target{
		Base: srv.URL, Tokens: env.Tokens, LinksEnabled: links,
		Seed: func(t testing.TB, worlds ...seed.World) {
			for _, w := range worlds {
				if err := w.Apply(context.Background(), env.Store); err != nil {
					t.Fatal(err)
				}
			}
		},
		Events: func(testing.TB) []events.Envelope { return env.Events() },
		Deliver: func(t testing.TB, e events.Envelope) {
			if err := env.Deliver(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		},
		SetNow: env.SetNow,
		Now:    env.Now,
		AuditLog: func(_ testing.TB, tenant ids.ID) []httpsuite.AuditRow {
			var out []httpsuite.AuditRow
			for _, e := range env.Store.VendorLog() {
				if e.TenantID == tenant.String() {
					tt := "vendor"
					if e.TokenType == "support" {
						tt = "support"
					}
					out = append(out, httpsuite.AuditRow{ActionKey: e.ActionKey, Outcome: e.Outcome, TargetID: e.TargetID, TokenType: tt})
				}
			}
			return out
		},
		PlatformAuditLog: func(testing.TB) []httpsuite.AuditRow {
			var out []httpsuite.AuditRow
			for _, r := range env.Store.PlatformLog() {
				tt := "vendor"
				if r.TokenType == "support" {
					tt = "support"
				}
				out = append(out, httpsuite.AuditRow{ActionKey: r.ActionKey, Outcome: r.Outcome, TargetID: r.TargetID, TokenType: tt})
			}
			return out
		},
		SetAuditWritable: func(_ testing.TB, ok bool) { env.Store.FailAuditWrites(!ok) },
		CloseRegistration: func(t testing.TB, tenant, reg ids.ID) {
			ctx := tenancy.With(context.Background(), tenancy.Context{TenantID: tenant})
			err := env.Store.InTx(ctx, func(c context.Context, tx repository.Tx) error {
				r, err := tx.GetRegistration(c, reg)
				if err != nil {
					return err
				}
				r.Status = model.RegClosed
				return tx.UpdateRegistration(c, r)
			})
			if err != nil {
				t.Fatal(err)
			}
		},
	}
}

func TestDoubleIsolation(t *testing.T)         { httpsuite.Isolation(t, factory) }
func TestDoubleOutletScope(t *testing.T)       { httpsuite.OutletScope(t, factory) }
func TestDoubleRealms(t *testing.T)            { httpsuite.Realms(t, factory) }
func TestDoubleLinksOff(t *testing.T)          { httpsuite.LinksOff(t, factory) }
func TestDoubleVendor(t *testing.T)            { httpsuite.Vendor(t, factory) }
func TestDoubleDirectory(t *testing.T)         { httpsuite.Directory(t, factory) }
func TestDoubleAudit(t *testing.T)             { httpsuite.Audit(t, factory) }
func TestDoubleLinks(t *testing.T)             { httpsuite.Links(t, factory) }
func TestDoubleRegistrations(t *testing.T)     { httpsuite.Registrations(t, factory) }
func TestDoubleCommercial(t *testing.T)        { httpsuite.Commercial(t, factory) }
func TestDoubleLifecycle(t *testing.T)         { httpsuite.Lifecycle(t, factory) }
func TestDoubleSeriesTail(t *testing.T)        { httpsuite.SeriesTail(t, factory) }
func TestDoubleBooks(t *testing.T)             { httpsuite.Books(t, factory) }
func TestDoubleLinksP2(t *testing.T)           { httpsuite.LinksP2(t, factory) }
func TestDoublePhaseTwoIsolation(t *testing.T) { httpsuite.PhaseTwoIsolation(t, factory) }
