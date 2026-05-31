# OpenGate — Implementation Plan

**Version:** 1.1
**Status:** Draft for review
**Document type:** Implementation plan (epics, user stories, sprint plan, risk register)
**Author:** Jelena Marjanović
**Date:** May 2026
**Predecessor documents:** opengate-prd-v1.md, opengate-pfd-v1.md, opengate-system-architecture-v1.md, opengate-system-design-v1.md, opengate-database-schema-v1.md
**Successor document:** None at the planning layer; the next artifact is the codebase itself.

---

## 1. How to read this document

This document is the final document in the OpenGate planning layer. The five documents above this one specified what the system must be, what its functional capabilities are, what components realize those capabilities, what implementation patterns govern those components, and what the database schema looks like in concrete SQL. The present document closes the planning gap by decomposing the work into the units that the implementation phase will execute: epics, user stories, tasks where appropriate, sprints, and the connecting tissue of estimates, dependencies, and risks.

The document follows the strict Agile/Scrum convention as it is practiced in mature engineering organizations that use Jira, Linear, ClickUp, or similar tracking systems. The choice of strict format was made deliberately at the start of this document's drafting; the alternative pragmatic format with prose-style stories was considered and rejected on the grounds that the portfolio context benefits from demonstrating that the author can produce industry-standard Agile artifacts in their canonical shapes. A reviewer reading this document recognizes the structure immediately and can drop the stories into any backlog tool with minimal rework. The strict format also imposes useful discipline on the author: each story must have a clear "As a / I want / So that" formulation, each acceptance criterion must be expressible in Given-When-Then form, and each story must be small enough to be estimable at the Fibonacci scale used here.

The document is organized into three parts. Part One establishes the process foundations: the sprint cadence, the Definition of Ready, the Definition of Done, the story format and INVEST validation, the estimation methodology, and the acceptance criteria conventions. Part Two is the epic catalog, with fourteen epics that together cover the entire scope established by the preceding documents; each epic decomposes into a set of user stories with full details. Part Three contains the sprint plan that sequences the stories across the twelve-sprint, sixty-day project window, the dependency matrix that captures the constraints between stories, the risk register that enumerates the known threats to delivery, and the closing sections on deferred decisions and document status.

The audience for this document is the engineer who will execute the implementation, which in the immediate term is the author herself. The document is also intended to be readable by a future reviewer evaluating the OpenGate portfolio repository who wants to verify that the implementation effort was planned rigorously rather than improvised. The format and the level of detail are calibrated to both audiences: detailed enough that the author can pick up any story and execute it without re-reading the design documents, and structured enough that the reviewer can scan the artifact and recognize the discipline behind it.

A note on what this document is not. This is not a fixed-scope contract; the plan can and will be adjusted as the implementation proceeds, with story estimates revised in light of actual velocity and stories added or removed based on what the work reveals. The Agile philosophy under which the plan is written treats the plan as a hypothesis, not a commitment. The artifacts in this document are therefore living artifacts that the implementation will validate and refine. Any change to a story after the implementation has begun is captured as a story update with a timestamp; the document's version history reflects the evolution.

---

## 2. Sprint cadence and project window

The project window is sixty calendar days, beginning on the first day of implementation and ending sixty days later. The window is divided into twelve sprints of five working days each, with no holidays or planned absences in the schedule. The sprint length of five working days (one calendar week) is shorter than the conventional two-week sprint, which is a deliberate choice for a solo-developer project: the shorter cadence forces more frequent reflection on progress, reduces the cost of mid-sprint scope adjustments, and produces a finer-grained record of velocity for retrospective analysis.

The sprint cadence within each five-day week follows a standardized rhythm. Sprint planning is performed at the start of day one and consists of pulling stories from the backlog up to the sprint's capacity, performing a final INVEST check on the pulled stories, and confirming that each story's dependencies are satisfied. Daily check-in is performed at the start of each subsequent day and consists of a self-review of yesterday's progress against the sprint goal. The sprint review is performed at the end of day five and consists of demonstrating the completed stories against their acceptance criteria. The sprint retrospective immediately follows the review and produces written notes on what went well, what did not, and what to adjust in the next sprint.

The implementation effort budget assumes approximately six productive hours per working day, after accounting for documentation updates, tooling friction, and the cognitive cost of context switching across the project's many concerns. With five working days per sprint and twelve sprints in the window, the total budget is approximately three hundred and sixty productive hours.

The story point capacity per sprint is calibrated from the budget. Using the conventional translation that one story point represents approximately three to four hours of productive work for a story of moderate complexity, a single-developer sprint with thirty productive hours has a capacity of approximately eight to ten story points. The plan assumes a baseline capacity of nine story points per sprint, with the understanding that early sprints may carry less while the project's mechanics are being established and later sprints may carry more as familiarity grows. The total project capacity over twelve sprints is therefore approximately one hundred and eight story points. The catalog in Part Two assigns one hundred and four story points across the planned stories, leaving four points of slack for unforeseen work or for stories whose estimates revise upward in execution.

---

## 3. Definition of Ready

The Definition of Ready (DoR) specifies the criteria a story must satisfy before it can be pulled into a sprint. A story that does not meet the DoR is not yet ready for implementation and must be refined before it can be planned. The OpenGate DoR is composed of seven explicit checks that the author applies during sprint planning to each candidate story.

The first check is that the story follows the canonical format. The story must have a stable identifier, a title that summarizes the work in one phrase, a description in the "As a [role], I want [capability], so that [benefit]" form, a non-trivial description paragraph that elaborates on the intent, and at least three acceptance criteria written in the Given-When-Then form.

The second check is that the story is sized within the team's capacity. The story must be estimated at no more than eight story points; a story estimated at thirteen or more is too large to fit into a single sprint with margin and must be split. A story estimated at zero or unsized must be estimated before sprint pull.

The third check is that the story's dependencies are satisfied or are themselves in the same sprint. A story that depends on another story not yet completed cannot be pulled unless the dependency is also being pulled in the same sprint and is scheduled earlier within the sprint. A story whose dependency chain extends across multiple sprints is broken into smaller stories until the dependency chain fits.

The fourth check is that the story is INVEST-compliant. INVEST is the acronym for Independent, Negotiable, Valuable, Estimable, Small, Testable; each story must pass each criterion. The author records the INVEST check explicitly on each story; this record is part of the story's metadata and appears in the story format used throughout Part Two.

The fifth check is that the acceptance criteria are testable. Each Given-When-Then criterion must be expressible as a concrete test, whether unit, integration, or end-to-end, that the implementation produces and that passes when the story is complete. A criterion that cannot be tested is rewritten or replaced.

The sixth check is that the technical notes are sufficient. The author must be able to identify which sections of the design documents the story implements without further research. The technical notes section of each story includes pointers to the relevant sections of the PRD, PFD, System Architecture, System Design, and Database Schema documents.

The seventh check is that the story is connected to a concrete business outcome. The "So that..." clause must articulate a benefit that traces back to one of the goals in PRD section three or to one of the six use case scenarios in PRD section five. Stories that fail this check are evaluated for removal from the backlog.

---

## 4. Definition of Done

The Definition of Done (DoD) specifies the criteria a story must satisfy before it can be considered complete and removed from the active sprint. The DoD is uniform across all stories in OpenGate; individual stories may add story-specific completion checks in their acceptance criteria, but the baseline DoD applies to every story without exception.

The first DoD criterion is that all acceptance criteria from the story are demonstrated as passing. Each Given-When-Then statement is exercised either by an automated test that passes in CI or by a manual verification recorded in the sprint review notes. A criterion that cannot be demonstrated is not marked done.

The second criterion is that the code follows the conventions established in the design documents. The hexagonal architecture discipline from System Design section seven is observed, port interfaces are not bypassed, the multi-tenant filter is present on every tenant-scoped query, and the naming conventions from Database Schema section two are followed. Code review against these conventions is performed by the author against herself, with the explicit discipline of reading the diff once after a brief break before merge.

The third criterion is that automated tests are present and pass. The test types required depend on the story: command handlers require unit tests with in-memory adapters and integration tests against the Postgres adapter, query handlers require both, projector workers require integration tests, and end-to-end-relevant stories require an end-to-end test in the e2e suite. The exact test coverage per story is specified in the story's acceptance criteria.

The fourth criterion is that the code passes the static analysis pipeline. The pipeline includes `go vet`, `staticcheck`, `gosec`, `golangci-lint` with the project's configured set of linters, and the contract test pass that verifies port interface conformance between in-memory and Postgres adapters where both exist.

The fifth criterion is that any new API endpoints are documented in the OpenAPI specification. Endpoints that are not in the spec are not considered done; the spec is the contract that both the dashboard and any external consumer relies on, and missing documentation is a defect.

The sixth criterion is that observability instrumentation is present. Any new use case, projector, or job emits spans following the naming conventions from System Design section eight, with the OpenGate-specific attributes appropriate to its operation. Any new metric emitted by the story is named according to the conventions in the same section.

The seventh criterion is that the relevant documentation is updated. README excerpts, architecture documentation, OpenAPI specification, and observability dashboard configurations that the story affects are updated in the same commit as the code changes. Documentation drift is not acceptable.

The eighth criterion is that the code is merged into the main branch. A story whose code lives on a feature branch is not done; the merge to main is the final step. The merge follows a fast-forward or squash strategy as appropriate; the merge commit message references the story identifier.

---

## 5. Story format and INVEST validation

Each user story in Part Two follows a uniform format with the following sections.

The story identifier follows the convention `US-EE.NN`, where `EE` is the epic number from one to fourteen and `NN` is the story sequence number within the epic. Identifiers are not reused even if a story is removed; a removed story's identifier remains as a gap in the sequence to preserve referential stability.

The story title is a short phrase, typically four to eight words, that captures the work in a form recognizable in a backlog list view.

The story narrative follows the canonical "As a [role], I want [capability], so that [benefit]" format. The role is drawn from the project's identified personas: the administrator (owner or manager), the auditor, the operator, the engineer (the implementer herself), and occasionally the external integrator (the developer of a webhook subscriber).

The description elaborates on the intent of the story in two to four sentences of prose. The description connects the story to the design documents and articulates the specific outcome the story produces.

The acceptance criteria are listed as numbered Gherkin scenarios in the Given-When-Then form. Each criterion is concrete and testable; vague criteria such as "the feature works" are not permitted.

The story points estimate uses the Fibonacci scale of one, two, three, five, eight, with thirteen reserved as a marker that the story is too large and must be split. The estimate is calibrated relative to a reference story (US-01.01, the initial project scaffolding, is calibrated at two points and serves as the anchor).

The dependencies list other story identifiers that must be complete before this story can begin. A story with no dependencies has the entry "none".

The technical notes section identifies the sections of the design documents that the story implements and any specific implementation considerations not captured in the acceptance criteria.

The INVEST validation is a brief assertion that each of the six INVEST criteria is satisfied: Independent (the story does not have hidden dependencies beyond those listed), Negotiable (the story's scope can be discussed during planning if needed), Valuable (the "So that..." clause traces to a real benefit), Estimable (the story is well-enough understood to assign a point estimate), Small (the story fits within a single sprint with margin), and Testable (each acceptance criterion is a concrete test). A story that fails any INVEST check is refined until it passes.

---

## 6. Estimation methodology

Story points in OpenGate use the modified Fibonacci scale with values one, two, three, five, and eight. Thirteen and higher are not used because a story at that estimate is too large to fit a sprint with margin and must be split before sprint pull. Half-points and fractional values are not permitted; estimates are integers from the allowed set.

The reference story for calibration is US-01.01, "Initialize Go module and hexagonal project layout", which is estimated at two story points. The story consists of creating the project directory structure, initializing the Go module, adding the standard development tools (linters, formatters, build configuration), and verifying that the empty project compiles. The work is straightforward but not trivial; a developer familiar with Go can complete it in about six productive hours. Any story estimated at two points should require approximately the same effort.

A story of one point is half the size of the reference: roughly three productive hours, suitable for narrow changes such as adding a single new query to an existing read model. A story of three points is approximately half again as large as the reference: roughly nine productive hours, suitable for a moderately complex change. A story of five points is approximately eighteen productive hours, suitable for a fully new use case with non-trivial domain logic and accompanying tests. A story of eight points is approximately thirty productive hours, the upper bound that fits a sprint with no margin; the plan avoids eight-point stories where possible because they leave no room for unexpected complexity.

The estimates in Part Two were produced by the author through a two-pass process. The first pass assigned estimates based on intuition from prior experience. The second pass reviewed the estimates against the reference story, looking for outliers where the estimate seemed inconsistent with the work being asked. Several estimates were revised in the second pass; the document reflects the post-revision values.

The estimates are explicitly stated as forecasts rather than commitments. The Agile principle that estimates can be wrong is taken seriously: a story whose actual effort substantially exceeds its estimate during execution is logged for retrospective review, and patterns observed across multiple over-estimated or under-estimated stories inform future estimation calibration.

---

## 7. Acceptance criteria conventions

Each acceptance criterion is a single Gherkin Given-When-Then scenario. The convention is described here once and is applied uniformly throughout Part Two.

The "Given" clause establishes the precondition for the test. It identifies the state of the system before the action under test is performed. Typical Given clauses include "Given a tenant with an active administrator session", "Given a member with status `active` and an assigned policy covering the door", and "Given the access decision idempotency cache contains no entry for the request".

The "When" clause describes the action under test. The action is a concrete operation: a specific API call, a CLI invocation, a sequence of events arriving at a projector, the elapse of a configured duration. Vague actions such as "When the user does something" are not permitted; the action must be reproducible by a test author from the description alone.

The "Then" clause states the expected outcome. The outcome is observable: a specific HTTP response status, a specific database state, a specific metric value, a specific log line. Multiple outcomes can be combined with "And" clauses; an acceptance criterion with several "And" outcomes remains a single scenario because all the outcomes are produced by the same action under the same precondition.

Where the Gherkin Given-When-Then form is awkward for a particular criterion, the form is preserved by reformulating the criterion until it fits. The discipline of preserving the form across all criteria is itself valuable because it forces the author to be explicit about the precondition, the action, and the expected outcome, eliminating ambiguity that prose criteria often hide.

The criteria are numbered within each story, with the numbering restarting at one for each story. References to specific criteria from other documents or from sprint review notes use the form `US-EE.NN/AC-K`, where K is the criterion number.

---

# Part II — Epic catalog

This part of the document enumerates the fourteen epics that together cover the full scope of the OpenGate v1 implementation. Each epic is presented with a goal statement, a business value statement, the epic-level acceptance criteria, and the list of user stories that constitute the epic. The user stories follow the canonical format established in section five of this document.

The fourteen epics, with their identifiers and titles, are E1 Project Bootstrap and Foundations, E2 Tenant and Identity Domain, E3 Event Store and Projection Infrastructure, E4 Member and Credential Domain, E5 Access Policy and Door Domain, E6 Access Authorization Decision Path, E7 Reader Operations and SSE Push, E8 Offline Reconciliation, E9 Audit Log and Compliance Queries, E10 Webhook Subscriptions and Delivery, E11 Tenant Data Export, E12 Reader Simulator, E13 Admin Dashboard Frontend, and E14 Demo, Documentation, and Handover. The sequence of epics approximates the implementation order, though several epics overlap in time as captured in the sprint plan in Part Three.

---

## 8. Epic E1: Project Bootstrap and Foundations

### Epic goal

Establish the project's foundational structure, tooling, and infrastructure such that all subsequent work proceeds against a stable platform. The epic encompasses the Go module layout following hexagonal architecture conventions, the build and test tooling, the static analysis pipeline, the Docker Compose configuration that includes Postgres and the observability stack, the goose migration framework integration, the OpenAPI scaffolding, and the initial CI workflow.

### Business value

A solid foundation reduces friction for every subsequent story and prevents accidental architectural drift. The work done here is invisible in the final product but determines whether the project remains coherent over the sixty-day window or accumulates technical debt that compromises the portfolio signal.

### Epic-level acceptance

The epic is complete when a fresh clone of the repository can be brought up with a single `make` command, the test suite executes against the running stack, and the observability dashboards render correctly in the bundled Grafana.

### User stories

**US-01.01: Initialize Go module and hexagonal project layout**

_Format:_ As the implementer, I want a properly structured Go module with the hexagonal architecture layout, so that all subsequent code follows clean architecture principles from day one.

_Description:_ Create the Go module with the appropriate module path, establish the `internal/domain`, `internal/application`, `internal/ports`, `internal/adapters/inbound`, `internal/adapters/outbound`, and `cmd/opengate` directories with placeholder files, and verify that the empty project compiles. Add the `go.mod` and `go.sum` files. The work establishes the package boundaries that all subsequent stories rely on.

_Acceptance Criteria:_

1. Given a fresh clone, When `go build ./...` is run, Then the build succeeds with no errors.
2. Given the project root, When `tree internal/` is run, Then the output shows the hexagonal layout with `domain`, `application`, `ports`, and `adapters` directories.
3. Given the project root, When `go mod verify` is run, Then the output reports the module's dependencies as verified.

_Story Points:_ 2 (reference story)

_Dependencies:_ none

_Technical Notes:_ The layout is specified in System Design section seven. The module path is `github.com/JelenaMarjanovic/opengate`.

_INVEST:_ Independent (no dependencies). Negotiable (layout details can be adjusted in planning). Valuable (foundation for all subsequent work). Estimable (calibrated as the reference). Small (fits in two points). Testable (build commands serve as tests).

---

**US-01.02: Configure development tooling and static analysis**

_Format:_ As the implementer, I want the linting and static analysis tools configured at the project level, so that code quality is enforced automatically rather than relying on manual review.

_Description:_ Add configuration files for `golangci-lint` with the project's selected linter set (including `gosec`, `staticcheck`, `errcheck`, `revive`, `gofmt`, and the standard set), set up `pre-commit` hooks that run `gofmt` and `go vet` on staged files, and add a `Makefile` with targets for `lint`, `test`, and `build`. The tooling becomes part of the Definition of Done from this point forward.

_Acceptance Criteria:_

1. Given the project root, When `make lint` is run, Then golangci-lint executes successfully and reports no issues on the initial codebase.
2. Given a deliberately malformed Go file, When `pre-commit` runs on staged changes, Then the malformed file fails the formatting check and the commit is rejected.
3. Given the Makefile, When `make help` is run, Then the output lists `lint`, `test`, `build`, and other available targets with brief descriptions.

_Story Points:_ 2

_Dependencies:_ US-01.01

_Technical Notes:_ The full golangci-lint configuration includes the linter selection appropriate for a hexagonal Go project. The pre-commit framework is the standard `pre-commit/pre-commit` tool.

_INVEST:_ All criteria satisfied.

---

**US-01.03: Set up Docker Compose stack with Postgres and observability**

_Format:_ As the implementer, I want a Docker Compose configuration that brings up Postgres, the OTel collector, Tempo, Prometheus, and Grafana, so that local development and demo runs use the same environment.

_Description:_ Create a `docker-compose.yml` file with services for `postgres`, `otel-collector`, `tempo`, `prometheus`, `grafana`, and `caddy` (the reverse proxy). Configure named volumes for the data-bearing services. Configure the OTel collector to receive OTLP gRPC on port 4317 and to forward traces to Tempo and metrics to Prometheus. Configure Grafana with the Tempo and Prometheus data sources auto-provisioned.

_Acceptance Criteria:_

1. Given a fresh checkout, When `docker compose up -d` is run, Then all containers reach the running state within sixty seconds.
2. Given the running stack, When a browser visits the Grafana URL, Then the Grafana home page loads and the Tempo and Prometheus data sources are listed as connected.
3. Given the running stack, When the Postgres health check is queried, Then it reports healthy and accepts connections on port 5432 from within the Docker network.

_Story Points:_ 5

_Dependencies:_ US-01.01

_Technical Notes:_ The deployment topology is specified in System Architecture section seven. The Caddy configuration handles TLS termination and routes `/api/`, `/grafana/`, and `/` paths.

_INVEST:_ All criteria satisfied.

---

**US-01.04: Integrate goose migration framework with embedded files**

_Format:_ As the implementer, I want goose integrated as a subcommand of the OpenGate binary with migration files embedded via `go:embed`, so that schema migrations ship with the binary and require no external files at deployment time.

_Description:_ Add the `pressly/goose/v3` dependency, create the `cmd/opengate/migrate.go` file with the subcommand wrapper, configure `go:embed` to include the migrations directory, and add the first migration file that creates the `opengate_app` and `opengate_bypass` roles per Database Schema section thirteen.

_Acceptance Criteria:_

1. Given a fresh database, When `opengate migrate up` is run, Then the goose_db_version table is created and the role migration is applied.
2. Given an applied migration, When `opengate migrate down` is run, Then the role migration is reversed and the database returns to its pre-migration state.
3. Given the binary, When the binary is built without the migrations directory in the filesystem, Then `opengate migrate status` still reports the embedded migrations as available.

_Story Points:_ 3

_Dependencies:_ US-01.01

_Technical Notes:_ The goose integration pattern is specified in Database Schema section three. The embedded filesystem uses Go's standard `embed` package.

Reality (v1.1): US-01.04 as executed delivered the goose mechanism and `go:embed` only. The first content migration was the `tenants` DDL (Database Schema §5.1), not the role-creation migration named in the description and AC-1/AC-2; the `opengate_app`/`opengate_bypass` role migration (`create_app_roles`) landed in US-02.01. The full Sprint-1 migration sequence, tooling, and pin record is authoritative in the Sprint 1 retrospective.

_INVEST:_ All criteria satisfied.

---

**US-01.05: Set up CI workflow with build, lint, and test**

_Format:_ As the implementer, I want a CI workflow that runs build, lint, and test on every push, so that regressions are caught automatically before they reach the main branch.

_Description:_ Add a GitHub Actions workflow (assuming the repository is hosted on GitHub for portfolio purposes) that runs on every push and pull request. The workflow installs Go, runs `make lint`, runs `make test` against a Postgres service container, and uploads test coverage reports. The workflow is the substrate for the Definition of Done criterion that requires code to pass automated checks.

_Acceptance Criteria:_

1. Given a push to a branch, When the CI workflow runs, Then the build, lint, and test stages all complete successfully on the initial codebase.
2. Given a deliberate test failure introduced in a test branch, When the CI workflow runs, Then the workflow fails and the failure annotation identifies the failing test.
3. Given a successful workflow run, When the coverage report is inspected, Then it reports the test coverage percentage across the codebase.

_Story Points:_ 3

_Dependencies:_ US-01.01, US-01.02

_Technical Notes:_ The workflow uses the `services` keyword to bring up a Postgres container for integration tests. Go version follows the latest stable release as specified in the PRD.

_INVEST:_ All criteria satisfied.

---

**US-01.06: Configure structured logging and error wrapping conventions**

_Format:_ As the implementer, I want `log/slog`-based structured logging and consistent error wrapping in place from the start, so that observability and error handling are uniform across the codebase.

_Description:_ Configure the standard library `log/slog` logger to emit JSON-formatted records to stdout at the level configured by the `LOG_LEVEL` environment variable. Establish the convention that all errors returned from adapters wrap their underlying cause via `fmt.Errorf("%w", err)`, and that all errors returned from use cases are domain sentinels. Add a small logging helper that extracts the trace identifier and tenant identifier from the request context and includes them in log records.

_Acceptance Criteria:_

1. Given a log statement at info level, When the application runs with `LOG_LEVEL=INFO`, Then the log record appears in stdout as a JSON object with timestamp, level, and message fields.
2. Given a wrapped error chain three levels deep, When the chain is inspected with `errors.Is` against a sentinel, Then the sentinel is correctly identified.
3. Given a request with a tenant context and an active span, When a log statement is emitted within that context, Then the log record includes the tenant identifier and the trace identifier.

_Story Points:_ 2

_Dependencies:_ US-01.01

_Technical Notes:_ The logging conventions are specified in System Design section eight. The error wrapping conventions are specified in System Design section seven.

_INVEST:_ All criteria satisfied.

**Epic E1 total: 17 story points.**

---

## 9. Epic E2: Tenant and Identity Domain

### Epic goal

Implement the foundational identity capability that underlies every other capability in the system: tenants, administrative users, dashboard sessions, authentication, Casbin-based authorization, and the bootstrap CLI subcommand that creates the first tenant and the first owner user.

### Business value

Without identity, no other capability can be exercised. Every command in the system requires an authenticated session, every authorization decision requires a Casbin policy check, and every multi-tenant isolation property requires a tenant context. The epic establishes these prerequisites and exercises the multi-tenant isolation and role-based access control patterns from PRD section four.

### Epic-level acceptance

The epic is complete when an operator can bootstrap a new tenant via CLI, the owner can log in to the dashboard (whose stub renders the home page), the session is validated on subsequent requests, and the Casbin authorization gate rejects requests that exceed the session's role.

### User stories

**US-02.01: Implement tenant aggregate and bootstrap CLI**

_Format:_ As an operator, I want to bootstrap a new OpenGate tenant via the CLI with the first owner user, so that the deployment is usable immediately after migration without requiring a self-service signup flow that this version does not provide.

_Description:_ Create the `tenants` and `users` table migrations per Database Schema sections 5.1 and 5.2, implement the tenant and user aggregates per System Design section eleven, and add the `bootstrap` subcommand to the OpenGate binary. The subcommand reads `OPENGATE_BOOTSTRAP_TENANT_NAME`, `OPENGATE_BOOTSTRAP_OWNER_EMAIL`, and `OPENGATE_BOOTSTRAP_OWNER_PASSWORD` from the environment and creates the records in a single transaction.

_Acceptance Criteria:_

1. Given the environment variables are set, When `opengate bootstrap` is run, Then a tenant row and a user row are inserted into Postgres atomically.
2. Given a missing environment variable, When `opengate bootstrap` is run, Then the command exits with a non-zero status and a clear error message identifying the missing variable.
3. Given an existing tenant with the same name, When `opengate bootstrap` is run, Then the command exits with a non-zero status without modifying the database.

_Story Points:_ 3

_Dependencies:_ US-01.04

_Technical Notes:_ The bootstrap uses the `opengate_bypass` role to circumvent RLS. The password is hashed with Argon2id using the parameters from System Design section nine.

_INVEST:_ All criteria satisfied.

---

**US-02.02: Implement password hashing with Argon2id**

_Format:_ As the implementer, I want a reusable password hashing module that uses Argon2id with the OWASP-recommended parameters, so that user passwords are stored securely and the parameters can evolve over time without breaking existing hashes.

_Description:_ Create `internal/auth/password.go` with `HashPassword(plaintext string) (string, error)` and `VerifyPassword(plaintext, phc string) (rehash bool, err error)` functions. The hash output is PHC-formatted including the parameters used to produce it. The verify function returns a rehash flag indicating that the stored hash uses outdated parameters and should be replaced on successful verification.

_Acceptance Criteria:_

1. Given a plaintext password, When `HashPassword` is called, Then it returns a PHC-formatted string of the form `$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>`.
2. Given a valid PHC hash and the correct plaintext, When `VerifyPassword` is called, Then it returns no error and the rehash flag is false.
3. Given a valid PHC hash with outdated parameters, When `VerifyPassword` is called with the correct plaintext, Then it returns no error and the rehash flag is true.
4. Given a valid PHC hash and a wrong plaintext, When `VerifyPassword` is called, Then it returns an error that satisfies `errors.Is(err, ErrPasswordMismatch)`.

_Story Points:_ 2

_Dependencies:_ US-01.01

_Technical Notes:_ The Argon2id parameters are specified in System Design section nine. The implementation uses `golang.org/x/crypto/argon2`. Realized scope (v1.1): `HashPassword` was delivered in US-02.01 (decision D1), so US-02.02 as executed reduced to specifying and verifying `VerifyPassword`, the PHC round-trip, and the malformed-hash guards. The 2-point estimate reflects the original combined scope.

_INVEST:_ All criteria satisfied.

---

**US-02.03: Implement session creation, validation, and cleanup**

_Format:_ As an administrator, I want to log in to the dashboard and have my session validated on subsequent requests, so that I do not have to re-authenticate on every action and so that my session expires after inactivity for security.

_Description:_ Create the `sessions` table migration per Database Schema section 5.3 and implement the session use case that handles login, session lookup, session refresh on activity, and explicit logout. Add the session middleware that intercepts inbound HTTP requests, extracts the session cookie, looks up the session in the database, and either populates the request context or returns 401 Unauthorized.

_Acceptance Criteria:_

1. Given valid credentials for an active user, When POST `/api/v1/auth/login` is called, Then the response is 200 OK with a Set-Cookie header containing the session token, and a row is inserted into the sessions table.
2. Given an expired session cookie, When any authenticated endpoint is called, Then the response is 401 Unauthorized and the session row's last_seen_at is not updated.
3. Given a valid active session, When any authenticated endpoint is called, Then the response is processed normally and the session row's last_seen_at is updated to the current time.
4. Given an active session, When POST `/api/v1/auth/logout` is called, Then the response is 204 No Content and the session row is deleted.

_Story Points:_ 5

_Dependencies:_ US-02.01, US-02.02

_Technical Notes:_ Session token generation, hashing, and cookie attributes are specified in System Design section nine. The session middleware is part of the chi middleware stack.

Scope (v1.1): US-02.03 delivers the `sessions` table, the login/logout/refresh use case, and the session middleware only; the connection pool and RLS policies are US-02.05. The two pre-authentication identity lookups — user-by-email at login, and session-by-token on every request — execute on the BYPASSRLS pool, because they must resolve identity across a not-yet-known tenant boundary and because US-02.05 forces RLS on `users` and `sessions` (a context-less query against an RLS-forced table returns zero rows, so a naive lookup would pass in this sprint and then break when US-02.05 lands). Open decision, deferred to this story's articulation: how login resolves the tenant, given that `users.email` is unique per tenant (`UNIQUE (tenant_id, email)`) and the `/api/v1/auth/login` endpoint carries no tenant in its path. Candidate mechanisms: tenant identifier in the request body, host/subdomain resolution, single-tenant-per-deployment, or globally-unique email. The chosen mechanism is recorded in System Design section nine.

_INVEST:_ All criteria satisfied.

---

**US-02.04: Implement Casbin authorization adapter and middleware**

_Format:_ As a tenant owner, I want role-based authorization enforced on every API endpoint, so that a manager cannot perform owner-only operations and an auditor cannot perform write operations.

_Description:_ Create the `casbin_rules` table migration per Database Schema section 5.6 with the seed rules for owner, manager, and auditor roles. Implement the Casbin authorizer adapter that loads the policy from the database at startup, caches it in memory, refreshes it every thirty seconds, and exposes an `Enforce(role, resource, action)` method. Add the authorization middleware that runs after the session middleware and checks the requested operation against the authenticated session's role.

_Acceptance Criteria:_

1. Given an auditor-role session, When PUT `/api/v1/tenants/{t}/members/{m}` is called, Then the response is 403 Forbidden with a Problem Details body.
2. Given an owner-role session, When the same endpoint is called with valid payload, Then the response is processed normally.
3. Given a Casbin policy change applied via a migration, When the authorizer's refresh interval elapses, Then the new policy is in effect without requiring application restart.

_Story Points:_ 5

_Dependencies:_ US-02.03

_Technical Notes:_ The Casbin adapter pattern is specified in System Design section eleven. The model file (`config/rbac_model.conf`) is part of the migration's deliverables.

_INVEST:_ All criteria satisfied.

---

**US-02.05: Implement connection pool with tenant binding, RLS policies, and dual-layer verification**

_Format:_ As the implementer, I want the regular Postgres pool to bind `app.current_tenant_id` from the request context on every checkout, and Row-Level Security enabled and forced on every tenant-scoped table existing so far, so that tenant isolation is enforced by two independent layers from the moment identity data is first written.

_Description:_ Create `internal/adapters/outbound/postgres/pool.go` with the pgx pool `AfterAcquire`/`BeforeRelease` hooks per System Design section ten; the `AfterAcquire` hook logs a warning when no tenant is in context. Add the BYPASSRLS pool variant for operator paths (bootstrap, export) and the pre-authentication identity lookups (see US-02.03). In the same story, add the migration that enables and forces RLS and creates the `tenant_isolation` policies on `tenants`, `users`, and `sessions` per Database Schema section thirteen, and add the dual-layer verification test. The pool code lands before the RLS migration, per the migrations-follow-code discipline. This story absorbs former US-02.06.

_Acceptance Criteria:_

1.  Given two tenants each with one user seeded via the BYPASSRLS pool, When a connection bound to tenant A queries `users`, Then only tenant A's user is returned.
2.  Given a connection with no tenant in context, When `tenants`, `users`, or `sessions` is queried, Then zero rows are returned and the application logs a warning identifying the missing tenant context.
3.  Given a raw query that omits the `WHERE tenant_id` predicate on a connection bound to tenant A, When it is executed against `users`, Then only tenant A's rows are returned, confirming the RLS layer is independent of the application-layer filter.
4.  Given the BYPASSRLS pool, When the same query is issued, Then rows from both tenants are returned, confirming the protection comes from RLS specifically and not from another accident.
5.  Given the migration is rolled back, When `pg_policies` is queried, Then no `tenant_isolation` policies remain on `tenants`, `users`, or `sessions`.

_Story Points:_ 5

_Dependencies:_ US-02.01, US-02.03, US-01.04

_Technical Notes:_ The pool configuration and the dual-layer verification test are specified in System Design section ten; the policy definitions are in Database Schema section thirteen. The dependency on US-02.03 is new in v1.1: this story enables RLS on the `sessions` table, which US-02.03 creates, so US-02.03 must complete first. Both stories sit in Sprint 3, so the dependency is satisfied within the sprint.

_INVEST:_ All criteria satisfied. The story is larger (5 points) than the INVEST "Small" ideal but is not further divisible without producing two halves neither of which can satisfy an isolation acceptance criterion on its own.

---

**US-02.06: Enable RLS on initial tables and verify isolation** — _Absorbed into US-02.05 (v1.1)._

The connection-level tenant binding (formerly US-02.05) and the RLS policies it drives (formerly this story) are one jointly-verifiable capability and are now a single story; see US-02.05, whose acceptance criteria subsume the policy-presence, isolation, and rollback checks formerly listed here. The 2-point estimate is folded into US-02.05's 5. Retained as a stub for traceability.

**Epic E2 total: 20 story points.**

---

## 10. Epic E3: Event Store and Projection Infrastructure

### Epic goal

Implement the event-sourcing substrate: the `events` table, the `EventStore` port and Postgres adapter, the projection framework that materializes read models, the River job queue integration, and the projection-lag observability metrics. The epic is the most architecturally foundational after E1 and E2, because every domain aggregate in subsequent epics depends on the event store working correctly.

### Business value

Event sourcing is one of the twelve architectural patterns from PRD section four. The infrastructure is invisible in the final UI but is the substrate on which audit completeness, exportability, and reconciliation are built. Without it, the system would still functionally serve requests but would not produce the senior-engineering portfolio signal.

### Epic-level acceptance

The epic is complete when an event can be appended to the store, loaded back, and consumed by a projector that materializes a stub read model.

### User stories

**US-03.01: Create events table and projection_progress table**

_Format:_ As the implementer, I want the events and projection_progress tables created with their full schema, so that aggregates can persist their state changes and projectors can track their consumed position.

_Description:_ Create the migrations for the `events` table (Database Schema section 6.1) and the `projection_progress` table (Database Schema section 6.2), including the `events_stream_position_seq` sequence and all indexes. Enable RLS on `events` and add the `tenant_isolation` policy.

_Acceptance Criteria:_

1. Given the migrations are applied, When the tables are queried with `\d events` in psql, Then all columns, indexes, and constraints from the design are present.
2. Given the sequence exists, When `SELECT nextval('events_stream_position_seq')` is called twice, Then the second call returns a value one greater than the first.
3. Given an attempt to insert two events with the same `(aggregate_id, sequence)`, Then the second insert fails with a unique constraint violation.

_Story Points:_ 2

_Dependencies:_ US-02.06

_Technical Notes:_ The migration includes the `events_stream_position_idx`, `events_tenant_occurred_at_idx`, and `events_tenant_type_occurred_at_idx` indexes from Database Schema section 6.1.

_INVEST:_ All criteria satisfied.

---

**US-03.02: Define EventStore port and domain event types**

_Format:_ As the implementer, I want the `EventStore` port interface and the `Event` and `EventMetadata` domain types defined, so that use cases can be written against the port and the type system enforces correct usage.

_Description:_ Create `internal/ports/outbound/event_store.go` with the `EventStore` interface (Append, Load, ReadAfterPosition, ReadByTenantAndTimeRange) per System Design section two. Create `internal/domain/events/event.go` with the `Event` and `EventMetadata` structs. Add the sentinel errors `ErrConcurrencyConflict` and `ErrEventNotFound`.

_Acceptance Criteria:_

1. Given the port interface is defined, When a use case imports it, Then the import resolves without circular dependency.
2. Given the Event struct, When an event is serialized with `encoding/json`, Then the result is a stable JSON object with the field names from the design.
3. Given the sentinel errors, When `errors.Is(err, ErrConcurrencyConflict)` is called, Then it returns true for any wrapped instance of the sentinel.

_Story Points:_ 2

_Dependencies:_ US-01.01

_Technical Notes:_ The interface signatures are specified in System Design section two with code excerpts that serve as the canonical shape.

_INVEST:_ All criteria satisfied.

---

**US-03.03: Implement PostgresEventStore adapter with concurrency control**

_Format:_ As the implementer, I want the Postgres adapter implementation of the `EventStore` port with optimistic concurrency control on append, so that the event store enforces the sequence-monotonicity invariant at the database level.

_Description:_ Implement `internal/adapters/outbound/postgres/event_store.go` with all four methods of the `EventStore` port. The `Append` method uses a single SQL statement with the unique constraint on `(aggregate_id, sequence)` to enforce concurrency control; a unique constraint violation is translated into `ErrConcurrencyConflict`. The `Load`, `ReadAfterPosition`, and `ReadByTenantAndTimeRange` methods use sqlc-generated queries.

_Acceptance Criteria:_

1. Given an empty events table, When `Append` is called with two events for the same aggregate, Then both events are persisted with sequences 1 and 2.
2. Given an event at sequence 1 exists, When `Append` is called with `expectedSequence = 0` (stale read), Then the call returns `ErrConcurrencyConflict`.
3. Given fifty events for an aggregate, When `Load` is called for that aggregate, Then all fifty events are returned in sequence order.
4. Given the contract test suite, When the in-memory and Postgres adapters are tested against the same suite, Then both pass all tests.

_Story Points:_ 5

_Dependencies:_ US-03.01, US-03.02

_Technical Notes:_ The implementation pattern is specified in System Design section two. The sqlc queries are in `internal/adapters/outbound/postgres/queries/events.sql`.

_INVEST:_ All criteria satisfied.

---

**US-03.04: Integrate River job queue with trace context propagation**

_Format:_ As the implementer, I want the River job queue integrated with the application, so that background jobs (projectors, webhook delivery, exports, cleanup) can be enqueued and processed.

_Description:_ Add the River dependency, create the worker-mode binary subcommand, define the `JobEnqueuer` port and its River-backed adapter, implement trace context serialization into job arguments and deserialization on worker pickup, and wire the worker pool startup into the application lifecycle.

_Acceptance Criteria:_

1. Given a job is enqueued within a transaction, When the transaction commits, Then the job appears in the River queue and is processed by a worker.
2. Given the transaction is rolled back, When the worker pool is inspected, Then the job is not present (transactional outbox semantics).
3. Given a job whose arguments include a trace context, When the worker processes the job, Then the worker's span is a child of the originating span as verified in Tempo.

_Story Points:_ 5

_Dependencies:_ US-03.01, US-01.06

_Technical Notes:_ The River integration pattern is specified in System Design section six. Trace context propagation is specified in the same section.

_INVEST:_ All criteria satisfied.

---

**US-03.05: Implement projection framework with advisory-lock coordination**

_Format:_ As the implementer, I want a projection framework that runs projector jobs as singletons coordinated by advisory locks, so that read models are materialized exactly once per event regardless of how many worker instances are running.

_Description:_ Create `internal/application/projector/projector.go` with the projector job kind, the per-projector batch processing loop, the advisory-lock acquisition keyed on `projector.<name>`, and the watermark update within the same transaction as the read model updates. Add the projection-lag metric `opengate_projection_lag_seconds` per System Design section three.

_Acceptance Criteria:_

1. Given two worker instances are running with the same projector job kind, When the projector job is scheduled, Then exactly one worker processes events at any moment, verified by the advisory lock holder.
2. Given the projector processes a batch and crashes before commit, When the next iteration runs, Then the same batch is re-processed (the watermark was not advanced).
3. Given the projector has processed events up to position P, When the projection-lag metric is queried, Then the metric value equals `now() - last_event_at` for the projector.

_Story Points:_ 5

_Dependencies:_ US-03.04, US-02.05

_Technical Notes:_ The advisory-lock pattern is specified in System Design section five. The projector job structure is in System Design section six.

_INVEST:_ All criteria satisfied.

---

**US-03.06: Implement three idempotency cache tables and middleware**

_Format:_ As an API consumer (dashboard or reader), I want idempotency-key support on every mutating endpoint, so that I can safely retry requests without producing duplicate side effects.

_Description:_ Create the three idempotency cache tables (`command_idempotency_keys`, `decision_idempotency_keys`, `reconciliation_idempotency_keys`) per Database Schema section seven. Implement the idempotency middleware for command-level and decision-level keys per System Design section four. Add the cleanup River job that runs every five minutes.

_Acceptance Criteria:_

1. Given an `Idempotency-Key` header is present, When the same request is retried with the same key and same body, Then the cached response is returned and the handler is not re-invoked.
2. Given an `Idempotency-Key` header is present with the same key but a different body, When the request is processed, Then the response is 409 Conflict with a Problem Details body.
3. Given an idempotency key older than ten minutes, When the cleanup job runs, Then the key is deleted from the table.

_Story Points:_ 5

_Dependencies:_ US-03.05

_Technical Notes:_ The three idempotency variants are specified in System Design section four. The cleanup job kind is `cleanup.idempotency_keys`.

_INVEST:_ All criteria satisfied.

**Epic E3 total: 24 story points.**

---

## 11. Epic E4: Member and Credential Domain

### Epic goal

Implement the member and credential aggregates with their event types, command handlers, read models, and the use cases that the dashboard exposes. The epic exercises the event-sourcing and CQRS patterns in their most direct form and includes the advisory-lock-coordinated credential identifier generation flow.

### Business value

Members and credentials are the most frequently mutated entities in the domain, so the patterns established here will be replicated in the policy, door, and subscription epics that follow. Getting the patterns right at this stage saves rework in later epics.

### Epic-level acceptance

The epic is complete when an administrator can create a member, issue a credential to that member, revoke the credential, and see all of these changes reflected in the dashboard's data views, with full event history in the events table.

### User stories

**US-04.01: Implement member aggregate, events, and command handlers**

_Format:_ As an administrator, I want to create, update, and transition the status of members in my tenant, so that I can manage who is enrolled at my gym.

_Description:_ Create the member aggregate per System Design section twelve, define the `member.created.v1`, `member.identity_updated.v1`, and `member.status_transitioned.v1` event types, implement the three command handlers, and add the synchronous projection to the `members_view` read model within the command transaction. Create the `members_view` table migration per Database Schema section 8.1.

_Acceptance Criteria:_

1. Given an authenticated owner session, When POST `/api/v1/tenants/{t}/members` is called with valid payload, Then a member.created.v1 event is appended, a members_view row is inserted, and the response is 201 Created.
2. Given an existing member, When PATCH `/api/v1/tenants/{t}/members/{m}` is called with a name change, Then a member.identity_updated.v1 event is appended and the members_view row is updated.
3. Given an active member, When POST `/api/v1/tenants/{t}/members/{m}/transition` is called with target status `suspended`, Then a member.status_transitioned.v1 event is appended and the read model reflects the new status.
4. Given an expired member, When the transition endpoint is called with target status `suspended`, Then the response is 422 Unprocessable Entity (invalid state transition).

_Story Points:_ 5

_Dependencies:_ US-03.03, US-02.06

_Technical Notes:_ Member aggregate invariants are in System Design section twelve. The members_view synchronous projection is specified in System Design section three.

_INVEST:_ All criteria satisfied.

---

**US-04.02: Implement member list and detail queries with search**

_Format:_ As an administrator, I want to list, search, and view members in my tenant, so that I can find specific members and review their details from the dashboard.

_Description:_ Implement the query use cases for member list (paginated, filterable by status), member search (substring match on name and email via trigram index), and member detail (full member record plus associated credentials and recent access events count). Add the `pg_trgm` extension migration and the trigram indexes per Database Schema section 8.1.

_Acceptance Criteria:_

1. Given fifty members exist, When GET `/api/v1/tenants/{t}/members?limit=20` is called, Then the response contains twenty members and a cursor for the next page.
2. Given a member named "Marko Petrović", When GET `/api/v1/tenants/{t}/members?q=marko` is called, Then the response includes the member regardless of case.
3. Given a member ID, When GET `/api/v1/tenants/{t}/members/{m}` is called, Then the response includes the member's full record, the count of their credentials, and the count of their access events in the last thirty days.

_Story Points:_ 3

_Dependencies:_ US-04.01

_Technical Notes:_ The trigram search pattern is specified in System Design section twelve. Pagination uses keyset cursors per the convention from Database Schema section ten.

_INVEST:_ All criteria satisfied.

---

**US-04.03: Implement credentials_view and credential aggregate**

_Format:_ As an administrator, I want to issue and revoke credentials for members, so that I can grant physical access to enrolled members and remove it when needed.

_Description:_ Create the `credentials_view` table migration per Database Schema section 8.2, implement the credential aggregate with events `credential.issued.v1`, `credential.activated.v1`, `credential.revoked.v1`, and `credential.expired.v1`, and implement the issuance and revocation command handlers. The issuance handler supports both administrator-provided identifier and backend-generated identifier modes.

_Acceptance Criteria:_

1. Given an active member, When POST `/api/v1/tenants/{t}/members/{m}/credentials` is called with an explicit identifier, Then a credential.issued.v1 event is appended and the credentials_view row is inserted with status `active`.
2. Given an active credential, When POST `/api/v1/tenants/{t}/credentials/{c}/revoke` is called, Then a credential.revoked.v1 event is appended, the credentials_view status is `revoked`, and the revoked_at timestamp is populated within the same transaction.
3. Given a revoked credential, When the revoke endpoint is called again, Then the response is 409 Conflict (idempotent operation, but the second call's semantics are not a fresh revocation).

_Story Points:_ 5

_Dependencies:_ US-04.01

_Technical Notes:_ The credentials_view is synchronously projected per System Design section three. The aggregate invariants include the rule that a revoked credential cannot be reactivated.

_INVEST:_ All criteria satisfied.

---

**US-04.04: Implement backend-generated credential identifier with advisory lock**

_Format:_ As an administrator performing bulk enrollment, I want the backend to generate credential identifiers for me when I do not have physical cards to scan, so that I can issue many credentials without scanning each one.

_Description:_ Extend the credential issuance command to support backend-generated identifiers via the advisory-lock-protected critical section keyed on `credential.generate:<tenant_id>` per System Design section thirteen. The generator finds the highest existing credential identifier suffix in the tenant, increments it, and produces a new identifier.

_Acceptance Criteria:_

1. Given a tenant with one hundred existing credentials, When the generation endpoint is called, Then a new credential is issued with an identifier whose suffix is one greater than the highest existing.
2. Given two concurrent requests for backend-generated credentials, When both are processed, Then they receive different identifiers (the advisory lock serializes the critical section).
3. Given a tenant with no credentials yet, When the generation endpoint is called, Then the new credential's identifier has suffix 1.

_Story Points:_ 3

_Dependencies:_ US-04.03

_Technical Notes:_ The advisory lock pattern is in System Design section five. The integer wrapping behavior for the generated identifier prefix follows the System Design specification.

_INVEST:_ All criteria satisfied.

**Epic E4 total: 16 story points.**

---

## 12. Epic E5: Access Policy and Door Domain

### Epic goal

Implement the access policy and door aggregates, the policy decomposition into doors, time windows, and assignments, and the time-window evaluation algorithm used by the access decision path. The epic completes the data prerequisites for the access decision in E6.

### Business value

The policy capability is the heart of the configurable access control behavior; without it, the access decision in E6 would have nothing to evaluate. The deliberately restricted policy model from PFD section five is the design choice being demonstrated.

### Epic-level acceptance

The epic is complete when an administrator can create a door, create a policy with door coverage and time windows, assign the policy to a member, and see the effective permissions preview reflect the configuration accurately.

### User stories

**US-05.01: Implement door aggregate and doors_view**

_Format:_ As an administrator, I want to create and configure doors in my tenant, so that I can model the physical entry points of my gym.

_Description:_ Create the `doors_view` table migration per Database Schema section 9.2, implement the door aggregate with events `door.created.v1`, `door.config_updated.v1`, and `door.status_changed.v1`, and implement the door management command handlers.

_Acceptance Criteria:_

1. Given an authenticated owner, When POST `/api/v1/tenants/{t}/doors` is called with valid payload, Then a door.created.v1 event is appended and the doors_view row is inserted with status `offline`.
2. Given an existing door, When PATCH `/api/v1/tenants/{t}/doors/{d}` is called with a name change, Then a door.config_updated.v1 event is appended and the read model is updated.
3. Given a duplicate door name within the tenant, When the create endpoint is called, Then the response is 422 Unprocessable Entity.

_Story Points:_ 3

_Dependencies:_ US-04.01

_Technical Notes:_ The doors_view is synchronously projected because the access decision uses it. The door status transitions are driven by heartbeat events handled in E7.

_INVEST:_ All criteria satisfied.

---

**US-05.02: Implement policy aggregate with decomposed read model**

_Format:_ As an administrator, I want to create and configure access policies with door coverage and time windows, so that I can define when members are allowed to access specific doors.

_Description:_ Create the migrations for `policies_view`, `policy_doors_view`, `policy_time_windows_view`, and `policy_assignments_view` per Database Schema section 9.1. Implement the policy aggregate with events for policy creation, door-coverage modification, time-window modification, member assignment, and member unassignment. Implement the policy management command handlers.

_Acceptance Criteria:_

1. Given an authenticated owner, When POST `/api/v1/tenants/{t}/policies` is called with a name, door list, and time windows, Then the events are appended and all four read model tables are populated within the transaction.
2. Given an existing policy, When PUT `/api/v1/tenants/{t}/policies/{p}/assignments` is called with a member list, Then the policy_assignments_view is updated to match.
3. Given a time window with `start_time` ≥ `end_time`, When the create endpoint is called, Then the response is 422 Unprocessable Entity (the constraint forbids midnight-crossing windows in this version).

_Story Points:_ 5

_Dependencies:_ US-05.01, US-04.01

_Technical Notes:_ The policy decomposition is specified in Database Schema section 9.1. The weekdays bitmask convention is specified in the same section.

_INVEST:_ All criteria satisfied.

---

**US-05.03: Implement DecisionEvaluator domain service**

_Format:_ As the implementer, I want a pure-function `DecisionEvaluator` that takes member, credential, policies, door, and current time and returns a decision with reason code, so that the access decision logic is isolated and testable.

_Description:_ Create `internal/domain/access/evaluator.go` with the `DecisionEvaluator` struct and its `Evaluate` method. The method implements the algorithm specified in System Design section fourteen: check credential validity, check member status, evaluate each assigned policy's door coverage and time windows, and return grant if any policy covers the door at the current time-of-day in the tenant's timezone.

_Acceptance Criteria:_

1. Given a member with an active credential and a policy covering the door at the current time, When `Evaluate` is called, Then the result is `{Decision: grant, ReasonCode: granted}`.
2. Given a revoked credential, When `Evaluate` is called, Then the result is `{Decision: deny, ReasonCode: deny_revoked_credential}` regardless of policy state.
3. Given a policy with a time window 09:00-17:00 on weekdays and the current time is Saturday 12:00, When `Evaluate` is called, Then the result is `{Decision: deny, ReasonCode: deny_outside_policy_window}`.
4. Given a member with no policies covering the door, When `Evaluate` is called, Then the result is `{Decision: deny, ReasonCode: deny_no_policy_covers_door}`.

_Story Points:_ 3

_Dependencies:_ US-05.02

_Technical Notes:_ The algorithm is in System Design section fourteen. The evaluator is a pure function with no I/O, making it trivially testable with table-driven tests.

_INVEST:_ All criteria satisfied.

---

**US-05.04: Implement effective-permissions preview endpoint**

_Format:_ As an administrator, I want to see at a glance what doors a specific member can access right now, so that I can verify their access configuration without making them attempt entry.

_Description:_ Implement GET `/api/v1/tenants/{t}/members/{m}/effective-permissions` that returns the list of (door, allowed-now) tuples for the member. The endpoint uses the `DecisionEvaluator` against the member's assigned policies and the current time.

_Acceptance Criteria:_

1. Given a member with a policy covering all doors at the current time, When the endpoint is called, Then the response lists every door with `allowed_now: true`.
2. Given a member with no assigned policies, When the endpoint is called, Then the response lists every door with `allowed_now: false`.
3. Given a member with mixed policy coverage, When the endpoint is called, Then the response correctly identifies which doors are currently allowed.

_Story Points:_ 2

_Dependencies:_ US-05.03

_Technical Notes:_ The endpoint is a read-only query that does not write any events; it is the dashboard's debugging aid for administrators.

_INVEST:_ All criteria satisfied.

**Epic E5 total: 13 story points.**

---

## 13. Epic E6: Access Authorization Decision Path

### Epic goal

Implement the synchronous access authorization decision path: the reader-facing authentication endpoint, the decision use case that orchestrates read model lookups and event store appends within a single transaction, and the decision-level idempotency cache. This epic is the runtime-critical path of the system with the fifty-millisecond p99 latency target.

### Business value

The access decision is the most operationally consequential capability in OpenGate. A correctness or performance bug here would directly affect the user experience at the door. The epic exercises event sourcing, CQRS, idempotent command handling, and OpenTelemetry tracing in their most performance-sensitive composition.

### Epic-level acceptance

The epic is complete when a reader can present an authenticated credential identifier and door identifier to the API, receive a grant or deny decision with a reason code, and have the decision recorded as an event in the event store, all within the latency budget.

### User stories

**US-06.01: Implement reader API key authentication middleware**

_Format:_ As a reader client, I want to authenticate to the OpenGate API using my provisioned API key, so that the backend can attribute my requests to the correct reader and enforce its scope.

_Description:_ Implement the reader authentication middleware that extracts the Bearer token from the Authorization header, looks up the corresponding reader by API key hash with a five-minute in-process cache (per System Design section nine), and populates the request context with the reader's tenant ID and reader ID.

_Acceptance Criteria:_

1. Given a valid API key, When a reader endpoint is called, Then the request proceeds with the correct reader context populated.
2. Given an invalid API key, When the endpoint is called, Then the response is 401 Unauthorized.
3. Given an API key was just rotated, When the rotation overlap window is active, Then the endpoint accepts either the old or the new key.

_Story Points:_ 3

_Dependencies:_ US-02.02

_Technical Notes:_ The API key verification cache is specified in System Design section nine. The rotation overlap mechanism uses the `rotation_key_hash` column from Database Schema section 5.4.

_INVEST:_ All criteria satisfied.

---

**US-06.02: Implement readers table and reader provisioning**

_Format:_ As an administrator, I want to provision a new reader for a door, so that I can later connect a physical or simulated reader and have it authenticated against the backend.

_Description:_ Create the `readers` table migration per Database Schema section 5.4 and implement the reader provisioning endpoint POST `/api/v1/tenants/{t}/readers`. The endpoint generates a random API key, hashes it with Argon2id, persists the reader record, and returns the plaintext API key in the response (only once; not retrievable later). Provisioning is an administrator action; the reader does not self-provision.

_Acceptance Criteria:_

1. Given an authenticated owner, When the provisioning endpoint is called with a valid door reference, Then a reader row is created and the plaintext API key is returned in the response.
2. Given a non-existent door reference, When the provisioning endpoint is called, Then the response is 422 Unprocessable Entity.
3. Given the reader is provisioned, When GET on the same endpoint is called, Then the response does not include the plaintext API key (only the hash existence is acknowledged).

_Story Points:_ 3

_Dependencies:_ US-05.01

_Technical Notes:_ The reader provisioning pattern is implicit in System Design section nine. The plaintext key return is the standard pattern for token-issuing endpoints.

_INVEST:_ All criteria satisfied.

---

**US-06.03: Implement AccessDecision use case with synchronous event write**

_Format:_ As a reader client, I want to submit an authentication request and receive a grant or deny decision within fifty milliseconds at p99, so that I can serve members at the door without perceptible delay.

_Description:_ Implement the AccessDecision use case per System Design section sixteen. The use case loads the credential, member, and policies from their read models, evaluates the decision via `DecisionEvaluator`, appends the access-attempted event to the event store, writes the idempotency cache entry, and returns the decision. All persistence operations are in a single transaction.

_Acceptance Criteria:_

1. Given a valid credential and an open time-window for the door, When POST `/api/v1/readers/{r}/authenticate` is called, Then the response is 200 OK with `{"decision": "grant", "reason_code": "granted"}` and an access-attempted event is appended.
2. Given a revoked credential, When the endpoint is called, Then the response is `{"decision": "deny", "reason_code": "deny_revoked_credential"}` and an access-attempted event is still appended (deny outcomes are recorded too).
3. Given a benchmark with one hundred concurrent decisions, When the latency distribution is measured, Then the p99 is below fifty milliseconds.
4. Given a retry with the same idempotency key, When the endpoint is called twice with identical payloads, Then the second call returns the cached response without re-evaluating.

_Story Points:_ 5

_Dependencies:_ US-05.03, US-06.01, US-03.06

_Technical Notes:_ The full decision sequence is in System Design section sixteen. The performance budget breakdown is in the same section.

_INVEST:_ All criteria satisfied.

---

**US-06.04: Add OpenTelemetry instrumentation on decision path**

_Format:_ As an operator, I want the access decision path fully instrumented with OpenTelemetry spans and metrics, so that I can observe decision latency and outcomes through Grafana.

_Description:_ Add the span `command.authenticate_access` wrapping the decision use case with the attributes from System Design section eight (`opengate.decision`, `opengate.decision_reason`, `opengate.credential_id`, `opengate.door_id`, `opengate.member_id`). Emit the `opengate_decision_duration_seconds` histogram labeled by outcome.

_Acceptance Criteria:_

1. Given an access decision is processed, When Tempo is queried for the trace, Then the span tree includes the command span, the read model query spans, the evaluator span (sub-millisecond), and the event store append span.
2. Given a hundred decisions of varying outcomes, When Prometheus is queried for `opengate_decision_duration_seconds`, Then the histogram has buckets populated for both grant and deny outcomes.
3. Given a denied decision, When the corresponding span is inspected, Then the `opengate.decision_reason` attribute matches the reason code in the response.

_Story Points:_ 2

_Dependencies:_ US-06.03

_Technical Notes:_ The OTel conventions are in System Design section eight. The metric naming follows the same section.

_INVEST:_ All criteria satisfied.

**Epic E6 total: 13 story points.**

---

## 14. Epic E7: Reader Operations and SSE Push

### Epic goal

Implement the SSE push side of the dual-direction reader port: the LISTEN/NOTIFY-backed fanout from use cases and projectors to active reader streams, the SSE server endpoint, the reader heartbeat endpoint, and the door status transitions that result from heartbeat freshness.

### Business value

The push side completes the Reader port's dual-direction discipline established in System Architecture section eight. Without it, readers would have no way to learn of credential or policy changes during normal operation; the only state propagation would be on reconnection, which is the offline-reconciliation mode and not the steady state.

### Epic-level acceptance

The epic is complete when a credential issuance produces an SSE event on the affected reader's open stream within seconds, and a reader heartbeat keeps the door's status `online` while heartbeat absence drives it to `offline`.

### User stories

**US-07.01: Implement ReaderNotifier port and pg LISTEN/NOTIFY adapter**

_Format:_ As the implementer, I want a `ReaderNotifier` port whose Postgres adapter writes to the `opengate_reader_events` LISTEN channel, so that use cases can notify readers without coupling to the SSE transport.

_Description:_ Create `internal/ports/outbound/reader_notifier.go` and `internal/adapters/outbound/postgres/reader_notifier.go`. The adapter wraps `NOTIFY opengate_reader_events, '<json_payload>'` within the originating transaction. The notification fires on transaction commit; uncommitted notifications are discarded automatically.

_Acceptance Criteria:_

1. Given the adapter is called within a transaction, When the transaction commits, Then a LISTEN-ing connection in another session receives the notification.
2. Given the adapter is called within a transaction that is rolled back, When the same LISTEN-ing connection is inspected, Then no notification is received.
3. Given a notification with a tenant filter and a reader ID, When the listener filters by reader ID, Then only notifications for that reader pass through.

_Story Points:_ 3

_Dependencies:_ US-06.02

_Technical Notes:_ The pattern is specified in System Architecture section eight and System Design section fifteen.

_INVEST:_ All criteria satisfied.

---

**US-07.02: Implement SSE push server endpoint**

_Format:_ As a reader client, I want to open a long-lived SSE stream to the OpenGate API and receive credential and policy updates as they happen, so that my local cache stays current during online operation.

_Description:_ Implement GET `/api/v1/readers/{r}/stream` with the SSE protocol: set the appropriate headers, open a LISTEN on `opengate_reader_events`, filter notifications by the authenticated reader's ID, and write SSE events to the response writer. The endpoint supports `Last-Event-Id` header for resume after disconnection.

_Acceptance Criteria:_

1. Given an authenticated reader opens the stream, When a credential.revoked.v1 event is produced for that reader's tenant, Then the reader receives an SSE event of type `credential.revoked` within five hundred milliseconds.
2. Given the reader disconnects and reconnects with `Last-Event-Id: 100`, When events with stream position greater than 100 exist, Then the reader receives them in order on the new connection.
3. Given the keepalive interval (thirty seconds) elapses without other events, When the stream is inspected, Then a `keepalive` event has been written to prevent proxy timeout.

_Story Points:_ 5

_Dependencies:_ US-07.01, US-06.01

_Technical Notes:_ The SSE implementation details are in System Design section fifteen. The Last-Event-Id resumption is the standard EventSource behavior.

_INVEST:_ All criteria satisfied.

---

**US-07.03: Implement reader heartbeat endpoint and door status transitions**

_Format:_ As a reader client, I want to send periodic heartbeats to the backend, so that the dashboard reflects my reader as online or detects when I have gone offline.

_Description:_ Implement POST `/api/v1/readers/{r}/heartbeat` that updates the `readers.last_seen_at`, updates the `doors_view.last_heartbeat_at` for the reader's door, and computes any status transition (online → degraded → offline based on threshold elapsed since last heartbeat). Status transitions emit `door.status_changed.v1` events.

_Acceptance Criteria:_

1. Given a reader sends a heartbeat, When the heartbeat endpoint returns 204, Then the door's status is `online` and the `last_heartbeat_at` matches the request time.
2. Given a reader has not sent a heartbeat for sixty seconds, When the door status checker job runs, Then the door's status transitions to `offline` and a `door.status_changed.v1` event is appended.
3. Given the door was offline and a heartbeat arrives, When the heartbeat endpoint processes it, Then the door's status transitions to `online` and the corresponding event is appended.

_Story Points:_ 3

_Dependencies:_ US-07.02, US-06.02

_Technical Notes:_ The status transition thresholds are configurable; defaults are in System Architecture section eight. The status checker is a periodic River job.

_INVEST:_ All criteria satisfied.

**Epic E7 total: 11 story points.**

---

## 15. Epic E8: Offline Reconciliation

### Epic goal

Implement the offline reconciliation use case that accepts batches of access events from a reader that was offline, deduplicates them via composite-key idempotency, and appends them to the event store. The epic exercises the reconciliation-variant idempotency from System Design section four.

### Business value

Offline reconciliation is the substrate of one of the six PRD use case scenarios and is the most distributed-systems-flavored capability in the system. Demonstrating it correctly is a senior-engineering signal because the deduplication-on-retry property is easy to get wrong.

### Epic-level acceptance

The epic is complete when a reader can submit a batch of access events accumulated offline, the backend deduplicates them on retry, and a conflict between local decisions and backend state is highlighted in the audit log read model when E9 is also complete.

### User stories

**US-08.01: Implement Reconciliation use case with composite-key dedup**

_Format:_ As a reader that was offline, I want to submit my accumulated access events to the backend on reconnection, so that my offline decisions are recorded in the audit log without duplication regardless of retries.

_Description:_ Create the `reconciliation_idempotency_keys` table migration per Database Schema section 7.3 and implement POST `/api/v1/readers/{r}/events` that accepts a batch of local access events, deduplicates each by composite key `(reader_id, sequence_no)`, and appends the new ones as `access.attempted_offline.v1` events. Each event is processed in its own transaction so that a single bad event does not abort the batch.

_Acceptance Criteria:_

1. Given an empty idempotency table, When the endpoint is called with three local events, Then three `access.attempted_offline.v1` events are appended and three reconciliation_idempotency_keys rows are inserted.
2. Given the same batch is submitted again, When the endpoint is called, Then no new events are appended and the response indicates three duplicates and zero new.
3. Given a batch with two new events and one already-seen, When the endpoint is called, Then two events are appended and the response indicates one duplicate and two new.

_Story Points:_ 5

_Dependencies:_ US-06.03

_Technical Notes:_ The reconciliation idempotency variant is specified in System Design section four. The per-event transaction pattern is in System Design section seventeen.

_INVEST:_ All criteria satisfied.

**Epic E8 total: 5 story points.**

---

## 16. Epic E9: Audit Log and Compliance Queries

### Epic goal

Implement the asynchronous audit log read model projector, the audit log read model schema with its indexes, the compliance query endpoint with multi-dimensional filtering and keyset pagination, and the CSV export of audit query results.

### Business value

Audit queries are one of the six PRD use case scenarios and are the cleanest demonstration of CQRS in the system. The keyset pagination pattern is a senior-engineering signal often missing in implementations that use naive offset pagination.

### Epic-level acceptance

The epic is complete when an auditor can filter the audit log by tenant, door, member, decision outcome, and time range, navigate through results with stable keyset pagination, drill into a specific event for full reasoning, and export results to CSV.

### User stories

**US-09.01: Implement audit_log_view projector**

_Format:_ As an auditor, I want access events to appear in the audit log within seconds of occurring, so that compliance reviews use timely data.

_Description:_ Create the `audit_log_view` table migration per Database Schema section ten and implement the `projector.audit_log` River job that consumes access-attempted and access-attempted-offline events and writes denormalized rows. The projector resolves the member name and door name from their respective read models at projection time.

_Acceptance Criteria:_

1. Given an access-attempted event is appended, When the projector iteration runs, Then a corresponding audit_log_view row is inserted with the member name and door name denormalized.
2. Given the projection lag metric is queried during normal operation, When the value is inspected, Then it is below five seconds.
3. Given a member is renamed, When subsequent audit projections run, Then historical audit_log_view rows for that member are updated to the new name (the projector handles member-renamed events).

_Story Points:_ 5

_Dependencies:_ US-04.02, US-05.01, US-06.03

_Technical Notes:_ The audit_log_view schema with all indexes is in Database Schema section ten. The projector follows the framework from US-03.05.

_INVEST:_ All criteria satisfied.

---

**US-09.02: Implement audit search query with keyset pagination**

_Format:_ As an auditor, I want to search the audit log with multiple filters and paginate efficiently through results, so that I can investigate access patterns over arbitrary time ranges.

_Description:_ Implement GET `/api/v1/tenants/{t}/audit` with query parameters for filter dimensions (door IDs, member IDs, decision outcome, reason codes, time range) and a `cursor` parameter for keyset pagination. The endpoint returns up to fifty results per page with a next-page cursor.

_Acceptance Criteria:_

1. Given ten thousand audit events exist, When the endpoint is called with `limit=50`, Then the response returns fifty events in reverse chronological order with a next-page cursor.
2. Given the next-page cursor is used in a follow-up request, When the response is inspected, Then the next fifty events follow in order with no overlap with the previous page.
3. Given a query with `door_ids=[D1,D2]&decision=deny`, When the response is inspected, Then all returned events are deny outcomes at doors D1 or D2.
4. Given a query at offset 5000 (via cursor walk), When the latency is measured, Then it is below two hundred milliseconds at p95 (consistent with offset-zero performance).

_Story Points:_ 5

_Dependencies:_ US-09.01

_Technical Notes:_ The keyset pagination pattern is in System Design section eighteen. The indexes in Database Schema section ten support the query patterns.

_INVEST:_ All criteria satisfied.

---

**US-09.03: Implement audit CSV export job**

_Format:_ As an auditor, I want to export audit query results to CSV, so that I can include them in compliance reports outside the OpenGate dashboard.

_Description:_ Implement POST `/api/v1/tenants/{t}/audit/export` that accepts the same filters as the search query and enqueues a `cleanup.audit_csv_export` River job. The job streams results to a CSV file in the shared download volume and updates `export_status_view` with the file path.

_Acceptance Criteria:_

1. Given an export is requested with no filter (all events), When the job runs to completion, Then a CSV file is produced with all matching rows.
2. Given the export status is queried during execution, When the response is inspected, Then it reports `status: running` and progress information.
3. Given the export is complete, When the download URL is requested, Then the file streams to the client with appropriate Content-Disposition.

_Story Points:_ 3

_Dependencies:_ US-09.02

_Technical Notes:_ The export job pattern follows the framework from E3. The shared download volume is part of the Docker Compose configuration from US-01.03.

_INVEST:_ All criteria satisfied.

**Epic E9 total: 13 story points.**

---

## 17. Epic E10: Webhook Subscriptions and Delivery

### Epic goal

Implement the webhook subscription management, the HMAC-signed delivery with exponential backoff retry, the dead-letter queue, and the subscription delivery read model that surfaces the dead-letter inspection view.

### Business value

Webhooks are the primary integration mechanism for OpenGate-using gyms with their other software systems. The pattern is also one of the twelve PRD architectural patterns being demonstrated.

### Epic-level acceptance

The epic is complete when a tenant administrator can configure a webhook subscription, an event matching the filter produces an HMAC-signed delivery attempt, failures trigger retries with backoff, and persistent failures appear in the dead-letter inspection view.

### User stories

**US-10.01: Implement subscriptions table and management endpoints**

_Format:_ As a tenant administrator, I want to create and configure webhook subscriptions, so that my external systems can receive event notifications.

_Description:_ Create the `subscriptions` table migration per Database Schema section 5.5 and implement the subscription CRUD endpoints. Include the secret-rotation mechanism with the overlap window.

_Acceptance Criteria:_

1. Given an authenticated manager, When POST `/api/v1/tenants/{t}/subscriptions` is called with a URL and event filter, Then a subscription row is created with a generated secret returned in the response.
2. Given an existing subscription, When the rotate-secret endpoint is called, Then the rotation_secret_hash is populated, the rotation_expires_at is set to 24 hours from now, and the new secret is returned.
3. Given an invalid URL format, When the create endpoint is called, Then the response is 422 Unprocessable Entity.

_Story Points:_ 3

_Dependencies:_ US-02.04

_Technical Notes:_ The subscription schema is in Database Schema section 5.5. The rotation pattern is in System Design section nineteen.

_INVEST:_ All criteria satisfied.

---

**US-10.02: Implement webhook delivery worker with HMAC signing**

_Format:_ As an external subscriber, I want to receive webhook deliveries signed with HMAC-SHA256, so that I can verify the authenticity of incoming notifications.

_Description:_ Implement the `subscription.deliver` River job that loads the subscription and event, constructs the JSON payload with the standard envelope, computes the HMAC signature, makes the HTTP POST request with the appropriate headers, and records the outcome as a `subscription.delivery.attempted` event.

_Acceptance Criteria:_

1. Given a matching event is produced, When the delivery worker processes the job, Then an HTTP POST is made to the subscription URL with the `OpenGate-Signature`, `OpenGate-Webhook-Id`, and `Content-Type` headers.
2. Given a valid subscriber that responds with 200 OK, When the worker completes, Then the delivery is recorded as success.
3. Given a subscriber that responds with 500, When the worker completes, Then the job is rescheduled by River with exponential backoff and the failed attempt is recorded.

_Story Points:_ 5

_Dependencies:_ US-10.01, US-03.04

_Technical Notes:_ The signing scheme is in System Architecture section eight. The retry configuration is in System Design section nineteen.

_INVEST:_ All criteria satisfied.

---

**US-10.03: Implement subscription_delivery_view projector and dead-letter view**

_Format:_ As a tenant administrator, I want to inspect failed webhook deliveries in the dashboard, so that I can troubleshoot subscriber issues.

_Description:_ Create the `subscription_delivery_view` table per Database Schema section 11.1 and implement the projector that consumes `subscription.delivery.attempted` events. Implement the dashboard endpoint that lists dead-lettered deliveries with their last-error details.

_Acceptance Criteria:_

1. Given a delivery succeeds, When the projector runs, Then the corresponding row in subscription_delivery_view has status `succeeded`.
2. Given a delivery exhausts its retry budget, When the worker dead-letters it, Then the projector updates the row to status `dead_lettered` and the dashboard query returns it.
3. Given an administrator triggers a manual retry, When the retry job runs, Then a new delivery attempt is recorded and a successful retry sets the status back to `succeeded`.

_Story Points:_ 3

_Dependencies:_ US-10.02

_Technical Notes:_ The projector framework is from US-03.05. The dead-letter inspection view is in System Design section nineteen.

_INVEST:_ All criteria satisfied.

**Epic E10 total: 11 story points.**

---

## 18. Epic E11: Tenant Data Export

### Epic goal

Implement the tenant data export job that produces a signed, verifiable archive containing all of a tenant's events, read model snapshots, and configuration. The archive format and signature scheme are specified in System Architecture section eight.

### Business value

Data export supports the right-to-data-portability principle that any modern multi-tenant system must respect. The Ed25519 signing scheme demonstrates cryptographic integration into application-layer features.

### Epic-level acceptance

The epic is complete when an owner can initiate an export, the job produces a `.tar.gz` archive in the shared download volume, the manifest is signed with the deployment's Ed25519 key, and a third party can verify the signature using the published public key.

### User stories

**US-11.01: Implement export_status_view and export initiation endpoint**

_Format:_ As a tenant owner, I want to initiate a data export and monitor its progress, so that I can retrieve my tenant's data when needed.

_Description:_ Create the `export_status_view` table per Database Schema section 11.2 and implement POST `/api/v1/tenants/{t}/exports` that creates the export status row and enqueues the `export.run` River job. Also implement GET endpoints for listing exports and getting a download URL.

_Acceptance Criteria:_

1. Given an authenticated owner, When the export endpoint is called, Then a row is created with status `pending` and the job is enqueued.
2. Given the job picks up the work, When the status is queried, Then status transitions through `running` to `completed`.
3. Given a completed export, When the download URL is requested, Then the response includes a time-limited URL that streams the archive file.

_Story Points:_ 3

_Dependencies:_ US-03.04

_Technical Notes:_ The export status schema is in Database Schema section 11.2. The job structure is in System Design section twenty.

_INVEST:_ All criteria satisfied.

---

**US-11.02: Implement export job with Ed25519 manifest signing**

_Format:_ As a tenant owner, I want my export to be cryptographically signed so that I can prove its integrity, so that any downstream system can verify the archive has not been tampered with.

_Description:_ Implement the export job per System Design section twenty: iterate events with tenant filter, write monthly batches to gzipped JSON files, snapshot read models, capture configuration with secrets redacted, compute SHA-256 checksums, write the manifest, sign with Ed25519, and package as `.tar.gz`. Add the `/.well-known/opengate-signing-key.pub` endpoint to publish the public key.

_Acceptance Criteria:_

1. Given an export completes, When the archive is extracted, Then the structure matches the format specified in System Architecture section eight (manifest, events, read-models, config).
2. Given the manifest, When its signature is verified against the published public key using Ed25519, Then verification succeeds.
3. Given any file in the archive is modified, When the manifest checksums are recomputed, Then they no longer match (tampering is detectable).

_Story Points:_ 5

_Dependencies:_ US-11.01

_Technical Notes:_ The archive format is in System Architecture section eight. The signing key loading is in System Design section twenty.

_INVEST:_ All criteria satisfied.

**Epic E11 total: 8 story points.**

---

## 19. Epic E12: Reader Simulator

### Epic goal

Implement the reader simulator binary mode that drives N simulated readers per tenant, with realistic latency, occasional transient failures, and controllable online/offline schedule. The simulator is the substrate for the demo and for end-to-end tests.

### Business value

The simulator makes the system demonstrable without physical hardware. It is the substrate for the demo video and for any reviewer's local exploration of the system, and so is critical to the portfolio signal even though it is operationally peripheral.

### Epic-level acceptance

The epic is complete when the simulator can be configured to drive multiple readers per tenant, the simulated readers connect via the reader API, send authentication requests, respect online/offline schedules, and produce traffic that resembles a real deployment.

### User stories

**US-12.01: Implement simulator binary mode and reader fleet management**

_Format:_ As a demo presenter, I want to start a configurable number of simulated readers per tenant with a single command, so that I can demonstrate the system at any scale.

_Description:_ Implement the `simulator` subcommand that reads a YAML configuration file specifying tenants and reader counts, provisions each reader if not already provisioned, and starts the simulator goroutines.

_Acceptance Criteria:_

1. Given a configuration with two tenants and five readers each, When `opengate simulator --config=demo.yaml` is run, Then ten readers are provisioned and their goroutines are started.
2. Given the simulator is already running and the configuration is unchanged, When the binary is restarted, Then the same readers are reused (the API keys persisted to local volume).
3. Given a SIGTERM is received, When the simulator shuts down, Then all goroutines complete in-flight requests and exit cleanly.

_Story Points:_ 3

_Dependencies:_ US-06.02

_Technical Notes:_ The simulator is run as a separate Docker Compose service per System Architecture section seven.

_INVEST:_ All criteria satisfied.

---

**US-12.02: Implement realistic reader behavior with latency and failures**

_Format:_ As a demo presenter, I want the simulated readers to produce traffic that resembles real hardware, so that the dashboard shows the kind of latency, failures, and patterns that would be observed in a real deployment.

_Description:_ Implement the reader behavior loop with configurable parameters: authentication request interval (with jitter), simulated network latency (1-20ms), transient failure rate (0.5%), heartbeat interval (30s), online/offline schedule (configurable runs of online and offline durations). Include the local-cache fallback when offline.

_Acceptance Criteria:_

1. Given a simulator running for one minute with five readers, When the access decision latency is measured, Then the recorded latencies show realistic jitter.
2. Given a configured failure rate of 0.5%, When the simulator runs for ten thousand requests, Then approximately fifty are transient failures consistent with the rate.
3. Given a reader is in its offline window, When access requests occur, Then they are evaluated against the local cache and accumulated for reconciliation.
4. Given the offline window ends, When the reader reconnects, Then the accumulated events are submitted via the reconciliation endpoint.

_Story Points:_ 5

_Dependencies:_ US-12.01, US-08.01

_Technical Notes:_ The behavior parameters are specified in System Architecture section six.

_INVEST:_ All criteria satisfied.

**Epic E12 total: 8 story points.**

---

## 20. Epic E13: Admin Dashboard Frontend

### Epic goal

Implement the Next.js 15 admin dashboard with its seven main navigation entries (Home, Members, Credentials, Doors, Policies, Audit, Settings), the authentication flow, the permission-aware rendering, and the SSE-based real-time event feed on the home page. The dashboard is the primary user-visible artifact of the system.

### Business value

The dashboard is what reviewers see first when they bring up the system. The brand palette discipline, the anti-AI-aesthetics positioning, and the dense-functional layout are all portfolio-signal elements that the dashboard surfaces.

### Epic-level acceptance

The epic is complete when an administrator can perform every operation specified in PFD section fourteen through the dashboard UI, with appropriate empty states, loading skeletons, and error handling.

### User stories

**US-13.01: Initialize Next.js 15 project with OpenAPI client generation**

_Format:_ As the implementer, I want the Next.js 15 project initialized with the OpenAPI specification driving the typed API client, so that the dashboard and the backend share a single source of truth.

_Description:_ Initialize the Next.js project with TypeScript, Tailwind, and shadcn-ui. Configure `openapi-typescript` to generate the API client from the OpenAPI specification. Establish the brand palette in the Tailwind configuration with the imperial purple primary, lavender tint secondary, magenta-plum critical, and sage safe colors.

_Acceptance Criteria:_

1. Given the Next.js project, When `npm run build` succeeds, Then the production build artifacts are produced without errors.
2. Given the OpenAPI specification, When the client generation runs, Then the generated TypeScript types compile and are importable by dashboard components.
3. Given the Tailwind configuration, When a component uses the `bg-primary` class, Then it renders in the imperial purple color from the brand palette.

_Story Points:_ 3

_Dependencies:_ US-02.03

_Technical Notes:_ The Next.js 15 setup follows the App Router pattern. The shadcn-ui components are themed via the Tailwind config.

_INVEST:_ All criteria satisfied.

---

**US-13.02: Implement login flow and session-aware routing**

_Format:_ As an administrator, I want to log in to the dashboard with my email and password and have my session remembered across reloads, so that I do not have to re-authenticate frequently.

_Description:_ Implement the `/login` page with the form fields, the credential submission to POST `/api/v1/auth/login`, the session cookie handling, and the redirect logic. Add the middleware that intercepts unauthenticated requests to any other route and redirects to login while preserving the original URL.

_Acceptance Criteria:_

1. Given a logged-out user, When `/members` is visited, Then the user is redirected to `/login?return_to=/members`.
2. Given valid credentials, When the login form is submitted, Then the user is redirected to the original URL with the session cookie set.
3. Given an expired session detected via 401, When any page is accessed, Then the user is shown a "session expired" notice with a re-login link.

_Story Points:_ 3

_Dependencies:_ US-13.01

_Technical Notes:_ The session cookie is HttpOnly so the JavaScript cannot read it; the Next.js middleware uses the cookie's presence as a heuristic and the API call confirms.

_INVEST:_ All criteria satisfied.

---

**US-13.03: Implement Members page with list, search, and detail**

_Format:_ As an administrator, I want to view, search, and inspect members in my tenant through the dashboard, so that I can manage enrollments.

_Description:_ Implement `/members` (list with search) and `/members/[id]` (detail). Use the keyset pagination cursor for the list, with infinite-scroll-style pagination. The detail page shows the member's identity, status history, associated credentials, assigned policies, and recent access events.

_Acceptance Criteria:_

1. Given the page is loaded, When the data is fetching, Then a skeleton matching the eventual list shape is shown.
2. Given the data has arrived and there are zero members, When the list renders, Then an empty state with a "Create your first member" CTA is shown.
3. Given a member detail page is visited, When the data has loaded, Then all sub-sections (credentials, policies, recent events) render correctly.

_Story Points:_ 5

_Dependencies:_ US-13.02, US-04.02

_Technical Notes:_ The page follows the dashboard touchpoints specified in PFD section three.

_INVEST:_ All criteria satisfied.

---

**US-13.04: Implement Credentials, Doors, and Policies pages**

_Format:_ As an administrator, I want to manage credentials, doors, and policies through the dashboard, so that I have parity between API capabilities and UI capabilities.

_Description:_ Implement the three sets of pages following the same pattern as Members: list with search, detail page, create form, edit form. Include the effective-permissions preview on member detail (from US-05.04).

_Acceptance Criteria:_

1. Given the Credentials page, When a credential is revoked, Then the row updates optimistically and a toast notification confirms the action.
2. Given the Policies page, When a new policy is created with three time windows and two doors, Then the policy detail page reflects all four entities correctly.
3. Given the Doors page, When a door's status changes (via heartbeat/timeout), Then the status indicator updates in near-real-time via polling.

_Story Points:_ 5

_Dependencies:_ US-13.03

_Technical Notes:_ Polling for door status uses ten-second intervals per System Architecture section fourteen.

_INVEST:_ All criteria satisfied.

---

**US-13.05: Implement Home page with SSE real-time event feed**

_Format:_ As an administrator, I want to see access events streaming in real time on the dashboard home page, so that I can monitor activity at a glance.

_Description:_ Implement the `/` (Home) page with summary tiles (online doors count, active members count, recent decision rate) and the real-time event feed via Server-Sent Events. The feed shows the most recent twenty-five access events with appropriate styling (deny in critical color, grant in neutral).

_Acceptance Criteria:_

1. Given the home page is open, When an access decision is made, Then the new event appears at the top of the feed within one second.
2. Given the SSE connection is lost, When the connection state changes, Then a small indicator shows reconnecting and the feed resumes when the connection is restored.
3. Given a deny event is in the feed, When the event is inspected, Then the reason code is displayed in a way that distinguishes it from grants (color, prefix, or icon).

_Story Points:_ 5

_Dependencies:_ US-13.04

_Technical Notes:_ The SSE consumer in the browser uses the standard EventSource API. The server-side SSE endpoint for the dashboard is similar to but distinct from the reader-facing SSE in US-07.02.

_INVEST:_ All criteria satisfied.

---

**US-13.06: Implement Audit page with filters and CSV export**

_Format:_ As an auditor, I want to filter the audit log by multiple dimensions and export results, so that I can produce compliance reports.

_Description:_ Implement `/audit` with the filter controls (date range, doors, members, decision, reason codes), the results table with keyset pagination, the drill-down detail view for individual events, and the export-to-CSV button.

_Acceptance Criteria:_

1. Given filters are applied, When the URL is shared, Then opening the URL in another browser reproduces the filtered view.
2. Given the export button is clicked, When the export is requested, Then the user is shown progress and the download link appears when the file is ready.
3. Given a conflict-flagged event (offline reconciliation discrepancy), When the event renders in the list, Then it is visually distinguished from regular deny events.

_Story Points:_ 5

_Dependencies:_ US-13.05, US-09.02

_Technical Notes:_ The filter state is encoded in URL query parameters for shareability.

_INVEST:_ All criteria satisfied.

---

**US-13.07: Implement Settings pages (Users, Subscriptions, Export, Tenant)**

_Format:_ As a tenant owner, I want to manage users, subscriptions, exports, and tenant settings through the dashboard, so that all configuration is accessible without CLI.

_Description:_ Implement the four settings sub-pages under `/settings` with their respective CRUD operations. Use permission-aware rendering: owner-only controls are not rendered for manager or auditor sessions.

_Acceptance Criteria:_

1. Given a manager-role session, When `/settings/users` is visited, Then the page returns 403 or redirects to the dashboard home with an "insufficient role" notice.
2. Given an owner-role session, When a new user is invited, Then the new user appears in the list with the assigned role.
3. Given the dead-letter inspection view, When dead-lettered deliveries exist, Then they are listed with retry and discard actions.

_Story Points:_ 5

_Dependencies:_ US-13.04, US-10.01, US-11.01

_Technical Notes:_ Permission-aware rendering uses both server-side (route protection) and client-side (button visibility) enforcement.

_INVEST:_ All criteria satisfied.

**Epic E13 total: 31 story points.**

---

## 21. Epic E14: Demo, Documentation, and Handover

### Epic goal

Produce the final deliverables that make the OpenGate repository portfolio-ready: the comprehensive README, the architecture-decision records, the demo video, the Grafana dashboard configurations, and the final handover document that captures the project's outcomes and lessons.

### Business value

A working system without polished documentation and a compelling demo is not a portfolio artifact. The epic transforms the working codebase into a presentable artifact that a hiring manager can evaluate in fifteen minutes.

### Epic-level acceptance

The epic is complete when the repository's README produces a positive first impression in two minutes of skim, the demo video runs for under six minutes and demonstrates all six PRD use case scenarios, and the Grafana dashboards render meaningful operational views out of the box.

### User stories

**US-14.01: Write comprehensive README and quick-start guide**

_Format:_ As a reviewer, I want a clear and compelling README that lets me understand and run OpenGate in fifteen minutes, so that I can evaluate the portfolio artifact efficiently.

_Description:_ Write the README following the Stripe/DigitalOcean documentation style established in the project's preferences. Include the project pitch, the architectural overview, the quick-start steps for bringing up the Docker Compose stack, and links to all planning documents.

_Acceptance Criteria:_

1. Given a fresh clone, When the README's quick-start steps are followed, Then the system is running locally within fifteen minutes.
2. Given the README is opened, When the content above the fold is scanned, Then the reviewer can articulate what OpenGate is and why it exists.
3. Given the README, When the architecture section is read, Then the reviewer can identify the twelve demonstrated patterns from the PRD.

_Story Points:_ 3

_Dependencies:_ All other epics

_Technical Notes:_ The documentation standard from the project preferences applies in full.

_INVEST:_ All criteria satisfied.

---

**US-14.02: Configure preconfigured Grafana dashboards**

_Format:_ As an operator, I want Grafana dashboards ready out of the box that show command latency, decision latency, projection lag, webhook delivery, and reader connectivity, so that I do not have to build them myself.

_Description:_ Create the Grafana dashboard JSON files and bundle them in the Docker Compose configuration's auto-provisioning directory. The dashboards include panels for the metric set defined in System Design section eight.

_Acceptance Criteria:_

1. Given the Docker Compose stack is up, When the Grafana UI is opened, Then four preconfigured dashboards are listed.
2. Given the system has been generating traffic, When the dashboards are opened, Then panels show populated data.
3. Given a dashboard panel, When the panel's edit view is opened, Then the underlying PromQL query is readable.

_Story Points:_ 3

_Dependencies:_ US-06.04 and other instrumentation stories

_Technical Notes:_ The dashboard JSON is committed under `docker/grafana/provisioning/`.

_INVEST:_ All criteria satisfied.

---

**US-14.03: Record demo video showcasing six PRD scenarios**

_Format:_ As a hiring manager, I want a short demo video that shows OpenGate solving real scenarios end-to-end, so that I can evaluate the system without setting it up locally.

_Description:_ Record a screen capture of the demo runner exercising the six use case scenarios from PRD section five. The video has narration explaining what each scenario demonstrates and why it matters. Final length is under six minutes.

_Acceptance Criteria:_

1. Given the video is watched start to finish, When it ends, Then the viewer has seen all six PRD scenarios demonstrated.
2. Given each scenario, When the demonstration occurs, Then the dashboard, the audit log, and the observability dashboards are shown reflecting the scenario's effects.
3. Given the video is uploaded, When its URL is included in the README, Then the link resolves and plays in standard browsers.

_Story Points:_ 3

_Dependencies:_ US-14.01, US-14.02

_Technical Notes:_ The demo runner is a small CLI that scripts the scenarios via API calls and pauses for narration.

_INVEST:_ All criteria satisfied.

---

**US-14.04: Write final handover document and project retrospective**

_Format:_ As the future maintainer (or the present author six months later), I want a concise handover document capturing decisions, gotchas, and lessons learned, so that the project context is preserved beyond the immediate development window.

_Description:_ Write the final handover document summarizing what was built, what was deferred, what failed and was reworked, and what an extender of the project would need to know to continue. Include a brief retrospective on the planning process itself.

_Acceptance Criteria:_

1. Given the handover document, When a new contributor reads it, Then they can identify the next reasonable extension to the project.
2. Given the retrospective section, When it is read, Then it identifies at least three concrete lessons learned during the sixty-day window.
3. Given the document is committed to the repository, When the README is updated, Then a link to the handover is present in the README.

_Story Points:_ 2

_Dependencies:_ US-14.03

_Technical Notes:_ The handover follows the same anti-fluff style as the planning documents.

_INVEST:_ All criteria satisfied.

**Epic E14 total: 11 story points.**

---

# Part III — Sprint plan, risks, and closing

## 22. Sprint plan

The fourteen epics are sequenced across twelve sprints based on dependency order, sprint capacity of approximately nine story points, and the strategic principle that the architectural foundations (E1, E2, E3) must be complete before any domain epic begins. The plan also front-loads the most uncertain work (E6 decision path, E8 reconciliation) into the middle sprints so that schedule risk is detected with time to recover.

The table below summarizes the sprint plan. Each row shows the sprint number, the calendar week within the sixty-day window, the primary epics worked on in that sprint, the story IDs pulled, and the total story points. Stories that span sprints are split across rows.

| Sprint | Week | Epics              | Stories                                                    | Points |
| ------ | ---- | ------------------ | ---------------------------------------------------------- | ------ |
| S1     | 1    | E1                 | US-01.01, US-01.02, US-01.03, US-01.04                     | 12     |
| S2     | 2    | E1, E2             | US-01.05, US-01.06, US-02.01, US-02.02                     | 10     |
| S3     | 3    | E2, E3             | US-02.03, US-02.04, US-02.05                               | 15     |
| S4     | 4    | E2, E3             | US-03.01, US-03.02, US-03.03                               | 9      |
| S5     | 5    | E3, E4             | US-03.04, US-03.05, US-03.06                               | 15     |
| S6     | 6    | E4, E5             | US-04.01, US-04.02, US-04.03, US-04.04                     | 16     |
| S7     | 7    | E5, E6             | US-05.01, US-05.02, US-05.03, US-05.04                     | 13     |
| S8     | 8    | E6, E7             | US-06.01, US-06.02, US-06.03, US-06.04                     | 13     |
| S9     | 9    | E7, E8             | US-07.01, US-07.02, US-07.03, US-08.01                     | 16     |
| S10    | 10   | E9, E10            | US-09.01, US-09.02, US-09.03, US-10.01                     | 16     |
| S11    | 11   | E10, E11, E12, E13 | US-10.02, US-10.03, US-11.01, US-11.02, US-12.01, US-13.01 | 22     |
| S12    | 12   | E12, E13, E14      | US-12.02, US-13.02 — US-13.07, US-14.01 — US-14.04         | 44     |

The total points in the plan is 201, verified two ways that agree: the sum of the 55 per-story estimates is 201, and the sum of the 14 epic totals is 201. This exceeds the sprint capacity of 108 (twelve sprints at nine points) — a genuine over-allocation of 93 points, not an artifact of double-counting, since each story appears exactly once. The over-allocation is absorbed by the planned E13 dashboard scope reduction (risk R-02): the dashboard pages carry substantial total work concentrated in the final sprints, and several E13 page-stories are expected to slip beyond the sixty-day window and become known scope-reduction items, as documented in the risk register in section twenty-three.

The dependency chain is verified by inspecting the dependencies field of each story and confirming that every story's dependencies are completed in or before its assigned sprint. The verification was performed manually during plan construction and is documented as a deferred task to be re-verified after each sprint review.

---

## 23. Risk register

The risks below are documented in a standard format with identifier, description, impact (high, medium, low), likelihood (high, medium, low), mitigation strategy, and triggering condition that would cause the risk to materialize.

**R-01: Sprint capacity underestimated for solo-developer mode.** Impact high, likelihood medium. The plan assumes nine story points per sprint but a solo developer working on architecturally-complex novel code may average closer to seven. Mitigation is to monitor velocity through sprint two and adjust the plan in the sprint three retrospective if velocity is below seven points on average. Triggering condition: cumulative completed story points after sprint three is below twenty-one.

**R-02: Dashboard scope exceeds remaining time.** Impact high, likelihood high. Epic E13 carries thirty-one story points concentrated in sprints eleven and twelve, which is double the sprint capacity. Mitigation is to reduce the dashboard scope to the essential pages (Home, Members, Credentials, Audit) and defer the Doors, Policies, Settings pages to a v1.1 if time pressure materializes. Triggering condition: less than fifteen story points completed across sprints eleven and twelve through the end of sprint eleven.

**R-03: Postgres LISTEN/NOTIFY scalability on SSE fanout.** Impact medium, likelihood low. The pattern works correctly at the project's scale, but a production deployment with many concurrent SSE consumers might exhibit issues. Mitigation is to limit demonstrated reader count to fifty during the demo and to document the architectural alternative (NATS or Redis Streams) as a future scaling path. Triggering condition: SSE fanout latency exceeds two seconds at the demo scale.

**R-04: Argon2id parameters too aggressive for development machine.** Impact low, likelihood medium. The OWASP parameters target production-grade hardware; a development laptop may experience login latency above one second. Mitigation is to reduce parameters for the development environment via a separate configuration profile, with full parameters retained for production. Triggering condition: login takes longer than one second on the development environment.

**R-05: River library breaks compatibility in a point release.** Impact medium, likelihood low. River is a relatively young library and could introduce a breaking change during the project window. Mitigation is to pin the River version in go.mod and to monitor the project's release notes weekly. Triggering condition: River releases an incompatible version that go modules picks up automatically.

**R-06: OpenAPI specification drift between dashboard and backend.** Impact medium, likelihood medium. The dashboard generates its client from the spec, but the spec and the actual server validators can drift if the spec is edited without updating both sides. Mitigation is to add a CI step that asserts the spec's request validators match the actual server's behavior via a contract test. Triggering condition: a dashboard request fails validation that should have succeeded based on the spec.

**R-07: Goose migration test runtime exceeds CI budget.** Impact low, likelihood low. The migration test that runs up-down-up for every migration may slow CI as the migration count grows. Mitigation is to add a flag that runs the migration test only on the migration files changed in the current PR. Triggering condition: total CI time exceeds ten minutes.

**R-08: Event store grows unboundedly during testing.** Impact medium, likelihood medium. Without retention, the events table grows monotonically, and a long-running development environment may accumulate enough events to slow query performance. Mitigation is to add a development-only seed script that truncates and reseeds the database, and to document the truncation procedure in the README. Triggering condition: developer reports slow query performance after several weeks of development use.

**R-09: Demo video production takes longer than estimated.** Impact medium, likelihood medium. Recording, narrating, and editing a six-minute demo video commonly takes ten or more hours, not the eight estimated for US-14.03. Mitigation is to start the demo runner development in sprint ten so the script is ready when the video recording begins. Triggering condition: video work has not started by the end of sprint eleven.

**R-10: Reviewer is unfamiliar with one of the patterns.** Impact low, likelihood medium. The portfolio context depends on a reviewer recognizing the patterns demonstrated; a reviewer unfamiliar with, say, event sourcing might not appreciate its presence. Mitigation is to write the README in a way that explains each pattern briefly and connects it to industry practice. Triggering condition: feedback from early reviewers indicates patterns are not recognized.

---

## 24. Dependency matrix

The matrix below shows the inter-story dependencies in tabular form. Each row lists a story and the stories it depends on. The matrix is consolidated from the per-story dependencies field; it serves as a single point of reference for sprint planning verification.

| Story    | Direct dependencies          |
| -------- | ---------------------------- |
| US-01.01 | none                         |
| US-01.02 | US-01.01                     |
| US-01.03 | US-01.01                     |
| US-01.04 | US-01.01                     |
| US-01.05 | US-01.01, US-01.02           |
| US-01.06 | US-01.01                     |
| US-02.01 | US-01.04                     |
| US-02.02 | US-01.01                     |
| US-02.03 | US-02.01, US-02.02           |
| US-02.04 | US-02.03                     |
| US-02.05 | US-02.01, US-02.03, US-01.04 |
| US-03.01 | US-02.05                     |
| US-03.02 | US-01.01                     |
| US-03.03 | US-03.01, US-03.02           |
| US-03.04 | US-03.01, US-01.06           |
| US-03.05 | US-03.04, US-02.05           |
| US-03.06 | US-03.05                     |
| US-04.01 | US-03.03, US-02.05           |
| US-04.02 | US-04.01                     |
| US-04.03 | US-04.01                     |
| US-04.04 | US-04.03                     |
| US-05.01 | US-04.01                     |
| US-05.02 | US-05.01, US-04.01           |
| US-05.03 | US-05.02                     |
| US-05.04 | US-05.03                     |
| US-06.01 | US-02.02                     |
| US-06.02 | US-05.01                     |
| US-06.03 | US-05.03, US-06.01, US-03.06 |
| US-06.04 | US-06.03                     |
| US-07.01 | US-06.02                     |
| US-07.02 | US-07.01, US-06.01           |
| US-07.03 | US-07.02, US-06.02           |
| US-08.01 | US-06.03                     |
| US-09.01 | US-04.02, US-05.01, US-06.03 |
| US-09.02 | US-09.01                     |
| US-09.03 | US-09.02                     |
| US-10.01 | US-02.04                     |
| US-10.02 | US-10.01, US-03.04           |
| US-10.03 | US-10.02                     |
| US-11.01 | US-03.04                     |
| US-11.02 | US-11.01                     |
| US-12.01 | US-06.02                     |
| US-12.02 | US-12.01, US-08.01           |
| US-13.01 | US-02.03                     |
| US-13.02 | US-13.01                     |
| US-13.03 | US-13.02, US-04.02           |
| US-13.04 | US-13.03                     |
| US-13.05 | US-13.04                     |
| US-13.06 | US-13.05, US-09.02           |
| US-13.07 | US-13.04, US-10.01, US-11.01 |
| US-14.01 | (all other epics complete)   |
| US-14.02 | US-06.04                     |
| US-14.03 | US-14.01, US-14.02           |
| US-14.04 | US-14.03                     |

The matrix has no cycles (verified by topological sort during plan construction). Every story can be reached from US-01.01 through a chain of dependencies, confirming that the plan is connected.

---

## 25. Deferred decisions

The decisions deferred from this document, to be resolved during implementation execution, are enumerated below.

The exact wireframes for each dashboard page are not specified in this document; they will be produced as low-fidelity sketches during the implementation of each E13 story and iterated as the implementation progresses.

The exact contents and format of the demo video script are not specified; they will be drafted during sprint ten as preparation for the recording in sprint twelve.

The exact text of the OpenAPI specification documentation is not specified; the spec will accrete iteratively as endpoints are added in each epic, with documentation written alongside.

The exact contents of the ADR (Architecture Decision Records) directory, which captures the rationale for major decisions, is not specified; ADRs will be written as decisions are made or revised during implementation. The intent is that the ADR directory accumulates approximately fifteen to twenty ADRs over the project window.

The exact strategy for handling production deployment beyond the Docker Compose stack — Kubernetes manifests, Helm charts, infrastructure as code — is deferred indefinitely. The current scope is portfolio sample only; production deployment is out of scope.

---

## 26. Document status and closing

This document is version one point zero of the Implementation Plan for OpenGate. The document and all five preceding planning documents (PRD, PFD, System Architecture, System Design, Database Schema) now collectively constitute the complete planning corpus for the project. The next artifact in the project lifecycle is the codebase itself, the production of which is the work that the present document plans.

The total story points across all fourteen epics is two hundred one. The total sprint capacity is one hundred eight. The plan therefore explicitly carries a known overallocation, justified by the principle that E13 dashboard work can be scope-reduced or deferred to v1.1 if velocity does not support its full completion. The author proceeds to sprint one with this understanding.

The first sprint, S1, begins on the day after this document is accepted. Sprint planning for S1 consists of pulling the four stories US-01.01, US-01.02, US-01.03, US-01.04 with a total of twelve story points, conducting the final INVEST check on each pulled story, and confirming that all dependencies are satisfied (US-01.01 has no dependencies, the other three depend only on US-01.01 which will be completed first within the sprint). The sprint goal is "Project bootstrapped and ready for domain implementation." The sprint review at the end of S1 demonstrates the project building, the Docker Compose stack running, and the first migration applied.

The author and the assistant proceed to implementation in the next working session, with the System Design and Database Schema documents serving as the primary references during execution and the present document serving as the navigation map across the sprints.
