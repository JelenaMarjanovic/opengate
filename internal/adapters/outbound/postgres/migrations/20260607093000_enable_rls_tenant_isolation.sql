-- +goose Up
-- +goose StatementBegin

-- tenant_isolation constrains visible rows to the connection's bound tenant,
-- read from the app.current_tenant_id session variable set by the pool's
-- AfterAcquire hook. The two-argument current_setting (missing_ok = true)
-- plus nullif('') yields NULL — not an error — when no tenant is bound, so a
-- context-less query returns zero rows (AC2) rather than raising. This is a
-- deliberate deviation from the literal SQL in Database Schema §13 / System
-- Design §10, which raise on the empty or unset variable; logged for the v1.2
-- reconciliation pass. FORCE makes the policy apply to the table owner too,
-- which matters because `tenants` is owned by the migration-runner role
-- rather than opengate_app.

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenants
    USING (id = nullif(current_setting('app.current_tenant_id', true), '')::uuid);

ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON users
    USING (tenant_id = nullif(current_setting('app.current_tenant_id', true), '')::uuid);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sessions
    USING (tenant_id = nullif(current_setting('app.current_tenant_id', true), '')::uuid);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS tenant_isolation ON sessions;
ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sessions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON users;
ALTER TABLE users NO FORCE ROW LEVEL SECURITY;
ALTER TABLE users DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON tenants;
ALTER TABLE tenants NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants DISABLE ROW LEVEL SECURITY;
-- +goose StatementEnd
