package service_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/service"
)

// ADR-0003 N1 R1: an idempotency record never holds the one-time invitation code. A replay returns the
// same link without it.
func TestReplayedProposeLinkReturnsTheLinkWithoutTheCode(t *testing.T) {
	f := newFixture(t)
	in := service.ProposeInput{ID: f.newID(), Template: "supplier_buyer", Grants: []service.GrantIn{{Scope: "test.echo.read", Direction: "outbound"}}}
	ctx := service.WithIdem(f.as(orgA), service.Idem{Op: "proposeLink", Key: "k-1", Fingerprint: "fp"})
	first, err := f.svc.ProposeLink(ctx, in)
	if err != nil || len(first.Code) != 16 {
		t.Fatalf("first call: %v code %q", err, first.Code)
	}
	again, err := f.svc.ProposeLink(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.Link.ID != first.Link.ID || again.Code != "" {
		t.Fatalf("replay: link %s code %q, want link %s and no code", again.Link.ID, again.Code, first.Link.ID)
	}
	if b, _ := json.Marshal(first); strings.Contains(string(b), first.Code) {
		t.Fatalf("the serialised proposal (what the idempotency record stores) holds the code: %s", b)
	}
	if _, err := f.svc.AcceptLink(f.as(orgB), first.Code, nil); err != nil {
		t.Fatal(err)
	}
}

// N1 R3.1: the counterparty snapshot is a closed list. A new field must be a conscious decision.
func TestCounterpartySnapshotIsAClosedFieldList(t *testing.T) {
	names := func(v any) string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			out = append(out, rt.Field(i).Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(model.Party{}); got != "DisplayName,Registrations" {
		t.Errorf("party snapshot fields changed: %s", got)
	}
	if got := names(model.PartyRegistration{}); got != "LegalName,GSTIN,StateCode,GSTINVerified" {
		t.Errorf("registration snapshot fields changed: %s", got)
	}
}

// N1 R3.2 and R3.3: never refreshed; re-checked at confirm; a change is 409 counterparty_changed.
func TestConfirmRefusesWhenTheCounterpartyChangedAndTheSnapshotIsNeverRefreshed(t *testing.T) {
	f := newFixture(t)
	l, code := f.proposed()
	acc, err := f.svc.AcceptLink(f.as(orgB), code, []ids.ID{seed.Other.Outlet})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RenameOrganisation(f.as(orgB), "A Different Name Pvt Ltd"); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ConfirmLink(f.as(orgA), l.ID, orgB)
	wantCode(t, err, "counterparty_changed")
	got, err := f.svc.GetLink(f.as(orgA), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.LinkAccepted || got.AcceptorParty.DisplayName != acc.AcceptorParty.DisplayName {
		t.Fatalf("the refused confirm changed the link or its snapshot: %s %q", got.Status, got.AcceptorParty.DisplayName)
	}
	for _, g := range got.Grants {
		if g.Status == model.GrantActive {
			t.Fatal("a grant went live")
		}
	}
}

func TestConfirmSucceedsWhenTheCounterpartyIsUnchanged(t *testing.T) {
	f := newFixture(t)
	if l := f.active(); l.Status != model.LinkActive {
		t.Fatalf("status %s", l.Status)
	}
}
