package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

const (
	// setTenantSQL binds the request's tenant onto the checked-out connection at
	// session scope. The value is passed positionally as $1 and NEVER concatenated
	// into the statement, so a tenant id can never become a SQL-injection vector.
	// set_config(..., false) sets the variable at session scope (the third arg,
	// is_local=false), so the binding persists for the whole checkout rather than a
	// single transaction.
	setTenantSQL = `SELECT set_config('app.current_tenant_id', $1, false)`

	// clearTenantSQL resets the tenant binding to the empty string. It runs in two
	// places: when no tenant is present at acquire time (fail closed), and on every
	// release, so a recycled connection never carries a previous tenant's id.
	//
	// CONTRACT (forward dependency on US-02.05): the reset value is the empty
	// string '', NOT NULL. The RLS policies introduced in US-02.05 MUST therefore
	// use the empty-tolerant form
	//     tenant_id = nullif(current_setting('app.current_tenant_id', true), '')::uuid
	// so that both an unset variable and this empty-string reset yield zero rows
	// instead of erroring on ''::uuid. Do not change this reset value without
	// changing that policy, and vice versa.
	clearTenantSQL = `SELECT set_config('app.current_tenant_id', '', false)`
)

// resetReleaseTimeout bounds the reset query that AfterRelease runs on its own
// background context. The request context is gone (and may be canceled) by
// release time, so the reset cannot ride on it; a few seconds is generous for a
// single set_config round-trip yet still fails fast on a wedged connection.
const resetReleaseTimeout = 5 * time.Second

// execer is the narrow slice of *pgx.Conn the tenant hooks depend on: a single
// Exec. The hooks bind to this seam rather than to *pgx.Conn directly so the
// bind and reset logic is unit-testable with a stub (or a deliberately-closed
// connection) without standing up a live pool. *pgx.Conn satisfies it.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewBypassPool opens a pgx connection pool for the BYPASSRLS operator path
// (System Design §10): the bootstrap CLI and, later, the data export job.
//
// Unlike the regular request pool (NewPool), it installs no tenant-binding
// hooks. Operator paths run outside any single tenant's RLS scope, so there is
// no app.current_tenant_id to bind.
func NewBypassPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create bypass pool: %w", err)
	}
	return pool, nil
}

// NewPool opens the regular, RLS-bound pgx connection pool used by the
// authenticated request path (US-02.03). Every connection handed to a caller is
// bound to the request's tenant before use (PrepareConn) and scrubbed when it is
// returned (AfterRelease), so the upcoming US-02.05 RLS policies always see the
// correct app.current_tenant_id and a recycled connection never leaks one
// tenant's id to the next caller.
//
// It differs from NewBypassPool in two deliberate ways:
//
//   - It takes a *slog.Logger. The hooks must log — a missing tenant on this pool
//     is a warning, and a failed reset on release is an error — so the logger is
//     injected through the constructor and closed over by the hooks rather than
//     reaching for slog.Default(). NewBypassPool installs no hooks and needs none.
//   - It parses the DSN into a *pgxpool.Config first, so the hooks can be
//     registered before the pool is built. NewBypassPool, having no hooks, uses
//     the simpler pgxpool.New.
//
// NewPool fails fast on an empty or unparseable DSN; its caller (the future
// serve command) is responsible for validating that DATABASE_URL is set.
func NewPool(ctx context.Context, dsn string, logger *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}

	// PrepareConn runs immediately before a pooled connection is handed to the
	// caller, with the caller's request context — which carries the tenant. It is
	// the pgx v5.9 successor to the deprecated BeforeAcquire and has identical
	// timing. Returning a nil error means "do not fail the instigating query": on
	// a bind failure bindTenant returns false, which tells the pool to destroy
	// this connection and transparently retry the acquire on a fresh one, instead
	// of handing the caller a connection that is not correctly bound.
	cfg.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		return bindTenant(ctx, conn, logger), nil
	}

	// AfterRelease runs after the caller returns the connection, but before it
	// goes back into the pool. No context is passed in (see resetReleaseTimeout),
	// so the reset rides on its own bounded background context.
	cfg.AfterRelease = func(conn *pgx.Conn) bool {
		ctx, cancel := context.WithTimeout(context.Background(), resetReleaseTimeout)
		defer cancel()
		return releaseReset(ctx, conn, logger)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// bindTenant binds the request's tenant onto conn at session scope before the
// connection is handed to the caller, and reports whether the connection is
// valid to use (false makes the pool discard it and retry on another).
//
// The tenant is read with tenant.IDFromContext, the panic-free accessor:
// tenant.FromContext panics when no tenant is set, which inside a pool hook would
// take down the request, so the missing-tenant case must be handled, not crash.
func bindTenant(ctx context.Context, conn execer, logger *slog.Logger) bool {
	id, ok := tenant.IDFromContext(ctx)
	if !ok {
		// A missing tenant on the RLS-bound pool is a programming error: this is
		// the authenticated path, so a tenant should always be present. We FAIL
		// CLOSED — bind the variable to '' explicitly so the connection's state is
		// deterministic whether it is fresh or recycled, and once US-02.05 enables
		// RLS this empty value denies all rows (via the nullif form documented on
		// clearTenantSQL) rather than leaking the previous caller's tenant. We warn
		// rather than crash, and return true: returning false here would make the
		// pool discard the connection and immediately retry, and since the missing
		// tenant comes from the context (shared by every retry) that would loop.
		if _, err := conn.Exec(ctx, clearTenantSQL); err != nil {
			// The empty-bind itself failing is a broken-connection problem, not the
			// missing-tenant condition, so discarding and retrying on a fresh
			// connection is correct and cannot loop (a healthy connection binds ''
			// without error). Surface it as an error and return false.
			logger.LogAttrs(ctx, slog.LevelError,
				"postgres: clearing tenant binding for tenant-less acquire failed; discarding connection",
				slog.String("hook", "prepare_conn"),
				slog.String("error", err.Error()),
			)
			return false
		}
		logger.LogAttrs(ctx, slog.LevelWarn,
			"postgres: no tenant in context on RLS-bound pool; bound empty (RLS will deny all rows)",
			slog.String("hook", "prepare_conn"),
			slog.String("event", "missing_tenant"),
		)
		return true
	}

	if _, err := conn.Exec(ctx, setTenantSQL, id.String()); err != nil {
		logger.LogAttrs(ctx, slog.LevelError,
			"postgres: binding tenant on acquire failed; discarding connection",
			slog.String("hook", "prepare_conn"),
			slog.String("error", err.Error()),
		)
		return false
	}
	return true
}

// releaseReset clears the tenant binding when a connection is released back to
// the pool, and reports whether the connection is safe to reuse. It is the body
// of the AfterRelease hook, extracted behind the execer seam so the failure path
// is unit-testable without a live pool.
//
// Why reset at all: set_config(..., false) is SESSION-scoped, so the binding
// survives the checkout. Without this explicit reset, the next caller to check
// out a recycled connection would inherit the previous caller's
// app.current_tenant_id — a cross-tenant data leak once RLS is enforced. Every
// release must therefore leave the connection clean.
//
// Why a failed reset destroys the connection (returns false): a connection we
// cannot PROVE is clean must never be reused. Logging-and-continuing would
// return a possibly-dirty connection to the pool; returning false makes the pool
// discard it instead, trading one connection for the guarantee of isolation.
func releaseReset(ctx context.Context, conn execer, logger *slog.Logger) bool {
	// The reset value is the empty string (see clearTenantSQL): the US-02.05 RLS
	// policy's nullif(..., '') form depends on it. Do not change one without the other.
	if _, err := conn.Exec(ctx, clearTenantSQL); err != nil {
		logger.LogAttrs(ctx, slog.LevelError,
			"postgres: resetting tenant binding on release failed; discarding connection",
			slog.String("hook", "after_release"),
			slog.String("error", err.Error()),
		)
		return false
	}
	return true
}
