-- +goose Up
-- +goose StatementBegin
-- Per-projector checkpoint of how far each read-model projector has consumed the
-- global event stream (Database Schema §6.2). This is a deployment-wide table:
-- it carries NO tenant_id and NO row-level security, because a projector runs a
-- single pass across ALL tenants' events and advances one cursor. Scoping it per
-- tenant would defeat the single-pass design.
CREATE TABLE projection_progress (
    projector_name      text PRIMARY KEY,
    last_position       bigint NOT NULL DEFAULT 0,
    last_event_at       timestamptz,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- Seed the five known v1 projectors at position 0 so each has a row to advance
-- from on first run; an absent row would force every projector to special-case
-- its own bootstrap. New projectors add their row in their own migration.
INSERT INTO projection_progress (projector_name) VALUES
    ('audit_log'),
    ('door_status'),
    ('subscription_delivery'),
    ('export_status'),
    ('reader_connectivity');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS projection_progress;
-- +goose StatementEnd
