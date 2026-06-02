package outbound

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// ErrSessionNotFound is returned by SessionStore lookups and mutations when the
// targeted session does not exist (or, for the post-authentication mutations, is
// not visible within the caller's tenant). It is a domain-meaningful sentinel
// matched via errors.Is (System Design §7). Whether logout treats "already
// gone" as success is a use-case decision (Step 4/5); the store's contract is
// only to report the row's presence faithfully.
var ErrSessionNotFound = errors.New("session not found")

// NewSession is the fully-formed session row the login use case hands to
// SessionStore.Create. Every field is supplied by the caller (Step 4 mints the
// id and token, hashes the token into TokenHash, snapshots Role at issue time,
// and computes ExpiresAt from the tenant's session timeout); the store defaults
// nothing security-relevant.
type NewSession struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	Role       domain.Role
	IssuedAt   time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	// IssuedFromIP is the client IP recorded for forensics. The zero (invalid)
	// netip.Addr maps to SQL NULL — the column is nullable.
	IssuedFromIP netip.Addr
	// UserAgent is the client user agent recorded for forensics. The empty
	// string maps to SQL NULL — the column is nullable.
	UserAgent string
}

// SessionRecord is the projection FindByTokenHash returns: the session fields a
// validated request needs, plus the two tenant fields the same by-token lookup
// folds in via a JOIN. It deliberately omits TokenHash — the secret is never
// needed after the lookup, and not carrying it keeps it out of logs and structs
// (System Design §9).
type SessionRecord struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	UserID     uuid.UUID
	Role       domain.Role
	IssuedAt   time.Time
	LastSeenAt time.Time
	// ExpiresAt is the session's current expiry; the validate use case compares
	// it against now to decide whether the session is still live.
	ExpiresAt time.Time
	// SessionTimeout is the owning tenant's configured idle window, carried from
	// the JOINed tenants row so the refresh use case can recompute the
	// sliding-window expires_at = now + SessionTimeout without a second query.
	SessionTimeout time.Duration
	// TenantStatus is the owning tenant's CURRENT status (read live via the JOIN,
	// not snapshotted at issue), so the validate use case can reject a session
	// whose tenant has been suspended since it was issued (DB §5.1).
	TenantStatus domain.TenantStatus
}

// SessionStore persists and retrieves session rows. Its methods straddle the
// pre/post-authentication boundary that is the security spine of US-02.03, so
// the adapter wires them to DIFFERENT pools — read each method's contract:
//
//   - Create and FindByTokenHash are PRE-AUTHENTICATION. They run before a
//     tenant context exists (Create is the login act; FindByTokenHash resolves
//     an opaque cookie token whose tenant is the lookup's OUTPUT, not an input).
//     The adapter runs them on the BYPASSRLS pool and does NOT call
//     tenant.FromContext.
//
//   - Refresh and Delete are POST-AUTHENTICATION (System Design §7 convention).
//     By the time they run, the middleware has validated the session and set the
//     tenant context, so the adapter runs them on the regular RLS-bound pool and
//     reads the tenant from context (the panic-on-absent contract is correct
//     here — a missing tenant on these paths is a programming error). The tenant
//     is also applied as an explicit SQL predicate for defense-in-depth before
//     US-02.05's RLS exists.
type SessionStore interface {
	// Create inserts a new session row. Pre-authentication; bypass pool.
	Create(ctx context.Context, s NewSession) error

	// FindByTokenHash resolves a session by the SHA-256 hash of its cookie
	// token, returning ErrSessionNotFound if no row matches. Pre-authentication;
	// bypass pool. The matched row's tenant_id is part of the result, and the same
	// lookup folds in the owning tenant's SessionTimeout and TenantStatus via a
	// JOIN so the validate/refresh use cases need no second query.
	FindByTokenHash(ctx context.Context, tokenHash []byte) (SessionRecord, error)

	// Refresh slides the session window by writing lastSeenAt and expiresAt for
	// the session with the given id, scoped to the tenant from context. Returns
	// ErrSessionNotFound when no row is updated (the session vanished or its
	// tenant does not match). Post-authentication; regular pool.
	Refresh(ctx context.Context, id uuid.UUID, lastSeenAt, expiresAt time.Time) error

	// Delete removes the session with the given id, scoped to the tenant from
	// context. Returns ErrSessionNotFound when no row is deleted (already gone or
	// tenant mismatch). Post-authentication; regular pool.
	Delete(ctx context.Context, id uuid.UUID) error
}
