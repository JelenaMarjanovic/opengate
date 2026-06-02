-- +goose Up
-- +goose StatementBegin

-- Add the slug column nullable first. A fresh deployment may already hold a
-- bootstrapped tenant (the bootstrap CLI runs before this migration on an
-- upgrade), so existing rows must be backfilled before NOT NULL can be imposed.
ALTER TABLE tenants ADD COLUMN slug text;

-- Backfill every existing tenant's slug deterministically from its name. The Go
-- side (internal/domain.NormalizeSlug, used by the bootstrap CLI) mirrors this
-- exact rule; the two MUST stay in lockstep, hence the per-step commentary below
-- so a future editor changes both together.
--
-- Why each transformation, for a junior reader:
--   lower(name)
--     Slugs are case-insensitive identifiers that appear in URLs
--     (POST /api/v1/tenants/{slug}/auth/login). Folding case keeps a tenant's
--     slug canonical and avoids "Acme" vs "acme" resolving to two tenants.
--   regexp_replace(..., '[^a-z0-9]+', '-', 'g')
--     Collapse every run of non-alphanumeric characters (spaces, punctuation,
--     accented or non-Latin letters) into a single hyphen -- the one separator
--     the grammar permits. "Acme  Climbing!!" -> "acme-climbing".
--   trim(both '-' from ...)
--     A leading or trailing hyphen would violate the grammar (^[a-z0-9]...$) and
--     reads badly in a URL ("-acme-"). Trimming yields a clean label.
--   collision suffix: '-' || left(id::text, 8)
--     Two distinct names can normalize to the same base ("Acme!" and "Acme?"
--     both -> "acme"). The UNIQUE constraint added below would then reject the
--     backfill. Appending a short, stable suffix taken from the row's unique id
--     (the first 8 hex characters of the UUID) guarantees uniqueness without a
--     second pass. Applied to ALL members of a colliding group so the result is
--     order-independent and deterministic.
--   empty-base fallback: left(id::text, 8)
--     A name with no [a-z0-9] character at all (e.g. all punctuation) normalizes
--     to '', which fails both the grammar and the length>=1 bound. Falling back
--     to the id prefix gives such a row a valid, unique slug instead of breaking
--     the migration. The id prefix is itself a valid slug ([a-z0-9]{8}).
WITH normalized AS (
    SELECT
        id,
        -- Base slug per the normalization rule (no collision handling yet).
        trim(both '-' from regexp_replace(lower(name), '[^a-z0-9]+', '-', 'g')) AS base
    FROM tenants
),
counted AS (
    -- base_count tells us, for each row, how many rows share its base slug.
    SELECT id, base, count(*) OVER (PARTITION BY base) AS base_count
    FROM normalized
)
UPDATE tenants t
SET slug = CASE
    WHEN c.base = ''      THEN left(t.id::text, 8)                       -- underivable name
    WHEN c.base_count > 1 THEN c.base || '-' || left(t.id::text, 8)      -- disambiguate collision
    ELSE c.base                                                         -- unique base, use as-is
END
FROM counted c
WHERE t.id = c.id;

-- Every row now has a value: enforce presence and global uniqueness.
ALTER TABLE tenants ALTER COLUMN slug SET NOT NULL;
ALTER TABLE tenants ADD CONSTRAINT tenants_slug_unique UNIQUE (slug);

-- Enforce the slug grammar at the storage layer. The pattern matches a DNS
-- label (lowercase alphanumeric segments joined by single hyphens, no leading,
-- trailing, or doubled hyphens) and the 1..63 length bound is a DNS label's
-- maximum (RFC 1035) -- a forward-compatibility choice for possible subdomain
-- routing, not a current requirement. The UNIQUE constraint above already
-- creates the supporting index, so no separate CREATE INDEX is added.
ALTER TABLE tenants ADD CONSTRAINT tenants_slug_format_check
    CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) BETWEEN 1 AND 63);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Exact inverse of Up (reverse order) so the round-trip test round-trips and
-- tenants returns to its prior shape. IF EXISTS keeps the Down idempotent.
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_slug_format_check;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_slug_unique;
ALTER TABLE tenants DROP COLUMN IF EXISTS slug;
-- +goose StatementEnd
