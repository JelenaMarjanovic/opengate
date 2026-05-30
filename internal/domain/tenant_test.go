package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// TestNewTenantDefaults proves a valid name yields an active tenant with the
// DB §5.1 defaults and the supplied ID.
func TestNewTenantDefaults(t *testing.T) {
	id := uuid.New()
	tn, err := domain.NewTenant(id, "  Acme Climbing  ")
	if err != nil {
		t.Fatalf("NewTenant: %v", err)
	}

	if tn.ID != id {
		t.Errorf("ID = %s, want %s", tn.ID, id)
	}
	if tn.Name != "Acme Climbing" {
		t.Errorf("Name = %q, want %q (trimmed)", tn.Name, "Acme Climbing")
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
// rejected with ErrTenantNameRequired.
func TestNewTenantRejectsEmptyName(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := domain.NewTenant(uuid.New(), name); !errors.Is(err, domain.ErrTenantNameRequired) {
			t.Errorf("NewTenant(%q) err = %v, want ErrTenantNameRequired", name, err)
		}
	}
}
