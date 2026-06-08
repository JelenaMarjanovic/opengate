package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// predicatelessUsersQuery reads users with NO WHERE tenant_id predicate. It is
// the linchpin of the dual-layer proof: the SAME statement run on the regular
// (opengate_app) pool under a tenant context returns only that tenant's rows
// (RLS does the scoping, AC3), while on the bypass (opengate_bypass) pool it
// returns every tenant's rows (BYPASSRLS exempts the role, AC4). Issuing it
// directly — not through a sqlc-generated function, which always carries the
// application's tenant filter — is what isolates RLS as the cause.
const predicatelessUsersQuery = `SELECT id FROM users`

// TestRLSTenantIsolation is the dual-layer verification for US-02.05. The
// "regular" pool connects as opengate_app (subject to RLS) and the "bypass" pool
// as opengate_bypass (BYPASSRLS) — role-specific DSNs, not the container
// superuser, so the policies are actually exercised rather than bypassed
// vacuously. Data is seeded through the bypass pool, the only role that can write
// under forced RLS with no tenant bound. The four subtests map one-to-one to
// AC1–AC4; AC5 (rollback) is the separate TestRLSMigrationRollback below, kept
// apart because it tears RLS down while AC1–AC4 need it enabled throughout.
func TestRLSTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startMigratedContainer(ctx, t) // starts + migrates up as superuser
	superPool := openSuperuserPool(ctx, t, container)
	bypassPool := openBypassPool(ctx, t, container)
	regularPool := openRegularPool(ctx, t, container) // opengate_app, RLS-bound, discard logger

	// Scaffold the minimal read privilege opengate_app lacks in v1 so the regular
	// pool can OBSERVE the policy on tenants and users (see grantAppSelect). Granted
	// by the superuser in the arrange phase, never in the migration.
	grantAppSelect(ctx, t, superPool, "tenants")
	grantAppSelect(ctx, t, superPool, "users")

	// Two tenants, one user each, and one session for tenant A's user. The seed is
	// done on the bypass pool: forced RLS plus an empty tenant context would reject
	// these writes on the regular pool. The A-tenant session makes the AC2 sessions
	// assertion non-vacuous — zero rows there proves RLS, not an empty table.
	tenantA, tenantB := uuid.New(), uuid.New()
	userA, userB := uuid.New(), uuid.New()
	seedTenant(ctx, t, bypassPool, tenantA, "RLS Tenant A", "rls-tenant-a", 60*time.Minute)
	seedTenant(ctx, t, bypassPool, tenantB, "RLS Tenant B", "rls-tenant-b", 60*time.Minute)
	seedUser(ctx, t, bypassPool, userA, tenantA, "a@rls.test", "hash-A", false)
	seedUser(ctx, t, bypassPool, userB, tenantB, "b@rls.test", "hash-B", false)
	seedSession(ctx, t, bypassPool, uuid.New(), tenantA, userA)

	// The tenant context the pool's AfterAcquire hook reads (US-02.03).
	ctxA := tenant.NewContext(ctx, tenant.ID(tenantA))

	// AC1: under a tenant-A context the regular pool sees only tenant A's user.
	t.Run("AC1 regular pool under tenant A sees only A's user", func(t *testing.T) {
		got := selectIDs(ctxA, t, regularPool, predicatelessUsersQuery)
		t.Logf("AC1: regular pool (opengate_app) under tenant A=%s sees users %v; tenant B user %s must be absent",
			userA, got, userB)
		if len(got) != 1 || got[0] != userA {
			t.Errorf("AC1: visible users under tenant A = %v, want exactly [%s]", got, userA)
		}
		if containsUUID(got, userB) {
			t.Errorf("AC1: tenant B's user %s leaked into tenant A's view %v", userB, got)
		}
	})

	// AC2: under a context with NO tenant, the regular pool returns zero rows from
	// all three tables AND emits the AfterAcquire missing-tenant warning.
	t.Run("AC2 no-tenant context yields zero rows and a warning", func(t *testing.T) {
		// A dedicated pool with a capturing logger so the warning is observable in
		// isolation. The bind hook fires on every acquire; with no tenant in ctx it
		// binds '' and warns, and the policy's nullif('') -> NULL makes id/tenant_id
		// = NULL (never true), so each table yields zero rows.
		var buf bytes.Buffer
		capturingPool, err := postgres.NewPool(ctx, deriveAppDSN(ctx, t, container),
			observability.NewLogger(&buf, slog.LevelDebug))
		if err != nil {
			t.Fatalf("new capturing pool: %v", err)
		}
		defer capturingPool.Close()

		// All three reads use the context-less ctx (no tenant). sessions is read via
		// id alone, which opengate_app holds at the column level without scaffolding.
		for _, q := range []struct {
			table, query string
		}{
			{"tenants", `SELECT id FROM tenants`},
			{"users", `SELECT id FROM users`},
			{"sessions", `SELECT id FROM sessions`},
		} {
			got := selectIDs(ctx, t, capturingPool, q.query)
			t.Logf("AC2: regular pool with NO tenant context -> %s returned %d rows (want 0)", q.table, len(got))
			if len(got) != 0 {
				t.Errorf("AC2 %s: got %d rows %v, want 0", q.table, len(got), got)
			}
		}

		// The warning proves the null-safe policy form is REQUIRED: with the literal
		// current_setting('app.current_tenant_id')::uuid the context-less query would
		// raise on '' (invalid uuid) instead of returning zero rows.
		rec := findWarnLine(t, buf.Bytes())
		t.Logf("AC2: captured AfterAcquire warning -> level=%v event=%v hook=%v msg=%q",
			rec[slog.LevelKey], rec["event"], rec["hook"], rec["msg"])
		if rec["event"] != "missing_tenant" {
			t.Errorf("AC2 warn event = %v, want missing_tenant", rec["event"])
		}
		if rec["hook"] != "prepare_conn" {
			t.Errorf("AC2 warn hook = %v, want prepare_conn", rec["hook"])
		}
	})

	// AC3: the predicate-less query (no WHERE tenant_id) on the regular pool under
	// tenant A still returns only A's rows — RLS, not the application filter, scopes
	// the result. Same statement AC4 runs on the bypass pool with the opposite result.
	t.Run("AC3 predicate-less query on regular pool stays tenant-scoped", func(t *testing.T) {
		got := selectIDs(ctxA, t, regularPool, predicatelessUsersQuery)
		t.Logf("AC3: predicate-less `SELECT id FROM users` on regular pool under tenant A -> %v (RLS-scoped, not app-filtered)", got)
		if containsUUID(got, userB) {
			t.Errorf("AC3: predicate-less query leaked tenant B's user %s: %v", userB, got)
		}
		if len(got) != 1 || got[0] != userA {
			t.Errorf("AC3: predicate-less query under tenant A = %v, want exactly [%s]", got, userA)
		}
	})

	// AC4: the SAME predicate-less query on the bypass pool returns BOTH tenants'
	// users, confirming the protection is RLS-specific and comes from the role's
	// BYPASSRLS exemption — not from an application filter or some other accident.
	t.Run("AC4 predicate-less query on bypass pool sees both tenants", func(t *testing.T) {
		got := selectIDs(ctx, t, bypassPool, predicatelessUsersQuery)
		t.Logf("AC4: same predicate-less query on bypass pool (opengate_bypass, BYPASSRLS) -> %v (both A=%s and B=%s present)",
			got, userA, userB)
		if !containsUUID(got, userA) || !containsUUID(got, userB) {
			t.Errorf("AC4: bypass pool visible users = %v, want both %s and %s", got, userA, userB)
		}
		if len(got) != 2 {
			t.Errorf("AC4: bypass pool returned %d users %v, want exactly 2 (A and B)", len(got), got)
		}
	})
}

// beforeTenantRLSVersion is the goose version (timestamp prefix) of
// grant_casbin_rules_select, the migration directly beneath
// enable_rls_tenant_isolation (20260607093000). Rolling DownTo this version
// reverts the identity RLS migration regardless of how many later migrations
// (US-03.01's event store and beyond) sit above it.
const beforeTenantRLSVersion int64 = 20260607090000

// TestRLSMigrationRollback is AC5: after every migration is applied, rolling back
// through enable_rls_tenant_isolation must drop the tenant_isolation policy from
// all three identity tables. Originally a single one-step Down — valid while that
// migration was the most recent — it now rolls DownTo the version directly beneath
// it (beforeTenantRLSVersion) because US-03.01 stacked the event-store migrations
// on top. It is deliberately separate from the AC1–AC4 assertions, which require
// RLS enabled throughout. The structure mirrors TestMigrationsRoundTrip: a
// superuser sql.DB driving the goose provider.
func TestRLSMigrationRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := testsupport.StartPostgres(ctx, t)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	// Apply every migration; enable_rls_tenant_isolation creates the policy on the
	// three identity tables.
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	// Sanity (RLS enabled): the policy is present on all three tables when up. This
	// also proves the migration's Up actually created the policies.
	for _, table := range []string{"tenants", "users", "sessions"} {
		assertTenantIsolationPolicy(t, db, table, true)
	}
	t.Log("AC5: RLS migration applied -> tenant_isolation present on tenants, users, sessions")

	// Roll back everything down to (but keeping) the migration directly beneath
	// enable_rls_tenant_isolation, so that migration's Down runs and drops the three
	// policies. DownTo — not a blind one-step Down — because US-03.01's event-store
	// migrations now sit on top; the target version is independent of how many.
	if _, err := provider.DownTo(ctx, beforeTenantRLSVersion); err != nil {
		t.Fatalf("down to %d: %v", beforeTenantRLSVersion, err)
	}
	// AC5: pg_policies shows no tenant_isolation on any of the three tables.
	for _, table := range []string{"tenants", "users", "sessions"} {
		assertTenantIsolationPolicy(t, db, table, false)
	}
	t.Log("AC5: rolled back through enable_rls_tenant_isolation -> tenant_isolation absent on tenants, users, sessions")
}

// grantAppSelect is test-only scaffolding. opengate_app holds NO grant on
// tenants or users in v1 (confirmed in preflight), so without this the regular
// pool cannot SELECT them to OBSERVE the tenant_isolation policy — a context-less
// or cross-tenant read would fail with "permission denied for table" (SQLSTATE
// 42501) instead of returning the RLS-filtered rows the ACs assert. It is granted
// here, by the superuser, in the test's arrange phase — NOT in the migration —
// because production has no regular-pool reader of these tables yet; a migration
// grant would be dead privilege. sessions needs no scaffolding: opengate_app's
// column-level SELECT(id, tenant_id) already covers the id the probes read.
// `table` is a trusted in-test constant, never user input.
func grantAppSelect(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "GRANT SELECT ON "+table+" TO opengate_app"); err != nil {
		t.Fatalf("grant select on %s to opengate_app: %v", table, err)
	}
}

// seedSession inserts one minimal valid session via the bypass pool. role is an
// allowed value and expires_at is set (both NOT NULL / CHECK-constrained);
// issued_at and last_seen_at default to now(). The token hash is any unique
// value. Values are trusted in-test constants, never user input.
func seedSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, tenantID, userID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, tenant_id, user_id, token_hash, role, expires_at)
		 VALUES ($1, $2, $3, $4, 'owner', now() + interval '1 hour')`,
		id, tenantID, userID, tokenHash(id.String()))
	if err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

// selectIDs runs a single-column id query through the pool and collects the
// returned UUIDs. It is used for the RLS visibility assertions, where the SET of
// visible ids — not the row contents — is what each AC turns on.
func selectIDs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id from %q: %v", query, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", query, err)
	}
	return ids
}

// containsUUID reports whether target is present in ids.
func containsUUID(ids []uuid.UUID, target uuid.UUID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// assertTenantIsolationPolicy asserts the presence (want=true) or absence
// (want=false) of the tenant_isolation policy on the given table, read from
// pg_policies — the catalog view of row-security policies.
func assertTenantIsolationPolicy(t *testing.T, db *sql.DB, table string, want bool) {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM pg_policies
			WHERE schemaname = 'public' AND tablename = $1 AND policyname = 'tenant_isolation'
		)`, table,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("query pg_policies for %s: %v", table, err)
	}
	if exists != want {
		t.Fatalf("tenant_isolation policy on %s present = %v, want %v", table, exists, want)
	}
}
