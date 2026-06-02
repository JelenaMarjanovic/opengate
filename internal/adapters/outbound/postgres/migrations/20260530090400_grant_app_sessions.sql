-- +goose Up
-- +goose StatementBegin
-- opengate_app operates the authenticated session paths under RLS:
--   UPDATE — the sliding-window refresh of last_seen_at/expires_at on each
--            validated request (US-02.03 AC-3).
--   DELETE — explicit logout removes the session row (US-02.03 AC-4).
-- INSERT is deliberately NOT granted: sessions are minted only by the
--   pre-authentication login path, which runs on the bypass pool. Withholding
--   INSERT from the application role means authenticated code cannot forge a
--   session. SELECT is not granted yet either: US-02.03 reads sessions only
--   on the bypass pool (the by-token lookup, which has no tenant context).
--   Both accrete in a later migration when an authenticated session-management
--   feature needs them.
GRANT UPDATE, DELETE ON sessions TO opengate_app;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- opengate_app does not own sessions, so these explicit table grants do not drop
-- with the role and the table is not dropped here; revoke exactly what Up granted
-- so the round-trip leaves no lingering privilege.
REVOKE UPDATE, DELETE ON sessions FROM opengate_app;
-- +goose StatementEnd
