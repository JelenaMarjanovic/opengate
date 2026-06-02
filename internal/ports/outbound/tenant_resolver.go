package outbound

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// ErrTenantNotFound is returned by TenantResolver.ResolveBySlug when no tenant
// has the requested slug. It is a domain-meaningful sentinel matched via
// errors.Is (System Design §7), distinct from apperr.ErrInternal used for
// unexpected failures. The login use case maps it to a generic "invalid
// credentials" response so an attacker cannot enumerate valid tenant slugs.
var ErrTenantNotFound = errors.New("tenant not found")

// TenantRef is the minimal read-for-auth projection of a tenant: just the
// fields the pre-authentication login flow needs. It is deliberately NOT the
// full domain.Tenant aggregate — this is identity resolution, not aggregate
// reconstitution, so returning the whole aggregate would over-fetch and couple
// the auth path to fields it must not depend on.
type TenantRef struct {
	// ID is the resolved tenant's identifier, used to scope the subsequent
	// user lookup (UserReader.FindByEmail's explicit tenantID argument).
	ID uuid.UUID
	// Status lets the login use case refuse a suspended tenant before checking
	// any credential.
	Status domain.TenantStatus
	// SessionTimeout is the tenant's configured idle window, from which the use
	// case computes a new session's expires_at.
	SessionTimeout time.Duration
}

// TenantResolver resolves a tenant by its URL slug. It is a PRE-AUTHENTICATION
// port: the slug is how the tenant is discovered, so no tenant context exists
// yet, and the Postgres adapter runs it on the BYPASSRLS pool WITHOUT calling
// tenant.FromContext (System Design §10 pre-authentication carve-out).
type TenantResolver interface {
	// ResolveBySlug returns the TenantRef for the given slug, or ErrTenantNotFound
	// if no tenant has it. ctx is first per the universal System Design §7 rule,
	// but the tenant is NOT read from it — the slug is the input that identifies
	// the tenant.
	ResolveBySlug(ctx context.Context, slug string) (TenantRef, error)
}
