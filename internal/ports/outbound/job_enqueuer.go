package outbound

import (
	"context"
	"errors"

	"github.com/riverqueue/river"
)

// ErrEnqueueFailed is returned by JobEnqueuer.Enqueue when a job could not be
// enqueued (the underlying insert failed). It is a domain-meaningful sentinel
// matched via errors.Is (System Design §7). The concrete driver cause is
// flattened into the error message and never wrapped, so no pgx or River error
// type leaks across the port boundary — adapters do not return driver errors.
var ErrEnqueueFailed = errors.New("enqueue failed")

// JobEnqueuer is the outbound port through which the command path schedules
// background jobs. The production implementation is the tx-scoped River adapter
// in internal/adapters/outbound/queue; it inserts within the caller's database
// transaction so a job and the events it accompanies commit or roll back as one
// unit (the transactional-outbox guarantee — a job becomes visible to workers
// only if the surrounding business transaction commits).
//
// The port is intentionally driver-free EXCEPT for river.JobArgs: River is
// OpenGate's job vocabulary (System Design §6), so JobArgs is the natural unit of
// work to enqueue. This coupling is agreed and deliberate, mirroring how the
// EventStore port speaks in domain events.
//
// There is deliberately no tenant or transaction parameter on the method:
//
//   - The transaction is bound at construction (the adapter is tx-scoped), so a
//     single unit-of-work hands the same tx to every outbound port it drives.
//   - Tenant and trace context ride in ctx (System Design §7). The adapter reads
//     the active trace context from ctx and injects it into the job's metadata,
//     so the worker can continue the same trace when it later runs the job.
type JobEnqueuer interface {
	// Enqueue schedules job for background processing within the transaction the
	// adapter was constructed with. It returns nil on success and ErrEnqueueFailed
	// (matched via errors.Is) if the insert failed.
	Enqueue(ctx context.Context, job river.JobArgs) error
}
