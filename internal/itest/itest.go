// Package itest assembles the real service (Postgres-backed) for integration tests: a fresh database on
// the local dev stack (internal/pgtest), the wired handler, a fake clock for entity timestamps and the
// test token issuers. Other services' integration tests use the double instead.
package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"
	"github.com/hostryhq/go-common/testkit"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/app"
	"github.com/hostryhq/svc-tenancy/internal/pgtest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// Stack is the real service over a fresh database.
type Stack struct {
	DB    *pgtest.DB
	App   *app.App
	Srv   *httptest.Server
	Clock *testkit.FakeClock
	*double.Tokens
}

// New starts a stack with the link routes on.
func New(t testing.TB) *Stack {
	t.Helper()
	db := pgtest.New(t)
	clk := testkit.NewClock()
	clk.Set(seed.At)
	tk := double.NewTokens()
	a, err := app.Build(app.Options{
		AppDB: db.App, VendorDB: db.Vendor,
		Tenant: tk.TenantIssuer.Verifier(), Vendor: tk.VendorIssuer.Verifier(), Services: tk.Registry.Verifier("tenancy"),
		Clock: clk, AuthClock: clock.System, IDs: ids.NewGenerator(clk.Now, nil), Log: slog.New(slog.DiscardHandler), LinksEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Stack{DB: db, App: a, Srv: httptest.NewServer(a.Handler), Clock: clk, Tokens: tk}
	t.Cleanup(s.Srv.Close)
	return s
}

// Seed stores the example world (internal/seed).
func (s *Stack) Seed(t testing.TB) {
	t.Helper()
	if err := seed.ExampleWorld(context.Background(), s.App.Repo); err != nil {
		t.Fatal(err)
	}
}

// Response is a decoded HTTP answer.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// JSON decodes the body into a map.
func (r Response) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.Body)
	}
	return m
}

// Code returns the problem code of an error response ("" otherwise).
func (r Response) Code() string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.Body, &p)
	return p.Code
}

// Do sends a request; body may be nil, a string, []byte or any JSON-marshalable value. The Idempotency-Key
// header is added for mutating methods unless the headers already carry one.
func Do(t testing.TB, base, method, path string, headers http.Header, body any) Response {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewReader([]byte(b))
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead && headers.Get("Idempotency-Key") == "" {
		req.Header.Set("Idempotency-Key", ids.New().String())
	}
	for k, vs := range headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Body: b, Header: resp.Header}
}

// Bearer returns headers with a bearer token.
func Bearer(tok string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	return h
}

// Merge combines header sets (later wins).
func Merge(hs ...http.Header) http.Header {
	out := http.Header{}
	for _, h := range hs {
		for k, vs := range h {
			out[k] = vs
		}
	}
	return out
}
