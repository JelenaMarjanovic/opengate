package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/coordination/advisory"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// The cleanup tests run against a real Postgres container: the retention boundary is
// evaluated by the database (now() inside the purge transaction), the singleton is a
// real advisory lock, and the privileges the job runs under are real grants. None of
// those is observable through an in-memory double.
//
// The job is driven DIRECTLY (CleanupIdempotencyKeys), never through River's
// scheduler, matching how the projector runner is tested: asserting on async
// scheduling is flaky (the US-03.04 AC-3 lesson), and the scheduling itself is
// asserted separately in the queue adapter.

// cleanupEnv is the shared fixture. The two pools are the production split, not a
// convenience:
//
//   - bypass runs the job as opengate_bypass, the exact role and privilege set the
//     worker uses in production. Running the purge as the superuser would prove the
//     SQL and hide the grants; this way a missing grant fails the build.
//   - super seeds and asserts. It has to be a second role, because opengate_bypass
//     deliberately holds no INSERT anywhere here and no SELECT on the row bodies —
//     it cannot set up or observe its own fixtures, which is the point.
type cleanupEnv struct {
	super    *pgxpool.Pool
	bypass   *pgxpool.Pool
	tenantID uuid.UUID
}

// TestIdempotencyCleanup drives the cleanup job's scenarios against one container.
func TestIdempotencyCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	env := setupCleanup(ctx, t)

	t.Run("AC-3 expired command keys deleted, fresh retained", func(t *testing.T) {
		testExpiredCommandKeys(ctx, t, env)
	})
	t.Run("AC-3 expired decision keys deleted, fresh retained", func(t *testing.T) {
		testExpiredDecisionKeys(ctx, t, env)
	})
	t.Run("retention boundary is strict", func(t *testing.T) { testRetentionBoundary(ctx, t, env) })
	t.Run("reconciliation keys are never purged", func(t *testing.T) { testReconciliationUntouched(ctx, t, env) })
	t.Run("one tick purges every tenant", func(t *testing.T) { testCrossTenant(ctx, t, env) })
	t.Run("contended tick skips without error", func(t *testing.T) { testLockContention(ctx, t, env) })
	t.Run("metric counts deleted rows per table", func(t *testing.T) { testDeleteMetric(ctx, t, env) })
}

// testExpiredCommandKeys is AC-3 for command_idempotency_keys: a key older than the
// ten-minute retention is deleted, a fresh one survives the same tick.
func testExpiredCommandKeys(ctx context.Context, t *testing.T, env *cleanupEnv) {
	clearIdempotency(ctx, t, env)

	// One minute past the window and one minute inside it: both margins are far larger
	// than the milliseconds between seeding and the purge, so the outcome is decided by
	// the retention boundary rather than by how long the test took to get here.
	seedCommandKey(ctx, t, env, env.tenantID, "expired", idempotencyRetention+time.Minute)
	seedCommandKey(ctx, t, env, env.tenantID, "fresh", idempotencyRetention-time.Minute)

	res := runCleanup(ctx, t, env, nil)
	assertDeleted(t, res, "command_idempotency_keys", 1)

	if keyExists(ctx, t, env, "command_idempotency_keys", env.tenantID, "expired") {
		t.Error("expired command key survived the cleanup; AC-3 requires it to be deleted")
	}
	if !keyExists(ctx, t, env, "command_idempotency_keys", env.tenantID, "fresh") {
		t.Error("fresh command key was deleted; the cleanup must only purge past the retention window")
	}
	t.Logf("command_idempotency_keys: deleted the %s-old key, retained the %s-old key",
		idempotencyRetention+time.Minute, idempotencyRetention-time.Minute)
}

// testExpiredDecisionKeys is AC-3 for decision_idempotency_keys. The decision table
// has no application grants yet (E4), but its retention is the same ten minutes and
// the same job enforces it, so it is covered now rather than when its writer lands.
func testExpiredDecisionKeys(ctx context.Context, t *testing.T, env *cleanupEnv) {
	clearIdempotency(ctx, t, env)

	seedDecisionKey(ctx, t, env, env.tenantID, "expired", idempotencyRetention+time.Minute)
	seedDecisionKey(ctx, t, env, env.tenantID, "fresh", idempotencyRetention-time.Minute)

	res := runCleanup(ctx, t, env, nil)
	assertDeleted(t, res, "decision_idempotency_keys", 1)

	if keyExists(ctx, t, env, "decision_idempotency_keys", env.tenantID, "expired") {
		t.Error("expired decision key survived the cleanup; AC-3 requires it to be deleted")
	}
	if !keyExists(ctx, t, env, "decision_idempotency_keys", env.tenantID, "fresh") {
		t.Error("fresh decision key was deleted; the cleanup must only purge past the retention window")
	}
	t.Logf("decision_idempotency_keys: deleted the %s-old key, retained the %s-old key",
		idempotencyRetention+time.Minute, idempotencyRetention-time.Minute)
}

// testRetentionBoundary pins the STRICT "<": a row sitting exactly on the retention
// edge SURVIVES. That is the side worth asserting, because it is the safe one — the
// key lives one tick too long (a redundant replay protection) instead of vanishing
// one instant too early (a lost one).
//
// Hitting the edge exactly needs a trick. The boundary is now() inside the purge
// transaction, and a row seeded from a DIFFERENT transaction is always slightly older
// than that by the time the purge starts, so "exactly on the edge" is unreachable
// through CleanupIdempotencyKeys. Here the row is instead seeded and purged inside
// ONE transaction: both now() calls are that transaction's timestamp, so created_at
// lands precisely on the boundary. The statement executed is the production constant,
// not a copy, so the assertion is about the shipped predicate.
func testRetentionBoundary(ctx context.Context, t *testing.T, env *cleanupEnv) {
	clearIdempotency(ctx, t, env)

	for _, table := range purgedTables {
		tx, err := env.super.Begin(ctx)
		if err != nil {
			t.Fatalf("begin boundary tx: %v", err)
		}
		// Always discarded: this transaction exists only to hold now() still.
		defer func() { _ = tx.Rollback(ctx) }()

		seconds := idempotencyRetention.Seconds()
		switch table.name {
		case "command_idempotency_keys":
			_, err = tx.Exec(ctx, `
				INSERT INTO command_idempotency_keys
				    (tenant_id, idempotency_key, request_hash, response_status, response_body, created_at)
				VALUES ($1, 'boundary', '\x00'::bytea, 200, '\x00'::bytea, now() - make_interval(secs => $2))`,
				env.tenantID, seconds)
		case "decision_idempotency_keys":
			_, err = tx.Exec(ctx, `
				INSERT INTO decision_idempotency_keys
				    (tenant_id, idempotency_key, decision, reason_code, response_body, created_at)
				VALUES ($1, 'boundary', 'grant', 'test', '\x00'::bytea, now() - make_interval(secs => $2))`,
				env.tenantID, seconds)
		default:
			t.Fatalf("unhandled purged table %q; add a boundary case for it", table.name)
		}
		if err != nil {
			t.Fatalf("seed boundary row in %s: %v", table.name, err)
		}

		tag, err := tx.Exec(ctx, table.deleteSQL, seconds)
		if err != nil {
			t.Fatalf("boundary delete on %s: %v", table.name, err)
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("%s: a row exactly on the retention boundary was deleted (%d rows); "+
				"the predicate must be strict \"<\", sparing the edge", table.name, tag.RowsAffected())
		}
		_ = tx.Rollback(ctx)
	}
	t.Logf("both purge statements spare a row whose created_at equals now() - %s exactly", idempotencyRetention)
}

// testReconciliationUntouched proves the job never purges
// reconciliation_idempotency_keys. Those keys protect events that are themselves
// never deleted, so purging them would let a reconnecting reader replay its buffer
// and double-apply an access event whose original is still in the store.
//
// The row is seeded far past the command/decision retention (a full day), so the
// assertion cannot pass merely because the row was young.
func testReconciliationUntouched(ctx context.Context, t *testing.T, env *cleanupEnv) {
	clearIdempotency(ctx, t, env)

	readerID := seedReconciliationKey(ctx, t, env, 24*time.Hour)

	if _, err := runCleanupErr(ctx, env, nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var n int
	if err := env.super.QueryRow(ctx,
		`SELECT count(*) FROM reconciliation_idempotency_keys WHERE reader_id = $1`, readerID,
	).Scan(&n); err != nil {
		t.Fatalf("count reconciliation keys: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciliation key count = %d, want 1; the cleanup must never purge this table", n)
	}
	t.Log("reconciliation_idempotency_keys: a 24h-old key is still present after the purge")
}

// testCrossTenant proves one tick purges every tenant's expired keys. The job runs on
// the bypass pool with no tenant bound, deliberately: on the RLS-bound pool the
// tenant_isolation policy would scope the DELETE to a single tenant, quietly turning
// a deployment-wide retention job into a per-tenant one.
func testCrossTenant(ctx context.Context, t *testing.T, env *cleanupEnv) {
	clearIdempotency(ctx, t, env)

	tenantA, tenantB := uuid.New(), uuid.New()
	seedCommandKey(ctx, t, env, tenantA, "expired-a", idempotencyRetention+time.Minute)
	seedCommandKey(ctx, t, env, tenantB, "expired-b", idempotencyRetention+time.Minute)

	res := runCleanup(ctx, t, env, nil)
	assertDeleted(t, res, "command_idempotency_keys", 2)

	for _, c := range []struct {
		tenant uuid.UUID
		key    string
	}{{tenantA, "expired-a"}, {tenantB, "expired-b"}} {
		if keyExists(ctx, t, env, "command_idempotency_keys", c.tenant, c.key) {
			t.Errorf("tenant %s key %q survived; one tick must purge every tenant", c.tenant, c.key)
		}
	}
	t.Log("one tick deleted the expired keys of two different tenants")
}

// testLockContention is the property that keeps a busy deployment from turning
// contention into failure: while another transaction holds
// job.cleanup_idempotency_keys, a tick returns NO error and deletes nothing.
//
// It then releases the lock and re-runs, so the skip is proven to be the lock's doing
// and not a broken predicate that would delete nothing either way.
func testLockContention(ctx context.Context, t *testing.T, env *cleanupEnv) {
	clearIdempotency(ctx, t, env)
	seedCommandKey(ctx, t, env, env.tenantID, "expired", idempotencyRetention+time.Minute)

	// Hold the job's lock in an unrelated transaction, standing in for another worker
	// instance mid-purge. The blocking acquire is safe here: nothing else holds it.
	holder, err := env.super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder tx: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisory.LockID(idempotencyLockName)); err != nil {
		t.Fatalf("hold %s: %v", idempotencyLockName, err)
	}

	res, err := runCleanupErr(ctx, env, nil)
	if err != nil {
		t.Fatalf("contended cleanup returned an error: %v; a skipped tick must not fail the job", err)
	}
	if !res.Skipped {
		t.Error("contended cleanup did not report Skipped; the advisory lock failed to exclude it")
	}
	if len(res.Tables) != 0 {
		t.Errorf("contended cleanup reported %d table results, want 0 (it did no work)", len(res.Tables))
	}
	if !keyExists(ctx, t, env, "command_idempotency_keys", env.tenantID, "expired") {
		t.Error("contended cleanup deleted rows; it must not run any DELETE without the lock")
	}
	t.Log("contended tick: err=nil, Skipped=true, 0 rows deleted")

	// Release and re-run: the same key is now purged, so the skip above was contention.
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release lock holder: %v", err)
	}
	res = runCleanup(ctx, t, env, nil)
	assertDeleted(t, res, "command_idempotency_keys", 1)
	if keyExists(ctx, t, env, "command_idempotency_keys", env.tenantID, "expired") {
		t.Error("key survived the uncontended re-run; the skip was not caused by the lock")
	}
	t.Log("uncontended re-run of the same tick deleted the key")
}

// testDeleteMetric asserts the counter reflects what was deleted, per table, on an
// isolated registry so the assertion is unaffected by any other test's registrations.
func testDeleteMetric(ctx context.Context, t *testing.T, env *cleanupEnv) {
	clearIdempotency(ctx, t, env)

	const (
		commandRows  = 2
		decisionRows = 3
	)
	for i := range commandRows {
		seedCommandKey(ctx, t, env, env.tenantID, fmt.Sprintf("cmd-%d", i), idempotencyRetention+time.Minute)
	}
	for i := range decisionRows {
		seedDecisionKey(ctx, t, env, env.tenantID, fmt.Sprintf("dec-%d", i), idempotencyRetention+time.Minute)
	}

	m, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	runCleanup(ctx, t, env, m)

	for _, c := range []struct {
		table string
		want  float64
	}{
		{"command_idempotency_keys", commandRows},
		{"decision_idempotency_keys", decisionRows},
	} {
		if got := testutil.ToFloat64(m.deleted.WithLabelValues(c.table)); got != c.want {
			t.Errorf("%s{table=%q} = %v, want %v", deletedMetricName, c.table, got, c.want)
		}
	}

	// A second tick finds nothing left. The counter must hold its value (a counter
	// never decreases) and the zero-delete tick must still leave the series present,
	// so a scrape can tell "nothing expired" apart from "the job stopped running".
	runCleanup(ctx, t, env, m)
	if got := testutil.ToFloat64(m.deleted.WithLabelValues("command_idempotency_keys")); got != commandRows {
		t.Errorf("%s after an empty tick = %v, want %v (unchanged)", deletedMetricName, got, float64(commandRows))
	}
	t.Logf("%s: command=%d decision=%d after the purge, unchanged by an empty tick",
		deletedMetricName, commandRows, decisionRows)
}

// --- fixtures and assertions ---

// runCleanup runs one iteration on the BYPASS pool and fails the test on error or on
// an unexpected skip (nothing else holds the lock in these tests).
func runCleanup(ctx context.Context, t *testing.T, env *cleanupEnv, m *Metrics) Result {
	t.Helper()
	res, err := runCleanupErr(ctx, env, m)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if res.Skipped {
		t.Fatal("cleanup skipped unexpectedly; no other transaction should hold the job lock here")
	}
	return res
}

// runCleanupErr is the raw call, for the cases that assert on the error or the skip.
func runCleanupErr(ctx context.Context, env *cleanupEnv, m *Metrics) (Result, error) {
	return CleanupIdempotencyKeys(ctx, env.bypass, m)
}

// assertDeleted checks the per-table row count the job reported.
func assertDeleted(t *testing.T, res Result, table string, want int64) {
	t.Helper()
	for _, r := range res.Tables {
		if r.Table == table {
			if r.Deleted != want {
				t.Errorf("%s: deleted %d rows, want %d", table, r.Deleted, want)
			}
			return
		}
	}
	t.Errorf("cleanup result has no entry for %s (got %+v)", table, res.Tables)
}

// setupCleanup starts one migrated container and opens both pools: the superuser
// (fixtures) and opengate_bypass (the job's production identity).
func setupCleanup(ctx context.Context, t *testing.T) *cleanupEnv {
	t.Helper()

	container := testsupport.StartPostgres(ctx, t)
	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	migrateUp(ctx, t, superDSN)

	super := openPool(ctx, t, superDSN)
	bypass := openPool(ctx, t, deriveBypassDSN(ctx, t, container))

	// One tenant for the reconciliation fixture, whose event row has a foreign key to
	// tenants. The idempotency tables themselves carry no such key, so the cross-tenant
	// test can use bare UUIDs.
	tenantID := uuid.New()
	if _, err := super.Exec(ctx,
		`INSERT INTO tenants (id, name, slug, status, session_timeout)
		 VALUES ($1, $2, $3, 'active', make_interval(mins => 60))`,
		tenantID, "Cleanup Test Tenant", "cleanup-test"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	return &cleanupEnv{super: super, bypass: bypass, tenantID: tenantID}
}

// migrateUp applies every embedded application migration via goose, mirroring the
// migrate subcommand (goose drives database/sql, not a pgx pool).
func migrateUp(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	defer func() { _ = db.Close() }()

	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatalf("new goose provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

// openPool opens a pgx pool against dsn and closes it at test end.
func openPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// deriveBypassDSN builds the opengate_bypass connection string from the container
// host/port and the well-known credentials create_app_roles installs (user
// opengate_bypass, password 'placeholder').
func deriveBypassDSN(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
	t.Helper()
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return fmt.Sprintf("postgres://opengate_bypass:placeholder@%s:%s/opengate_test?sslmode=disable",
		host, port.Port())
}

// clearIdempotency empties all three idempotency tables (and the events the
// reconciliation rows reference) so the subtests, which share one container, cannot
// see each other's rows. Order matters: the reconciliation FK points at events.
func clearIdempotency(ctx context.Context, t *testing.T, env *cleanupEnv) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM command_idempotency_keys`,
		`DELETE FROM decision_idempotency_keys`,
		`DELETE FROM reconciliation_idempotency_keys`,
		`DELETE FROM events`,
	} {
		if _, err := env.super.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// seedCommandKey inserts one command idempotency key aged by age. The age is measured
// from the DATABASE's clock (created_at = now() - age), so the fixture and the job's
// boundary are on the same clock and the test host's own time never enters into it.
func seedCommandKey(ctx context.Context, t *testing.T, env *cleanupEnv, tenantID uuid.UUID, key string, age time.Duration) {
	t.Helper()
	if _, err := env.super.Exec(ctx, `
		INSERT INTO command_idempotency_keys
		    (tenant_id, idempotency_key, request_hash, response_status, response_body, created_at)
		VALUES ($1, $2, '\x00'::bytea, 200, '\x7b7d'::bytea, now() - make_interval(secs => $3))`,
		tenantID, key, age.Seconds()); err != nil {
		t.Fatalf("seed command key %q: %v", key, err)
	}
}

// seedDecisionKey inserts one decision idempotency key aged by age, on the same
// database-clock basis as seedCommandKey.
func seedDecisionKey(ctx context.Context, t *testing.T, env *cleanupEnv, tenantID uuid.UUID, key string, age time.Duration) {
	t.Helper()
	if _, err := env.super.Exec(ctx, `
		INSERT INTO decision_idempotency_keys
		    (tenant_id, idempotency_key, decision, reason_code, response_body, created_at)
		VALUES ($1, $2, 'grant', 'policy_match', '\x7b7d'::bytea, now() - make_interval(secs => $3))`,
		tenantID, key, age.Seconds()); err != nil {
		t.Fatalf("seed decision key %q: %v", key, err)
	}
}

// seedReconciliationKey inserts one reconciliation idempotency key aged by age,
// together with the event its foreign key requires, and returns the reader id.
func seedReconciliationKey(ctx context.Context, t *testing.T, env *cleanupEnv, age time.Duration) uuid.UUID {
	t.Helper()
	eventID := uuid.New()
	if _, err := env.super.Exec(ctx, `
		INSERT INTO events
		    (id, tenant_id, aggregate_id, aggregate_type, sequence, stream_position,
		     event_type, payload, metadata, occurred_at)
		VALUES ($1, $2, $3, 'reader', 1, nextval('events_stream_position_seq'),
		        'reader.access.v1', '{}'::jsonb, '{"actor_type":"system"}'::jsonb, now())`,
		eventID, env.tenantID, uuid.New()); err != nil {
		t.Fatalf("seed event for reconciliation key: %v", err)
	}

	readerID := uuid.New()
	if _, err := env.super.Exec(ctx, `
		INSERT INTO reconciliation_idempotency_keys
		    (tenant_id, reader_id, sequence_no, event_id, created_at)
		VALUES ($1, $2, 1, $3, now() - make_interval(secs => $4))`,
		env.tenantID, readerID, eventID, age.Seconds()); err != nil {
		t.Fatalf("seed reconciliation key: %v", err)
	}
	return readerID
}

// keyExists reports whether (tenantID, key) is still present in table. The table name
// is interpolated from an in-test constant (never user input), which is why this is
// not a bound parameter — identifiers cannot be parameterized in SQL.
func keyExists(ctx context.Context, t *testing.T, env *cleanupEnv, table string, tenantID uuid.UUID, key string) bool {
	t.Helper()
	var n int
	query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1 AND idempotency_key = $2`, table)
	if err := env.super.QueryRow(ctx, query, tenantID, key).Scan(&n); err != nil {
		t.Fatalf("count %s rows for key %q: %v", table, key, err)
	}
	return n > 0
}
