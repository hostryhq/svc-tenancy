package pg

import (
	"context"
	"database/sql"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
)

// The staff work lists are derived from the directory ledger rows (ADR-0008 S2): each inner query yields
// (organisation, name, subject id, sort date, has date, age in days, amount) and $1 is today (IST). Nothing is
// scheduled and nothing is sent (BR-A1-50). The statements are constants: SQL is never built from data.
const (
	listHead = `SELECT q.org, q.name, q.subject, CASE WHEN q.has_due THEN to_char(q.sort_due, 'YYYY-MM-DD') ELSE '' END, q.age, q.amount FROM (`
	listTail = `) AS q(org, name, subject, sort_due, has_due, age, amount)
		WHERE $2::text = '' OR (q.sort_due, q.subject) > (COALESCE(NULLIF($3::text, '')::date, DATE '0001-01-01'), NULLIF($2::text, '')::uuid)
		ORDER BY q.sort_due, q.subject LIMIT $4`

	// outstanding above the tolerance and due before today
	listOverdue = `SELECT c.subject_tenant_id, o.name, c.charge_id, c.due_date, true, ($1::text::date - c.due_date), c.outstanding_paise
		FROM directory_charges c JOIN directory_organisations o ON o.subject_tenant_id = c.subject_tenant_id
		WHERE c.status = 'open' AND c.outstanding_paise > c.tolerance_paise AND c.due_date < $1::text::date`
	// the next period starts within 7 days (or already has): no live charge covers it
	listToRaise = `SELECT s.subject_tenant_id, o.name, s.subscription_id, s.next_period_start, true, NULL::int, s.agreed_paise + s.addons_paise
		FROM directory_subscriptions s JOIN directory_organisations o ON o.subject_tenant_id = s.subject_tenant_id
		WHERE s.state = 'active' AND s.cycle <> 'none' AND NOT (s.cancel_at_period_end AND s.next_period_start > s.paid_through)
		  AND s.next_period_start <= $1::text::date + 7`
	listNoInvoice = `SELECT c.subject_tenant_id, o.name, c.charge_id, c.due_date, true, NULL::int, c.total_paise
		FROM directory_charges c JOIN directory_organisations o ON o.subject_tenant_id = c.subject_tenant_id
		WHERE c.status = 'open' AND c.invoice_number IS NULL AND $1::text <> ''`
	listPilots = `SELECT s.subject_tenant_id, o.name, s.subscription_id, s.paid_through, true, NULL::int, NULL::bigint
		FROM directory_subscriptions s JOIN directory_organisations o ON o.subject_tenant_id = s.subject_tenant_id
		WHERE s.state = 'pilot' AND NOT s.cancel_at_period_end AND s.paid_through <= $1::text::date + 14`
	listRetired = `SELECT s.subject_tenant_id, o.name, s.subscription_id, DATE '0001-01-01', false, NULL::int, NULL::bigint
		FROM directory_subscriptions s JOIN directory_organisations o ON o.subject_tenant_id = s.subject_tenant_id
		JOIN plan_versions v ON v.plan_key = s.plan_key AND v.version = s.plan_version
		WHERE s.state <> 'cancelled' AND NOT (s.cancel_at_period_end AND s.paid_through < $1::text::date) AND v.state = 'retired'`
	listOverLimit = `SELECT s.subject_tenant_id, o.name, s.subscription_id, DATE '0001-01-01', false, NULL::int, NULL::bigint
		FROM directory_subscriptions s JOIN directory_organisations o ON o.subject_tenant_id = s.subject_tenant_id
		JOIN plan_versions v ON v.plan_key = s.plan_key AND v.version = s.plan_version
		WHERE s.state <> 'cancelled' AND NOT (s.cancel_at_period_end AND s.paid_through < $1::text::date)
		  AND o.outlet_count > v.included_outlets + s.extra_outlets`
)

func (d dirRepo) LedgerList(ctx context.Context, kind, today, afterDue string, after ids.ID, limit int) ([]model.LedgerRow, error) {
	var rows *sql.Rows
	var err error
	cursor := after.String()
	switch kind {
	case "overdue_charges":
		rows, err = d.db.QueryContext(ctx, listHead+listOverdue+listTail, today, cursor, afterDue, limit)
	case "charges_to_raise":
		rows, err = d.db.QueryContext(ctx, listHead+listToRaise+listTail, today, cursor, afterDue, limit)
	case "charges_without_invoice_reference":
		rows, err = d.db.QueryContext(ctx, listHead+listNoInvoice+listTail, today, cursor, afterDue, limit)
	case "pilots_ending":
		rows, err = d.db.QueryContext(ctx, listHead+listPilots+listTail, today, cursor, afterDue, limit)
	case "retired_version_subscriptions":
		rows, err = d.db.QueryContext(ctx, listHead+listRetired+listTail, today, cursor, afterDue, limit)
	case "outlets_over_limit":
		rows, err = d.db.QueryContext(ctx, listHead+listOverLimit+listTail, today, cursor, afterDue, limit)
	default:
		return nil, apperr.New(apperr.NotFound, "not_found", "")
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.LedgerRow
	for rows.Next() {
		var r model.LedgerRow
		var age, amt sql.NullInt64
		if err := rows.Scan(&r.OrganisationID, &r.OrganisationName, &r.SubjectID, &r.DueDate, &age, &amt); err != nil {
			return nil, err
		}
		if age.Valid {
			n := int(age.Int64)
			r.AgeDays = &n
		}
		if amt.Valid {
			r.AmountP = &amt.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d dirRepo) LedgerExport(ctx context.Context, from, to string) ([]model.ExportRow, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT c.subject_tenant_id, o.name, to_char(c.period_start, 'YYYY-MM-DD'), to_char(c.period_end, 'YYYY-MM-DD'), c.taxable_paise,
			c.total_paise, to_char(c.due_date, 'YYYY-MM-DD'), COALESCE(c.invoice_number, ''),
			CASE WHEN c.status = 'open' AND c.outstanding_paise <= c.tolerance_paise THEN 'paid' ELSE c.status END, c.outstanding_paise
		FROM directory_charges c JOIN directory_organisations o ON o.subject_tenant_id = c.subject_tenant_id
		WHERE ($1::text = '' OR c.due_date >= $1::text::date) AND ($2::text = '' OR c.due_date <= $2::text::date)
		ORDER BY c.due_date, c.charge_id`, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.ExportRow
	for rows.Next() {
		var r model.ExportRow
		if err := rows.Scan(&r.OrganisationID, &r.OrganisationName, &r.PeriodStart, &r.PeriodEnd, &r.TaxableP, &r.TotalP, &r.DueDate, &r.InvoiceNumber, &r.Status, &r.OutstandingP); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
