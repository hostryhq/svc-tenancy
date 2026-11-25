package seed

import (
	"context"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// Ids of the Phase 2 example worlds (api-specs info.x-example-worlds).
const (
	BillingConfigID ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f8041"
	SubscriptionID  ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f8011"
	ChargeID        ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f8021"
	PaymentID       ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f8031"
	BookID          ids.ID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f8051"
	// Approver is the vendor user of the examples that approves lifecycle actions and records payments.
	Approver = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a78"
)

// p2Spec says what a Phase 2 world adds to the base organisation 4a60.
type p2Spec struct {
	plans        bool   // platform: plan table_service draft v1 and Hostry's GST configuration 8041
	activePlan   bool   // version 1 is active
	subscribed   bool   // subscription 8011
	charged      bool   // and the open charge 8021
	annualListed bool   // version 1 also lists an annual price (1299000)
	payment      bool   // payment 8031 against charge 8021
	book         bool   // bill book 8051
	status       string // organisation status; empty leaves it as seeded (trial)
}

// WorldInstant is the instant "now" is in a world: the instant of the spec examples unless the world's
// description names another day. The double and the integration stack set their clock to it when a world is
// loaded, so derived fields (overdue, a payment's date received) are true in that world.
func WorldInstant(world string) time.Time {
	switch world {
	case WorldChargeOverdue:
		return time.Date(2026, 12, 11, 13, 5, 11, 120_000_000, time.UTC) // "on 2026-12-11"
	case WorldSubscribed, WorldPaymentRecorded:
		return time.Date(2026, 12, 2, 13, 5, 11, 120_000_000, time.UTC) // the example payment was received on 2026-12-02
	}
	return At
}

var tableServiceFeatures = []string{"restaurant.core", "restaurant.kds", "restaurant.captain", "restaurant.reports.core"}

// applyPhase2 stores the Phase 2 additions of a world. Platform data is written first (a subscription refers
// to its plan version), then the organisation's ledger and status.
func applyPhase2(ctx context.Context, repo repository.Repository, p p2Spec) error {
	if p.plans {
		if err := repo.InPlatformOnlyTx(ctx, func(c context.Context, tx repository.Tx) error {
			v := model.PlanVersion{PlanKey: "table_service", Version: 1, State: model.VersionDraft, FeatureKeys: tableServiceFeatures, IncludedOutlets: 1,
				Prices: []model.PlanPrice{{Cycle: model.CycleMonthly, ListPaise: 129900}}, CreatedAt: At}
			if p.annualListed {
				v.Prices = append(v.Prices, model.PlanPrice{Cycle: model.CycleAnnual, ListPaise: 1299000})
			}
			if p.activePlan {
				v.State, v.ActivatedAt = model.VersionActive, &At
			}
			if err := tx.InsertPlan(c, model.Plan{Key: "table_service", NameKey: "plan.table_service", CreatedAt: At, Versions: []model.PlanVersion{v}}); err != nil {
				return err
			}
			return tx.InsertBillingConfig(c, model.BillingConfig{ID: BillingConfigID, EffectiveFrom: "2026-12-01", HostryStateCode: "33", SAC: "997331", GSTRateBP: 1800,
				GSTRegistered: true, PaymentToleranceP: 99, PaymentTermsDays: 7, CreatedAt: At})
		}); err != nil {
			return err
		}
	}
	if !p.subscribed && !p.book && p.status == "" {
		return nil
	}
	return repo.InTx(asTenant(ctx, Org), func(c context.Context, tx repository.Tx) error {
		if p.status != "" {
			if err := tx.SetOrgStatus(c, p.status, At); err != nil {
				return err
			}
		}
		if p.book {
			if err := tx.InsertManualBook(c, model.ManualBook{ID: BookID, TaxRegistrationID: Registration, GSTIN: ExampleGSTIN, Code: "BKB",
				OutletIDs: []ids.ID{Outlet}, Status: model.BookActive, CreatedAt: At}); err != nil {
				return err
			}
		}
		if !p.subscribed {
			return nil
		}
		if err := tx.SetOrgStatus(c, model.OrgActive, At); err != nil { // the first plan moves trial to active
			return err
		}
		if err := tx.InsertSubscription(c, model.Subscription{ID: SubscriptionID, PlanKey: "table_service", PlanVersion: 1, Cycle: model.CycleMonthly, AgreedPaise: 129900,
			StartDate: "2026-12-01", PaidThrough: paidThrough(p), State: model.SubActive, CreatedAt: At}); err != nil {
			return err
		}
		if err := tx.ReplaceEntitlements(c, SubscriptionID, tableServiceFeatures, At); err != nil {
			return err
		}
		if !p.charged {
			return nil // subscribed-uncharged: nothing raised yet
		}
		if err := tx.InsertCharge(c, model.Charge{ID: ChargeID, SubscriptionID: SubscriptionID, Kind: model.ChargePeriod, PeriodStart: "2026-12-01", PeriodEnd: "2026-12-31",
			TaxableP: 129900, CGSTP: 11691, SGSTP: 11691, TotalP: 153282, TolerancePaise: 99, DueDate: "2026-12-08", Status: model.ChargeOpen, CreatedBy: Approver, CreatedAt: At}); err != nil {
			return err
		}
		if !p.payment {
			return nil
		}
		return tx.InsertPayment(c, model.Payment{ID: PaymentID, ChargeID: ChargeID, AmountP: 153282, Method: "upi", Reference: "UTR1234567890", ReceivedOn: "2026-12-02",
			RecordedBy: Approver, CreatedAt: At})
	})
}

// paidThrough is where the paid period ends: nothing is paid for until a charge is raised.
func paidThrough(p p2Spec) string {
	if !p.charged {
		return ""
	}
	return "2026-12-31"
}
