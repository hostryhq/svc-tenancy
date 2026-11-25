// Package httpsuite holds HTTP-level suites that run against the real service on Postgres and against
// the in-memory test double alike: tenant isolation (T2, TST-08, TST-09, TST-10), outlet scoping (T14),
// realm and authorisation checks (T12, TST-14, TST-19, TST-27), the GSTIN rules (T15), vendor onboarding,
// the directory and vendor audit (AD3 to AD6). A suite that passes for both keeps the double honest.
package httpsuite

import (
	"net/http"
	"testing"
	"time"

	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// AuditRow is a vendor_access_log row, as far as the suites check it.
type AuditRow struct {
	ActionKey, Outcome, TargetID, TokenType string
}

// Target is a freshly started service with no data.
type Target struct {
	Base string
	*double.Tokens
	// Seed stores fixture organisations.
	Seed func(t testing.TB, worlds ...seed.World)
	// Events returns every event in the outbox, in order.
	Events func(t testing.TB) []events.Envelope
	// AuditLog returns the vendor_access_log rows of an organisation.
	AuditLog func(t testing.TB, tenant ids.ID) []AuditRow
	// PlatformAuditLog returns the platform_vendor_access_log rows: vendor actions that name no organisation.
	PlatformAuditLog func(t testing.TB) []AuditRow
	// SetAuditWritable makes the vendor access log writable or not, to prove fail-closed behaviour.
	SetAuditWritable func(t testing.TB, ok bool)
	// CloseRegistration closes a registration directly in the store, to model a GSTIN that comes back.
	CloseRegistration func(t testing.TB, tenant, registration ids.ID)
	// LinksEnabled reports whether the link routes were registered.
	LinksEnabled bool
	// Deliver applies an event from another service (billing.trading_day.changed, billing.series.reconciled), as
	// the bus consumer does.
	Deliver func(t testing.TB, e events.Envelope)
	// SetNow moves the clock of the service's entities (charge dates, "today" in IST).
	SetNow func(t time.Time)
	// Now is the instant the service's grant verifier judges grants at.
	Now func() time.Time
}

// vendorUser is the subject of the vendor tokens the suites mint: a UUID, because payments and charges record it.
const vendorUser = double.ExampleApprover

// Factory starts a new Target; links says whether the link routes are on.
type Factory func(t *testing.T, links bool) *Target

// Call sends a request with the given headers.
func (g *Target) Call(t testing.TB, method, path string, h http.Header, body any) itest.Response {
	t.Helper()
	return itest.Do(t, g.Base, method, path, h, body)
}

// Owner returns headers of an owner of tenant (strong session, every permission at every outlet).
func (g *Target) Owner(tenant ids.ID) http.Header { return itest.Bearer(g.TenantToken(tenant)) }

// Vendor returns the two credentials of a vendor user holding perms.
func (g *Target) Vendor(perms ...string) http.Header {
	if len(perms) == 0 {
		perms = []string{"admin.org.read", "admin.org.create", "admin.registration.verify"}
	}
	return g.VendorHeadersAs(vendorUser, perms...)
}
