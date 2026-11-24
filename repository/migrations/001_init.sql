-- Tenancy service schema, first migration (forward-only). Runs as the migrator role, which owns every
-- table; the runtime roles below must already exist (cluster provisioning, see scripts/provision-roles.sql):
--   tenancy_app     DML only, NOSUPERUSER NOBYPASSRLS, not an owner (the service's runtime role)
--   tenancy_vendor  SELECT on the directory tables and nothing else (ADR-0013 V3)
--   tenancy_relay   outbox relay: the outbox table only (ADR-0003 section 6)
--
-- Table classes (ADR-0003 section 2): tenant-owned (standard policy), two-party (links, link_*),
-- service-internal (outbox, idempotency_keys, event_keys), vendor realm (directory_*). The state
-- reference table is code (internal/states).
--
-- ADR-0003 A1.1: the three GSTIN constraints are in THIS migration, before any registration can exist.

-- ---------------------------------------------------------------- organisation and structure
CREATE TABLE organisations (
    tenant_id      uuid PRIMARY KEY,
    name           text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    status         text        NOT NULL CHECK (status IN ('trial', 'active', 'suspended', 'closed')),
    data_placement text        NOT NULL DEFAULT 'primary',
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL
);

CREATE TABLE organisation_settings (
    tenant_id           uuid PRIMARY KEY REFERENCES organisations (tenant_id),
    offline_window_days int         NOT NULL CHECK (offline_window_days BETWEEN 1 AND 30),
    idle_lock_minutes   int         NOT NULL CHECK (idle_lock_minutes BETWEEN 1 AND 120),
    updated_at          timestamptz NOT NULL
);

CREATE TABLE brands (
    tenant_id  uuid        NOT NULL REFERENCES organisations (tenant_id),
    id         uuid        NOT NULL,
    name       text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    is_default boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
CREATE UNIQUE INDEX brands_one_default ON brands (tenant_id) WHERE is_default;

CREATE TABLE tax_registrations (
    tenant_id           uuid        NOT NULL REFERENCES organisations (tenant_id),
    id                  uuid        NOT NULL,
    legal_name          text        NOT NULL CHECK (char_length(legal_name) BETWEEN 1 AND 200),
    regime              text        NOT NULL CHECK (regime IN ('regular', 'composition', 'unregistered')),
    gstin_normalised    text        CHECK (gstin_normalised ~ '^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$'),
    state_code          text        NOT NULL CHECK (state_code ~ '^[0-9]{2}$'),
    verification_state  text        NOT NULL DEFAULT 'unverified' CHECK (verification_state IN ('unverified', 'verified', 'rejected')),
    verification_method text        CHECK (verification_method IN ('certificate_seen', 'gst_portal_lookup', 'other')),
    verified_at         timestamptz,
    verified_by         text,
    status              text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    -- unregistered has no GSTIN; every other regime has one
    CONSTRAINT tax_registrations_gstin_kind CHECK ((regime = 'unregistered') = (gstin_normalised IS NULL)),
    CONSTRAINT tax_registrations_verified_fields CHECK ((verification_state = 'verified') = (verified_at IS NOT NULL))
);
-- A1.1 item 2, constraint 1: an organisation cannot hold a GSTIN twice (among records that are not closed).
CREATE UNIQUE INDEX tax_registrations_tenant_gstin ON tax_registrations (tenant_id, gstin_normalised)
    WHERE gstin_normalised IS NOT NULL AND status <> 'closed';
-- A1.1 item 2, constraint 2: a GSTIN has at most one verified holder, platform-wide. A unique index holds
-- across every organisation whatever the row-level policy, so no lookup table or bypass is needed.
CREATE UNIQUE INDEX tax_registrations_verified_gstin ON tax_registrations (gstin_normalised)
    WHERE gstin_normalised IS NOT NULL AND verification_state = 'verified' AND status <> 'closed';

-- Outlet-scoped (ADR-0003 A1.4): the outlet's own id column is named outlet_id.
CREATE TABLE outlets (
    tenant_id            uuid        NOT NULL REFERENCES organisations (tenant_id),
    outlet_id            uuid        NOT NULL,
    brand_id             uuid        NOT NULL,
    name                 text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    kind                 text        NOT NULL CHECK (kind IN ('restaurant', 'bakery_outlet', 'production_unit', 'stall', 'property', 'service_point')),
    status               text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    state_code           text        NOT NULL CHECK (state_code ~ '^[0-9]{2}$'),
    tax_registration_id  uuid        NOT NULL,
    located_in_outlet_id uuid,
    created_at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, outlet_id),
    FOREIGN KEY (tenant_id, brand_id) REFERENCES brands (tenant_id, id),
    FOREIGN KEY (tenant_id, tax_registration_id) REFERENCES tax_registrations (tenant_id, id),
    FOREIGN KEY (tenant_id, located_in_outlet_id) REFERENCES outlets (tenant_id, outlet_id)
);
CREATE INDEX outlets_scope ON outlets (tenant_id, outlet_id);

-- ---------------------------------------------------------------- document-series registry (ADR-0003 section 1a)
CREATE TABLE document_series (
    tenant_id           uuid        NOT NULL REFERENCES organisations (tenant_id),
    id                  uuid        NOT NULL,
    tax_registration_id uuid        NOT NULL,
    gstin_normalised    text,       -- copied from the registration when the code is issued
    code                text        NOT NULL CHECK (char_length(code) = 3),
    code_index          int         NOT NULL CHECK (code_index BETWEEN 0 AND 32767),
    doc_type            text        NOT NULL CHECK (doc_type IN ('tax_invoice', 'bill_of_supply', 'plain_bill', 'credit_note')),
    outlet_id           uuid        NOT NULL,
    device_id           uuid        NOT NULL,
    status              text        NOT NULL CHECK (status IN ('dormant', 'active', 'closed', 'closed_unreconciled')),
    issued_at           timestamptz NOT NULL,
    closed_at           timestamptz,
    closed_reason       text,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlets (tenant_id, outlet_id),
    FOREIGN KEY (tenant_id, tax_registration_id) REFERENCES tax_registrations (tenant_id, id)
);
-- A code is never issued twice under a registration, across outlets, document types and years.
CREATE UNIQUE INDEX document_series_registration_code ON document_series (tenant_id, tax_registration_id, code);
-- A1.1 item 2, constraint 3: the legal rule itself, per GSTIN across records and organisations.
CREATE UNIQUE INDEX document_series_gstin_code ON document_series (gstin_normalised, code) WHERE gstin_normalised IS NOT NULL;
CREATE INDEX document_series_device ON document_series (tenant_id, outlet_id, device_id);
CREATE INDEX document_series_issued ON document_series (tenant_id, tax_registration_id, issued_at);

CREATE TABLE series_allocators (
    tenant_id           uuid        NOT NULL REFERENCES organisations (tenant_id),
    tax_registration_id uuid        NOT NULL,
    next_index          int         NOT NULL CHECK (next_index >= 0),
    updated_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, tax_registration_id),
    FOREIGN KEY (tenant_id, tax_registration_id) REFERENCES tax_registrations (tenant_id, id)
);

-- ---------------------------------------------------------------- service-internal
CREATE TABLE idempotency_keys (
    tenant_id   uuid        NOT NULL REFERENCES organisations (tenant_id),
    op          text        NOT NULL,
    key         text        NOT NULL,
    fingerprint text        NOT NULL,
    response    bytea,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, op, key)
);

CREATE TABLE event_keys (
    tenant_id uuid   NOT NULL REFERENCES organisations (tenant_id),
    key       text   NOT NULL,
    last_seq  bigint NOT NULL,
    PRIMARY KEY (tenant_id, key)
);

-- go-common outbox schema (ADR-0011 B4); tenant_id is text there.
CREATE TABLE outbox (
    id           uuid        PRIMARY KEY,
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    type         text        NOT NULL,
    subject      text        NOT NULL,
    tenant_id    text        NOT NULL,
    key          text        NOT NULL DEFAULT '',
    key_seq      bigint      NOT NULL DEFAULT 0,
    payload      bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished ON outbox (seq) WHERE published_at IS NULL;
CREATE INDEX outbox_published_at ON outbox (published_at) WHERE published_at IS NOT NULL;

-- ---------------------------------------------------------------- vendor realm
-- ADR-0013 V5: written in the transaction of the action, for every request made on a vendor user's authority.
CREATE TABLE vendor_access_log (
    tenant_id          uuid        NOT NULL REFERENCES organisations (tenant_id),
    id                 uuid        NOT NULL,
    occurred_at        timestamptz NOT NULL,
    vendor_user        text        NOT NULL,
    token_type         text        NOT NULL,
    support_session_id text,
    action_key         text        NOT NULL,
    target_id          text,
    ticket_reference   text,
    outcome            text        NOT NULL CHECK (outcome IN ('ok', 'refused')),
    result_count       int,
    request_id         text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

-- ADR-0013 V3: directory tables, one row per organisation and per registration, written in the same
-- transaction as the organisation, outlet and registration they describe. No phone, email, member or
-- device data. create_key_hash and create_fingerprint (digests) make vendor creation idempotent.
CREATE TABLE directory_organisations (
    subject_tenant_id  uuid        PRIMARY KEY,
    name               text        NOT NULL,
    status             text        NOT NULL,
    outlet_count       int         NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL,
    create_key_hash    text,
    create_fingerprint text
);
CREATE UNIQUE INDEX directory_organisations_create_key ON directory_organisations (create_key_hash) WHERE create_key_hash IS NOT NULL;

CREATE TABLE directory_registrations (
    subject_tenant_id  uuid NOT NULL,
    registration_id    uuid NOT NULL,
    gstin_normalised   text,
    legal_name         text NOT NULL,
    regime             text NOT NULL,
    state_code         text NOT NULL,
    verification_state text NOT NULL,
    status             text NOT NULL,
    PRIMARY KEY (subject_tenant_id, registration_id)
);
CREATE INDEX directory_registrations_gstin ON directory_registrations (gstin_normalised) WHERE gstin_normalised IS NOT NULL;

-- ---------------------------------------------------------------- two-party class: links (ADR-0003 section 3, A1.3, A1.7)
CREATE TABLE links (
    id                uuid        PRIMARY KEY,
    party_a_tenant_id uuid        NOT NULL REFERENCES organisations (tenant_id), -- the proposer
    party_b_tenant_id uuid        REFERENCES organisations (tenant_id),          -- the acceptor, once known
    template          text        NOT NULL CHECK (template IN ('on_premises_outlet', 'service_provider', 'supplier_buyer')),
    status            text        NOT NULL CHECK (status IN ('proposed', 'accepted', 'active', 'suspended', 'declined', 'expired', 'revoked')),
    proposer_outlets  jsonb       NOT NULL DEFAULT '[]',
    acceptor_outlets  jsonb       NOT NULL DEFAULT '[]',
    proposer_party    jsonb       NOT NULL,  -- display name and registrations the proposer shows the acceptor
    acceptor_party    jsonb,                 -- and the acceptor shows the proposer, taken at acceptance
    suspended_by      uuid,
    proposed_at       timestamptz NOT NULL,
    expires_at        timestamptz NOT NULL,
    accepted_at       timestamptz,
    confirmed_at      timestamptz,
    CHECK (party_b_tenant_id IS NULL OR party_b_tenant_id <> party_a_tenant_id)
);
CREATE INDEX links_party_a ON links (party_a_tenant_id, id);
CREATE INDEX links_party_b ON links (party_b_tenant_id, id) WHERE party_b_tenant_id IS NOT NULL;

-- The invitation code is stored only as a SHA-256 digest. It is visible to its proposer, or to a caller that
-- presents the code (the session setting app.invite_hash is the hex digest): a capability, so an acceptor
-- who is not yet a party can redeem it and nobody can list invitations.
CREATE TABLE link_invitations (
    code_hash          bytea       PRIMARY KEY,
    link_id            uuid        NOT NULL REFERENCES links (id),
    proposer_tenant_id uuid        NOT NULL REFERENCES organisations (tenant_id),
    expires_at         timestamptz NOT NULL,
    used_at            timestamptz
);

CREATE TABLE link_grants (
    id           uuid  PRIMARY KEY,
    link_id      uuid  NOT NULL REFERENCES links (id),
    scope        text  NOT NULL,
    grantor_side text  NOT NULL CHECK (grantor_side IN ('proposer', 'acceptor')),
    outlet_ids   jsonb NOT NULL DEFAULT '[]',
    status       text  NOT NULL CHECK (status IN ('pending', 'active', 'removed'))
);
CREATE INDEX link_grants_link ON link_grants (link_id);

CREATE TABLE link_amendments (
    id               uuid        NOT NULL,
    link_id          uuid        NOT NULL REFERENCES links (id),
    proposed_by_side text        NOT NULL CHECK (proposed_by_side IN ('proposer', 'acceptor')),
    grants           jsonb       NOT NULL,
    status           text        NOT NULL CHECK (status IN ('pending', 'accepted', 'declined')),
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (link_id, id)
);

-- Append-only for the application role (INSERT and SELECT only below); no payload, ever.
CREATE TABLE link_activity (
    id          uuid        PRIMARY KEY,
    link_id     uuid        NOT NULL REFERENCES links (id),
    occurred_at timestamptz NOT NULL,
    side        text        NOT NULL CHECK (side IN ('proposer', 'acceptor', 'system')),
    actor_id    text        NOT NULL DEFAULT '',
    action      text        NOT NULL,
    scope       text,
    object_ref  text,
    outcome     text        NOT NULL CHECK (outcome IN ('succeeded', 'refused'))
);
CREATE INDEX link_activity_link ON link_activity (link_id, id);

-- ---------------------------------------------------------------- row-level security (ADR-0003 section 6)
-- Standard policy on every tenant-owned and service-internal table: enabled, forced, fail closed.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['organisations', 'organisation_settings', 'brands', 'tax_registrations', 'outlets',
                             'document_series', 'series_allocators', 'idempotency_keys', 'event_keys', 'vendor_access_log']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- The outbox carries tenant_id as text: the same policy for the application role, plus the relay role.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox TO tenancy_app
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY relay_all ON outbox TO tenancy_relay USING (true) WITH CHECK (true);

-- Vendor-realm directory: two policies (ADR-0013 V3). The application role touches only its own
-- organisation's rows; tenancy_vendor reads all rows and can write none.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['directory_organisations', 'directory_registrations']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY app_own ON %I TO tenancy_app
            USING      (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
        EXECUTE format('CREATE POLICY vendor_read ON %I FOR SELECT TO tenancy_vendor USING (true)', t);
    END LOOP;
END $$;

-- Two-party class: visible to either party, written only by this service (the application role, in the
-- context of the acting party). An invitation presented by its holder makes its one link visible.
ALTER TABLE link_invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE link_invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY invitation_access ON link_invitations
    USING      (proposer_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR code_hash = decode(NULLIF(current_setting('app.invite_hash', true), ''), 'hex'))
    WITH CHECK (proposer_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR code_hash = decode(NULLIF(current_setting('app.invite_hash', true), ''), 'hex'));

ALTER TABLE links ENABLE ROW LEVEL SECURITY;
ALTER TABLE links FORCE ROW LEVEL SECURITY;
CREATE POLICY party_access ON links
    USING      (party_a_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR party_b_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR EXISTS (SELECT 1 FROM link_invitations i WHERE i.link_id = links.id))
    WITH CHECK (party_a_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR party_b_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['link_grants', 'link_amendments', 'link_activity']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY party_access ON %I
            USING      (EXISTS (SELECT 1 FROM links l WHERE l.id = %I.link_id))
            WITH CHECK (EXISTS (SELECT 1 FROM links l WHERE l.id = %I.link_id))$p$, t, t, t);
    END LOOP;
END $$;

-- ---------------------------------------------------------------- privileges
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON organisations, organisation_settings, brands, tax_registrations, outlets,
      document_series, series_allocators, idempotency_keys, event_keys, links, link_grants, link_amendments,
      link_invitations, directory_organisations, directory_registrations TO tenancy_app;
GRANT SELECT, INSERT ON vendor_access_log, link_activity, outbox TO tenancy_app;
GRANT SELECT ON directory_organisations, directory_registrations TO tenancy_vendor;
GRANT SELECT, UPDATE, DELETE ON outbox TO tenancy_relay;
