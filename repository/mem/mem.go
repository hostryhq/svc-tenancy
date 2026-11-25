// Package mem is the in-memory implementation of repository.Repository. The test double runs the real
// service and API over it. Transactions are serialised and copy-on-write, so a failed transaction (or a
// failed vendor audit write) leaves no trace, as in Postgres. Every uniqueness rule that Postgres holds
// with an index is enforced here too, across tenants.
package mem

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/auditsink"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/internal/nulldb"
	"github.com/hostryhq/svc-tenancy/repository"
)

type key struct{ t, id ids.ID }
type idemKey struct {
	t       ids.ID
	op, key string
}
type idemVal struct {
	fp   string
	resp []byte
}
type invite struct {
	link    ids.ID
	expires time.Time
	used    bool
}

var errAuditDown = errors.New("mem: vendor access log unwritable")

// VendorLogEntry is one vendor_access_log row.
type VendorLogEntry struct {
	auth.VendorAccessEntry
}

type state struct {
	orgs     map[ids.ID]model.Org
	settings map[ids.ID]model.Settings
	brands   map[key]model.Brand
	outlets  map[key]model.Outlet
	regs     map[key]model.Registration
	series   map[key]model.Series
	alloc    map[key]int // (tenant, registration) -> next index
	idem     map[idemKey]idemVal
	keySeq   map[key]int64 // (tenant, aggregate key as id) -> last key_seq
	links    map[ids.ID]model.Link
	invites  map[string]*invite
	amends   map[key]model.Amendment
	activity []model.Activity
	outbox   []events.Envelope
	vlog     []VendorLogEntry
	plog     []VendorLogEntry // platform_vendor_access_log: rows that concern no organisation
	com      *commercial      // Phase 2: plans, ledger, trading state, lifecycle
}

func newState() *state {
	return &state{orgs: map[ids.ID]model.Org{}, settings: map[ids.ID]model.Settings{}, brands: map[key]model.Brand{},
		outlets: map[key]model.Outlet{}, regs: map[key]model.Registration{}, series: map[key]model.Series{},
		alloc: map[key]int{}, idem: map[idemKey]idemVal{}, keySeq: map[key]int64{}, links: map[ids.ID]model.Link{},
		invites: map[string]*invite{}, amends: map[key]model.Amendment{}, com: newCommercial()}
}

func cloneLink(l model.Link) model.Link {
	l.Grants = append([]model.Grant(nil), l.Grants...)
	for i := range l.Grants {
		l.Grants[i].OutletIDs = append([]ids.ID(nil), l.Grants[i].OutletIDs...)
	}
	l.ProposerOutlets = append([]ids.ID(nil), l.ProposerOutlets...)
	l.AcceptorOutlets = append([]ids.ID(nil), l.AcceptorOutlets...)
	l.ProposerParty.Registrations = append([]model.PartyRegistration(nil), l.ProposerParty.Registrations...)
	l.AcceptorParty.Registrations = append([]model.PartyRegistration(nil), l.AcceptorParty.Registrations...)
	return l
}

func (s *state) clone() *state {
	c := newState()
	for k, v := range s.orgs {
		c.orgs[k] = v
	}
	for k, v := range s.settings {
		c.settings[k] = v
	}
	for k, v := range s.brands {
		c.brands[k] = v
	}
	for k, v := range s.outlets {
		c.outlets[k] = v
	}
	for k, v := range s.regs {
		c.regs[k] = v
	}
	for k, v := range s.series {
		c.series[k] = v
	}
	for k, v := range s.alloc {
		c.alloc[k] = v
	}
	for k, v := range s.idem {
		c.idem[k] = v
	}
	for k, v := range s.keySeq {
		c.keySeq[k] = v
	}
	for k, v := range s.links {
		c.links[k] = cloneLink(v)
	}
	for k, v := range s.invites {
		x := *v
		c.invites[k] = &x
	}
	for k, v := range s.amends {
		v.Add = append([]model.Grant(nil), v.Add...)
		c.amends[k] = v
	}
	c.activity = append([]model.Activity(nil), s.activity...)
	c.outbox = append([]events.Envelope(nil), s.outbox...)
	c.vlog = append([]VendorLogEntry(nil), s.vlog...)
	c.plog = append([]VendorLogEntry(nil), s.plog...)
	c.com = s.com.clone()
	return c
}

// Store is the in-memory repository. It also implements auth.VendorAuditSink, writing the vendor access
// log and its event inside the running transaction.
type Store struct {
	mu        sync.Mutex
	st        *state
	pending   *state // the transaction in progress (valid while mu is held by InTx)
	failAudit bool
	db        *sql.DB
	gen       *ids.Generator
}

// New returns an empty store. gen issues ids for audit rows.
func New(gen *ids.Generator) *Store {
	return &Store{st: newState(), db: nulldb.Open(), gen: gen}
}

var (
	_ repository.Repository  = (*Store)(nil)
	_ auth.VendorAuditSink   = (*Store)(nil)
	_ auth.PlatformAuditSink = (*Store)(nil)
)

// Events returns every event written to the outbox, in order.
func (s *Store) Events() []events.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Envelope(nil), s.st.outbox...)
}

// VendorLog returns the vendor_access_log rows, in order.
func (s *Store) VendorLog() []VendorLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]VendorLogEntry(nil), s.st.vlog...)
}

// PlatformLog returns the platform_vendor_access_log rows (vendor actions that name no organisation).
func (s *Store) PlatformLog() []VendorLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]VendorLogEntry(nil), s.st.plog...)
}

// Reset drops every row, as a fresh store (used to load an example world).
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = newState()
}

// RecordPlatformInTx writes the platform log row into the running transaction.
func (s *Store) RecordPlatformInTx(_ context.Context, _ tenancy.Execer, e auth.VendorAccessEntry) error {
	if s.failAudit {
		return errAuditDown
	}
	s.pending.plog = append(s.pending.plog, VendorLogEntry{keyed(e)})
	return nil
}

// RecordPlatformRefused records a refused request in the platform log, in a transaction of its own.
func (s *Store) RecordPlatformRefused(_ context.Context, e auth.VendorAccessEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Outcome = auth.OutcomeRefused
	s.st.plog = append(s.st.plog, VendorLogEntry{keyed(e)})
	return nil
}

// FailAuditWrites makes the vendor access log unwritable, to prove that vendor requests fail closed.
func (s *Store) FailAuditWrites(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAudit = fail
}

// RecordInTx writes the log row and its outbox event into the running transaction.
func (s *Store) RecordInTx(_ context.Context, _ tenancy.Execer, e auth.VendorAccessEntry) error {
	if s.failAudit {
		return errAuditDown
	}
	tid := ids.ID(e.TenantID)
	s.pending.vlog = append(s.pending.vlog, VendorLogEntry{keyed(e)})
	return s.pending.publish(tid, tev.VendorAccessRecorded(e.ID, e.Time, tid, vendorData(e)))
}

// RecordRefused records a refused request in a transaction of its own.
func (s *Store) RecordRefused(_ context.Context, e auth.VendorAccessEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := ids.Parse(e.TenantID); err != nil {
		return nil // no organisation to attach the row to
	}
	e.Outcome = auth.OutcomeRefused
	s.st.vlog = append(s.st.vlog, VendorLogEntry{keyed(e)})
	return s.st.publish(ids.ID(e.TenantID), tev.VendorAccessRecorded(e.ID, e.Time, ids.ID(e.TenantID), vendorData(e)))
}

// keyed stores the stable action key, as the Postgres sink does, instead of the raw operation id.
func keyed(e auth.VendorAccessEntry) auth.VendorAccessEntry {
	tt := "vendor"
	if e.TokenType == auth.TypeSupport {
		tt = "support"
	}
	e.ActionKey = auditsink.ActionKey(e.ActionKey, tt)
	return e
}

func vendorData(e auth.VendorAccessEntry) tev.VendorAccess {
	outcome := "succeeded"
	if e.Outcome == auth.OutcomeRefused {
		outcome = "refused"
	}
	tt := "vendor"
	if e.TokenType == auth.TypeSupport {
		tt = "support"
	}
	return tev.VendorAccess{EntryID: e.ID, VendorUserID: e.VendorUser, TokenType: tt, SupportSessionID: e.SupportSession,
		ActionKey: auditsink.ActionKey(e.ActionKey, tt), TargetID: e.TargetID, TicketReference: e.Ticket, Outcome: outcome,
		ResultCount: e.ResultCount, RequestID: e.RequestID}
}

func (st *state) publish(tenant ids.ID, e events.Envelope) error {
	if err := e.Validate(); err != nil {
		return err
	}
	k := key{tenant, ids.ID(e.Key)}
	st.keySeq[k]++
	e.KeySeq = st.keySeq[k]
	st.outbox = append(st.outbox, e)
	return nil
}

// InTx implements repository.Repository.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.st.clone()
	s.pending = work
	defer func() { s.pending = nil }()
	if err := auth.WithAuditedTx(ctx, s.db, func(c context.Context, _ *sql.Tx) error {
		return fn(c, &tx{st: work, t: tc.TenantID})
	}); err != nil {
		return err
	}
	s.st = work
	return nil
}

// InPlatformTx implements repository.Repository.
func (s *Store) InPlatformTx(ctx context.Context, fn func(ctx context.Context, tx repository.Tx) error) error {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	return s.platformTx(ctx, tc.TenantID, fn)
}

func (s *Store) platformTx(ctx context.Context, tenant ids.ID, fn func(ctx context.Context, tx repository.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.st.clone()
	s.pending = work
	defer func() { s.pending = nil }()
	if err := auth.WithPlatformAuditedTx(ctx, s.db, func(c context.Context, _ *sql.Tx) error {
		return fn(c, &tx{st: work, t: tenant})
	}); err != nil {
		return err
	}
	s.st = work
	return nil
}

// Ping implements repository.Repository.
func (s *Store) Ping(context.Context) error { return nil }

// Directory implements repository.Repository.
func (s *Store) Directory() repository.Directory { return dir{s} }

type dir struct{ s *Store }

func (d dir) snapshot() *state {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	return d.s.st
}

func (d dir) List(_ context.Context, f repository.DirectoryFilter) ([]model.DirOrg, error) {
	st := d.snapshot()
	want := map[ids.ID]bool{}
	for _, id := range f.OrganisationIDs {
		want[id] = true
	}
	q := strings.ToLower(f.Q)
	var rows []model.DirOrg
	for id, o := range st.orgs {
		if f.After != "" && id >= f.After {
			continue
		}
		if f.Status != "" && o.Status != f.Status {
			continue
		}
		if len(want) > 0 && !want[id] {
			continue
		}
		if q != "" {
			match := strings.Contains(strings.ToLower(o.Name), q) || strings.EqualFold(id.String(), q)
			for k, r := range st.regs {
				if k.t == id && r.GSTIN != "" && r.GSTIN == strings.ToUpper(strings.ReplaceAll(f.Q, " ", "")) {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		n := 0
		for k := range st.outlets {
			if k.t == id {
				n++
			}
		}
		rows = append(rows, model.DirOrg{ID: id, Name: o.Name, Status: o.Status, OutletCount: n, CreatedAt: o.CreatedAt})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	if f.Limit > 0 && len(rows) > f.Limit {
		rows = rows[:f.Limit]
	}
	return rows, nil
}

func (d dir) FindCreation(_ context.Context, keyHash string) (ids.ID, string, bool, error) {
	for id, o := range d.snapshot().orgs {
		if o.CreateKeyHash != "" && o.CreateKeyHash == keyHash {
			return id, o.CreateFingerprint, true, nil
		}
	}
	return "", "", false, nil
}

func (d dir) Holders(_ context.Context, gstin string) ([]model.DirHolder, error) {
	var out []model.DirHolder
	for k, r := range d.snapshot().regs {
		if r.GSTIN == gstin {
			out = append(out, model.DirHolder{TenantID: k.t, RegistrationID: r.ID, Verified: r.Verification == model.Verified, Closed: r.Status == model.RegClosed})
		}
	}
	return out, nil
}

func (d dir) TenantIDs(_ context.Context, after ids.ID, limit int) ([]ids.ID, error) {
	var out []ids.ID
	for id := range d.snapshot().orgs {
		if id > after {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// tx is one tenant-scoped transaction over a working copy.
type tx struct {
	st         *state
	t          ids.ID
	inviteLink ids.ID // link made visible by presenting its invitation code
}

func (x *tx) InsertOrg(_ context.Context, o model.Org, s model.Settings) error {
	if _, ok := x.st.orgs[x.t]; ok {
		return model.ErrIDConflict
	}
	if o.CreateKeyHash != "" {
		for _, e := range x.st.orgs {
			if e.CreateKeyHash == o.CreateKeyHash {
				return model.ErrCreateKeyTaken
			}
		}
	}
	x.st.orgs[x.t], x.st.settings[x.t] = o, s
	return nil
}

func (x *tx) GetOrg(context.Context) (model.Org, error) {
	o, ok := x.st.orgs[x.t]
	if !ok {
		return o, model.ErrNotFound
	}
	return o, nil
}

func (x *tx) UpdateOrgName(_ context.Context, name string, now time.Time) (model.Org, error) {
	o, ok := x.st.orgs[x.t]
	if !ok {
		return o, model.ErrNotFound
	}
	o.Name, o.UpdatedAt = name, now
	x.st.orgs[x.t] = o
	return o, nil
}

func (x *tx) GetSettings(context.Context) (model.Settings, error) {
	s, ok := x.st.settings[x.t]
	if !ok {
		return s, model.ErrNotFound
	}
	return s, nil
}

func (x *tx) PutSettings(_ context.Context, s model.Settings) error {
	x.st.settings[x.t] = s
	return nil
}

func (x *tx) InsertBrand(_ context.Context, b model.Brand) error {
	if _, ok := x.st.brands[key{x.t, b.ID}]; ok {
		return model.ErrIDConflict
	}
	x.st.brands[key{x.t, b.ID}] = b
	return nil
}

func (x *tx) GetBrand(_ context.Context, id ids.ID) (model.Brand, error) {
	b, ok := x.st.brands[key{x.t, id}]
	if !ok {
		return b, model.ErrNotFound
	}
	return b, nil
}

func (x *tx) DefaultBrand(context.Context) (model.Brand, error) {
	for k, b := range x.st.brands {
		if k.t == x.t && b.IsDefault {
			return b, nil
		}
	}
	return model.Brand{}, model.ErrNotFound
}

func (x *tx) ListBrands(_ context.Context, after ids.ID, limit int) ([]model.Brand, error) {
	var out []model.Brand
	for k, b := range x.st.brands {
		if k.t == x.t && b.ID > after {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

func cut[T any](rows []T, limit int) []T {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

func (x *tx) InsertOutlet(_ context.Context, o model.Outlet) error {
	if _, ok := x.st.outlets[key{x.t, o.ID}]; ok {
		return model.ErrIDConflict
	}
	x.st.outlets[key{x.t, o.ID}] = o
	return nil
}

func (x *tx) GetOutlet(_ context.Context, id ids.ID) (model.Outlet, error) {
	o, ok := x.st.outlets[key{x.t, id}]
	if !ok {
		return o, model.ErrNotFound
	}
	return o, nil
}

func (x *tx) ListOutlets(_ context.Context, scope auth.OutletScope, kind string, after ids.ID, limit int) ([]model.Outlet, error) {
	var out []model.Outlet
	for k, o := range x.st.outlets {
		if k.t != x.t || o.ID <= after || (kind != "" && o.Kind != kind) {
			continue
		}
		if scope.All() || scope.Contains(o.ID.String()) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

func (x *tx) UpdateOutlet(_ context.Context, o model.Outlet) error {
	if _, ok := x.st.outlets[key{x.t, o.ID}]; !ok {
		return model.ErrNotFound
	}
	x.st.outlets[key{x.t, o.ID}] = o
	return nil
}

// checkGSTIN enforces the constraints Postgres holds with unique indexes (ADR-0003 A1.1 item 2).
func (x *tx) checkGSTIN(r model.Registration) error {
	if r.GSTIN == "" || r.Status == model.RegClosed {
		return nil
	}
	for k, e := range x.st.regs {
		if e.ID == r.ID && k.t == x.t || e.GSTIN != r.GSTIN || e.Status == model.RegClosed {
			continue
		}
		if k.t == x.t {
			return model.ErrDuplicateGSTIN
		}
		if r.Verification == model.Verified && e.Verification == model.Verified {
			return model.ErrVerifiedElsewhere
		}
	}
	// A verified record elsewhere also blocks this one becoming verified within the same organisation.
	return nil
}

func (x *tx) InsertRegistration(_ context.Context, r model.Registration) error {
	if _, ok := x.st.regs[key{x.t, r.ID}]; ok {
		return model.ErrIDConflict
	}
	if err := x.checkGSTIN(r); err != nil {
		return err
	}
	x.st.regs[key{x.t, r.ID}] = r
	return nil
}

func (x *tx) GetRegistration(_ context.Context, id ids.ID) (model.Registration, error) {
	r, ok := x.st.regs[key{x.t, id}]
	if !ok {
		return r, model.ErrNotFound
	}
	return r, nil
}

func (x *tx) ListRegistrations(_ context.Context, after ids.ID, limit int) ([]model.Registration, error) {
	var out []model.Registration
	for k, r := range x.st.regs {
		if k.t == x.t && r.ID > after {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

func (x *tx) UpdateRegistration(_ context.Context, r model.Registration) error {
	if _, ok := x.st.regs[key{x.t, r.ID}]; !ok {
		return model.ErrNotFound
	}
	if err := x.checkGSTIN(r); err != nil {
		return err
	}
	x.st.regs[key{x.t, r.ID}] = r
	return nil
}

func (x *tx) SeriesForDevice(_ context.Context, outletID, deviceID ids.ID) ([]model.Series, error) {
	var out []model.Series
	for k, s := range x.st.series {
		if k.t == x.t && s.OutletID == outletID && s.DeviceID == deviceID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (x *tx) ListSeries(_ context.Context, scope auth.OutletScope, status string, after ids.ID, limit int) ([]model.Series, error) {
	var out []model.Series
	for k, s := range x.st.series {
		if k.t != x.t || s.ID <= after || (status != "" && s.Status != status) {
			continue
		}
		if scope.All() || scope.Contains(s.OutletID.String()) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

func (x *tx) GetSeries(_ context.Context, id ids.ID) (model.Series, error) {
	s, ok := x.st.series[key{x.t, id}]
	if !ok {
		return s, model.ErrNotFound
	}
	return s, nil
}

func (x *tx) InsertSeries(_ context.Context, s model.Series) error {
	for k, b := range x.st.com.books {
		if k.t == x.t && b.TaxRegistrationID == s.TaxRegistrationID && b.Code == s.Code {
			return model.ErrSeriesCollision // a registered bill book owns the code
		}
	}
	for k, e := range x.st.series {
		if e.Code != s.Code {
			continue
		}
		if k.t == x.t && e.TaxRegistrationID == s.TaxRegistrationID || (s.GSTIN != "" && e.GSTIN == s.GSTIN) {
			return model.ErrSeriesCollision
		}
	}
	x.st.series[key{x.t, s.ID}] = s
	return nil
}

func (x *tx) UpdateSeries(_ context.Context, s model.Series) error {
	if _, ok := x.st.series[key{x.t, s.ID}]; !ok {
		return model.ErrNotFound
	}
	x.st.series[key{x.t, s.ID}] = s
	return nil
}

func (x *tx) CloseDeviceSeries(_ context.Context, deviceID ids.ID, now time.Time) (int, int, error) {
	closed, total := 0, 0
	for k, s := range x.st.series {
		if k.t != x.t || s.DeviceID != deviceID {
			continue
		}
		total++
		if s.Status == model.SeriesActive || s.Status == model.SeriesDormant {
			s.Status, s.ClosedAt = model.SeriesClosed, &now
			x.st.series[k] = s
			closed++
		}
	}
	return closed, total, nil
}

func (x *tx) CloseRegistrationSeries(_ context.Context, regID ids.ID, now time.Time) ([]model.Series, error) {
	var closed []model.Series
	for k, s := range x.st.series {
		if k.t == x.t && s.TaxRegistrationID == regID && (s.Status == model.SeriesActive || s.Status == model.SeriesDormant) {
			s.Status, s.ClosedAt = model.SeriesClosed, &now
			x.st.series[k] = s
			closed = append(closed, s)
		}
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].ID < closed[j].ID })
	return closed, nil
}

func (x *tx) NextCodeIndex(_ context.Context, reg ids.ID) (int, error) {
	return x.st.alloc[key{x.t, reg}], nil
}

func (x *tx) SetCodeIndex(_ context.Context, reg ids.ID, next int, _ time.Time) error {
	if next > x.st.alloc[key{x.t, reg}] {
		x.st.alloc[key{x.t, reg}] = next
	}
	return nil
}

func (x *tx) CountIssued(_ context.Context, reg ids.ID, from, to time.Time) (int, error) {
	n := 0
	for k, s := range x.st.series {
		if k.t == x.t && s.TaxRegistrationID == reg && !s.IssuedAt.Before(from) && s.IssuedAt.Before(to) {
			n++
		}
	}
	return n, nil
}

func (x *tx) MaxCodeIndexForGSTIN(_ context.Context, gstin string) (int, error) {
	max := -1
	for k, s := range x.st.series {
		if k.t == x.t && s.GSTIN == gstin && s.CodeIndex > max {
			max = s.CodeIndex
		}
	}
	return max, nil
}

func (x *tx) ForceAdvanceAllocators(_ context.Context, n int, _ time.Time) error {
	for k, r := range x.st.regs {
		if k.t == x.t && r.Regime != "" {
			x.st.alloc[k] += n
		}
	}
	return nil
}

func (x *tx) IdemGet(_ context.Context, op, k string) (string, []byte, bool, error) {
	v, ok := x.st.idem[idemKey{x.t, op, k}]
	return v.fp, v.resp, ok && v.resp != nil, nil
}

func (x *tx) IdemPut(_ context.Context, op, k, fp string, resp []byte, _ time.Time) error {
	x.st.idem[idemKey{x.t, op, k}] = idemVal{fp: fp, resp: resp}
	return nil
}

func (x *tx) Publish(_ context.Context, e events.Envelope) error { return x.st.publish(x.t, e) }

// ---- links ----

func (x *tx) visible(l model.Link) bool {
	return l.ProposerID == x.t || (l.AcceptorID != "" && l.AcceptorID == x.t) || l.ID == x.inviteLink
}

func (x *tx) InsertLink(_ context.Context, l model.Link, hash []byte, expires time.Time) error {
	if _, ok := x.st.links[l.ID]; ok {
		return model.ErrIDConflict
	}
	x.st.links[l.ID] = cloneLink(l)
	x.st.invites[string(hash)] = &invite{link: l.ID, expires: expires}
	return nil
}

func (x *tx) GetLink(_ context.Context, id ids.ID, _ bool) (model.Link, error) {
	l, ok := x.st.links[id]
	if !ok || !x.visible(l) {
		return model.Link{}, model.ErrNotFound
	}
	return cloneLink(l), nil
}

func (x *tx) FindLinkByInvite(_ context.Context, hash []byte) (ids.ID, error) {
	iv, ok := x.st.invites[string(hash)]
	if !ok || iv.used {
		return "", model.ErrNotFound
	}
	x.inviteLink = iv.link
	return iv.link, nil
}

func (x *tx) UseInvite(_ context.Context, hash []byte, _ time.Time) (bool, error) {
	iv, ok := x.st.invites[string(hash)]
	if !ok || iv.used {
		return false, nil
	}
	iv.used = true
	return true, nil
}

func (x *tx) ListLinks(_ context.Context, status string, after ids.ID, limit int) ([]model.Link, error) {
	var out []model.Link
	for id, l := range x.st.links {
		if id > after && (l.ProposerID == x.t || l.AcceptorID == x.t) && (status == "" || l.Status == status) {
			out = append(out, cloneLink(l))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

func (x *tx) SaveLink(_ context.Context, l model.Link) error {
	cur, ok := x.st.links[l.ID]
	if !ok || !x.visible(cur) {
		return model.ErrNotFound
	}
	x.st.links[l.ID] = cloneLink(l)
	return nil
}

func (x *tx) InsertAmendment(_ context.Context, a model.Amendment) error {
	x.st.amends[key{a.LinkID, a.ID}] = a
	return nil
}

func (x *tx) GetAmendment(_ context.Context, linkID, id ids.ID) (model.Amendment, error) {
	a, ok := x.st.amends[key{linkID, id}]
	if l, lok := x.st.links[linkID]; !ok || !lok || !x.visible(l) {
		return a, model.ErrNotFound
	}
	return a, nil
}

func (x *tx) SaveAmendment(_ context.Context, a model.Amendment) error {
	x.st.amends[key{a.LinkID, a.ID}] = a
	return nil
}

func (x *tx) AppendActivity(_ context.Context, a model.Activity) error {
	x.st.activity = append(x.st.activity, a)
	return nil
}

func (x *tx) ListActivity(_ context.Context, linkID, after ids.ID, limit int) ([]model.Activity, error) {
	var out []model.Activity
	if l, ok := x.st.links[linkID]; !ok || !x.visible(l) {
		return nil, nil
	}
	for _, a := range x.st.activity {
		if a.LinkID == linkID && a.ID > after {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return cut(out, limit), nil
}

func (x *tx) ExpireDueLinks(_ context.Context, now time.Time) ([]model.Link, error) {
	var out []model.Link
	for id, l := range x.st.links {
		if (l.ProposerID == x.t || l.AcceptorID == x.t) && (l.Status == model.LinkProposed || l.Status == model.LinkAccepted) && !now.Before(l.ExpiresAt) {
			l.Status = model.LinkExpired
			x.st.links[id] = l
			out = append(out, cloneLink(l))
		}
	}
	return out, nil
}
