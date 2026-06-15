package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// All projection tests use a real Postgres container: the Option 3b snapshot
// boundary is an MVCC property (in-flight vs committed transactions), which an
// in-memory double cannot model. The runner is exercised directly (Run /
// applyOnce), never through River's periodic scheduler, because async scheduling is
// flaky to assert (the US-03.04 AC-3 lesson).

// projEnv is the shared fixture: one migrated container, a superuser pool that
// stands in for the production BYPASSRLS pool (the superuser bypasses RLS and may
// read every tenant's events and write projection_progress), and one seeded tenant
// the events foreign key resolves against. Subtests isolate themselves with unique
// projector names and aggregate ids, so they share this fixture safely.
type projEnv struct {
	pool     *pgxpool.Pool
	tenantID uuid.UUID
}

// TestProjectionFramework drives the projector framework's four acceptance
// scenarios against one shared container.
func TestProjectionFramework(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	env := setupProjection(ctx, t)

	t.Run("AC-1 singleton mutual exclusion", func(t *testing.T) { testSingleton(ctx, t, env) })
	t.Run("AC-2 crash reprocess is idempotent", func(t *testing.T) { testCrashReprocess(ctx, t, env) })
	t.Run("AC-3 lag metric", func(t *testing.T) { testLagMetric(ctx, t, env) })
	t.Run("3b out-of-order-commit boundary", func(t *testing.T) { testBoundary(ctx, t, env) })
}

// testSingleton (AC-1) launches two concurrent Run calls for the same projector and
// proves the advisory lock makes exactly one apply the batch while the other skips.
// Determinism comes from the projector signaling and then blocking inside Apply, so
// the second Run is guaranteed to try the lock while the first holds it.
func testSingleton(ctx context.Context, t *testing.T, env *projEnv) {
	const name = "test_singleton"
	clearEvents(ctx, t, env.pool)
	registerProgressRow(ctx, t, env.pool, name)
	aggID := uuid.New()
	seedEvent(ctx, t, env, aggID, 0, "test.created.v1", time.Now().UTC())

	p := &testProjector{
		name:     name,
		applying: make(chan int, 1),
		proceed:  make(chan struct{}),
	}

	g1Done := make(chan error, 1)
	go func() { g1Done <- Run(ctx, env.pool, p, nil) }()

	// Wait until g1 holds the lock and is inside Apply (signaled from within Apply).
	select {
	case <-p.applying:
	case err := <-g1Done:
		t.Fatalf("g1 Run returned before applying: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("g1 did not acquire the lock and enter Apply in time")
	}

	// g2 runs while g1 holds the lock: it must skip (TryWithLock => acquired=false)
	// and return promptly without ever entering Apply.
	g2Done := make(chan error, 1)
	go func() { g2Done <- Run(ctx, env.pool, p, nil) }()
	select {
	case err := <-g2Done:
		if err != nil {
			t.Fatalf("g2 Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("g2 Run did not return; a contended TryWithLock must skip immediately, not block")
	}
	// g2 must not have entered Apply (no second signal pending).
	select {
	case <-p.applying:
		t.Fatal("g2 entered Apply; the singleton lock failed to exclude the second runner")
	default:
	}

	// Release g1; it finishes Apply and commits.
	close(p.proceed)
	if err := <-g1Done; err != nil {
		t.Fatalf("g1 Run: %v", err)
	}

	if got := p.applyCount.Load(); got != 1 {
		t.Fatalf("Apply ran %d times, want exactly 1 (singleton)", got)
	}
	// Single application: one row, event_count=1 (no double count).
	assertViewRow(ctx, t, env.pool, aggID, 1, "test.created.v1")
}

// testCrashReprocess (AC-2) rolls back a fully-applied iteration to simulate a crash
// between consuming and committing, then proves a normal Run reprocesses the same
// event and the idempotent (absolute, not incremental) upsert yields exactly one
// correct row — no double count.
func testCrashReprocess(ctx context.Context, t *testing.T, env *projEnv) {
	const name = "test_reprocess"
	clearEvents(ctx, t, env.pool)
	registerProgressRow(ctx, t, env.pool, name)
	aggID := uuid.New()
	seedEvent(ctx, t, env, aggID, 0, "test.created.v1", time.Now().UTC())

	p := &testProjector{name: name}

	// Crash path: replay the runner's inside-lock body (read + Apply + advance) in a
	// transaction, then ROLL BACK instead of commit — the process "dies" here.
	func() {
		tx, err := env.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin crash tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := applyOnce(ctx, tx, p); err != nil {
			t.Fatalf("applyOnce (crash path): %v", err)
		}
		// No commit: the rollback in the deferred call discards the work.
	}()

	// The rollback undid both the read-model write and the watermark advance.
	assertNoViewRow(ctx, t, env.pool, aggID)
	if xid := readWatermark(ctx, t, env.pool, name); xid != "0" {
		t.Fatalf("last_consumed_xid = %q after a rolled-back crash, want \"0\" (unchanged)", xid)
	}
	if got := p.applyCount.Load(); got != 1 {
		t.Fatalf("Apply ran %d times on the crash path, want 1", got)
	}

	// Recovery: a normal Run must REPROCESS the same event (watermark never advanced).
	if err := Run(ctx, env.pool, p, nil); err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	// Idempotent: exactly one row, event_count still 1 (an unconditional increment
	// would have produced 2 here — the bug this scenario exists to catch).
	assertViewRow(ctx, t, env.pool, aggID, 1, "test.created.v1")
	if xid := readWatermark(ctx, t, env.pool, name); xid == "0" {
		t.Fatal("last_consumed_xid still \"0\" after recovery Run; the watermark did not advance")
	}
	if got := p.applyCount.Load(); got != 2 {
		t.Fatalf("Apply ran %d times total, want 2 (crash + recovery)", got)
	}
}

// testLagMetric (AC-3) seeds an event five seconds in the past, runs the projector,
// and asserts the lag gauge reads ~5s.
func testLagMetric(ctx context.Context, t *testing.T, env *projEnv) {
	const name = "test_lag"
	clearEvents(ctx, t, env.pool)
	registerProgressRow(ctx, t, env.pool, name)
	aggID := uuid.New()
	occurred := time.Now().UTC().Add(-5 * time.Second)
	seedEvent(ctx, t, env, aggID, 0, "test.created.v1", occurred)

	m, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	p := &testProjector{name: name}
	if err := Run(ctx, env.pool, p, m); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lag := testutil.ToFloat64(m.lag.WithLabelValues(name))
	// Band centered on 5s: the floor rules out a 0/unset gauge, the ceiling tolerates
	// the small seed->record delay (and slow CI) without admitting a wrong value.
	if lag < 4.5 || lag > 8.0 {
		t.Fatalf("lag gauge = %.2fs, want ~5s (event occurred 5s before the run)", lag)
	}
}

// testBoundary is the Option 3b correctness showcase, fully deterministic (no timing
// reliance): a committed event whose inserting transaction sorts AFTER an
// still-in-flight one must NOT be consumed until the older transaction commits, so no
// event is ever skipped. A naive stream_position watermark would consume the later
// event and then lose the earlier one.
func testBoundary(ctx context.Context, t *testing.T, env *projEnv) {
	const name = "test_boundary"
	clearEvents(ctx, t, env.pool)
	registerProgressRow(ctx, t, env.pool, name)
	aggX := uuid.New()
	aggY := uuid.New()
	p := &testProjector{name: name}

	// A: append X within txA and HOLD txA open, keeping its xid in flight.
	txA, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin txA: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }() // safety net; A is committed on the happy path
	if err := postgres.NewEventStore(txA).Append(ctx, aggX, 0,
		[]events.Event{newTestEvent(env.tenantID, "test.x.v1", time.Now().UTC())}); err != nil {
		t.Fatalf("append X in txA: %v", err)
	}

	// B: append Y within txB (its xid is greater than A's, since B's first write
	// follows A's) and COMMIT B.
	txB, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin txB: %v", err)
	}
	if err := postgres.NewEventStore(txB).Append(ctx, aggY, 0,
		[]events.Event{newTestEvent(env.tenantID, "test.y.v1", time.Now().UTC())}); err != nil {
		_ = txB.Rollback(ctx)
		t.Fatalf("append Y in txB: %v", err)
	}
	if err := txB.Commit(ctx); err != nil {
		t.Fatalf("commit txB: %v", err)
	}

	// Run while A is in flight: the boundary insert_xid < pg_snapshot_xmin(...)
	// excludes Y because A (a smaller xid) is still running, even though Y committed.
	// Nothing is consumed.
	if err := Run(ctx, env.pool, p, nil); err != nil {
		t.Fatalf("Run (A in flight): %v", err)
	}
	assertNoViewRow(ctx, t, env.pool, aggX)
	assertNoViewRow(ctx, t, env.pool, aggY)
	if xid := readWatermark(ctx, t, env.pool, name); xid != "0" {
		t.Fatalf("watermark = %q while A in flight, want \"0\" (nothing consumed)", xid)
	}
	if got := p.applyCount.Load(); got != 0 {
		t.Fatalf("Apply ran %d times while A in flight, want 0", got)
	}

	// Commit A: now no transaction with an xid <= Y's is in flight.
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit txA: %v", err)
	}

	// Run again: X and Y are both below xmin now, so both are consumed in insert_xid
	// order (X before Y) — no event lost.
	if err := Run(ctx, env.pool, p, nil); err != nil {
		t.Fatalf("Run (A committed): %v", err)
	}
	assertViewRow(ctx, t, env.pool, aggX, 1, "test.x.v1")
	assertViewRow(ctx, t, env.pool, aggY, 1, "test.y.v1")
	if xid := readWatermark(ctx, t, env.pool, name); xid == "0" {
		t.Fatal("watermark still \"0\" after consuming X and Y")
	}
}

// testProjector is the test read-model projector. Its Apply performs an idempotent,
// ABSOLUTE upsert per aggregate (event_count = the aggregate's latest sequence
// number, never event_count + N), so a replayed batch produces the same end state.
// The applying/proceed channels let the singleton test (AC-1) pin the moment the
// lock is held; they are nil in the other tests, where Apply neither signals nor
// blocks.
type testProjector struct {
	name       string
	applyCount atomic.Int32
	applying   chan int      // receives the batch size when Apply is entered (nil => no signal)
	proceed    chan struct{} // Apply blocks until this is closed (nil => no block)
}

// Name identifies the projector (advisory-lock suffix and projection_progress row).
func (p *testProjector) Name() string { return p.name }

// Apply folds the batch into test_projection_view with an idempotent absolute upsert.
func (p *testProjector) Apply(ctx context.Context, tx pgx.Tx, evts []events.Event) error {
	p.applyCount.Add(1)
	if err := p.upsert(ctx, tx, evts); err != nil {
		return err
	}
	if p.applying != nil {
		p.applying <- len(evts)
	}
	if p.proceed != nil {
		<-p.proceed
	}
	return nil
}

// upsert reduces the batch to absolute per-aggregate state (latest sequence number
// and its event type) and writes it. Keyed by aggregate_id with an EXCLUDED-based
// DO UPDATE, it is idempotent: replaying the same events sets the same values.
func (p *testProjector) upsert(ctx context.Context, tx pgx.Tx, evts []events.Event) error {
	type state struct {
		count    int64
		lastType string
	}
	latest := make(map[uuid.UUID]state)
	for i := range evts {
		e := &evts[i]
		if cur, ok := latest[e.AggregateID]; !ok || e.Sequence >= cur.count {
			latest[e.AggregateID] = state{count: e.Sequence, lastType: e.Type}
		}
	}
	for aggID, s := range latest {
		if _, err := tx.Exec(ctx, `
			INSERT INTO test_projection_view (aggregate_id, event_count, last_event_type)
			VALUES ($1, $2, $3)
			ON CONFLICT (aggregate_id) DO UPDATE
			SET event_count     = EXCLUDED.event_count,
			    last_event_type = EXCLUDED.last_event_type`,
			aggID, s.count, s.lastType); err != nil {
			return err
		}
	}
	return nil
}

// --- fixtures and assertions ---

// setupProjection starts one migrated container, opens the superuser (bypass-stand-in)
// pool, creates the test read model, and seeds the tenant the events FK needs.
func setupProjection(ctx context.Context, t *testing.T) *projEnv {
	t.Helper()

	container := testsupport.StartPostgres(ctx, t)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	migrateUp(ctx, t, dsn)

	// A generous pool: the boundary test holds two transactions open while a third
	// (the runner's) begins, so the pool must not starve. MaxConns is set on the
	// config rather than via a DSN param, because pool_max_conns is a pgxpool-only
	// setting that the database/sql handle goose uses would reject as a server param.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// The test read model lives only in the test (not a production migration), with no
	// RLS — projectors write read models cross-tenant.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE test_projection_view (
			aggregate_id    uuid PRIMARY KEY,
			event_count     int  NOT NULL,
			last_event_type text NOT NULL
		)`); err != nil {
		t.Fatalf("create test_projection_view: %v", err)
	}

	tenantID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, slug, status, session_timeout)
		 VALUES ($1, $2, $3, 'active', make_interval(mins => 60))`,
		tenantID, "Projection Test Tenant", "projection-test"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	return &projEnv{pool: pool, tenantID: tenantID}
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

// clearEvents empties the shared events table. The subtests share one container for
// speed, but each projector starts at watermark 0 and reads the WHOLE log, so without
// this a later subtest's projector would also consume earlier subtests' committed
// events — inflating the lag sample (AC-3) and advancing the boundary projector's
// watermark off leftover events. Clearing at each subtest's start isolates them at
// the event-log level. No table has a foreign key to events, so a plain DELETE is safe.
func clearEvents(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM events`); err != nil {
		t.Fatalf("clear events: %v", err)
	}
}

// registerProgressRow inserts a projection_progress row for a test projector so its
// boundary read has a watermark row to join and its advance has a row to update.
func registerProgressRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO projection_progress (projector_name) VALUES ($1)`, name); err != nil {
		t.Fatalf("register projection_progress row %q: %v", name, err)
	}
}

// newTestEvent builds a minimal valid domain event for a tenant. Append assigns the
// aggregate_id (its method parameter) and the sequence, so those are left zero here.
func newTestEvent(tenantID uuid.UUID, eventType string, occurredAt time.Time) events.Event {
	return events.Event{
		ID:            uuid.New(),
		TenantID:      tenantID,
		AggregateType: "test_aggregate",
		Type:          eventType,
		Payload:       json.RawMessage(`{}`),
		Metadata:      events.EventMetadata{ActorType: "system"},
		OccurredAt:    occurredAt,
	}
}

// seedEvent appends one committed event on aggregateID (sequence = seq+1) in its own
// transaction, so its inserting transaction completes and the boundary admits it.
func seedEvent(ctx context.Context, t *testing.T, env *projEnv, aggregateID uuid.UUID, seq int64, eventType string, occurredAt time.Time) {
	t.Helper()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := postgres.NewEventStore(tx).Append(ctx, aggregateID, seq,
		[]events.Event{newTestEvent(env.tenantID, eventType, occurredAt)}); err != nil {
		t.Fatalf("append seed event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed event: %v", err)
	}
}

// readWatermark returns last_consumed_xid as its opaque text token ("0" before any
// advance), so the tests can assert it changed (or did not) without an xid8 type.
func readWatermark(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var xid string
	if err := pool.QueryRow(ctx,
		`SELECT last_consumed_xid::text FROM projection_progress WHERE projector_name = $1`, name,
	).Scan(&xid); err != nil {
		t.Fatalf("read watermark %q: %v", name, err)
	}
	return xid
}

// assertViewRow asserts the read-model row for aggID has the expected absolute count
// and latest event type.
func assertViewRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, aggID uuid.UUID, wantCount int, wantType string) {
	t.Helper()
	var (
		count     int
		eventType string
	)
	if err := pool.QueryRow(ctx,
		`SELECT event_count, last_event_type FROM test_projection_view WHERE aggregate_id = $1`, aggID,
	).Scan(&count, &eventType); err != nil {
		t.Fatalf("read view row for %s: %v", aggID, err)
	}
	if count != wantCount {
		t.Errorf("event_count = %d, want %d", count, wantCount)
	}
	if eventType != wantType {
		t.Errorf("last_event_type = %q, want %q", eventType, wantType)
	}
}

// assertNoViewRow asserts the read model has no row for aggID.
func assertNoViewRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, aggID uuid.UUID) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM test_projection_view WHERE aggregate_id = $1`, aggID,
	).Scan(&n); err != nil {
		t.Fatalf("count view rows for %s: %v", aggID, err)
	}
	if n != 0 {
		t.Fatalf("read model has %d rows for %s, want 0", n, aggID)
	}
}
