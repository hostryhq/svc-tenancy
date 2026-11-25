//go:build integration

package main

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hostryhq/go-common/bus"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/config"
	"github.com/hostryhq/svc-tenancy/internal/pgtest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository/pg"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func freeAddr(t *testing.T) (host string, port int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), l.Addr().(*net.TCPAddr).Port
}

// startNATS starts only the NATS service of the infra dev compose under a project of its own and a free port,
// with accounts generated from the api-specs event schemas, and provisions the streams and consumers.
// It skips when Docker or the sibling checkouts are missing. HOSTRY_INFRA_DIR and HOSTRY_EVENTS_DIR override
// the default sibling locations.
func startNATS(t *testing.T) (url, seedDir string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
	compose := filepath.Join(envOr("HOSTRY_INFRA_DIR", "../../../infra"), "dev", "compose.yaml")
	if _, err := os.Stat(compose); err != nil {
		t.Skip("infra/dev/compose.yaml not found (set HOSTRY_INFRA_DIR)")
	}
	reg, err := bus.LoadRegistry(eventsOf(t, envOr("HOSTRY_EVENTS_DIR", "../../../api-specs/events")))
	if err != nil {
		t.Skipf("api-specs events not loadable (set HOSTRY_EVENTS_DIR): %v", err)
	}
	conf := t.TempDir()
	keys, err := bus.GenerateKeys(reg.Services())
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.WriteDevFiles(conf, keys); err != nil {
		t.Fatal(err)
	}
	_, port := freeAddr(t)
	project := fmt.Sprintf("tenancywire%d", os.Getpid())
	env := append(os.Environ(), "HOSTRY_NATS_CONF_DIR="+conf, fmt.Sprintf("HOSTRY_NATS_PORT=%d", port))
	t.Cleanup(func() {
		down := exec.Command("docker", "compose", "-f", compose, "-p", project, "down", "-v", "--timeout", "2")
		down.Env = env
		_ = down.Run()
	})
	up := exec.Command("docker", "compose", "-f", compose, "-p", project, "up", "-d", "--wait", "nats")
	up.Env = env
	if out, err := up.CombinedOutput(); err != nil {
		t.Skipf("compose up nats failed: %v: %s", err, out)
	}
	url = fmt.Sprintf("nats://127.0.0.1:%d", port)
	admin, err := bus.ConnectAdmin(url, filepath.Join(conf, "seeds", bus.AdminUser+".nk"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := bus.Provision(ctx, admin, reg, bus.ProvisionOptions{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	return url, filepath.Join(conf, "seeds")
}

func countWhere(t *testing.T, db *sql.DB, query string, args ...any) (n int) {
	t.Helper()
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// The consumer and sweep wiring of serve: with NATS configured the service binds its durable consumers on the
// billing and identity streams, applies a published event in one transaction with its inbox row, and the relay
// role's inbox sweep removes rows past retention.
func TestServeConsumesBusAndSweepsInbox(t *testing.T) {
	natsURL, seeds := startNATS(t)
	db := pgtest.New(t)
	repo := pg.New(db.App, db.Vendor)
	if err := seed.Example.Apply(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	tk := double.NewTokens()
	addr, _ := freeAddr(t)
	sweepEvery = 200 * time.Millisecond
	t.Cleanup(func() { sweepEvery = time.Hour })
	cfg := config.Config{
		Addr: addr, AppDSN: db.DSN(pgtest.App), VendorDSN: db.DSN(pgtest.Vendor), RelayDSN: db.DSN(pgtest.Relay),
		TenantJWKS:  jwks(t, map[string]*ecdsa.PublicKey{tk.TenantIssuer.Kid: tk.TenantIssuer.PublicKey()}),
		VendorJWKS:  jwks(t, map[string]*ecdsa.PublicKey{tk.VendorIssuer.Kid: tk.VendorIssuer.PublicKey()}),
		ServiceKeys: jwks(t, tk.Registry.KeySet().Keys()), SelfName: "tenancy",
		TenantIssuer: "hostry-identity", TenantAudience: "hostry-tenant", VendorIssuer: "hostry-identity-vendor", VendorAudience: "hostry-vendor",
		KeyRefresh: time.Minute, NatsURL: natsURL, NatsSeedFile: filepath.Join(seeds, "tenancy.nk"),
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.DiscardHandler)) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("no graceful shutdown")
		}
	}()
	eventually(t, "service ready", func() bool {
		r, err := http.Get("http://" + addr + "/readyz")
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == 200
	})

	// An old inbox row (past the 8 day retention) is swept by the relay role; a fresh one is kept.
	old := ids.New()
	if _, err := db.Admin.Exec(`INSERT INTO inbox (consumer, event_id, processed_at) VALUES ('billing', $1, now() - interval '9 days')`, old); err != nil {
		t.Fatal(err)
	}

	// Publish billing.trading_day.changed as billing and see it applied.
	nc, err := bus.Connect(natsURL, "billing", filepath.Join(seeds, "billing.nk"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	pub, err := bus.NewPublisher(nc)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	e := events.Envelope{ID: ids.New(), Type: "billing.trading_day.changed", V: 1, Source: "billing", TenantID: seed.Org, OutletID: seed.Outlet,
		Key: seed.Outlet.String() + ":2026-11-24", KeySeq: 1, OccurredAt: at,
		Data: []byte(fmt.Sprintf(`{"outlet_id":%q,"business_date":"2026-11-24","state":"open","changed_at":%q}`, seed.Outlet, at.Format(time.RFC3339Nano)))}
	raw, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(context.Background(), "evt.billing.trading_day.changed.v1", e.ID.String(), raw); err != nil {
		t.Fatal(err)
	}
	eventually(t, "trading state applied and inbox row written", func() bool {
		return countWhere(t, db.Admin, `SELECT count(*) FROM outlet_trading_state WHERE outlet_id = $1 AND state = 'open' AND business_date = '2026-11-24'`, seed.Outlet) == 1 &&
			countWhere(t, db.Admin, `SELECT count(*) FROM inbox WHERE consumer = 'billing' AND event_id = $1`, e.ID) == 1
	})
	// The identity stream is consumed too (device.drained, the series tail of a revoked device).
	inc, err := bus.Connect(natsURL, "identity", filepath.Join(seeds, "identity.nk"))
	if err != nil {
		t.Fatal(err)
	}
	defer inc.Close()
	ipub, err := bus.NewPublisher(inc)
	if err != nil {
		t.Fatal(err)
	}
	d := events.Envelope{ID: ids.New(), Type: "identity.device.drained", V: 1, Source: "identity", TenantID: seed.Org, OutletID: seed.Outlet,
		Key: ids.New().String(), KeySeq: 1, OccurredAt: at,
		Data: []byte(fmt.Sprintf(`{"device_id":%q,"drained_at":%q,"series":[{"series_id":%q,"last_number":7}]}`, ids.New(), at.Format(time.RFC3339Nano), ids.New()))}
	raw, err = d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := ipub.Publish(context.Background(), "evt.identity.device.drained.v1", d.ID.String(), raw); err != nil {
		t.Fatal(err)
	}
	eventually(t, "drained event applied (inbox row of the identity consumer)", func() bool {
		return countWhere(t, db.Admin, `SELECT count(*) FROM inbox WHERE consumer = 'identity' AND event_id = $1`, d.ID) == 1
	})
	eventually(t, "inbox sweep removed the expired row", func() bool {
		return countWhere(t, db.Admin, `SELECT count(*) FROM inbox WHERE event_id = $1`, old) == 0
	})
	if n := countWhere(t, db.Admin, `SELECT count(*) FROM inbox WHERE event_id = $1`, e.ID); n != 1 {
		t.Fatalf("the sweep keeps rows within retention: %d", n)
	}
}

// eventsOf copies the event schemas of the services tenancy exchanges events with into a temporary directory.
// The sync schemas are left out: go-common's registry (92884d6) does not yet accept their two-segment type
// (sync.op), which is irrelevant to this test.
func eventsOf(t *testing.T, dir string) fs.FS {
	t.Helper()
	dst := t.TempDir()
	src := os.DirFS(dir)
	for _, svc := range []string{"billing", "identity", "tenancy"} {
		sub, err := fs.Sub(src, svc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(filepath.Join(dst, svc), sub); err != nil {
			t.Skipf("event schemas of %s not found in %s: %v", svc, dir, err)
		}
	}
	return os.DirFS(dst)
}
