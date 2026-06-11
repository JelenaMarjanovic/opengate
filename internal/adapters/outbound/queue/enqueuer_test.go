package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestJobEnqueuer exercises the tx-scoped River adapter end-to-end against a real
// Postgres whose `river` schema, tables, and opengate_app grants all come from
// Step 1's migrate path (queue.MigrateRiver) — so it also proves that path, not a
// hand-built schema. One container is shared; each subtest clears river_job first
// so its assertion stands alone.
//
// The four subtests map to the Step 2 plan: rollback→absent (outbox), commit→
// present (enqueue side), the trace-context round-trip through job metadata, and
// the empirical check that River preserves the {"trace": ...} envelope verbatim.
func TestJobEnqueuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	env := setupEnqueue(ctx, t)

	// Production's global W3C propagator (TraceContext + Baggage). Without it the
	// global propagator is a no-op and Inject would write nothing even with an
	// active span, so the round-trip subtest below depends on this being set.
	observability.SetGlobalTracePropagator()

	// rollback → absent (AC-2). Enqueue inside a tx, then roll it back: the outbox
	// guarantee means the job row must never have become visible. A successful
	// Enqueue here also proves opengate_app's Step 1 grants suffice for InsertTx.
	t.Run("rollback leaves no job (outbox)", func(t *testing.T) {
		resetJobs(ctx, t, env.superPool)

		tx, err := env.appPool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		// Safety net: release the tx's connection even if a later step calls
		// t.Fatalf (Goexit runs defers). A no-op after an explicit commit/rollback.
		defer func() { _ = tx.Rollback(ctx) }()
		if err := NewJobEnqueuer(tx, env.client).Enqueue(ctx, noopArgs{}); err != nil {
			t.Fatalf("enqueue (would also indicate insufficient opengate_app grants): %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		if n := countJobs(ctx, t, env.superPool, testJobKind); n != 0 {
			t.Errorf("after rollback: river_job rows for %q = %d, want 0", testJobKind, n)
		}
	})

	// commit → present (AC-1, enqueue side). The same flow, committed, leaves
	// exactly one job of the expected kind. Worker processing is Step 3.
	t.Run("commit persists exactly one job", func(t *testing.T) {
		resetJobs(ctx, t, env.superPool)

		tx, err := env.appPool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		// Safety net: release the tx's connection even if a later step calls
		// t.Fatalf (Goexit runs defers). A no-op after an explicit commit/rollback.
		defer func() { _ = tx.Rollback(ctx) }()
		if err := NewJobEnqueuer(tx, env.client).Enqueue(ctx, noopArgs{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		if n := countJobs(ctx, t, env.superPool, testJobKind); n != 1 {
			t.Fatalf("after commit: river_job rows for %q = %d, want 1", testJobKind, n)
		}
		t.Logf("opengate_app command-path grants (incl. column UPDATE(kind)) sufficed for InsertTx: one %q row committed", testJobKind)
	})

	// trace inject round-trip. A synthetic sampled SpanContext (no OTel SDK needed
	// for the inject side — the propagator serializes whatever SpanContext is in
	// ctx) must come back out of the persisted metadata identical, mirroring the
	// Step 3 work side: metadata.trace → MapCarrier → Extract → parent SpanContext.
	t.Run("trace context round-trips through job metadata", func(t *testing.T) {
		resetJobs(ctx, t, env.superPool)

		wantTrace, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
		if err != nil {
			t.Fatalf("trace id: %v", err)
		}
		wantSpan, err := trace.SpanIDFromHex("0123456789abcdef")
		if err != nil {
			t.Fatalf("span id: %v", err)
		}
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    wantTrace,
			SpanID:     wantSpan,
			TraceFlags: trace.FlagsSampled,
		})
		spanCtx := trace.ContextWithSpanContext(ctx, sc)

		tx, err := env.appPool.Begin(spanCtx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		// Safety net: release the tx's connection even if a later step calls
		// t.Fatalf (Goexit runs defers). A no-op after an explicit commit/rollback.
		defer func() { _ = tx.Rollback(ctx) }()
		if err := NewJobEnqueuer(tx, env.client).Enqueue(spanCtx, noopArgs{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(spanCtx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		carrier := readTraceCarrier(ctx, t, env.superPool, testJobKind)
		extracted := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
		got := trace.SpanContextFromContext(extracted)

		if got.TraceID() != wantTrace {
			t.Errorf("extracted TraceID = %s, want %s", got.TraceID(), wantTrace)
		}
		if got.SpanID() != wantSpan {
			t.Errorf("extracted SpanID = %s, want %s", got.SpanID(), wantSpan)
		}
	})

	// metadata coexistence (empirical). Confirm River persisted the {"trace": ...}
	// envelope rather than clobbering or transforming it — the shape Step 3's
	// extract depends on. The raw metadata is logged so the persisted contract is
	// visible in test output.
	t.Run("River preserves the trace metadata key", func(t *testing.T) {
		resetJobs(ctx, t, env.superPool)

		tx, err := env.appPool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		// Safety net: release the tx's connection even if a later step calls
		// t.Fatalf (Goexit runs defers). A no-op after an explicit commit/rollback.
		defer func() { _ = tx.Rollback(ctx) }()
		if err := NewJobEnqueuer(tx, env.client).Enqueue(ctx, noopArgs{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		raw := readJobMetadata(ctx, t, env.superPool, testJobKind)
		t.Logf("persisted river_job.metadata for %q: %s", testJobKind, raw)

		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			t.Fatalf("metadata is not a JSON object: %v (raw=%s)", err, raw)
		}
		if _, ok := top[metadataTraceKey]; !ok {
			t.Errorf("River did not preserve the %q key; metadata = %s", metadataTraceKey, raw)
		}
	})
}

// enqueueEnv bundles the shared container-backed fixtures the JobEnqueuer
// subtests run against: the RLS-bound opengate_app pool (the command-path role
// whose Step 1 grants must suffice for InsertTx), a superuser pool for migrate +
// out-of-band reads/cleanup, and the Step 1 RoleAPI insert-only client.
type enqueueEnv struct {
	appPool   *pgxpool.Pool
	superPool *pgxpool.Pool
	client    *river.Client[pgx.Tx]
}

// setupEnqueue starts one migrated Postgres (goose up, then the River schema,
// tables, and grants via MigrateRiver) and returns the fixtures the subtests
// share.
func setupEnqueue(ctx context.Context, t *testing.T) *enqueueEnv {
	t.Helper()

	container := testsupport.StartPostgres(ctx, t)
	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("super connection string: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Phase 1: application (goose) migrations create the app roles incl.
	// opengate_app. Phases 2-4: the River schema, tables, and grants.
	applyGooseMigrations(ctx, t, superDSN)
	superPool := openPool(ctx, t, superDSN)
	if err := MigrateRiver(ctx, superPool, logger); err != nil {
		t.Fatalf("MigrateRiver: %v", err)
	}

	// The command path runs as opengate_app (RLS-bound). river_job carries no RLS
	// policy, so no tenant binding is needed here; what matters is that the insert
	// runs under opengate_app's identity, exercising exactly the Step 1 grants.
	appPool := openPool(ctx, t, deriveAppDSN(ctx, t, container))

	// Step 1's insert-only RoleAPI client, built over the app pool's driver. Its
	// own pool is nominal — InsertTx runs on the tx passed per call.
	client, err := newRiverClient(RoleAPI, appPool, logger)
	if err != nil {
		t.Fatalf("newRiverClient(RoleAPI): %v", err)
	}

	return &enqueueEnv{appPool: appPool, superPool: superPool, client: client}
}

// openPool opens a pgx pool for dsn, registered for cleanup.
func openPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applyGooseMigrations runs every embedded application migration up against dsn
// (migrate phase 1), mirroring the migrate subcommand. It opens its own
// database/sql handle because goose drives sql.DB, not a pgx pool.
func applyGooseMigrations(ctx context.Context, t *testing.T, dsn string) {
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
		t.Fatalf("new provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

// deriveAppDSN builds the opengate_app connection string from the container
// host/port and the well-known credentials create_app_roles installs (user
// opengate_app, password 'placeholder').
func deriveAppDSN(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
	t.Helper()
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return fmt.Sprintf("postgres://opengate_app:placeholder@%s:%s/opengate_test?sslmode=disable",
		host, port.Port())
}

// resetJobs deletes all rows from river.river_job so each subtest asserts against
// a clean table. Run on the superuser pool, which owns the schema.
func resetJobs(ctx context.Context, t *testing.T, super *pgxpool.Pool) {
	t.Helper()
	if _, err := super.Exec(ctx, "DELETE FROM river.river_job"); err != nil {
		t.Fatalf("reset river_job: %v", err)
	}
}

// countJobs returns the number of river_job rows of the given kind, read on a
// connection separate from the adapter's tx (the superuser pool).
func countJobs(ctx context.Context, t *testing.T, super *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := super.QueryRow(ctx,
		"SELECT count(*) FROM river.river_job WHERE kind = $1", kind,
	).Scan(&n); err != nil {
		t.Fatalf("count river_job (%s): %v", kind, err)
	}
	return n
}

// readJobMetadata returns the raw metadata jsonb of the single job of kind.
func readJobMetadata(ctx context.Context, t *testing.T, super *pgxpool.Pool, kind string) []byte {
	t.Helper()
	var raw []byte
	if err := super.QueryRow(ctx,
		"SELECT metadata FROM river.river_job WHERE kind = $1", kind,
	).Scan(&raw); err != nil {
		t.Fatalf("read river_job metadata (%s): %v", kind, err)
	}
	return raw
}

// readTraceCarrier reads the job's metadata and reconstructs the W3C carrier
// stored under metadataTraceKey — the exact shape Step 3's worker will extract.
func readTraceCarrier(ctx context.Context, t *testing.T, super *pgxpool.Pool, kind string) propagation.MapCarrier {
	t.Helper()
	raw := readJobMetadata(ctx, t, super, kind)
	var envelope map[string]propagation.MapCarrier
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal metadata envelope: %v (raw=%s)", err, raw)
	}
	carrier, ok := envelope[metadataTraceKey]
	if !ok {
		t.Fatalf("metadata has no %q key: %s", metadataTraceKey, raw)
	}
	return carrier
}
