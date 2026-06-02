package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// UserWriter is the Postgres adapter implementing ports.UserWriter. Like
// UserReader it is a PRE-AUTHENTICATION adapter constructed over the BYPASSRLS
// pool: the login flow has not yet established a session, so it never calls
// tenant.FromContext and takes the tenant as the explicit tenantID argument
// (System Design §10).
//
// It is a SEPARATE struct from UserReader, not a shared one. Read and mutation
// are distinct responsibilities with distinct callers, and keeping them apart
// mirrors the small-focused-adapter precedent already set by UserReader,
// TenantResolver, and IdentityWriter — one struct per port. Both share the same
// bypass *db.Queries shape, but that is an implementation similarity, not a
// reason to fuse two ports into one adapter.
type UserWriter struct{ q *db.Queries } // bypass pool

// Compile-time assertion that the adapter satisfies the port.
var _ ports.UserWriter = (*UserWriter)(nil)

// NewUserWriter returns a UserWriter backed by the given BYPASSRLS pool.
func NewUserWriter(bypassPool *pgxpool.Pool) *UserWriter {
	return &UserWriter{q: db.New(bypassPool)}
}

// UpdatePasswordHash replaces the stored Argon2id PHC hash for the (tenantID,
// userID) user — the rehash-on-login write. Zero rows updated maps to
// ports.ErrUserNotFound (the user vanished or the tenant did not match); any
// other failure wraps apperr.ErrInternal so no raw pgx error crosses the port
// boundary (System Design §7). The phc is secret material: it is passed only as
// a bound query parameter and never placed in the error message or any log.
func (w *UserWriter) UpdatePasswordHash(ctx context.Context, tenantID, userID uuid.UUID, phc string) error {
	rows, err := w.q.UpdateUserPasswordHash(ctx, db.UpdateUserPasswordHashParams{
		PasswordHash: phc,
		ID:           userID,
		TenantID:     tenantID,
	})
	if err != nil {
		return fmt.Errorf("update user password hash: %w: %w", apperr.ErrInternal, err)
	}
	if rows == 0 {
		return ports.ErrUserNotFound
	}
	return nil
}

// RecordLastLogin stamps last_login_at at the given time for the (tenantID,
// userID) user. The caller owns the clock, so the timestamp is written verbatim.
// Same zero-rows -> ports.ErrUserNotFound and apperr.ErrInternal wrapping
// semantics as UpdatePasswordHash.
func (w *UserWriter) RecordLastLogin(ctx context.Context, tenantID, userID uuid.UUID, at time.Time) error {
	rows, err := w.q.UpdateUserLastLogin(ctx, db.UpdateUserLastLoginParams{
		LastLoginAt: timestamptz(at),
		ID:          userID,
		TenantID:    tenantID,
	})
	if err != nil {
		return fmt.Errorf("record last login: %w: %w", apperr.ErrInternal, err)
	}
	if rows == 0 {
		return ports.ErrUserNotFound
	}
	return nil
}
