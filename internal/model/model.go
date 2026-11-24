// Package model holds the tenancy domain types shared by service and repository, and the
// typed errors the service raises. It has no behaviour that needs I/O.
package model

import (
	"time"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"
)

// Organisation, outlet, registration and series states.
const (
	OrgTrial = "trial"

	OutletActive   = "active"
	OutletInactive = "inactive"

	RegimeRegular      = "regular"
	RegimeComposition  = "composition"
	RegimeUnregistered = "unregistered"

	Unverified = "unverified"
	Verified   = "verified"
	Rejected   = "rejected"

	RegOpen   = "open"
	RegClosed = "closed"

	SeriesDormant            = "dormant"
	SeriesActive             = "active"
	SeriesClosed             = "closed"
	SeriesClosedUnreconciled = "closed_unreconciled"
)

// Link states (ADR-0003 A1.3) and sides.
const (
	LinkProposed  = "proposed"
	LinkAccepted  = "accepted"
	LinkActive    = "active"
	LinkSuspended = "suspended"
	LinkDeclined  = "declined"
	LinkExpired   = "expired"
	LinkRevoked   = "revoked"

	SideProposer = "proposer"
	SideAcceptor = "acceptor"
	SideSystem   = "system"

	GrantPending = "pending"
	GrantActive  = "active"
	GrantRemoved = "removed"

	AmendPending  = "pending"
	AmendAccepted = "accepted"
	AmendDeclined = "declined"
)

// Org is the tenant root.
type Org struct {
	ID            ids.ID
	Name          string
	Status        string
	DataPlacement string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// CreateKeyHash and CreateFingerprint make vendor creation idempotent (ADR-0013 V4); empty for
	// organisations that were not created through the vendor operation.
	CreateKeyHash     string
	CreateFingerprint string
}

// Settings are the organisation-wide settings.
type Settings struct {
	OfflineWindowDays int
	IdleLockMinutes   int
	UpdatedAt         time.Time
}

// Brand is a trading name inside an organisation.
type Brand struct {
	ID        ids.ID
	Name      string
	IsDefault bool
	CreatedAt time.Time
}

// Outlet is an outlet or property (one ID space).
type Outlet struct {
	ID                ids.ID
	BrandID           ids.ID
	Name              string
	Kind              string
	Status            string
	StateCode         string
	TaxRegistrationID ids.ID
	LocatedInOutletID ids.ID
	CreatedAt         time.Time
}

// Registration is a tax registration; GSTIN is the normalised form ("" when unregistered).
type Registration struct {
	ID           ids.ID
	LegalName    string
	Regime       string
	GSTIN        string
	StateCode    string
	Verification string
	Method       string
	VerifiedAt   *time.Time
	VerifiedBy   string
	Status       string
	CreatedAt    time.Time
}

// Series is one series code under a registration.
type Series struct {
	ID                ids.ID
	TaxRegistrationID ids.ID
	GSTIN             string
	Code              string
	CodeIndex         int
	DocType           string
	OutletID          ids.ID
	DeviceID          ids.ID
	Status            string
	IssuedAt          time.Time
	ClosedAt          *time.Time
	ClosedReason      string
}

// Grant is one directional scope of a link. GrantorSide is the side that owns the data.
type Grant struct {
	ID          ids.ID
	LinkID      ids.ID
	Scope       string
	GrantorSide string
	OutletIDs   []ids.ID
	Status      string
}

// Party is the snapshot one party leaves for the other when it proposes or accepts (A1.3).
type Party struct {
	DisplayName   string
	Registrations []PartyRegistration
}

// PartyRegistration is what the other side sees of a registration.
type PartyRegistration struct {
	LegalName     string `json:"legal_name"`
	GSTIN         string `json:"gstin,omitempty"`
	StateCode     string `json:"state_code"`
	GSTINVerified bool   `json:"gstin_verified"`
}

// Link is a relationship between a proposer and, once it accepts, an acceptor.
type Link struct {
	ID              ids.ID
	Template        string
	Status          string
	ProposerID      ids.ID
	AcceptorID      ids.ID
	ProposerOutlets []ids.ID
	AcceptorOutlets []ids.ID
	ProposerParty   Party
	AcceptorParty   Party
	SuspendedBy     ids.ID
	ProposedAt      time.Time
	ExpiresAt       time.Time
	AcceptedAt      *time.Time
	ConfirmedAt     *time.Time
	Grants          []Grant
}

// Amendment adds grants to a link; the other side must accept (A1.7, TC-16).
type Amendment struct {
	ID             ids.ID
	LinkID         ids.ID
	ProposedBySide string
	Add            []Grant
	Status         string
	CreatedAt      time.Time
}

// Activity is one link_activity row: a state change, with no payload.
type Activity struct {
	ID         ids.ID
	LinkID     ids.ID
	OccurredAt time.Time
	Side       string
	ActorID    string
	Action     string
	Scope      string
	ObjectRef  string
	Outcome    string
}

// DirOrg is the vendor directory view of an organisation.
type DirOrg struct {
	ID          ids.ID
	Name        string
	Status      string
	OutletCount int
	CreatedAt   time.Time
}

// DirHolder is an organisation and registration that holds (or held) a GSTIN.
type DirHolder struct {
	TenantID       ids.ID
	RegistrationID ids.ID
	Verified       bool
	Closed         bool
}

// Typed errors raised by service and repository.
var (
	ErrNotFound      = apperr.New(apperr.NotFound, "not_found", "")
	ErrIDConflict    = apperr.New(apperr.Conflict, "id_conflict", "a record with this id already exists")
	ErrGSTINInUse    = apperr.New(apperr.Conflict, "gstin_in_use", "")
	ErrRegUnverified = apperr.New(apperr.Conflict, "registration_unverified", "")
	ErrSeriesCap     = apperr.New(apperr.RateLimited, "series_cap_reached", "")
	ErrLinkState     = apperr.New(apperr.Conflict, "link_state_conflict", "")
	ErrInviteInvalid = apperr.New(apperr.Conflict, "invitation_code_invalid", "")
	// Errors the repository returns for constraint violations; the service maps them.
	ErrDuplicateGSTIN    = apperr.New(apperr.Conflict, "gstin_duplicate", "")
	ErrVerifiedElsewhere = apperr.New(apperr.Conflict, "gstin_in_use", "")
	ErrSeriesCollision   = apperr.New(apperr.Conflict, "series_collision", "")
	ErrCreateKeyTaken    = apperr.New(apperr.Conflict, "create_key_taken", "")
)

// Invalid builds a 422 with one field error.
func Invalid(field, code, msg string) *apperr.Error {
	return apperr.New(apperr.Validation, "validation", "validation failed").WithFields(apperr.FieldError{Field: field, Code: code, Message: msg})
}
