package queue

import (
	"context"

	"github.com/riverqueue/river"
)

// testJobKind is the kind of the noop test job. The enqueue tests (Step 2) and
// the worker round-trip test (Step 3) both target it; no production worker
// registers it. Kept as a const so both steps assert against one literal.
const testJobKind = "test.noop"

// noopArgs is a minimal river.JobArgs used only by this package's tests. It
// carries no fields, so it encodes to {}; its sole purpose is to exercise the
// insert path here and, in Step 3, the work path. The value receiver means both
// noopArgs{} and a pointer satisfy river.JobArgs.
type noopArgs struct{}

// Kind identifies the job. River allows dots in kinds (it rejects only spaces and
// commas), so "test.noop" is valid.
func (noopArgs) Kind() string { return testJobKind }

// Compile-time assertion that the test job satisfies River's args interface.
var _ river.JobArgs = noopArgs{}

// noopWorker is the test-only worker for the test.noop kind. The production
// worker subcommand registers NO workers (it is the pool foundation only — real
// job kinds and their workers arrive in later epics), so this lives in the test
// package and is registered only by the AC-1 round-trip test.
//
// It embeds river.WorkerDefaults to inherit the default timeout/retry hooks and
// signals each completed run on a buffered channel so the test can observe that
// the worker actually executed (in addition to the job reaching the completed
// state). The channel is buffered so a Work call never blocks even if the test
// is not yet receiving.
type noopWorker struct {
	river.WorkerDefaults[noopArgs]
	worked chan struct{}
}

// Work satisfies river.Worker[noopArgs]. It does the trivial job — record that it
// ran — and returns nil so River marks the job completed. The non-blocking send
// tolerates a full buffer (e.g. more runs than the test drains).
func (w *noopWorker) Work(_ context.Context, _ *river.Job[noopArgs]) error {
	select {
	case w.worked <- struct{}{}:
	default:
	}
	return nil
}

// Compile-time assertion that noopWorker satisfies River's worker interface for
// the noop args.
var _ river.Worker[noopArgs] = (*noopWorker)(nil)
