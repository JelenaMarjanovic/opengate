-- name: CreateSession :exec
-- Pre-authentication session insert (login). Runs on the BYPASSRLS pool because
-- login is the authentication act itself — no tenant context exists yet. Every
-- value is supplied by the caller: the Step 4 use case mints the id and token,
-- hashes the token, snapshots the role, and computes expires_at from the
-- tenant's session_timeout. No security-relevant column is defaulted in SQL —
-- the caller is the single source of truth for the row's contents.
INSERT INTO sessions (
    id, tenant_id, user_id, token_hash, role,
    issued_at, last_seen_at, expires_at, issued_from_ip, user_agent
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: FindSessionByTokenHash :one
-- Pre-authentication session lookup, run before the tenant context exists: the
-- cookie carries only the opaque token, so the lookup is by the globally-unique
-- token_hash and the tenant_id is READ FROM the matched row (it cannot be an
-- input — it is the output). Runs on the BYPASSRLS pool; the adapter does NOT
-- call tenant.FromContext. token_hash is deliberately NOT selected back out: it
-- is never needed after the lookup, and not returning it keeps the secret
-- material out of logs and structs.
--
-- Joined to tenants so ONE round trip also yields the tenant's session_timeout
-- (the sliding-window expires_at = last_seen_at + session_timeout recompute the
-- refresh use case needs) and status (so the validate use case can reject a
-- session whose tenant was suspended since issue — DB §5.1). The JOIN is safe on
-- the bypass pool: opengate_bypass holds SELECT on both sessions and tenants
-- (create_app_roles). It is an INNER join and never drops a session, because
-- sessions.tenant_id is NOT NULL REFERENCES tenants(id). No row -> the adapter
-- maps pgx.ErrNoRows to ports.ErrSessionNotFound.
SELECT
    s.id, s.tenant_id, s.user_id, s.role,
    s.issued_at, s.last_seen_at, s.expires_at,
    t.session_timeout, t.status
FROM sessions s
JOIN tenants t ON t.id = s.tenant_id
WHERE s.token_hash = $1;

-- name: RefreshSession :execrows
-- Post-authentication sliding-window refresh (System Design §7 convention). By
-- the time this runs, the middleware has validated the session and set the
-- tenant context, so the adapter executes it on the regular RLS-bound pool and
-- takes the tenant from context, passing it as $4.
--
-- The `tenant_id = $4` predicate is BELT-AND-SUSPENDERS, not redundant noise —
-- do NOT "simplify" it away. (a) RLS does not exist until US-02.05, so until
-- then this explicit predicate is the ONLY tenant scoping on the write. (b) Once
-- RLS lands, the explicit predicate and the policy agree, and the redundancy is
-- cheap defense-in-depth — the dual-layer philosophy (System Design §10). The
-- `id = $3` predicate is the primary key. :execrows lets the caller detect
-- "zero rows updated" — a session that vanished, or a tenant mismatch.
UPDATE sessions
SET last_seen_at = $1, expires_at = $2
WHERE id = $3 AND tenant_id = $4;

-- name: DeleteSession :execrows
-- Post-authentication logout (System Design §7 convention): regular RLS-bound
-- pool, tenant taken from context by the adapter and passed as $2. The
-- `tenant_id = $2` predicate is the same belt-and-suspenders scoping as
-- RefreshSession (see the note above) — the only tenant scoping until US-02.05's
-- RLS lands, and cheap defense-in-depth after. Do NOT remove it. :execrows so
-- logout can distinguish "deleted" from "already gone."
DELETE FROM sessions
WHERE id = $1 AND tenant_id = $2;
