package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// aggregateSequenceConstraint is the name of the per-aggregate optimistic-
// concurrency unique constraint (aggregate_id, sequence) defined in the events
// table migration. A 23505 on THIS constraint is a genuine concurrency conflict
// (two appends raced on the same aggregate version); a 23505 on any OTHER events
// constraint (stream_position uniqueness, the primary key) is not a conflict but
// an internal defect (a nextval or UUID collision), so the mapping is constraint-
// specific and must not be collapsed.
const aggregateSequenceConstraint = "events_aggregate_sequence_unique"

// appendEventsSQL inserts a batch of events in a single statement. The per-event
// columns arrive as parallel arrays unnested into rows; aggregate_id ($1) is the
// authoritative method parameter applied to every row, and stream_position is
// assigned inline by nextval('events_stream_position_seq') (the column has no
// DEFAULT — see the create_events migration). RETURNING stream_position backs the
// post-condition count check here and is the future seam for a downstream publish
// (NOTIFY/SSE) that will read the assigned positions.
const appendEventsSQL = `
INSERT INTO events (
    id, tenant_id, aggregate_id, aggregate_type, sequence, stream_position,
    event_type, payload, metadata, occurred_at
)
SELECT
    u.id, u.tenant_id, $1, u.aggregate_type, u.sequence,
    nextval('events_stream_position_seq'),
    u.event_type, u.payload, u.metadata, u.occurred_at
FROM unnest(
    $2::uuid[], $3::uuid[], $4::text[], $5::bigint[],
    $6::text[], $7::jsonb[], $8::jsonb[], $9::timestamptz[]
) AS u(id, tenant_id, aggregate_type, sequence, event_type, payload, metadata, occurred_at)
RETURNING stream_position;`

// EventStore is the Postgres adapter implementing ports.EventStore. It is
// pool-agnostic: it holds a db.DBTX (which *pgxpool.Pool satisfies) and never
// references RLS, tenant context, or pool selection. The composition root decides
// which pool to construct it over — the RLS-bound opengate_app pool for the
// command path (Append/Load), the BYPASSRLS opengate_bypass pool for the
// cross-tenant reads (ReadAfterPosition/ReadByTenantAndTimeRange) — so the same
// adapter type serves both paths without knowing which one it is.
type EventStore struct {
	db db.DBTX     // held for the hand-written Append (single INSERT ... RETURNING)
	q  *db.Queries // sqlc queries (Step 1) backing the three reads
}

// Compile-time assertion that the adapter satisfies the port.
var _ ports.EventStore = (*EventStore)(nil)

// NewEventStore returns an EventStore backed by the given pool. The argument is a
// db.DBTX so the adapter stays pool-agnostic; a *pgxpool.Pool satisfies it.
func NewEventStore(pool db.DBTX) *EventStore {
	return &EventStore{db: pool, q: db.New(pool)}
}

// Append commits evts as the next contiguous sequence on aggregateID (Model B:
// the i-th event, 0-based, is assigned sequence = expectedSequence + 1 + i). The
// aggregate_id of every row is the method parameter (the per-event AggregateID is
// not read on input); stream_position is assigned by the statement via nextval,
// not the caller. tenant_id per row comes from each event's TenantID — on the
// RLS-bound pool the events policy's WITH CHECK enforces it matches the bound
// tenant; the adapter does not set it. The input slice is never mutated.
//
// A unique-constraint violation on events_aggregate_sequence_unique maps to
// events.ErrConcurrencyConflict (the caller re-loads and retries). Any other
// 23505 (stream_position or id collision) is a real defect and surfaces as a
// wrapped internal error, never as a concurrency conflict.
func (s *EventStore) Append(ctx context.Context, aggregateID uuid.UUID, expectedSequence int64, evts []events.Event) error {
	if len(evts) == 0 {
		return nil // no-op: nothing to append
	}

	// Build the per-event parameter arrays. Sequences are derived here (Model B),
	// not read from the input events, and the input slice is left untouched.
	n := len(evts)
	ids := make([]uuid.UUID, n)
	tenantIDs := make([]uuid.UUID, n)
	aggregateTypes := make([]string, n)
	sequences := make([]int64, n)
	eventTypes := make([]string, n)
	payloads := make([][]byte, n)
	metadatas := make([][]byte, n)
	occurredAts := make([]time.Time, n)

	for i := range evts {
		evt := &evts[i]
		ids[i] = evt.ID
		tenantIDs[i] = evt.TenantID
		aggregateTypes[i] = evt.AggregateType
		sequences[i] = expectedSequence + 1 + int64(i)
		eventTypes[i] = evt.Type
		payloads[i] = evt.Payload // json.RawMessage is already bytes
		metaBytes, err := json.Marshal(evt.Metadata)
		if err != nil {
			return fmt.Errorf("marshal event metadata: %w: %w", apperr.ErrInternal, err)
		}
		metadatas[i] = metaBytes
		occurredAts[i] = evt.OccurredAt
	}

	rows, err := s.db.Query(ctx, appendEventsSQL,
		aggregateID, ids, tenantIDs, aggregateTypes, sequences,
		eventTypes, payloads, metadatas, occurredAts)
	if err != nil {
		return mapAppendError(err)
	}
	defer rows.Close()

	// Drain RETURNING and count. The positions are not propagated (the port
	// returns only error); the count is a cheap post-condition.
	var returned int
	for rows.Next() {
		var pos int64
		if err := rows.Scan(&pos); err != nil {
			return fmt.Errorf("scan returned stream_position: %w: %w", apperr.ErrInternal, err)
		}
		returned++
	}
	// rows.Err() surfaces a deferred error (incl. a constraint violation that the
	// driver only reports once the result is consumed), so it is mapped too.
	if err := rows.Err(); err != nil {
		return mapAppendError(err)
	}
	if returned != n {
		return fmt.Errorf("append inserted %d events, expected %d: %w", returned, n, apperr.ErrInternal)
	}
	return nil
}

// mapAppendError translates a driver error from the Append insert into the right
// domain or internal error. Only a 23505 on events_aggregate_sequence_unique is a
// concurrency conflict; every other unique violation (stream_position, primary
// key) is an internal defect and must not be masked as a conflict.
func mapAppendError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		if pgErr.ConstraintName == aggregateSequenceConstraint {
			return fmt.Errorf("append events: %w", events.ErrConcurrencyConflict)
		}
		// A 23505 on any other constraint is NOT a concurrency conflict; it points
		// to a nextval or UUID collision and must surface as an internal error.
		return fmt.Errorf("append events: unexpected unique violation on %q: %w: %w",
			pgErr.ConstraintName, apperr.ErrInternal, err)
	}
	return fmt.Errorf("append events: %w: %w", apperr.ErrInternal, err)
}

// Load returns all events for aggregateID in sequence order. An aggregate with no
// events yields an empty slice (not an error): a new aggregate legitimately has
// none. Runs the Step 1 LoadAggregateEvents query.
func (s *EventStore) Load(ctx context.Context, aggregateID uuid.UUID) ([]events.Event, error) {
	rows, err := s.q.LoadAggregateEvents(ctx, aggregateID)
	if err != nil {
		return nil, fmt.Errorf("load aggregate events: %w: %w", apperr.ErrInternal, err)
	}
	return mapEvents(rows)
}

// ReadAfterPosition returns events with stream_position > position, in global
// order, capped at limit. Runs the Step 1 ReadEventsAfterPosition query; the
// port's limit int is cast to the generated int32 field.
func (s *EventStore) ReadAfterPosition(ctx context.Context, position int64, limit int) ([]events.Event, error) {
	// Clamp the page size into the int32 range the generated LIMIT param uses, so
	// the narrowing cannot overflow on a pathological caller value (a projector
	// page size is small and positive; this guard only handles the absurd cases).
	pageSize := limit
	if pageSize < 0 {
		pageSize = 0
	} else if pageSize > math.MaxInt32 {
		pageSize = math.MaxInt32
	}
	rows, err := s.q.ReadEventsAfterPosition(ctx, db.ReadEventsAfterPositionParams{
		StreamPosition: position,
		Limit:          int32(pageSize), //nolint:gosec // G115: bounded by the clamp above
	})
	if err != nil {
		return nil, fmt.Errorf("read events after position: %w: %w", apperr.ErrInternal, err)
	}
	return mapEvents(rows)
}

// ReadByTenantAndTimeRange returns a tenant's events within the inclusive
// [from, to] window, in chronological order. Runs the Step 1
// ReadTenantEventsInTimeRange query; the bounds are passed as valid Timestamptz.
func (s *EventStore) ReadByTenantAndTimeRange(ctx context.Context, tenantID uuid.UUID, from, to time.Time) ([]events.Event, error) {
	rows, err := s.q.ReadTenantEventsInTimeRange(ctx, db.ReadTenantEventsInTimeRangeParams{
		TenantID: tenantID,
		FromTime: timestamptz(from),
		ToTime:   timestamptz(to),
	})
	if err != nil {
		return nil, fmt.Errorf("read tenant events in time range: %w: %w", apperr.ErrInternal, err)
	}
	return mapEvents(rows)
}

// mapEvents maps a slice of generated db.Event rows to domain events, returning a
// non-nil empty slice when there are no rows so callers can range over the result
// uniformly. A malformed metadata row is a wrapped internal error.
func mapEvents(rows []db.Event) ([]events.Event, error) {
	out := make([]events.Event, 0, len(rows))
	for i := range rows {
		evt, err := mapEvent(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, evt)
	}
	return out, nil
}

// mapEvent maps one generated db.Event row to a domain events.Event. It returns
// an error because metadata is stored as JSONB and must be unmarshalled; a
// malformed metadata row is a wrapped internal error rather than a silent zero.
func mapEvent(row *db.Event) (events.Event, error) {
	var metadata events.EventMetadata
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		return events.Event{}, fmt.Errorf("unmarshal event metadata (id %s): %w: %w",
			row.ID, apperr.ErrInternal, err)
	}
	return events.Event{
		ID:             row.ID,
		AggregateID:    row.AggregateID,
		AggregateType:  row.AggregateType,
		Sequence:       row.Sequence,
		StreamPosition: row.StreamPosition,
		TenantID:       row.TenantID,
		OccurredAt:     row.OccurredAt.Time,
		Type:           row.EventType,
		Payload:        json.RawMessage(row.Payload),
		Metadata:       metadata,
	}, nil
}
