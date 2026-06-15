-- +goose Up
-- +goose StatementBegin
-- The three idempotency cache tables (Database Schema §7). They de-duplicate
-- retried work on three independent paths, so they share the §7 grouping but are
-- otherwise unrelated tables created together because one story introduces them.
--
-- Each carries a tenant_id and gets tenant_isolation RLS in the next migration
-- (the reconciliation table denormalizes tenant_id precisely so it can carry that
-- policy; its functional key is (reader_id, sequence_no)). RLS and the role
-- grants are split into their own migrations, mirroring the events table's
-- create/enable-rls/grant split (20260608090000..0300).

-- command_idempotency_keys caches the HTTP response of a completed command so a
-- retried request with the same Idempotency-Key replays the stored response
-- instead of re-executing the command (US-03.06, command middleware). request_hash
-- is the SHA-256 over the authenticated actor id and the request body, letting the
-- middleware detect a key reused with a different payload AND a key replayed by a
-- different actor inside the same tenant. created_at is indexed for the cleanup
-- job's range delete (DELETE ... WHERE created_at < ...).
--
-- response_headers stores the whitelisted response headers (Content-Type,
-- Location, ETag) so a replay reproduces an EQUIVALENT response rather than only
-- the status and body: without them net/http sniffs the content type of the
-- replayed body, and a JSON body sniffs as text/plain. The shape mirrors
-- http.Header — a JSON object of header name to array of values, e.g.
-- {"Content-Type":["application/json"]}. The whitelist is applied in the
-- middleware before the write, so unnamed headers (Set-Cookie above all) never
-- reach this column. Only the command table gets it: the decision path is an
-- in-transaction use-case concern (E4), not an HTTP response cache.
CREATE TABLE command_idempotency_keys (
    tenant_id           uuid NOT NULL,
    idempotency_key     text NOT NULL,
    request_hash        bytea NOT NULL,    -- SHA-256 of actor id || request body
    response_status     int NOT NULL,
    response_headers    jsonb NOT NULL DEFAULT '{}'::jsonb,
    response_body       bytea NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);
CREATE INDEX command_idempotency_keys_created_at_idx
    ON command_idempotency_keys (created_at);

-- decision_idempotency_keys caches an access-decision outcome so a replayed
-- decision request returns the identical grant/deny verdict. It is written
-- in-transaction by the AccessDecision use case (System Design §16), not by an
-- HTTP middleware, so its application grants are deferred to E4; the table is
-- created now because it is schema and the cleanup job (commit 3) purges it.
-- created_at is indexed for that range delete.
CREATE TABLE decision_idempotency_keys (
    tenant_id           uuid NOT NULL,
    idempotency_key     text NOT NULL,
    decision            text NOT NULL,
    reason_code         text NOT NULL,
    response_body       bytea NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key),
    CONSTRAINT decision_idempotency_keys_decision_check
        CHECK (decision IN ('grant', 'deny'))
);
CREATE INDEX decision_idempotency_keys_created_at_idx
    ON decision_idempotency_keys (created_at);

-- reconciliation_idempotency_keys de-duplicates offline reader reconciliation
-- events keyed by (reader_id, sequence_no) so a reader replaying its buffer after
-- reconnecting cannot double-apply an event (E6 consumer). It references events(id)
-- because each accepted reconciliation maps to one stored event. Unlike the other
-- two it is NEVER purged (the cleanup job does not touch it), so it has no
-- created_at index; the tenant_id index backs cross-tenant audit, the functional
-- lookup being the (reader_id, sequence_no) primary key.
CREATE TABLE reconciliation_idempotency_keys (
    tenant_id           uuid NOT NULL,
    reader_id           uuid NOT NULL,
    sequence_no         bigint NOT NULL,
    event_id            uuid NOT NULL REFERENCES events(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (reader_id, sequence_no),
    CONSTRAINT reconciliation_sequence_positive CHECK (sequence_no > 0)
);
CREATE INDEX reconciliation_idempotency_keys_tenant_idx
    ON reconciliation_idempotency_keys (tenant_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Dropping each table also drops its indexes and constraints. Order is immaterial
-- among the three (no inter-table FKs); the FK from reconciliation to events is to
-- an earlier migration's table, which outlives this one on the way down.
DROP TABLE IF EXISTS reconciliation_idempotency_keys;
DROP TABLE IF EXISTS decision_idempotency_keys;
DROP TABLE IF EXISTS command_idempotency_keys;
-- +goose StatementEnd
