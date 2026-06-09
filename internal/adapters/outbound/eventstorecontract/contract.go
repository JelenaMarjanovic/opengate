// Package eventstorecontract holds the shared, exported contract suite that every
// ports.EventStore implementation must satisfy (US-03.03, AC4). Both the Postgres
// adapter and the in-memory double import it from their _test.go files and run it
// against their own factory, so the two implementations are proven to have the
// same observable semantics from a single source of truth.
//
// Like internal/testsupport, this is a regular (non-_test.go) package because
// _test.go helpers are not importable across packages. It imports testing, but no
// production code imports it, so it never reaches the shipped binary.
package eventstorecontract

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// StoreFactory yields a fresh, empty EventStore plus the ctx and tenantID a
// single contract subtest must use. The Postgres factory returns a tenant-bound
// ctx (so RLS scopes the command-path reads) and a real tenants row; the
// in-memory factory returns context.Background() and a generated tenantID. Each
// subtest calls the factory once, so state never leaks between subtests.
type StoreFactory func(t *testing.T) (store ports.EventStore, ctx context.Context, tenantID uuid.UUID)

// RunEventStoreContract runs the full EventStore contract against the given
// factory. Both adapters call it; both must pass identically. Each subtest is its
// own function so the assertions stay legible (and within the complexity ceiling).
func RunEventStoreContract(t *testing.T, newStore StoreFactory) {
	t.Helper()

	t.Run("AC1 append then load returns events in sequence order", func(t *testing.T) {
		assertAppendThenLoad(t, newStore)
	})
	t.Run("AC2 stale expectedSequence is a concurrency conflict", func(t *testing.T) {
		assertStaleAppendConflicts(t, newStore)
	})
	t.Run("AC3 fifty events load in sequence order", func(t *testing.T) {
		assertFiftyEventsInOrder(t, newStore)
	})
	t.Run("reads: ReadAfterPosition resumes from a returned position", func(t *testing.T) {
		assertReadAfterPositionResumes(t, newStore)
	})
	t.Run("reads: ReadByTenantAndTimeRange returns exactly the in-window subset", func(t *testing.T) {
		assertReadByTimeRangeSubset(t, newStore)
	})
}

// assertAppendThenLoad is AC1: appending two events to an empty store and loading
// them back yields the two events with Sequence 1 and 2, in order.
func assertAppendThenLoad(t *testing.T, newStore StoreFactory) {
	t.Helper()
	store, ctx, tenantID := newStore(t)
	aggregateID := uuid.New()
	evts := buildEvents(tenantID, aggregateID, 2)

	if err := store.Append(ctx, aggregateID, 0, evts); err != nil {
		t.Fatalf("Append: %v", err)
	}

	loaded, err := store.Load(ctx, aggregateID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("Load returned %d events, want 2", len(loaded))
	}
	// Sequences are asserted EXACTLY (1, 2): Model B derives them from
	// expectedSequence and they are adapter-independent.
	assertSequences(t, loaded)
	assertPositions(t, loaded)
}

// assertStaleAppendConflicts is AC2: a second append with a stale
// expectedSequence returns ErrConcurrencyConflict and writes nothing.
func assertStaleAppendConflicts(t *testing.T, newStore StoreFactory) {
	t.Helper()
	store, ctx, tenantID := newStore(t)
	aggregateID := uuid.New()
	evts := buildEvents(tenantID, aggregateID, 2)

	// First append establishes sequence 1.
	if err := store.Append(ctx, aggregateID, 0, evts[:1]); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	// A second append with the now-stale expectedSequence 0 must conflict.
	err := store.Append(ctx, aggregateID, 0, evts[1:])
	if !errors.Is(err, events.ErrConcurrencyConflict) {
		t.Fatalf("stale Append err = %v, want errors.Is ErrConcurrencyConflict", err)
	}

	// The conflicting append wrote nothing: exactly one event remains.
	loaded, err := store.Load(ctx, aggregateID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("Load returned %d events after conflict, want 1", len(loaded))
	}
	if loaded[0].Sequence != 1 {
		t.Errorf("surviving event Sequence = %d, want 1", loaded[0].Sequence)
	}
}

// assertFiftyEventsInOrder is AC3: fifty events appended across several batches
// (with the correct running expectedSequence) load back in sequence order 1..50.
func assertFiftyEventsInOrder(t *testing.T, newStore StoreFactory) {
	t.Helper()
	store, ctx, tenantID := newStore(t)
	aggregateID := uuid.New()
	const total = 50
	evts := buildEvents(tenantID, aggregateID, total)

	// Several batches exercise the multi-call sequence arithmetic, not just one.
	batches := [][2]int{{0, 10}, {10, 30}, {30, total}}
	for _, b := range batches {
		lo, hi := b[0], b[1]
		if err := store.Append(ctx, aggregateID, int64(lo), evts[lo:hi]); err != nil {
			t.Fatalf("Append[%d:%d]: %v", lo, hi, err)
		}
	}

	loaded, err := store.Load(ctx, aggregateID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != total {
		t.Fatalf("Load returned %d events, want %d", len(loaded), total)
	}
	assertSequences(t, loaded)
	assertPositions(t, loaded)
}

// assertReadAfterPositionResumes covers ReadAfterPosition: read all from 0, then
// resume after the first returned event's position and assert the remainder, in
// stream-position order. The cursor is derived from a prior read, never hardcoded.
func assertReadAfterPositionResumes(t *testing.T, newStore StoreFactory) {
	t.Helper()
	store, ctx, tenantID := newStore(t)
	aggregateID := uuid.New()
	const total = 5
	evts := buildEvents(tenantID, aggregateID, total)
	if err := store.Append(ctx, aggregateID, 0, evts); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Read everything from position 0 (all real positions are > 0). Absolute
	// values are NOT asserted — only non-zero, distinct, strictly increasing.
	all, err := store.ReadAfterPosition(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ReadAfterPosition(0): %v", err)
	}
	if len(all) != total {
		t.Fatalf("ReadAfterPosition(0) returned %d events, want %d", len(all), total)
	}
	assertPositions(t, all)

	// Resume after the FIRST returned event's position (derived): the remainder is
	// everything but that first event, in order.
	cursor := all[0].StreamPosition
	rest, err := store.ReadAfterPosition(ctx, cursor, 100)
	if err != nil {
		t.Fatalf("ReadAfterPosition(%d): %v", cursor, err)
	}
	if len(rest) != total-1 {
		t.Fatalf("ReadAfterPosition(%d) returned %d events, want %d", cursor, len(rest), total-1)
	}
	for i, evt := range rest {
		if evt.StreamPosition <= cursor {
			t.Errorf("rest[%d].StreamPosition = %d, want > cursor %d", i, evt.StreamPosition, cursor)
		}
		// The remainder is the tail of the full read (events 2..total).
		if evt.ID != all[i+1].ID {
			t.Errorf("rest[%d].ID = %s, want %s", i, evt.ID, all[i+1].ID)
		}
	}
}

// assertReadByTimeRangeSubset covers ReadByTenantAndTimeRange: a window over a
// known subset returns exactly that subset, in chronological order.
func assertReadByTimeRangeSubset(t *testing.T, newStore StoreFactory) {
	t.Helper()
	store, ctx, tenantID := newStore(t)
	aggregateID := uuid.New()
	const total = 5
	evts := buildEvents(tenantID, aggregateID, total)
	if err := store.Append(ctx, aggregateID, 0, evts); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// buildEvents stamps OccurredAt as baseTime + i minutes. A window over events
	// index 1..3 (inclusive on both bounds) must return exactly those.
	from := occurredAt(1)
	to := occurredAt(3)
	got, err := store.ReadByTenantAndTimeRange(ctx, tenantID, from, to)
	if err != nil {
		t.Fatalf("ReadByTenantAndTimeRange: %v", err)
	}

	want := []uuid.UUID{evts[1].ID, evts[2].ID, evts[3].ID}
	if len(got) != len(want) {
		t.Fatalf("range read returned %d events, want %d", len(got), len(want))
	}
	var prev time.Time
	for i, evt := range got {
		if evt.ID != want[i] {
			t.Errorf("got[%d].ID = %s, want %s", i, evt.ID, want[i])
		}
		// Chronological order: occurred_at never decreases across the result.
		if i > 0 && evt.OccurredAt.Before(prev) {
			t.Errorf("got[%d].OccurredAt = %s is before previous %s (not chronological)",
				i, evt.OccurredAt, prev)
		}
		prev = evt.OccurredAt
	}
}

// assertSequences checks that loaded events carry the exact 1-based sequence of
// their position (1, 2, …), the adapter-independent Model B guarantee.
func assertSequences(t *testing.T, evts []events.Event) {
	t.Helper()
	for i, evt := range evts {
		if want := int64(i + 1); evt.Sequence != want {
			t.Errorf("evts[%d].Sequence = %d, want %d", i, evt.Sequence, want)
		}
	}
}

// baseTime is the fixed, UTC anchor for every contract event's OccurredAt. Using
// a fixed instant (not time.Now) keeps the time-range assertions deterministic.
var baseTime = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)

// occurredAt returns the OccurredAt of the i-th contract event: baseTime + i
// minutes, so events are strictly increasing in time and the range subtest can
// pick exact bounds.
func occurredAt(i int) time.Time {
	return baseTime.Add(time.Duration(i) * time.Minute)
}

// buildEvents constructs count contract events for one aggregate, all in tenantID
// with a populated metadata and compact payload. Sequence and StreamPosition are
// left zero on input — Append assigns them. OccurredAt is fixed and increasing so
// the time-range read is deterministic. IDs are fresh per event.
func buildEvents(tenantID, aggregateID uuid.UUID, count int) []events.Event {
	out := make([]events.Event, count)
	for i := range out {
		out[i] = events.Event{
			ID:            uuid.New(),
			AggregateID:   aggregateID,
			AggregateType: "member",
			TenantID:      tenantID,
			OccurredAt:    occurredAt(i),
			Type:          "member.created.v1",
			Payload:       json.RawMessage(`{"k":"v"}`),
			Metadata: events.EventMetadata{
				CorrelationID: "corr-" + uuid.NewString(),
				ActorID:       uuid.New(),
				ActorType:     "user",
				AppVersion:    "test",
			},
		}
	}
	return out
}

// assertPositions checks the adapter-agnostic stream-position invariants: every
// position is non-zero, all are distinct, and they strictly increase in append
// order. Absolute values are deliberately NOT asserted — the in-memory counter
// starts at 1 while the Postgres sequence is shared and may already be advanced.
func assertPositions(t *testing.T, evts []events.Event) {
	t.Helper()
	seen := make(map[int64]bool, len(evts))
	var prev int64
	for i, evt := range evts {
		if evt.StreamPosition == 0 {
			t.Errorf("evts[%d].StreamPosition = 0, want non-zero", i)
		}
		if seen[evt.StreamPosition] {
			t.Errorf("evts[%d].StreamPosition = %d is duplicated", i, evt.StreamPosition)
		}
		seen[evt.StreamPosition] = true
		if i > 0 && evt.StreamPosition <= prev {
			t.Errorf("evts[%d].StreamPosition = %d, want > previous %d", i, evt.StreamPosition, prev)
		}
		prev = evt.StreamPosition
	}
}
