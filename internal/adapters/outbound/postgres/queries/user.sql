-- name: FindUserByEmail :one
-- Pre-authentication user lookup (System Design §10 carve-out). Runs on the
-- BYPASSRLS pool because login has not yet established a session, so there is no
-- authenticated tenant context to bind; the adapter does NOT call
-- tenant.FromContext here. The tenant_id is the EXPLICIT first parameter (the
-- result of ResolveTenantBySlug), not ambient context — the (tenant_id, email)
-- pair is what uniquely identifies the row via users_email_per_tenant_unique,
-- which is what makes the explicit-argument scoping correct and safe. Returns
-- password_hash for the verification step plus role / status /
-- must_change_password for the login decision. No row -> the adapter maps
-- pgx.ErrNoRows to ports.ErrUserNotFound.
SELECT id, tenant_id, email, password_hash, role, status, must_change_password
FROM users
WHERE tenant_id = $1 AND email = $2;
