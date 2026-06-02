package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidSlug is returned when a tenant slug does not satisfy the slug
// grammar. It is the Go-side counterpart of the tenants_slug_format_check DB
// constraint added by the 20260530090200_add_tenant_slug migration.
var ErrInvalidSlug = errors.New("invalid tenant slug")

// Slug length bounds. The 63-character ceiling is the maximum length of a single
// DNS label (RFC 1035): a deliberate forward-compatibility choice so a slug
// stays usable as a subdomain should per-tenant subdomain routing ever be added.
const (
	slugMinLen = 1
	slugMaxLen = 63
)

// slugFormat is the Go twin of the SQL pattern in tenants_slug_format_check:
// one or more lowercase-alphanumeric segments separated by single hyphens, with
// no leading, trailing, or doubled hyphens. The two MUST stay in lockstep; if
// you change one, change the other (see the add_tenant_slug migration).
var slugFormat = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// nonSlugRun matches any maximal run of characters that are not lowercase ASCII
// alphanumerics. NormalizeSlug collapses each such run to a single hyphen,
// mirroring the migration backfill's regexp_replace(..., '[^a-z0-9]+', '-').
var nonSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

// NormalizeSlug derives a slug candidate from an arbitrary string using the
// exact rule the add_tenant_slug migration applies in SQL during backfill:
// lowercase, then collapse every run of non-[a-z0-9] characters to one hyphen,
// then trim leading and trailing hyphens. The result MAY be empty (for an input
// with no alphanumeric characters) and is NOT guaranteed to satisfy the length
// bound; callers must validate it with ValidateSlug. Keeping this rule identical
// to the SQL backfill is what stops the bootstrap and the migration from
// drifting apart (see the migration's commentary for the SQL form).
func NormalizeSlug(s string) string {
	lowered := strings.ToLower(s)
	hyphenated := nonSlugRun.ReplaceAllString(lowered, "-")
	return strings.Trim(hyphenated, "-")
}

// ValidateSlug reports whether s is a well-formed tenant slug, returning
// ErrInvalidSlug (wrapped with the offending value) otherwise. The accepted set
// is exactly that of the tenants_slug_format_check constraint: the format match
// guarantees an all-ASCII value, so a byte-length bound here equals the
// constraint's char_length bound.
func ValidateSlug(s string) error {
	if !slugFormat.MatchString(s) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidSlug, s, slugFormat.String())
	}
	if n := len(s); n < slugMinLen || n > slugMaxLen {
		return fmt.Errorf("%w: %q has length %d, must be %d..%d", ErrInvalidSlug, s, n, slugMinLen, slugMaxLen)
	}
	return nil
}
