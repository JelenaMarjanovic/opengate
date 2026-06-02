package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// TestNormalizeSlug proves the Go normalization matches the rule the
// add_tenant_slug migration applies in SQL: lowercase, collapse non-[a-z0-9]
// runs to a single hyphen, trim leading/trailing hyphens. An input with no
// alphanumeric characters normalizes to the empty string (caller must handle).
func TestNormalizeSlug(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Acme Climbing", "acme-climbing"},
		{"  Acme   Gym!!  ", "acme-gym"},
		{"UPPER_lower 99", "upper-lower-99"},
		{"---weird---", "weird"},
		{"a.b_c/d", "a-b-c-d"},
		{"café", "caf"}, // non-ASCII letters are not [a-z0-9]; they collapse to hyphens
		{"!!!", ""},     // no alphanumerics -> empty, signaling "underivable"
		{"", ""},
	}
	for _, c := range cases {
		if got := domain.NormalizeSlug(c.in); got != c.want {
			t.Errorf("NormalizeSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestValidateSlugAccepts proves well-formed slugs (including a 63-char maximum)
// pass validation.
func TestValidateSlugAccepts(t *testing.T) {
	valid := []string{"a", "acme", "acme-gym", "a1-b2-c3", "0", strings.Repeat("a", 63)}
	for _, s := range valid {
		if err := domain.ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", s, err)
		}
	}
}

// TestValidateSlugRejects proves malformed or out-of-bound slugs return
// ErrInvalidSlug, matching the tenants_slug_format_check constraint's rejections.
func TestValidateSlugRejects(t *testing.T) {
	invalid := []string{
		"",                      // empty
		"Acme",                  // uppercase
		"acme gym",              // space
		"-acme",                 // leading hyphen
		"acme-",                 // trailing hyphen
		"ac--me",                // doubled hyphen
		"acme_gym",              // underscore
		strings.Repeat("a", 64), // over the 63-char bound
	}
	for _, s := range invalid {
		if err := domain.ValidateSlug(s); !errors.Is(err, domain.ErrInvalidSlug) {
			t.Errorf("ValidateSlug(%q) = %v, want ErrInvalidSlug", s, err)
		}
	}
}
