-- +goose Up
-- +goose StatementBegin
-- Read access to the global policy table. casbin_rules (20260606090000) was
-- created by the migration superuser and therefore grants no privileges to the
-- application roles by default, so without this GRANT the policy read fails with
-- "permission denied for table casbin_rules" (SQLSTATE 42501).
--
-- Only opengate_bypass needs SELECT: the US-02.04 step-2 policy loader runs the
-- ListCasbinPolicyRules query on the BYPASSRLS pool, because the table is global
-- and the load is a system-level, tenant-less operation. The request path
-- authorizes against the in-memory Casbin enforcer (loaded once on the bypass
-- pool and refreshed), never reading casbin_rules on the regular RLS-bound pool,
-- so opengate_app gets no grant — its SELECT would be dead privilege.
--
-- No role gets INSERT/UPDATE/DELETE: the policy is migration-managed (seeded in
-- 20260606090000) and is never written at runtime.
--
-- Table-wide (not column-level) SELECT is correct here: casbin_rules holds no
-- secret material — it is the same public policy for every tenant — so there is
-- no least-privilege reason to withhold any column. The table carries no
-- tenant_id and is deliberately left out of RLS (US-02.05), so this grant is the
-- complete access control for the table: a global, read-only policy source.
GRANT SELECT ON casbin_rules TO opengate_bypass;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Revoke exactly what Up granted so the round-trip leaves no lingering privilege.
REVOKE SELECT ON casbin_rules FROM opengate_bypass;
-- +goose StatementEnd
