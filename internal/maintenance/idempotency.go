package maintenance

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/coordination/advisory"
)

// idempotencyRetention is how long an idempotency key survives after it is written
// (System Design §4: ten minutes, sized to the dashboard's longest reasonable retry
// budget). It is a single Go constant rather than an interval literal repeated in
// each DELETE, so the two tables can never drift apart on the window they enforce.
const idempotencyRetention = 10 * time.Minute

// idempotencyLockName is the advisory-lock key for this job, in the "job.<name>"
// form System Design §5 reserves for singleton maintenance jobs (as opposed to
// "projector.<name>" for projectors). Every worker instance derives the same
// identifier from this string, which is what makes the lock deployment-wide.
const idempotencyLockName = "job.cleanup_idempotency_keys"

// The purge statements, one per table. The retention boundary is computed by
// POSTGRES -- now() minus an interval built from the seconds this process passes in
// -- not by the worker. A worker whose clock has drifted therefore cannot widen or
// narrow the window: every instance cuts at the database's own idea of "now". Within
// one transaction now() is the transaction's start time, so both tables are cut at
// exactly the same instant.
//
// The comparison is strict "<", so a row whose created_at lands exactly on the
// boundary SURVIVES this tick and is deleted by the next one. The bias is toward
// keeping a key one tick too long (a redundant replay protection) rather than
// dropping it one instant too early (a lost one).
//
// The statements are separate constants instead of one string with the table name
// interpolated: the table names are then literals in the source, so no code path
// exists that could build this DELETE from anything but a compile-time constant.
const (
	deleteExpiredCommandKeysSQL = `
DELETE FROM command_idempotency_keys
WHERE created_at < now() - make_interval(secs => $1)`

	deleteExpiredDecisionKeysSQL = `
DELETE FROM decision_idempotency_keys
WHERE created_at < now() - make_interval(secs => $1)`
)

// purgedTable pairs a table with the statement that purges it, so the job iterates
// one list instead of repeating the Exec-and-count block per table.
type purgedTable struct {
	name      string
	deleteSQL string
}

// purgedTables enumerates EXACTLY the idempotency tables subject to the ten-minute
// retention.
//
// reconciliation_idempotency_keys is deliberately absent and must stay absent. Its
// retention is the full event-store retention: each row protects an event that is
// itself never purged (System Design §4), so deleting a key here would let a reader
// replaying its buffer double-apply an access event whose original is still in the
// store. The bypass role holds no DELETE on that table precisely so that a future
// edit adding it to this list fails loudly with a permission error instead of
// silently destroying an audit trail.
var purgedTables = []purgedTable{
	{name: "command_idempotency_keys", deleteSQL: deleteExpiredCommandKeysSQL},
	{name: "decision_idempotency_keys", deleteSQL: deleteExpiredDecisionKeysSQL},
}

// TableResult reports how many rows one purge statement deleted.
type TableResult struct {
	// Table is the purged table's name -- also the value of the metric's "table" label.
	Table string
	// Deleted is the number of rows this iteration removed from it.
	Deleted int64
}

// Result is the outcome of one CleanupIdempotencyKeys iteration.
type Result struct {
	// Skipped is true when another instance held the advisory lock, so this
	// iteration did no work. It is NOT an error: see CleanupIdempotencyKeys.
	Skipped bool
	// Tables carries one entry per purged table, in purgedTables order. It is nil
	// when Skipped is true.
	Tables []TableResult
}

// CleanupIdempotencyKeys runs ONE retention iteration in ONE transaction on the
// bypass (BYPASSRLS, no tenant binding) pool, deleting every command and decision
// idempotency key older than idempotencyRetention.
//
// It runs on the bypass pool because it is deliberately NOT tenant-scoped: one tick
// purges expired keys for every tenant. On the RLS-bound pool the tenant_isolation
// policy would restrict the DELETE to the single bound tenant, so a deployment-wide
// retention job would silently degrade into a per-request one. BYPASSRLS exempts the
// role from row-level security but NOT from table privileges, hence the explicit
// DELETE grants in 20260615091200_grant_idempotency_keys.
//
// The iteration is a deployment-wide singleton via a transaction-scoped advisory
// lock: if another instance already holds it, this call SKIPS -- the deferred
// rollback discards the empty transaction -- and returns (Result{Skipped: true},
// nil). A contended tick must never surface as a job error, because on a busy
// deployment contention is the normal state, and failing the job would turn it into
// a retry storm against a lock that is doing its job.
//
// Both DELETEs run inside the lock and inside the same transaction, so a tick is
// all-or-nothing across the two tables.
//
// Sizing note: the delete is unbounded (no LIMIT, no ctid batching) on purpose. With
// a ten-minute retention and a five-minute tick, one iteration removes at most about
// fifteen minutes of accumulated keys, and the created_at index makes that a range
// scan. If command volume ever grows enough for one tick's delete to hold locks
// longer than is comfortable, the upgrade path is a batched delete (a LIMITed ctid
// sub-select looped until it deletes fewer rows than the batch size) -- the same way
// the projector framework documents its composite-cursor upgrade for the batch
// boundary.
//
// On a successful commit, and ONLY when the lock was acquired, the per-table row
// counts are published to the delete counter.
func CleanupIdempotencyKeys(ctx context.Context, pool *pgxpool.Pool, metrics *Metrics) (Result, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("cleanup idempotency keys: begin: %w", err)
	}
	// No-op once the tx commits; on the not-acquired and error paths it discards the
	// transaction (and releases the advisory lock if one was taken).
	defer func() { _ = tx.Rollback(ctx) }()

	// Seconds (not the Duration) because make_interval's secs argument is a double
	// precision; the constant is the single source of the window either way.
	retentionSeconds := idempotencyRetention.Seconds()

	tables := make([]TableResult, 0, len(purgedTables))
	acquired, err := advisory.TryWithLock(ctx, tx, idempotencyLockName, func() error {
		for _, t := range purgedTables {
			tag, execErr := tx.Exec(ctx, t.deleteSQL, retentionSeconds)
			if execErr != nil {
				return fmt.Errorf("cleanup idempotency keys: delete from %s: %w", t.name, execErr)
			}
			tables = append(tables, TableResult{Table: t.name, Deleted: tag.RowsAffected()})
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if !acquired {
		// Another instance is purging right now; skip this iteration cleanly.
		return Result{Skipped: true}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("cleanup idempotency keys: commit: %w", err)
	}

	// Publish only after the durable commit: a counter incremented for rows a
	// rolled-back transaction "deleted" would overstate the purge permanently.
	if metrics != nil {
		for _, t := range tables {
			metrics.RecordDeleted(t.Table, t.Deleted)
		}
	}
	return Result{Tables: tables}, nil
}
