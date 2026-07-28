# OpenGate

Multi-tenant access control for fitness venues. Event-sourced Go service on Postgres, self-hostable, shipped as one binary with four operational modes.

**Status: platform substrate complete, domain features not started.** The first three epics are done — 17 of 54 live user stories. Authentication, tenant isolation, the event store, the projection framework, the job queue and command idempotency work end to end. Members, credentials, doors, access policies, the access-decision path and the admin dashboard are specified but not built. Last substantive work: July 2026. See [Status](#status).

---

## The problem

Small fitness venues — independent gyms, CrossFit boxes, climbing gyms, martial arts schools — grant door access from membership state that changes constantly. Subscriptions lapse mid-month, cards get lost and reissued, class passes expire, staff roles change.

The system deciding whether a door opens has to answer three questions: who is at the door, are they entitled to be there right now, and — weeks later, during a dispute or an audit — exactly what happened and in what order.

OpenGate treats the third question as the design constraint rather than a reporting feature. Every state change is an immutable event; current state is a projection over that log. Audit completeness is structural, not something bolted on afterwards.

## Why this repository exists

OpenGate is a portfolio artifact, and the documentation says so rather than pretending otherwise. It has no users and no commercial ambition. The domain was chosen because the author spent roughly eight years building physical access control, ticketing and camera integration systems — including a stadium deployment for a Serbian first-league football club and an ALPR parking system — so the design constraints in the architecture documents come from field experience rather than inference.

Full framing, including what was deliberately left out: [`docs/planning/opengate-prd-v1.md`](docs/planning/opengate-prd-v1.md).

## Status

| Epic | Scope | Done |
|---|---|---|
| E1 | Project bootstrap, tooling, compose stack, CI | 6/6 |
| E2 | Tenants, users, sessions, authn, Casbin authz, RLS | 5/5 |
| E3 | Event store, projections, River queue, advisory locks, idempotency | 6/6 |
| E4 | Member and credential aggregates | 0/4 |
| E5–E14 | Policies, doors, access decision, SSE push, offline reconciliation, audit queries, webhooks, export, simulator, dashboard | 0/33 |

Counts come from [the implementation plan](docs/planning/opengate-implementation-plan-v1.md), which is the source of truth for scope. It carries 55 story headers but 54 live stories: US-02.06 was absorbed into US-02.05 in the v1.1 reconciliation — connection-level tenant binding and the RLS policies it drives are one jointly-verifiable capability — and its header is retained only for traceability. `docs/tracking/opengate-stories.csv` predates that reconciliation and is stale.

### Why it stopped here

The epic order was infrastructure-first by design, not by accident — E1 through E3 build the substrate that every later epic depends on, and the implementation plan says so in the business-value field of each epic, written before any code existed. Members, doors and policies are CRUD over that substrate; they exercise no pattern the event store and projection framework do not already demonstrate.

So the work stopped at the point where the remaining epics stopped teaching the reader anything new, and the author's time went elsewhere. That is a resourcing decision, not an architectural one. The remaining 37 stories have written acceptance criteria and no known blockers.

### What runs today

Subcommands: `migrate`, `bootstrap`, `api`, `worker`.

| Method | Path | Auth |
|---|---|---|
| `GET` | `/livez` | none |
| `GET` | `/readyz` | none |
| `POST` | `/api/v1/tenants/{tenant}/auth/login` | none |
| `POST` | `/api/v1/auth/logout` | session cookie |
| `GET` | `/api/v1/auth/whoami` | session cookie |

The worker registers one job: `cleanup.idempotency_keys`, a five-minute periodic purge enforcing the ten-minute idempotency retention window, serialized deployment-wide by advisory lock.

The command idempotency middleware is implemented and tested but is deliberately wired to no route. It guards mutating command endpoints, and the first of those arrives with E4.

### What is specified but not implemented

Member and credential command handlers, access policies and time-window evaluation, the synchronous access-decision path, SSE push to readers, offline reconciliation, audit queries with keyset pagination, HMAC-signed webhooks, signed tenant data export, the reader simulator, and the Next.js admin dashboard. Each has written acceptance criteria in [the implementation plan](docs/planning/opengate-implementation-plan-v1.md).

## Quickstart

Requires Go 1.26+, Docker and Docker Compose.

```bash
git clone https://github.com/JelenaMarjanovic/opengate.git
cd opengate
cp .env.example .env          # set POSTGRES_PASSWORD and GRAFANA_ADMIN_PASSWORD
```

Postgres is not published to the host by default. Uncomment the `ports` block under the `postgres` service in `docker-compose.yml`, then:

```bash
docker compose up -d postgres
make build
```

Migrations run as the superuser, because they create roles:

```bash
export OPENGATE_DATABASE_URL='postgres://opengate:<POSTGRES_PASSWORD>@localhost:5432/opengate?sslmode=disable'
./bin/opengate migrate up
```

This creates two login roles: `opengate_app` (RLS-bound, used for request traffic) and `opengate_bypass` (`BYPASSRLS`, used only for pre-auth lookups and bootstrap). Both are created with the password `placeholder` for local development; change them with `ALTER ROLE` anywhere else.

Create the first tenant and owner:

```bash
export BYPASS_RLS_DATABASE_URL='postgres://opengate_bypass:placeholder@localhost:5432/opengate?sslmode=disable'
export OPENGATE_BOOTSTRAP_TENANT_NAME='Iron Works Gym'
export OPENGATE_BOOTSTRAP_TENANT_SLUG='iron-works-gym'   # optional; derived from the name when unset
export OPENGATE_BOOTSTRAP_OWNER_EMAIL='owner@example.com'
export OPENGATE_BOOTSTRAP_OWNER_PASSWORD='<pick one>'
./bin/opengate bootstrap
```

Serve the API:

```bash
export DATABASE_URL='postgres://opengate_app:placeholder@localhost:5432/opengate?sslmode=disable'
export COOKIE_SECURE=false     # local HTTP only; never in a deployment
./bin/opengate api

curl -s localhost:8080/livez
curl -s -c jar -X POST localhost:8080/api/v1/tenants/iron-works-gym/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"owner@example.com","password":"<the one you picked>"}'
curl -s -b jar localhost:8080/api/v1/auth/whoami
```

Run the queue consumer in a second shell. It works jobs across all tenants, so it runs as `opengate_bypass` and needs only that DSN — no `DATABASE_URL`:

```bash
export BYPASS_RLS_DATABASE_URL='postgres://opengate_bypass:placeholder@localhost:5432/opengate?sslmode=disable'
./bin/opengate worker
```

### Configuration

| Variable | Default | Read by |
|---|---|---|
| `LOG_LEVEL` | `INFO` | all |
| `OPENGATE_DATABASE_URL` | — | `migrate` |
| `BYPASS_RLS_DATABASE_URL` | — | `bootstrap`, `api`, `worker` |
| `DATABASE_URL` | — | `api` |
| `HTTP_ADDR` | `:8080` | `api` |
| `COOKIE_SECURE` | `true` | `api` |
| `AUTHZ_REFRESH_INTERVAL` | `30s` | `api` |

The full stack — Postgres, OpenTelemetry Collector, Tempo, Prometheus, Grafana, Caddy — comes up with `docker compose up -d`. Caddy is remapped to host ports 8080/8443, which collides with the `api` subcommand's default `HTTP_ADDR`; set `HTTP_ADDR` to something else when running both. Caddy does not yet proxy the API — that route arrives with the dashboard epic.

## Architecture

Hexagonal, with the domain holding no knowledge of transport or persistence.

```
cmd/opengate/                    subcommands: migrate, bootstrap, api, worker
internal/
  domain/                        tenant, user, slug, event types — no I/O
  application/                   use cases: auth, identity bootstrap
  ports/inbound|outbound/        interfaces the application depends on
  adapters/
    inbound/http/                chi router, session middleware, Casbin gate,
                                 idempotency middleware, RFC 9457
    outbound/postgres/           sqlc queries, RLS-bound pool, event store,
                                 idempotency store, goose migrations
    outbound/authz/              Casbin model and policy-refreshing authorizer
    outbound/queue/              River client, tx-scoped enqueue, worker,
                                 projection and maintenance jobs
    outbound/inmemory/           in-memory event store for tests
  projection/                    projector framework, runner, lag metrics
  maintenance/                   retention jobs, each an advisory-lock singleton
  coordination/advisory/         Postgres advisory-lock primitive
  auth/                          Argon2id hashing and verification
  tenant/                        request-scoped tenant context
  apperr/                        sentinel error taxonomy
  observability/                 slog JSON logger, W3C trace propagation
  testsupport/                   shared testcontainers Postgres startup
deploy/                          Caddy, OTel Collector, Prometheus, Tempo, Grafana config
```

Design documents, in narrowing order of abstraction: [PRD](docs/planning/opengate-prd-v1.md) → [PFD](docs/planning/opengate-pfd-v1.md) → [System Architecture](docs/planning/opengate-system-architecture-v1.md) → [System Design](docs/planning/opengate-system-design-v1.md) → [Database Schema](docs/planning/opengate-database-schema-v1.md) → [Implementation Plan](docs/planning/opengate-implementation-plan-v1.md).

## Worth reviewing

If you have ten minutes and want to judge the engineering rather than the feature count:

- **Tenant isolation is enforced twice.** Postgres row-level security policies plus application-layer tenant binding, with tests that verify each layer independently — [`migrations/20260607093000_enable_rls_tenant_isolation.sql`](internal/adapters/outbound/postgres/migrations/20260607093000_enable_rls_tenant_isolation.sql), [`events_rls_test.go`](internal/adapters/outbound/postgres/events_rls_test.go).
- **Three DSNs separated by privilege.** Superuser for migrations, `BYPASSRLS` for pre-auth lookups only, RLS-bound for everything else. The reasoning is in [`internal/config/config.go`](internal/config/config.go).
- **Event store with optimistic concurrency control** — [`event_store.go`](internal/adapters/outbound/postgres/event_store.go), contract-tested against both the Postgres and in-memory adapters via [`eventstorecontract`](internal/adapters/outbound/eventstorecontract/contract.go).
- **Trace context survives the queue boundary.** W3C context is injected into River job metadata at enqueue and extracted in the worker, so a trace spans the HTTP request and the async job — [`queue/`](internal/adapters/outbound/queue/), [`observability/propagation.go`](internal/observability/propagation.go).
- **Projections consume at snapshot boundaries** rather than by naive offset, using `xid` ordering to avoid the in-flight-transaction gap — [`projection/projector.go`](internal/projection/projector.go).
- **Retried commands replay a cached response, not just a duplicate-detection error.** The `Idempotency-Key` middleware keys on tenant, actor and a hash of the request body, and restores the stored response through an allow-list of headers — so a `Set-Cookie` minted for one request can never be replayed onto another ten minutes later — [`idempotency.go`](internal/adapters/inbound/http/idempotency.go). Retention is purged by a periodic job that cuts at the database's `now()` rather than the worker's clock, and that deliberately never touches `reconciliation_idempotency_keys`, whose rows guard events the store never deletes — [`maintenance/idempotency.go`](internal/maintenance/idempotency.go).
- **Casbin policy is migration-managed and refreshed on an interval**, enforced as middleware at the HTTP boundary rather than scattered through handlers — [`authz_middleware.go`](internal/adapters/inbound/http/authz_middleware.go).
- **Secrets are asserted absent from logs**, not merely omitted by convention — see the session-token log assertion in [`cmd/opengate/auth_api_test.go`](cmd/opengate/auth_api_test.go).
- **Client IP is derived from the trusted-proxy hop**, not from the leftmost `X-Forwarded-For` value — [`client_ip_test.go`](internal/adapters/inbound/http/client_ip_test.go).

Four sprint retrospectives, including what was cut and why, are in [`docs/tracking/`](docs/tracking/).

## Testing

```bash
make test        # unit + integration, with coverage profile
make ci          # fmt-check, vet, lint, test, build
```

Integration tests start real Postgres containers via testcontainers, so Docker must be running. Container startup is centralized in [`internal/testsupport/postgres.go`](internal/testsupport/postgres.go).

## Development

`make help` lists all targets. Static analysis is golangci-lint (pinned, project-local under `./bin`); git hooks are managed by lefthook via `make hooks-install`.

Typed query code under `internal/adapters/outbound/postgres/db/` is generated by [sqlc](https://sqlc.dev) from the SQL in `queries/` and is committed, so CI never needs the sqlc binary. Re-run `make generate` after changing a query or a migration a query reads its schema from, then commit the result. `make generate-check` fails on drift and is kept out of `make ci` deliberately, so a missing sqlc binary cannot break the main pipeline.

## License

Apache 2.0 (target — see PRD §1).
