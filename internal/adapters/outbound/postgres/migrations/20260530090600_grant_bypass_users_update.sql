-- +goose Up
-- +goose StatementBegin
-- The login flow's rehash-on-login and last-login writes run on the bypass pool
-- and update users; the role already holds SELECT (for the WHERE predicate) and
-- INSERT (bootstrap), so only UPDATE is missing.
GRANT UPDATE ON users TO opengate_bypass;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
REVOKE UPDATE ON users FROM opengate_bypass;
-- +goose StatementEnd
