-- name: ResolveTenantBySlug :one
-- Pre-authentication tenant resolution (System Design §10 carve-out). The slug
-- arrives from the URL path segment and IS how the tenant is discovered, so no
-- tenant context exists yet; the adapter runs this on the BYPASSRLS pool and
-- does NOT call tenant.FromContext. Returns exactly the fields the login use
-- case needs: the id (to scope the subsequent user lookup), the status (to
-- refuse a suspended tenant), and the session_timeout (to compute session
-- expiry). No row when the slug is unknown -> the adapter maps pgx.ErrNoRows to
-- ports.ErrTenantNotFound.
SELECT id, status, session_timeout
FROM tenants
WHERE slug = $1;
