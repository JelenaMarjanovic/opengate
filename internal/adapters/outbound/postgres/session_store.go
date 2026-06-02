package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// SessionStore is the Postgres adapter implementing ports.SessionStore. It is
// the one adapter that straddles the pre/post-authentication boundary, so it is
// constructed with BOTH pools and each method reaches for the correct one:
//
//   - Create and FindByTokenHash (PRE-authentication) run on the bypass pool.
//     Create is the login act itself and FindByTokenHash resolves an opaque
//     cookie token before any tenant context exists, so neither can bind a
//     tenant and neither calls tenant.FromContext.
//   - Refresh and Delete (POST-authentication) run on the regular RLS-bound
//     pool. By the time they execute the middleware has set the tenant context,
//     so they read it with tenant.FromContext (panic-on-absent is correct: a
//     missing tenant here is a programming error) and pass it as an explicit SQL
//     predicate for defense-in-depth ahead of US-02.05's RLS.
//
// One struct with both pools is chosen over two structs because the four methods
// are one cohesive store from the use case's perspective, and the pool choice is
// an implementation detail each method documents rather than a reason to
// fragment the port. The pool hooks on the regular pool bind/reset the tenant
// session variable around every Refresh/Delete automatically (see pool.go).
type SessionStore struct {
	bypass  *db.Queries // pre-auth: Create, FindByTokenHash
	regular *db.Queries // post-auth: Refresh, Delete (tenant-bound by pool hooks + ctx)
}

// Compile-time assertion that the adapter satisfies the port.
var _ ports.SessionStore = (*SessionStore)(nil)

// NewSessionStore returns a SessionStore wired to the BYPASSRLS pool for the
// pre-authentication methods and the regular RLS-bound pool for the
// post-authentication methods.
func NewSessionStore(bypassPool, regularPool *pgxpool.Pool) *SessionStore {
	return &SessionStore{
		bypass:  db.New(bypassPool),
		regular: db.New(regularPool),
	}
}

// Create inserts a new session row on the bypass pool (pre-authentication). All
// values come from the caller; an insert failure wraps apperr.ErrInternal.
func (s *SessionStore) Create(ctx context.Context, ns ports.NewSession) error {
	err := s.bypass.CreateSession(ctx, db.CreateSessionParams{
		ID:           ns.ID,
		TenantID:     ns.TenantID,
		UserID:       ns.UserID,
		TokenHash:    ns.TokenHash,
		Role:         string(ns.Role),
		IssuedAt:     timestamptz(ns.IssuedAt),
		LastSeenAt:   timestamptz(ns.LastSeenAt),
		ExpiresAt:    timestamptz(ns.ExpiresAt),
		IssuedFromIp: nullableAddr(ns.IssuedFromIP),
		UserAgent:    nullableString(ns.UserAgent),
	})
	if err != nil {
		return fmt.Errorf("create session: %w: %w", apperr.ErrInternal, err)
	}
	return nil
}

// FindByTokenHash resolves a session by its token hash on the bypass pool
// (pre-authentication). A missing row yields ports.ErrSessionNotFound (mapped
// from pgx.ErrNoRows); any other failure wraps apperr.ErrInternal. The matched
// row's tenant_id is returned as part of the SessionRecord — it is the output of
// this lookup, not an input. The query JOINs tenants, so the same row also
// carries the tenant's session_timeout and status: the interval is converted to
// a time.Duration via durationFromInterval (the SAME helper ResolveBySlug uses,
// so the two interval call sites cannot diverge) and the status text is mapped
// into the typed domain.TenantStatus.
func (s *SessionStore) FindByTokenHash(ctx context.Context, tokenHash []byte) (ports.SessionRecord, error) {
	row, err := s.bypass.FindSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.SessionRecord{}, ports.ErrSessionNotFound
		}
		return ports.SessionRecord{}, fmt.Errorf("find session by token hash: %w: %w", apperr.ErrInternal, err)
	}
	return ports.SessionRecord{
		ID:             row.ID,
		TenantID:       row.TenantID,
		UserID:         row.UserID,
		Role:           domain.Role(row.Role),
		IssuedAt:       row.IssuedAt.Time,
		LastSeenAt:     row.LastSeenAt.Time,
		ExpiresAt:      row.ExpiresAt.Time,
		SessionTimeout: durationFromInterval(row.SessionTimeout),
		TenantStatus:   domain.TenantStatus(row.Status),
	}, nil
}

// Refresh slides the session window on the regular RLS-bound pool
// (post-authentication). The tenant is read from context and passed as the
// explicit tenant_id predicate (belt-and-suspenders ahead of RLS); zero rows
// updated maps to ports.ErrSessionNotFound (the session vanished or the tenant
// did not match).
func (s *SessionStore) Refresh(ctx context.Context, id uuid.UUID, lastSeenAt, expiresAt time.Time) error {
	tid := tenant.FromContext(ctx) // post-auth: a missing tenant here is a programming error
	rows, err := s.regular.RefreshSession(ctx, db.RefreshSessionParams{
		LastSeenAt: timestamptz(lastSeenAt),
		ExpiresAt:  timestamptz(expiresAt),
		ID:         id,
		TenantID:   uuid.UUID(tid),
	})
	if err != nil {
		return fmt.Errorf("refresh session: %w: %w", apperr.ErrInternal, err)
	}
	if rows == 0 {
		return ports.ErrSessionNotFound
	}
	return nil
}

// Delete removes a session on the regular RLS-bound pool (post-authentication).
// The tenant is read from context and passed as the explicit tenant_id predicate
// (belt-and-suspenders ahead of RLS); zero rows deleted maps to
// ports.ErrSessionNotFound (already gone or tenant mismatch). Whether logout
// treats that as success is decided by the use case, not here.
func (s *SessionStore) Delete(ctx context.Context, id uuid.UUID) error {
	tid := tenant.FromContext(ctx) // post-auth: a missing tenant here is a programming error
	rows, err := s.regular.DeleteSession(ctx, db.DeleteSessionParams{
		ID:       id,
		TenantID: uuid.UUID(tid),
	})
	if err != nil {
		return fmt.Errorf("delete session: %w: %w", apperr.ErrInternal, err)
	}
	if rows == 0 {
		return ports.ErrSessionNotFound
	}
	return nil
}
