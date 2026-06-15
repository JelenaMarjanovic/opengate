-- +goose Up
-- +goose StatementBegin
-- Role grants for projection_progress (Database Schema §6.2), deferred from
-- create_projection_progress and called out in grant_events: "the projector that
-- writes that table arrives with a later story (US-03.04+), and its grants land in
-- that story's migration." US-03.05 is that story.
--
-- The projector runner reads and advances projection_progress on the BYPASSRLS pool
-- (opengate_bypass), so that role needs:
--   SELECT -- the boundary read joins projection_progress for last_consumed_xid, and
--            the empty-batch path reads last_event_at for the lag sample.
--   UPDATE -- advancing the watermark (last_consumed_xid, last_position, last_event_at).
--
-- No INSERT and no DELETE: the v1 rows are seeded by create_projection_progress and a
-- new projector adds its row in its own migration, so the runner only ever reads and
-- updates an existing row; write access beyond UPDATE would be dead -- and dangerous
-- -- privilege. projection_progress carries no tenant_id and no RLS (a single pass
-- across all tenants), so these are plain table grants with no policy interaction.
--
-- BYPASSRLS exempts a role from row-level security policies; it does NOT confer table
-- privileges, so this explicit grant is required even though the role is bypass-capable.
GRANT SELECT, UPDATE ON projection_progress TO opengate_bypass;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- opengate_bypass does not own projection_progress, so the grant does not drop with
-- the table; revoke exactly what Up granted so the round-trip leaves no privilege behind.
REVOKE SELECT, UPDATE ON projection_progress FROM opengate_bypass;
-- +goose StatementEnd
