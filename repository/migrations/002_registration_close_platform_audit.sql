-- Registration rejection and closure (ADR-0003 A1.1 items 4 and 6), and the platform vendor access log
-- for vendor routes that name no organisation (ADR-0013 V5 item 3). Expand only: new nullable columns and
-- a new table; nothing existing changes meaning.

ALTER TABLE tax_registrations
    ADD COLUMN rejected_at     timestamptz,
    ADD COLUMN rejected_by     text,
    ADD COLUMN rejected_reason text,
    ADD COLUMN closed_at       timestamptz,
    ADD COLUMN closed_reason   text,
    ADD CONSTRAINT tax_registrations_rejected_fields CHECK ((verification_state = 'rejected') = (rejected_at IS NOT NULL));

-- Platform table: no tenant_id and no row-level security, because the row concerns no organisation. It is
-- append-only for the application role (insert and select, no update or delete), like vendor_access_log.
-- Ids and fixed-pattern values only: no free text, tokens or phone numbers.
CREATE TABLE platform_vendor_access_log (
    id                 uuid        PRIMARY KEY,
    occurred_at        timestamptz NOT NULL,
    vendor_user        text        NOT NULL,
    token_type         text        NOT NULL,
    support_session_id text,
    action_key         text        NOT NULL,
    target_id          text,
    ticket_reference   text,
    outcome            text        NOT NULL CHECK (outcome IN ('ok', 'refused')),
    result_count       int,
    request_id         text        NOT NULL DEFAULT ''
);
CREATE INDEX platform_vendor_access_log_time ON platform_vendor_access_log (occurred_at);

GRANT SELECT, INSERT ON platform_vendor_access_log TO tenancy_app;
