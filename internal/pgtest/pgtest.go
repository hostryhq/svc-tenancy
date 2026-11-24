// Package pgtest gives integration tests a throw-away Postgres database on the local dev stack
// (infra/dev/compose.yaml). Each call creates a fresh database owned by the migrator role, applies the
// migrations as the migrator, and returns pools for the runtime roles, so tests see exactly the
// privileges of production: the application role is not an owner and has no BYPASSRLS.
//
// Connection: TENANCY_TEST_PG_ADMIN_DSN (a superuser DSN used only to create roles and databases);
// default is the dev compose Postgres on 127.0.0.1:55432. The role passwords below are throw-away values
// for the local test database only.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/go-common/ids"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hostryhq/svc-tenancy/repository/migrations"
)

const (
	defaultAdmin = "postgres://hostry:hostry-dev-only@127.0.0.1:55432/hostry?sslmode=disable"
	devPassword  = "tenancy-local-test-only"
)

// Roles of the service (see scripts/provision-roles.sql).
const (
	Migrator = "tenancy_migrator"
	App      = "tenancy_app"
	Vendor   = "tenancy_vendor"
	Relay    = "tenancy_relay"
)

// DB is a fresh test database.
type DB struct {
	Name     string
	Admin    *sql.DB // superuser pool on the new database (for tests that must go around the roles)
	Migrator *sql.DB
	App      *sql.DB
	Vendor   *sql.DB
	Relay    *sql.DB
	dsn      func(user string) string
}

// DSN returns a connection string for a role on the test database.
func (d *DB) DSN(role string) string { return d.dsn(role) }

func adminDSN() string {
	if v := os.Getenv("TENANCY_TEST_PG_ADMIN_DSN"); v != "" {
		return v
	}
	return defaultAdmin
}

func withUser(base, db, user, pass string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

// New creates the database, roles and schema, and registers cleanup.
func New(t testing.TB) *DB {
	t.Helper()
	ctx := context.Background()
	admin, err := sql.Open("pgx", adminDSN())
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("no Postgres for integration tests (%v); start infra/dev compose or set TENANCY_TEST_PG_ADMIN_DSN", err)
	}
	if err := provisionRoles(ctx, admin); err != nil {
		t.Fatal(err)
	}
	name := "tenancy_test_" + strings.ReplaceAll(ids.New().String()[24:], "-", "")
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, name, Migrator)); err != nil {
		t.Fatal(err)
	}
	d := &DB{Name: name}
	d.dsn = func(user string) string { return withUser(adminDSN(), name, user, devPassword) }
	open := func(user string, max int) *sql.DB {
		db, err := sql.Open("pgx", d.dsn(user))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(max)
		return db
	}
	d.Migrator, d.App, d.Vendor, d.Relay = open(Migrator, 2), open(App, 20), open(Vendor, 4), open(Relay, 2)
	if d.Admin, err = sql.Open("pgx", withUser(adminDSN(), name, "", "")); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := migrations.Apply(cctx, d.Migrator); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		for _, db := range []*sql.DB{d.Migrator, d.App, d.Vendor, d.Relay, d.Admin} {
			db.Close()
		}
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
		admin.Close()
	})
	return d
}

// provisionRoles creates the cluster roles if missing (idempotent, serialised across test processes).
func provisionRoles(ctx context.Context, admin *sql.DB) error {
	conn, err := admin.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(7464011002)`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(7464011002)`) }()
	for _, r := range []string{Migrator, App, Vendor, Relay} {
		var exists bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, r).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := conn.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD '%s'`, r, devPassword)); err != nil {
				return err
			}
		}
	}
	return nil
}
