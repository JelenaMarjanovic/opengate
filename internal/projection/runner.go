package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/coordination/advisory"
	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
)

// batchSize bounds how many events one iteration consumes. It is embedded directly
// in boundaryReadSQL's LIMIT (a fixed constant, never user input). The runner
// advances the watermark to the batch's maximum insert_xid with a strict ">" read,
// which is only safe if a single transaction's events never straddle the LIMIT
// boundary -- otherwise the strict ">" would skip the unconsumed remainder of that
// transaction. OpenGate's command path appends a handful of events per transaction,
// far below this cap, so a straddle cannot occur; a future bulk-append path would
// have to revisit this (e.g. a composite (insert_xid, stream_position) cursor).
const batchSize = 100

// boundaryReadSQL is the Option 3b snapshot-boundary read. It joins
// projection_progress purely to pull last_consumed_xid into the predicate (the
// watermark is taken from the row in-SQL, so no xid is constructed in Go on the read
// side), and selects events whose insert_xid is above that watermark and strictly
// below pg_snapshot_xmin(pg_current_snapshot()) -- the smallest still-running xid,
// so every selected event is from a transaction that has fully committed. Ordering
// by (insert_xid, stream_position) makes the last row the batch's maximum insert_xid
// (the new watermark) and the scan resumable. insert_xid is projected as ::text and
// treated as an opaque token: it is never parsed numerically in Go, sidestepping the
// fact that xid8 is an unsigned 64-bit value that would not fit a Go int64.
const boundaryReadSQL = `
SELECT
    e.id, e.tenant_id, e.aggregate_id, e.aggregate_type, e.sequence,
    e.stream_position, e.event_type, e.payload, e.metadata, e.occurred_at,
    e.insert_xid::text
FROM events e
JOIN projection_progress p ON p.projector_name = $1
WHERE e.insert_xid > p.last_consumed_xid
  AND e.insert_xid < pg_snapshot_xmin(pg_current_snapshot())
ORDER BY e.insert_xid, e.stream_position
LIMIT 100`

// advanceProgressSQL moves the watermark after a batch is applied. last_consumed_xid
// (authoritative) takes the batch's maximum insert_xid, passed as the opaque text
// token and cast back with ::xid8 so no Go xid8 type is needed. last_position is the
// batch's max stream_position (non-authoritative -- see the add_events_insert_xid
// migration -- so it may regress under out-of-order consumption and must never be a
// boundary). last_event_at is the batch's max occurred_at (the lag source). No FOR
// UPDATE is taken on the row: the advisory lock already guarantees no two runners
// touch the same projection_progress row concurrently.
const advanceProgressSQL = `
UPDATE projection_progress
SET last_consumed_xid = $2::xid8,
    last_position     = $3,
    last_event_at     = $4,
    updated_at        = now()
WHERE projector_name = $1`

// Run executes ONE projector iteration in ONE transaction on the bypass (BYPASSRLS,
// no tenant binding) pool. Projectors read across all tenants and write the no-RLS
// projection_progress, so they run on the bypass pool, never the RLS-bound request
// pool.
//
// The iteration is a deployment-wide singleton via a transaction-scoped advisory
// lock ("projector."+Name()): if another instance already holds it, this call
// no-ops -- the deferred rollback discards the empty transaction -- and returns nil.
// Because the lock guarantees no two runners touch the same projection_progress row
// at once, the watermark UPDATE needs no FOR UPDATE.
//
// On a successful commit, and ONLY when the lock was acquired (the owning instance
// records its own sample), Run publishes the lag gauge as now - last_event_at.
func Run(ctx context.Context, pool *pgxpool.Pool, p Projector, metrics *Metrics) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("projection %q: begin: %w", p.Name(), err)
	}
	// No-op once the tx commits; on the not-acquired and error paths it discards the
	// transaction (and releases the advisory lock if one was taken).
	defer func() { _ = tx.Rollback(ctx) }()

	var lagEventAt time.Time
	acquired, err := advisory.TryWithLock(ctx, tx, lockName(p.Name()), func() error {
		var ferr error
		lagEventAt, ferr = applyOnce(ctx, tx, p)
		return ferr
	})
	if err != nil {
		return err
	}
	if !acquired {
		// Another instance owns this projector right now; skip this iteration cleanly.
		return nil
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("projection %q: commit: %w", p.Name(), err)
	}

	// After the durable commit, publish the lag sample. Skip when there is no
	// last_event_at yet (a never-consumed projector that found an empty batch).
	if metrics != nil && !lagEventAt.IsZero() {
		metrics.RecordLag(p.Name(), time.Since(lagEventAt).Seconds())
	}
	return nil
}

// lockName is the advisory-lock key for a projector. Centralized so the runner and
// any wiring derive the identical key from the projector name.
func lockName(projectorName string) string { return "projector." + projectorName }

// applyOnce performs one consumption pass INSIDE an already-open transaction that
// already holds the projector's advisory lock: read the snapshot-boundary batch,
// apply it, and advance the watermark. It returns the timestamp for the lag sample
// (the batch's max occurred_at, or the stored last_event_at when the batch is empty)
// and does NOT commit -- Run owns commit/rollback and the lag recording.
//
// It is unexported and factored out of Run so the crash-reprocess test can replay
// this exact body and then roll back instead of commit.
func applyOnce(ctx context.Context, tx pgx.Tx, p Projector) (lagEventAt time.Time, err error) {
	batch, lastXID, maxPos, maxOccurred, err := readBatch(ctx, tx, p.Name())
	if err != nil {
		return time.Time{}, err
	}

	if len(batch) == 0 {
		// Nothing consumable: either no events, or all candidates are still in flight
		// per the snapshot boundary. Do not move the watermark. Sample lag from the
		// stored last_event_at so a caught-up projector still reports a value.
		return readLastEventAt(ctx, tx, p.Name())
	}

	if err := p.Apply(ctx, tx, batch); err != nil {
		return time.Time{}, fmt.Errorf("projection %q: apply: %w", p.Name(), err)
	}
	if err := advanceProgress(ctx, tx, p.Name(), lastXID, maxPos, maxOccurred); err != nil {
		return time.Time{}, err
	}
	return maxOccurred, nil
}

// readBatch runs the snapshot-boundary read and maps the rows to domain events,
// also returning the batch's maximum insert_xid (as the opaque text token -- the
// last row, since rows are ascending by insert_xid), maximum stream_position, and
// maximum occurred_at, all computed in Go across the batch (NOT taken from the last
// row, because out-of-order commits mean a later insert_xid can carry a smaller
// stream_position or occurred_at).
func readBatch(ctx context.Context, tx pgx.Tx, projectorName string) (
	batch []events.Event, lastXID string, maxPos int64, maxOccurred time.Time, err error,
) {
	rows, err := tx.Query(ctx, boundaryReadSQL, projectorName)
	if err != nil {
		return nil, "", 0, time.Time{}, fmt.Errorf("projection %q: read boundary batch: %w", projectorName, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			evt          events.Event
			payloadBytes []byte
			metaBytes    []byte
			occurredAt   time.Time
			xidText      string
		)
		if err := rows.Scan(
			&evt.ID, &evt.TenantID, &evt.AggregateID, &evt.AggregateType, &evt.Sequence,
			&evt.StreamPosition, &evt.Type, &payloadBytes, &metaBytes, &occurredAt, &xidText,
		); err != nil {
			return nil, "", 0, time.Time{}, fmt.Errorf("projection %q: scan event: %w", projectorName, err)
		}
		if err := json.Unmarshal(metaBytes, &evt.Metadata); err != nil {
			return nil, "", 0, time.Time{}, fmt.Errorf("projection %q: unmarshal metadata (event %s): %w",
				projectorName, evt.ID, err)
		}
		evt.Payload = json.RawMessage(payloadBytes)
		evt.OccurredAt = occurredAt

		batch = append(batch, evt)
		lastXID = xidText // ascending insert_xid order: the last row is the batch maximum
		if evt.StreamPosition > maxPos {
			maxPos = evt.StreamPosition
		}
		if occurredAt.After(maxOccurred) {
			maxOccurred = occurredAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, time.Time{}, fmt.Errorf("projection %q: iterate batch: %w", projectorName, err)
	}
	return batch, lastXID, maxPos, maxOccurred, nil
}

// advanceProgress writes the new watermark. It requires exactly one row to change
// (the projector's seeded projection_progress row); zero rows means the row is
// missing, which is a wiring error worth surfacing rather than silently ignoring.
func advanceProgress(ctx context.Context, tx pgx.Tx, projectorName, lastXID string, maxPos int64, maxOccurred time.Time) error {
	ct, err := tx.Exec(ctx, advanceProgressSQL, projectorName, lastXID, maxPos, maxOccurred)
	if err != nil {
		return fmt.Errorf("projection %q: advance watermark: %w", projectorName, err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("projection %q: advance watermark changed %d rows, want 1 (missing projection_progress row?)",
			projectorName, ct.RowsAffected())
	}
	return nil
}

// readLastEventAt reads the stored last_event_at for the empty-batch lag sample. A
// NULL (never-consumed projector) yields the zero time, which Run reads as "no
// sample" and does not record.
func readLastEventAt(ctx context.Context, tx pgx.Tx, projectorName string) (time.Time, error) {
	// Scan into *time.Time so a SQL NULL maps to a nil pointer rather than erroring.
	var ts *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT last_event_at FROM projection_progress WHERE projector_name = $1`, projectorName,
	).Scan(&ts); err != nil {
		return time.Time{}, fmt.Errorf("projection %q: read last_event_at: %w", projectorName, err)
	}
	if ts == nil {
		return time.Time{}, nil
	}
	return *ts, nil
}
