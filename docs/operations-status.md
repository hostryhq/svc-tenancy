# Operations status

Every operationId of `api-specs/openapi/tenancy/v1/openapi.yaml` (commit recorded in `internal/gen/SPEC_VERSION`).
Status is **implemented** in the service (`api` -> `service` -> `repository/pg`), in the test double (`double/`, same API and
service over `repository/mem`) and covered by the shared contract suite (`contracttest`) in the example world the spec declares for it
(`SeedWorld`, `internal/seed/worlds.go`), on the real service and the double, with no operation skipped.
**Not yet implemented: none.**

Phase 2 adds the rows from `changeOutletTaxRegistration` down (spec commit `1462fb8`, ADR-0008 Admin A1, ADR-0014 A1.2 to A1.3, A2.5 to A2.6). `S` = scenario suite in `internal/httpsuite` (`Commercial`, `Lifecycle`, `SeriesTail`, `Books`, `LinksP2`, `PhaseTwoIsolation`), run against the real service and the double, responses checked against the OpenAPI document.

Contract column: `C` = example-driven suite in the operation's example world (both real service and double); `L` = link
lifecycle scenario suite (`internal/httpsuite/links.go`, every response validated against the OpenAPI document, real
service and double), which goes beyond the single-state examples.

| operationId | Method and path | Permission or caller | Status | Contract | Notes |
|---|---|---|---|---|---|
| getCurrentOrganisation | GET /v1/organisations/current | organisation.read | implemented | C | support-viewable |
| updateCurrentOrganisation | PATCH /v1/organisations/current | organisation.manage | implemented | C | publishes `tenancy.organisation.updated` (name) |
| listBrands | GET /v1/brands | brand.read | implemented | C | |
| createBrand | POST /v1/brands | brand.manage | implemented | C | same id and content is an idempotent repeat |
| getBrand | GET /v1/brands/{brand_id} | brand.read | implemented | C | |
| listOutlets | GET /v1/outlets | outlet.read | implemented | C | outlet scoped (T14) |
| createOutlet | POST /v1/outlets | outlet.create | implemented | C | needs an organisation-wide grant; publishes `tenancy.outlet.created` |
| getOutlet | GET /v1/outlets/{outlet_id} | outlet.read | implemented | C | out-of-scope outlet is 404 |
| updateOutlet | PATCH /v1/outlets/{outlet_id} | outlet.manage | implemented | C | publishes `tenancy.outlet.updated` |
| listTaxRegistrations | GET /v1/tax-registrations | tax_registration.read | implemented | C | |
| createTaxRegistration | POST /v1/tax-registrations | tax_registration.manage (step-up) | implemented | C | GSTIN format, check character, state; neutral when another organisation holds the GSTIN |
| getTaxRegistration | GET /v1/tax-registrations/{registration_id} | tax_registration.read | implemented | C | |
| updateTaxRegistration | PATCH /v1/tax-registrations/{registration_id} | tax_registration.manage (step-up) | implemented | C | 409 `registration_fixed` once verified or closed (a no-op still succeeds) |
| closeTaxRegistration | PUT /v1/tax-registrations/{registration_id}/closure | tax_registration.manage (step-up) | implemented | C | closes the registration and its dormant/active series; 409 `registration_in_use` while an active outlet references it; terminal, idempotent, no event; reason kept on the row, not in the body |
| listStates | GET /v1/states | authenticated (go-auth `Authenticated`) | implemented | C | static reference table (`internal/states`) |
| getOrganisationSettings | GET /v1/organisation-settings | settings.read | implemented | C | |
| updateOrganisationSettings | PUT /v1/organisation-settings | settings.manage | implemented | C | publishes `tenancy.organisation.updated` (settings) only on change |
| listDocumentSeries | GET /v1/document-series | series.read | implemented | C | outlet scoped; filter intersected with scope |
| closeDocumentSeriesUnreconciled | PUT /v1/document-series/{series_id}/closure | series.close_unreconciled | implemented | C | from any state |
| listLinks | GET /v1/links | link.read | implemented (flag) | C, L | routes exist only when `LINKS_ENABLED` is true |
| proposeLink | POST /v1/links | link.manage (step-up) | implemented (flag) | L | one-time 80-bit code, 14 days |
| getLink | GET /v1/links/{link_id} | link.read | implemented (flag) | L | |
| acceptLink | POST /v1/link-acceptances | link.manage (step-up) | implemented (flag) | L | rate limited per tenant and address; state `accepted` |
| confirmLink | PUT /v1/links/{link_id}/confirmation | link.manage (step-up) | implemented (flag) | L | proposer only; grants go live here |
| declineLink | PUT /v1/links/{link_id}/declination | link.manage (step-up) | implemented (flag) | L | |
| suspendLink | PUT /v1/links/{link_id}/suspension | link.manage (step-up) | implemented (flag) | L | |
| resumeLink | DELETE /v1/links/{link_id}/suspension | link.manage (step-up) | implemented (flag) | L | only the suspending party |
| revokeLink | PUT /v1/links/{link_id}/revocation | link.revoke (step-up) | implemented (flag) | L | terminal, idempotent |
| proposeLinkAmendment | POST /v1/links/{link_id}/amendments | link.manage (step-up) | implemented (flag) | L | |
| decideLinkAmendment | PUT /v1/links/{link_id}/amendments/{amendment_id}/decision | link.manage (step-up) | implemented (flag) | L | the other party decides |
| removeLinkGrant | DELETE /v1/links/{link_id}/grants/{grant_id} | link.manage (step-up) | implemented (flag) | L | unilateral, immediate |
| listLinkActivity | GET /v1/links/{link_id}/activity | link.read | implemented (flag) | L | append-only |
| issueDocumentSeries | POST /internal/v1/document-series-issuances | caller: identity | implemented | C | verified registration only; 200 per IST day; collision jump |
| closeDeviceSeries | PUT /internal/v1/devices/{device_id}/series-closure | caller: identity | implemented | C | |
| getLinkDecision | GET /internal/v1/link-decisions | caller: any service (`auth.AnyCaller`) | implemented (flag) | C, L | service token only |
| listOrganisationDirectory | GET /internal/v1/vendor/organisations | vendor admin.org.read, caller: admin | implemented | C | `tenancy_vendor` role; no audit row |
| createOrganisation | POST /internal/v1/vendor/organisations | vendor admin.org.create, caller: admin | implemented | C | one transaction; idempotent on the vendor's key; organisation-less route: audit row goes to `platform_vendor_access_log` (go-auth `PlatformAudit`, `WithPlatformAuditedTx`), also for a refused request |
| getOrganisationDirectoryEntry | GET /internal/v1/vendor/organisations/{organisation_id} | vendor admin.org.read, caller: admin | implemented | C | ordinary role under that organisation's context |
| verifyTaxRegistration | PUT /internal/v1/vendor/organisations/{organisation_id}/tax-registrations/{registration_id}/verification | vendor admin.registration.verify, caller: admin | implemented | C | 409 `gstin_in_use`; allocator floor for a returning GSTIN; a rejected registration can be verified |
| rejectTaxRegistration | PUT /internal/v1/vendor/organisations/{organisation_id}/tax-registrations/{registration_id}/rejection | vendor admin.registration.verify, caller: admin | implemented | C | only while unverified (409 `registration_verified` / `registration_closed`); idempotent; reason kept on the row, not in the body |

| changeOutletTaxRegistration | PUT /v1/outlets/{outlet_id}/tax-registration | tax_registration.manage (step-up), outlet scoped | implemented | C, S | target open and verified or unregistered (409 `registration_not_eligible`); 409 `open_trading_day`; closes the outlet's series under the old registration; publishes `tenancy.outlet.updated` and `tenancy.series.closed` (cause `outlet_moved`); same target again returns the outlet |
| reissueLinkInvitation | POST /v1/links/{link_id}/invitations | link.manage (step-up) | implemented (flag) | C, S | proposer only, while proposed (409 `link_not_proposed`); the old code stops working; the new code is shown once, a replay returns `secret_withheld` |
| reacceptLink | PUT /v1/links/{link_id}/re-acceptance | link.manage (step-up) | implemented (flag) | C, S | acceptor only, while accepted (409 `link_not_accepted`); new snapshot; publishes `tenancy.link.accepted` again |
| listManualBookSeries | GET /v1/manual-book-series | series.read | implemented | C, S | books that serve at least one outlet in the caller's scope |
| registerManualBookSeries | POST /v1/manual-book-series | series.manual_book.register (strong only, step-up) | implemented | C, S | x-provisional (P2B-6); code unique per GSTIN against device series codes (409 `series_code_in_use`); the allocator skips a book's code |
| getMyPlan | GET /v1/subscription | subscription.read | implemented | C, S | own organisation only; no vendor user ids, no notes; overdue is a label |
| listBillingConfigurations | GET /internal/v1/vendor/billing-configurations | vendor admin.subscription.read, caller: admin | implemented | C, S | platform data, directory class (no audit row) |
| createBillingConfiguration | POST /internal/v1/vendor/billing-configurations | vendor admin.plan.manage, caller: admin | implemented | C, S | one version per effective date (409 `effective_from_taken`); platform audit row |
| listPlans, getPlan | GET /internal/v1/vendor/plans, .../plans/{plan_key} | vendor admin.subscription.read, caller: admin | implemented | C, S | directory class |
| createPlan, createPlanVersion | POST /internal/v1/vendor/plans, .../plans/{plan_key}/versions | vendor admin.plan.manage, caller: admin | implemented | C, S | plans and versions are data; a new version is a draft |
| activatePlanVersion, retirePlanVersion | POST .../versions/{version}/activation, .../retirement | vendor admin.plan.manage, caller: admin | implemented | C, S | draft to active (409 `version_not_draft`), active to retired (409 `version_not_active`); an active version is immutable |
| startSubscription | POST /internal/v1/vendor/organisations/{organisation_id}/subscription | vendor admin.subscription.start, caller: admin | implemented | C, S | subscription row, entitlement rows and the event in one transaction; first plan moves trial to active; below list needs `admin.subscription.adjust` and a reason; above list or add-ons need `admin.subscription.manage`; `x-permission-widened-by` is a tenant-realm support rule and has no meaning on a vendor route, so it is not applied |
| changeSubscription | PUT .../subscription | vendor admin.subscription.manage, caller: admin | implemented | C, S | a new row from `effective_from`, no proration |
| cancelSubscription | POST .../subscription/cancellation | vendor admin.subscription.manage, caller: admin | implemented | C, S | `cancel_at_period_end`; reads cancelled from the day after `paid_through`; idempotent |
| getOrganisationSubscription | GET .../subscription | vendor admin.subscription.read, caller: admin | implemented | C, S | current row and history; directory class |
| raiseCharge | POST .../charges | vendor admin.subscription.manage, caller: admin | implemented | S (contract skipped, see below) | GST from the configuration in force for the period, stored on the charge; 409 `duplicate_period`; a pilot has no charges |
| listCharges, getCharge | GET .../charges, .../charges/{charge_id} | vendor admin.subscription.read, caller: admin | implemented | C, S | the list is a directory read, a charge detail read is audited |
| setChargeInvoiceReference | PUT .../charges/{charge_id}/invoice-reference | vendor admin.subscription.manage, caller: admin | implemented | C, S | the number and date from the founder's books, set once (409 `invoice_reference_set`); the panel records the number only (P2A-6) |
| recordPayment, listChargePayments | POST/GET .../charges/{charge_id}/payments | vendor admin.subscription.manage / read, caller: admin | implemented | C, S | by hand; partial payments; 422 `overpayment`; not in the future; append-only |
| reversePayment | POST .../payments/{payment_id}/reversal | vendor admin.subscription.adjust, caller: admin | implemented | C, S | a reversing row, once per payment |
| waiveCharge, cancelCharge | POST .../charges/{charge_id}/waiver, .../cancellation | vendor admin.subscription.adjust, caller: admin | implemented | C, S | status and reason on the charge; cancelling a charge with payments is 409 `charge_has_payments` |
| listLedgerEntries | GET /internal/v1/vendor/ledger-lists/{list_kind} | vendor admin.subscription.read, caller: admin | implemented | C, S | six lists derived on read from the vendor-readable ledger directory; nothing is scheduled or sent |
| exportLedger | GET /internal/v1/vendor/ledger-exports | vendor admin.report.ledger, caller: admin | implemented | C, S | CSV with ids and names only; formula-looking names are defused; platform audit row |
| suspendOrganisation, reinstateOrganisation, closeOrganisation | POST .../suspension, .../reinstatement, .../closure | vendor admin.org.lifecycle.approve, caller: admin, vendor approval grant | implemented | C, S | go-auth `GrantVerifier` (signature, audience, kind, organisation, approver, subject hash recomputed from the path and the target status), the grant id recorded in the action's transaction, 409 `open_trading_day` (suspend and close) from `outlet_trading_state`, tenant audit entry with the show-to-owner marker, `tenancy.organisation.status_changed` |

72 of 72 operations implemented.

## Known limits

- `createOrganisation` writes its audit row to the platform log (no tenant), so the new organisation's own
  `vendor_access_log` starts empty; the platform row names the new organisation as its target.
- The open-trading-day guard (ADR-0014 A1.2) runs at execution. A1.2 also says "at request", but the contract has no
  tenancy operation for a request-time check, so `admin` has to ask through the execution route (a refused execution
  leaves the approval approved, as A1.3 item 3 says).
- `closeDeviceSeries` publishes no event: `tenancy.series.closed` has no cause for it (causes are
  `registration_closed`, `outlet_moved`, `closed_unreconciled`).
- `identity.device.drained` is not consumed: it is the device's statement of its last number, and tenancy fills
  `tail_confirmed_at` and `last_number` only from `billing.series.reconciled`, which billing publishes once its register
  holds every number (ADR-0014 A2.5 item 2).
- Waivers and cancellations are a status and a reason on the charge row (the application role may update `charges`),
  not separate rows; payments, reversals, grants and lifecycle entries are append-only.
- A paper bill book needs an open registration that is not rejected, not a verified one: the contract's own example
  registers a book under the base world's unverified registration.
- A bill book's code makes the series allocator jump 200 codes when it meets it (the existing collision rule).

## Spec problems found

- `raiseCharge` declares world `plan-active` (no subscription) but its example response names subscription 8011; the
  world `subscribed` already holds a charge for the example period, so no declared world makes the example true. The
  operation is skipped in the contract run with that reason and covered by the `Commercial` scenario suite.
- `changeSubscription`'s example asks for an annual cycle on a plan version whose world lists only a monthly price. The
  seeded `table_service` has the annual price too (BR-A1-12), so the example holds.
- The due date of the first charge in the examples is the period start plus the terms (2026-12-08), not the raising date
  plus the terms the spec text says; the service takes the later of the raising date and the period start.
- `startSubscription` carries `x-permission-widened-by: admin.subscription.manage`, a rule go-auth defines for
  tenant-realm support tokens; on a vendor route it is not applied.

## Contract suite

One run of `contracttest` per target (`double/contract_test.go`, `api/contract_integration_test.go`), 72 operations plus
the unknown-route check; one skipped (`raiseCharge`, see Spec problems). Before each operation the harness calls `SeedWorld` with the operation's
`x-example-world`; `double.SeedWorld` / `itest.Stack.SeedWorld` empty the store and load the world through the repository
(`internal/seed/worlds.go`), and the suite authenticates as the world's caller (organisation 4a63 in the worlds where the
caller is the invited party). `ExpectExamples` is not set: response ids and instants minted by the service (grant ids,
activity ids, issue times) are not the spec's literal values; the response schemas are validated instead.
