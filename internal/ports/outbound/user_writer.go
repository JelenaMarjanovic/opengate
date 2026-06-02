package outbound

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// UserWriter performs the user mutations the login flow runs on a successful
// authentication: the rehash-on-login password write and the last-login stamp.
// It is a SEPARATE interface from UserReader by design — read and mutation are
// different responsibilities with different callers, and the segregation mirrors
// the small-focused-port split already implicit across this package
// (TenantResolver, UserReader, IdentityWriter are each one cohesive port). The
// reuse of ErrUserNotFound (declared alongside UserReader) over a parallel
// sentinel is deliberate: a zero-rows mutation means exactly what a missing read
// means.
//
// All methods are PRE-AUTHENTICATION: they run during login, before a session —
// and therefore any tenant context — exists, so the tenant is passed EXPLICITLY
// and the Postgres adapter runs them on the BYPASSRLS pool and does NOT call
// tenant.FromContext (System Design §10). Callers scope every mutation by the
// (tenantID, userID) pair, which the future RLS policy (US-02.05) will reinforce
// rather than replace.
type UserWriter interface {
	// UpdatePasswordHash replaces the stored Argon2id PHC hash for the user
	// identified by (tenantID, userID) — the rehash-on-login write. Returns
	// ErrUserNotFound if no row matched (the user was removed, or the tenant did
	// not match), so the caller can decide whether that is benign. The phc is
	// secret material and must never be logged.
	UpdatePasswordHash(ctx context.Context, tenantID, userID uuid.UUID, phc string) error

	// RecordLastLogin stamps last_login_at at the given time for the user
	// identified by (tenantID, userID). The caller owns the clock (the time is an
	// explicit argument, not now() in SQL) so it is testable. Same not-found
	// semantics as UpdatePasswordHash.
	RecordLastLogin(ctx context.Context, tenantID, userID uuid.UUID, at time.Time) error
}
