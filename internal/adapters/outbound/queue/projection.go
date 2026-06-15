package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/JelenaMarjanovic/opengate/internal/projection"
)

// Projection wiring (US-03.05). Each projector is driven by a River periodic job
// that fires every projectionInterval onto the dedicated projectionQueue; the job's
// worker delegates to projection.Run, which enforces the per-projector singleton via
// an advisory lock. The queue is only a safety net (decision §213): River's
// concurrency does not provide the singleton guarantee, the lock does.
const (
	// projectionQueue is the dedicated queue for projector ticks, separate from the
	// default queue so a slow projector cannot starve ordinary jobs and vice versa.
	projectionQueue = "projection"

	// projectionQueueMaxWorkers caps concurrency on the projection queue. It is sized
	// to the five seeded v1 projectors (create_projection_progress) so distinct
	// projectors can run concurrently; per-projector mutual exclusion is the advisory
	// lock's job, not the queue's. Must stay within 1..river.QueueNumWorkersMax.
	projectionQueueMaxWorkers = 5

	// projectionInterval is how often each projector's periodic job fires. A tick
	// that overruns the interval is self-limiting: the next tick's job runs, its
	// TryWithLock finds the lock held by the still-running tick, and it no-ops
	// quickly instead of piling up work.
	projectionInterval = 500 * time.Millisecond

	// projectorJobKind is the River kind shared by every projector tick. One args
	// type serves all projectors; the projector identity rides in the args and the
	// worker resolves it, so adding a projector needs no new job kind or worker type.
	projectorJobKind = "projection.run"
)

// projectorJobArgs is the River job payload for one projector tick. It carries only
// the projector name; the worker maps it to a registered Projector.
type projectorJobArgs struct {
	Projector string `json:"projector"`
}

// Kind identifies the job. River allows dots in kinds, so "projection.run" is valid.
func (projectorJobArgs) Kind() string { return projectorJobKind }

// Compile-time assertion that the args satisfy River's interface.
var _ river.JobArgs = projectorJobArgs{}

// projectorWorker runs one projector tick. A single worker type serves every
// projector: it looks the projector up by name and hands off to projection.Run on
// the worker pool's BYPASSRLS pool.
type projectorWorker struct {
	river.WorkerDefaults[projectorJobArgs]
	pool       *pgxpool.Pool
	metrics    *projection.Metrics
	projectors map[string]projection.Projector
}

// Compile-time assertion that the worker satisfies River's worker interface.
var _ river.Worker[projectorJobArgs] = (*projectorWorker)(nil)

// Work resolves the projector named in the job and runs one iteration. An unknown
// name is a wiring error (a periodic job was registered without its projector) and
// surfaces rather than silently completing.
func (w *projectorWorker) Work(ctx context.Context, job *river.Job[projectorJobArgs]) error {
	p, ok := w.projectors[job.Args.Projector]
	if !ok {
		return fmt.Errorf("queue: no projector registered for %q", job.Args.Projector)
	}
	return projection.Run(ctx, w.pool, p, w.metrics)
}

// newProjectorWorker builds the single projector worker over pool and metrics,
// indexing the projectors by name for Work to resolve. The map is empty in
// production (no real projector is registered yet), which is harmless: with no
// periodic job enqueuing a tick, Work is never called.
func newProjectorWorker(pool *pgxpool.Pool, metrics *projection.Metrics, projectors []projection.Projector) *projectorWorker {
	byName := make(map[string]projection.Projector, len(projectors))
	for _, p := range projectors {
		byName[p.Name()] = p
	}
	return &projectorWorker{pool: pool, metrics: metrics, projectors: byName}
}

// projectorPeriodicJobs builds one periodic job per projector: every
// projectionInterval, insert a projectorJobArgs onto the projection queue, also
// running once immediately on worker start (RunOnStart) so a freshly-deployed worker
// does not wait a full interval before catching up.
func projectorPeriodicJobs(projectors []projection.Projector) []*river.PeriodicJob {
	jobs := make([]*river.PeriodicJob, 0, len(projectors))
	for _, p := range projectors {
		// Capture the name in a fresh variable so the constructor closure does not
		// close over the loop value.
		name := p.Name()
		jobs = append(jobs, river.NewPeriodicJob(
			river.PeriodicInterval(projectionInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return projectorJobArgs{Projector: name}, &river.InsertOpts{Queue: projectionQueue}
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	return jobs
}
