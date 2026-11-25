// Package scopecheck defines the T14 resources of the tenancy service (ADR-0003 A1.4) for go-auth's
// generated test (scopetest): one Resource per x-outlet-scoped resource, driven through the service's real
// methods. The same definitions run over the in-memory repository and over Postgres.
package scopecheck

import (
	"context"

	"github.com/hostryhq/go-auth/scopetest"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/service"
)

// World is the organisation the harness works in (tenant A of testkit, with the harness's outlets X and Y).
var World = seed.World{Org: testkit.TenantA, Outlet: scopetest.OutletX, OutletY: scopetest.OutletY,
	DefaultBrand: "018f0000-0000-7000-8000-0000000000e1", Brand: "018f0000-0000-7000-8000-0000000000e2",
	Registration: "018f0000-0000-7000-8000-0000000000e3", Series: "018f0000-0000-7000-8000-0000000000e4",
	Device: "018f0000-0000-7000-8000-0000000000e5", Name: "Scope Test", GSTIN: seed.ExampleGSTIN}

func idsOf(n int, f func(i int) string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

// Resources returns the outlet-scoped resources of the service: outlets and document series.
func Resources(svc *service.Service) []scopetest.Resource {
	return []scopetest.Resource{
		{
			Name: "outlets", Permission: "outlet.read",
			Seed: func(ctx context.Context, x, y string) (string, string, error) { return x, y, nil }, // both outlets exist in World
			List: func(ctx context.Context) ([]string, error) {
				pg, err := svc.ListOutlets(ctx, "", "", 200)
				return idsOf(len(pg.Items), func(i int) string { return pg.Items[i].ID.String() }), err
			},
			Get: func(ctx context.Context, id string) error { _, err := svc.GetOutlet(ctx, ids.ID(id)); return err },
		},
		{
			Name: "document series", Permission: "series.read",
			Seed: func(ctx context.Context, x, y string) (string, string, error) {
				rows, err := svc.IssueSeries(tenantCtx(), service.IssueInput{OutletID: ids.ID(y), DeviceID: "018f0000-0000-7000-8000-0000000000e6", DocTypes: []string{"tax_invoice", "credit_note"}, State: "active"})
				if err != nil {
					return "", "", err
				}
				return World.Series.String(), rows[0].ID.String(), nil
			},
			List: func(ctx context.Context) ([]string, error) {
				pg, err := svc.ListSeries(ctx, "", "", "", 200)
				return idsOf(len(pg.Items), func(i int) string { return pg.Items[i].ID.String() }), err
			},
			ListFiltered: func(ctx context.Context, outlets []string) ([]string, error) {
				pg, err := svc.ListSeries(ctx, ids.ID(outlets[0]), "", "", 200)
				return idsOf(len(pg.Items), func(i int) string { return pg.Items[i].ID.String() }), err
			},
			Get: func(ctx context.Context, id string) error { _, err := svc.GetSeries(ctx, ids.ID(id)); return err },
		},
	}
}
