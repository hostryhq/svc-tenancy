package service

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/states"
	"github.com/hostryhq/svc-tenancy/repository"
)

// The commercial module of ADR-0008 S2: plans and versions as data, Hostry's own GST configuration, the
// subscription and its entitlements. Money is integer paise excluding GST unless a field says otherwise.
// Nothing here gates trading: overdue is a label and non-payment switches nothing off (ADR-0008 S5).

const dateLayout = "2006-01-02"

var (
	planKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	featureKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{1,48}$`)
	sacPattern        = regexp.MustCompile(`^[0-9]{4,8}$`)
	addonNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
)

// maxMoney bounds every amount so sums and rate products stay far from int64 overflow.
const maxMoney = int64(1_000_000_000_000)

// pilotReviewDays is the default review date of a pilot (BR-A1-21).
const pilotReviewDays = 90

func (s *Service) today() string { return s.clock.Now().In(clock.IST).Format(dateLayout) }

func parseDate(field, v string) (time.Time, error) {
	t, err := time.Parse(dateLayout, v)
	if err != nil {
		return t, model.Invalid(field, "date", "must be a date, YYYY-MM-DD")
	}
	return t, nil
}

// addMonths adds n months, clamping to the last day of the month (31 Jan + 1 month is 28 or 29 Feb).
func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(d, last), 0, 0, 0, 0, time.UTC)
}

// PeriodEnd is the last day of the first cycle that starts on start.
func PeriodEnd(start time.Time, cycle string) time.Time {
	switch cycle {
	case model.CycleMonthly:
		return addMonths(start, 1).AddDate(0, 0, -1)
	case model.CycleAnnual:
		return addMonths(start, 12).AddDate(0, 0, -1)
	}
	return start
}

// ---- plans ----

// VersionInput is the content of a plan version (BR-A1-10, BR-A1-11).
type VersionInput struct {
	FeatureKeys     []string
	IncludedOutlets int
	Prices          []model.PlanPrice
}

func checkVersion(field string, in VersionInput) (model.PlanVersion, error) {
	if len(in.FeatureKeys) > 40 {
		return model.PlanVersion{}, model.Invalid(field+".feature_keys", "too_many", "at most 40 feature keys")
	}
	seen := map[string]bool{}
	keys := make([]string, 0, len(in.FeatureKeys))
	for _, k := range in.FeatureKeys {
		if !featureKeyPattern.MatchString(k) || seen[k] {
			return model.PlanVersion{}, model.Invalid(field+".feature_keys", "invalid", "feature keys are lower_snake with dots and unique")
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if in.IncludedOutlets < 0 || in.IncludedOutlets > 10000 {
		return model.PlanVersion{}, model.Invalid(field+".included_outlets", "range", "0 to 10000")
	}
	if len(in.Prices) < 1 || len(in.Prices) > 3 {
		return model.PlanVersion{}, model.Invalid(field+".prices", "length", "1 to 3 prices")
	}
	cycles := map[string]bool{}
	for _, p := range in.Prices {
		switch p.Cycle {
		case model.CycleNone, model.CycleMonthly, model.CycleAnnual:
		default:
			return model.PlanVersion{}, model.Invalid(field+".prices", "cycle", "unknown cycle")
		}
		if cycles[p.Cycle] || p.ListPaise < 0 || p.ListPaise > maxMoney {
			return model.PlanVersion{}, model.Invalid(field+".prices", "invalid", "one price per cycle, not negative")
		}
		cycles[p.Cycle] = true
	}
	return model.PlanVersion{FeatureKeys: keys, IncludedOutlets: in.IncludedOutlets, Prices: in.Prices}, nil
}

// CreatePlan adds a plan with its first draft version. A plan is sellable only when a version is activated.
func (s *Service) CreatePlan(ctx context.Context, key, nameKey string, in VersionInput) (model.Plan, error) {
	if !planKeyPattern.MatchString(key) {
		return model.Plan{}, model.Invalid("key", "pattern", "lower_snake, 2 to 32 characters")
	}
	nameKey = strings.TrimSpace(nameKey)
	if n := len(nameKey); n < 1 || n > 100 {
		return model.Plan{}, model.Invalid("name_key", "length", "1 to 100 characters")
	}
	v, err := checkVersion("version", in)
	if err != nil {
		return model.Plan{}, err
	}
	return runPlatform(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Plan, error) {
		now := s.clock.Now()
		v.PlanKey, v.Version, v.State, v.CreatedAt = key, 1, model.VersionDraft, now
		p := model.Plan{Key: key, NameKey: nameKey, CreatedAt: now, Versions: []model.PlanVersion{v}}
		if err := tx.InsertPlan(ctx, p); err != nil {
			return p, err
		}
		return p, nil
	})
}

// CreatePlanVersion adds the next draft version; an active version is never edited (BR-A1-10).
func (s *Service) CreatePlanVersion(ctx context.Context, key string, in VersionInput) (model.PlanVersion, error) {
	v, err := checkVersion("", in)
	if err != nil {
		return v, err
	}
	return runPlatform(ctx, s, func(ctx context.Context, tx repository.Tx) (model.PlanVersion, error) {
		n, err := tx.NextPlanVersion(ctx, key)
		if err != nil {
			return v, err
		}
		v.PlanKey, v.Version, v.State, v.CreatedAt = key, n, model.VersionDraft, s.clock.Now()
		return v, tx.InsertPlanVersion(ctx, v)
	})
}

// ActivatePlanVersion makes a draft sellable and immutable.
func (s *Service) ActivatePlanVersion(ctx context.Context, key string, version int) (model.PlanVersion, error) {
	return runPlatform(ctx, s, func(ctx context.Context, tx repository.Tx) (model.PlanVersion, error) {
		v, err := tx.GetPlanVersion(ctx, key, version)
		if err != nil {
			return v, err
		}
		if v.State != model.VersionDraft {
			return v, apperr.New(apperr.Conflict, "version_not_draft", "only a draft version can be activated")
		}
		now := s.clock.Now()
		v.State, v.ActivatedAt = model.VersionActive, &now
		return v, tx.UpdatePlanVersion(ctx, v)
	})
}

// RetirePlanVersion stops selling a version; existing subscriptions keep it and show on a work list (BR-A1-15).
func (s *Service) RetirePlanVersion(ctx context.Context, key string, version int) (model.PlanVersion, error) {
	return runPlatform(ctx, s, func(ctx context.Context, tx repository.Tx) (model.PlanVersion, error) {
		v, err := tx.GetPlanVersion(ctx, key, version)
		if err != nil {
			return v, err
		}
		if v.State != model.VersionActive {
			return v, apperr.New(apperr.Conflict, "version_not_active", "only an active version can be retired")
		}
		now := s.clock.Now()
		v.State, v.RetiredAt = model.VersionRetired, &now
		return v, tx.UpdatePlanVersion(ctx, v)
	})
}

// GetPlan reads one plan with its versions (a directory-class read: no audit row).
func (s *Service) GetPlan(ctx context.Context, key string) (model.Plan, error) {
	var out model.Plan
	err := s.repo.InPlatformReadTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		var err error
		out, err = tx.GetPlan(ctx, key)
		return err
	})
	return out, err
}

// ListPlans pages plans in key order after afterKey.
func (s *Service) ListPlans(ctx context.Context, afterKey string, limit int) (PlanPage, error) {
	var rows []model.Plan
	err := s.repo.InPlatformReadTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		var err error
		rows, err = tx.ListPlans(ctx, afterKey, limit+1)
		return err
	})
	items, more := paginate(rows, limit)
	p := PlanPage{Items: items}
	if more {
		p.NextKey = items[len(items)-1].Key
	}
	return p, err
}

// PlanPage is one page of plans; NextKey is the key to continue after.
type PlanPage struct {
	Items   []model.Plan
	NextKey string
}

// ---- Hostry's GST configuration ----

// BillingConfigInput is a new configuration version (BR-A1-30). SAC and registration facts are CA-pending.
type BillingConfigInput struct {
	EffectiveFrom     string
	HostryStateCode   string
	SAC               string
	GSTRateBP         int
	GSTRegistered     bool
	PaymentToleranceP int64
	PaymentTermsDays  int
}

// CreateBillingConfig adds a configuration version. Versions are effective-dated; one per date.
func (s *Service) CreateBillingConfig(ctx context.Context, in BillingConfigInput) (model.BillingConfig, error) {
	if _, err := parseDate("effective_from", in.EffectiveFrom); err != nil {
		return model.BillingConfig{}, err
	}
	if !states.Valid(in.HostryStateCode) {
		return model.BillingConfig{}, model.Invalid("hostry_state_code", "unknown_state", "state is not in the reference table")
	}
	if !sacPattern.MatchString(in.SAC) {
		return model.BillingConfig{}, model.Invalid("sac", "pattern", "4 to 8 digits")
	}
	if in.GSTRateBP < 0 || in.GSTRateBP > 2800 {
		return model.BillingConfig{}, model.Invalid("gst_rate_bp", "range", "0 to 2800")
	}
	if in.PaymentToleranceP < 0 || in.PaymentToleranceP > 10000 {
		return model.BillingConfig{}, model.Invalid("payment_tolerance_paise", "range", "0 to 10000")
	}
	if in.PaymentTermsDays < 0 || in.PaymentTermsDays > 365 {
		return model.BillingConfig{}, model.Invalid("payment_terms_days", "range", "0 to 365")
	}
	return runPlatform(ctx, s, func(ctx context.Context, tx repository.Tx) (model.BillingConfig, error) {
		c := model.BillingConfig{ID: s.newID(), EffectiveFrom: in.EffectiveFrom, HostryStateCode: in.HostryStateCode, SAC: in.SAC, GSTRateBP: in.GSTRateBP,
			GSTRegistered: in.GSTRegistered, PaymentToleranceP: in.PaymentToleranceP, PaymentTermsDays: in.PaymentTermsDays, CreatedAt: s.clock.Now()}
		return c, tx.InsertBillingConfig(ctx, c)
	})
}

// ListBillingConfigs pages the versions, newest first.
func (s *Service) ListBillingConfigs(ctx context.Context, after ids.ID, limit int) (Page[model.BillingConfig], error) {
	var rows []model.BillingConfig
	err := s.repo.InPlatformReadTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		var err error
		rows, err = tx.ListBillingConfigs(ctx, after, limit+1)
		return err
	})
	return pageOf(rows, limit, func(c model.BillingConfig) ids.ID { return c.ID }), err
}

// ---- subscription ----

// SubView is a subscription row with its derived fields.
type SubView struct {
	model.Subscription
	State            string // the effective state: a cancellation that reached paid_through reads cancelled
	Overdue          bool   // derived: an open charge past its due date (BR-A1-26, BR-A1-52)
	OutletsOverLimit bool   // soft notice (BR-A1-25, A1-Q9)
}

// effectiveState derives cancelled once a requested cancellation reaches paid_through (BR-A1-24).
func effectiveState(sub model.Subscription, today string) string {
	if sub.State == model.SubCancelled || (sub.CancelAtPeriodEnd && sub.PaidThrough != "" && today > sub.PaidThrough) {
		return model.SubCancelled
	}
	return sub.State
}

// StartInput assigns a plan version or a pilot to an organisation.
type StartInput struct {
	PlanKey     string
	Version     int
	Cycle       string
	StartDate   string
	AgreedPaise *int64
	Addons      []model.Addon
	ReviewDate  string
	Reason      string
}

// ChangeInput changes plan, price or add-ons from the next charge.
type ChangeInput struct {
	PlanKey       string
	Version       int
	Cycle         string
	EffectiveFrom string
	AgreedPaise   *int64
	Addons        []model.Addon
	Reason        string
}

func checkAddons(in []model.Addon) ([]model.Addon, error) {
	if len(in) > 20 {
		return nil, model.Invalid("addons", "too_many", "at most 20 add-on lines")
	}
	seen := map[string]bool{}
	out := make([]model.Addon, 0, len(in))
	for _, a := range in {
		if !addonNamePattern.MatchString(a.Name) || seen[a.Name] || a.Quantity < 1 || a.Quantity > 1000 || a.AgreedPaise < 0 || a.AgreedPaise > maxMoney {
			return nil, model.Invalid("addons", "invalid", "add-on lines need a unique key, a quantity of 1 or more and a price")
		}
		seen[a.Name] = true
		out = append(out, a)
	}
	return out, nil
}

// priceFor resolves the agreed price (BR-A1-22): the list price by default; below it needs
// admin.subscription.adjust and a reason; above it needs admin.subscription.manage. Add-ons need manage.
func (s *Service) priceFor(ctx context.Context, v model.PlanVersion, cycle string, agreed *int64, addons []model.Addon, reason string) (int64, error) {
	list, ok := v.ListPrice(cycle)
	if !ok {
		return 0, model.Invalid("cycle", "no_list_price", "the plan version has no list price for this cycle")
	}
	price := list
	if agreed != nil {
		if *agreed < 0 || *agreed > maxMoney {
			return 0, model.Invalid("agreed_price", "range", "not a valid amount")
		}
		price = *agreed
	}
	switch {
	case price < list:
		if _, err := auth.ScopeFor(ctx, "admin.subscription.adjust"); err != nil {
			return 0, err
		}
		if n := len([]rune(strings.TrimSpace(reason))); n < 10 || n > 500 {
			return 0, model.Invalid("reason", "required", "a price below list needs a reason of 10 to 500 characters")
		}
	case price > list || len(addons) > 0:
		if _, err := auth.ScopeFor(ctx, "admin.subscription.manage"); err != nil {
			return 0, err
		}
	}
	return price, nil
}

// terms derives the stored state and the paid-through date (a pilot's review date) of a new row.
func terms(cycle string, start time.Time, review string) (state, paidThrough string, err error) {
	if cycle == model.CycleNone {
		if review != "" {
			r, e := parseDate("review_date", review)
			if e != nil {
				return "", "", e
			}
			if r.Before(start) {
				return "", "", model.Invalid("review_date", "before_start", "the review date is before the start")
			}
			return model.SubPilot, review, nil
		}
		return model.SubPilot, start.AddDate(0, 0, pilotReviewDays).Format(dateLayout), nil
	}
	return model.SubActive, PeriodEnd(start, cycle).Format(dateLayout), nil
}

func checkCycle(cycle string) error {
	switch cycle {
	case model.CycleNone, model.CycleMonthly, model.CycleAnnual:
		return nil
	}
	return model.Invalid("cycle", "unknown", "unknown cycle")
}

// activeVersion reads a plan version that can be sold.
func activeVersion(ctx context.Context, tx repository.Tx, key string, version int) (model.PlanVersion, error) {
	if !planKeyPattern.MatchString(key) || version < 1 {
		return model.PlanVersion{}, model.Invalid("plan_key", "invalid", "not a plan version")
	}
	v, err := tx.GetPlanVersion(ctx, key, version)
	if err != nil {
		return v, err
	}
	if v.State != model.VersionActive {
		return v, apperr.New(apperr.Conflict, "plan_version_not_active", "the plan version is not active")
	}
	return v, nil
}

// StartSubscription assigns a plan version or a pilot (BR-A1-20 to BR-A1-22). The subscription row and the
// organisation's entitlement rows change in one transaction (BR-A1-14, CB1); entitlements carry no expiry.
// The first plan moves the organisation from trial to active (BR-A1-74). A replay with the same key
// returns the same row.
func (s *Service) StartSubscription(ctx context.Context, orgID ids.ID, in StartInput) (SubView, error) {
	if err := checkCycle(in.Cycle); err != nil {
		return SubView{}, err
	}
	start, err := parseDate("start_date", in.StartDate)
	if err != nil {
		return SubView{}, err
	}
	addons, err := checkAddons(in.Addons)
	if err != nil {
		return SubView{}, err
	}
	state, paid, err := terms(in.Cycle, start, in.ReviewDate)
	if err != nil {
		return SubView{}, err
	}
	ctx, err = auth.VendorTenant(ctx, "admin.subscription.start", orgID)
	if err != nil {
		return SubView{}, err
	}
	auth.SetAuditTarget(ctx, orgID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (SubView, error) {
		org, err := tx.GetOrg(ctx)
		if err != nil {
			return SubView{}, err
		}
		if org.Status == model.OrgClosed {
			return SubView{}, apperr.New(apperr.Conflict, "organisation_closed", "the organisation is closed")
		}
		cur, has, err := tx.CurrentSubscription(ctx)
		if err != nil {
			return SubView{}, err
		}
		now := s.clock.Now()
		if has {
			if effectiveState(cur, s.today()) != model.SubCancelled {
				return SubView{}, apperr.New(apperr.Conflict, "subscription_exists", "the organisation has a subscription; change it instead")
			}
			cur.SupersededAt = &now
			if err := tx.UpdateSubscription(ctx, cur); err != nil {
				return SubView{}, err
			}
		}
		ver, err := activeVersion(ctx, tx, in.PlanKey, in.Version)
		if err != nil {
			return SubView{}, err
		}
		price, err := s.priceFor(ctx, ver, in.Cycle, in.AgreedPaise, addons, in.Reason)
		if err != nil {
			return SubView{}, err
		}
		sub := model.Subscription{ID: s.newID(), PlanKey: in.PlanKey, PlanVersion: in.Version, Cycle: in.Cycle, AgreedPaise: price, Addons: addons,
			StartDate: in.StartDate, PaidThrough: paid, State: state, CreatedAt: now}
		if err := tx.InsertSubscription(ctx, sub); err != nil {
			return SubView{}, err
		}
		if err := s.applyPlan(ctx, tx, org, sub, ver, p.VendorUser()); err != nil {
			return SubView{}, err
		}
		s.log.InfoContext(ctx, "subscription started", "organisation_id", orgID, "plan_key", sub.PlanKey, "plan_version", sub.PlanVersion, "cycle", sub.Cycle)
		return s.subView(ctx, tx, sub)
	})
}

// applyPlan replaces the entitlement rows, publishes the change and, for the first plan, moves a trial
// organisation to active. All of it is in the caller's transaction.
func (s *Service) applyPlan(ctx context.Context, tx repository.Tx, org model.Org, sub model.Subscription, ver model.PlanVersion, actorID string) error {
	now := s.clock.Now()
	if err := tx.ReplaceEntitlements(ctx, sub.ID, ver.FeatureKeys, now); err != nil {
		return err
	}
	if err := s.publish(ctx, tx, tev.SubscriptionChanged(s.newID(), now, actorID, org.ID, sub.ID, sub.PlanKey, sub.PlanVersion, sub.Cycle, sub.State)); err != nil {
		return err
	}
	if org.Status == model.OrgTrial {
		if err := tx.SetOrgStatus(ctx, model.OrgActive, now); err != nil {
			return err
		}
		return s.publish(ctx, tx, tev.OrganisationUpdated(s.newID(), now, actorID, org.ID, model.OrgActive, "status"))
	}
	return nil
}

// ChangeSubscription records a plan, price or add-on change as a new row from effective_from, with no
// proration and no mid-cycle credit (BR-A1-23). The earlier row becomes history.
func (s *Service) ChangeSubscription(ctx context.Context, orgID ids.ID, in ChangeInput) (SubView, error) {
	if err := checkCycle(in.Cycle); err != nil {
		return SubView{}, err
	}
	from, err := parseDate("effective_from", in.EffectiveFrom)
	if err != nil {
		return SubView{}, err
	}
	addons, err := checkAddons(in.Addons)
	if err != nil {
		return SubView{}, err
	}
	ctx, err = auth.VendorTenant(ctx, "admin.subscription.manage", orgID)
	if err != nil {
		return SubView{}, err
	}
	auth.SetAuditTarget(ctx, orgID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (SubView, error) {
		org, err := tx.GetOrg(ctx)
		if err != nil {
			return SubView{}, err
		}
		if org.Status == model.OrgClosed {
			return SubView{}, apperr.New(apperr.Conflict, "organisation_closed", "the organisation is closed")
		}
		cur, has, err := tx.CurrentSubscription(ctx)
		if err != nil {
			return SubView{}, err
		}
		if !has || effectiveState(cur, s.today()) == model.SubCancelled {
			return SubView{}, apperr.New(apperr.Conflict, "no_active_subscription", "the organisation has no current subscription to change")
		}
		if !from.After(mustDate(cur.StartDate)) {
			return SubView{}, model.Invalid("effective_from", "not_after_start", "must be after the current row starts")
		}
		ver, err := activeVersion(ctx, tx, in.PlanKey, in.Version)
		if err != nil {
			return SubView{}, err
		}
		price, err := s.priceFor(ctx, ver, in.Cycle, in.AgreedPaise, addons, in.Reason)
		if err != nil {
			return SubView{}, err
		}
		state, paid, err := terms(in.Cycle, from, "")
		if err != nil {
			return SubView{}, err
		}
		if cur.State == model.SubActive && in.Cycle != model.CycleNone && cur.PaidThrough != "" {
			paid = cur.PaidThrough // the paid period is not shortened or extended by a change
		}
		now := s.clock.Now()
		cur.SupersededAt = &now
		if err := tx.UpdateSubscription(ctx, cur); err != nil {
			return SubView{}, err
		}
		sub := model.Subscription{ID: s.newID(), PlanKey: in.PlanKey, PlanVersion: in.Version, Cycle: in.Cycle, AgreedPaise: price, Addons: addons,
			StartDate: in.EffectiveFrom, PaidThrough: paid, State: state, CreatedAt: now}
		if err := tx.InsertSubscription(ctx, sub); err != nil {
			return SubView{}, err
		}
		if err := s.applyPlan(ctx, tx, org, sub, ver, p.VendorUser()); err != nil {
			return SubView{}, err
		}
		return s.subView(ctx, tx, sub)
	})
}

func mustDate(v string) time.Time {
	t, _ := time.Parse(dateLayout, v)
	return t
}

// CancelSubscription records the owner's cancellation request: no further charge is raised after
// paid_through, and the state reads cancelled from the day after it. No refund is modelled; closing the
// organisation is a separate lifecycle action. Repeating it changes nothing.
func (s *Service) CancelSubscription(ctx context.Context, orgID ids.ID) (SubView, error) {
	ctx, err := auth.VendorTenant(ctx, "admin.subscription.manage", orgID)
	if err != nil {
		return SubView{}, err
	}
	auth.SetAuditTarget(ctx, orgID.String())
	p, _ := auth.PrincipalFrom(ctx)
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (SubView, error) {
		org, err := tx.GetOrg(ctx)
		if err != nil {
			return SubView{}, err
		}
		cur, has, err := tx.CurrentSubscription(ctx)
		if err != nil {
			return SubView{}, err
		}
		if !has || effectiveState(cur, s.today()) == model.SubCancelled {
			return SubView{}, apperr.New(apperr.Conflict, "no_active_subscription", "the organisation has no current subscription to cancel")
		}
		if cur.CancelAtPeriodEnd {
			return s.subView(ctx, tx, cur)
		}
		cur.CancelAtPeriodEnd = true
		if err := tx.UpdateSubscription(ctx, cur); err != nil {
			return SubView{}, err
		}
		if err := s.publish(ctx, tx, tev.SubscriptionChanged(s.newID(), s.clock.Now(), p.VendorUser(), org.ID, cur.ID, cur.PlanKey, cur.PlanVersion, cur.Cycle, cur.State)); err != nil {
			return SubView{}, err
		}
		return s.subView(ctx, tx, cur)
	})
}

// SubHistory is the current subscription and the earlier rows.
type SubHistory struct {
	Current *SubView
	History []SubView
}

// GetOrganisationSubscription reads the current subscription and its history (a directory-class read).
func (s *Service) GetOrganisationSubscription(ctx context.Context, orgID ids.ID) (SubHistory, error) {
	ctx, err := auth.VendorTenant(ctx, "admin.subscription.read", orgID)
	if err != nil {
		return SubHistory{}, err
	}
	auth.SetAuditTarget(ctx, orgID.String())
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (SubHistory, error) {
		if _, err := tx.GetOrg(ctx); err != nil {
			return SubHistory{}, err
		}
		var out SubHistory
		cur, has, err := tx.CurrentSubscription(ctx)
		if err != nil {
			return out, err
		}
		if has {
			v, err := s.subView(ctx, tx, cur)
			if err != nil {
				return out, err
			}
			out.Current = &v
		}
		hist, err := tx.SubscriptionHistory(ctx)
		if err != nil {
			return out, err
		}
		for _, h := range hist {
			h := h
			out.History = append(out.History, SubView{Subscription: h, State: h.State})
		}
		return out, nil
	})
}

// subView adds the derived fields to a subscription row.
func (s *Service) subView(ctx context.Context, tx repository.Tx, sub model.Subscription) (SubView, error) {
	v := SubView{Subscription: sub, State: effectiveState(sub, s.today())}
	charges, net, err := allCharges(ctx, tx)
	if err != nil {
		return v, err
	}
	today := s.today()
	for _, c := range charges {
		cv := chargeView(c, net[c.ID])
		if c.Status == model.ChargeOpen && cv.Outstanding > c.TolerancePaise && c.DueDate < today {
			v.Overdue = true
		}
	}
	if ver, err := tx.GetPlanVersion(ctx, sub.PlanKey, sub.PlanVersion); err == nil {
		n, err := tx.CountOutlets(ctx)
		if err != nil {
			return v, err
		}
		extra := 0
		for _, a := range sub.Addons {
			if a.Name == "extra_outlet" {
				extra += a.Quantity
			}
		}
		v.OutletsOverLimit = n > ver.IncludedOutlets+extra
	}
	return v, nil
}

// ChargeView is a charge with its derived status and outstanding amount.
type ChargeView struct {
	model.Charge
	Status      string // open, paid, waived or cancelled
	Outstanding int64
}

// chargeView derives the status: paid when payments (net of reversals) come within the tolerance of the total
// (BR-A1-41); waived and cancelled charges owe nothing.
func chargeView(c model.Charge, net int64) ChargeView {
	v := ChargeView{Charge: c, Status: c.Status}
	if c.Status != model.ChargeOpen {
		return v
	}
	v.Outstanding = max(c.TotalP-net, 0)
	if v.Outstanding <= c.TolerancePaise {
		v.Status = model.ChargePaid
	}
	return v
}

// allCharges reads every charge of the organisation with its net paid amount. An organisation has tens of rows.
func allCharges(ctx context.Context, tx repository.Tx) ([]model.Charge, map[ids.ID]int64, error) {
	var out []model.Charge
	var after ids.ID
	for {
		rows, err := tx.ListCharges(ctx, "", after, 500)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, rows...)
		if len(rows) < 500 {
			break
		}
		after = rows[len(rows)-1].ID
	}
	net, err := tx.NetPaid(ctx)
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PeriodStart > out[j].PeriodStart })
	return out, net, nil
}
