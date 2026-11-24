package pg

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
)

// Links are two-party rows: visible to either party under row-level security. An acceptor who is not yet
// a party sees the one link whose invitation code it presents (FindLinkByInvite sets app.invite_hash for
// the transaction). Every statement also names the caller's tenant explicitly.

const linkCols = `id, party_a_tenant_id, party_b_tenant_id, template, status, proposer_outlets, acceptor_outlets, proposer_party, acceptor_party,
	suspended_by, proposed_at, expires_at, accepted_at, confirmed_at`

func scanLink(row interface{ Scan(...any) error }) (model.Link, error) {
	var l model.Link
	var b, susp sql.NullString
	var po, ao, pp, ap []byte
	var acc, conf sql.NullTime
	err := row.Scan(&l.ID, &l.ProposerID, &b, &l.Template, &l.Status, &po, &ao, &pp, &ap, &susp, &l.ProposedAt, &l.ExpiresAt, &acc, &conf)
	if err != nil {
		return l, err
	}
	l.AcceptorID, l.SuspendedBy = ids.ID(b.String), ids.ID(susp.String)
	l.ProposerOutlets, l.AcceptorOutlets = parseIDs(po), parseIDs(ao)
	_ = json.Unmarshal(pp, &l.ProposerParty)
	if len(ap) > 0 {
		_ = json.Unmarshal(ap, &l.AcceptorParty)
	}
	l.ProposedAt, l.ExpiresAt, l.AcceptedAt, l.ConfirmedAt = utc(l.ProposedAt), utc(l.ExpiresAt), nullTime(acc), nullTime(conf)
	return l, nil
}

func (x *pgTx) loadGrants(ctx context.Context, l *model.Link) error {
	rows, err := x.tx.QueryContext(ctx, `SELECT id, scope, grantor_side, outlet_ids, status FROM link_grants WHERE link_id = $1 ORDER BY id`, l.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	l.Grants = nil
	for rows.Next() {
		g := model.Grant{LinkID: l.ID}
		var oi []byte
		if err := rows.Scan(&g.ID, &g.Scope, &g.GrantorSide, &oi, &g.Status); err != nil {
			return err
		}
		g.OutletIDs = parseIDs(oi)
		l.Grants = append(l.Grants, g)
	}
	return rows.Err()
}

func (x *pgTx) visibleLinkID() string {
	if x.inviteLink == "" {
		return nilUUID
	}
	return x.inviteLink.String()
}

func (x *pgTx) InsertLink(ctx context.Context, l model.Link, hash []byte, expires time.Time) error {
	pp, _ := json.Marshal(l.ProposerParty)
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO links (id, party_a_tenant_id, template, status, proposer_outlets, proposer_party, proposed_at, expires_at)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8)`, l.ID, x.t, l.Template, l.Status, jsonIDs(l.ProposerOutlets), string(pp), l.ProposedAt, l.ExpiresAt); err != nil {
		if code, _ := pgCode(err); code == "23505" {
			return model.ErrIDConflict
		}
		return err
	}
	for _, g := range l.Grants {
		if _, err := x.tx.ExecContext(ctx, `INSERT INTO link_grants (id, link_id, scope, grantor_side, outlet_ids, status) VALUES ($1,$2,$3,$4,$5::jsonb,$6)`,
			g.ID, l.ID, g.Scope, g.GrantorSide, jsonIDs(g.OutletIDs), g.Status); err != nil {
			return err
		}
	}
	_, err := x.tx.ExecContext(ctx, `INSERT INTO link_invitations (code_hash, link_id, proposer_tenant_id, expires_at) VALUES ($1,$2,$3,$4)`, hash, l.ID, x.t, expires)
	return err
}

func (x *pgTx) GetLink(ctx context.Context, id ids.ID, forUpdate bool) (model.Link, error) {
	q := `SELECT ` + linkCols + ` FROM links WHERE id = $1 AND (party_a_tenant_id = $2 OR party_b_tenant_id = $2 OR id = $3::uuid)`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	l, err := scanLink(x.tx.QueryRowContext(ctx, q, id, x.t, x.visibleLinkID()))
	if err != nil {
		return l, notFound(err)
	}
	return l, x.loadGrants(ctx, &l)
}

func (x *pgTx) FindLinkByInvite(ctx context.Context, hash []byte) (ids.ID, error) {
	if _, err := x.tx.ExecContext(ctx, `SELECT set_config('app.invite_hash', $1, true)`, hex.EncodeToString(hash)); err != nil {
		return "", err
	}
	var id ids.ID
	err := x.tx.QueryRowContext(ctx, `SELECT link_id FROM link_invitations WHERE code_hash = $1 AND used_at IS NULL`, hash).Scan(&id)
	if err != nil {
		return "", notFound(err)
	}
	x.inviteLink = id
	return id, nil
}

func (x *pgTx) UseInvite(ctx context.Context, hash []byte, now time.Time) (bool, error) {
	res, err := x.tx.ExecContext(ctx, `UPDATE link_invitations SET used_at = $2 WHERE code_hash = $1 AND used_at IS NULL`, hash, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (x *pgTx) ListLinks(ctx context.Context, status string, after ids.ID, limit int) ([]model.Link, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT `+linkCols+` FROM links
		WHERE (party_a_tenant_id = $1 OR party_b_tenant_id = $1) AND id > $2::uuid AND ($3::text = '' OR status = $3) ORDER BY id LIMIT $4`,
		x.t, afterID(after), status, limit)
	if err != nil {
		return nil, err
	}
	var out []model.Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := x.loadGrants(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (x *pgTx) SaveLink(ctx context.Context, l model.Link) error {
	ap, _ := json.Marshal(l.AcceptorParty)
	res, err := x.tx.ExecContext(ctx, `UPDATE links SET status = $2, party_b_tenant_id = $3, acceptor_outlets = $4::jsonb, acceptor_party = $5::jsonb,
		suspended_by = $6, accepted_at = $7, confirmed_at = $8
		WHERE id = $1 AND (party_a_tenant_id = $9 OR party_b_tenant_id = $9 OR id = $10::uuid)`,
		l.ID, l.Status, nullStr(l.AcceptorID.String()), jsonIDs(l.AcceptorOutlets), nullJSON(l, string(ap)), nullStr(l.SuspendedBy.String()), l.AcceptedAt, l.ConfirmedAt, x.t, x.visibleLinkID())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrNotFound
	}
	for _, g := range l.Grants {
		if _, err := x.tx.ExecContext(ctx, `INSERT INTO link_grants (id, link_id, scope, grantor_side, outlet_ids, status) VALUES ($1,$2,$3,$4,$5::jsonb,$6)
			ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status`, g.ID, l.ID, g.Scope, g.GrantorSide, jsonIDs(g.OutletIDs), g.Status); err != nil {
			return err
		}
	}
	return nil
}

// nullJSON keeps acceptor_party NULL until the link has an acceptor.
func nullJSON(l model.Link, s string) any {
	if l.AcceptorID == "" {
		return nil
	}
	return s
}

func (x *pgTx) InsertAmendment(ctx context.Context, a model.Amendment) error {
	g, _ := json.Marshal(a.Add)
	_, err := x.tx.ExecContext(ctx, `INSERT INTO link_amendments (id, link_id, proposed_by_side, grants, status, created_at) VALUES ($1,$2,$3,$4::jsonb,$5,$6)`,
		a.ID, a.LinkID, a.ProposedBySide, string(g), a.Status, a.CreatedAt)
	return err
}

func (x *pgTx) GetAmendment(ctx context.Context, linkID, id ids.ID) (model.Amendment, error) {
	a := model.Amendment{ID: id, LinkID: linkID}
	var g []byte
	err := x.tx.QueryRowContext(ctx, `SELECT proposed_by_side, grants, status, created_at FROM link_amendments WHERE link_id = $1 AND id = $2`, linkID, id).
		Scan(&a.ProposedBySide, &g, &a.Status, &a.CreatedAt)
	if err != nil {
		return a, notFound(err)
	}
	_ = json.Unmarshal(g, &a.Add)
	a.CreatedAt = utc(a.CreatedAt)
	return a, nil
}

func (x *pgTx) SaveAmendment(ctx context.Context, a model.Amendment) error {
	_, err := x.tx.ExecContext(ctx, `UPDATE link_amendments SET status = $3 WHERE link_id = $1 AND id = $2`, a.LinkID, a.ID, a.Status)
	return err
}

func (x *pgTx) AppendActivity(ctx context.Context, a model.Activity) error {
	_, err := x.tx.ExecContext(ctx, `INSERT INTO link_activity (id, link_id, occurred_at, side, actor_id, action, scope, object_ref, outcome) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		a.ID, a.LinkID, a.OccurredAt, a.Side, a.ActorID, a.Action, nullStr(a.Scope), nullStr(a.ObjectRef), a.Outcome)
	return err
}

func (x *pgTx) ListActivity(ctx context.Context, linkID, after ids.ID, limit int) ([]model.Activity, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT id, link_id, occurred_at, side, actor_id, action, scope, object_ref, outcome FROM link_activity
		WHERE link_id = $1 AND id > $2::uuid ORDER BY id LIMIT $3`, linkID, afterID(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Activity
	for rows.Next() {
		var a model.Activity
		var sc, ref sql.NullString
		if err := rows.Scan(&a.ID, &a.LinkID, &a.OccurredAt, &a.Side, &a.ActorID, &a.Action, &sc, &ref, &a.Outcome); err != nil {
			return nil, err
		}
		a.Scope, a.ObjectRef, a.OccurredAt = sc.String, ref.String, utc(a.OccurredAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (x *pgTx) ExpireDueLinks(ctx context.Context, now time.Time) ([]model.Link, error) {
	rows, err := x.tx.QueryContext(ctx, `UPDATE links SET status = 'expired' WHERE (party_a_tenant_id = $1 OR party_b_tenant_id = $1)
		AND status IN ('proposed', 'accepted') AND expires_at <= $2 RETURNING id`, x.t, now)
	if err != nil {
		return nil, err
	}
	var due []ids.ID
	for rows.Next() {
		var id ids.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, id)
	}
	rows.Close()
	var out []model.Link
	for _, id := range due {
		l, err := x.GetLink(ctx, id, false)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}
