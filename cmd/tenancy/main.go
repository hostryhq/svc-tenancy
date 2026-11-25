// Command tenancy is the tenancy service: organisations, brands, outlets, tax registrations, the
// document-series registry, link control plane and the vendor directory. Wiring only.
//
//	tenancy serve     (default) run the HTTP API and the outbox relay
//	tenancy migrate   apply the SQL migrations as the migrator role (separate step; never at serve time)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/bus"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/idempotency"
	"github.com/hostryhq/go-common/inbox"
	"github.com/hostryhq/go-common/logx"
	"github.com/hostryhq/go-common/outbox"

	"github.com/hostryhq/svc-tenancy/internal/app"
	"github.com/hostryhq/svc-tenancy/internal/config"
	"github.com/hostryhq/svc-tenancy/repository/migrations"
	"github.com/hostryhq/svc-tenancy/repository/pg"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load(os.LookupEnv)
	if cmd == "migrate" {
		err = migrateConfigErr(cfg)
	}
	log := logx.New(os.Stderr, slog.LevelInfo, "tenancy")
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "migrate":
		err = migrate(ctx, cfg)
	case "serve":
		err = serve(ctx, cfg, log)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

// migrateConfigErr: migrating needs only the migrator DSN, so the serving settings may be absent.
func migrateConfigErr(c config.Config) error {
	if c.MigrateDSN == "" {
		return errors.New("TENANCY_DB_MIGRATE_DSN is required for migrate")
	}
	return nil
}

func migrate(ctx context.Context, cfg config.Config) error {
	db, err := pg.Open(cfg.MigrateDSN, 1)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return migrations.Apply(ctx, db)
}

// keyLoader loads a JWKS document from a URL or a file path.
type keyLoader struct{ src string }

func (k keyLoader) Load(ctx context.Context) (auth.KeyDoc, error) {
	var b []byte
	if len(k.src) > 7 && (k.src[:7] == "http://" || k.src[:8] == "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.src, nil)
		if err != nil {
			return auth.KeyDoc{}, err
		}
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Do(req)
		if err != nil {
			return auth.KeyDoc{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		if b, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
			return auth.KeyDoc{}, err
		}
	} else {
		var err error
		if b, err = os.ReadFile(k.src); err != nil {
			return auth.KeyDoc{}, err
		}
	}
	keys, err := auth.ParseJWKS(b)
	return auth.KeyDoc{Keys: keys}, err
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	appDB, err := pg.Open(cfg.AppDSN, 20)
	if err != nil {
		return err
	}
	defer func() { _ = appDB.Close() }()
	vendorDB, err := pg.Open(cfg.VendorDSN, 4)
	if err != nil {
		return err
	}
	defer func() { _ = vendorDB.Close() }()
	// TC-01: refuse to run as a superuser, a BYPASSRLS role or a table owner.
	for name, db := range map[string]*sql.DB{"app": appDB, "vendor": vendorDB} {
		if err := pg.VerifyRuntimeRole(ctx, db); err != nil {
			return fmt.Errorf("%s pool: %w", name, err)
		}
	}

	tenantKeys := auth.NewCachedKeySet(keyLoader{cfg.TenantJWKS}, log)
	vendorKeys := auth.NewCachedKeySet(keyLoader{cfg.VendorJWKS}, log)
	for _, ks := range []*auth.CachedKeySet{tenantKeys, vendorKeys} {
		if err := ks.Refresh(ctx); err != nil {
			return fmt.Errorf("load signing keys: %w", err)
		}
		go ks.Run(ctx, cfg.KeyRefresh)
	}
	svcDoc, err := keyLoader{cfg.ServiceKeys}.Load(ctx)
	if err != nil {
		return fmt.Errorf("load service keys: %w", err)
	}

	a, err := app.Build(app.Options{
		AppDB: appDB, VendorDB: vendorDB, Log: log, LinksEnabled: cfg.LinksEnabled,
		Tenant:   &auth.Verifier{Keys: tenantKeys, Issuer: cfg.TenantIssuer, Audience: cfg.TenantAudience, Realm: auth.RealmTenant},
		Vendor:   &auth.Verifier{Keys: vendorKeys, Issuer: cfg.VendorIssuer, Audience: cfg.VendorAudience, Realm: auth.RealmVendor},
		Services: &auth.ServiceVerifier{Registry: auth.NewStaticKeySet(svcDoc), Self: cfg.SelfName},
		// Vendor approval grants are signed with identity's vendor-realm key (ADR-0014 A2.6): the same key set
		// and issuer as vendor tokens; the audience is this service. The service consumes the grant id itself.
		Grants: &auth.GrantVerifier{Keys: vendorKeys, Issuer: cfg.VendorIssuer, Audience: cfg.SelfName},
	})
	if err != nil {
		return err
	}

	if cfg.NatsURL != "" && cfg.RelayDSN != "" {
		go relay(ctx, cfg, log)
	} else {
		log.Warn("outbox relay disabled: NATS_URL or TENANCY_DB_RELAY_DSN not set; events stay in the outbox")
	}
	if cfg.LinksEnabled {
		go expireLoop(ctx, a, log)
	}
	if cfg.NatsURL != "" {
		for _, publisher := range consumedStreams {
			go consume(ctx, cfg, publisher, a, appDB, log)
		}
	} else {
		log.Warn("event consumer disabled: NATS_URL not set; the open-trading-day guard sees no trading state and series tails are not confirmed")
	}

	srv := &http.Server{Addr: cfg.Addr, Handler: a.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("tenancy listening", "addr", cfg.Addr, "links_enabled", cfg.LinksEnabled)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

func relay(ctx context.Context, cfg config.Config, log *slog.Logger) {
	db, err := pg.Open(cfg.RelayDSN, 2)
	if err != nil {
		log.Error("relay: open database", "error", err)
		return
	}
	defer func() { _ = db.Close() }()
	if err := pg.VerifyRuntimeRole(ctx, db); err != nil {
		log.Error("relay: refusing to start", "error", err)
		return
	}
	nc, err := bus.Connect(cfg.NatsURL, "tenancy", cfg.NatsSeedFile)
	if err != nil {
		log.Error("relay: connect to the bus", "error", err)
		return
	}
	defer nc.Close()
	go idempotencySweepLoop(ctx, db, log)
	go inboxSweepLoop(ctx, db, log)
	pub, err := bus.NewPublisher(nc)
	if err != nil {
		log.Error("relay: publisher", "error", err)
		return
	}
	r := &outbox.Relay{DB: db, Sink: pub, LockKey: 7_464_011_003, Log: log}
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("relay stopped", "error", err)
	}
}

// consumedStreams are the publishers tenancy applies events from: billing (trading_day.changed, series.reconciled)
// and identity (device.drained, the series tail of a revoked device).
var consumedStreams = []string{"billing", "identity"}

// sweepEvery is the period of the idempotency and inbox sweeps (a variable so the wiring test can shorten it).
var sweepEvery = time.Hour

// consume reads one publisher's events from the bus. Each is
// applied in one transaction with its inbox row, acknowledged after commit, redelivered on failure and parked
// after the last attempt (ADR-0011 B5).
func consume(ctx context.Context, cfg config.Config, publisher string, a *app.App, db *sql.DB, log *slog.Logger) {
	repo, ok := a.Repo.(*pg.Repo)
	if !ok {
		log.Error("event consumer: " + publisher + ": repository is not Postgres")
		return
	}
	nc, err := bus.Connect(cfg.NatsURL, "tenancy", cfg.NatsSeedFile)
	if err != nil {
		log.Error("event consumer: "+publisher+": connect to the bus", "error", err)
		return
	}
	defer nc.Close()
	c, err := bus.BindConsumer(ctx, nc, publisher, "tenancy")
	if err != nil {
		log.Error("event consumer: "+publisher+": bind", "error", err)
		return
	}
	sub := &inbox.Subscriber{DB: db, Consumer: publisher, Log: log, Handle: func(ctx context.Context, tx *sql.Tx, e events.Envelope) error {
		rtx, err := repo.Adopt(ctx, tx, e.TenantID)
		if err != nil {
			return err
		}
		return a.Service.ApplyEvent(ctx, rtx, e)
	}}
	if err := sub.Run(ctx, c.Source(5*time.Second)); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("event consumer stopped", "error", err)
	}
}

func expireLoop(ctx context.Context, a *app.App, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := a.Service.ExpireDueLinks(ctx); err != nil {
				log.Error("link expiry sweep failed", "error", err)
			} else if n > 0 {
				log.Info("links expired", "count", n)
			}
		}
	}
}

// idempotencySweepLoop deletes expired idempotency records (24 h retention) once an hour, as the relay role.
func idempotencySweepLoop(ctx context.Context, db *sql.DB, log *slog.Logger) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := idempotency.Sweep(ctx, db, idempotency.PGOptions{}); err != nil {
				log.Error("idempotency sweep failed", "error", err)
			} else if n > 0 {
				log.Info("idempotency records expired", "count", n)
			}
		}
	}
}

// inboxSweepLoop removes inbox rows older than their retention (8 days) once an hour, as the relay role, the
// only role that may delete them.
func inboxSweepLoop(ctx context.Context, db *sql.DB, log *slog.Logger) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	sub := &inbox.Subscriber{DB: db}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sub.Cleanup(ctx); err != nil {
				log.Error("inbox sweep failed", "error", err)
			}
		}
	}
}
