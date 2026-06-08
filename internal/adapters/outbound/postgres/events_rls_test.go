package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// predicatelessEventsQuery reads event ids with NO WHERE tenant_id predicate, so
// any tenant scoping it shows comes from RLS rather than an application filter —
// the same isolation device the US-02.05 users probe uses. Run on the regular
// (opengate_app) pool under a tenant context it returns only that tenant's
// events; on the bypass pool it would return every tenant's.
const predicatelessEventsQuery = `SELECT id FROM events`

// eventInsertSQL inserts one fully specified events row. Every column is NOT NULL
// with no default, so all ten are supplied. aggregate_type, event_type, and the
// empty JSON payload/metadata are fixed shapes — the assertions turn on
// id/tenant_id/aggregate_id/sequence/stream_position, not the body. occurred_at
// is now(); stream_position is passed explicitly because the column has no
// sequence default (the application assigns it at append time, Database Schema §6.1).
const eventInsertSQL = `
INSERT INTO events
    (id, tenant_id, aggregate_id, aggregate_type, sequence, stream_position, event_type, payload, metadata, occurred_at)
VALUES
    ($1, $2, $3, 'member', $4, $5, 'member.created.v1', '{}'::jsonb, '{}'::jsonb, now())`

// TestEventsRLSTenantIsolation is the SQL-level verification of the events
// tenant_isolation policy (US-03.01). It mirrors the US-02.05 RLS test: the
// "regular" pool connects as opengate_app (subject to RLS) and the "bypass" pool
// as opengate_bypass (BYPASSRLS) — role-specific DSNs, not the container
// superuser, so the policy is actually exercised rather than bypassed vacuously.
// Data is seeded through the bypass pool, the only role that can write under
// forced RLS with no tenant bound. There is no EventStore adapter yet (US-03.03),
// so every assertion goes straight through the pools. The subtests cover, in
// order: cross-tenant read invisibility, the insert WITH CHECK derived from the
// USING clause (AC for insert-side enforcement), the per-aggregate unique
// constraint (AC3, SQLSTATE 23505), and fail-closed reads under no tenant context.
func TestEventsRLSTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startMigratedContainer(ctx, t) // starts + migrates up as superuser
	superPool := openSuperuserPool(ctx, t, container)
	bypassPool := openBypassPool(ctx, t, container)
	regularPool := openRegularPool(ctx, t, container) // opengate_app, RLS-bound, discard logger

	// Scaffold the table privileges neither application role holds on events in v1
	// (no production reader/writer until US-03.03, so a migration grant would be
	// dead privilege). Granted by the superuser in the arrange phase, never in the
	// migration — the same rationale as grantAppSelect in the US-02.05 RLS test. No
	// grant on tenants is needed: the FK referential-integrity check bypasses RLS,
	// so opengate_app appends an event for its own tenant without being able to
	// SELECT the tenants row (verified directly against PG16).
	grantEventsAccess(ctx, t, superPool)

	// Two tenants, one event each, seeded on the bypass pool — forced RLS plus an
	// empty tenant context would reject these writes on the regular pool. Ids are
	// generated in Go so later subtests can target B's event by id and reuse A's
	// aggregate to provoke the unique-constraint violation.
	tenantA, tenantB := uuid.New(), uuid.New()
	aggA, aggB := uuid.New(), uuid.New()
	eventA, eventB := uuid.New(), uuid.New()
	seedTenant(ctx, t, bypassPool, tenantA, "Events Tenant A", "events-tenant-a", 60*time.Minute)
	seedTenant(ctx, t, bypassPool, tenantB, "Events Tenant B", "events-tenant-b", 60*time.Minute)
	seedEvent(ctx, t, bypassPool, eventA, tenantA, aggA, 1, 1)
	seedEvent(ctx, t, bypassPool, eventB, tenantB, aggB, 1, 2)

	// The tenant context the pool's AfterAcquire hook reads (US-02.03).
	ctxA := tenant.NewContext(ctx, tenant.ID(tenantA))

	// Read isolation: under tenant A the regular pool sees only A's event, and a
	// targeted read of B's event id returns nothing. Declared first so A still has
	// exactly one event (the insert subtest below adds another for A).
	t.Run("read isolation: tenant A sees only A's event", func(t *testing.T) {
		got := selectIDs(ctxA, t, regularPool, predicatelessEventsQuery)
		t.Logf("regular pool (opengate_app) under tenant A=%s sees events %v; tenant B event %s must be absent",
			tenantA, got, eventB)
		if len(got) != 1 || got[0] != eventA {
			t.Errorf("visible events under tenant A = %v, want exactly [%s]", got, eventA)
		}
		if containsUUID(got, eventB) {
			t.Errorf("tenant B's event %s leaked into tenant A's view %v", eventB, got)
		}

		// A direct lookup of B's event by id still returns zero rows: RLS hides the
		// row even when the predicate names it, so this is not an app-filter artifact.
		targeted := selectIDs(ctxA, t, regularPool, `SELECT id FROM events WHERE id = $1`, eventB)
		t.Logf("targeted `WHERE id = %s` (tenant B) under tenant A -> %d rows (want 0)", eventB, len(targeted))
		if len(targeted) != 0 {
			t.Errorf("targeted read of tenant B's event %s under tenant A = %v, want 0 rows", eventB, targeted)
		}
	})

	// Insert WITH CHECK (derived from USING): bound to A, an INSERT carrying
	// tenant_id = B is rejected by the policy (SQLSTATE 42501), while the same
	// insert for tenant_id = A succeeds — proving the insert-side enforcement.
	t.Run("insert with-check: tenant A may not write B's tenant_id", func(t *testing.T) {
		// Cross-tenant write. The aggregate_id and stream_position are fresh, so the
		// ONLY thing that can reject this row is the RLS policy, not a unique constraint.
		err := insertEvent(ctxA, regularPool, uuid.New(), tenantB, uuid.New(), 1, 1001)
		pgErr := requirePgCode(t, err, "42501", "cross-tenant insert (tenant_id=B) on a tenant-A connection")
		t.Logf("cross-tenant insert rejected by policy: SQLSTATE=%s msg=%q", pgErr.Code, pgErr.Message)

		// The identical insert for the connection's own tenant is accepted...
		ownEvent := uuid.New()
		if err := insertEvent(ctxA, regularPool, ownEvent, tenantA, uuid.New(), 1, 1002); err != nil {
			t.Fatalf("own-tenant insert (tenant_id=A) on a tenant-A connection: %v", err)
		}
		// ...and becomes visible to tenant A through the same RLS-scoped read.
		got := selectIDs(ctxA, t, regularPool, predicatelessEventsQuery)
		t.Logf("after own-tenant insert, tenant A sees events %v (must include %s)", got, ownEvent)
		if !containsUUID(got, ownEvent) {
			t.Errorf("own-tenant insert %s not visible under tenant A: %v", ownEvent, got)
		}
	})

	// AC3: a second event reusing A's (aggregate_id, sequence) is rejected by the
	// events_aggregate_sequence_unique constraint with SQLSTATE 23505. Run on the
	// bypass pool (BYPASSRLS) so RLS plays no part and the unique constraint is the
	// sole cause; a fresh id and a distinct stream_position rule out the PK and the
	// stream_position unique constraint.
	t.Run("AC3: duplicate (aggregate_id, sequence) violates unique constraint", func(t *testing.T) {
		err := insertEvent(ctx, bypassPool, uuid.New(), tenantA, aggA, 1, 2001)
		pgErr := requirePgCode(t, err, "23505", "duplicate (aggregate_id, sequence) insert")
		t.Logf("duplicate (aggregate_id, sequence) rejected: SQLSTATE=%s constraint=%q", pgErr.Code, pgErr.ConstraintName)
		if pgErr.ConstraintName != "events_aggregate_sequence_unique" {
			t.Errorf("unique violation constraint = %q, want events_aggregate_sequence_unique", pgErr.ConstraintName)
		}
	})

	// Fail closed: the regular pool with NO tenant in context binds '' on acquire,
	// and the null-safe policy (nullif('') -> NULL) makes tenant_id = NULL — never
	// true — so the predicate-less read returns zero of the seeded rows. This is the
	// events-specific complement to the US-02.05 AC2 generic fail-closed check.
	t.Run("fail-closed: no tenant context yields zero events rows", func(t *testing.T) {
		got := selectIDs(ctx, t, regularPool, predicatelessEventsQuery)
		t.Logf("regular pool with NO tenant context -> events returned %d rows (want 0)", len(got))
		if len(got) != 0 {
			t.Errorf("no-tenant read of events = %v, want 0 rows (fail-closed)", got)
		}
	})
}

// grantEventsAccess grants opengate_app and opengate_bypass the table-level
// SELECT+INSERT on events that neither holds in v1. RLS exemption (BYPASSRLS on
// opengate_bypass) is not a privilege grant, so the bypass role still needs the
// table grant to seed. The superuser owns events (it ran the migration), so it is
// the role that can GRANT here. Test-only scaffolding, deliberately out of the
// migration — see the comment at the call site.
func grantEventsAccess(ctx context.Context, t *testing.T, superPool *pgxpool.Pool) {
	t.Helper()
	for _, role := range []string{"opengate_app", "opengate_bypass"} {
		// role is a trusted in-test constant, never user input.
		if _, err := superPool.Exec(ctx, "GRANT SELECT, INSERT ON events TO "+role); err != nil {
			t.Fatalf("grant events access to %s: %v", role, err)
		}
	}
}

// seedEvent inserts one events row via the given pool, failing the test on error.
// It is the fatal arrange-phase wrapper over insertEvent.
func seedEvent(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, tenantID, aggregateID uuid.UUID, sequence, streamPosition int64) {
	t.Helper()
	if err := insertEvent(ctx, pool, id, tenantID, aggregateID, sequence, streamPosition); err != nil {
		t.Fatalf("seed event %s (tenant %s): %v", id, tenantID, err)
	}
}

// insertEvent appends one events row through the pool and RETURNS the error
// rather than failing, so the RLS and unique-constraint subtests can assert the
// exact SQLSTATE. The tenant binding, when the pool is the RLS-bound regular pool,
// comes from the tenant carried in ctx (the AfterAcquire hook reads it).
func insertEvent(ctx context.Context, pool *pgxpool.Pool, id, tenantID, aggregateID uuid.UUID, sequence, streamPosition int64) error {
	_, err := pool.Exec(ctx, eventInsertSQL, id, tenantID, aggregateID, sequence, streamPosition)
	return err
}

// requirePgCode asserts err is a *pgconn.PgError carrying wantCode (a SQLSTATE),
// failing with what for context, and returns the PgError so callers can make
// further assertions (e.g. on ConstraintName). It checks the structured SQLSTATE,
// not message prose, so it is stable across Postgres wording changes.
func requirePgCode(t *testing.T, err error, wantCode, what string) *pgconn.PgError {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got nil error, want SQLSTATE %s", what, wantCode)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: got %T (%v), want *pgconn.PgError with SQLSTATE %s", what, err, err, wantCode)
	}
	if pgErr.Code != wantCode {
		t.Fatalf("%s: SQLSTATE = %s (%q), want %s", what, pgErr.Code, pgErr.Message, wantCode)
	}
	return pgErr
}
