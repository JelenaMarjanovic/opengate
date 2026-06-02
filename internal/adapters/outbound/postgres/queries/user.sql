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

-- name: UpdateUserPasswordHash :execrows
-- Replaces the stored Argon2id PHC hash for one user. Pre-authentication: runs
-- on the bypass pool during the login flow's rehash-on-login step. Scoped by the
-- explicit (id, tenant_id) pair; tenant_id is belt-and-suspenders alongside the
-- future RLS policy and is the only tenant scoping until US-02.05. The SET clause
-- assigns only the bound hash and now(); the new hash is supplied as a parameter
-- and is never computed or read in SQL. :execrows lets the adapter detect a no-op
-- (zero rows -> the user vanished, or the tenant did not match) and map it to
-- ports.ErrUserNotFound.
UPDATE users
SET password_hash = $1, updated_at = now()
WHERE id = $2 AND tenant_id = $3;

-- name: UpdateUserLastLogin :execrows
-- Records the timestamp of a successful login. Pre-authentication, bypass pool,
-- same (id, tenant_id) belt-and-suspenders scoping as UpdateUserPasswordHash. The
-- timestamp is supplied by the caller (the use case owns the clock for
-- testability) rather than defaulting to now() in SQL, so tests can assert an
-- exact value. :execrows carries the same zero-rows -> ErrUserNotFound contract.
UPDATE users
SET last_login_at = $1
WHERE id = $2 AND tenant_id = $3;
