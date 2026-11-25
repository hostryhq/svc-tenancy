-- ADR-0003 Note N1 R2: narrow the app.invite_hash capability. Forward-only; links are flagged off, so
-- no customer data depends on the old policies.
--  1. The capability matches only an unused, unexpired invitation.
--  2. Policies are split by command: INSERT on link_invitations is proposer-only; the capability allows
--     SELECT and the single update that marks the invitation used.
--  3. The capability shows links and link_grants; link_activity and link_amendments are for parties only.

-- Postgres also checks the new row of an UPDATE against the SELECT policy, so the used row must stay
-- visible to the organisation that used it: used_by records it (no one else sees a used invitation).
ALTER TABLE link_invitations ADD COLUMN used_by uuid;

DROP POLICY invitation_access ON link_invitations;

CREATE POLICY invitation_select ON link_invitations FOR SELECT
    USING (proposer_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR used_by = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR (code_hash = decode(NULLIF(current_setting('app.invite_hash', true), ''), 'hex')
               AND used_at IS NULL AND expires_at > now()));
-- Only the proposer of that very link may add an invitation to it (a foreign key alone would let any
-- organisation attach an invitation, whose code it knows, to someone else's link).
CREATE POLICY invitation_insert ON link_invitations FOR INSERT
    WITH CHECK (proposer_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                AND EXISTS (SELECT 1 FROM links l WHERE l.id = link_invitations.link_id
                            AND l.party_a_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid));
CREATE POLICY invitation_use ON link_invitations FOR UPDATE
    USING (proposer_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR (code_hash = decode(NULLIF(current_setting('app.invite_hash', true), ''), 'hex')
               AND used_at IS NULL AND expires_at > now()))
    WITH CHECK (proposer_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR (used_by = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                    AND code_hash = decode(NULLIF(current_setting('app.invite_hash', true), ''), 'hex')));

DROP POLICY party_access ON link_activity;
DROP POLICY party_access ON link_amendments;
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['link_amendments', 'link_activity']
    LOOP
        EXECUTE format($p$CREATE POLICY party_access ON %I
            USING      (EXISTS (SELECT 1 FROM links l WHERE l.id = %I.link_id
                                AND (l.party_a_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                                  OR l.party_b_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)))
            WITH CHECK (EXISTS (SELECT 1 FROM links l WHERE l.id = %I.link_id
                                AND (l.party_a_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                                  OR l.party_b_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)))$p$, t, t, t);
    END LOOP;
END $$;

-- Idempotency records move to go-common's table (idempotency.Schema, copied verbatim): 24 h expiry, a
-- done flag, and no one-time secret in the stored body (ADR-0003 N1 R1). The claim, the effect and the
-- result commit in one transaction. tenancy's own idempotency_keys table is retired: nothing is deployed
-- that depends on it, so it is dropped here rather than left unused.
DROP TABLE idempotency_keys;

CREATE TABLE IF NOT EXISTS idempotency_records (
  tenant_id    text        NOT NULL,
  key          text        NOT NULL,
  fingerprint  text        NOT NULL,
  done         boolean     NOT NULL DEFAULT false,
  status       integer     NOT NULL DEFAULT 0,
  content_type text        NOT NULL DEFAULT '',
  body         bytea,
  body_sha256  text        NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL,
  expires_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);
CREATE INDEX IF NOT EXISTS idempotency_records_expires ON idempotency_records (expires_at);

ALTER TABLE idempotency_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_records FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON idempotency_records TO tenancy_app
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
-- The application role never deletes (an expired record is reclaimed by update). The periodic sweep
-- (idempotency.Sweep) runs as the relay role, which may delete only expired rows.
CREATE POLICY sweep_expired ON idempotency_records FOR DELETE TO tenancy_relay USING (expires_at <= now());
GRANT SELECT, INSERT, UPDATE ON idempotency_records TO tenancy_app;
GRANT SELECT, DELETE ON idempotency_records TO tenancy_relay;
