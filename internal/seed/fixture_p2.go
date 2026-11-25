package seed

import (
	"context"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// ExampleRequester files the lifecycle requests of the examples (the vendor user of the other examples).
const ExampleRequester = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a66"

// Phase2Fixture gives an organisation one row in every Phase 2 tenant table (a subscription with its
// entitlements, a charge with a payment, a bill book, a trading day, a used grant and a lifecycle entry), so
// isolation tests can prove that no other organisation sees or writes any of them. The shared plan is stored once.
func Phase2Fixture(ctx context.Context, repo repository.Repository, w World) error {
	if err := repo.InPlatformOnlyTx(ctx, func(c context.Context, tx repository.Tx) error {
		if _, err := tx.GetPlan(c, "table_service"); err == nil {
			return nil
		}
		v := model.PlanVersion{PlanKey: "table_service", Version: 1, State: model.VersionActive, FeatureKeys: tableServiceFeatures, IncludedOutlets: 1,
			Prices: []model.PlanPrice{{Cycle: model.CycleMonthly, ListPaise: 129900}}, CreatedAt: At, ActivatedAt: &At}
		return tx.InsertPlan(c, model.Plan{Key: "table_service", NameKey: "plan.table_service", CreatedAt: At, Versions: []model.PlanVersion{v}})
	}); err != nil {
		return err
	}
	return repo.InTx(asTenant(ctx, w.Org), func(c context.Context, tx repository.Tx) error {
		sub, charge, pay := ids.New(), ids.New(), ids.New()
		if err := tx.InsertSubscription(c, model.Subscription{ID: sub, PlanKey: "table_service", PlanVersion: 1, Cycle: model.CycleMonthly, AgreedPaise: 129900,
			StartDate: "2026-12-01", PaidThrough: "2026-12-31", State: model.SubActive, CreatedAt: At}); err != nil {
			return err
		}
		if err := tx.ReplaceEntitlements(c, sub, tableServiceFeatures, At); err != nil {
			return err
		}
		if err := tx.InsertCharge(c, model.Charge{ID: charge, SubscriptionID: sub, Kind: model.ChargePeriod, PeriodStart: "2026-12-01", PeriodEnd: "2026-12-31",
			TaxableP: 129900, CGSTP: 11691, SGSTP: 11691, TotalP: 153282, TolerancePaise: 99, DueDate: "2026-12-08", Status: model.ChargeOpen, CreatedBy: Approver, CreatedAt: At}); err != nil {
			return err
		}
		if err := tx.InsertPayment(c, model.Payment{ID: pay, ChargeID: charge, AmountP: 1000, Method: "cash", ReceivedOn: "2026-12-02", RecordedBy: Approver, CreatedAt: At}); err != nil {
			return err
		}
		if err := tx.InsertManualBook(c, model.ManualBook{ID: ids.New(), TaxRegistrationID: w.Registration, GSTIN: w.GSTIN, Code: "BKX", OutletIDs: []ids.ID{w.Outlet},
			Status: model.BookActive, CreatedAt: At}); err != nil {
			return err
		}
		if _, err := tx.UpsertTradingState(c, model.TradingState{OutletID: w.Outlet, Source: model.TradingSourceBilling, BusinessDate: "2026-11-24", State: "open", ChangedAt: At}); err != nil {
			return err
		}
		if _, err := tx.MarkGrantUsed(c, ids.New().String(), ids.New().String(), "org_suspend", At); err != nil {
			return err
		}
		return tx.AppendLifecycle(c, model.LifecycleEntry{ID: ids.New(), Kind: "org_suspend", ApprovalID: ids.New().String(), Requester: ExampleRequester, Approver: Approver,
			From: model.OrgActive, To: model.OrgSuspended, Ticket: "SUP-1", Reason: "fixture row for isolation tests", OccurredAt: At, ShowToOwner: true})
	})
}
