//go:build integration

package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/config"
	"github.com/hostryhq/svc-tenancy/internal/pgtest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

func jwks(t *testing.T, keys map[string]*ecdsa.PublicKey) string {
	t.Helper()
	var doc struct {
		Keys []map[string]string `json:"keys"`
	}
	for kid, k := range keys {
		raw, err := k.Bytes() // 0x04 || X || Y
		if err != nil {
			t.Fatal(err)
		}
		doc.Keys = append(doc.Keys, map[string]string{"kty": "EC", "crv": "P-256", "kid": kid,
			"x": base64.RawURLEncoding.EncodeToString(raw[1:33]), "y": base64.RawURLEncoding.EncodeToString(raw[33:65])})
	}
	b, _ := json.Marshal(doc)
	p := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The process wiring end to end: the runtime-role check, JWKS loading, the guard, the API and a clean
// shutdown, against a real database migrated by the migrator.
func TestServeWiring(t *testing.T) {
	db := pgtest.New(t)
	tk := double.NewTokens()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg := config.Config{
		Addr: addr, AppDSN: db.DSN(pgtest.App), VendorDSN: db.DSN(pgtest.Vendor), LinksEnabled: true,
		TenantJWKS:  jwks(t, map[string]*ecdsa.PublicKey{tk.TenantIssuer.Kid: tk.TenantIssuer.PublicKey()}),
		VendorJWKS:  jwks(t, map[string]*ecdsa.PublicKey{tk.VendorIssuer.Kid: tk.VendorIssuer.PublicKey()}),
		ServiceKeys: jwks(t, tk.Registry.KeySet().Keys()), SelfName: "tenancy",
		TenantIssuer: "hostry-identity", TenantAudience: "hostry-tenant", VendorIssuer: "hostry-identity-vendor", VendorAudience: "hostry-vendor",
		KeyRefresh: time.Minute,
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.DiscardHandler)) }()
	var resp *http.Response
	for i := 0; i < 100; i++ {
		if resp, err = http.Get("http://" + addr + "/readyz"); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("service did not come up: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("readyz %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/v1/organisations/current", nil)
	req.Header.Set("Authorization", "Bearer "+tk.TenantToken(ids.ID(seed.Org)))
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != 404 { // a valid token for a tenant that does not exist: 404, not 401 or 500
		t.Fatalf("authenticated request: %d", r2.StatusCode)
	}
	stop()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no graceful shutdown")
	}

	// TC-01: the same wiring refuses a superuser.
	bad := cfg
	bad.AppDSN = db.DSN("")
	if err := serve(context.Background(), bad, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("serve must refuse a superuser runtime role")
	}
}
