package auditsink

import (
	"context"
	"sync"
)

// Approval names the vendor approval an action executed (ADR-0005 A3.4 item 2). The executor notes it after
// the grant verifies; the vendor-access sink reads it when it writes the row in the same transaction, so
// tenancy.vendor_access.recorded carries approval_id and grant_jti for org_suspend, org_reinstate and org_close.
type Approval struct {
	ApprovalID string
	GrantJTI   string
}

type noteKey struct{}

type note struct {
	mu sync.Mutex
	a  Approval
}

// WithApprovalNote returns a context that can carry one Approval from the executor to the audit sink.
func WithApprovalNote(ctx context.Context) context.Context {
	return context.WithValue(ctx, noteKey{}, &note{})
}

// NoteApproval records the executed approval; a no-op when the context has no note.
func NoteApproval(ctx context.Context, approvalID, jti string) {
	if n, ok := ctx.Value(noteKey{}).(*note); ok {
		n.mu.Lock()
		n.a = Approval{ApprovalID: approvalID, GrantJTI: jti}
		n.mu.Unlock()
	}
}

// ApprovalFrom returns the noted approval, or the zero value.
func ApprovalFrom(ctx context.Context) Approval {
	if n, ok := ctx.Value(noteKey{}).(*note); ok {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.a
	}
	return Approval{}
}
