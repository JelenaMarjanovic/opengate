package queue

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"

	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestCleanupIdempotencyKeysRegistration pins the scheduling contract of the
// retention job: the kind, the five-minute interval, the dedicated maintenance
// queue, and that queue's presence (with its concurrency) in the worker config.
//
// It asserts on the pieces the registration is BUILT from, because River exposes no
// way to read a *PeriodicJob's schedule or constructor back — its fields are
// unexported. TestWorkerPoolRunsCleanupJob closes that gap from the other side by
// observing the job River actually inserts.
//
// No container: newWorkerConfig only assembles a struct, and the pool it stores is
// never dereferenced during construction.
func TestCleanupIdempotencyKeysRegistration(t *testing.T) {
	// The interval is the System Design §4 / Database Schema §15 figure. Paired with
	// the ten-minute retention it bounds a key's post-expiry life at one interval.
	if cleanupIdempotencyKeysInterval != 5*time.Minute {
		t.Errorf("cleanupIdempotencyKeysInterval = %s, want 5m0s", cleanupIdempotencyKeysInterval)
	}
	// The schedule as River will evaluate it: the next fire is one interval out.
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if next := river.PeriodicInterval(cleanupIdempotencyKeysInterval).Next(base); !next.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("periodic schedule Next(%s) = %s, want %s", base, next, base.Add(5*time.Minute))
	}

	// The insert constructor: the System Design §6 kind, onto the maintenance queue.
	args, opts := cleanupIdempotencyKeysJobConstructor()
	if got := args.Kind(); got != "cleanup.idempotency_keys" {
		t.Errorf("job kind = %q, want %q", got, "cleanup.idempotency_keys")
	}
	if opts == nil || opts.Queue != maintenanceQueue {
		t.Errorf("insert opts queue = %+v, want %q", opts, maintenanceQueue)
	}
	if maintenanceQueue != "maintenance" {
		t.Errorf("maintenanceQueue = %q, want %q", maintenanceQueue, "maintenance")
	}

	// The production worker config. The pool is never dereferenced at construction.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wc, err := newWorkerConfig(&pgxpool.Pool{}, logger, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("newWorkerConfig: %v", err)
	}

	// The maintenance queue exists and is single-worker: every job on it is a
	// deployment-wide singleton, so more workers would only fetch ticks that skip.
	maintQueue, ok := wc.queues[maintenanceQueue]
	if !ok {
		t.Fatalf("worker config has no %q queue; the periodic job would insert onto a queue nobody polls", maintenanceQueue)
	}
	if maintQueue.MaxWorkers != 1 {
		t.Errorf("%q queue MaxWorkers = %d, want 1", maintenanceQueue, maintQueue.MaxWorkers)
	}
	// The queues it must not have displaced.
	for _, q := range []string{river.QueueDefault, projectionQueue} {
		if _, ok := wc.queues[q]; !ok {
			t.Errorf("worker config lost the %q queue", q)
		}
	}

	// Exactly one periodic job: the retention tick. No projector is registered yet, so
	// projectorPeriodicJobs contributes none — if that ever changes, this count is the
	// reminder to update it deliberately.
	if len(wc.periodicJobs) != 1 {
		t.Errorf("worker config has %d periodic jobs, want 1 (the retention tick only)", len(wc.periodicJobs))
	}
}

// TestWorkerPoolRunsCleanupJob is the end-to-end registration proof, on the
// PRODUCTION pool constructor: starting the worker inserts the RunOnStart retention
// tick, River routes it to the maintenance queue, the registered worker picks it up,
// and the purge actually deletes an expired key — as opengate_bypass, on the shipped
// grants.
//
// This is what a unit test of the registration cannot show. Every link it exercises
// (kind -> queue -> registered worker -> DELETE privilege) is one that a config-only
// assertion would pass while production silently did nothing.
func TestWorkerPoolRunsCleanupJob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
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

	// An expired key, seeded on the superuser pool (opengate_bypass holds no INSERT).
	tenantID := uuid.New()
	if _, err := superPool.Exec(ctx, `
		INSERT INTO command_idempotency_keys
		    (tenant_id, idempotency_key, request_hash, response_status, response_body, created_at)
		VALUES ($1, 'expired', '\x00'::bytea, 200, '\x7b7d'::bytea, now() - interval '11 minutes')`,
		tenantID); err != nil {
		t.Fatalf("seed expired key: %v", err)
	}

	bypassPool := openPool(ctx, t, deriveBypassDSN(ctx, t, container))
	pool, err := NewWorkerPool(bypassPool, logger)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}

	// Subscribe BEFORE Start so the RunOnStart tick's completion cannot be missed.
	completed, cancelSub := pool.client.Subscribe(river.EventKindJobCompleted)
	defer cancelSub()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Safety net if an assertion fails before the explicit drain; a second Stop is a no-op.
	defer pool.Stop()

	// Wait for the retention tick to complete. The pool has no other periodic job, so
	// any completion is this one; the kind is asserted regardless.
	const waitFor = 30 * time.Second
	select {
	case ev := <-completed:
		if ev.Job.Kind != cleanupIdempotencyKeysJobKind {
			t.Fatalf("completed job kind = %q, want %q", ev.Job.Kind, cleanupIdempotencyKeysJobKind)
		}
		if ev.Job.Queue != maintenanceQueue {
			t.Fatalf("completed job queue = %q, want %q", ev.Job.Queue, maintenanceQueue)
		}
	case <-time.After(waitFor):
		t.Fatalf("no %s job completed within %s; the RunOnStart tick did not reach a worker",
			cleanupIdempotencyKeysJobKind, waitFor)
	}

	// The durable job row, read out of band: state and queue as River persisted them.
	var state, queue string
	if err := superPool.QueryRow(ctx,
		`SELECT state, queue FROM river.river_job WHERE kind = $1`, cleanupIdempotencyKeysJobKind,
	).Scan(&state, &queue); err != nil {
		t.Fatalf("read river_job row: %v", err)
	}
	if state != "completed" || queue != maintenanceQueue {
		t.Fatalf("river_job = (state %q, queue %q), want (\"completed\", %q)", state, queue, maintenanceQueue)
	}

	// The tick did the work: the expired key is gone, purged by the production wiring
	// under the production role.
	var n int
	if err := superPool.QueryRow(ctx,
		`SELECT count(*) FROM command_idempotency_keys WHERE tenant_id = $1`, tenantID,
	).Scan(&n); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d expired key(s) remain after the tick; the job ran but purged nothing", n)
	}
	t.Logf("production worker pool completed a %q job on the %q queue and purged the expired key",
		cleanupIdempotencyKeysJobKind, maintenanceQueue)

	pool.Stop()
}
