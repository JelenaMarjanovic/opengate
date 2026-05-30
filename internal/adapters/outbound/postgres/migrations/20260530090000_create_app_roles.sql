-- +goose Up
-- +goose StatementBegin
-- App roles per DB §13. Idempotent DO blocks so the migration is safe if a role was
-- created out-of-band. Placeholder passwords are replaced out-of-band in production
-- (ALTER ROLE ... PASSWORD ...); in dev/test the password is literally 'placeholder'
-- (ephemeral containers). The migration runner must be superuser/CREATEROLE.
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'opengate_app') THEN
        CREATE ROLE opengate_app WITH LOGIN PASSWORD 'placeholder';
    END IF;
END $$;
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'opengate_bypass') THEN
        CREATE ROLE opengate_bypass WITH LOGIN BYPASSRLS PASSWORD 'placeholder';
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO opengate_app, opengate_bypass;

-- §13 grants only schema USAGE; the bootstrap (opengate_bypass) needs table privileges.
-- tenants already exists, so grant here; the users grant is in the users migration.
GRANT SELECT, INSERT ON tenants TO opengate_bypass;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- DROP OWNED BY removes the role's granted privileges so DROP ROLE does not fail on
-- dependent grants (the roles own no objects). Guarded for idempotent round-trip.
DO $$ BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'opengate_bypass') THEN
        DROP OWNED BY opengate_bypass; DROP ROLE opengate_bypass;
    END IF;
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'opengate_app') THEN
        DROP OWNED BY opengate_app; DROP ROLE opengate_app;
    END IF;
END $$;
-- +goose StatementEnd
