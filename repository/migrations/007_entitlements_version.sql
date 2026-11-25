-- ADR-0014 T10: the entitlement snapshot a device holds carries a version, bumped by every plan change
-- and every organisation status change. Expand-only: a new column with a default.
ALTER TABLE organisations ADD COLUMN entitlements_bumps integer NOT NULL DEFAULT 0;
