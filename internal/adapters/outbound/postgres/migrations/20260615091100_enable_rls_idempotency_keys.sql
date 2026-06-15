-- +goose Up
-- +goose StatementBegin

-- tenant_isolation on the three idempotency cache tables (Database Schema §7),
-- the canonical null-safe v1.2 policy form already used by events and the prior
-- tenant tables: the two-argument current_setting (missing_ok = true) plus
-- nullif('') yields NULL — not an error — when no tenant is bound, so a
-- context-less query returns zero rows rather than raising. FORCE makes the
-- policy apply to the table owner (the migration-runner role) too.
--
-- All three carry a tenant_id and therefore the same policy. The reconciliation
-- table denormalizes tenant_id specifically so it can be scoped here even though
-- its functional key is (reader_id, sequence_no). There is no explicit WITH CHECK:
-- Postgres applies the USING expression to the new rows of INSERT/UPDATE as well,
-- so on the RLS-bound pool a row can only be written with the connection's own
-- tenant_id.

ALTER TABLE command_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON command_idempotency_keys
    USING (tenant_id = nullif(current_setting('app.current_tenant_id', true), '')::uuid);

ALTER TABLE decision_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE decision_idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON decision_idempotency_keys
    USING (tenant_id = nullif(current_setting('app.current_tenant_id', true), '')::uuid);

ALTER TABLE reconciliation_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON reconciliation_idempotency_keys
    USING (tenant_id = nullif(current_setting('app.current_tenant_id', true), '')::uuid);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS tenant_isolation ON reconciliation_idempotency_keys;
ALTER TABLE reconciliation_idempotency_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_idempotency_keys DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON decision_idempotency_keys;
ALTER TABLE decision_idempotency_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE decision_idempotency_keys DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON command_idempotency_keys;
ALTER TABLE command_idempotency_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE command_idempotency_keys DISABLE ROW LEVEL SECURITY;
-- +goose StatementEnd
