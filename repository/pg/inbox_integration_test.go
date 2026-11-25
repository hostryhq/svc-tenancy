//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/inbox"

	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository/pg"
)

// delivery is one bus message for the inbox subscriber.
type delivery struct {
	data   []byte
	acked  bool
	nacked bool
	tries  uint64
}

func (d *delivery) Subject() string         { return "evt.billing.test.v1" }
func (d *delivery) Data() []byte            { return d.data }
func (d *delivery) NumDelivered() uint64    { return d.tries }
func (d *delivery) Ack() error              { d.acked = true; return nil }
func (d *delivery) Nak(time.Duration) error { d.nacked = true; return nil }

func billingEnvelope(typ string, tenant, outlet ids.ID, at time.Time, data any) []byte {
	b, _ := json.Marshal(data)
	e := events.Envelope{ID: ids.New(), Type: typ, V: 1, Source: "billing", TenantID: tenant, OutletID: outlet, Key: outlet.String(), KeySeq: 1, OccurredAt: at, Data: b}
	out, err := e.Marshal()
	if err != nil {
		panic(err)
	}
	return out
}

// The bus consumer wires the inbox subscriber to the service over the real schema: the handler's effect and
// its inbox row commit together, the tenant context applies row-level security to the handler's writes, a
// redelivery changes nothing, and an event that cannot be applied is parked after the last attempt.
func TestEventInboxAppliesBillingEvents(t *testing.T) {
	s := stack(t)
	repo := s.App.Repo.(*pg.Repo)
	sub := &inbox.Subscriber{DB: s.DB.App, Consumer: "billing", MaxAttempts: 2, Handle: func(ctx context.Context, tx *sql.Tx, e events.Envelope) error {
		rtx, err := repo.Adopt(ctx, tx, e.TenantID)
		if err != nil {
			return err
		}
		return s.App.Service.ApplyEvent(ctx, rtx, e)
	}}
	state := func(tenant, outlet ids.ID) (day, st string) {
		if err := s.DB.Admin.QueryRowContext(ctx, `SELECT to_char(business_date, 'YYYY-MM-DD'), state FROM outlet_trading_state WHERE tenant_id = $1 AND outlet_id = $2 AND source = 'billing'`,
			tenant, outlet).Scan(&day, &st); err != nil {
			t.Fatal(err)
		}
		return
	}
	at := seed.At.Add(time.Hour)
	closeDay := &delivery{data: billingEnvelope("billing.trading_day.changed", tenantA, seed.Example.Outlet, at,
		obj{"outlet_id": seed.Example.Outlet, "business_date": "2026-11-24", "state": "closed", "changed_at": at.Format(time.RFC3339Nano)}), tries: 1}
	if err := sub.Process(ctx, closeDay); err != nil || !closeDay.acked {
		t.Fatalf("applied and acknowledged: %v %+v", err, closeDay)
	}
	if day, st := state(tenantA, seed.Example.Outlet); day != "2026-11-24" || st != "closed" {
		t.Fatalf("the trading state follows billing: %s %s", day, st)
	}
	again := &delivery{data: closeDay.data, tries: 2}
	if err := sub.Process(ctx, again); err != nil || !again.acked {
		t.Fatalf("a redelivery is acknowledged without effect: %v", err)
	}
	var n int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM inbox WHERE consumer = 'billing'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("one inbox row: %d %v", n, err)
	}
	// An older change for the same day arrives late and is ignored.
	stale := &delivery{data: billingEnvelope("billing.trading_day.changed", tenantA, seed.Example.Outlet, at.Add(-time.Minute),
		obj{"outlet_id": seed.Example.Outlet, "business_date": "2026-11-24", "state": "open", "changed_at": at.Add(-time.Minute).Format(time.RFC3339Nano)}), tries: 1}
	if err := sub.Process(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, st := state(tenantA, seed.Example.Outlet); st != "closed" {
		t.Fatalf("out-of-order delivery: %s", st)
	}
	// An event of tenant A naming tenant B's outlet writes nothing for B (row-level security and the outlet check).
	cross := &delivery{data: billingEnvelope("billing.trading_day.changed", tenantA, seed.Other.Outlet, at.Add(time.Hour),
		obj{"outlet_id": seed.Other.Outlet, "business_date": "2026-11-25", "state": "open", "changed_at": at.Add(time.Hour).Format(time.RFC3339Nano)}), tries: 1}
	if err := sub.Process(ctx, cross); err != nil {
		t.Fatal(err)
	}
	if day, st := state(tenantB, seed.Other.Outlet); day != "2026-11-24" || st != "open" {
		t.Fatalf("tenant B's state is untouched: %s %s", day, st)
	}
	// An event that cannot be applied is redelivered, then parked on the last attempt.
	bad := billingEnvelope("billing.series.reconciled", tenantA, seed.Example.Outlet, at, obj{"series_id": "", "last_number": -1})
	first := &delivery{data: bad, tries: 1}
	if err := sub.Process(ctx, first); err != nil || !first.nacked || first.acked {
		t.Fatalf("a failing handler is redelivered: %v %+v", err, first)
	}
	last := &delivery{data: bad, tries: 2}
	if err := sub.Process(ctx, last); err != nil || !last.acked {
		t.Fatalf("the last attempt is parked and acknowledged: %v %+v", err, last)
	}
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM event_parked WHERE consumer = 'billing'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("one parked event: %d %v", n, err)
	}
}

// The migration seeds the catalogue of BR-A1-12 as drafts: nothing is sellable until the founder activates a
// version, and the application role cannot change a plan's content.
func TestSeededCatalogueIsDraftOnly(t *testing.T) {
	s := itest.New(t)
	var plans, versions, notDraft int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM plans), (SELECT count(*) FROM plan_versions), (SELECT count(*) FROM plan_versions WHERE state <> 'draft')`).Scan(&plans, &versions, &notDraft); err != nil {
		t.Fatal(err)
	}
	if plans != 6 || versions != 6 || notDraft != 0 {
		t.Fatalf("plans %d, versions %d, not draft %d", plans, versions, notDraft)
	}
	var price int64
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT (p->>'list_paise')::bigint FROM plan_versions v, jsonb_array_elements(v.prices) p WHERE v.plan_key = 'table_service' AND p->>'cycle' = 'monthly'`).Scan(&price); err != nil || price != 129900 {
		t.Fatalf("table_service monthly list price: %d %v", price, err)
	}
}
