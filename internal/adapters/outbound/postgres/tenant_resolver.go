package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// TenantResolver is the Postgres adapter implementing ports.TenantResolver. It
// is a PRE-AUTHENTICATION adapter: it is constructed over the BYPASSRLS pool and
// never reads a tenant from context, because the slug it is given IS how the
// tenant is being discovered (System Design §10 pre-authentication carve-out).
type TenantResolver struct{ q *db.Queries } // bypass pool

// Compile-time assertion that the adapter satisfies the port.
var _ ports.TenantResolver = (*TenantResolver)(nil)

// NewTenantResolver returns a TenantResolver backed by the given BYPASSRLS pool.
func NewTenantResolver(bypassPool *pgxpool.Pool) *TenantResolver {
	return &TenantResolver{q: db.New(bypassPool)}
}

// ResolveBySlug resolves a tenant by its URL slug, returning the read-for-auth
// TenantRef. An unknown slug yields ports.ErrTenantNotFound (mapped from
// pgx.ErrNoRows); any other failure wraps apperr.ErrInternal so no raw pgx error
// crosses the port boundary (System Design §7).
func (r *TenantResolver) ResolveBySlug(ctx context.Context, slug string) (ports.TenantRef, error) {
	row, err := r.q.ResolveTenantBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.TenantRef{}, ports.ErrTenantNotFound
		}
		return ports.TenantRef{}, fmt.Errorf("resolve tenant by slug: %w: %w", apperr.ErrInternal, err)
	}
	return ports.TenantRef{
		ID:             row.ID,
		Status:         domain.TenantStatus(row.Status),
		SessionTimeout: durationFromInterval(row.SessionTimeout),
	}, nil
}
