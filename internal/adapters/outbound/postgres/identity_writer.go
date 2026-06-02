package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// pgUniqueViolation is the SQLSTATE code Postgres returns for a unique-constraint
// violation (class 23, unique_violation). The tenant insert can trip it via
// tenants_slug_unique when a second bootstrap uses a colliding slug.
const pgUniqueViolation = "23505"

// IdentityWriter is the Postgres adapter implementing ports.IdentityWriter. It
// is constructed over the BYPASSRLS pool because the bootstrap path provisions
// a tenant before any RLS-scoped session exists (System Design §10, §11).
type IdentityWriter struct{ pool *pgxpool.Pool } // bypass pool

// NewIdentityWriter returns an IdentityWriter backed by the given bypass pool.
func NewIdentityWriter(pool *pgxpool.Pool) *IdentityWriter { return &IdentityWriter{pool: pool} }

// CreateTenantWithOwner inserts the tenant and its owner in one transaction. A name
// pre-check inside the tx makes check-and-insert atomic for this operator path
// (concurrent bootstrap is unsupported). Duplicate name -> ports.ErrTenantExists, no write.
func (w *IdentityWriter) CreateTenantWithOwner(ctx context.Context, t domain.Tenant, owner domain.User) (err error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w: %w", apperr.ErrInternal, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}() // commit makes rollback a no-op

	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE name = $1)`, t.Name).Scan(&exists); err != nil {
		return fmt.Errorf("check tenant name: %w: %w", apperr.ErrInternal, err)
	}
	if exists {
		return ports.ErrTenantExists // domain-meaningful; not wrapped in ErrInternal
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO tenants (id, name, slug, contact_email, timezone, session_timeout, status)
         VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		t.ID, t.Name, t.Slug, t.ContactEmail, t.Timezone, t.SessionTimeout, string(t.Status)); err != nil {
		// The name collision is caught by the pre-check above; a slug collision is
		// caught here by tenants_slug_unique. Both map to the same domain sentinel
		// so a second bootstrap fails legibly rather than with a raw pg error.
		if isUniqueViolation(err) {
			return ports.ErrTenantExists
		}
		return fmt.Errorf("insert tenant: %w: %w", apperr.ErrInternal, err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, status, must_change_password)
         VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		owner.ID, owner.TenantID, owner.Email, owner.PasswordHash, string(owner.Role),
		string(owner.Status), owner.MustChangePassword); err != nil {
		return fmt.Errorf("insert owner user: %w: %w", apperr.ErrInternal, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w: %w", apperr.ErrInternal, err)
	}
	return nil
}

// isUniqueViolation reports whether err is (or wraps) a Postgres unique-constraint
// violation. It is used to translate a tenants_slug_unique collision into the
// domain-meaningful ports.ErrTenantExists instead of leaking a raw driver error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
