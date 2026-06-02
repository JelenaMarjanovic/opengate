-- +goose Up
-- +goose StatementBegin
CREATE TABLE sessions (
    id                  uuid PRIMARY KEY,
    tenant_id           uuid NOT NULL REFERENCES tenants(id),
    user_id             uuid NOT NULL REFERENCES users(id),
    token_hash          bytea NOT NULL,  -- SHA-256 of session token
    role                text NOT NULL,   -- snapshotted at issue time
    issued_at           timestamptz NOT NULL DEFAULT now(),
    last_seen_at        timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,
    issued_from_ip      inet,
    user_agent          text,

    CONSTRAINT sessions_token_hash_unique UNIQUE (token_hash),
    CONSTRAINT sessions_role_check
        CHECK (role IN ('owner', 'manager', 'auditor'))
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_user_id_idx ON sessions (tenant_id, user_id);

-- Bypass-role grants (US-02.03, Step 1, Task 3). The pre-authentication session
-- paths run on the BYPASSRLS operator pool because the tenant is not yet bound
-- when they execute:
--   SELECT  session-by-token lookup (resolve a bearer token to its session)
--   INSERT  login (persist a freshly issued session)
--   UPDATE  sliding-window refresh of last_seen_at / expires_at
--   DELETE  logout, and later the expired-session cleanup job
-- opengate_app is intentionally NOT granted here; the regular request pool's
-- sessions grants are wired alongside that pool in Step 2, keeping each role's
-- grants next to the code that uses them.
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO opengate_bypass;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Dropping the table also drops its indexes, constraints, and the table-level
-- grants above, so no explicit REVOKE is needed.
DROP TABLE IF EXISTS sessions;
-- +goose StatementEnd
