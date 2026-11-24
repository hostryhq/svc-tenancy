// Package repository holds the persistence interfaces of the tenancy service. The service depends
// on these interfaces only; repository/pg is the Postgres implementation and repository/mem the
// in-memory one used by the test double.
//
// Every Tx runs for exactly one tenant, taken from the tenancy context in ctx (go-common). The
// implementation applies it to the database (row-level security) and adds an explicit tenant
// predicate to every statement. Vendor-authority requests run through the go-auth audited
// transaction so the vendor_access_log row commits with the work (ADR-0013 V5).
package repository

import (
	"context"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
)

// Repository opens transactions and exposes the vendor directory.
type Repository interface {
	// InTx runs fn in one transaction for the tenant in ctx. fn's error rolls everything back.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	// Directory reads the vendor directory through the separate tenancy_vendor role.
	Directory() Directory
	// Ping checks the database for readiness.
	Ping(ctx context.Context) error
}

// Directory is the cross-tenant, read-only vendor view (ADR-0013 V3). It can read the directory
// tables and nothing else.
type Directory interface {
	// List returns organisations newest first after the id (exclusive), at most limit+0 rows.
	List(ctx context.Context, f DirectoryFilter) ([]model.DirOrg, error)
	// FindCreation returns the organisation created under an idempotency key hash, with its fingerprint.
	FindCreation(ctx context.Context, keyHash string) (id ids.ID, fingerprint string, found bool, err error)
	// Holders returns every non-closed or closed registration, in any organisation, carrying a GSTIN.
	Holders(ctx context.Context, gstin string) ([]model.DirHolder, error)
	// TenantIDs pages over all organisation ids (for per-tenant loops that set the context for each).
	TenantIDs(ctx context.Context, after ids.ID, limit int) ([]ids.ID, error)
}

// DirectoryFilter selects directory rows.
type DirectoryFilter struct {
	Q               string // name contains, organisation id, or exact normalised GSTIN
	Status          string
	OrganisationIDs []ids.ID
	After           ids.ID
	Limit           int
}

// Tx is one tenant-scoped transaction.
type Tx interface {
	// ---- organisation and settings ----
	InsertOrg(ctx context.Context, o model.Org, s model.Settings) error
	GetOrg(ctx context.Context) (model.Org, error)
	UpdateOrgName(ctx context.Context, name string, now time.Time) (model.Org, error)
	GetSettings(ctx context.Context) (model.Settings, error)
	PutSettings(ctx context.Context, s model.Settings) error

	// ---- brands ----
	InsertBrand(ctx context.Context, b model.Brand) error
	GetBrand(ctx context.Context, id ids.ID) (model.Brand, error)
	DefaultBrand(ctx context.Context) (model.Brand, error)
	ListBrands(ctx context.Context, after ids.ID, limit int) ([]model.Brand, error)

	// ---- outlets (outlet-scoped) ----
	InsertOutlet(ctx context.Context, o model.Outlet) error
	GetOutlet(ctx context.Context, id ids.ID) (model.Outlet, error)
	// ListOutlets returns the outlets inside scope; kind "" means any.
	ListOutlets(ctx context.Context, scope auth.OutletScope, kind string, after ids.ID, limit int) ([]model.Outlet, error)
	UpdateOutlet(ctx context.Context, o model.Outlet) error

	// ---- tax registrations ----
	// InsertRegistration returns model.ErrDuplicateGSTIN when the organisation already holds the GSTIN.
	InsertRegistration(ctx context.Context, r model.Registration) error
	GetRegistration(ctx context.Context, id ids.ID) (model.Registration, error)
	ListRegistrations(ctx context.Context, after ids.ID, limit int) ([]model.Registration, error)
	// UpdateRegistration returns model.ErrDuplicateGSTIN or model.ErrVerifiedElsewhere on a violated unique index.
	UpdateRegistration(ctx context.Context, r model.Registration) error

	// ---- document-series registry ----
	// SeriesForDevice returns the series of one device at one outlet (all statuses).
	SeriesForDevice(ctx context.Context, outletID, deviceID ids.ID) ([]model.Series, error)
	// ListSeries returns the series inside scope (outlet filter and status "" mean any).
	ListSeries(ctx context.Context, scope auth.OutletScope, status string, after ids.ID, limit int) ([]model.Series, error)
	GetSeries(ctx context.Context, id ids.ID) (model.Series, error)
	// InsertSeries returns model.ErrSeriesCollision when (registration, code) or (gstin, code) is taken.
	InsertSeries(ctx context.Context, s model.Series) error
	UpdateSeries(ctx context.Context, s model.Series) error
	// CloseDeviceSeries closes every dormant or active series of the device. It returns how many it
	// closed and how many series the device has in total (0 means the device is unknown here).
	CloseDeviceSeries(ctx context.Context, deviceID ids.ID, now time.Time) (closed, total int, err error)
	// NextCodeIndex locks and returns the registration's allocator position (0 when none yet).
	NextCodeIndex(ctx context.Context, registrationID ids.ID) (int, error)
	// SetCodeIndex stores the allocator position; it never moves backwards.
	SetCodeIndex(ctx context.Context, registrationID ids.ID, next int, now time.Time) error
	// CountIssued counts series issued to the registration in [from, to).
	CountIssued(ctx context.Context, registrationID ids.ID, from, to time.Time) (int, error)
	// MaxCodeIndexForGSTIN returns the highest allocator index issued under the GSTIN in this
	// organisation, or -1 when none.
	MaxCodeIndexForGSTIN(ctx context.Context, gstin string) (int, error)
	// ForceAdvanceAllocators moves every allocator of the organisation forward by n codes (restore jump).
	ForceAdvanceAllocators(ctx context.Context, n int, now time.Time) error

	// ---- idempotency (scoped by tenant, operation and key) ----
	IdemGet(ctx context.Context, op, key string) (fingerprint string, response []byte, found bool, err error)
	IdemPut(ctx context.Context, op, key, fingerprint string, response []byte, now time.Time) error

	// ---- events ----
	// Publish writes e to the outbox in this transaction, assigning key_seq per key.
	Publish(ctx context.Context, e events.Envelope) error

	// ---- links (two-party) ----
	InsertLink(ctx context.Context, l model.Link, inviteHash []byte, inviteExpires time.Time) error
	// GetLink returns the aggregate if the caller's tenant is a party; forUpdate locks it.
	GetLink(ctx context.Context, id ids.ID, forUpdate bool) (model.Link, error)
	// FindLinkByInvite resolves a hashed invitation code to an unused link id. In Postgres it also makes
	// that one link visible to the caller for the rest of the transaction (acceptance, A1.3); a caller
	// that does not present the code can never see a link it is not a party to.
	FindLinkByInvite(ctx context.Context, hash []byte) (ids.ID, error)
	// UseInvite marks the invitation used; false when it was already used.
	UseInvite(ctx context.Context, hash []byte, now time.Time) (bool, error)
	ListLinks(ctx context.Context, status string, after ids.ID, limit int) ([]model.Link, error)
	SaveLink(ctx context.Context, l model.Link) error
	InsertAmendment(ctx context.Context, a model.Amendment) error
	GetAmendment(ctx context.Context, linkID, id ids.ID) (model.Amendment, error)
	SaveAmendment(ctx context.Context, a model.Amendment) error
	AppendActivity(ctx context.Context, a model.Activity) error
	ListActivity(ctx context.Context, linkID ids.ID, after ids.ID, limit int) ([]model.Activity, error)
	// ExpireDueLinks marks links of this tenant past expiry as expired and returns them.
	ExpireDueLinks(ctx context.Context, now time.Time) ([]model.Link, error)
}
