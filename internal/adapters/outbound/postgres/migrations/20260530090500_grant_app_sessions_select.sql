-- +goose Up
-- +goose StatementBegin
-- Column-level SELECT for the post-authentication session mutations (US-02.03,
-- Step 3). The Step 2 grant (20260530090400) gave opengate_app UPDATE and DELETE
-- on sessions but withheld SELECT, on the reasoning that US-02.03 reads sessions
-- only on the bypass pool. That reasoning was incomplete: PostgreSQL requires
-- SELECT privilege on every column read in an UPDATE/DELETE *WHERE* clause, not
-- just UPDATE/DELETE on the table. The two post-auth queries are
--   UPDATE sessions SET last_seen_at=$1, expires_at=$2 WHERE id=$3 AND tenant_id=$4
--   DELETE FROM sessions                                WHERE id=$1 AND tenant_id=$2
-- so opengate_app must be able to read `id` and `tenant_id` or both statements
-- fail with "permission denied for table sessions" (SQLSTATE 42501).
--
-- The grant is COLUMN-LEVEL on exactly (id, tenant_id) — the only columns the
-- predicates read — rather than table-wide SELECT, to preserve least privilege:
-- opengate_app still cannot read token_hash, user_agent, issued_from_ip, or any
-- other session column. The SET targets are literal parameters and read no
-- column, so no further SELECT columns are needed. When US-02.05 forces RLS on
-- sessions, this SELECT will additionally be constrained to the connection's
-- tenant by the tenant_isolation policy.
GRANT SELECT (id, tenant_id) ON sessions TO opengate_app;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Revoke exactly what Up granted so the round-trip leaves no lingering privilege.
REVOKE SELECT (id, tenant_id) ON sessions FROM opengate_app;
-- +goose StatementEnd
