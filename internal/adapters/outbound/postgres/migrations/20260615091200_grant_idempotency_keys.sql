-- +goose Up
-- +goose StatementBegin
-- Role grants for the idempotency cache tables (Database Schema §7), deferred from
-- their create migration and split by the runtime SQL path that needs them. Only
-- the command-path and cleanup-path grants land here; the decision table's
-- application grants (E4) and any reconciliation grants (E6) are deferred to those
-- stories. The decision table still gets the bypass DELETE now because the cleanup
-- job (commit 3) purges it.
--
-- opengate_app -- the RLS-bound application pool, used by the command middleware
-- (commit 2):
--   SELECT -- the replay lookup reads the cached response for a seen key.
--   INSERT -- the record statement persists a completed command's response as
--            `INSERT ... ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`.
-- No UPDATE: ON CONFLICT DO NOTHING has no UPDATE branch, so Postgres does NOT
-- require UPDATE here -- unlike ON CONFLICT DO UPDATE, which did in the US-03.04
-- River finding. No DELETE: the application path never purges; the cleanup job runs
-- on the bypass pool below.
GRANT SELECT, INSERT ON command_idempotency_keys TO opengate_app;

-- opengate_bypass -- the BYPASSRLS worker pool, used by the cleanup job (commit 3):
--   DELETE -- the cleanup is `DELETE ... WHERE created_at < ...`.
-- It runs against both purged tables. BYPASSRLS exempts the role from row-level
-- security but NOT from table privileges (the US-03.05 projection_progress lesson),
-- so this grant is required for the cleanup to run in production.
--
-- NOT SUFFICIENT on its own: the claim originally written here -- that DELETE covers
-- the predicate, so no SELECT is needed -- is false. Postgres also requires SELECT on
-- each column READ to evaluate the qualifier, i.e. created_at. The follow-up
-- migration 20260615091300 adds the minimal column-level `SELECT (created_at)`; see
-- its header for the empirical evidence. This comment is corrected rather than the
-- DDL: the grant above is right, it was merely incomplete, and migrations are
-- append-only once written.
GRANT DELETE ON command_idempotency_keys TO opengate_bypass;
GRANT DELETE ON decision_idempotency_keys TO opengate_bypass;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- opengate_app and opengate_bypass do not own these tables, so the grants do not
-- drop with them; revoke exactly what Up granted, in reverse order, so the
-- round-trip leaves no lingering privilege.
REVOKE DELETE ON decision_idempotency_keys FROM opengate_bypass;
REVOKE DELETE ON command_idempotency_keys FROM opengate_bypass;
REVOKE SELECT, INSERT ON command_idempotency_keys FROM opengate_app;
-- +goose StatementEnd
