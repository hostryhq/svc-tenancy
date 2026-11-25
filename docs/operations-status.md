# Operations status

Every operationId of `api-specs/openapi/tenancy/v1/openapi.yaml` (commit recorded in `internal/gen/SPEC_VERSION`).
Status is **implemented** in the service (`api` -> `service` -> `repository/pg`), in the test double (`double/`, same API and
service over `repository/mem`) and covered by the shared contract suite (`contracttest`) in the example world the spec declares for it
(`SeedWorld`, `internal/seed/worlds.go`), on the real service and the double, with no operation skipped.
**Not yet implemented: none.**

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

40 of 40 operations implemented.

## Known limits

- `closeTaxRegistration` closes every dormant or active series as plain `closed`. The spec asks for the unreconciled
  marker on a series with an unreconciled tail, but tenancy keeps no record of a device's tail (sync does); the tenant
  marks such series with `closeDocumentSeriesUnreconciled`. Needs a rule from the architect if tenancy must decide.
- `createOrganisation` writes its audit row to the platform log (no tenant), so the new organisation's own
  `vendor_access_log` starts empty; the platform row names the new organisation as its target.

## Contract suite

One run of `contracttest` per target (`double/contract_test.go`, `api/contract_integration_test.go`), 40 operations plus
the unknown-route check, nothing skipped. Before each operation the harness calls `SeedWorld` with the operation's
`x-example-world`; `double.SeedWorld` / `itest.Stack.SeedWorld` empty the store and load the world through the repository
(`internal/seed/worlds.go`), and the suite authenticates as the world's caller (organisation 4a63 in the worlds where the
caller is the invited party). `ExpectExamples` is not set: response ids and instants minted by the service (grant ids,
activity ids, issue times) are not the spec's literal values; the response schemas are validated instead.
