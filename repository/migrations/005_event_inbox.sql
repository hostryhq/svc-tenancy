-- Consumer side of the event bus (ADR-0011 B5): the inbox of go-common (inbox.Schema, copied verbatim) so the
-- handler's effect and its dedupe row commit in one transaction, and the table for events parked after the
-- last attempt. Tenancy consumes billing.trading_day.changed and billing.series.reconciled.
-- Service-internal tables: no tenant column, written only by the inbox subscriber.

CREATE TABLE IF NOT EXISTS inbox (
  consumer     text        NOT NULL,
  event_id     uuid        NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);
CREATE TABLE IF NOT EXISTS event_parked (
  id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  consumer    text        NOT NULL,
  event_id    text        NOT NULL,
  subject     text        NOT NULL,
  payload     bytea       NOT NULL,
  error       text        NOT NULL,
  attempts    bigint      NOT NULL,
  parked_at   timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz
);

GRANT SELECT, INSERT ON inbox, event_parked TO tenancy_app;
-- The application role never deletes. Old inbox rows are removed by the sweep, as the relay role.
GRANT SELECT, DELETE ON inbox TO tenancy_relay;
