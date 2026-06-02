package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/application/identity"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// fakeIdentityWriter records the arguments of the last CreateTenantWithOwner
// call and returns a configurable error, so the use case is testable without a
// database.
type fakeIdentityWriter struct {
	gotTenant domain.Tenant
	gotOwner  domain.User
	called    bool
	err       error
}

func (f *fakeIdentityWriter) CreateTenantWithOwner(_ context.Context, t domain.Tenant, owner domain.User) error {
	f.called = true
	f.gotTenant = t
	f.gotOwner = owner
	return f.err
}

// TestBootstrapperRunBuildsOwnerAndHashes proves Run hashes the password, mints
// distinct UUIDv7 IDs, and hands the writer an owner of the new tenant.
func TestBootstrapperRunBuildsOwnerAndHashes(t *testing.T) {
	w := &fakeIdentityWriter{}
	b := identity.NewBootstrapper(w)

	if err := b.Run(context.Background(), "Acme Climbing", "acme-climbing", "owner@acme.test", "s3cret"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !w.called {
		t.Fatal("writer.CreateTenantWithOwner was not called")
	}
	if w.gotTenant.Name != "Acme Climbing" {
		t.Errorf("tenant name = %q, want Acme Climbing", w.gotTenant.Name)
	}
	if w.gotTenant.Slug != "acme-climbing" {
		t.Errorf("tenant slug = %q, want acme-climbing", w.gotTenant.Slug)
	}
	if w.gotOwner.Role != domain.RoleOwner {
		t.Errorf("owner role = %q, want owner", w.gotOwner.Role)
	}
	if w.gotOwner.Email != "owner@acme.test" {
		t.Errorf("owner email = %q, want owner@acme.test", w.gotOwner.Email)
	}
	// The plaintext must never reach the writer; a hash must.
	if w.gotOwner.PasswordHash == "s3cret" || w.gotOwner.PasswordHash == "" {
		t.Errorf("password hash = %q, want a non-plaintext hash", w.gotOwner.PasswordHash)
	}
	// Tenant and owner get distinct IDs; the owner is bound to the new tenant.
	if w.gotTenant.ID == w.gotOwner.ID {
		t.Error("tenant and owner share an ID; want distinct UUIDs")
	}
	if w.gotOwner.TenantID != w.gotTenant.ID {
		t.Errorf("owner.TenantID = %s, want tenant.ID %s", w.gotOwner.TenantID, w.gotTenant.ID)
	}
}

// TestBootstrapperRunPropagatesTenantExists proves the port's ErrTenantExists
// reaches the caller unchanged so the CLI can map it to a clear message.
func TestBootstrapperRunPropagatesTenantExists(t *testing.T) {
	w := &fakeIdentityWriter{err: ports.ErrTenantExists}
	b := identity.NewBootstrapper(w)

	err := b.Run(context.Background(), "Acme", "acme", "owner@acme.test", "s3cret")
	if !errors.Is(err, ports.ErrTenantExists) {
		t.Errorf("Run err = %v, want ErrTenantExists", err)
	}
}

// TestBootstrapperRunRejectsBadEmail proves a malformed email fails in the
// domain constructor before the writer is ever touched.
func TestBootstrapperRunRejectsBadEmail(t *testing.T) {
	w := &fakeIdentityWriter{}
	b := identity.NewBootstrapper(w)

	if err := b.Run(context.Background(), "Acme", "acme", "not-an-email", "s3cret"); err == nil {
		t.Fatal("expected error for malformed email")
	}
	if w.called {
		t.Error("writer was called despite invalid email")
	}
}
