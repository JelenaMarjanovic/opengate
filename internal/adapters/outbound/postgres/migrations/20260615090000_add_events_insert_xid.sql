-- +goose Up
-- +goose StatementBegin
-- US-03.05: snapshot-boundary consumption (Option 3b) for the async read-model
-- projector framework. A naive stream_position watermark is unsafe: stream_position
-- is assigned by nextval() INSIDE the append transaction, so concurrent appends on
-- different aggregates can COMMIT out of order; a projector that consumed a higher
-- position before a lower one committed would skip the lower event forever. Instead
-- each event records its inserting transaction id, and the projector consumes only
-- transactions that are no longer in flight, ordered and watermarked by that id.

-- insert_xid is the transaction that inserted the row. xid8 is the modern 64-bit,
-- wraparound-safe transaction id (PostgreSQL 16; the pg_*_xact_id family, NOT the
-- deprecated txid_*). pg_current_xact_id() is VOLATILE, so this ADD COLUMN ...
-- DEFAULT REWRITES the events table: every existing row receives the migration's
-- own xid, which sorts before any future append -- correct, since they are
-- already-committed history. The rewrite is acceptable only because the table is
-- small/empty at migration time; a large existing table would instead need a phased
-- backfill (add nullable, backfill in batches, set NOT NULL), which is out of scope.
ALTER TABLE events
    ADD COLUMN insert_xid xid8 NOT NULL DEFAULT pg_current_xact_id();

-- Serves the projector's boundary range scan (insert_xid > watermark AND insert_xid
-- < snapshot xmin) and its ORDER BY insert_xid, stream_position. Cross-tenant (no
-- tenant_id), matching projection_progress's no-RLS, single-pass-across-all-tenants
-- consumption.
CREATE INDEX events_insert_xid_idx
    ON events (insert_xid, stream_position);

-- last_consumed_xid is the AUTHORITATIVE consumption watermark: the projector reads
-- strictly above it and advances it to the maximum insert_xid it has consumed.
-- last_position (added in create_projection_progress) is hereby REDEFINED as a
-- NON-authoritative progress indicator (the highest stream_position applied so far);
-- under out-of-order consumption it may briefly exceed an unconsumed lower position,
-- so it must never be used as a consumption boundary. last_event_at stays the lag
-- source. The default '0'::xid8 sorts below every real transaction id, so a
-- never-run projector consumes from the beginning of the log.
ALTER TABLE projection_progress
    ADD COLUMN last_consumed_xid xid8 NOT NULL DEFAULT '0'::xid8;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Drop the index explicitly before the column (dropping the column would cascade to
-- it anyway; being explicit keeps the Down readable and order-independent).
DROP INDEX events_insert_xid_idx;
ALTER TABLE events DROP COLUMN insert_xid;
ALTER TABLE projection_progress DROP COLUMN last_consumed_xid;
-- +goose StatementEnd
