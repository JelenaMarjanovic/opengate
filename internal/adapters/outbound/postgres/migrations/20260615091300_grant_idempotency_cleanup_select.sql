-- +goose Up
-- +goose StatementBegin
-- Corrective grant for the cleanup.idempotency_keys job (US-03.06, commit 3).
--
-- The previous migration (20260615091200) gave opengate_bypass only DELETE on the
-- two purged tables, on the stated assumption that "DELETE privilege covers the
-- predicate, so no separate SELECT is needed". That assumption is wrong. Postgres
-- checks privileges per column: a column whose value is READ to evaluate a
-- statement requires SELECT on that column, and `DELETE ... WHERE created_at < ...`
-- reads created_at. With DELETE alone the purge fails at runtime with
--
--   ERROR: permission denied for table command_idempotency_keys
--
-- Verified empirically against postgres:16.14 (the image the container tests pin):
-- the identical statement errors with DELETE alone and succeeds once the grant below
-- is in place. The cleanup job's container test exercises exactly this path as
-- opengate_bypass, so a regression here fails the build rather than production.
--
-- The grant is COLUMN-level, and that is the point. The job needs to read exactly
-- one column — the retention boundary — while command_idempotency_keys also stores
-- the cached response body of every completed command. A table-level SELECT would
-- hand the worker role read access to all of that for no reason. `SELECT
-- (created_at)` leaves the job able to find expired rows and unable to read what
-- they cached; note that has_table_privilege(...,'SELECT') therefore stays FALSE
-- while has_column_privilege(...,'created_at','SELECT') is TRUE.
--
-- reconciliation_idempotency_keys is deliberately absent: it is never purged (its
-- keys protect events that are themselves never deleted — System Design §4), so the
-- worker role gets neither DELETE nor SELECT on it, and a future edit that tried to
-- purge it would fail loudly instead of silently destroying an audit trail.
GRANT SELECT (created_at) ON command_idempotency_keys TO opengate_bypass;
GRANT SELECT (created_at) ON decision_idempotency_keys TO opengate_bypass;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Revoke exactly what Up granted, in reverse order. Column-level privileges are
-- revoked with the same column list; opengate_bypass does not own these tables, so
-- nothing here drops implicitly with them.
REVOKE SELECT (created_at) ON decision_idempotency_keys FROM opengate_bypass;
REVOKE SELECT (created_at) ON command_idempotency_keys FROM opengate_bypass;
-- +goose StatementEnd
