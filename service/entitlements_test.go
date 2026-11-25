package service_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository"
)

func TestEntitlementSnapshot(t *testing.T) {
	f := newFixture(t)
	if err := seed.Phase2Fixture(context.Background(), f.store, seed.Example); err != nil {
		t.Fatal(err)
	}

	t.Run("carries the plan features and included outlets", func(t *testing.T) {
		got, err := f.svc.GetEntitlements(system(seed.Org))
		if err != nil {
			t.Fatal(err)
		}
		if got.PlanKey != "table_service" || got.PlanVersion != 1 || got.Limits["outlets"] != 1 || len(got.Features) != 4 || got.Features[1] != "restaurant.core" {
			t.Fatalf("snapshot %+v", got)
		}
	})

	t.Run("another organisation never sees it", func(t *testing.T) {
		got, err := f.svc.GetEntitlements(system(seed.Other.Org))
		if err != nil {
			t.Fatal(err)
		}
		if got.PlanKey != "" || len(got.Features) != 0 || got.Features == nil || got.Version != 1 {
			t.Fatalf("an organisation with no plan got %+v", got)
		}
	})

	t.Run("an overdue charge changes nothing (T10, CB5)", func(t *testing.T) {
		before, _ := f.svc.GetEntitlements(system(seed.Org))
		f.clk.Set(seed.At.Add(90 * 24 * time.Hour)) // the fixture charge fell due on 2026-12-08
		after, err := f.svc.GetEntitlements(system(seed.Org))
		if err != nil {
			t.Fatal(err)
		}
		if after.Version != before.Version || !reflect.DeepEqual(after.Features, before.Features) || !reflect.DeepEqual(after.Limits, before.Limits) || after.OrgStatus != before.OrgStatus {
			t.Fatalf("overdue changed the snapshot: %+v -> %+v", before, after)
		}
	})

	t.Run("a status change bumps the version and reaches the snapshot", func(t *testing.T) {
		before, _ := f.svc.GetEntitlements(system(seed.Org))
		err := f.store.InTx(system(seed.Org), func(c context.Context, tx repository.Tx) error {
			return tx.SetOrgStatus(c, model.OrgSuspended, f.clk.Now())
		})
		if err != nil {
			t.Fatal(err)
		}
		after, _ := f.svc.GetEntitlements(system(seed.Org))
		if after.OrgStatus != model.OrgSuspended || after.Version != before.Version+1 || len(after.Features) != len(before.Features) {
			t.Fatalf("after suspension %+v, before %+v", after, before)
		}
	})
}
