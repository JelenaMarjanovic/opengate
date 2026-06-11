package queue

import "github.com/riverqueue/river"

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
