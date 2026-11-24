package service

import (
	"context"
	"crypto/sha256"
	"strconv"
	"strings"
	"time"

	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/ids"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// The link control plane (ADR-0003 section 3 with A1.3 and A1.7). Phase 1 builds propose, accept,
// confirm, decline, suspend, resume, revoke, amendments, the activity log and the decision lookup. No
// data crosses a link yet. Only an active link authorises anything.

// InviteTTL is how long an invitation code and an unconfirmed link live.
const InviteTTL = 14 * 24 * time.Hour

// inviteAlphabet is the 32-symbol alphabet of invitation codes (no I, L, O, U); 16 symbols give 80 bits.
const inviteAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// GrantIn is a scope the caller offers (outbound) or asks for (inbound).
type GrantIn struct {
	Scope     string
	Direction string // outbound: the caller grants; inbound: the counterparty grants
	OutletIDs []ids.ID
}

// ProposeInput is a link proposal.
type ProposeInput struct {
	ID        ids.ID
	Template  string
	Grants    []GrantIn
	OutletIDs []ids.ID
}

// Proposal is the proposed link and its one-time invitation code.
type Proposal struct {
	Link model.Link
	Code string
}

// Decision is the answer to go-auth's link lookup.
type Decision struct {
	Allowed      bool
	Reason       string
	DataOwner    ids.ID
	OutletIDs    []ids.ID
	CacheSeconds int
}

func (s *Service) newCode() (string, error) {
	b := make([]byte, 16)
	if _, err := s.rnd.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 16)
	for i, x := range b {
		out[i] = inviteAlphabet[x&31] // 32 symbols: no modulo bias
	}
	return string(out), nil
}

func hashCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
	return h[:]
}

func sideOf(l model.Link, tenant ids.ID) string {
	switch tenant {
	case l.ProposerID:
		return model.SideProposer
	case l.AcceptorID:
		if l.AcceptorID != "" {
			return model.SideAcceptor
		}
	}
	return ""
}

func counterpartyOf(l model.Link, tenant ids.ID) ids.ID {
	if tenant == l.ProposerID {
		return l.AcceptorID
	}
	return l.ProposerID
}

// Expired reports whether a link that was never confirmed is past its time.
func Expired(l model.Link, now time.Time) bool {
	return (l.Status == model.LinkProposed || l.Status == model.LinkAccepted) && !now.Before(l.ExpiresAt)
}

// EffectiveStatus is the stored status, with an unconfirmed link past 14 days shown as expired.
func EffectiveStatus(l model.Link, now time.Time) string {
	if Expired(l, now) {
		return model.LinkExpired
	}
	return l.Status
}

func (s *Service) activity(ctx context.Context, tx repository.Tx, l model.Link, side, action, scope, ref string) error {
	return tx.AppendActivity(ctx, model.Activity{ID: s.newID(), LinkID: l.ID, OccurredAt: s.clock.Now(), Side: side,
		ActorID: actor(ctx), Action: action, Scope: scope, ObjectRef: ref, Outcome: "succeeded"})
}

func (s *Service) linkEvent(ctx context.Context, tx repository.Tx, verb string, l model.Link, tenant ids.ID) error {
	return s.publish(ctx, tx, tev.LinkEvent(s.newID(), verb, s.clock.Now(), actor(ctx), tenant, counterpartyOf(l, tenant), l.ID))
}

// snapshot is what this organisation shows the other party: display name and the registrations of the
// outlets it placed in the link (all open registrations when it named none).
func (s *Service) snapshot(ctx context.Context, tx repository.Tx, outletIDs []ids.ID) (model.Party, error) {
	org, err := tx.GetOrg(ctx)
	if err != nil {
		return model.Party{}, err
	}
	var regs []model.Registration
	if len(outletIDs) > 0 {
		seen := map[ids.ID]bool{}
		for _, id := range outletIDs {
			o, err := tx.GetOutlet(ctx, id)
			if err != nil {
				return model.Party{}, model.Invalid("outlet_ids", "unknown_outlet", "an outlet is not in this organisation")
			}
			if seen[o.TaxRegistrationID] {
				continue
			}
			seen[o.TaxRegistrationID] = true
			r, err := tx.GetRegistration(ctx, o.TaxRegistrationID)
			if err != nil {
				return model.Party{}, err
			}
			regs = append(regs, r)
		}
	} else {
		all, err := tx.ListRegistrations(ctx, "", 1000)
		if err != nil {
			return model.Party{}, err
		}
		for _, r := range all {
			if r.Status == model.RegOpen {
				regs = append(regs, r)
			}
		}
	}
	p := model.Party{DisplayName: org.Name}
	for _, r := range regs {
		p.Registrations = append(p.Registrations, model.PartyRegistration{LegalName: r.LegalName, GSTIN: r.GSTIN, StateCode: r.StateCode,
			GSTINVerified: r.Verification == model.Verified})
	}
	return p, nil
}

func (s *Service) checkGrants(in []GrantIn) error {
	if len(in) < 1 || len(in) > 20 {
		return model.Invalid("grants", "length", "1 to 20 grants")
	}
	seen := map[string]bool{}
	for i, g := range in {
		if !s.linkScopes[g.Scope] {
			return model.Invalid("grants", "unknown_scope", "scope "+strconv.Itoa(i)+" is not in the platform scope catalogue")
		}
		if g.Direction != "outbound" && g.Direction != "inbound" {
			return model.Invalid("grants", "direction", "direction must be outbound or inbound")
		}
		k := g.Scope + "/" + g.Direction
		if seen[k] {
			return model.Invalid("grants", "duplicate", "a scope and direction can appear once")
		}
		seen[k] = true
		for _, o := range g.OutletIDs {
			if err := validID("grants.outlet_ids", o); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) grantsOf(link ids.ID, in []GrantIn, status string) []model.Grant {
	out := make([]model.Grant, 0, len(in))
	for _, g := range in {
		side := model.SideProposer
		if g.Direction == "inbound" {
			side = model.SideAcceptor
		}
		out = append(out, model.Grant{ID: s.newID(), LinkID: link, Scope: g.Scope, GrantorSide: side, OutletIDs: g.OutletIDs, Status: status})
	}
	return out
}

// ProposeLink creates a proposed link and its one-time invitation code. The proposer learns nothing
// about the counterparty until it accepts (no tenant enumeration).
func (s *Service) ProposeLink(ctx context.Context, in ProposeInput) (Proposal, error) {
	if err := validID("id", in.ID); err != nil {
		return Proposal{}, err
	}
	switch in.Template {
	case "on_premises_outlet", "service_provider", "supplier_buyer":
	default:
		return Proposal{}, model.Invalid("template", "unknown", "unknown template")
	}
	if err := s.checkGrants(in.Grants); err != nil {
		return Proposal{}, err
	}
	for _, o := range in.OutletIDs {
		if err := validID("outlet_ids", o); err != nil {
			return Proposal{}, err
		}
	}
	tid, err := tenantOf(ctx)
	if err != nil {
		return Proposal{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Proposal, error) {
		if _, err := tx.GetLink(ctx, in.ID, false); err == nil {
			return Proposal{}, model.Invalid("id", "already_exists", "a link with this id already exists")
		}
		party, err := s.snapshot(ctx, tx, in.OutletIDs)
		if err != nil {
			return Proposal{}, err
		}
		for _, g := range in.Grants {
			if g.Direction != "outbound" {
				continue
			}
			for _, o := range g.OutletIDs {
				if _, err := tx.GetOutlet(ctx, o); err != nil {
					return Proposal{}, model.Invalid("grants.outlet_ids", "unknown_outlet", "an outlet is not in this organisation")
				}
			}
		}
		code, err := s.newCode()
		if err != nil {
			return Proposal{}, err
		}
		now := s.clock.Now()
		l := model.Link{ID: in.ID, Template: in.Template, Status: model.LinkProposed, ProposerID: tid, ProposerOutlets: in.OutletIDs,
			ProposerParty: party, ProposedAt: now, ExpiresAt: now.Add(InviteTTL), Grants: s.grantsOf(in.ID, in.Grants, model.GrantPending)}
		if err := tx.InsertLink(ctx, l, hashCode(code), l.ExpiresAt); err != nil {
			return Proposal{}, err
		}
		if err := s.activity(ctx, tx, l, model.SideProposer, "link.proposed", "", ""); err != nil {
			return Proposal{}, err
		}
		if err := s.linkEvent(ctx, tx, "proposed", l, tid); err != nil {
			return Proposal{}, err
		}
		return Proposal{Link: l, Code: code}, nil
	})
}

// AcceptLink redeems an invitation code. The link becomes accepted: no grant is live until the proposer
// confirms the accepting organisation (A1.3). Redemption is rate limited per tenant and address, and
// every failure (used, expired, unknown, malformed) is the same 409.
func (s *Service) AcceptLink(ctx context.Context, code string, outletIDs []ids.ID) (model.Link, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return model.Link{}, err
	}
	for _, k := range []string{"tenant:" + tid.String(), "addr:" + clientAddr(ctx)} {
		if strings.HasSuffix(k, ":") {
			continue
		}
		if ok, wait := s.limiter.allow(k); !ok {
			return model.Link{}, &RetryAfter{Err: apperr.New(apperr.RateLimited, "rate_limited", "too many attempts"), Seconds: int(wait.Seconds()) + 1}
		}
	}
	for _, o := range outletIDs {
		if err := validID("outlet_ids", o); err != nil {
			return model.Link{}, err
		}
	}
	if len(code) != 16 || strings.Trim(code, inviteAlphabet) != "" {
		return model.Link{}, model.ErrInviteInvalid
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Link, error) {
		h := hashCode(code)
		id, err := tx.FindLinkByInvite(ctx, h)
		if err != nil {
			return model.Link{}, model.ErrInviteInvalid
		}
		l, err := tx.GetLink(ctx, id, true)
		if err != nil || l.Status != model.LinkProposed || Expired(l, s.clock.Now()) || l.ProposerID == tid {
			return model.Link{}, model.ErrInviteInvalid
		}
		if used, err := tx.UseInvite(ctx, h, s.clock.Now()); err != nil || !used {
			return model.Link{}, model.ErrInviteInvalid
		}
		party, err := s.snapshot(ctx, tx, outletIDs)
		if err != nil {
			return model.Link{}, err
		}
		for _, g := range l.Grants {
			if g.GrantorSide != model.SideAcceptor {
				continue
			}
			for _, o := range g.OutletIDs {
				if _, err := tx.GetOutlet(ctx, o); err != nil {
					return model.Link{}, model.Invalid("outlet_ids", "unknown_outlet", "a granted outlet is not in this organisation")
				}
			}
		}
		now := s.clock.Now()
		l.Status, l.AcceptorID, l.AcceptorOutlets, l.AcceptorParty, l.AcceptedAt = model.LinkAccepted, tid, outletIDs, party, &now
		if err := tx.SaveLink(ctx, l); err != nil {
			return model.Link{}, err
		}
		if err := s.activity(ctx, tx, l, model.SideAcceptor, "link.accepted", "", ""); err != nil {
			return model.Link{}, err
		}
		return l, s.linkEvent(ctx, tx, "accepted", l, tid)
	})
}

// GetLink returns a link the caller is a party to; any other link is 404.
func (s *Service) GetLink(ctx context.Context, id ids.ID) (model.Link, error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Link, error) { return tx.GetLink(ctx, id, false) })
}

// ListLinks pages the caller's links, optionally by effective status.
func (s *Service) ListLinks(ctx context.Context, status string, after ids.ID, limit int) (Page[model.Link], error) {
	switch status {
	case "", "proposed", "accepted", "active", "suspended", "declined", "expired", "revoked":
	default:
		return Page[model.Link]{}, model.Invalid("status", "unknown", "unknown status")
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.Link], error) {
		rows, err := tx.ListLinks(ctx, "", after, 1000)
		if err != nil {
			return Page[model.Link]{}, err
		}
		now := s.clock.Now()
		var kept []model.Link
		for _, l := range rows {
			if status == "" || EffectiveStatus(l, now) == status {
				kept = append(kept, l)
			}
			if len(kept) > limit {
				break
			}
		}
		return pageOf(kept, limit, func(l model.Link) ids.ID { return l.ID }), nil
	})
}

// transition loads the link for update as one of its parties and applies fn.
func (s *Service) transition(ctx context.Context, id ids.ID, verb string, fn func(l *model.Link, side string, now time.Time) (action string, err error)) (model.Link, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return model.Link{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Link, error) {
		l, err := tx.GetLink(ctx, id, true)
		if err != nil {
			return l, err
		}
		now := s.clock.Now()
		if Expired(l, now) {
			return l, model.ErrLinkState
		}
		side := sideOf(l, tid)
		action, err := fn(&l, side, now)
		if err != nil {
			return l, err
		}
		if action == "" { // nothing changed (idempotent repeat)
			return l, nil
		}
		if err := tx.SaveLink(ctx, l); err != nil {
			return l, err
		}
		if err := s.activity(ctx, tx, l, side, action, "", ""); err != nil {
			return l, err
		}
		return l, s.linkEvent(ctx, tx, verb, l, tid)
	})
}

func setGrants(l *model.Link, from, to string) {
	for i := range l.Grants {
		if l.Grants[i].Status == from {
			l.Grants[i].Status = to
		}
	}
}

// ConfirmLink is the proposer's confirmation of the accepting organisation. The id must be the one the
// proposer was shown, so a swap between viewing and confirming is refused. Only now do grants go live.
func (s *Service) ConfirmLink(ctx context.Context, id, counterparty ids.ID) (model.Link, error) {
	return s.transition(ctx, id, "confirmed", func(l *model.Link, side string, now time.Time) (string, error) {
		if side != model.SideProposer || l.Status != model.LinkAccepted {
			return "", model.ErrLinkState
		}
		if l.AcceptorID != counterparty {
			return "", model.Invalid("counterparty_organisation_id", "counterparty_mismatch", "the accepting organisation is not the one confirmed")
		}
		l.Status, l.ConfirmedAt = model.LinkActive, &now
		setGrants(l, model.GrantPending, model.GrantActive)
		return "link.confirmed", nil
	})
}

func checkReason(reason string) error {
	if n := len([]rune(strings.TrimSpace(reason))); n < 3 || n > 200 {
		return model.Invalid("reason", "length", "3 to 200 characters")
	}
	return nil
}

// DeclineLink withdraws a proposed link, or lets the proposer reject (or the acceptor withdraw from) an
// accepted one. Terminal.
func (s *Service) DeclineLink(ctx context.Context, id ids.ID, reason string) (model.Link, error) {
	if err := checkReason(reason); err != nil {
		return model.Link{}, err
	}
	return s.transition(ctx, id, "declined", func(l *model.Link, side string, _ time.Time) (string, error) {
		if side == "" || (l.Status != model.LinkProposed && l.Status != model.LinkAccepted) {
			return "", model.ErrLinkState
		}
		l.Status = model.LinkDeclined
		setGrants(l, model.GrantPending, model.GrantRemoved)
		return "link.declined", nil
	})
}

// SuspendLink suspends an active link at once, for either party; no call or event crosses.
func (s *Service) SuspendLink(ctx context.Context, id ids.ID, reason string) (model.Link, error) {
	if err := checkReason(reason); err != nil {
		return model.Link{}, err
	}
	tid, _ := tenantOf(ctx)
	return s.transition(ctx, id, "suspended", func(l *model.Link, side string, _ time.Time) (string, error) {
		if side == "" || l.Status != model.LinkActive {
			return "", model.ErrLinkState
		}
		l.Status, l.SuspendedBy = model.LinkSuspended, tid
		return "link.suspended", nil
	})
}

// ResumeLink reactivates a suspended link; only the party that suspended it may.
func (s *Service) ResumeLink(ctx context.Context, id ids.ID) (model.Link, error) {
	tid, _ := tenantOf(ctx)
	return s.transition(ctx, id, "resumed", func(l *model.Link, side string, _ time.Time) (string, error) {
		if side == "" || l.Status != model.LinkSuspended || l.SuspendedBy != tid {
			return "", model.ErrLinkState
		}
		l.Status, l.SuspendedBy = model.LinkActive, ""
		return "link.resumed", nil
	})
}

// RevokeLink ends an active or suspended link for good; repeating it is harmless. Each side keeps what
// it owns; no new call or event crosses.
func (s *Service) RevokeLink(ctx context.Context, id ids.ID, reason string) (model.Link, error) {
	if err := checkReason(reason); err != nil {
		return model.Link{}, err
	}
	return s.transition(ctx, id, "revoked", func(l *model.Link, side string, _ time.Time) (string, error) {
		switch {
		case side == "":
			return "", model.ErrLinkState
		case l.Status == model.LinkRevoked:
			return "", nil
		case l.Status != model.LinkActive && l.Status != model.LinkSuspended:
			return "", model.ErrLinkState
		}
		l.Status, l.SuspendedBy = model.LinkRevoked, ""
		for i := range l.Grants {
			l.Grants[i].Status = model.GrantRemoved
		}
		return "link.revoked", nil
	})
}

// ProposeAmendment proposes adding grants to an active link. It takes effect only when the other party
// accepts: a grant is never widened by the party that receives it, and a grantor never widened without
// the grantee's acceptance (TC-16).
func (s *Service) ProposeAmendment(ctx context.Context, linkID, id ids.ID, in []GrantIn) (model.Amendment, error) {
	if err := validID("id", id); err != nil {
		return model.Amendment{}, err
	}
	if err := s.checkGrants(in); err != nil {
		return model.Amendment{}, err
	}
	tid, err := tenantOf(ctx)
	if err != nil {
		return model.Amendment{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Amendment, error) {
		l, err := tx.GetLink(ctx, linkID, true)
		if err != nil {
			return model.Amendment{}, err
		}
		side := sideOf(l, tid)
		if side == "" || l.Status != model.LinkActive || Expired(l, s.clock.Now()) {
			return model.Amendment{}, model.ErrLinkState
		}
		if _, err := tx.GetAmendment(ctx, linkID, id); err == nil {
			return model.Amendment{}, model.Invalid("id", "already_exists", "an amendment with this id already exists")
		}
		// Directions in the request are relative to the caller; store the grantor as a side of the link.
		add := s.grantsOf(linkID, in, model.GrantPending)
		for i := range add {
			add[i].GrantorSide = grantorSideFor(side, add[i].GrantorSide)
			for _, ex := range l.Grants {
				if ex.Scope == add[i].Scope && ex.GrantorSide == add[i].GrantorSide && ex.Status != model.GrantRemoved {
					return model.Amendment{}, model.Invalid("add_grants", "duplicate", "the link already has this scope in this direction")
				}
			}
		}
		a := model.Amendment{ID: id, LinkID: linkID, ProposedBySide: side, Add: add, Status: model.AmendPending, CreatedAt: s.clock.Now()}
		if err := tx.InsertAmendment(ctx, a); err != nil {
			return a, err
		}
		return a, s.activity(ctx, tx, l, side, "link.amendment_proposed", "", id.String())
	})
}

// grantorSideFor converts a grant side given relative to the proposing side ("proposer" = the caller)
// into a side of the link.
func grantorSideFor(callerSide, relative string) string {
	if callerSide == model.SideProposer {
		return relative
	}
	if relative == model.SideProposer {
		return model.SideAcceptor
	}
	return model.SideProposer
}

// DecideAmendment lets the other party accept or decline. Accepting adds the grants, active at once.
func (s *Service) DecideAmendment(ctx context.Context, linkID, id ids.ID, accept bool) (model.Amendment, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return model.Amendment{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (model.Amendment, error) {
		l, err := tx.GetLink(ctx, linkID, true)
		if err != nil {
			return model.Amendment{}, err
		}
		a, err := tx.GetAmendment(ctx, linkID, id)
		if err != nil {
			return a, err
		}
		side := sideOf(l, tid)
		if side == "" || side == a.ProposedBySide || a.Status != model.AmendPending || l.Status != model.LinkActive {
			return a, model.ErrLinkState
		}
		if accept {
			a.Status = model.AmendAccepted
			for _, g := range a.Add {
				g.Status = model.GrantActive
				l.Grants = append(l.Grants, g)
			}
			if err := tx.SaveLink(ctx, l); err != nil {
				return a, err
			}
		} else {
			a.Status = model.AmendDeclined
		}
		if err := tx.SaveAmendment(ctx, a); err != nil {
			return a, err
		}
		if err := s.activity(ctx, tx, l, side, "link.amendment_decided", "", id.String()); err != nil {
			return a, err
		}
		if accept {
			return a, s.linkEvent(ctx, tx, "amended", l, tid)
		}
		return a, nil
	})
}

// RemoveGrant removes one grant at once, unilaterally, for either party. Repeating it is harmless.
func (s *Service) RemoveGrant(ctx context.Context, linkID, grantID ids.ID) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = run(ctx, s, func(ctx context.Context, tx repository.Tx) (struct{}, error) {
		l, err := tx.GetLink(ctx, linkID, true)
		if err != nil {
			return struct{}{}, err
		}
		side := sideOf(l, tid)
		for i := range l.Grants {
			if l.Grants[i].ID != grantID {
				continue
			}
			if l.Grants[i].Status == model.GrantRemoved {
				return struct{}{}, nil
			}
			l.Grants[i].Status = model.GrantRemoved
			if err := tx.SaveLink(ctx, l); err != nil {
				return struct{}{}, err
			}
			if err := s.activity(ctx, tx, l, side, "link.grant_removed", l.Grants[i].Scope, grantID.String()); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, s.linkEvent(ctx, tx, "amended", l, tid)
		}
		return struct{}{}, model.ErrNotFound
	})
	return err
}

// ListActivity pages a link's activity log, readable by both parties.
func (s *Service) ListActivity(ctx context.Context, linkID, after ids.ID, limit int) (Page[model.Activity], error) {
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Page[model.Activity], error) {
		if _, err := tx.GetLink(ctx, linkID, false); err != nil {
			return Page[model.Activity]{}, err
		}
		rows, err := tx.ListActivity(ctx, linkID, after, limit+1)
		return pageOf(rows, limit, func(a model.Activity) ids.ID { return a.ID }), err
	})
}

// Decide answers go-auth: may the acting tenant (named by the service token) use scope on this link?
// Only an active link with an active grant in the acting tenant's favour allows anything.
func (s *Service) Decide(ctx context.Context, linkID ids.ID, scope string) (Decision, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return Decision{}, err
	}
	return run(ctx, s, func(ctx context.Context, tx repository.Tx) (Decision, error) {
		l, err := tx.GetLink(ctx, linkID, false)
		if err != nil {
			return Decision{}, err
		}
		d := Decision{CacheSeconds: 30}
		side := sideOf(l, tid)
		if EffectiveStatus(l, s.clock.Now()) != model.LinkActive {
			d.Reason = "link_not_active"
			return d, nil
		}
		for _, g := range l.Grants {
			if g.Scope != scope || g.Status != model.GrantActive || g.GrantorSide == side {
				continue
			}
			d.Allowed = true
			d.DataOwner, d.OutletIDs = l.ProposerID, l.ProposerOutlets
			if g.GrantorSide == model.SideAcceptor {
				d.DataOwner, d.OutletIDs = l.AcceptorID, l.AcceptorOutlets
			}
			if len(g.OutletIDs) > 0 {
				d.OutletIDs = g.OutletIDs
			}
			return d, nil
		}
		d.Reason = "scope_not_granted"
		return d, nil
	})
}

// ExpireDueLinks marks unconfirmed links past 14 days as expired, per tenant, and publishes
// tenancy.link.expired. It is run periodically by the process.
func (s *Service) ExpireDueLinks(ctx context.Context) (expired int, err error) {
	var after ids.ID
	for {
		batch, err := s.repo.Directory().TenantIDs(ctx, after, 100)
		if err != nil {
			return expired, err
		}
		for _, t := range batch {
			if err := s.internalTx(t, func(c context.Context, tx repository.Tx) error {
				due, err := tx.ExpireDueLinks(c, s.clock.Now())
				for _, l := range due {
					if err == nil {
						err = s.linkEvent(c, tx, "expired", l, t)
					}
					if err == nil {
						err = s.activity(c, tx, l, model.SideSystem, "link.expired", "", "")
					}
					expired++
				}
				return err
			}); err != nil {
				return expired, err
			}
		}
		if len(batch) < 100 {
			return expired, nil
		}
		after = batch[len(batch)-1]
	}
}
