package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/gstin"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// dirRepo reads the vendor directory through the tenancy_vendor pool: a role that can select the two
// directory tables and nothing else (ADR-0013 V3). It never sets a tenant context.
type dirRepo struct{ db *sql.DB }

var _ repository.Directory = dirRepo{}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (d dirRepo) List(ctx context.Context, f repository.DirectoryFilter) ([]model.DirOrg, error) {
	var after any
	if f.After != "" {
		after = f.After.String()
	}
	var want any
	if len(f.OrganisationIDs) > 0 {
		b, _ := json.Marshal(f.OrganisationIDs)
		want = string(b)
	}
	// A partial GSTIN never matches: only the full normalised string is compared, and only for equality.
	rows, err := d.db.QueryContext(ctx, `SELECT o.subject_tenant_id, o.name, o.status, o.outlet_count, o.created_at
		FROM directory_organisations o
		WHERE ($1::uuid IS NULL OR o.subject_tenant_id < $1::uuid)
		  AND ($2::text = '' OR o.status = $2)
		  AND ($3::jsonb IS NULL OR o.subject_tenant_id IN (SELECT jsonb_array_elements_text($3::jsonb)::uuid))
		  AND ($4::text = ''
		       OR lower(o.name) LIKE '%' || lower($5) || '%' ESCAPE '\'
		       OR o.subject_tenant_id::text = lower($4)
		       OR EXISTS (SELECT 1 FROM directory_registrations r WHERE r.subject_tenant_id = o.subject_tenant_id AND r.gstin_normalised = $6))
		ORDER BY o.subject_tenant_id DESC LIMIT $7`, after, f.Status, want, f.Q, likeEscape(f.Q), gstin.Normalise(f.Q), f.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.DirOrg
	for rows.Next() {
		var o model.DirOrg
		if err := rows.Scan(&o.ID, &o.Name, &o.Status, &o.OutletCount, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.CreatedAt = utc(o.CreatedAt)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d dirRepo) FindCreation(ctx context.Context, keyHash string) (ids.ID, string, bool, error) {
	var id ids.ID
	var fp sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT subject_tenant_id, create_fingerprint FROM directory_organisations WHERE create_key_hash = $1`, keyHash).Scan(&id, &fp)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return id, fp.String, err == nil, err
}

func (d dirRepo) Holders(ctx context.Context, g string) ([]model.DirHolder, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT subject_tenant_id, registration_id, verification_state = 'verified', status = 'closed'
		FROM directory_registrations WHERE gstin_normalised = $1`, g)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.DirHolder
	for rows.Next() {
		var h model.DirHolder
		if err := rows.Scan(&h.TenantID, &h.RegistrationID, &h.Verified, &h.Closed); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (d dirRepo) TenantIDs(ctx context.Context, after ids.ID, limit int) ([]ids.ID, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT subject_tenant_id FROM directory_organisations WHERE subject_tenant_id > $1::uuid ORDER BY subject_tenant_id LIMIT $2`, afterID(after), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ids.ID
	for rows.Next() {
		var id ids.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
