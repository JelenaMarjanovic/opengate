package queue

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestWorkerProcessesEnqueuedJob is the AC-1 verification: a job enqueued through
// the real command path (as opengate_app, inside a transaction) is fetched,
// worked, and completed by the worker pool (as opengate_bypass) — exercising the
// full enqueue->process path end to end on the Steps 1-2 grants alone, with no
// grant additions.
//
// The pieces are deliberately the production ones:
//   - enqueue: the Step 2 JobEnqueuer on the RLS-bound opengate_app pool;
//   - process: the worker-role client on the BYPASSRLS opengate_bypass pool, with
//     the trace-extract middleware installed (decision B3);
//
// only the test.noop worker is registered (production registers none).
func TestWorkerProcessesEnqueuedJob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	env := setupWorker(ctx, t)

	// The global propagator is part of production wiring; install it so enqueue's
	// Inject and the middleware's Extract behave as in production.
	observability.SetGlobalTracePropagator()

	resetJobs(ctx, t, env.superPool)

	// Subscribe to completion BEFORE Start so no event is missed, then boot the pool.
	completed, cancelSub := env.workerClient.Subscribe(river.EventKindJobCompleted)
	defer cancelSub()
	if err := env.workerClient.Start(ctx); err != nil {
		t.Fatalf("worker Start: %v", err)
	}
	// Safety net: if an assertion fails before the explicit drain, still stop the
	// pool so the test does not leak its goroutines. A second Stop is a no-op.
	defer func() { _ = env.workerClient.Stop(context.Background()) }()

	// Enqueue a test.noop job through the REAL command path: a tx on the opengate_app
	// pool, the Step 2 enqueuer using the RoleAPI client, committed so the outbox
	// makes it visible to the worker.
	tx, err := env.appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := NewJobEnqueuer(tx, env.apiClient).Enqueue(ctx, noopArgs{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Wait for completion. Both signals must arrive: the worker actually ran
	// (env.worker.worked) and River emitted the completion event. A generous timeout
	// guards against a wedged pool without making the happy path slow.
	const waitFor = 15 * time.Second
	select {
	case <-env.worker.worked:
	case <-time.After(waitFor):
		t.Fatalf("worker did not run the job within %s", waitFor)
	}
	select {
	case ev := <-completed:
		if ev.Job.Kind != testJobKind {
			t.Fatalf("completed job kind = %q, want %q", ev.Job.Kind, testJobKind)
		}
	case <-time.After(waitFor):
		t.Fatalf("no job-completed event within %s", waitFor)
	}

	// Assert the durable state: the row reached 'completed' in river_job (read
	// out-of-band on the superuser pool).
	if state := jobState(ctx, t, env.superPool, testJobKind); state != "completed" {
		t.Fatalf("river_job.state = %q, want \"completed\"", state)
	}
	t.Logf("full enqueue(opengate_app)->process(opengate_bypass) path completed a %q job on Steps 1-2 grants, no additions", testJobKind)

	// Graceful drain (decision A3, happy path): Stop within the budget returns
	// cleanly with no in-flight work left.
	stopCtx, cancel := context.WithTimeout(ctx, workerDrainTimeout)
	defer cancel()
	if err := env.workerClient.Stop(stopCtx); err != nil {
		t.Fatalf("graceful Stop: %v", err)
	}
}

// workerEnv bundles the container-backed fixtures the AC-1 test needs: the
// opengate_app pool (enqueue side), the opengate_bypass pool (process side), a
// superuser pool for migrate + out-of-band reads, the RoleAPI insert client, and
// the RoleWorker client with the test.noop worker registered.
type workerEnv struct {
	appPool      *pgxpool.Pool
	superPool    *pgxpool.Pool
	apiClient    *river.Client[pgx.Tx]
	workerClient *river.Client[pgx.Tx]
	worker       *noopWorker
}

// setupWorker starts one migrated Postgres (goose, then the River schema/tables/
// grants via MigrateRiver) and wires both ends of the queue: the insert-only
// RoleAPI client on the app pool and a RoleWorker client on the bypass pool with
// the test.noop worker registered and the trace middleware installed.
func setupWorker(ctx context.Context, t *testing.T) *workerEnv {
	t.Helper()

	container := testsupport.StartPostgres(ctx, t)
	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("super connection string: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	applyGooseMigrations(ctx, t, superDSN)
	superPool := openPool(ctx, t, superDSN)
	if err := MigrateRiver(ctx, superPool, logger); err != nil {
		t.Fatalf("MigrateRiver: %v", err)
	}

	appPool := openPool(ctx, t, deriveAppDSN(ctx, t, container))
	bypassPool := openPool(ctx, t, deriveBypassDSN(ctx, t, container))

	apiClient, err := newRiverClient(RoleAPI, appPool, logger, nil)
	if err != nil {
		t.Fatalf("newRiverClient(RoleAPI): %v", err)
	}

	// Worker client: default queue, the test.noop worker registered (production
	// registers none), the global trace middleware. This is the only place the noop
	// worker is wired.
	worker := &noopWorker{worked: make(chan struct{}, 1)}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	workerClient, err := newRiverClient(RoleWorker, bypassPool, logger, &workerConfig{
		queues:          map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		workers:         workers,
		middleware:      []rivertype.Middleware{&traceMiddleware{}},
		softStopTimeout: workerSoftStopTimeout,
	})
	if err != nil {
		t.Fatalf("newRiverClient(RoleWorker): %v", err)
	}

	return &workerEnv{
		appPool:      appPool,
		superPool:    superPool,
		apiClient:    apiClient,
		workerClient: workerClient,
		worker:       worker,
	}
}

// deriveBypassDSN builds the opengate_bypass connection string from the container
// host/port and the well-known credentials create_app_roles installs (user
// opengate_bypass, password 'placeholder'). It mirrors deriveAppDSN; the worker
// processes jobs under this BYPASSRLS identity.
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

// jobState reads the state of the single job of kind, out-of-band on the superuser
// pool, so the assertion observes the durable row rather than an in-memory event.
func jobState(ctx context.Context, t *testing.T, super *pgxpool.Pool, kind string) string {
	t.Helper()
	var state string
	if err := super.QueryRow(ctx,
		"SELECT state FROM river.river_job WHERE kind = $1", kind,
	).Scan(&state); err != nil {
		t.Fatalf("read river_job state (%s): %v", kind, err)
	}
	return state
}
