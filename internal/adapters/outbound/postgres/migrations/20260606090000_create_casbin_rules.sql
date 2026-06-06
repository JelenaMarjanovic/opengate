-- +goose Up
-- +goose StatementBegin
-- casbin_rules is Casbin's default-adapter policy table, NOT OpenGate domain
-- design: the column shape (a serial id plus ptype and v0..v5) is exactly what
-- the step-2 enforcer's adapter expects to read, so it is kept verbatim. The
-- serial id is deliberate over a uuid -- Casbin's default adapter keys on integer
-- ids, and a custom uuid adapter would be unjustified complexity for no
-- OpenGate-specific gain.
--
-- The table is GLOBAL: the role -> (resource, action) mapping is identical for
-- every tenant, so it carries no tenant_id and is deliberately left out of RLS
-- (US-02.05). Every tenant-bound connection must be able to read it to make an
-- authorization decision. Per Database Schema 5.6 the policy is
-- migration-managed and version-controlled with the code, not mutated at runtime;
-- this migration seeds the complete v1 rule set below.
CREATE TABLE casbin_rules (
    id      serial PRIMARY KEY,
    ptype   varchar(10) NOT NULL,
    v0      varchar(256),
    v1      varchar(256),
    v2      varchar(256),
    v3      varchar(256),
    v4      varchar(256),
    v5      varchar(256)
);

CREATE INDEX casbin_rules_ptype_idx ON casbin_rules (ptype);

-- The model is: subject (role), object (resource type), action.
INSERT INTO casbin_rules (ptype, v0, v1, v2) VALUES
    ('p', 'owner',   '*',           '*'),
    ('p', 'manager', 'members',     'read'),
    ('p', 'manager', 'members',     'write'),
    ('p', 'manager', 'credentials', 'read'),
    ('p', 'manager', 'credentials', 'write'),
    ('p', 'manager', 'policies',    'read'),
    ('p', 'manager', 'policies',    'write'),
    ('p', 'manager', 'doors',       'read'),
    ('p', 'manager', 'doors',       'write'),
    ('p', 'manager', 'audit',       'read'),
    ('p', 'manager', 'subscriptions', 'read'),
    ('p', 'manager', 'subscriptions', 'write'),
    ('p', 'auditor', 'members',     'read'),
    ('p', 'auditor', 'credentials', 'read'),
    ('p', 'auditor', 'policies',    'read'),
    ('p', 'auditor', 'doors',       'read'),
    ('p', 'auditor', 'audit',       'read');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- DROP TABLE removes the casbin_rules_ptype_idx index with it, so the Up is fully
-- reversed and a full Down leaves no casbin_rules table behind.
DROP TABLE casbin_rules;
-- +goose StatementEnd
