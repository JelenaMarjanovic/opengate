package queue

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Worker pool sizing and shutdown timing.
//
// These mirror the api subcommand's 30s graceful-drain budget (apiShutdownTimeout)
// so both subcommands shut down on the same clock, and encode decision A3:
//
//   - workerMaxWorkers is the per-queue concurrency on the default queue. It is a
//     fixed, conservative foundation value; later stories can promote it to a
//     config knob if a deployment needs more parallelism. It must stay within
//     1..river.QueueNumWorkersMax.
//   - workerDrainTimeout is the total grace budget for a graceful stop — the
//     deadline on Stop(ctx).
//   - workerSoftStopTimeout is Config.SoftStopTimeout: the grace budget MINUS a
//     margin. River gives in-flight jobs this long to finish on a soft stop before
//     it cancels their contexts internally. The margin (workerSoftStopMargin)
//     leaves room between that internal escalation and the outer Stop deadline, so
//     the well-behaved path completes within the budget and only a genuinely stuck
//     job trips the explicit StopAndCancel fallback.
//   - workerHardStopTimeout bounds that last-resort StopAndCancel.
const (
	workerMaxWorkers      = 10
	workerDrainTimeout    = 30 * time.Second
	workerSoftStopMargin  = 5 * time.Second
	workerSoftStopTimeout = workerDrainTimeout - workerSoftStopMargin // 25s
	workerHardStopTimeout = 5 * time.Second
)

// WorkerPool is the worker-role River client plus its lifecycle, exposed to the
// composition root (the `worker` subcommand) so River's types stay inside this
// adapter. The subcommand only deals with Start and Stop; the graceful-drain
// orchestration (decision A3) lives here, next to the SoftStopTimeout it pairs
// with.
type WorkerPool struct {
	client *river.Client[pgx.Tx]
	logger *slog.Logger
}

// NewWorkerPool builds the PRODUCTION worker pool over the BYPASSRLS pool's
// driver (the worker fetches/works/completes jobs cross-tenant as opengate_bypass).
//
// It is deliberately the FOUNDATION only: it polls the default queue with the
// global trace-extract middleware installed, but registers NO workers. The Workers
// registry is intentionally empty — no real job kind has a worker yet (those
// arrive in later epics), and production has no enqueuers for real kinds, so the
// pool runs and polls but processes nothing until a later story registers one. The
// test.noop worker is wired only by the AC-1 round-trip test, never here.
func NewWorkerPool(pool *pgxpool.Pool, logger *slog.Logger) (*WorkerPool, error) {
	wc := &workerConfig{
		queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: workerMaxWorkers}},
		workers: river.NewWorkers(), // intentionally empty — foundation only (Step 3)
		// Global trace-extract middleware, registered once for every worked job
		// (decision B3), mirroring the single insert-side injection point.
		middleware:      []rivertype.Middleware{&traceMiddleware{}},
		softStopTimeout: workerSoftStopTimeout,
	}
	client, err := newRiverClient(RoleWorker, pool, logger, wc)
	if err != nil {
		return nil, err
	}
	return &WorkerPool{client: client, logger: logger}, nil
}

// Start boots the pool: it begins polling its queues and returns once the pool's
// goroutines are running (it does not block). ctx is the run context; the pool
// runs until Stop is called. The `worker` subcommand passes a NON-signal context
// here so that shutdown is driven explicitly through Stop (decision A3) rather
// than by cancelling this context, which would hand River the escalation timing.
func (w *WorkerPool) Start(ctx context.Context) error {
	return w.client.Start(ctx)
}

// Stop performs the decision-A3 graceful drain. It is called once the shutdown
// signal has arrived, so it derives its deadlines from a fresh background context
// (the signal context is already cancelled by then, exactly as serveAPI does for
// the HTTP drain).
//
//	Stop(ctx)           -- graceful: stop accepting new jobs, wait for in-flight
//	                       ones. With SoftStopTimeout set, River escalates to
//	                       cancelling stuck jobs' contexts internally after the
//	                       soft timeout.
//	StopAndCancel(ctx)  -- hard fallback: only if the graceful drain overruns the
//	                       full grace budget (a job ignoring cancellation), cancel
//	                       all work and wait briefly for it to unwind.
//
// Errors are logged, not returned: shutdown is best-effort and the process is
// exiting regardless, so there is no caller left to act on a drain error.
func (w *WorkerPool) Stop() {
	drainWorkerClient(w.logger, w.client)
}

// drainWorkerClient is the A3 drain, factored out so the AC-1 test can drive the
// same orchestration over a client it built directly (with the test.noop worker
// registered) instead of through NewWorkerPool.
func drainWorkerClient(logger *slog.Logger, client *river.Client[pgx.Tx]) {
	stopCtx, cancel := context.WithTimeout(context.Background(), workerDrainTimeout)
	defer cancel()

	switch err := client.Stop(stopCtx); {
	case err == nil:
		logger.Info("worker: pool drained cleanly")
	case errors.Is(err, context.DeadlineExceeded):
		// The graceful drain (including River's internal soft->hard escalation)
		// could not finish within the grace budget. Escalate explicitly.
		logger.Warn("worker: graceful stop exceeded deadline; cancelling in-flight jobs",
			slog.Duration("timeout", workerDrainTimeout))
		hardCtx, hardCancel := context.WithTimeout(context.Background(), workerHardStopTimeout)
		defer hardCancel()
		if err := client.StopAndCancel(hardCtx); err != nil {
			logger.Error("worker: hard stop error", slog.String("error", err.Error()))
			return
		}
		logger.Info("worker: pool hard-stopped")
	default:
		logger.Error("worker: pool stop error", slog.String("error", err.Error()))
	}
}
