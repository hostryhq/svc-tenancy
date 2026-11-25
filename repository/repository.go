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
	// InPlatformTx is InTx for a vendor route that names no organisation (createOrganisation): the
	// audit row goes to the platform log, which has no tenant, in the same transaction.
	InPlatformTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	// InPlatformOnlyTx is the transaction of a vendor route on platform data that belongs to no organisation
	// (plans, Hostry's GST configuration, the ledger export): the platform audit row commits with the work.
	// The tenant methods of Tx are not usable in it; idempotency records live under model.PlatformTenant.
	InPlatformOnlyTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	// InPlatformReadTx is a read-only transaction on platform data for directory-class vendor reads
	// (x-vendor-audit: none): no audit row, nothing is written.
	InPlatformReadTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
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
	// LedgerList derives one staff work list from the directory ledger rows (no scheduled work, nothing is
	// sent). today and the horizon dates are IST dates (YYYY-MM-DD). Rows are ordered by (due date, subject id)
	// and continue after (afterDue, afterID).
	LedgerList(ctx context.Context, kind string, today string, afterDue string, afterID ids.ID, limit int) ([]model.LedgerRow, error)
	// LedgerExport returns charge rows whose due date lies in [from, to] ("" is open) for the CSV export.
	LedgerExport(ctx context.Context, from, to string) ([]model.ExportRow, error)
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
	// CloseRegistrationSeries closes every dormant or active series under the registration and returns them
	// (as closed), for the tenancy.series.closed events.
	CloseRegistrationSeries(ctx context.Context, registrationID ids.ID, now time.Time) ([]model.Series, error)
	// CloseOutletSeries closes every dormant or active series of the outlet under the registration and returns them.
	CloseOutletSeries(ctx context.Context, outletID, registrationID ids.ID, now time.Time) ([]model.Series, error)
	// OpenSeriesIDs returns the ids of the outlet's dormant and active series.
	OpenSeriesIDs(ctx context.Context, outletID ids.ID) ([]ids.ID, error)
	// ConfirmTail fills tail_confirmed_at and last_number of a closed series that has none yet (the first
	// statement wins). It returns false when the series is unknown here, open, or already confirmed.
	ConfirmTail(ctx context.Context, seriesID ids.ID, at time.Time, last int) (bool, error)

	// ---- manual bill books ----
	// InsertManualBook returns model.ErrSeriesCodeInUse when the code is a device series code under the same
	// GSTIN or another book's code under the registration.
	InsertManualBook(ctx context.Context, b model.ManualBook) error
	// ListManualBooks returns the books after the id, in id order.
	ListManualBooks(ctx context.Context, after ids.ID, limit int) ([]model.ManualBook, error)

	// ---- organisation status, lifecycle audit and the single-use grant record ----
	// SetOrgStatus changes the organisation status and its directory row.
	SetOrgStatus(ctx context.Context, status string, now time.Time) error
	AppendLifecycle(ctx context.Context, e model.LifecycleEntry) error
	// MarkGrantUsed records the vendor approval grant id as used in this transaction; false when it was
	// used before (ADR-0014 A2.6 item 4).
	MarkGrantUsed(ctx context.Context, jti, approvalID, kind string, now time.Time) (first bool, err error)

	// ---- trading state (fed by billing.trading_day.changed, ADR-0014 A1.2) ----
	// UpsertTradingState keeps one row per outlet and source: the latest business date wins, and for one date
	// the later change. It reports whether the stored row changed.
	UpsertTradingState(ctx context.Context, s model.TradingState) (bool, error)
	// OutletsInProgress counts the outlets (all, or only outletID when set) with a day open and dated from..to
	// inclusive (IST dates, YYYY-MM-DD).
	OutletsInProgress(ctx context.Context, from, to string, outletID ids.ID) (int, error)

	// ---- plans and Hostry's GST configuration (platform data, readable in any transaction) ----
	GetPlan(ctx context.Context, key string) (model.Plan, error)
	// ListPlans returns plans in key order after the key (exclusive).
	ListPlans(ctx context.Context, afterKey string, limit int) ([]model.Plan, error)
	// InsertPlan stores a plan with its versions; model.ErrPlanExists when the key is taken.
	InsertPlan(ctx context.Context, p model.Plan) error
	InsertPlanVersion(ctx context.Context, v model.PlanVersion) error
	UpdatePlanVersion(ctx context.Context, v model.PlanVersion) error
	GetPlanVersion(ctx context.Context, key string, version int) (model.PlanVersion, error)
	// NextPlanVersion locks the plan and returns the next version number (404 when the plan is unknown).
	NextPlanVersion(ctx context.Context, key string) (int, error)
	// InsertBillingConfig returns apperr conflict `effective_from_taken` when that date has a version.
	InsertBillingConfig(ctx context.Context, c model.BillingConfig) error
	// ListBillingConfigs returns versions newest first (effective_from, then id) after the id.
	ListBillingConfigs(ctx context.Context, after ids.ID, limit int) ([]model.BillingConfig, error)
	// BillingConfigFor returns the version with the latest effective_from not after date.
	BillingConfigFor(ctx context.Context, date string) (model.BillingConfig, bool, error)

	// ---- subscription, entitlements, charges and payments (tenant-owned, ADR-0008 S2) ----
	CurrentSubscription(ctx context.Context) (model.Subscription, bool, error)
	// SubscriptionHistory returns the superseded rows, newest first.
	SubscriptionHistory(ctx context.Context) ([]model.Subscription, error)
	InsertSubscription(ctx context.Context, s model.Subscription) error
	UpdateSubscription(ctx context.Context, s model.Subscription) error
	// ReplaceEntitlements sets the organisation's entitlement rows to the keys, in this transaction.
	ReplaceEntitlements(ctx context.Context, subscriptionID ids.ID, keys []string, now time.Time) error
	Entitlements(ctx context.Context) ([]string, error)
	InsertCharge(ctx context.Context, c model.Charge) error
	GetCharge(ctx context.Context, id ids.ID) (model.Charge, error)
	UpdateCharge(ctx context.Context, c model.Charge) error
	// ListCharges returns charges newest period first after the id, status "" meaning any stored status.
	ListCharges(ctx context.Context, status string, after ids.ID, limit int) ([]model.Charge, error)
	// LiveChargesOverlapping returns the period charges that are not cancelled and overlap [start, end].
	LiveChargesOverlapping(ctx context.Context, start, end string) ([]model.Charge, error)
	InsertPayment(ctx context.Context, p model.Payment) error
	GetPayment(ctx context.Context, chargeID, id ids.ID) (model.Payment, error)
	// ListPayments returns the rows of one charge, oldest first, after the id.
	ListPayments(ctx context.Context, chargeID ids.ID, after ids.ID, limit int) ([]model.Payment, error)
	// NetPaid returns the net paid amount per charge (payments less reversals).
	NetPaid(ctx context.Context) (map[ids.ID]int64, error)
	// AllPayments returns every payment row of the organisation, oldest first.
	AllPayments(ctx context.Context) ([]model.Payment, error)
	// CountPaymentRef counts original payments of the organisation with the same reference (a duplicate is
	// a warning only, BR-A1-45).
	CountPaymentRef(ctx context.Context, reference string) (int, error)
	// CountOutlets counts the organisation's outlets (the basis of the soft outlet limit, A1-Q9).
	CountOutlets(ctx context.Context) (int, error)
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
	// ReissueInvite invalidates the link's unused invitations and stores a new one (proposer only).
	ReissueInvite(ctx context.Context, linkID ids.ID, hash []byte, expires, now time.Time) error
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
