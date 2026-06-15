// Package projection is the async read-model projector framework. A Projector
// folds the global event log into a read model; the generic runner (Run) executes
// one projector iteration in one transaction as a deployment-wide singleton.
//
// Consumption uses a transaction-id snapshot boundary (Option 3b), not a
// stream_position watermark. stream_position is assigned by nextval() inside the
// append transaction, so concurrent appends on different aggregates can commit out
// of order; a position-watermarked projector that consumed a higher position before
// a lower one committed would skip the lower event forever. Instead every event
// records its inserting transaction id (events.insert_xid), and the runner consumes
// only events from transactions that are no longer in flight -- insert_xid below
// pg_snapshot_xmin(pg_current_snapshot()) -- ordered and watermarked by insert_xid
// (projection_progress.last_consumed_xid). This keeps the write path fully
// concurrent yet loses no event.
//
// The runner runs on the BYPASSRLS pool (projectors read across all tenants and
// write the no-RLS projection_progress) and is wired in production as a River
// periodic job; see internal/adapters/outbound/queue. The framework itself has no
// River dependency so the runner is testable directly.
package projection
