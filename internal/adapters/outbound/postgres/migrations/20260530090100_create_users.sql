-- +goose Up
-- +goose StatementBegin
CREATE TABLE users (
    id                   uuid PRIMARY KEY,
    tenant_id            uuid NOT NULL REFERENCES tenants(id),
    email                text NOT NULL,
    password_hash        text NOT NULL,  -- Argon2id, PHC-formatted
    role                 text NOT NULL,
    status               text NOT NULL DEFAULT 'active',
    last_login_at        timestamptz,
    must_change_password boolean NOT NULL DEFAULT false,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_role_check   CHECK (role   IN ('owner', 'manager', 'auditor')),
    CONSTRAINT users_status_check CHECK (status IN ('active', 'deactivated')),
    CONSTRAINT users_email_per_tenant_unique UNIQUE (tenant_id, email)
);
CREATE INDEX users_tenant_status_idx ON users (tenant_id, status);
GRANT SELECT, INSERT ON users TO opengate_bypass;  -- bootstrap inserts the first owner
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS users_tenant_status_idx;
DROP TABLE IF EXISTS users;  -- grants on users drop with the table
-- +goose StatementEnd
