-- name: ListCasbinPolicyRules :many
-- Lists the `p` (policy) rows from the global casbin_rules table for the
-- authorizer to load. casbin_rules carries no tenant_id and is deliberately
-- outside RLS (it is the same role -> (resource, action) mapping for every
-- tenant), so this runs on the BYPASSRLS pool: the load is a system-level,
-- tenant-less operation, and on the regular RLS-bound pool it would trip the
-- missing-tenant warning. Only v0..v2 are selected because the v1 model is
-- (sub, obj, act) -- a three-field `p` rule -- so v3..v5 are always null here.
SELECT v0, v1, v2 FROM casbin_rules WHERE ptype = 'p';
