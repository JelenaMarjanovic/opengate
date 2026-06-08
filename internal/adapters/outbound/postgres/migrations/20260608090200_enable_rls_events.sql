-- +goose Up
-- +goose StatementBegin

-- tenant_isolation constrains visible events to the connection's bound tenant,
-- read from the app.current_tenant_id session variable set by the pool's
-- AfterAcquire hook. This is the null-safe v1.2 form (Database Schema §13): the
-- two-argument current_setting (missing_ok = true) plus nullif('') yields NULL —
-- not an error — when no tenant is bound, so a context-less query returns zero
-- rows rather than raising. FORCE makes the policy apply to the table owner too.
--
-- Only the events table is touched here. The full §13 block also enables RLS on
-- readers, subscriptions, and the *_view tables that do not exist yet; tenants,
-- users, and sessions already have this policy from US-02.05. Each table gets
-- its policy in the migration that introduces it.
--
-- There is no explicit WITH CHECK: Postgres applies the USING expression to the
-- new rows of INSERT/UPDATE as well, so on the RLS-bound pool an event can only
-- be written with the connection's own tenant_id. That insert-side enforcement
-- is exercised by the SQL-level RLS isolation test for events.
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON events
    USING (tenant_id = nullif(current_setting('app.current_tenant_id', true), '')::uuid);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS tenant_isolation ON events;
ALTER TABLE events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE events DISABLE ROW LEVEL SECURITY;
-- +goose StatementEnd
