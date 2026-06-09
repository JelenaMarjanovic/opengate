// Package inmemory holds in-memory doubles of the outbound ports, used by the
// contract suites and by higher-level tests that need a real port implementation
// without a database. The doubles reproduce the observable semantics of the
// production adapters (sequence assignment, optimistic-concurrency conflicts,
// global ordering) but operate on raw in-process data: no RLS, no tenant context,
// no SQL. Tenant scoping for the range read is the explicit tenantID argument,
// exactly as on the BYPASSRLS production path.
package inmemory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// EventStore is a faithful in-memory double of ports.EventStore. It
// mirrors the Postgres adapter's observable semantics so both pass the same
// contract suite.
type EventStore struct {
	mu sync.Mutex

	// byAggregate holds each aggregate's events in append (sequence) order; it
	// backs Load and the current-sequence check that drives Append's conflict
	// detection.
	byAggregate map[uuid.UUID][]events.Event
	// all is the flat global log in append order; it backs the two range reads.
	all []events.Event
	// streamPos is the global monotonic stream-position counter; the first event
	// appended gets 1 (++streamPos).
	streamPos int64
}

// Compile-time assertion that the double satisfies the port.
var _ ports.EventStore = (*EventStore)(nil)

// NewEventStore returns an empty in-memory event store.
func NewEventStore() *EventStore {
	return &EventStore{byAggregate: make(map[uuid.UUID][]events.Event)}
}

// Append mirrors the Postgres semantics under a single lock: the aggregate's
// current sequence (the highest stored sequence, or 0 if none) must equal
// expectedSequence, else events.ErrConcurrencyConflict. On success the i-th event
// (0-based) is assigned sequence = expectedSequence + 1 + i, a fresh global
// stream_position, and aggregate_id = aggregateID (the parameter, authoritative).
// Copies are stored so the caller's slice is never mutated; empty evts is a no-op.
func (s *EventStore) Append(ctx context.Context, aggregateID uuid.UUID, expectedSequence int64, evts []events.Event) error {
	if len(evts) == 0 {
		return nil // no-op: nothing to append
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.currentSequence(aggregateID) != expectedSequence {
		return events.ErrConcurrencyConflict
	}

	for i := range evts {
		// Store a copy so a later mutation of the caller's slice cannot reach into
		// the store; the assigned fields are authoritative over the input's.
		stored := evts[i]
		stored.AggregateID = aggregateID
		stored.Sequence = expectedSequence + 1 + int64(i)
		s.streamPos++
		stored.StreamPosition = s.streamPos

		s.byAggregate[aggregateID] = append(s.byAggregate[aggregateID], stored)
		s.all = append(s.all, stored)
	}
	return nil
}

// currentSequence returns the highest stored sequence for aggregateID, or 0 if it
// has no events. The slice is kept in append (ascending sequence) order, so the
// last element holds the current version. Caller must hold the lock.
func (s *EventStore) currentSequence(aggregateID uuid.UUID) int64 {
	stream := s.byAggregate[aggregateID]
	if len(stream) == 0 {
		return 0
	}
	return stream[len(stream)-1].Sequence
}

// Load returns a copy of the aggregate's events in sequence order, or an empty
// slice if the aggregate has none.
func (s *EventStore) Load(ctx context.Context, aggregateID uuid.UUID) ([]events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream := s.byAggregate[aggregateID]
	out := make([]events.Event, len(stream))
	copy(out, stream)
	return out, nil
}

// ReadAfterPosition returns copies of the global events with stream_position >
// position, sorted by stream_position, capped at limit.
func (s *EventStore) ReadAfterPosition(ctx context.Context, position int64, limit int) ([]events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]events.Event, 0, len(s.all))
	for _, evt := range s.all {
		if evt.StreamPosition > position {
			out = append(out, evt)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StreamPosition < out[j].StreamPosition
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ReadByTenantAndTimeRange returns copies of the global events for tenantID whose
// occurred_at falls in the inclusive [from, to] window, sorted by occurred_at
// then stream_position (the same tie-break the SQL query uses). Tenant scoping is
// the explicit argument — there is no RLS here.
func (s *EventStore) ReadByTenantAndTimeRange(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]events.Event, 0, len(s.all))
	for _, evt := range s.all {
		if evt.TenantID != tenantID {
			continue
		}
		// Inclusive bounds: occurred_at in [from, to].
		if evt.OccurredAt.Before(from) || evt.OccurredAt.After(to) {
			continue
		}
		out = append(out, evt)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].StreamPosition < out[j].StreamPosition
		}
		return out[i].OccurredAt.Before(out[j].OccurredAt)
	})
	return out, nil
}
