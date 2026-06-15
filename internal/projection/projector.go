package projection

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
)

// Projector applies the global event stream to a single read model. Each
// implementation is registered with the framework as one River periodic job and
// runs as a deployment-wide singleton, coordinated by an advisory lock keyed on
// Name.
type Projector interface {
	// Name identifies the projector. It is the advisory-lock key suffix
	// ("projector."+Name()) and the projection_progress row id, so it must be
	// stable and unique across projectors.
	Name() string

	// Apply applies a batch of events to the read model within tx. The batch is in
	// ascending (insert_xid, stream_position) order.
	//
	// Apply MUST be idempotent under reprocessing. The read-model write and the
	// watermark advance commit together, but a process can die after Apply and
	// before the commit reaches the disk, so the next run replays the same batch;
	// replaying it must produce the same end state. Idempotency means upserts keyed
	// by aggregate identifier whose SET is an ABSOLUTE value derived from the events
	// (for example the aggregate's latest sequence number), NEVER an unconditional
	// increment -- an increment would double-count on reprocess.
	Apply(ctx context.Context, tx pgx.Tx, evts []events.Event) error
}
