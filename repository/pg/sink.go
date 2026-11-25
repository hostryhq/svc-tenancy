package pg

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/outbox"
	"github.com/hostryhq/go-common/tenancy"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/auditsink"
)

// AuditSink writes the vendor_access_log row and its tenancy.vendor_access.recorded outbox event in the
// transaction of the action (ADR-0013 V5). If the row cannot be written the request fails and nothing
// changes. A refused request is recorded in a short transaction of its own.
type AuditSink struct {
	DB  *sql.DB // the application pool
	Gen *ids.Generator
	Log *slog.Logger
}

var _ auth.VendorAuditSink = (*AuditSink)(nil)

func tokenType(t auth.TokenType) string {
	if t == auth.TypeSupport {
		return "support"
	}
	return "vendor"
}

func (s *AuditSink) event(ctx context.Context, e auth.VendorAccessEntry, tenant ids.ID) events.Envelope {
	ap := auditsink.ApprovalFrom(ctx)
	outcome := "succeeded"
	if e.Outcome == auth.OutcomeRefused {
		outcome = "refused"
	}
	tt := tokenType(e.TokenType)
	env := tev.VendorAccessRecorded(s.Gen.New(), e.Time, tenant, tev.VendorAccess{EntryID: e.ID, VendorUserID: e.VendorUser, TokenType: tt,
		SupportSessionID: e.SupportSession, ActionKey: auditsink.ActionKey(e.ActionKey, tt), TargetID: e.TargetID, TicketReference: e.Ticket,
		ApprovalID: ap.ApprovalID, GrantJTI: ap.GrantJTI, Outcome: outcome, ResultCount: e.ResultCount, RequestID: e.RequestID})
	env.KeySeq = 1 // one event per entry id
	return env
}

func insertEntry(ctx context.Context, tx *sql.Tx, tenant ids.ID, e auth.VendorAccessEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO vendor_access_log (tenant_id, id, occurred_at, vendor_user, token_type, support_session_id, action_key, target_id, ticket_reference, outcome, result_count, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, tenant, e.ID, e.Time, e.VendorUser, tokenType(e.TokenType), nullStr(e.SupportSession),
		auditsink.ActionKey(e.ActionKey, tokenType(e.TokenType)), nullStr(e.TargetID), nullStr(e.Ticket), e.Outcome, e.ResultCount, e.RequestID)
	return err
}

// RecordInTx implements auth.VendorAuditSink.
func (s *AuditSink) RecordInTx(ctx context.Context, x tenancy.Execer, e auth.VendorAccessEntry) error {
	tx, ok := x.(*sql.Tx)
	if !ok {
		return errors.New("pg: audit sink needs a *sql.Tx")
	}
	tid, err := ids.Parse(e.TenantID)
	if err != nil {
		return err
	}
	if err := insertEntry(ctx, tx, tid, e); err != nil {
		return err
	}
	return outbox.Publish(ctx, tx, s.event(ctx, e, tid))
}

// RecordRefused implements auth.VendorAuditSink.
func (s *AuditSink) RecordRefused(ctx context.Context, e auth.VendorAccessEntry) error {
	tid, err := ids.Parse(e.TenantID)
	if err != nil {
		// A vendor-realm refusal before an organisation is known has no organisation to attach the row to.
		s.Log.WarnContext(ctx, "vendor request refused before an organisation was named", "action", e.ActionKey, "vendor_user", e.VendorUser)
		return nil
	}
	e.Outcome = auth.OutcomeRefused
	c := tenancy.With(context.WithoutCancel(ctx), tenancy.Context{TenantID: tid, ActorID: e.VendorUser})
	return tenancy.WithTx(c, s.DB, func(c context.Context, tx *sql.Tx) error {
		if err := insertEntry(c, tx, tid, e); err != nil {
			return err
		}
		return outbox.Publish(c, tx, s.event(c, e, tid))
	})
}

// PlatformAuditSink writes platform_vendor_access_log rows: vendor actions that name no organisation
// (ADR-0013 V5 item 3). The table has no tenant column and no row-level security; the row commits in the
// transaction of the action, and a refused request is recorded in a short transaction of its own.
type PlatformAuditSink struct {
	DB *sql.DB // the application pool
}

var _ auth.PlatformAuditSink = (*PlatformAuditSink)(nil)

func insertPlatformEntry(ctx context.Context, x tenancy.Execer, e auth.VendorAccessEntry) error {
	_, err := x.ExecContext(ctx, `INSERT INTO platform_vendor_access_log (id, occurred_at, vendor_user, token_type, support_session_id, action_key, target_id, ticket_reference, outcome, result_count, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, e.ID, e.Time, e.VendorUser, tokenType(e.TokenType), nullStr(e.SupportSession),
		auditsink.ActionKey(e.ActionKey, tokenType(e.TokenType)), nullStr(e.TargetID), nullStr(e.Ticket), e.Outcome, e.ResultCount, e.RequestID)
	return err
}

// RecordPlatformInTx implements auth.PlatformAuditSink.
func (s *PlatformAuditSink) RecordPlatformInTx(ctx context.Context, x tenancy.Execer, e auth.VendorAccessEntry) error {
	return insertPlatformEntry(ctx, x, e)
}

// RecordPlatformRefused implements auth.PlatformAuditSink.
func (s *PlatformAuditSink) RecordPlatformRefused(ctx context.Context, e auth.VendorAccessEntry) error {
	e.Outcome = auth.OutcomeRefused
	tx, err := s.DB.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	if err := insertPlatformEntry(ctx, tx, e); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
