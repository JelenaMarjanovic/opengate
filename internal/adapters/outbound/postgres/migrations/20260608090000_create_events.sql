-- +goose Up
-- +goose StatementBegin
-- The append-only event store (Database Schema §6.1). Every state change in the
-- system is recorded here as an immutable, tenant-scoped event; the read-model
-- projections (US-03.x) are rebuilt from this log.
CREATE TABLE events (
    id                  uuid PRIMARY KEY,             -- UUIDv7, app-generated
    tenant_id           uuid NOT NULL REFERENCES tenants(id),
    aggregate_id        uuid NOT NULL,
    aggregate_type      text NOT NULL,
    sequence            bigint NOT NULL,              -- 1-based per aggregate
    stream_position     bigint NOT NULL,             -- global, monotonic
    event_type          text NOT NULL,                -- e.g. "member.created.v1"
    payload             jsonb NOT NULL,
    metadata            jsonb NOT NULL,
    occurred_at         timestamptz NOT NULL,

    -- Optimistic-concurrency guard: two appends racing on the same aggregate
    -- version collide here, so the loser retries instead of forking the stream.
    CONSTRAINT events_aggregate_sequence_unique
        UNIQUE (aggregate_id, sequence),
    -- The global ordering key is unique across the whole store, giving
    -- projectors a gap-free, replayable cursor.
    CONSTRAINT events_stream_position_unique
        UNIQUE (stream_position),
    CONSTRAINT events_sequence_positive
        CHECK (sequence > 0)
);

-- stream_position is deliberately NOT a column DEFAULT nextval(...). The
-- application reads nextval() BEFORE the insert so the assigned position can be
-- carried in the event's metadata to downstream consumers (Database Schema
-- §6.1). The sequence is therefore an independent object; the column is a bare
-- bigint the adapter populates at append time (US-03.03). Do not wire a default.
CREATE SEQUENCE events_stream_position_seq AS bigint;

-- Tenant-scoped reverse-chronological scans back the audit and activity feeds.
CREATE INDEX events_tenant_occurred_at_idx
    ON events (tenant_id, occurred_at DESC);
-- The same, narrowed to a single event type (e.g. door-open history).
CREATE INDEX events_tenant_type_occurred_at_idx
    ON events (tenant_id, event_type, occurred_at DESC);
-- Projector cursor scans walk the store in global order.
CREATE INDEX events_stream_position_idx
    ON events (stream_position);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Dropping the table also drops its constraints and indexes. The sequence is an
-- independent object (no column default ties it to the table), so it is dropped
-- explicitly.
DROP TABLE IF EXISTS events;
DROP SEQUENCE IF EXISTS events_stream_position_seq;
-- +goose StatementEnd
