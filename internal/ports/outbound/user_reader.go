package outbound

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// ErrUserNotFound is returned by UserReader.FindByEmail when no user matches the
// (tenantID, email) pair. It is a domain-meaningful sentinel matched via
// errors.Is (System Design §7). The login use case treats it identically to a
// wrong password — same response, same timing — so an attacker cannot tell a
// non-existent account from a wrong password (user enumeration defense).
var ErrUserNotFound = errors.New("user not found")

// AuthUser is the read-for-auth projection of a user: the fields the login flow
// needs to verify a credential and make the login decision. It carries the
// PasswordHash (for verification) but never the session token or any minted
// secret; like TenantRef it is a value type, not the domain.User aggregate.
type AuthUser struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	Email              string
	PasswordHash       string
	Role               domain.Role
	Status             domain.UserStatus
	MustChangePassword bool
}

// UserReader looks up a user for authentication. It is a PRE-AUTHENTICATION
// port: the login flow has not yet established a session, so the Postgres
// adapter runs it on the BYPASSRLS pool and does NOT call tenant.FromContext.
type UserReader interface {
	// FindByEmail returns the AuthUser for the (tenantID, email) pair, or
	// ErrUserNotFound if none exists. ctx is first per the universal System
	// Design §7 rule, but the tenant arrives as the EXPLICIT tenantID argument
	// (the result of TenantResolver.ResolveBySlug), NOT from context — there is
	// no tenant context on this pre-authentication path. The (tenantID, email)
	// pair uniquely identifies the row, which is what makes the explicit-argument
	// scoping correct and safe.
	FindByEmail(ctx context.Context, tenantID uuid.UUID, email string) (AuthUser, error)
}
