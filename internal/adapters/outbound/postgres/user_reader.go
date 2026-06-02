package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// UserReader is the Postgres adapter implementing ports.UserReader. It is a
// PRE-AUTHENTICATION adapter: constructed over the BYPASSRLS pool, it never
// calls tenant.FromContext. The tenant is supplied as the explicit tenantID
// argument (the result of TenantResolver.ResolveBySlug), not via context,
// because the login flow has not yet established a session (System Design §10).
type UserReader struct{ q *db.Queries } // bypass pool

// Compile-time assertion that the adapter satisfies the port.
var _ ports.UserReader = (*UserReader)(nil)

// NewUserReader returns a UserReader backed by the given BYPASSRLS pool.
func NewUserReader(bypassPool *pgxpool.Pool) *UserReader {
	return &UserReader{q: db.New(bypassPool)}
}

// FindByEmail looks up the user identified by the (tenantID, email) pair,
// returning the read-for-auth AuthUser. A missing row yields
// ports.ErrUserNotFound (mapped from pgx.ErrNoRows); any other failure wraps
// apperr.ErrInternal so no raw pgx error crosses the port boundary. The returned
// PasswordHash is handed to the verification step and is never logged here.
func (r *UserReader) FindByEmail(ctx context.Context, tenantID uuid.UUID, email string) (ports.AuthUser, error) {
	row, err := r.q.FindUserByEmail(ctx, db.FindUserByEmailParams{TenantID: tenantID, Email: email})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.AuthUser{}, ports.ErrUserNotFound
		}
		return ports.AuthUser{}, fmt.Errorf("find user by email: %w: %w", apperr.ErrInternal, err)
	}
	return ports.AuthUser{
		ID:                 row.ID,
		TenantID:           row.TenantID,
		Email:              row.Email,
		PasswordHash:       row.PasswordHash,
		Role:               domain.Role(row.Role),
		Status:             domain.UserStatus(row.Status),
		MustChangePassword: row.MustChangePassword,
	}, nil
}
