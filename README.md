# OpenGate

OpenGate is a self-hostable, multi-tenant access control SaaS. The backend is written in Go with Postgres-only persistence. Project planning artifacts (PRD, PFD, system architecture, system design, database schema, implementation plan) live in `docs/planning/`; epics and user stories live in `docs/tracking/`.

## Code generation

Typed query code under `internal/adapters/outbound/postgres/db/` is generated from the SQL in `internal/adapters/outbound/postgres/queries/` by [`sqlc`](https://sqlc.dev) (pinned as a Go `tool` directive in `go.mod`). The generated code is committed, so CI builds it directly and does not require `sqlc`. Re-run `make generate` whenever a query — or a migration a query reads its schema from — changes, then commit the result. `make generate-check` regenerates and fails on any diff; it is kept out of `make ci` so a missing `sqlc` binary cannot break the main pipeline.
