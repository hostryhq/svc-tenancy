package model

import (
	"time"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"
)

// Plan version, subscription and charge states (ADR-0008 S1, Admin A1 spec section 5).
const (
	VersionDraft   = "draft"
	VersionActive  = "active"
	VersionRetired = "retired"

	CycleNone    = "none"
	CycleMonthly = "monthly"
	CycleAnnual  = "annual"

	SubPilot     = "pilot"
	SubActive    = "active"
	SubCancelled = "cancelled"

	ChargeOpen      = "open"
	ChargePaid      = "paid" // derived: payments within tolerance of the total
	ChargeWaived    = "waived"
	ChargeCancelled = "cancelled"

	ChargePeriod = "period"
	ChargeExtra  = "extra"

	BookActive = "active"
	BookClosed = "closed"

	// TradingSourceBilling is the source of billing.trading_day.changed.
	TradingSourceBilling = "billing"
)

// PlatformTenant is the reserved tenant id under which idempotency records of platform vendor routes are
// kept. No organisation has it, no audit row uses it, and no tenant table accepts it.
const PlatformTenant ids.ID = "00000000-0000-0000-0000-000000000000"

// PlanPrice is a list price per cycle, in paise excluding GST.
type PlanPrice struct {
	Cycle     string `json:"cycle"`
	ListPaise int64  `json:"list_paise"`
}

// PlanVersion is one numbered version of a plan.
type PlanVersion struct {
	PlanKey         string
	Version         int
	State           string
	FeatureKeys     []string
	IncludedOutlets int
	Prices          []PlanPrice
	CreatedAt       time.Time
	ActivatedAt     *time.Time
	RetiredAt       *time.Time
}

// ListPrice returns the list price for a cycle.
func (v PlanVersion) ListPrice(cycle string) (int64, bool) {
	for _, p := range v.Prices {
		if p.Cycle == cycle {
			return p.ListPaise, true
		}
	}
	return 0, false
}

// Plan is a key and its versions, newest first.
type Plan struct {
	Key       string
	NameKey   string
	CreatedAt time.Time
	Versions  []PlanVersion
}

// BillingConfig is Hostry's own GST configuration, versioned and effective-dated (BR-A1-30).
type BillingConfig struct {
	ID                ids.ID
	EffectiveFrom     string // YYYY-MM-DD
	HostryStateCode   string
	SAC               string
	GSTRateBP         int
	GSTRegistered     bool
	PaymentToleranceP int64
	PaymentTermsDays  int
	CreatedAt         time.Time
}

// Addon is an add-on line of a subscription.
type Addon struct {
	Name        string `json:"name"`
	Quantity    int    `json:"quantity"`
	AgreedPaise int64  `json:"agreed_paise"`
}

// Subscription is one subscription row; the current row has no SupersededAt.
type Subscription struct {
	ID                ids.ID
	PlanKey           string
	PlanVersion       int
	Cycle             string
	AgreedPaise       int64
	Addons            []Addon
	StartDate         string // YYYY-MM-DD
	PaidThrough       string // YYYY-MM-DD; a pilot's review date
	State             string // pilot, active or cancelled (stored)
	CancelAtPeriodEnd bool
	CreatedAt         time.Time
	SupersededAt      *time.Time
}

// Charge is a ledger row; GST is stored as raised (BR-A1-30). Status is the stored status: open, waived
// or cancelled; paid is derived from the payments.
type Charge struct {
	ID             ids.ID
	SubscriptionID ids.ID
	Kind           string
	PeriodStart    string
	PeriodEnd      string
	TaxableP       int64
	CGSTP          int64
	SGSTP          int64
	IGSTP          int64
	TotalP         int64
	TolerancePaise int64
	DueDate        string
	InvoiceNumber  string
	InvoiceDate    string
	Status         string
	Note           string
	Reason         string
	CreatedBy      string
	CreatedAt      time.Time
}

// Payment is a recorded payment or its reversing row (ReversalOf set), append-only.
type Payment struct {
	ID         ids.ID
	ChargeID   ids.ID
	AmountP    int64
	Method     string
	Reference  string
	ReceivedOn string // YYYY-MM-DD
	RecordedBy string
	ReversalOf ids.ID
	Reason     string
	CreatedAt  time.Time
}

// ManualBook is a paper bill book registered as an entry-only series (ADR-0014 A1.6 item 4).
type ManualBook struct {
	ID                ids.ID
	TaxRegistrationID ids.ID
	GSTIN             string
	Code              string
	OutletIDs         []ids.ID
	Status            string
	CreatedAt         time.Time
}

// TradingState is the latest known business day of one outlet from one trading source
// (billing.trading_day.changed, ADR-0014 A1.2).
type TradingState struct {
	OutletID     ids.ID
	Source       string
	BusinessDate string // YYYY-MM-DD
	State        string // open or closed
	ChangedAt    time.Time
}

// LifecycleEntry is the tenant audit entry of a suspend, reinstate or close (BR-A1-73); ShowToOwner is the
// marker of ADR-0013 V7.
type LifecycleEntry struct {
	ID          ids.ID
	Kind        string
	ApprovalID  string
	Requester   string
	Approver    string
	From, To    string
	Ticket      string
	Reason      string
	ExportOffer bool
	OccurredAt  time.Time
	ShowToOwner bool
}

// LedgerRow is one row of a staff work list.
type LedgerRow struct {
	OrganisationID   ids.ID
	OrganisationName string
	SubjectID        ids.ID
	DueDate          string
	AgeDays          *int
	AmountP          *int64
}

// ExportRow is one row of the ledger CSV: ids and names only (BR-A1-38).
type ExportRow struct {
	OrganisationID   ids.ID
	OrganisationName string
	PeriodStart      string
	PeriodEnd        string
	TaxableP         int64
	TotalP           int64
	DueDate          string
	InvoiceNumber    string
	Status           string
	OutstandingP     int64
}

// Errors of the commercial module and the lifecycle.
var (
	ErrDuplicatePeriod     = apperr.New(apperr.Conflict, "duplicate_period", "a charge for this period already exists")
	ErrInvoiceReferenceSet = apperr.New(apperr.Conflict, "invoice_reference_set", "the invoice reference is set once")
	ErrOverpayment         = apperr.New(apperr.Validation, "overpayment", "the payment exceeds the outstanding amount")
	ErrNotEligible         = apperr.New(apperr.Conflict, "registration_not_eligible", "the registration cannot be the target")
	ErrLinkNotProposed     = apperr.New(apperr.Conflict, "link_not_proposed", "the link is not proposed")
	ErrLinkNotAccepted     = apperr.New(apperr.Conflict, "link_not_accepted", "the link is not accepted")
	ErrTailConfirmed       = apperr.New(apperr.Conflict, "tail_confirmed", "the device already stated its last number")
	ErrSeriesCodeInUse     = apperr.New(apperr.Conflict, "series_code_in_use", "the code is already used under this GSTIN")
	ErrPlanExists          = apperr.New(apperr.Conflict, "plan_exists", "a plan with this key exists")
	ErrGrantUsed           = apperr.New(apperr.Forbidden, "grant.used", "vendor approval grant was already used")
)
