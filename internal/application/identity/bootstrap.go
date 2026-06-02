package identity

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/auth"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// Bootstrapper creates the first tenant and its owner user. It depends only on
// the IdentityWriter port, never on a concrete adapter (System Design §7).
type Bootstrapper struct {
	writer ports.IdentityWriter
}

// NewBootstrapper returns a Bootstrapper that persists through the given writer.
func NewBootstrapper(writer ports.IdentityWriter) *Bootstrapper {
	return &Bootstrapper{writer: writer}
}

// Run hashes the owner password, mints UUIDv7 IDs for the tenant and owner,
// builds the domain aggregates, and persists them atomically via the writer. The
// tenantSlug is resolved by the caller (explicit or derived from the name) and
// re-validated here by NewTenant. The writer's ErrTenantExists propagates
// unchanged so the caller can map it to an operator-facing message.
func (b *Bootstrapper) Run(ctx context.Context, tenantName, tenantSlug, ownerEmail, ownerPassword string) error {
	hash, err := auth.HashPassword(ownerPassword)
	if err != nil {
		return fmt.Errorf("hash owner password: %w", err)
	}

	// UUIDv7 IDs are time-ordered, which keeps the primary-key index append-mostly.
	tenantID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate tenant id: %w", err)
	}
	userID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate owner id: %w", err)
	}

	tenant, err := domain.NewTenant(tenantID, tenantName, tenantSlug)
	if err != nil {
		return fmt.Errorf("build tenant: %w", err)
	}
	owner, err := domain.NewOwnerUser(userID, tenantID, ownerEmail, hash)
	if err != nil {
		return fmt.Errorf("build owner user: %w", err)
	}

	return b.writer.CreateTenantWithOwner(ctx, tenant, owner)
}
