package outbound

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
)

// EventStore is the outbound port through which the application persists and
// retrieves domain events. The production implementation is PostgresEventStore;
// the test double is InMemoryEventStore (both US-03.03).
type EventStore interface {
	// Append commits the given events as the next contiguous sequence on the
	// given aggregate. If expectedSequence does not match the current sequence
	// in the store, ErrConcurrencyConflict is returned and the caller re-loads
	// and retries.
	Append(ctx context.Context, aggregateID uuid.UUID, expectedSequence int64, evts []events.Event) error

	// Load returns all events for the given aggregate in sequence order.
	Load(ctx context.Context, aggregateID uuid.UUID) ([]events.Event, error)

	// ReadAfterPosition returns events with stream position > position, in
	// global order. Used by projector workers.
	ReadAfterPosition(ctx context.Context, position int64, limit int) ([]events.Event, error)

	// ReadByTenantAndTimeRange returns events for a tenant within an inclusive
	// time range. Used by the export capability.
	ReadByTenantAndTimeRange(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]events.Event, error)
}
