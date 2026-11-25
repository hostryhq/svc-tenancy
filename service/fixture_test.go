package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/apperr"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/tenancy"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository/mem"
	"github.com/hostryhq/svc-tenancy/service"
)

// fixture is the service over the in-memory repository with three seeded organisations.
type fixture struct {
	t     *testing.T
	svc   *service.Service
	store *mem.Store
	clk   *testkit.FakeClock
	cat   *auth.Catalogue
	iss   *authtest.Issuer
	rnd   *countReader
	gen   *ids.Generator
}

type countReader struct{ n byte }

func (c *countReader) Read(p []byte) (int, error) {
	for i := range p {
		c.n += 7
		p[i] = c.n
	}
	return len(p), nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cat, err := gen.Catalogue()
	if err != nil {
		t.Fatal(err)
	}
	clk := testkit.NewClock()
	clk.Set(seed.At)
	gens := clk.IDs()
	store := mem.New(gens)
	f := &fixture{t: t, store: store, clk: clk, cat: cat, iss: authtest.NewTenantIssuer(clock.System), rnd: &countReader{}, gen: gens}
	f.svc = service.New(store, service.Options{Clock: clk, IDs: gens, Log: slog.New(slog.DiscardHandler), Rand: f.rnd})
	for _, w := range []seed.World{seed.Example, seed.Other, seed.Third} {
		if err := w.Apply(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// as returns a context for an organisation-wide member of tenant (every permission, every outlet).
func (f *fixture) as(tenant ids.ID) context.Context {
	return f.asGrants(tenant, authtest.OwnerGrants())
}

func (f *fixture) asGrants(tenant ids.ID, grants []auth.Grant) context.Context {
	return f.iss.ContextFor(context.Background(), f.cat, authtest.Spec{Type: auth.TypeUser, Tenant: tenant, Grants: grants})
}

// system is the context of a service-token call naming tenant (no member permissions).
func system(tenant ids.ID) context.Context {
	return tenancy.With(context.Background(), tenancy.Context{TenantID: tenant, ActorID: "identity"})
}

// problem returns the apperr of err, failing the test when err is not a typed error.
func problem(t *testing.T, err error) *apperr.Error {
	t.Helper()
	var e *apperr.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected a typed error, got %v", err)
	}
	return e
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got success", code)
	}
	if got := problem(t, err).Code; got != code {
		t.Fatalf("error code %q, want %q (%v)", got, code, err)
	}
}

func wantField(t *testing.T, err error, field, code string) {
	t.Helper()
	e := problem(t, err)
	for _, f := range e.Fields {
		if f.Field == field && f.Code == code {
			return
		}
	}
	t.Fatalf("no field error %s/%s in %v", field, code, e.Fields)
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newID returns a fresh UUIDv7 for client-generated ids.
func (f *fixture) newID() ids.ID { return f.gen.New() }
