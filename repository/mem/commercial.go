package mem

import (
	"context"
	"sort"
	"time"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

type tradingKey struct {
	t, outlet ids.ID
	source    string
}

type lifeRow struct {
	t ids.ID
	e model.LifecycleEntry
}

// commercial is the Phase 2 state: platform data (plans, GST configuration), the tenant-owned ledger, the
// trading state and the lifecycle records. It is deep-copied for each transaction like the rest of the state.
type commercial struct {
	books     map[key]model.ManualBook
	trading   map[tradingKey]model.TradingState
	grants    map[key]string // (tenant, jti) -> approval id
	lifecycle []lifeRow
	plans     map[string]model.Plan
	configs   []model.BillingConfig
	subs      map[ids.ID][]model.Subscription // per tenant, oldest first
	ents      map[ids.ID][]string
	entBumps  map[ids.ID]int // plan and status changes per tenant
	charges   map[key]model.Charge
	pays      map[ids.ID][]model.Payment // per tenant, oldest first
}

func newCommercial() *commercial {
	return &commercial{books: map[key]model.ManualBook{}, trading: map[tradingKey]model.TradingState{}, grants: map[key]string{},
		plans: map[string]model.Plan{}, subs: map[ids.ID][]model.Subscription{}, ents: map[ids.ID][]string{}, entBumps: map[ids.ID]int{},
		charges: map[key]model.Charge{}, pays: map[ids.ID][]model.Payment{}}
}

func (c *commercial) clone() *commercial {
	n := newCommercial()
	for k, v := range c.books {
		v.OutletIDs = append([]ids.ID(nil), v.OutletIDs...)
		n.books[k] = v
	}
	for k, v := range c.trading {
		n.trading[k] = v
	}
	for k, v := range c.grants {
		n.grants[k] = v
	}
	n.lifecycle = append([]lifeRow(nil), c.lifecycle...)
	for k, v := range c.plans {
		v.Versions = append([]model.PlanVersion(nil), v.Versions...)
		n.plans[k] = v
	}
	n.configs = append([]model.BillingConfig(nil), c.configs...)
	for k, v := range c.subs {
		n.subs[k] = append([]model.Subscription(nil), v...)
	}
	for k, v := range c.ents {
		n.ents[k] = append([]string(nil), v...)
	}
	for k, v := range c.entBumps {
		n.entBumps[k] = v
	}
	for k, v := range c.charges {
		n.charges[k] = v
	}
	for k, v := range c.pays {
		n.pays[k] = append([]model.Payment(nil), v...)
	}
	return n
}

// Lifecycle returns the tenant lifecycle audit entries of an organisation, oldest first.
func (s *Store) Lifecycle(tenant ids.ID) []model.LifecycleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.LifecycleEntry
	for _, r := range s.st.com.lifecycle {
		if r.t == tenant {
			out = append(out, r.e)
		}
	}
	return out
}

// InPlatformOnlyTx implements repository.Repository.
func (s *Store) InPlatformOnlyTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	return s.platformTx(ctx, model.PlatformTenant, fn)
}

// InPlatformReadTx implements repository.Repository: reads a copy, writes nothing, records no audit row.
func (s *Store) InPlatformReadTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(ctx, &tx{st: s.st.clone(), t: model.PlatformTenant})
}

// ---- organisation status, lifecycle, grants ----

func (x *tx) SetOrgStatus(_ context.Context, status string, now time.Time) error {
	o, ok := x.st.orgs[x.t]
	if !ok {
		return model.ErrNotFound
	}
	o.Status, o.UpdatedAt = status, now
	x.st.orgs[x.t] = o
	x.st.com.entBumps[x.t]++
	return nil
}

func (x *tx) AppendLifecycle(_ context.Context, e model.LifecycleEntry) error {
	x.st.com.lifecycle = append(x.st.com.lifecycle, lifeRow{x.t, e})
	return nil
}

func (x *tx) MarkGrantUsed(_ context.Context, jti, approvalID, _ string, _ time.Time) (bool, error) {
	k := key{x.t, ids.ID(jti)}
	if _, dup := x.st.com.grants[k]; dup {
		return false, nil
	}
	for gk, a := range x.st.com.grants {
		if gk.t == x.t && a == approvalID {
			return false, nil
		}
	}
	x.st.com.grants[k] = approvalID
	return true, nil
}

// ---- manual books ----

func (x *tx) InsertManualBook(_ context.Context, b model.ManualBook) error {
	for k, e := range x.st.series {
		if k.t == x.t && e.Code == b.Code && (e.TaxRegistrationID == b.TaxRegistrationID || (b.GSTIN != "" && e.GSTIN == b.GSTIN)) {
			return model.ErrSeriesCodeInUse
		}
	}
	for k, e := range x.st.com.books {
		if k.t == x.t && e.TaxRegistrationID == b.TaxRegistrationID && e.Code == b.Code {
			return model.ErrSeriesCodeInUse
		}
	}
	b.OutletIDs = append([]ids.ID(nil), b.OutletIDs...)
	x.st.com.books[key{x.t, b.ID}] = b
	return nil
}

func (x *tx) ListManualBooks(_ context.Context, after ids.ID, limit int) ([]model.ManualBook, error) {
	var out []model.ManualBook
	for k, b := range x.st.com.books {
		if k.t == x.t && b.ID > after {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

// ---- series tail ----

func (x *tx) CloseOutletSeries(_ context.Context, outletID, regID ids.ID, now time.Time) ([]model.Series, error) {
	var out []model.Series
	for k, s := range x.st.series {
		if k.t == x.t && s.OutletID == outletID && s.TaxRegistrationID == regID && (s.Status == model.SeriesActive || s.Status == model.SeriesDormant) {
			s.Status, s.ClosedAt = model.SeriesClosed, &now
			x.st.series[k] = s
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (x *tx) OpenSeriesIDs(_ context.Context, outletID ids.ID) ([]ids.ID, error) {
	var out []ids.ID
	for k, s := range x.st.series {
		if k.t == x.t && s.OutletID == outletID && (s.Status == model.SeriesActive || s.Status == model.SeriesDormant) {
			out = append(out, s.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (x *tx) ConfirmTail(_ context.Context, id ids.ID, at time.Time, last int) (bool, error) {
	s, ok := x.st.series[key{x.t, id}]
	if !ok || (s.Status != model.SeriesClosed && s.Status != model.SeriesClosedUnreconciled) || s.TailConfirmedAt != nil {
		return false, nil
	}
	s.TailConfirmedAt, s.LastNumber = &at, &last
	x.st.series[key{x.t, id}] = s
	return true, nil
}

// ---- trading state ----

func (x *tx) UpsertTradingState(_ context.Context, s model.TradingState) (bool, error) {
	if _, ok := x.st.outlets[key{x.t, s.OutletID}]; !ok {
		return false, nil
	}
	k := tradingKey{x.t, s.OutletID, s.Source}
	if cur, ok := x.st.com.trading[k]; ok {
		if s.BusinessDate < cur.BusinessDate || (s.BusinessDate == cur.BusinessDate && !s.ChangedAt.After(cur.ChangedAt)) {
			return false, nil
		}
	}
	x.st.com.trading[k] = s
	return true, nil
}

func (x *tx) OutletsInProgress(_ context.Context, from, to string, outletID ids.ID) (int, error) {
	seen := map[ids.ID]bool{}
	for k, s := range x.st.com.trading {
		if k.t == x.t && s.State == "open" && s.BusinessDate >= from && s.BusinessDate <= to && (outletID == "" || s.OutletID == outletID) {
			seen[s.OutletID] = true
		}
	}
	return len(seen), nil
}

// ---- plans and the GST configuration ----

func (x *tx) GetPlan(_ context.Context, key string) (model.Plan, error) {
	p, ok := x.st.com.plans[key]
	if !ok {
		return p, model.ErrNotFound
	}
	return sortedVersions(p), nil
}

func sortedVersions(p model.Plan) model.Plan {
	v := append([]model.PlanVersion(nil), p.Versions...)
	sort.Slice(v, func(i, j int) bool { return v[i].Version > v[j].Version })
	p.Versions = v
	return p
}

func (x *tx) ListPlans(_ context.Context, afterKey string, limit int) ([]model.Plan, error) {
	var out []model.Plan
	for k, p := range x.st.com.plans {
		if k > afterKey {
			out = append(out, sortedVersions(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return cut(out, limit), nil
}

func (x *tx) InsertPlan(_ context.Context, p model.Plan) error {
	if _, ok := x.st.com.plans[p.Key]; ok {
		return model.ErrPlanExists
	}
	x.st.com.plans[p.Key] = p
	return nil
}

func (x *tx) InsertPlanVersion(_ context.Context, v model.PlanVersion) error {
	p, ok := x.st.com.plans[v.PlanKey]
	if !ok {
		return model.ErrNotFound
	}
	p.Versions = append(append([]model.PlanVersion(nil), p.Versions...), v)
	x.st.com.plans[v.PlanKey] = p
	return nil
}

func (x *tx) UpdatePlanVersion(_ context.Context, v model.PlanVersion) error {
	p, ok := x.st.com.plans[v.PlanKey]
	if !ok {
		return model.ErrNotFound
	}
	vs := append([]model.PlanVersion(nil), p.Versions...)
	for i := range vs {
		if vs[i].Version == v.Version {
			vs[i] = v
			p.Versions = vs
			x.st.com.plans[v.PlanKey] = p
			return nil
		}
	}
	return model.ErrNotFound
}

func (x *tx) GetPlanVersion(_ context.Context, key string, version int) (model.PlanVersion, error) {
	for _, v := range x.st.com.plans[key].Versions {
		if v.Version == version {
			return v, nil
		}
	}
	return model.PlanVersion{}, model.ErrNotFound
}

func (x *tx) NextPlanVersion(_ context.Context, key string) (int, error) {
	p, ok := x.st.com.plans[key]
	if !ok {
		return 0, model.ErrNotFound
	}
	n := 0
	for _, v := range p.Versions {
		n = max(n, v.Version)
	}
	return n + 1, nil
}

func (x *tx) InsertBillingConfig(_ context.Context, c model.BillingConfig) error {
	for _, e := range x.st.com.configs {
		if e.EffectiveFrom == c.EffectiveFrom {
			return apperr.New(apperr.Conflict, "effective_from_taken", "a configuration already starts on this date")
		}
	}
	x.st.com.configs = append(x.st.com.configs, c)
	return nil
}

func sortedConfigs(in []model.BillingConfig) []model.BillingConfig {
	out := append([]model.BillingConfig(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].EffectiveFrom != out[j].EffectiveFrom {
			return out[i].EffectiveFrom > out[j].EffectiveFrom
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (x *tx) ListBillingConfigs(_ context.Context, after ids.ID, limit int) ([]model.BillingConfig, error) {
	all := sortedConfigs(x.st.com.configs)
	if after != "" {
		i := 0
		for ; i < len(all) && all[i].ID != after; i++ {
		}
		if i < len(all) {
			all = all[i+1:]
		}
	}
	return cut(all, limit), nil
}

func (x *tx) BillingConfigFor(_ context.Context, date string) (model.BillingConfig, bool, error) {
	for _, c := range sortedConfigs(x.st.com.configs) {
		if c.EffectiveFrom <= date {
			return c, true, nil
		}
	}
	return model.BillingConfig{}, false, nil
}

// ---- subscription ----

func (x *tx) CurrentSubscription(_ context.Context) (model.Subscription, bool, error) {
	rows := x.st.com.subs[x.t]
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].SupersededAt == nil {
			return rows[i], true, nil
		}
	}
	return model.Subscription{}, false, nil
}

func (x *tx) SubscriptionHistory(_ context.Context) ([]model.Subscription, error) {
	var out []model.Subscription
	rows := x.st.com.subs[x.t]
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].SupersededAt != nil {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

func (x *tx) InsertSubscription(_ context.Context, s model.Subscription) error {
	for _, e := range x.st.com.subs[x.t] {
		if e.SupersededAt == nil {
			return model.ErrIDConflict // one current subscription per organisation
		}
	}
	s.Addons = append([]model.Addon(nil), s.Addons...)
	x.st.com.subs[x.t] = append(x.st.com.subs[x.t], s)
	return nil
}

func (x *tx) UpdateSubscription(_ context.Context, s model.Subscription) error {
	rows := x.st.com.subs[x.t]
	for i := range rows {
		if rows[i].ID == s.ID {
			s.Addons = rows[i].Addons
			rows[i] = s
			return nil
		}
	}
	return model.ErrNotFound
}

func (x *tx) ReplaceEntitlements(_ context.Context, _ ids.ID, keys []string, _ time.Time) error {
	out := append([]string(nil), keys...)
	sort.Strings(out)
	x.st.com.ents[x.t] = out
	x.st.com.entBumps[x.t]++
	return nil
}

func (x *tx) EntitlementVersion(_ context.Context) (int, error) {
	if _, ok := x.st.orgs[x.t]; !ok {
		return 0, model.ErrNotFound
	}
	return 1 + x.st.com.entBumps[x.t], nil
}

func (x *tx) Entitlements(_ context.Context) ([]string, error) {
	return append([]string(nil), x.st.com.ents[x.t]...), nil
}

func (x *tx) CountOutlets(_ context.Context) (int, error) {
	n := 0
	for k := range x.st.outlets {
		if k.t == x.t {
			n++
		}
	}
	return n, nil
}

// ---- charges and payments ----

func (x *tx) InsertCharge(_ context.Context, c model.Charge) error {
	x.st.com.charges[key{x.t, c.ID}] = c
	return nil
}

func (x *tx) GetCharge(_ context.Context, id ids.ID) (model.Charge, error) {
	c, ok := x.st.com.charges[key{x.t, id}]
	if !ok {
		return c, model.ErrNotFound
	}
	return c, nil
}

func (x *tx) UpdateCharge(_ context.Context, c model.Charge) error {
	if _, ok := x.st.com.charges[key{x.t, c.ID}]; !ok {
		return model.ErrNotFound
	}
	x.st.com.charges[key{x.t, c.ID}] = c
	return nil
}

func (x *tx) tenantCharges() []model.Charge {
	var out []model.Charge
	for k, c := range x.st.com.charges {
		if k.t == x.t {
			out = append(out, c)
		}
	}
	return out
}

func (x *tx) ListCharges(_ context.Context, status string, after ids.ID, limit int) ([]model.Charge, error) {
	all := x.tenantCharges()
	sort.Slice(all, func(i, j int) bool {
		if all[i].PeriodStart != all[j].PeriodStart {
			return all[i].PeriodStart > all[j].PeriodStart
		}
		return all[i].ID > all[j].ID
	})
	var out []model.Charge
	skipping := after != ""
	for _, c := range all {
		if skipping {
			if c.ID == after {
				skipping = false
			}
			continue
		}
		if status == "" || c.Status == status {
			out = append(out, c)
		}
	}
	return cut(out, limit), nil
}

func (x *tx) LiveChargesOverlapping(_ context.Context, start, end string) ([]model.Charge, error) {
	var out []model.Charge
	for _, c := range x.tenantCharges() {
		if c.Kind == model.ChargePeriod && c.Status != model.ChargeCancelled && c.PeriodStart <= end && c.PeriodEnd >= start {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (x *tx) InsertPayment(_ context.Context, p model.Payment) error {
	if p.ReversalOf != "" {
		for _, e := range x.st.com.pays[x.t] {
			if e.ReversalOf == p.ReversalOf {
				return model.ErrIDConflict
			}
		}
	}
	x.st.com.pays[x.t] = append(x.st.com.pays[x.t], p)
	return nil
}

func (x *tx) GetPayment(_ context.Context, chargeID, id ids.ID) (model.Payment, error) {
	for _, p := range x.st.com.pays[x.t] {
		if p.ID == id && p.ChargeID == chargeID {
			return p, nil
		}
	}
	return model.Payment{}, model.ErrNotFound
}

func (x *tx) ListPayments(_ context.Context, chargeID, after ids.ID, limit int) ([]model.Payment, error) {
	var out []model.Payment
	for _, p := range x.st.com.pays[x.t] {
		if p.ChargeID == chargeID && p.ID > after {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

func (x *tx) AllPayments(_ context.Context) ([]model.Payment, error) {
	out := append([]model.Payment(nil), x.st.com.pays[x.t]...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func netPaid(pays []model.Payment) map[ids.ID]int64 {
	out := map[ids.ID]int64{}
	for _, p := range pays {
		if p.ReversalOf == "" {
			out[p.ChargeID] += p.AmountP
		} else {
			out[p.ChargeID] -= p.AmountP
		}
	}
	return out
}

func (x *tx) NetPaid(_ context.Context) (map[ids.ID]int64, error) {
	return netPaid(x.st.com.pays[x.t]), nil
}

func (x *tx) CountPaymentRef(_ context.Context, reference string) (int, error) {
	n := 0
	for _, p := range x.st.com.pays[x.t] {
		if p.ReversalOf == "" && p.Reference == reference {
			n++
		}
	}
	return n, nil
}

// ---- links ----

func (x *tx) ReissueInvite(_ context.Context, linkID ids.ID, hash []byte, expires, _ time.Time) error {
	for _, iv := range x.st.invites {
		if iv.link == linkID {
			iv.used = true
		}
	}
	x.st.invites[string(hash)] = &invite{link: linkID, expires: expires}
	return nil
}
