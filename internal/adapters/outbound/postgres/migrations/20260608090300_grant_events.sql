-- +goose Up
-- +goose StatementBegin
-- Role grants for the events table (Database Schema §6.1), deferred from the
-- table's creation migration (20260608090000). The PostgresEventStore adapter
-- (US-03.03) reaches events from two pools, so the grants split by role.
--
-- opengate_app -- the RLS-bound application pool, used by the command path:
--   INSERT -- Append writes the new events.
--   SELECT -- Load reads an aggregate's stream in sequence order; also required
--            by Append's `RETURNING stream_position`, because Postgres needs
--            SELECT on a column to return it.
GRANT SELECT, INSERT ON events TO opengate_app;
--   USAGE ON SEQUENCE events_stream_position_seq -- Append calls nextval() on
--     the sequence inline to assign each event's global stream position. The
--     column has no DEFAULT nextval (20260608090000 keeps the sequence an
--     independent object so the position can be read BEFORE the insert and
--     carried in metadata), so the privilege is on the sequence, not the table.
--     nextval() requires USAGE; without it the insert fails with "permission
--     denied for sequence events_stream_position_seq" (SQLSTATE 42501). US-03.01
--     did not grant this -- its RLS test used literal stream positions -- so it
--     is introduced here.
GRANT USAGE ON SEQUENCE events_stream_position_seq TO opengate_app;

-- opengate_bypass -- the BYPASSRLS pool, used for cross-tenant reads:
--   SELECT -- ReadAfterPosition (projector cursor scans in global order) and
--            ReadByTenantAndTimeRange (the export capability) both run here
--            because they read across tenants without a bound tenant context.
-- No INSERT and no sequence USAGE: in production, events are written only by the
-- command path on opengate_app, so write access on the bypass role would be dead
-- -- and dangerous -- privilege.
GRANT SELECT ON events TO opengate_bypass;

-- projection_progress grants are intentionally not here: the projector that
-- writes that table arrives with a later story (US-03.04+), and its grants land
-- in that story's migration.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- opengate_app and opengate_bypass do not own events, so these explicit grants
-- do not drop with the table; revoke exactly what Up granted, in reverse order,
-- so the round-trip leaves no lingering privilege.
REVOKE SELECT ON events FROM opengate_bypass;
REVOKE USAGE ON SEQUENCE events_stream_position_seq FROM opengate_app;
REVOKE SELECT, INSERT ON events FROM opengate_app;
-- +goose StatementEnd
