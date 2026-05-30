package outbound

import (
	"context"
	"errors"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// ErrTenantExists is returned by IdentityWriter.CreateTenantWithOwner when a
// tenant with the requested name already exists. It is a domain-meaningful
// sentinel matched via errors.Is (System Design §7), distinct from the generic
// apperr.ErrInternal used for unexpected failures.
var ErrTenantExists = errors.New("tenant with that name already exists")

// IdentityWriter persists the initial identity records of a tenant. It is the
// outbound port the bootstrap use case depends on; the Postgres adapter
// implements it against the BYPASSRLS operator pool (System Design §10, §11).
type IdentityWriter interface {
	// CreateTenantWithOwner inserts the tenant and its owner user atomically.
	// A duplicate tenant name returns ErrTenantExists with no rows written.
	CreateTenantWithOwner(ctx context.Context, t domain.Tenant, owner domain.User) error
}
