package queue

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/JelenaMarjanovic/opengate/internal/maintenance"
)

// Maintenance wiring (US-03.06). Retention enforcement is driven by River periodic
// jobs on a dedicated queue; each job's worker delegates to internal/maintenance,
// which enforces the deployment-wide singleton with an advisory lock. As with the
// projection queue, River's concurrency is only a safety net -- the lock, not the
// queue, is what guarantees one purge at a time.
const (
	// maintenanceQueue is the dedicated queue for retention jobs. It is separate from
	// the projection queue because it is a different concern with a different cadence
	// (minutes, not milliseconds): a purge that runs long must not occupy a projector
	// slot, and a backed-up projector must not delay retention. The future
	// cleanup.sessions and cleanup.dead_letter jobs (System Design §6) belong here too.
	maintenanceQueue = "maintenance"

	// maintenanceQueueMaxWorkers caps concurrency on the maintenance queue. One is the
	// honest number: every job on this queue is a deployment-wide singleton guarded by
	// its own advisory lock, so additional workers would only fetch ticks that
	// immediately skip. Must stay within 1..river.QueueNumWorkersMax.
	maintenanceQueueMaxWorkers = 1

	// cleanupIdempotencyKeysInterval is how often the idempotency purge fires (System
	// Design §4 and Database Schema §15: every five minutes against a ten-minute
	// retention). The two-to-one ratio means a key is deleted between ten and fifteen
	// minutes after it was written -- never before the window closes, and never long
	// after.
	cleanupIdempotencyKeysInterval = 5 * time.Minute

	// cleanupIdempotencyKeysJobKind is the River kind from System Design §6. It is a
	// stable identifier: it is persisted in river_job rows, so renaming it would
	// orphan any job already enqueued under the old name.
	cleanupIdempotencyKeysJobKind = "cleanup.idempotency_keys"
)

// cleanupIdempotencyKeysArgs is the River job payload for one purge tick. It carries
// no fields: the tick is a pure "now do the retention pass" signal, and everything it
// needs (the retention window, the tables) is a constant of the job itself, not a
// property of an individual run. An empty struct still serializes to a valid `{}`
// args document.
type cleanupIdempotencyKeysArgs struct{}

// Kind identifies the job. River allows dots in kinds, so "cleanup.idempotency_keys"
// is valid.
func (cleanupIdempotencyKeysArgs) Kind() string { return cleanupIdempotencyKeysJobKind }

// Compile-time assertion that the args satisfy River's interface.
var _ river.JobArgs = cleanupIdempotencyKeysArgs{}

// cleanupIdempotencyKeysWorker runs one retention pass. It owns no logic of its own:
// the transaction, the advisory lock, and the DELETEs live in internal/maintenance,
// so the job's behavior is testable without River.
type cleanupIdempotencyKeysWorker struct {
	river.WorkerDefaults[cleanupIdempotencyKeysArgs]
	pool    *pgxpool.Pool
	metrics *maintenance.Metrics
	logger  *slog.Logger
}

// Compile-time assertion that the worker satisfies River's worker interface.
var _ river.Worker[cleanupIdempotencyKeysArgs] = (*cleanupIdempotencyKeysWorker)(nil)

// Work runs one purge iteration on the worker pool's BYPASSRLS pool.
//
// A SKIPPED iteration (another instance held the lock) completes the job
// successfully and logs at debug: contention is the expected state on a
// multi-instance deployment, and returning an error would put the job through
// River's retry backoff for a condition that resolves itself on the next tick five
// minutes later. Only a genuine failure -- a lost grant, a dead connection -- returns
// an error and gets retried.
func (w *cleanupIdempotencyKeysWorker) Work(ctx context.Context, _ *river.Job[cleanupIdempotencyKeysArgs]) error {
	res, err := maintenance.CleanupIdempotencyKeys(ctx, w.pool, w.metrics)
	if err != nil {
		return err
	}
	if res.Skipped {
		w.logger.Debug("cleanup: idempotency-key purge skipped; another instance holds the lock",
			slog.String("job_kind", cleanupIdempotencyKeysJobKind))
		return nil
	}

	// One attribute per purged table, so the log line reads the same shape as the
	// metric and a future table needs no change here.
	attrs := make([]any, 0, len(res.Tables)+1)
	attrs = append(attrs, slog.String("job_kind", cleanupIdempotencyKeysJobKind))
	for _, t := range res.Tables {
		attrs = append(attrs, slog.Int64(t.Table, t.Deleted))
	}
	w.logger.Info("cleanup: purged expired idempotency keys", attrs...)
	return nil
}

// newCleanupIdempotencyKeysWorker builds the purge worker over the bypass pool, the
// metrics handle, and the project logger.
func newCleanupIdempotencyKeysWorker(
	pool *pgxpool.Pool, metrics *maintenance.Metrics, logger *slog.Logger,
) *cleanupIdempotencyKeysWorker {
	return &cleanupIdempotencyKeysWorker{pool: pool, metrics: metrics, logger: logger}
}

// cleanupIdempotencyKeysJobConstructor is the periodic job's insert constructor:
// every tick inserts one argument-less job onto the maintenance queue. It is a named
// function rather than a literal inside the registration below so a test can assert
// the kind and the target queue directly.
func cleanupIdempotencyKeysJobConstructor() (river.JobArgs, *river.InsertOpts) {
	return cleanupIdempotencyKeysArgs{}, &river.InsertOpts{Queue: maintenanceQueue}
}

// cleanupIdempotencyKeysPeriodicJob registers the purge on its five-minute schedule.
//
// RunOnStart fires one tick immediately when the periodic scheduler starts, so a
// deployment that was down long enough to accumulate expired keys clears the backlog
// on boot instead of carrying it for up to another interval. Concurrent boots across
// instances are safe: River elects one scheduler leader, and even if two ticks did
// land at once the advisory lock admits exactly one -- the other skips.
func cleanupIdempotencyKeysPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(cleanupIdempotencyKeysInterval),
		cleanupIdempotencyKeysJobConstructor,
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
