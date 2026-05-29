-- +goose Up
-- +goose StatementBegin
CREATE TABLE tenants (
    id                  uuid PRIMARY KEY,
    name                text NOT NULL,
    contact_email       text,
    timezone            text NOT NULL DEFAULT 'UTC',
    session_timeout     interval NOT NULL DEFAULT '60 minutes',
    status              text NOT NULL DEFAULT 'active',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT tenants_status_check
        CHECK (status IN ('active', 'suspended'))
);

CREATE INDEX tenants_status_idx ON tenants (status);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS tenants_status_idx;
DROP TABLE IF EXISTS tenants;
-- +goose StatementEnd
