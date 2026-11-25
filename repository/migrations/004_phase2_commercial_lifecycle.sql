-- Phase 2: the commercial module (ADR-0008 S2), organisation lifecycle (ADR-0003 section 8, ADR-0014 A1.2,
-- A1.3, A2.6), series tail fields and manual bill books (ADR-0014 A2.5, A1.6). Forward-only, expand only:
-- new columns are nullable and new tables are new.
--
-- Table classes: platform reference data (plans, plan_versions, billing_configurations: no tenant, no row-level
-- security, readable by the vendor role, written only through vendor routes that name no organisation),
-- tenant-owned (the standard policy), and vendor-readable directory tables kept in the writer's transaction.

-- ---------------------------------------------------------------- series tail (A2.5 item 2)
ALTER TABLE document_series
    ADD COLUMN tail_confirmed_at timestamptz,
    ADD COLUMN last_number       bigint CHECK (last_number >= 0),
    ADD CONSTRAINT document_series_tail CHECK ((tail_confirmed_at IS NULL) = (last_number IS NULL));

-- ---------------------------------------------------------------- manual bill books (A1.6 item 4)
CREATE TABLE manual_book_series (
    tenant_id           uuid        NOT NULL REFERENCES organisations (tenant_id),
    id                  uuid        NOT NULL,
    tax_registration_id uuid        NOT NULL,
    gstin_normalised    text,
    code                text        NOT NULL CHECK (code ~ '^[A-Z0-9]{1,6}$'),
    outlet_ids          jsonb       NOT NULL DEFAULT '[]',
    status              text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'closed')),
    created_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, tax_registration_id) REFERENCES tax_registrations (tenant_id, id)
);
CREATE UNIQUE INDEX manual_book_series_code ON manual_book_series (tenant_id, tax_registration_id, code);

-- ---------------------------------------------------------------- trading state (A1.2 item 2)
-- One row per outlet and source: the latest business day billing (or a later product) reported. Never
-- shown outside the lifecycle and outlet-move guards.
CREATE TABLE outlet_trading_state (
    tenant_id     uuid        NOT NULL REFERENCES organisations (tenant_id),
    outlet_id     uuid        NOT NULL,
    source        text        NOT NULL,
    business_date date        NOT NULL,
    state         text        NOT NULL CHECK (state IN ('open', 'closed')),
    changed_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, outlet_id, source),
    FOREIGN KEY (tenant_id, outlet_id) REFERENCES outlets (tenant_id, outlet_id)
);

-- ---------------------------------------------------------------- vendor approval grants (A2.6 item 4)
CREATE TABLE approval_grant_used (
    tenant_id   uuid        NOT NULL REFERENCES organisations (tenant_id),
    jti         text        NOT NULL,
    approval_id text        NOT NULL,
    kind        text        NOT NULL,
    used_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, jti)
);

-- Tenant audit entry of a suspend, reinstate or close, with the "show to owner" marker (ADR-0013 V7).
CREATE TABLE organisation_lifecycle_log (
    tenant_id             uuid        NOT NULL REFERENCES organisations (tenant_id),
    id                    uuid        NOT NULL,
    kind                  text        NOT NULL CHECK (kind IN ('org_suspend', 'org_reinstate', 'org_close')),
    approval_id           text        NOT NULL,
    requester             text        NOT NULL,
    approver              text        NOT NULL,
    from_status           text        NOT NULL,
    to_status             text        NOT NULL,
    ticket_reference      text        NOT NULL,
    reason                text        NOT NULL,
    owner_export_offered  boolean     NOT NULL DEFAULT false,
    occurred_at           timestamptz NOT NULL,
    show_to_owner         boolean     NOT NULL DEFAULT true,
    PRIMARY KEY (tenant_id, id)
);

-- ---------------------------------------------------------------- platform reference data
CREATE TABLE plans (
    key        text        PRIMARY KEY CHECK (key ~ '^[a-z][a-z0-9_]{1,31}$'),
    name_key   text        NOT NULL CHECK (char_length(name_key) BETWEEN 1 AND 100),
    created_at timestamptz NOT NULL
);

CREATE TABLE plan_versions (
    plan_key         text        NOT NULL REFERENCES plans (key),
    version          int         NOT NULL CHECK (version >= 1),
    state            text        NOT NULL CHECK (state IN ('draft', 'active', 'retired')),
    feature_keys     jsonb       NOT NULL DEFAULT '[]',
    included_outlets int         NOT NULL CHECK (included_outlets >= 0),
    prices           jsonb       NOT NULL DEFAULT '[]',
    created_at       timestamptz NOT NULL,
    activated_at     timestamptz,
    retired_at       timestamptz,
    PRIMARY KEY (plan_key, version)
);

CREATE TABLE billing_configurations (
    id                      uuid        PRIMARY KEY,
    effective_from          date        NOT NULL UNIQUE,
    hostry_state_code       text        NOT NULL CHECK (hostry_state_code ~ '^[0-9]{2}$'),
    sac                     text        NOT NULL CHECK (sac ~ '^[0-9]{4,8}$'),
    gst_rate_bp             int         NOT NULL CHECK (gst_rate_bp BETWEEN 0 AND 2800),
    gst_registered          boolean     NOT NULL,
    payment_tolerance_paise bigint      NOT NULL CHECK (payment_tolerance_paise >= 0),
    payment_terms_days      int         NOT NULL CHECK (payment_terms_days >= 0),
    created_at              timestamptz NOT NULL
);

-- Seed catalogue (Admin A1 BR-A1-12; provisional until the founder answers A1-Q1 and A1-Q2): every plan is
-- a draft, so nothing is sellable until the founder activates a version. Prices are paise excluding GST.
INSERT INTO plans (key, name_key, created_at) VALUES
    ('pilot', 'plan.pilot', now()),
    ('table_service', 'plan.table_service', now()),
    ('extra_outlet', 'plan.extra_outlet', now()),
    ('counter', 'plan.counter', now()),
    ('pro', 'plan.pro', now()),
    ('whatsapp_addon', 'plan.whatsapp_addon', now());
INSERT INTO plan_versions (plan_key, version, state, feature_keys, included_outlets, prices, created_at) VALUES
    ('pilot', 1, 'draft', '["restaurant.core","restaurant.kds","restaurant.captain","restaurant.reports.core"]', 1,
        '[{"cycle":"none","list_paise":0}]', now()),
    ('table_service', 1, 'draft', '["restaurant.core","restaurant.kds","restaurant.captain","restaurant.reports.core"]', 1,
        '[{"cycle":"monthly","list_paise":129900},{"cycle":"annual","list_paise":1299000}]', now()),
    ('extra_outlet', 1, 'draft', '[]', 0,
        '[{"cycle":"monthly","list_paise":79900},{"cycle":"annual","list_paise":799000}]', now()),
    ('counter', 1, 'draft', '["restaurant.core","restaurant.reports.core"]', 1,
        '[{"cycle":"monthly","list_paise":49900}]', now()),
    ('pro', 1, 'draft', '["restaurant.core","restaurant.kds","restaurant.captain","restaurant.reports.core"]', 1,
        '[{"cycle":"monthly","list_paise":249900}]', now()),
    ('whatsapp_addon', 1, 'draft', '["whatsapp.ordering"]', 0,
        '[{"cycle":"monthly","list_paise":69900}]', now());

-- ---------------------------------------------------------------- tenant-owned ledger
CREATE TABLE subscriptions (
    tenant_id            uuid        NOT NULL REFERENCES organisations (tenant_id),
    id                   uuid        NOT NULL,
    plan_key             text        NOT NULL,
    plan_version         int         NOT NULL,
    cycle                text        NOT NULL CHECK (cycle IN ('none', 'monthly', 'annual')),
    agreed_paise         bigint      NOT NULL CHECK (agreed_paise >= 0),
    addons               jsonb       NOT NULL DEFAULT '[]',
    start_date           date        NOT NULL,
    paid_through         date,
    state                text        NOT NULL CHECK (state IN ('pilot', 'active', 'cancelled')),
    cancel_at_period_end boolean     NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL,
    superseded_at        timestamptz,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (plan_key, plan_version) REFERENCES plan_versions (plan_key, version)
);
-- One current subscription per organisation (BR-A1-20); earlier rows are history.
CREATE UNIQUE INDEX subscriptions_current ON subscriptions (tenant_id) WHERE superseded_at IS NULL;

-- The organisation's entitlement rows, changed in the same transaction as the subscription (BR-A1-14).
-- No expiry column: a late payment can never withdraw one (BR-A1-60). A key a new plan version no longer
-- carries is marked withdrawn_at by the plan change; rows are never deleted.
CREATE TABLE entitlements (
    tenant_id       uuid        NOT NULL REFERENCES organisations (tenant_id),
    feature_key     text        NOT NULL,
    subscription_id uuid        NOT NULL,
    granted_at      timestamptz NOT NULL,
    withdrawn_at    timestamptz,
    PRIMARY KEY (tenant_id, feature_key),
    FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id)
);

CREATE TABLE charges (
    tenant_id       uuid        NOT NULL REFERENCES organisations (tenant_id),
    id              uuid        NOT NULL,
    subscription_id uuid        NOT NULL,
    kind            text        NOT NULL CHECK (kind IN ('period', 'extra')),
    period_start    date        NOT NULL,
    period_end      date        NOT NULL CHECK (period_end >= period_start),
    taxable_paise   bigint      NOT NULL CHECK (taxable_paise >= 0),
    cgst_paise      bigint      NOT NULL CHECK (cgst_paise >= 0),
    sgst_paise      bigint      NOT NULL CHECK (sgst_paise >= 0),
    igst_paise      bigint      NOT NULL CHECK (igst_paise >= 0),
    total_paise     bigint      NOT NULL CHECK (total_paise = taxable_paise + cgst_paise + sgst_paise + igst_paise),
    tolerance_paise bigint      NOT NULL DEFAULT 0 CHECK (tolerance_paise >= 0),
    due_date        date        NOT NULL,
    invoice_number  text,
    invoice_date    date,
    status          text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'waived', 'cancelled')),
    note            text,
    reason          text,
    created_by      text        NOT NULL,
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, subscription_id) REFERENCES subscriptions (tenant_id, id),
    CONSTRAINT charges_invoice_pair CHECK ((invoice_number IS NULL) = (invoice_date IS NULL))
);
CREATE INDEX charges_period ON charges (tenant_id, period_start DESC, id DESC);

-- Append-only: payments and their reversing rows are inserted, never updated or deleted (BR-A1-43, BR-A1-44).
CREATE TABLE charge_payments (
    tenant_id    uuid        NOT NULL REFERENCES organisations (tenant_id),
    id           uuid        NOT NULL,
    charge_id    uuid        NOT NULL,
    amount_paise bigint      NOT NULL CHECK (amount_paise > 0),
    method       text        NOT NULL CHECK (method IN ('upi', 'bank_transfer', 'cash', 'cheque')),
    reference    text        CHECK (char_length(reference) <= 40),
    received_on  date        NOT NULL,
    recorded_by  text        NOT NULL,
    reversal_of  uuid,
    reason       text,
    created_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, charge_id) REFERENCES charges (tenant_id, id),
    FOREIGN KEY (tenant_id, reversal_of) REFERENCES charge_payments (tenant_id, id)
);
-- A payment is reversed at most once.
CREATE UNIQUE INDEX charge_payments_one_reversal ON charge_payments (tenant_id, reversal_of) WHERE reversal_of IS NOT NULL;
CREATE INDEX charge_payments_charge ON charge_payments (tenant_id, charge_id, id);

-- ---------------------------------------------------------------- vendor-readable ledger directory (ADR-0008 S2)
-- Cross-organisation lists read these, never the ledger tables. They are rewritten in the transaction of every
-- ledger change, hold no personal data (BR-A1-27, BR-A1-38), and are the only ledger tables the vendor role reads.
CREATE TABLE directory_subscriptions (
    subject_tenant_id    uuid        PRIMARY KEY,
    subscription_id      uuid        NOT NULL,
    plan_key             text        NOT NULL,
    plan_version         int         NOT NULL,
    cycle                text        NOT NULL,
    agreed_paise         bigint      NOT NULL,
    addons_paise         bigint      NOT NULL DEFAULT 0,
    extra_outlets        int         NOT NULL DEFAULT 0,
    state                text        NOT NULL,
    cancel_at_period_end boolean     NOT NULL,
    start_date           date        NOT NULL,
    paid_through         date,
    next_period_start    date,
    updated_at           timestamptz NOT NULL
);

CREATE TABLE directory_charges (
    subject_tenant_id uuid   NOT NULL,
    charge_id         uuid   NOT NULL,
    kind              text   NOT NULL,
    status            text   NOT NULL,
    taxable_paise     bigint NOT NULL,
    total_paise       bigint NOT NULL,
    outstanding_paise bigint NOT NULL,
    tolerance_paise   bigint NOT NULL,
    due_date          date   NOT NULL,
    period_start      date   NOT NULL,
    period_end        date   NOT NULL,
    invoice_number    text,
    PRIMARY KEY (subject_tenant_id, charge_id)
);

-- ---------------------------------------------------------------- row-level security
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['manual_book_series', 'outlet_trading_state', 'approval_grant_used', 'organisation_lifecycle_log',
                             'subscriptions', 'entitlements', 'charges', 'charge_payments']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
    FOREACH t IN ARRAY ARRAY['directory_subscriptions', 'directory_charges']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY app_own ON %I TO tenancy_app
            USING      (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
        EXECUTE format('CREATE POLICY vendor_read ON %I FOR SELECT TO tenancy_vendor USING (true)', t);
    END LOOP;
END $$;

-- ---------------------------------------------------------------- privileges
GRANT SELECT, INSERT, UPDATE ON manual_book_series, outlet_trading_state, subscriptions, entitlements, charges,
      directory_subscriptions, directory_charges TO tenancy_app;
-- Append-only for the application role: no update or delete.
GRANT SELECT, INSERT ON approval_grant_used, organisation_lifecycle_log, charge_payments TO tenancy_app;
-- Platform reference data: the application role writes it through vendor routes that name no organisation.
GRANT SELECT, INSERT, UPDATE ON plans, plan_versions TO tenancy_app;
GRANT SELECT, INSERT ON billing_configurations TO tenancy_app;
-- The vendor role reads the directory ledger and the plan data, and nothing tenant-owned.
GRANT SELECT ON directory_subscriptions, directory_charges, plans, plan_versions, billing_configurations TO tenancy_vendor;
