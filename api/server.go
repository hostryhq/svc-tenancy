// Package api is the controller layer: it authenticates through go-auth, validates the transport
// (strict JSON, Idempotency-Key, path ids), calls the service and maps results to the generated DTOs.
// It holds no business rule. Routes and their permissions are generated from the OpenAPI document
// (internal/gen.Policies), so a route cannot be registered without a declared permission.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/health"
	"github.com/hostryhq/go-common/httpx"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/page"
	"github.com/hostryhq/svc-tenancy/service"
)

// Config wires the handler.
type Config struct {
	Service *service.Service
	Guard   *auth.Guard
	Clock   clock.Clock
	Log     *slog.Logger
	// LinksEnabled registers the tenant-facing link routes (LINKS_ENABLED; default false).
	LinksEnabled bool
	// AnyService lists the service names x-service-callers "*" stands for.
	AnyService []string
	// Health, when set, mounts /healthz and /readyz.
	Health *health.Handler
}

// Server implements gen.ServerInterface.
type Server struct {
	svc *service.Service
	clk clock.Clock
	log *slog.Logger
}

var _ gen.ServerInterface = (*Server)(nil)

// New builds the HTTP handler: request id, panic recovery, generated routes behind the go-auth guard.
func New(cfg Config) http.Handler {
	if cfg.Clock == nil {
		cfg.Clock = clock.System
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{svc: cfg.Service, clk: cfg.Clock, log: cfg.Log}
	mux := http.NewServeMux()
	rt := &router{mux: mux, cfg: cfg}
	gen.HandlerWithOptions(s, gen.StdHTTPServerOptions{BaseRouter: rt, ErrorHandlerFunc: badParam})
	if cfg.Health != nil {
		cfg.Health.Register(mux)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apperr.WriteProblem(w, r, apperr.New(apperr.NotFound, "not_found", ""))
	})
	return httpx.Chain(mux, httpx.RequestID, httpx.Recover)
}

// router registers each generated route behind the go-auth guard with the policy declared in the spec.
type router struct {
	mux *http.ServeMux
	cfg Config
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

func (rt *router) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	p, ok := gen.Policies[pattern]
	if !ok {
		panic("api: no policy generated for " + pattern)
	}
	if p.FeatureFlag == "LINKS_ENABLED" && !rt.cfg.LinksEnabled {
		return // the link routes exist only when the flag is on
	}
	if p.Permission == "authenticated" { // any signed-in tenant caller; no permission to check
		rt.mux.Handle(pattern, rt.authenticated(http.HandlerFunc(h)))
		return
	}
	pol := auth.Policy{OperationID: p.OperationID, Permission: p.Permission, StepUp: p.StepUp, Internal: p.Internal, ServiceOnly: p.ServiceOnly}
	for _, c := range p.Callers {
		if c == "*" {
			pol.Callers = append(pol.Callers, rt.cfg.AnyService...)
		} else {
			pol.Callers = append(pol.Callers, c)
		}
	}
	if p.VendorRealm {
		pol.Realm, pol.VendorAuditNone, pol.DirectoryClass = auth.RealmVendor, p.VendorAuditNone, p.DirectoryClass
	}
	rt.cfg.Guard.Handle(rt.mux, pattern, pol, http.HandlerFunc(h))
}

// authenticated admits any verified tenant-realm bearer token (reference data such as the state table).
func (rt *router) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") && rt.cfg.Guard.Tenant != nil {
			if _, err := rt.cfg.Guard.Tenant.Verify(strings.TrimSpace(h[7:])); err == nil {
				next.ServeHTTP(w, r)
				return
			}
		}
		apperr.WriteProblem(w, r, apperr.New(apperr.Unauthenticated, "auth.invalid_token", "invalid or missing credentials"))
	})
}

// badParam answers parameter binding errors from the generated wrappers.
func badParam(w http.ResponseWriter, r *http.Request, err error) {
	var req *gen.RequiredHeaderError
	if errors.As(err, &req) && strings.EqualFold(req.ParamName, idempotency.Header) {
		apperr.WriteProblem(w, r, idempotency.ErrKeyInvalid)
		return
	}
	var f apperr.FieldError
	var inv *gen.InvalidParamFormatError
	if errors.As(err, &inv) {
		f = apperr.FieldError{Field: inv.ParamName, Code: "invalid"}
	}
	var missing *gen.RequiredParamError
	if errors.As(err, &missing) {
		f = apperr.FieldError{Field: missing.ParamName, Code: "required"}
	}
	apperr.WriteProblem(w, r, apperr.New(apperr.Validation, "validation", "invalid request parameter").WithFields(f))
}

// ---- transport helpers ----

type ctxT = context.Context

const maxBody = 1 << 20

// begin prepares a mutating request: it decodes the body strictly into body (an unknown field, such as a
// tenant id, is a 422), validates the Idempotency-Key and puts the idempotency identity on the context.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, op string, body any) (context.Context, bool) {
	ctx := service.WithClientAddr(r.Context(), clientHost(r))
	var canonical []byte
	if body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			s.fail(w, r, apperr.New(apperr.Validation, "validation", "request body unreadable"))
			return nil, false
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(body); err != nil || dec.More() {
			s.fail(w, r, decodeError(err))
			return nil, false
		}
		canonical, _ = json.Marshal(body)
	}
	key, ok, err := idempotency.KeyFromRequest(r)
	if err != nil || !ok {
		s.fail(w, r, idempotency.ErrKeyInvalid)
		return nil, false
	}
	return service.WithIdem(ctx, service.Idem{Op: op, Key: key, Fingerprint: idempotency.Fingerprint(r.Method, r.URL.Path, canonical)}), true
}

func decodeError(err error) error {
	field := "body"
	code := "malformed"
	if err != nil {
		if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
			field, code = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`), "unknown_field"
		}
	}
	return apperr.New(apperr.Validation, "validation", "invalid request body").WithFields(apperr.FieldError{Field: field, Code: code})
}

func clientHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// pathID parses a path id. A malformed id is the same 404 as a missing one.
func (s *Server) pathID(w http.ResponseWriter, r *http.Request, raw string) (ids.ID, bool) {
	id, err := ids.Parse(raw)
	if err != nil {
		s.fail(w, r, apperr.New(apperr.NotFound, "not_found", ""))
		return "", false
	}
	return id, true
}

// fail writes err as a problem, adding Retry-After to rate-limited answers.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ra *service.RetryAfter
	if errors.As(err, &ra) {
		w.Header().Set("Retry-After", strconv.Itoa(ra.Seconds))
	}
	apperr.WriteProblem(w, r, err)
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pageParams validates cursor and limit.
func pageParams(cursor *string, limit *int, def, max int) (after ids.ID, n int, err error) {
	c, err := page.Decode(cursor)
	if err != nil {
		return "", 0, err
	}
	after = ids.ID(c)
	if c != "" {
		if after, err = cursorBound(c); err != nil {
			return "", 0, apperr.New(apperr.Validation, "validation", "invalid cursor").WithFields(apperr.FieldError{Field: "cursor", Code: "invalid"})
		}
	}
	n, err = page.Limit(limit, def, max)
	return after, n, err
}

// cursorBound turns the id inside a cursor into a bound that is safe to compare with stored ids. A full
// id is used as is; a hex prefix of an id (cursors are opaque, and clients may hold truncated ones) is
// padded with zeros, which orders just before every id starting with that prefix.
func cursorBound(c string) (ids.ID, error) {
	if id, err := ids.Parse(c); err == nil {
		return id, nil
	}
	hex := strings.ReplaceAll(c, "-", "")
	if len(hex) == 0 || len(hex) > 32 {
		return "", errBadCursor
	}
	for _, r := range hex {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return "", errBadCursor
		}
	}
	hex += strings.Repeat("0", 32-len(hex))
	return ids.ID(hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:]), nil
}

var errBadCursor = errors.New("bad cursor")
