package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
)

// ---- organisation status, lifecycle audit, grants ----

func (x *pgTx) SetOrgStatus(ctx context.Context, status string, now time.Time) error {
	res, err := x.tx.ExecContext(ctx, `UPDATE organisations SET status = $2, updated_at = $3, entitlements_bumps = entitlements_bumps + 1 WHERE tenant_id = $1`, x.t, status, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrNotFound
	}
	_, err = x.tx.ExecContext(ctx, `UPDATE directory_organisations SET status = $2 WHERE subject_tenant_id = $1`, x.t, status)
	return err
}

func (x *pgTx) AppendLifecycle(ctx context.Context, e model.LifecycleEntry) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO organisation_lifecycle_log (tenant_id, id, kind, approval_id, requester, approver, from_status, to_status,
		ticket_reference, reason, owner_export_offered, occurred_at, show_to_owner) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		x.t, e.ID, e.Kind, e.ApprovalID, e.Requester, e.Approver, e.From, e.To, e.Ticket, e.Reason, e.ExportOffer, e.OccurredAt, e.ShowToOwner)
	return err
}

func (x *pgTx) MarkGrantUsed(ctx context.Context, jti, approvalID, kind string, now time.Time) (bool, error) {
	res, err := x.tx.ExecContext(ctx, `INSERT INTO approval_grant_used (tenant_id, jti, approval_id, kind, used_at) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		x.t, jti, approvalID, kind, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ---- manual bill books ----

func (x *pgTx) InsertManualBook(ctx context.Context, b model.ManualBook) error {
	var taken bool
	//scopelint:internal a book's code is unique under the GSTIN across every outlet's device series
	if err := x.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM document_series WHERE tenant_id = $1 AND gstin_normalised = $2 AND code = $3)
		OR EXISTS (SELECT 1 FROM manual_book_series WHERE tenant_id = $1 AND tax_registration_id = $4 AND code = $3)`, x.t, nullStr(b.GSTIN), b.Code, b.TaxRegistrationID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return model.ErrSeriesCodeInUse
	}
	_, err := x.tx.ExecContext(ctx, `INSERT INTO manual_book_series (tenant_id, id, tax_registration_id, gstin_normalised, code, outlet_ids, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, x.t, b.ID, b.TaxRegistrationID, nullStr(b.GSTIN), b.Code, jsonIDs(b.OutletIDs), b.Status, b.CreatedAt)
	if code, _ := pgCode(err); code == "23505" {
		return model.ErrSeriesCodeInUse
	}
	return err
}

func (x *pgTx) ListManualBooks(ctx context.Context, after ids.ID, limit int) ([]model.ManualBook, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT id, tax_registration_id, gstin_normalised, code, outlet_ids, status, created_at FROM manual_book_series
		WHERE tenant_id = $1 AND id > $2::uuid ORDER BY id LIMIT $3`, x.t, afterID(after), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.ManualBook
	for rows.Next() {
		var b model.ManualBook
		var g sql.NullString
		var outlets []byte
		if err := rows.Scan(&b.ID, &b.TaxRegistrationID, &g, &b.Code, &outlets, &b.Status, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.GSTIN, b.OutletIDs, b.CreatedAt = g.String, parseIDs(outlets), utc(b.CreatedAt)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- trading state ----

func (x *pgTx) UpsertTradingState(ctx context.Context, s model.TradingState) (bool, error) {
	var known bool
	if err := x.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM outlets WHERE tenant_id = $1 AND outlet_id = $2)`, x.t, s.OutletID).Scan(&known); err != nil {
		return false, err
	}
	if !known {
		return false, nil // an outlet this organisation does not have: nothing to guard
	}
	res, err := x.tx.ExecContext(ctx, `INSERT INTO outlet_trading_state (tenant_id, outlet_id, source, business_date, state, changed_at)
		VALUES ($1,$2,$3,$4::text::date,$5,$6)
		ON CONFLICT (tenant_id, outlet_id, source) DO UPDATE
		   SET business_date = EXCLUDED.business_date, state = EXCLUDED.state, changed_at = EXCLUDED.changed_at
		 WHERE (EXCLUDED.business_date, EXCLUDED.changed_at) > (outlet_trading_state.business_date, outlet_trading_state.changed_at)`,
		x.t, s.OutletID, s.Source, s.BusinessDate, s.State, s.ChangedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (x *pgTx) OutletsInProgress(ctx context.Context, from, to string, outletID ids.ID) (int, error) {
	var n int
	//scopelint:internal the guard counts every outlet of the organisation (or the one named); it never lists them
	err := x.tx.QueryRowContext(ctx, `SELECT count(DISTINCT outlet_id) FROM outlet_trading_state
		WHERE tenant_id = $1 AND state = 'open' AND business_date BETWEEN $2::text::date AND $3::text::date
		  AND ($4::text = '' OR outlet_id = NULLIF($4::text, '')::uuid)`, x.t, from, to, outletID.String()).Scan(&n)
	return n, err
}

// ---- plans and the GST configuration (platform data) ----

const versionCols = `plan_key, version, state, feature_keys, included_outlets, prices, created_at, activated_at, retired_at`

func scanVersion(row interface{ Scan(...any) error }) (model.PlanVersion, error) {
	var v model.PlanVersion
	var fk, pr []byte
	var act, ret sql.NullTime
	if err := row.Scan(&v.PlanKey, &v.Version, &v.State, &fk, &v.IncludedOutlets, &pr, &v.CreatedAt, &act, &ret); err != nil {
		return v, err
	}
	_ = json.Unmarshal(fk, &v.FeatureKeys)
	_ = json.Unmarshal(pr, &v.Prices)
	v.CreatedAt, v.ActivatedAt, v.RetiredAt = utc(v.CreatedAt), nullTime(act), nullTime(ret)
	return v, nil
}

func (x *pgTx) versions(ctx context.Context, key string) ([]model.PlanVersion, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT `+versionCols+` FROM plan_versions WHERE plan_key = $1 ORDER BY version DESC`, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.PlanVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (x *pgTx) GetPlan(ctx context.Context, key string) (model.Plan, error) {
	p := model.Plan{Key: key}
	if err := x.tx.QueryRowContext(ctx, `SELECT name_key, created_at FROM plans WHERE key = $1`, key).Scan(&p.NameKey, &p.CreatedAt); err != nil {
		return p, notFound(err)
	}
	p.CreatedAt = utc(p.CreatedAt)
	var err error
	p.Versions, err = x.versions(ctx, key)
	return p, err
}

func (x *pgTx) ListPlans(ctx context.Context, afterKey string, limit int) ([]model.Plan, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT key FROM plans WHERE key > $1 ORDER BY key LIMIT $2`, afterKey, limit)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	out := make([]model.Plan, 0, len(keys))
	for _, k := range keys {
		p, err := x.GetPlan(ctx, k)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (x *pgTx) InsertPlan(ctx context.Context, p model.Plan) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO plans (key, name_key, created_at) VALUES ($1,$2,$3)`, p.Key, p.NameKey, p.CreatedAt); err != nil {
		if code, _ := pgCode(err); code == "23505" {
			return model.ErrPlanExists
		}
		return err
	}
	for _, v := range p.Versions {
		if err := x.InsertPlanVersion(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

func (x *pgTx) InsertPlanVersion(ctx context.Context, v model.PlanVersion) error {
	fk, _ := json.Marshal(nonNil(v.FeatureKeys))
	pr, _ := json.Marshal(nonNilPrices(v.Prices))
	_, err := x.tx.ExecContext(ctx, `INSERT INTO plan_versions (`+versionCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		v.PlanKey, v.Version, v.State, string(fk), v.IncludedOutlets, string(pr), v.CreatedAt, v.ActivatedAt, v.RetiredAt)
	return err
}

func (x *pgTx) UpdatePlanVersion(ctx context.Context, v model.PlanVersion) error {
	_, err := x.tx.ExecContext(ctx, `UPDATE plan_versions SET state = $3, activated_at = $4, retired_at = $5 WHERE plan_key = $1 AND version = $2`,
		v.PlanKey, v.Version, v.State, v.ActivatedAt, v.RetiredAt)
	return err
}

func (x *pgTx) GetPlanVersion(ctx context.Context, key string, version int) (model.PlanVersion, error) {
	v, err := scanVersion(x.tx.QueryRowContext(ctx, `SELECT `+versionCols+` FROM plan_versions WHERE plan_key = $1 AND version = $2`, key, version))
	return v, notFound(err)
}

func (x *pgTx) NextPlanVersion(ctx context.Context, key string) (int, error) {
	var k string
	if err := x.tx.QueryRowContext(ctx, `SELECT key FROM plans WHERE key = $1 FOR UPDATE`, key).Scan(&k); err != nil {
		return 0, notFound(err)
	}
	var n int
	err := x.tx.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM plan_versions WHERE plan_key = $1`, key).Scan(&n)
	return n, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilPrices(s []model.PlanPrice) []model.PlanPrice {
	if s == nil {
		return []model.PlanPrice{}
	}
	return s
}

const configCols = `id, to_char(effective_from, 'YYYY-MM-DD'), hostry_state_code, sac, gst_rate_bp, gst_registered, payment_tolerance_paise, payment_terms_days, created_at`

func scanConfig(row interface{ Scan(...any) error }) (model.BillingConfig, error) {
	var c model.BillingConfig
	err := row.Scan(&c.ID, &c.EffectiveFrom, &c.HostryStateCode, &c.SAC, &c.GSTRateBP, &c.GSTRegistered, &c.PaymentToleranceP, &c.PaymentTermsDays, &c.CreatedAt)
	c.CreatedAt = utc(c.CreatedAt)
	return c, err
}

func (x *pgTx) InsertBillingConfig(ctx context.Context, c model.BillingConfig) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO billing_configurations (id, effective_from, hostry_state_code, sac, gst_rate_bp, gst_registered, payment_tolerance_paise,
		payment_terms_days, created_at) VALUES ($1,$2::text::date,$3,$4,$5,$6,$7,$8,$9)`,
		c.ID, c.EffectiveFrom, c.HostryStateCode, c.SAC, c.GSTRateBP, c.GSTRegistered, c.PaymentToleranceP, c.PaymentTermsDays, c.CreatedAt)
	if code, _ := pgCode(err); code == "23505" {
		return apperr.New(apperr.Conflict, "effective_from_taken", "a configuration already starts on this date")
	}
	return err
}

func (x *pgTx) ListBillingConfigs(ctx context.Context, after ids.ID, limit int) ([]model.BillingConfig, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT `+configCols+` FROM billing_configurations
		WHERE $2::text = '' OR (effective_from, id) < (SELECT effective_from, id FROM billing_configurations WHERE id = NULLIF($2::text, '')::uuid)
		ORDER BY effective_from DESC, id DESC LIMIT $1`, limit, after.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.BillingConfig
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (x *pgTx) BillingConfigFor(ctx context.Context, date string) (model.BillingConfig, bool, error) {
	c, err := scanConfig(x.tx.QueryRowContext(ctx, `SELECT `+configCols+` FROM billing_configurations WHERE effective_from <= $1::text::date ORDER BY effective_from DESC LIMIT 1`, date))
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// ---- subscription, entitlements ----

const subCols = `id, plan_key, plan_version, cycle, agreed_paise, addons, to_char(start_date, 'YYYY-MM-DD'), COALESCE(to_char(paid_through, 'YYYY-MM-DD'), ''),
	state, cancel_at_period_end, created_at, superseded_at`

func scanSub(row interface{ Scan(...any) error }) (model.Subscription, error) {
	var s model.Subscription
	var addons []byte
	var sup sql.NullTime
	err := row.Scan(&s.ID, &s.PlanKey, &s.PlanVersion, &s.Cycle, &s.AgreedPaise, &addons, &s.StartDate, &s.PaidThrough, &s.State, &s.CancelAtPeriodEnd, &s.CreatedAt, &sup)
	_ = json.Unmarshal(addons, &s.Addons)
	s.CreatedAt, s.SupersededAt = utc(s.CreatedAt), nullTime(sup)
	return s, err
}

func (x *pgTx) CurrentSubscription(ctx context.Context) (model.Subscription, bool, error) {
	s, err := scanSub(x.tx.QueryRowContext(ctx, `SELECT `+subCols+` FROM subscriptions WHERE tenant_id = $1 AND superseded_at IS NULL`, x.t))
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

func (x *pgTx) SubscriptionHistory(ctx context.Context) ([]model.Subscription, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT `+subCols+` FROM subscriptions WHERE tenant_id = $1 AND superseded_at IS NOT NULL ORDER BY id DESC`, x.t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Subscription
	for rows.Next() {
		s, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func addonsJSON(a []model.Addon) string {
	if a == nil {
		a = []model.Addon{}
	}
	b, _ := json.Marshal(a)
	return string(b)
}

func (x *pgTx) InsertSubscription(ctx context.Context, s model.Subscription) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO subscriptions (tenant_id, id, plan_key, plan_version, cycle, agreed_paise, addons, start_date, paid_through, state,
		cancel_at_period_end, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::text::date,NULLIF($9::text, '')::date,$10,$11,$12)`,
		x.t, s.ID, s.PlanKey, s.PlanVersion, s.Cycle, s.AgreedPaise, addonsJSON(s.Addons), s.StartDate, s.PaidThrough, s.State, s.CancelAtPeriodEnd, s.CreatedAt); err != nil {
		return err
	}
	return x.syncLedgerDirectory(ctx)
}

func (x *pgTx) UpdateSubscription(ctx context.Context, s model.Subscription) error {
	if _, err := x.tx.ExecContext(ctx, `UPDATE subscriptions SET paid_through = NULLIF($3::text, '')::date, state = $4, cancel_at_period_end = $5, superseded_at = $6
		WHERE tenant_id = $1 AND id = $2`, x.t, s.ID, s.PaidThrough, s.State, s.CancelAtPeriodEnd, s.SupersededAt); err != nil {
		return err
	}
	return x.syncLedgerDirectory(ctx)
}

func (x *pgTx) ReplaceEntitlements(ctx context.Context, subID ids.ID, keys []string, now time.Time) error {
	if _, err := x.tx.ExecContext(ctx, `UPDATE organisations SET entitlements_bumps = entitlements_bumps + 1 WHERE tenant_id = $1`, x.t); err != nil {
		return err
	}
	// A key the new version no longer carries is withdrawn (never deleted); a key it carries is (re)granted.
	if _, err := x.tx.ExecContext(ctx, `UPDATE entitlements SET withdrawn_at = $2 WHERE tenant_id = $1 AND withdrawn_at IS NULL`, x.t, now); err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := x.tx.ExecContext(ctx, `INSERT INTO entitlements (tenant_id, feature_key, subscription_id, granted_at) VALUES ($1,$2,$3,$4)
			ON CONFLICT (tenant_id, feature_key) DO UPDATE SET subscription_id = EXCLUDED.subscription_id, granted_at = EXCLUDED.granted_at, withdrawn_at = NULL`, x.t, k, subID, now); err != nil {
			return err
		}
	}
	return nil
}

func (x *pgTx) Entitlements(ctx context.Context) ([]string, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT feature_key FROM entitlements WHERE tenant_id = $1 AND withdrawn_at IS NULL ORDER BY feature_key`, x.t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (x *pgTx) EntitlementVersion(ctx context.Context) (int, error) {
	var n int
	err := x.tx.QueryRowContext(ctx, `SELECT 1 + entitlements_bumps FROM organisations WHERE tenant_id = $1`, x.t).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, model.ErrNotFound
	}
	return n, err
}

func (x *pgTx) CountOutlets(ctx context.Context) (int, error) {
	var n int
	//scopelint:internal the plan limit counts every outlet of the organisation
	err := x.tx.QueryRowContext(ctx, `SELECT count(*) FROM outlets WHERE tenant_id = $1`, x.t).Scan(&n)
	return n, err
}

// ---- charges and payments ----

const chargeCols = `id, subscription_id, kind, to_char(period_start, 'YYYY-MM-DD'), to_char(period_end, 'YYYY-MM-DD'), taxable_paise, cgst_paise, sgst_paise, igst_paise,
	total_paise, tolerance_paise, to_char(due_date, 'YYYY-MM-DD'), COALESCE(invoice_number, ''), COALESCE(to_char(invoice_date, 'YYYY-MM-DD'), ''), status,
	COALESCE(note, ''), COALESCE(reason, ''), created_by, created_at`

func scanCharge(row interface{ Scan(...any) error }) (model.Charge, error) {
	var c model.Charge
	err := row.Scan(&c.ID, &c.SubscriptionID, &c.Kind, &c.PeriodStart, &c.PeriodEnd, &c.TaxableP, &c.CGSTP, &c.SGSTP, &c.IGSTP, &c.TotalP, &c.TolerancePaise,
		&c.DueDate, &c.InvoiceNumber, &c.InvoiceDate, &c.Status, &c.Note, &c.Reason, &c.CreatedBy, &c.CreatedAt)
	c.CreatedAt = utc(c.CreatedAt)
	return c, err
}

func scanCharges(rows *sql.Rows, err error) ([]model.Charge, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Charge
	for rows.Next() {
		c, err := scanCharge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (x *pgTx) InsertCharge(ctx context.Context, c model.Charge) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO charges (tenant_id, id, subscription_id, kind, period_start, period_end, taxable_paise, cgst_paise, sgst_paise, igst_paise,
		total_paise, tolerance_paise, due_date, invoice_number, invoice_date, status, note, reason, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5::text::date,$6::text::date,$7,$8,$9,$10,$11,$12,$13::text::date,$14,NULLIF($15::text, '')::date,$16,$17,$18,$19,$20)`,
		x.t, c.ID, c.SubscriptionID, c.Kind, c.PeriodStart, c.PeriodEnd, c.TaxableP, c.CGSTP, c.SGSTP, c.IGSTP, c.TotalP, c.TolerancePaise, c.DueDate,
		nullStr(c.InvoiceNumber), c.InvoiceDate, c.Status, nullStr(c.Note), nullStr(c.Reason), c.CreatedBy, c.CreatedAt); err != nil {
		return err
	}
	return x.syncLedgerDirectory(ctx)
}

func (x *pgTx) GetCharge(ctx context.Context, id ids.ID) (model.Charge, error) {
	c, err := scanCharge(x.tx.QueryRowContext(ctx, `SELECT `+chargeCols+` FROM charges WHERE tenant_id = $1 AND id = $2`, x.t, id))
	return c, notFound(err)
}

func (x *pgTx) UpdateCharge(ctx context.Context, c model.Charge) error {
	if _, err := x.tx.ExecContext(ctx, `UPDATE charges SET status = $3, reason = $4, invoice_number = $5, invoice_date = NULLIF($6::text, '')::date
		WHERE tenant_id = $1 AND id = $2`, x.t, c.ID, c.Status, nullStr(c.Reason), nullStr(c.InvoiceNumber), c.InvoiceDate); err != nil {
		return err
	}
	return x.syncLedgerDirectory(ctx)
}

func (x *pgTx) ListCharges(ctx context.Context, status string, after ids.ID, limit int) ([]model.Charge, error) {
	return scanCharges(x.tx.QueryContext(ctx, `SELECT `+chargeCols+` FROM charges c
		WHERE tenant_id = $1 AND ($2::text = '' OR status = $2)
		  AND ($3::text = '' OR (period_start, id) < (SELECT period_start, id FROM charges WHERE tenant_id = $1 AND id = NULLIF($3::text, '')::uuid))
		ORDER BY period_start DESC, id DESC LIMIT $4`, x.t, status, after.String(), limit))
}

func (x *pgTx) LiveChargesOverlapping(ctx context.Context, start, end string) ([]model.Charge, error) {
	return scanCharges(x.tx.QueryContext(ctx, `SELECT `+chargeCols+` FROM charges WHERE tenant_id = $1 AND kind = 'period' AND status <> 'cancelled'
		AND period_start <= $3::text::date AND period_end >= $2::text::date ORDER BY id`, x.t, start, end))
}

const paymentCols = `id, charge_id, amount_paise, method, COALESCE(reference, ''), to_char(received_on, 'YYYY-MM-DD'), recorded_by, reversal_of, COALESCE(reason, ''), created_at`

func scanPayment(row interface{ Scan(...any) error }) (model.Payment, error) {
	var p model.Payment
	var rev sql.NullString
	err := row.Scan(&p.ID, &p.ChargeID, &p.AmountP, &p.Method, &p.Reference, &p.ReceivedOn, &p.RecordedBy, &rev, &p.Reason, &p.CreatedAt)
	p.ReversalOf, p.CreatedAt = ids.ID(rev.String), utc(p.CreatedAt)
	return p, err
}

func scanPayments(rows *sql.Rows, err error) ([]model.Payment, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (x *pgTx) InsertPayment(ctx context.Context, p model.Payment) error {
	var rev any
	if p.ReversalOf != "" {
		rev = p.ReversalOf
	}
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO charge_payments (tenant_id, id, charge_id, amount_paise, method, reference, received_on, recorded_by, reversal_of, reason, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::text::date,$8,$9,$10,$11)`,
		x.t, p.ID, p.ChargeID, p.AmountP, p.Method, nullStr(p.Reference), p.ReceivedOn, p.RecordedBy, rev, nullStr(p.Reason), p.CreatedAt); err != nil {
		return err
	}
	return x.syncLedgerDirectory(ctx)
}

func (x *pgTx) GetPayment(ctx context.Context, chargeID, id ids.ID) (model.Payment, error) {
	p, err := scanPayment(x.tx.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM charge_payments WHERE tenant_id = $1 AND charge_id = $2 AND id = $3`, x.t, chargeID, id))
	return p, notFound(err)
}

func (x *pgTx) ListPayments(ctx context.Context, chargeID, after ids.ID, limit int) ([]model.Payment, error) {
	return scanPayments(x.tx.QueryContext(ctx, `SELECT `+paymentCols+` FROM charge_payments WHERE tenant_id = $1 AND charge_id = $2 AND id > $3::uuid ORDER BY id LIMIT $4`,
		x.t, chargeID, afterID(after), limit))
}

func (x *pgTx) AllPayments(ctx context.Context) ([]model.Payment, error) {
	return scanPayments(x.tx.QueryContext(ctx, `SELECT `+paymentCols+` FROM charge_payments WHERE tenant_id = $1 ORDER BY id`, x.t))
}

func (x *pgTx) NetPaid(ctx context.Context) (map[ids.ID]int64, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT charge_id, sum(CASE WHEN reversal_of IS NULL THEN amount_paise ELSE -amount_paise END) FROM charge_payments WHERE tenant_id = $1 GROUP BY charge_id`, x.t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[ids.ID]int64{}
	for rows.Next() {
		var id ids.ID
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (x *pgTx) CountPaymentRef(ctx context.Context, reference string) (int, error) {
	var n int
	err := x.tx.QueryRowContext(ctx, `SELECT count(*) FROM charge_payments WHERE tenant_id = $1 AND reference = $2 AND reversal_of IS NULL`, x.t, reference).Scan(&n)
	return n, err
}

// syncLedgerDirectory rewrites this organisation's rows of the vendor-readable ledger directory in the
// transaction of the ledger change (ADR-0008 S2). It holds ids, plan keys, dates and amounts only.
func (x *pgTx) syncLedgerDirectory(ctx context.Context) error {
	// Charges are never deleted, so the rows are upserted: no DELETE is needed.
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO directory_charges (subject_tenant_id, charge_id, kind, status, taxable_paise, total_paise, outstanding_paise, tolerance_paise,
			due_date, period_start, period_end, invoice_number)
		SELECT c.tenant_id, c.id, c.kind, c.status, c.taxable_paise, c.total_paise,
		       CASE WHEN c.status = 'open' THEN GREATEST(c.total_paise - COALESCE(p.net, 0), 0) ELSE 0 END,
		       c.tolerance_paise, c.due_date, c.period_start, c.period_end, c.invoice_number
		  FROM charges c
		  LEFT JOIN (SELECT charge_id, sum(CASE WHEN reversal_of IS NULL THEN amount_paise ELSE -amount_paise END) AS net
		               FROM charge_payments WHERE tenant_id = $1 GROUP BY charge_id) p ON p.charge_id = c.id
		 WHERE c.tenant_id = $1
		ON CONFLICT (subject_tenant_id, charge_id) DO UPDATE SET status = EXCLUDED.status, outstanding_paise = EXCLUDED.outstanding_paise,
			tolerance_paise = EXCLUDED.tolerance_paise, invoice_number = EXCLUDED.invoice_number`, x.t); err != nil {
		return err
	}
	_, err := x.tx.ExecContext(ctx, `INSERT INTO directory_subscriptions (subject_tenant_id, subscription_id, plan_key, plan_version, cycle, agreed_paise, addons_paise, extra_outlets, state,
			cancel_at_period_end, start_date, paid_through, next_period_start, updated_at)
		SELECT s.tenant_id, s.id, s.plan_key, s.plan_version, s.cycle, s.agreed_paise,
		       COALESCE((SELECT sum((a->>'quantity')::bigint * (a->>'agreed_paise')::bigint) FROM jsonb_array_elements(s.addons) a), 0),
		       COALESCE((SELECT sum((a->>'quantity')::int) FROM jsonb_array_elements(s.addons) a WHERE a->>'name' = 'extra_outlet'), 0),
		       s.state, s.cancel_at_period_end, s.start_date, s.paid_through,
		       GREATEST(s.start_date, COALESCE((SELECT max(c.period_end) + 1 FROM charges c WHERE c.tenant_id = s.tenant_id AND c.kind = 'period' AND c.status <> 'cancelled'), s.start_date)),
		       now()
		  FROM subscriptions s WHERE s.tenant_id = $1 AND s.superseded_at IS NULL
		ON CONFLICT (subject_tenant_id) DO UPDATE SET subscription_id = EXCLUDED.subscription_id, plan_key = EXCLUDED.plan_key, plan_version = EXCLUDED.plan_version,
			cycle = EXCLUDED.cycle, agreed_paise = EXCLUDED.agreed_paise, addons_paise = EXCLUDED.addons_paise, extra_outlets = EXCLUDED.extra_outlets, state = EXCLUDED.state,
			cancel_at_period_end = EXCLUDED.cancel_at_period_end, start_date = EXCLUDED.start_date, paid_through = EXCLUDED.paid_through,
			next_period_start = EXCLUDED.next_period_start, updated_at = EXCLUDED.updated_at`, x.t)
	return err
}

// ---- links ----

func (x *pgTx) ReissueInvite(ctx context.Context, linkID ids.ID, hash []byte, expires, now time.Time) error {
	if _, err := x.tx.ExecContext(ctx, `UPDATE link_invitations SET used_at = $2, used_by = proposer_tenant_id WHERE link_id = $1 AND used_at IS NULL AND proposer_tenant_id = $3`, linkID, now, x.t); err != nil {
		return err
	}
	_, err := x.tx.ExecContext(ctx, `INSERT INTO link_invitations (code_hash, link_id, proposer_tenant_id, expires_at) VALUES ($1,$2,$3,$4)`, hash, linkID, x.t, expires)
	return err
}
