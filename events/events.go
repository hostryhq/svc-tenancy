// Package events builds the domain events the tenancy service publishes (api-specs events/tenancy).
// Events go to the transactional outbox in the same transaction as the state change. Payloads carry
// ids and states only: no GSTIN, no names (ADR-0003 A1.1 item 8, ADR-0011 B6).
package events

import (
	"encoding/json"
	"time"

	cevents "github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
)

// Source is the publishing service.
const Source = "tenancy"

func base(id ids.ID, typ string, tenant ids.ID, key string, at time.Time, actor string, data any) cevents.Envelope {
	b, _ := json.Marshal(data)
	return cevents.Envelope{
		ID: id, Type: typ, V: 1, Source: Source, TenantID: tenant, Key: key,
		OccurredAt: at.UTC(), Actor: actor, Data: b,
	}
}

// OrganisationCreated is published when an organisation is created.
func OrganisationCreated(id ids.ID, at time.Time, actor string, org ids.ID, status string) cevents.Envelope {
	return base(id, "tenancy.organisation.created", org, org.String(), at, actor,
		map[string]any{"organisation_id": org, "status": status})
}

// OrganisationUpdated is published when the name or settings change; changed lists name and/or settings.
func OrganisationUpdated(id ids.ID, at time.Time, actor string, org ids.ID, status string, changed ...string) cevents.Envelope {
	return base(id, "tenancy.organisation.updated", org, org.String(), at, actor,
		map[string]any{"organisation_id": org, "status": status, "changed": changed})
}

// OutletData is the payload of outlet events.
type OutletData struct {
	OutletID          ids.ID `json:"outlet_id"`
	BrandID           ids.ID `json:"brand_id"`
	Kind              string `json:"kind"`
	Status            string `json:"status"`
	TaxRegistrationID ids.ID `json:"tax_registration_id"`
}

// OutletEvent builds tenancy.outlet.created or tenancy.outlet.updated.
func OutletEvent(id ids.ID, typ string, at time.Time, actor string, tenant ids.ID, d OutletData) cevents.Envelope {
	e := base(id, typ, tenant, d.OutletID.String(), at, actor, d)
	e.OutletID = d.OutletID
	return e
}

// LinkEvent builds tenancy.link.<verb>. counterparty is empty until the link has one (proposed).
func LinkEvent(id ids.ID, verb string, at time.Time, actor string, tenant, counterparty, link ids.ID) cevents.Envelope {
	e := base(id, "tenancy.link."+verb, tenant, link.String(), at, actor, map[string]any{"link_id": link})
	if counterparty != "" {
		e.CounterpartyTenantID = counterparty
		e.LinkID = link.String()
	}
	return e
}

// VendorAccess is the payload of tenancy.vendor_access.recorded.
type VendorAccess struct {
	EntryID          ids.ID `json:"entry_id"`
	VendorUserID     string `json:"vendor_user_id"`
	TokenType        string `json:"token_type"`
	SupportSessionID string `json:"support_session_id,omitempty"`
	ActionKey        string `json:"action_key"`
	TargetID         string `json:"target_id,omitempty"`
	TicketReference  string `json:"ticket_reference,omitempty"`
	Outcome          string `json:"outcome"`
	ResultCount      int    `json:"result_count,omitempty"`
	RequestID        string `json:"request_id"`
}

// VendorAccessRecorded builds the event published for each vendor_access_log row.
func VendorAccessRecorded(id ids.ID, at time.Time, tenant ids.ID, d VendorAccess) cevents.Envelope {
	return base(id, "tenancy.vendor_access.recorded", tenant, d.EntryID.String(), at, d.VendorUserID, d)
}
