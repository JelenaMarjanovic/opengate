-- name: LoadAggregateEvents :many
-- Backs EventStore.Load (US-03.03, AC3): every event for one aggregate in
-- sequence order. Runs on the RLS-bound pool (opengate_app) -- the command path
-- loads an aggregate's history before deciding the next command, with the tenant
-- already bound, so RLS scopes the read to the caller's tenant. ORDER BY sequence
-- gives the deterministic 1-based replay order the aggregate rebuild depends on,
-- served by the events_aggregate_sequence_unique index (aggregate_id, sequence).
SELECT id, tenant_id, aggregate_id, aggregate_type, sequence, stream_position,
       event_type, payload, metadata, occurred_at
FROM events
WHERE aggregate_id = $1
ORDER BY sequence;

-- name: ReadEventsAfterPosition :many
-- Backs EventStore.ReadAfterPosition: events whose stream_position is strictly
-- greater than a cursor, in global (monotonic) order, capped by a limit. Runs on
-- the BYPASSRLS pool (opengate_bypass) -- projector workers walk the whole store
-- across tenants from a saved cursor, so no tenant is bound. The strict `>` plus
-- ORDER BY stream_position makes the scan gap-free and resumable, served by the
-- events_stream_position_idx index. The LIMIT is the projector's page size; sqlc
-- names it Limit, which the adapter fills from the port's `limit int`.
SELECT id, tenant_id, aggregate_id, aggregate_type, sequence, stream_position,
       event_type, payload, metadata, occurred_at
FROM events
WHERE stream_position > $1
ORDER BY stream_position
LIMIT $2;

-- name: ReadTenantEventsInTimeRange :many
-- Backs EventStore.ReadByTenantAndTimeRange: one tenant's events within an
-- INCLUSIVE [from, to] time window (System Design §2), in deterministic
-- chronological order. Runs on the BYPASSRLS pool (opengate_bypass) -- the export
-- capability names the tenant explicitly via sqlc.arg(tenant_id) rather than
-- taking it from RLS context. Both bounds are inclusive (occurred_at >= from_time
-- AND occurred_at <= to_time). The bounds use named args (from_time/to_time)
-- because the column occurred_at is referenced twice: positional params would
-- emit an OccurredAt/OccurredAt_2 field pair, whereas the named args yield clean,
-- self-documenting FromTime/ToTime fields (from/to alone are reserved SQL words).
-- The (occurred_at, stream_position) sort breaks ties on identical timestamps so
-- the export is reproducible, served by the events_tenant_occurred_at_idx index.
SELECT id, tenant_id, aggregate_id, aggregate_type, sequence, stream_position,
       event_type, payload, metadata, occurred_at
FROM events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND occurred_at >= sqlc.arg(from_time)
  AND occurred_at <= sqlc.arg(to_time)
ORDER BY occurred_at, stream_position;
