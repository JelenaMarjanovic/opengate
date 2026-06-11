package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// metadataTraceKey is the single key under which Enqueue stores the W3C trace
// carrier inside a River job's metadata jsonb. The worker side (Step 3) reads
// this EXACT key to rebuild the carrier, extract the parent context, and start a
// child span — so the constant is the contract between the enqueue and work
// halves and must not change without changing both.
const metadataTraceKey = "trace"

// JobEnqueuer is the tx-scoped River adapter implementing ports.JobEnqueuer. It
// is constructed per unit-of-work around a single pgx.Tx and inserts every job
// through that transaction (River's InsertTx), giving the transactional-outbox
// guarantee: a job becomes visible to workers only if the surrounding business
// transaction commits, and disappears with it on rollback.
//
// It holds two collaborators:
//
//   - tx: the concrete transaction the job is inserted on. The port stays
//     driver-free (it never names pgx); the concrete type lives here, at the
//     adapter — exactly as the EventStore adapter keeps its pool concrete.
//   - client: the insert-only RoleAPI River client (Step 1). InsertTx runs the
//     insert on the passed tx, not on the client's own pool, so the client
//     contributes only River's encoding/options machinery, never a connection.
type JobEnqueuer struct {
	tx     pgx.Tx
	client *river.Client[pgx.Tx]
}

// Compile-time assertion that the adapter satisfies the port.
var _ ports.JobEnqueuer = (*JobEnqueuer)(nil)

// NewJobEnqueuer returns a JobEnqueuer that inserts jobs on tx using client.
// Both are supplied by the unit-of-work at the composition root (wiring deferred
// to E4): tx is the same transaction the command's EventStore.Append runs on, so
// the events and the jobs they trigger share one atomic commit.
func NewJobEnqueuer(tx pgx.Tx, client *river.Client[pgx.Tx]) *JobEnqueuer {
	return &JobEnqueuer{tx: tx, client: client}
}

// Enqueue inserts job within the adapter's transaction, first injecting the
// active trace context into the job's metadata so the worker can continue the
// trace (System Design §6 carrier semantics; the carrier rides in metadata, not
// in the args struct).
//
// The metadata is always shaped {"<metadataTraceKey>": {<W3C carrier>}}. When no
// span is active — the case in production today, since the OTel SDK that produces
// spans is not yet wired — the global propagator writes nothing and the carrier
// stays an empty object; the key is still emitted so the worker's extract path
// always reads a stable shape. A failed insert is returned as ErrEnqueueFailed
// with the driver cause flattened to a string, never wrapped (§7: adapters do
// not return driver errors), so callers match the sentinel without seeing pgx.
func (e *JobEnqueuer) Enqueue(ctx context.Context, job river.JobArgs) error {
	// 1. Serialize the active trace context into a W3C text-map carrier. The
	//    carrier is non-nil so an empty result marshals to {} rather than null;
	//    Inject writes nothing when no span is active and never errors.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	// 2. Wrap the carrier under the agreed key and marshal it to the metadata
	//    blob. The key comes from metadataTraceKey so the insert and the Step 3
	//    extract share one source of truth. Marshalling a map of strings cannot
	//    realistically fail, but the error is surfaced via the sentinel rather
	//    than ignored.
	meta, err := json.Marshal(map[string]propagation.MapCarrier{metadataTraceKey: carrier})
	if err != nil {
		return fmt.Errorf("%w: marshal trace metadata: %s", ports.ErrEnqueueFailed, err)
	}

	// 3. Insert on the caller's transaction. On rollback the row never becomes
	//    visible (outbox); on commit it is available to the worker. River persists
	//    InsertOpts.Metadata verbatim, so the {"trace": ...} envelope survives.
	if _, err := e.client.InsertTx(ctx, e.tx, job, &river.InsertOpts{Metadata: meta}); err != nil {
		return fmt.Errorf("%w: %s", ports.ErrEnqueueFailed, err)
	}
	return nil
}
