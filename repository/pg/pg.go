// Package pg is the Postgres implementation of repository.Repository. Every transaction is opened through
// go-auth's audited transaction (which uses go-common's tenancy helper), so the tenant context is set with
// set_config('app.tenant_id', $1, true) before any statement, and a vendor-authority request writes its
// vendor_access_log row in the same transaction. Every statement also carries an explicit tenant predicate.
// SQL is parameterised; nothing is built from request data.
package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/outbox"
	"github.com/hostryhq/go-common/tenancy"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

const nilUUID = "00000000-0000-0000-0000-000000000000"

// Repo is the Postgres repository. app runs as tenancy_app, vendor as tenancy_vendor.
type Repo struct {
	app    *sql.DB
	vendor *sql.DB
}

// New wraps the two pools. Both must have passed VerifyRuntimeRole.
func New(app, vendor *sql.DB) *Repo { return &Repo{app: app, vendor: vendor} }

// Open opens a pgx-backed pool with conservative limits.
func Open(dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

var _ repository.Repository = (*Repo)(nil)

// InTx implements repository.Repository.
func (r *Repo) InTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	return auth.WithAuditedTx(ctx, r.app, func(ctx context.Context, tx *sql.Tx) error {
		return fn(ctx, &pgTx{tx: tx, t: tc.TenantID})
	})
}

// Ping implements repository.Repository.
func (r *Repo) Ping(ctx context.Context) error {
	if err := r.app.PingContext(ctx); err != nil {
		return err
	}
	return r.vendor.PingContext(ctx)
}

// Directory implements repository.Repository.
func (r *Repo) Directory() repository.Directory { return dirRepo{r.vendor} }

// VerifyRuntimeRole refuses a database role that could defeat row-level security (TC-01): superuser,
// BYPASSRLS, or the owner of any table. It is run for every runtime pool at start-up.
func VerifyRuntimeRole(ctx context.Context, db *sql.DB) error {
	var name string
	var super, bypass bool
	if err := db.QueryRowContext(ctx, `SELECT current_user, r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).Scan(&name, &super, &bypass); err != nil {
		return err
	}
	if super || bypass {
		return errors.New("pg: runtime role " + name + " is superuser or BYPASSRLS; refusing to start")
	}
	var owned int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return errors.New("pg: runtime role " + name + " owns tables (the migrator role does); refusing to start")
	}
	return nil
}

func pgCode(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrNotFound
	}
	return err
}

func utc(t time.Time) time.Time { return t.UTC() }

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func afterID(a ids.ID) string {
	if a == "" {
		return nilUUID
	}
	return a.String()
}

func jsonIDs(in []ids.ID) string {
	if len(in) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func parseIDs(b []byte) []ids.ID {
	var out []ids.ID
	_ = json.Unmarshal(b, &out)
	return out
}

// pgTx is one tenant-scoped transaction.
type pgTx struct {
	tx         *sql.Tx
	t          ids.ID
	inviteLink ids.ID
}

// ---- organisation and settings ----

func (x *pgTx) InsertOrg(ctx context.Context, o model.Org, s model.Settings) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO organisations (tenant_id, name, status, data_placement, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		x.t, o.Name, o.Status, o.DataPlacement, o.CreatedAt, o.UpdatedAt); err != nil {
		return err
	}
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO organisation_settings (tenant_id, offline_window_days, idle_lock_minutes, updated_at) VALUES ($1,$2,$3,$4)`,
		x.t, s.OfflineWindowDays, s.IdleLockMinutes, s.UpdatedAt); err != nil {
		return err
	}
	_, err := x.tx.ExecContext(ctx, `INSERT INTO directory_organisations (subject_tenant_id, name, status, outlet_count, created_at, create_key_hash, create_fingerprint)
		VALUES ($1,$2,$3,0,$4,$5,$6)`, x.t, o.Name, o.Status, o.CreatedAt, nullStr(o.CreateKeyHash), nullStr(o.CreateFingerprint))
	if code, c := pgCode(err); code == "23505" && c == "directory_organisations_create_key" {
		return model.ErrCreateKeyTaken
	}
	return err
}

func (x *pgTx) GetOrg(ctx context.Context) (model.Org, error) {
	o := model.Org{ID: x.t}
	err := x.tx.QueryRowContext(ctx, `SELECT name, status, data_placement, created_at, updated_at FROM organisations WHERE tenant_id = $1`, x.t).
		Scan(&o.Name, &o.Status, &o.DataPlacement, &o.CreatedAt, &o.UpdatedAt)
	o.CreatedAt, o.UpdatedAt = utc(o.CreatedAt), utc(o.UpdatedAt)
	return o, notFound(err)
}

func (x *pgTx) UpdateOrgName(ctx context.Context, name string, now time.Time) (model.Org, error) {
	if _, err := x.tx.ExecContext(ctx, `UPDATE organisations SET name = $2, updated_at = $3 WHERE tenant_id = $1`, x.t, name, now); err != nil {
		return model.Org{}, err
	}
	if _, err := x.tx.ExecContext(ctx, `UPDATE directory_organisations SET name = $2 WHERE subject_tenant_id = $1`, x.t, name); err != nil {
		return model.Org{}, err
	}
	return x.GetOrg(ctx)
}

func (x *pgTx) GetSettings(ctx context.Context) (model.Settings, error) {
	var s model.Settings
	err := x.tx.QueryRowContext(ctx, `SELECT offline_window_days, idle_lock_minutes, updated_at FROM organisation_settings WHERE tenant_id = $1`, x.t).
		Scan(&s.OfflineWindowDays, &s.IdleLockMinutes, &s.UpdatedAt)
	s.UpdatedAt = utc(s.UpdatedAt)
	return s, notFound(err)
}

func (x *pgTx) PutSettings(ctx context.Context, s model.Settings) error {
	_, err := x.tx.ExecContext(ctx, `UPDATE organisation_settings SET offline_window_days = $2, idle_lock_minutes = $3, updated_at = $4 WHERE tenant_id = $1`,
		x.t, s.OfflineWindowDays, s.IdleLockMinutes, s.UpdatedAt)
	return err
}

// ---- brands ----

func (x *pgTx) InsertBrand(ctx context.Context, b model.Brand) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO brands (tenant_id, id, name, is_default, created_at) VALUES ($1,$2,$3,$4,$5)`, x.t, b.ID, b.Name, b.IsDefault, b.CreatedAt)
	if code, _ := pgCode(err); code == "23505" {
		return model.ErrIDConflict
	}
	return err
}

func scanBrand(row interface{ Scan(...any) error }) (model.Brand, error) {
	var b model.Brand
	err := row.Scan(&b.ID, &b.Name, &b.IsDefault, &b.CreatedAt)
	b.CreatedAt = utc(b.CreatedAt)
	return b, err
}

func (x *pgTx) GetBrand(ctx context.Context, id ids.ID) (model.Brand, error) {
	b, err := scanBrand(x.tx.QueryRowContext(ctx, `SELECT id, name, is_default, created_at FROM brands WHERE tenant_id = $1 AND id = $2`, x.t, id))
	return b, notFound(err)
}

func (x *pgTx) DefaultBrand(ctx context.Context) (model.Brand, error) {
	b, err := scanBrand(x.tx.QueryRowContext(ctx, `SELECT id, name, is_default, created_at FROM brands WHERE tenant_id = $1 AND is_default`, x.t))
	return b, notFound(err)
}

func (x *pgTx) ListBrands(ctx context.Context, after ids.ID, limit int) ([]model.Brand, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT id, name, is_default, created_at FROM brands WHERE tenant_id = $1 AND id > $2::uuid ORDER BY id LIMIT $3`, x.t, afterID(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Brand
	for rows.Next() {
		b, err := scanBrand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- outlets ----

const outletCols = `outlet_id, brand_id, name, kind, status, state_code, tax_registration_id, located_in_outlet_id, created_at`

func scanOutlet(row interface{ Scan(...any) error }) (model.Outlet, error) {
	var o model.Outlet
	var loc sql.NullString
	err := row.Scan(&o.ID, &o.BrandID, &o.Name, &o.Kind, &o.Status, &o.StateCode, &o.TaxRegistrationID, &loc, &o.CreatedAt)
	o.LocatedInOutletID, o.CreatedAt = ids.ID(loc.String), utc(o.CreatedAt)
	return o, err
}

func (x *pgTx) InsertOutlet(ctx context.Context, o model.Outlet) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO outlets (tenant_id, outlet_id, brand_id, name, kind, status, state_code, tax_registration_id, located_in_outlet_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, x.t, o.ID, o.BrandID, o.Name, o.Kind, o.Status, o.StateCode, o.TaxRegistrationID, nullStr(o.LocatedInOutletID.String()), o.CreatedAt); err != nil {
		if code, _ := pgCode(err); code == "23505" {
			return model.ErrIDConflict
		}
		return err
	}
	_, err := x.tx.ExecContext(ctx, `UPDATE directory_organisations SET outlet_count = outlet_count + 1 WHERE subject_tenant_id = $1`, x.t)
	return err
}

func (x *pgTx) GetOutlet(ctx context.Context, id ids.ID) (model.Outlet, error) {
	o, err := scanOutlet(x.tx.QueryRowContext(ctx, `SELECT `+outletCols+` FROM outlets WHERE tenant_id = $1 AND outlet_id = $2`, x.t, id))
	return o, notFound(err)
}

// scopeJSON renders an outlet scope for a query: NULL means every outlet of the tenant.
func scopeJSON(scope auth.OutletScope) any {
	if scope.All() {
		return nil
	}
	b, _ := json.Marshal(scope.IDs())
	return string(b)
}

func (x *pgTx) ListOutlets(ctx context.Context, scope auth.OutletScope, kind string, after ids.ID, limit int) ([]model.Outlet, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT `+outletCols+` FROM outlets
		WHERE tenant_id = $1 AND outlet_id > $2::uuid AND ($3::text = '' OR kind = $3)
		  AND ($4::jsonb IS NULL OR outlet_id IN (SELECT jsonb_array_elements_text($4::jsonb)::uuid))
		ORDER BY outlet_id LIMIT $5`, x.t, afterID(after), kind, scopeJSON(scope), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Outlet
	for rows.Next() {
		o, err := scanOutlet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (x *pgTx) UpdateOutlet(ctx context.Context, o model.Outlet) error {
	res, err := x.tx.ExecContext(ctx, `UPDATE outlets SET name = $3, status = $4 WHERE tenant_id = $1 AND outlet_id = $2`, x.t, o.ID, o.Name, o.Status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrNotFound
	}
	return nil
}

// ---- tax registrations ----

const regCols = `id, legal_name, regime, gstin_normalised, state_code, verification_state, verification_method, verified_at, verified_by, status, created_at`

func scanReg(row interface{ Scan(...any) error }) (model.Registration, error) {
	var r model.Registration
	var g, m, by sql.NullString
	var va sql.NullTime
	err := row.Scan(&r.ID, &r.LegalName, &r.Regime, &g, &r.StateCode, &r.Verification, &m, &va, &by, &r.Status, &r.CreatedAt)
	r.GSTIN, r.Method, r.VerifiedBy, r.VerifiedAt, r.CreatedAt = g.String, m.String, by.String, nullTime(va), utc(r.CreatedAt)
	return r, err
}

func mapRegErr(err error) error {
	code, c := pgCode(err)
	if code != "23505" {
		return err
	}
	switch c {
	case "tax_registrations_tenant_gstin":
		return model.ErrDuplicateGSTIN
	case "tax_registrations_verified_gstin":
		return model.ErrVerifiedElsewhere
	}
	return model.ErrIDConflict
}

func (x *pgTx) syncDirReg(ctx context.Context, r model.Registration) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO directory_registrations (subject_tenant_id, registration_id, gstin_normalised, legal_name, regime, state_code, verification_state, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (subject_tenant_id, registration_id) DO UPDATE SET gstin_normalised = EXCLUDED.gstin_normalised, legal_name = EXCLUDED.legal_name,
		  verification_state = EXCLUDED.verification_state, status = EXCLUDED.status`,
		x.t, r.ID, nullStr(r.GSTIN), r.LegalName, r.Regime, r.StateCode, r.Verification, r.Status)
	return err
}

func (x *pgTx) InsertRegistration(ctx context.Context, r model.Registration) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO tax_registrations (tenant_id, id, legal_name, regime, gstin_normalised, state_code, verification_state, verification_method, verified_at, verified_by, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, x.t, r.ID, r.LegalName, r.Regime, nullStr(r.GSTIN), r.StateCode, r.Verification,
		nullStr(r.Method), r.VerifiedAt, nullStr(r.VerifiedBy), r.Status, r.CreatedAt); err != nil {
		return mapRegErr(err)
	}
	return x.syncDirReg(ctx, r)
}

func (x *pgTx) GetRegistration(ctx context.Context, id ids.ID) (model.Registration, error) {
	r, err := scanReg(x.tx.QueryRowContext(ctx, `SELECT `+regCols+` FROM tax_registrations WHERE tenant_id = $1 AND id = $2`, x.t, id))
	return r, notFound(err)
}

func (x *pgTx) ListRegistrations(ctx context.Context, after ids.ID, limit int) ([]model.Registration, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT `+regCols+` FROM tax_registrations WHERE tenant_id = $1 AND id > $2::uuid ORDER BY id LIMIT $3`, x.t, afterID(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Registration
	for rows.Next() {
		r, err := scanReg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (x *pgTx) UpdateRegistration(ctx context.Context, r model.Registration) error {
	res, err := x.tx.ExecContext(ctx, `UPDATE tax_registrations SET legal_name = $3, gstin_normalised = $4, verification_state = $5, verification_method = $6,
		verified_at = $7, verified_by = $8, status = $9 WHERE tenant_id = $1 AND id = $2`,
		x.t, r.ID, r.LegalName, nullStr(r.GSTIN), r.Verification, nullStr(r.Method), r.VerifiedAt, nullStr(r.VerifiedBy), r.Status)
	if err != nil {
		return mapRegErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrNotFound
	}
	return x.syncDirReg(ctx, r)
}

// ---- document-series registry ----

const seriesCols = `id, tax_registration_id, gstin_normalised, code, code_index, doc_type, outlet_id, device_id, status, issued_at, closed_at, closed_reason`

func scanSeries(row interface{ Scan(...any) error }) (model.Series, error) {
	var s model.Series
	var g, cr sql.NullString
	var ca sql.NullTime
	err := row.Scan(&s.ID, &s.TaxRegistrationID, &g, &s.Code, &s.CodeIndex, &s.DocType, &s.OutletID, &s.DeviceID, &s.Status, &s.IssuedAt, &ca, &cr)
	s.GSTIN, s.ClosedReason, s.ClosedAt, s.IssuedAt = g.String, cr.String, nullTime(ca), utc(s.IssuedAt)
	return s, err
}

func (x *pgTx) querySeries(ctx context.Context, q string, args ...any) ([]model.Series, error) {
	rows, err := x.tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		s, err := scanSeries(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (x *pgTx) SeriesForDevice(ctx context.Context, outletID, deviceID ids.ID) ([]model.Series, error) {
	return x.querySeries(ctx, `SELECT `+seriesCols+` FROM document_series WHERE tenant_id = $1 AND outlet_id = $2 AND device_id = $3 ORDER BY id`, x.t, outletID, deviceID)
}

func (x *pgTx) ListSeries(ctx context.Context, scope auth.OutletScope, status string, after ids.ID, limit int) ([]model.Series, error) {
	return x.querySeries(ctx, `SELECT `+seriesCols+` FROM document_series
		WHERE tenant_id = $1 AND id > $2::uuid AND ($3::text = '' OR status = $3)
		  AND ($4::jsonb IS NULL OR outlet_id IN (SELECT jsonb_array_elements_text($4::jsonb)::uuid))
		ORDER BY id LIMIT $5`, x.t, afterID(after), status, scopeJSON(scope), limit)
}

func (x *pgTx) GetSeries(ctx context.Context, id ids.ID) (model.Series, error) {
	s, err := scanSeries(x.tx.QueryRowContext(ctx, `SELECT `+seriesCols+` FROM document_series WHERE tenant_id = $1 AND id = $2`, x.t, id))
	return s, notFound(err)
}

// InsertSeries uses a savepoint so a refused code (unique index) leaves the transaction usable and the
// allocator can jump and retry.
func (x *pgTx) InsertSeries(ctx context.Context, s model.Series) error {
	if _, err := x.tx.ExecContext(ctx, `SAVEPOINT series_insert`); err != nil {
		return err
	}
	_, err := x.tx.ExecContext(ctx, `INSERT INTO document_series (tenant_id, id, tax_registration_id, gstin_normalised, code, code_index, doc_type, outlet_id, device_id, status, issued_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, x.t, s.ID, s.TaxRegistrationID, nullStr(s.GSTIN), s.Code, s.CodeIndex, s.DocType, s.OutletID, s.DeviceID, s.Status, s.IssuedAt)
	if err != nil {
		if code, _ := pgCode(err); code == "23505" {
			if _, rerr := x.tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT series_insert`); rerr != nil {
				return rerr
			}
			return model.ErrSeriesCollision
		}
		return err
	}
	_, err = x.tx.ExecContext(ctx, `RELEASE SAVEPOINT series_insert`)
	return err
}

func (x *pgTx) UpdateSeries(ctx context.Context, s model.Series) error {
	_, err := x.tx.ExecContext(ctx, `UPDATE document_series SET status = $3, closed_at = $4, closed_reason = $5 WHERE tenant_id = $1 AND id = $2`,
		x.t, s.ID, s.Status, s.ClosedAt, nullStr(s.ClosedReason))
	return err
}

func (x *pgTx) CloseDeviceSeries(ctx context.Context, deviceID ids.ID, now time.Time) (int, int, error) {
	//scopelint:internal closing a device's series is addressed by device id; the device belongs to the tenant
	res, err := x.tx.ExecContext(ctx, `UPDATE document_series SET status = 'closed', closed_at = $3 WHERE tenant_id = $1 AND device_id = $2 AND status IN ('dormant', 'active')`, x.t, deviceID, now)
	if err != nil {
		return 0, 0, err
	}
	closed, _ := res.RowsAffected()
	var total int
	//scopelint:internal see above
	err = x.tx.QueryRowContext(ctx, `SELECT count(*) FROM document_series WHERE tenant_id = $1 AND device_id = $2`, x.t, deviceID).Scan(&total)
	return int(closed), total, err
}

func (x *pgTx) NextCodeIndex(ctx context.Context, reg ids.ID) (int, error) {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO series_allocators (tenant_id, tax_registration_id, next_index, updated_at) VALUES ($1,$2,0,now()) ON CONFLICT DO NOTHING`, x.t, reg); err != nil {
		return 0, err
	}
	var n int
	// FOR UPDATE serialises concurrent issuance for the registration: parallel activations get distinct codes.
	err := x.tx.QueryRowContext(ctx, `SELECT next_index FROM series_allocators WHERE tenant_id = $1 AND tax_registration_id = $2 FOR UPDATE`, x.t, reg).Scan(&n)
	return n, err
}

func (x *pgTx) SetCodeIndex(ctx context.Context, reg ids.ID, next int, now time.Time) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO series_allocators (tenant_id, tax_registration_id, next_index, updated_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, tax_registration_id) DO UPDATE SET next_index = GREATEST(series_allocators.next_index, EXCLUDED.next_index), updated_at = EXCLUDED.updated_at`,
		x.t, reg, next, now)
	return err
}

func (x *pgTx) CountIssued(ctx context.Context, reg ids.ID, from, to time.Time) (int, error) {
	var n int
	//scopelint:internal the daily cap counts per registration across every outlet
	err := x.tx.QueryRowContext(ctx, `SELECT count(*) FROM document_series WHERE tenant_id = $1 AND tax_registration_id = $2 AND issued_at >= $3 AND issued_at < $4`, x.t, reg, from, to).Scan(&n)
	return n, err
}

func (x *pgTx) MaxCodeIndexForGSTIN(ctx context.Context, gstin string) (int, error) {
	var n int
	//scopelint:internal the allocator floor reads every outlet's codes under one GSTIN
	err := x.tx.QueryRowContext(ctx, `SELECT COALESCE(max(code_index), -1) FROM document_series WHERE tenant_id = $1 AND gstin_normalised = $2`, x.t, gstin).Scan(&n)
	return n, err
}

func (x *pgTx) ForceAdvanceAllocators(ctx context.Context, n int, now time.Time) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO series_allocators (tenant_id, tax_registration_id, next_index, updated_at)
		SELECT tenant_id, id, 0, $2 FROM tax_registrations WHERE tenant_id = $1 ON CONFLICT DO NOTHING`, x.t, now); err != nil {
		return err
	}
	_, err := x.tx.ExecContext(ctx, `UPDATE series_allocators SET next_index = next_index + $2, updated_at = $3 WHERE tenant_id = $1`, x.t, n, now)
	return err
}

// ---- idempotency ----

// IdemGet claims the key for this transaction when it is new. A concurrent transaction holding the same
// key makes the insert wait until it commits, so a replay always sees the finished response.
func (x *pgTx) IdemGet(ctx context.Context, op, key string) (string, []byte, bool, error) {
	res, err := x.tx.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id, op, key, fingerprint, created_at) VALUES ($1,$2,$3,'',now()) ON CONFLICT DO NOTHING`, x.t, op, key)
	if err != nil {
		return "", nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return "", nil, false, nil
	}
	var fp string
	var resp []byte
	err = x.tx.QueryRowContext(ctx, `SELECT fingerprint, response FROM idempotency_keys WHERE tenant_id = $1 AND op = $2 AND key = $3`, x.t, op, key).Scan(&fp, &resp)
	return fp, resp, err == nil, err
}

func (x *pgTx) IdemPut(ctx context.Context, op, key, fp string, resp []byte, _ time.Time) error {
	_, err := x.tx.ExecContext(ctx, `UPDATE idempotency_keys SET fingerprint = $4, response = $5 WHERE tenant_id = $1 AND op = $2 AND key = $3`, x.t, op, key, fp, resp)
	return err
}

// ---- events ----

func (x *pgTx) Publish(ctx context.Context, e events.Envelope) error {
	var seq int64
	if err := x.tx.QueryRowContext(ctx, `INSERT INTO event_keys (tenant_id, key, last_seq) VALUES ($1,$2,1)
		ON CONFLICT (tenant_id, key) DO UPDATE SET last_seq = event_keys.last_seq + 1 RETURNING last_seq`, x.t, e.Key).Scan(&seq); err != nil {
		return err
	}
	e.KeySeq = seq
	return outbox.Publish(ctx, x.tx, e)
}
