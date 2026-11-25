# svc-tenancy

Tenancy service: organisations, brands, outlets and properties, tax registrations (GSTIN rules and vendor verification),
the document-series registry (with its tail fields and entry-only bill books), organisation settings, the link control plane
(behind `LINKS_ENABLED`, default off), the vendor directory, the organisation lifecycle (suspend, reinstate, close under a
vendor approval grant) and the commercial module (plans, Hostry's GST configuration, subscriptions and entitlements,
charges, payments recorded by hand, work lists, CSV export, "My plan"), with its test double. Contract: `api-specs/openapi/tenancy/v1` (the commit used is in
`internal/gen/SPEC_VERSION`). Decisions: ADR-0003 (with Amendment A1), ADR-0013, ADR-0011, ADR-0005 A2, ADR-0008 (Admin A1), ADR-0014 A1.2 to A1.3 and A2.5 to A2.6. Status of every
operation: [docs/operations-status.md](docs/operations-status.md). No deployment exists yet (ADR-0001 AM12).

## Layout

```
cmd/tenancy/            wiring only: serve, migrate
api/                    controllers: go-auth guard, strict JSON, Idempotency-Key, DTO mapping (no business rules)
service/                business rules; depends on repository interfaces; no HTTP, no SQL
repository/             interfaces; pg/ (Postgres), mem/ (in memory), migrations/ (forward-only SQL)
events/                 domain events written to the transactional outbox
double/                 in-memory test double: the real api + service over repository/mem; failure injection
internal/gen/           generated: oapi-codegen types and server, policy table, permission catalogue (make gen)
internal/httpsuite/     HTTP suites run against both the real service and the double
```

## Run

Configuration is environment only; the process fails fast and lists every problem.

| Variable | Meaning |
|---|---|
| `TENANCY_DB_APP_DSN`, `TENANCY_DB_VENDOR_DSN` | connect as `tenancy_app` and `tenancy_vendor` (required) |
| `TENANCY_DB_RELAY_DSN`, `NATS_URL`, `NATS_SEED_FILE` | outbox relay (`tenancy_relay`); unset means events stay in the outbox. `NATS_URL` also starts the consumer of billing's `trading_day.changed` and `series.reconciled` events (inbox in the app database); without it the open-trading-day guard sees no trading state |
| `TENANCY_DB_MIGRATE_DSN` | the migrator, used only by `tenancy migrate` |
| `TENANCY_TENANT_JWKS`, `TENANCY_VENDOR_JWKS`, `TENANCY_SERVICE_KEYS` | JWKS documents (URL or file) of the token issuers and the service key registry. Vendor approval grants are verified with the vendor key set, issuer `TENANCY_VENDOR_ISSUER` and audience `TENANCY_SELF_NAME` (ADR-0014 A2.6) |
| `LINKS_ENABLED` | register the tenant-facing link routes (default `false`) |

DSNs and key files are secrets: pass them by reference from the secret store, never inline. Roles are provisioned once per
cluster by `scripts/provision-roles.sql`; `tenancy migrate` then applies `repository/migrations` as the migrator. At start-up
the service refuses to run as a superuser, a `BYPASSRLS` role or a table owner.

```
tenancy migrate     # as the migrator role
tenancy serve       # the HTTP API (and the outbox relay when configured)
```

## Test

```
make test           # go test ./... : unit, double contract suite, HTTP suites on the double, lints
make race           # the same with -race
make dev-up         # Postgres from ../infra/dev, own compose project, host port 55432
make integration    # -tags integration: real Postgres and the real service (contract, isolation, roles, registry)
make dev-down
make lint           # vet, golangci-lint (if installed), the outlet-scope lint (ADR-0003 A1.4 item 5)
make gen            # regenerate internal/gen from ../api-specs (pinned oapi-codegen)
```

Integration tests create a throw-away database per test on the dev Postgres (`TENANCY_TEST_PG_ADMIN_DSN`, default
`postgres://hostry:...@127.0.0.1:55432/hostry`); they skip when no database is reachable. Local development against unmerged
library branches uses an uncommitted `go.work` (git-ignored) listing `.` and the sibling checkouts, with
`GOPRIVATE=github.com/hostryhq/*`.

## Using the double

```go
env, _ := double.NewEnv()          // double plus the test issuers it trusts
env.SeedExampleWorld(ctx)          // the organisation the spec examples refer to
srv := env.Server()                // httptest server
tok := env.TenantToken(seed.Org)   // owner token; env.VendorHeaders(...), env.ServiceHeaders("identity", tenant), ...
env.Inject(double.Fault{Match: ..., Status: 503})   // failure injection: status, delay or hang
env.SeedWorld(ctx, "subscribed")   // an x-example-world of the spec; the double's clock moves to the world's day
env.Deliver(ctx, billingEvent)     // billing.trading_day.changed / billing.series.reconciled, as the bus consumer applies them
grant := env.MintGrant(double.GrantSpec{Kind: "org_suspend", Org: orgID, Now: env.Now()}) // a vendor approval grant for the lifecycle routes
```

Lifecycle routes (`suspendOrganisation`, `reinstateOrganisation`, `closeOrganisation`) need the grant in `Hostry-Vendor-Approval`
and a vendor token whose subject is the grant's approver (`env.VendorHeadersAs(approver, "admin.org.lifecycle.approve")`).

## Notes for operators

- Isolation: shared schema, `tenant_id` on every tenant-owned row, row-level security forced with one standard policy; the
  tenant is set per transaction by go-common's helper. Outlet scoping inside a tenant is service code plus the scope lint and
  the generated T14 test. Directory tables (vendor realm) carry two policies and are read through `tenancy_vendor`.
- Idempotency keys are scoped by tenant, operation and key; the response of a replay is stored with the key, in the same
  transaction as the effect. A one-time secret is never stored: a replay of `proposeLink` or `reissueLinkInvitation`
  returns the link with `secret_withheld`. Vendor routes that name no organisation (plans, GST configuration, ledger
  export) keep their records under the reserved platform tenant id (all zeros), which no organisation has.
- Platform data (`plans`, `plan_versions`, `billing_configurations`) belongs to no organisation: no tenant column, no
  row-level security, written only through vendor routes. The migration seeds the plan catalogue as drafts, so nothing is
  sellable until a version is activated. Cross-organisation work lists read `directory_subscriptions` and `directory_charges`,
  which are rewritten in the transaction of every ledger change and are the only ledger tables `tenancy_vendor` can read.
- Billing never gates trading: an overdue charge is a label, entitlements carry no expiry, and nothing is switched off
  automatically (ADR-0008 S5). Suspending needs a two-person approval and never blocks a trading day in progress.
- GSTINs never appear in logs or event payloads.
