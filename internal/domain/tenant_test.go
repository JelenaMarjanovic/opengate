package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// TestNewTenantDefaults proves a valid name and slug yield an active tenant with
// the DB §5.1 defaults, the supplied ID, and the slug stored verbatim.
func TestNewTenantDefaults(t *testing.T) {
	id := uuid.New()
	tn, err := domain.NewTenant(id, "  Acme Climbing  ", "acme-climbing")
	if err != nil {
		t.Fatalf("NewTenant: %v", err)
	}

	if tn.ID != id {
		t.Errorf("ID = %s, want %s", tn.ID, id)
	}
	if tn.Name != "Acme Climbing" {
		t.Errorf("Name = %q, want %q (trimmed)", tn.Name, "Acme Climbing")
	}
	if tn.Slug != "acme-climbing" {
		t.Errorf("Slug = %q, want acme-climbing", tn.Slug)
	}
	if tn.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want UTC", tn.Timezone)
	}
	if tn.SessionTimeout != 60*time.Minute {
		t.Errorf("SessionTimeout = %s, want 60m", tn.SessionTimeout)
	}
	if tn.Status != domain.StatusActive {
		t.Errorf("Status = %q, want active", tn.Status)
	}
}

// TestNewTenantRejectsEmptyName proves empty and whitespace-only names are
// rejected with ErrTenantNameRequired (the slug is valid, isolating the name path).
func TestNewTenantRejectsEmptyName(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := domain.NewTenant(uuid.New(), name, "valid-slug"); !errors.Is(err, domain.ErrTenantNameRequired) {
			t.Errorf("NewTenant(%q) err = %v, want ErrTenantNameRequired", name, err)
		}
	}
}

// TestNewTenantRejectsInvalidSlug proves a malformed slug is rejected with
// ErrInvalidSlug even when the name is valid, so an invalid slug can never enter
// the aggregate (and thus never reach the DB).
func TestNewTenantRejectsInvalidSlug(t *testing.T) {
	for _, slug := range []string{"", "Bad Slug", "-acme", "acme-", "ac--me", "UPPER"} {
		if _, err := domain.NewTenant(uuid.New(), "Acme", slug); !errors.Is(err, domain.ErrInvalidSlug) {
			t.Errorf("NewTenant(slug=%q) err = %v, want ErrInvalidSlug", slug, err)
		}
	}
}
