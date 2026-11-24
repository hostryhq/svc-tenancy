# Operations status

Every operationId of `api-specs/openapi/tenancy/v1/openapi.yaml` (commit recorded in `internal/gen/SPEC_VERSION`).
Status is **implemented** in the service (`api` -> `service` -> `repository/pg`), in the test double (`double/`, same API and
service over `repository/mem`) and covered by the shared contract suite (`contracttest`) or, where the stateless example
suite cannot drive an operation, by a named scenario suite. **Not yet implemented: none.**

Contract column: `C` = example-driven suite (both real service and double); `L` = link lifecycle scenario suite
(`internal/httpsuite/links.go`, every response validated against the OpenAPI document, real service and double).

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
| updateTaxRegistration | PATCH /v1/tax-registrations/{registration_id} | tax_registration.manage (step-up) | implemented | C | 409 `registration_fixed` once verified (a no-op still succeeds) |
| listStates | GET /v1/states | authenticated | implemented | C | static reference table (`internal/states`) |
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
| getLinkDecision | GET /internal/v1/link-decisions | caller: any service | implemented (flag) | L | `x-service-callers: ["*"]` expands to the configured service list |
| listOrganisationDirectory | GET /internal/v1/vendor/organisations | vendor admin.org.read, caller: admin | implemented | C | `tenancy_vendor` role; no audit row |
| createOrganisation | POST /internal/v1/vendor/organisations | vendor admin.org.create, caller: admin | implemented | C (own pass) | one transaction; idempotent on the vendor's key |
| getOrganisationDirectoryEntry | GET /internal/v1/vendor/organisations/{organisation_id} | vendor admin.org.read, caller: admin | implemented | C | ordinary role under that organisation's context |
| verifyTaxRegistration | PUT /internal/v1/vendor/organisations/{organisation_id}/tax-registrations/{registration_id}/verification | vendor admin.registration.verify, caller: admin | implemented | C | 409 `gstin_in_use`; allocator floor for a returning GSTIN |

38 of 38 operations implemented.

## Not in the contract yet (reported to api-specs)

- A way to **reject** a registration (ADR-0003 A1.1 item 4, AC-TEN-20): the schema allows `rejected` and the database holds it,
  but no operation sets it.
- A way to **close** a registration and its series when a verified GSTIN has to change (A1.1 item 6).
- `x-service-callers: ["*"]`, `x-permission: authenticated` and ULID ids are not representable in go-auth / the row-level
  policy as written; see the report of this work package.

## Contract suite skips

The example-driven suite is stateless and sends each operation once from the spec examples. It runs in two passes (the example
world, and an empty platform for `createOrganisation`, whose example GSTIN is verified in the example world). The 13 link
lifecycle operations other than `listLinks` are skipped there with a reason: their examples show one link id in six states and
one invitation code, so no single seeded state satisfies them. `internal/httpsuite/links.go` covers each of them over HTTP
against both the real service and the double, validating every response against the OpenAPI document.
