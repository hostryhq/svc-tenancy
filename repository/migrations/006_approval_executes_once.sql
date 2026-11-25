-- ADR-0005 A3.4 item 1: an approval executes once, whichever grant arrives. approval_grant_used is
-- unique on (tenant_id, approval_id) as well as on (tenant_id, jti). Expand-only: nothing is dropped.
CREATE UNIQUE INDEX approval_grant_used_approval_uq ON approval_grant_used (tenant_id, approval_id);
