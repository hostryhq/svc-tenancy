// Package service holds the business rules of the tenancy service. It has no HTTP and no SQL: it
// depends on repository interfaces, takes the tenant from the tenancy context and the caller's
// outlet scope from go-auth, and publishes events through the transactional outbox.
package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"

	tev "github.com/hostryhq/svc-tenancy/events"
	"github.com/hostryhq/svc-tenancy/internal/model"
	"github.com/hostryhq/svc-tenancy/repository"
)

// Service is the tenancy business layer.
type Service struct {
	repo  repository.Repository
	clock clock.Clock
	gen   *ids.Generator
	log   *slog.Logger
	rnd   io.Reader
	// linkScopes is the platform catalogue of link scopes (tenants cannot invent scopes).
	linkScopes map[string]bool
	limiter    *limiter
	// grants verifies the vendor approval grant of suspend, reinstate and close (ADR-0014 A2.6); nil refuses
	// every lifecycle action (fail closed).
	grants *auth.GrantVerifier
}

// Options configure a Service. Zero values pick production defaults.
type Options struct {
	Clock      clock.Clock
	IDs        *ids.Generator
	Log        *slog.Logger
	Rand       io.Reader // invitation codes; crypto/rand by default
	LinkScopes []string  // defaults to the Phase 1 test-only pair
	// Grants verifies vendor approval grants. The service consumes the grant id itself, in the action's
	// transaction, so the verifier's own Used hook must be nil.
	Grants *auth.GrantVerifier
}

// New builds a Service over repo.
func New(repo repository.Repository, o Options) *Service {
	s := &Service{repo: repo, clock: o.Clock, gen: o.IDs, log: o.Log, rnd: o.Rand, linkScopes: map[string]bool{}, grants: o.Grants}
	if s.clock == nil {
		s.clock = clock.System
	}
	if s.gen == nil {
		s.gen = ids.NewGenerator(s.clock.Now, nil)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.rnd == nil {
		s.rnd = rand.Reader
	}
	scopes := o.LinkScopes
	if len(scopes) == 0 {
		scopes = []string{"test.echo.read", "test.echo.post"}
	}
	for _, sc := range scopes {
		s.linkScopes[sc] = true
	}
	s.limiter = newLimiter(s.clock)
	return s
}

// Idem identifies one idempotent request: the operation, the client's key and the request fingerprint.
type Idem struct{ Op, Key, Fingerprint string }

type idemKey struct{}

// WithIdem marks ctx as carrying an Idempotency-Key request; the next transaction replays or records it.
func WithIdem(ctx context.Context, i Idem) context.Context {
	return context.WithValue(ctx, idemKey{}, i)
}

// IdemFrom returns the idempotency identity of the request in ctx.
func IdemFrom(ctx context.Context) (Idem, bool) { i, ok := ctx.Value(idemKey{}).(Idem); return i, ok }

func idemFrom(ctx context.Context) (Idem, bool) { return IdemFrom(ctx) }

type clientAddrKey struct{}

// WithClientAddr records the caller's network address, used for the invitation redemption rate limit.
func WithClientAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, clientAddrKey{}, addr)
}

func clientAddr(ctx context.Context) string { a, _ := ctx.Value(clientAddrKey{}).(string); return a }

// run executes fn in one transaction for the tenant in ctx. With an Idempotency-Key in ctx it first
// claims the key in the same transaction: a replay returns the stored result and runs nothing, so the
// effect and the key commit together and a retry can never repeat the effect. Errors are not stored.
func run[T any](ctx context.Context, s *Service, fn func(ctx context.Context, tx repository.Tx) (T, error)) (T, error) {
	var out T
	err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		v, err := claimed(ctx, s, tx, fn)
		out = v
		return err
	})
	var zero T
	if err != nil {
		return zero, err
	}
	return out, nil
}

// claimed runs fn inside tx, claiming the request's Idempotency-Key first when it has one.
func claimed[T any](ctx context.Context, s *Service, tx repository.Tx, fn func(ctx context.Context, tx repository.Tx) (T, error)) (T, error) {
	var out T
	im, ok := idemFrom(ctx)
	if !ok {
		return fn(ctx, tx)
	}
	fp, stored, found, err := tx.IdemGet(ctx, im.Op, im.Key)
	if err != nil {
		return out, err
	}
	if found {
		if fp != im.Fingerprint {
			return out, idempotency.ErrMismatch
		}
		return out, json.Unmarshal(stored, &out)
	}
	v, err := fn(ctx, tx)
	if err != nil {
		return out, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	return v, tx.IdemPut(ctx, im.Op, im.Key, im.Fingerprint, b, s.clock.Now())
}

// actor is the member, device or service performing the action (audit).
func actor(ctx context.Context) string {
	if tc, ok := tenancy.From(ctx); ok {
		return tc.ActorID
	}
	return ""
}

func tenantOf(ctx context.Context) (ids.ID, error) {
	tc, err := tenancy.Require(ctx)
	if err != nil {
		return "", err
	}
	return tc.TenantID, nil
}

// requireOutlet checks the caller's permission at an outlet read from the stored row. An outlet outside
// the caller's scope is the same 404 as a missing or foreign one (ADR-0003 A1.4 item 4).
func requireOutlet(ctx context.Context, perm string, outletID ids.ID) error {
	err := auth.Require(ctx, perm, outletID.String())
	var e *apperr.Error
	if errors.As(err, &e) && e.Kind == apperr.NotFound {
		return model.ErrNotFound
	}
	return err
}

// publish writes an event for the transaction's tenant.
func (s *Service) publish(ctx context.Context, tx repository.Tx, e events.Envelope) error {
	return tx.Publish(ctx, e)
}

func (s *Service) newID() ids.ID { return s.gen.New() }

// validID checks a client-supplied id (UUIDv7 only: ADR-0003's row-level policy casts tenant ids to uuid).
func validID(field string, id ids.ID) error {
	if _, err := ids.Parse(id.String()); err != nil {
		return model.Invalid(field, "uuidv7_required", "must be a UUIDv7")
	}
	return nil
}

func validName(field, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len([]rune(v)) > max {
		return "", model.Invalid(field, "length", "must be 1 to "+strconv.Itoa(max)+" characters")
	}
	return v, nil
}

// paginate trims rows fetched with limit+1 and reports whether another page exists.
func paginate[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// Page is one page of results with the id to continue after ("" when it is the last page).
type Page[T any] struct {
	Items []T
	Next  ids.ID
}

func pageOf[T any](rows []T, limit int, idOf func(T) ids.ID) Page[T] {
	items, more := paginate(rows, limit)
	p := Page[T]{Items: items}
	if more && len(items) > 0 {
		p.Next = idOf(items[len(items)-1])
	}
	return p
}

// limiter counts redemption attempts per key in a fixed window.
type limiter struct {
	mu     sync.Mutex
	clock  clock.Clock
	window time.Duration
	max    int
	hits   map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int
}

func newLimiter(c clock.Clock) *limiter {
	return &limiter{clock: c, window: 10 * time.Minute, max: 10, hits: map[string]*bucket{}}
}

// allow records an attempt and reports whether it is within the limit.
func (l *limiter) allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b := l.hits[key]
	if b == nil || now.Sub(b.start) >= l.window {
		b = &bucket{start: now}
		l.hits[key] = b
	}
	b.n++
	if b.n > l.max {
		return false, b.start.Add(l.window).Sub(now)
	}
	return true, 0
}

// RetryAfter is carried by rate-limited errors so the API can set Retry-After.
type RetryAfter struct {
	Err     *apperr.Error
	Seconds int
}

func (r *RetryAfter) Error() string { return r.Err.Error() }

// Unwrap exposes the typed error so it still maps to a 429 problem.
func (r *RetryAfter) Unwrap() error { return r.Err }

func apperrDenied() error {
	return apperr.New(apperr.Forbidden, auth.CodePermissionDenied, "permission denied")
}

// publishOutlet writes tenancy.outlet.created or .updated with the regime of the outlet's registration and the
// ids of its open series, so a consumer knows which codes to expect (ids only).
func (s *Service) publishOutlet(ctx context.Context, tx repository.Tx, typ string, tenant ids.ID, o model.Outlet) error {
	d := tev.OutletData{OutletID: o.ID, BrandID: o.BrandID, Kind: o.Kind, Status: o.Status, TaxRegistrationID: o.TaxRegistrationID}
	if reg, err := tx.GetRegistration(ctx, o.TaxRegistrationID); err == nil {
		d.TaxRegime = reg.Regime
	}
	sids, err := tx.OpenSeriesIDs(ctx, o.ID)
	if err != nil {
		return err
	}
	d.SeriesIDs = sids
	return s.publish(ctx, tx, tev.OutletEvent(s.newID(), typ, s.clock.Now(), actor(ctx), tenant, d))
}

// run executes fn for platform data of vendor routes that name no organisation (plans, GST configuration,
// the ledger export): one platform-audited transaction, with the Idempotency-Key claimed in it.
func runPlatform[T any](ctx context.Context, s *Service, fn func(ctx context.Context, tx repository.Tx) (T, error)) (T, error) {
	var out T
	err := s.repo.InPlatformOnlyTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		v, err := claimed(ctx, s, tx, fn)
		out = v
		return err
	})
	var zero T
	if err != nil {
		return zero, err
	}
	return out, nil
}
