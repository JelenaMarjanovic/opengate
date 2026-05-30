package domain_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// TestNewOwnerUserDefaults proves a valid email and hash yield an active owner
// who is not forced to change the password, with the email normalized.
func TestNewOwnerUserDefaults(t *testing.T) {
	id, tenantID := uuid.New(), uuid.New()
	u, err := domain.NewOwnerUser(id, tenantID, "Owner Name <owner@acme.test>", "$argon2id$hash")
	if err != nil {
		t.Fatalf("NewOwnerUser: %v", err)
	}

	if u.ID != id || u.TenantID != tenantID {
		t.Errorf("ID/TenantID = %s/%s, want %s/%s", u.ID, u.TenantID, id, tenantID)
	}
	if u.Email != "owner@acme.test" {
		t.Errorf("Email = %q, want normalized owner@acme.test", u.Email)
	}
	if u.Role != domain.RoleOwner {
		t.Errorf("Role = %q, want owner", u.Role)
	}
	if u.Status != domain.UserStatusActive {
		t.Errorf("Status = %q, want active", u.Status)
	}
	if u.MustChangePassword {
		t.Error("MustChangePassword = true, want false")
	}
}

// TestNewOwnerUserRejectsBadEmail proves an unparseable email is rejected.
func TestNewOwnerUserRejectsBadEmail(t *testing.T) {
	if _, err := domain.NewOwnerUser(uuid.New(), uuid.New(), "not-an-email", "$argon2id$hash"); !errors.Is(err, domain.ErrInvalidEmail) {
		t.Errorf("err = %v, want ErrInvalidEmail", err)
	}
}

// TestNewOwnerUserRejectsEmptyHash proves an empty password hash is rejected;
// the domain never stores an unhashed or missing credential.
func TestNewOwnerUserRejectsEmptyHash(t *testing.T) {
	if _, err := domain.NewOwnerUser(uuid.New(), uuid.New(), "owner@acme.test", ""); !errors.Is(err, domain.ErrPasswordHashRequired) {
		t.Errorf("err = %v, want ErrPasswordHashRequired", err)
	}
}
