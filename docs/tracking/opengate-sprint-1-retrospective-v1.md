# OpenGate Sprint 1 Retrospective

**Document version:** 1.0
**Sprint:** Sprint 1 (Epic E1 — Project Bootstrap and Foundations)
**Sprint window:** 2026-05-28 (single working session, LLM-assisted execution)
**Stories delivered:** US-01.01, US-01.02, US-01.03, US-01.04
**Story points committed / delivered:** 12 / 12
**Author:** OpenGate coordination team

## Purpose

This document is a backward-looking analytical record of how Sprint 1 unfolded. It complements the forward-looking Sprint 1 Handoff document, which is intended for a fresh Claude session entering Sprint 2 without prior conversation context. Where the Handoff is operational and tight, this Retrospective is reflective and long — it captures velocity reality, story-level outcomes with nuance, coordinator and executor observations, errors caught during the sprint and how they were caught, and recommendations for Sprint 2 cadence.

The retrospective is written in the engineer-to-engineer register the OpenGate project standard requires. No marketing language, no adjectives of self-congratulation, no padding. Where something went well, it is named; where something went poorly, it is named just as plainly.

## Sprint goal recap

Sprint 1 was framed as the foundations sprint — the work of taking an empty repository and producing a compilable, lintable, dockerized, migration-capable shell of an application against which all subsequent domain epics can build. Concretely, four stories were committed: US-01.01 to establish the hexagonal Go module layout, US-01.02 to bring in development tooling and static analysis, US-01.03 to stand up the Docker Compose stack with Postgres and observability services, and US-01.04 to install the goose migration mechanism along with the first migration. The four stories aggregate to 12 story points against a stated sprint capacity of 9 — a 33 percent overcommit acknowledged at sprint start as a known violation of the R-01 risk trigger.

## Delivery summary

All four committed stories were delivered. Zero carry-over to Sprint 2. The codebase at sprint close holds five commits on the `main` branch: the initial planning corpus commit, and one Conventional Commits-formatted commit per story. The git history is linear and clean. No squash, no force-push, no rebasing was used; each story is preserved as its own commit with its own narrative body documenting deviations from prompt and adaptations made during execution.

The full delivery scope compressed into a single LLM-assisted working session of roughly six hours of focused collaboration between the human arbiter and two cooperating Claude instances. This calendar compression is a property of the execution mode, not a sustainable solo-developer pace, and the velocity numbers in the next section must be read with that caveat.

## Velocity analysis

Planned capacity for Sprint 1 was 9 story points. Delivered scope was 12 story points. On its face this is a 133 percent velocity ratio. The honest interpretation requires three caveats.

First, the original 12-point commitment was knowingly above capacity at sprint start, flagged as a present-tense instance of risk R-01 from the Implementation Plan's risk register. Delivering all 12 does not establish that capacity is 12; it establishes only that we operated above the stated sustainable rate. Second, the calendar compression to a single session reflects LLM-assisted execution where the human spends roughly two to five minutes per story-equivalent of focused review rather than days of original drafting. Velocity in this mode is not comparable to typical solo-developer cadence and should not be projected forward as a baseline. Third, two of the four stories required at least one round-trip beyond the initial executor pass — US-01.03 needed re-verification due to a coordinator error in the validation recipe, and US-01.04 needed a micro-correction prompt to harden action validation. The "12 delivered" figure does not mean four single-shot completions.

For the R-01 risk trigger, the relevant question is the cumulative delivery rate after Sprint 3, not the Sprint 1 figure in isolation. R-01 fires if the cumulative across the first three sprints falls below 21 story points, which would imply an average sustainable velocity below 7 SP per sprint and would invalidate the remainder of the sprint plan. Sprint 1 delivered well above that threshold, so the trigger is not firing yet. Re-evaluation point: end of Sprint 3.

## Story-by-story outcomes

**US-01.01 — Initialize Go module and hexagonal project layout (2 SP).** Reference story for velocity calibration. Delivered single-shot with no executor correction iterations. Established the canonical layout under `internal/` with `domain`, `application`, `ports/{inbound,outbound}`, and `adapters/{inbound,outbound}` packages, each carrying a `doc.go` file that documents the package's import constraints in its package-level Godoc comment. This pattern institutionalizes architectural boundaries from day one: every contributor reading any package's source learns the boundary rules in the first five lines without consulting external documentation.

The module path used was `github.com/JelenaMarjanovic/opengate`, which is the case-sensitive correct value. The original Implementation Plan and System Design documents reference `github.com/jefimarjanovic/opengate`, an artifact of an earlier handle. The discrepancy was identified and consciously locked to the GitHub-authoritative casing during the sprint, with the understanding that the planning documents require a v1.1 revision to update the references. Go modules treat path casing as significant for hash computation and module cache layout; tolerating mixed casing would silently fracture builds.

The `cmd/opengate/main.go` skeleton uses exit code 2 for the no-subcommand case, following the Unix convention that exit code 2 signals usage error. A sanity check during validation surfaced a useful Go toolchain nuance: `go run` normalizes its own exit code to 1 when the program it runs exits non-zero, while logging the program's actual exit code as text. This is documented but easy to miss, and is the reason CI exit-code branching must use a built binary rather than `go run`.

**US-01.02 — Configure development tooling and static analysis (2 SP).** Delivered single-shot. Three substantive decisions were made consciously during this story, each documented in the commit body.

The first decision substituted lefthook for the pre-commit/pre-commit Python framework that the original Implementation Plan technical note specified. Justification: lefthook is a Go-native single static binary that does not introduce a Python runtime dependency into a portfolio Go project, runs hooks in parallel (measurably faster than the Python pre-commit framework's default sequential execution), and has a cleaner YAML configuration. Trade-off accepted: lefthook's community hook catalog is smaller than pre-commit's, but the OpenGate hook chain consists of three explicit local Go commands (`gofmt`, `go vet`, optional `golangci-lint`), not community plugins, so the smaller catalog is not a constraint.

The second decision installed `golangci-lint` v2.12.2 via the official install script into a project-local `./bin/` directory, deliberately avoiding both `go install` and the Go 1.24 `tool` directive. This follows upstream's explicit documented guidance against the tools-pattern approach for `golangci-lint` specifically, due to dependency-pin conflicts the tool brings in via its many internal linter dependencies. Lefthook, by contrast, is installed via the `tool` directive because it is Go-native and does not have that conflict surface. The asymmetry is intentional and documented in the Makefile comments.

The third decision locked the linter set to a curated nine: `errcheck`, `govet`, `staticcheck`, `revive`, `gosec`, `gocritic`, `misspell`, `godot`, and `gocyclo`, plus `gofmt` as the formatter under v2's formatter slot. The set is strict but not extreme; configurations like the noisy `wsl` whitespace linter and the test-noisy `gomnd` were deliberately excluded for being more friction than signal at this stage.

A side adjustment was needed: `cmd/opengate/main.go` had to gain a package-level doc comment to satisfy `revive`'s `package-comments` rule, since the original US-01.01 stub did not have one. This was bundled into US-01.02 rather than spawning a correction story.

**US-01.03 — Docker Compose stack with Postgres and observability (5 SP).** Delivered with one re-verification cycle (no executor correction prompt required, but a coordinator validation error required a second pass). This was the most complex story of the sprint by component count and decision density.

Five substantive decisions were made consciously before the prompt was written. The Postgres image was selected as `postgres:16.14-bookworm`, the Debian-based variant rather than Alpine, in order to use glibc rather than musl libc and thereby avoid known locale and collation edge cases that affect text sorting, comparison, and indexing. The OpenTelemetry Collector distribution was selected as `otel/opentelemetry-collector-contrib:0.145.0` rather than the core distribution, because the Prometheus exporter required by the architecture does not exist in the core build. The Caddy reverse proxy was configured to use the `tls internal` directive in development, producing a self-signed certificate issued by a locally-trusted CA, so that the TLS termination path is exercised in development rather than bypassed. Grafana authentication was configured as anonymous Viewer for demo convenience, with the admin password held in environment variables and required for any administrative mutation — a hybrid that signals security awareness without obstructing demo flow. Network topology was split into two bridge networks, `edge` and `internal`, with Postgres attached only to `internal` and verified unreachable from the edge-facing reverse proxy.

Two executor adaptations to the prompt were made and reported back. The Caddy host port mapping was changed from `80/443` to `8080/8443` because the host environment runs Apache on port 80, which would have caused the compose stack to fail at startup. The executor documented this adaptation inline in a docker-compose.yml comment, which is exemplary practice — the deviation is discoverable to any future reader of the file. The Grafana image pin was silently bumped from `13.0.0` (specified in the prompt) to `13.0.1` (current latest patch); this was flagged during retrospective review and consciously accepted as the new pin, but it represents a pin-discipline lapse that future executor prompts should explicitly guard against.

A coordinator validation error was caught and corrected: the validation recipe used port `443` in URLs while Caddy was actually bound to `8443`, causing `wget -q` commands to silently produce empty output (the `-q` flag suppresses the connection error). Diagnosis required a second validation pass with corrected port numbers. The error compounded with a latent inconsistency in the prompt itself: the `GF_SERVER_ROOT_URL` environment variable still pointed at port `443` after the port remap, which would have caused Grafana's self-generated absolute URLs to point at a dead port. Both were corrected before commit.

A coordinator wrong prediction was caught and owned: the coordinator predicted that Grafana's health endpoint would fail due to the `ROOT_URL` mismatch. The prediction was incorrect — the health endpoint does not depend on `ROOT_URL`. The prediction's underlying observation was still valid (the mismatch is real and affects redirects), but the specific symptom predicted was wrong. Lesson recorded.

**US-01.04 — Goose migration mechanism and initial tenants migration (3 SP).** Delivered with one micro-correction cycle. Four substantive decisions were made before the prompt.

The goose Provider API was selected over the older global API to avoid process-global mutable state, particularly important because the round-trip test must run against an isolated container in a manner that does not race with other test goroutines. The pgx v5 driver was used via its `database/sql` adapter (`pgx/v5/stdlib`) rather than introducing `lib/pq` solely for migration use; one driver in the project keeps the codebase smaller and avoids subtle behavioral differences. The subcommand parser was implemented in stdlib `flag` with manual dispatch rather than adopting `cobra` or `urfave/cli`; with only one subcommand currently and no compelling reason to anticipate the framework overhead, the YAGNI principle was applied. The round-trip test was implemented using `testcontainers-go` rather than reusing the existing compose Postgres, so the test is self-contained and CI-portable; a test that depends on the developer having run `docker compose up` is fragile and not a real test.

A scope decision was made and recorded: US-01.04 delivers the migration mechanism plus the first migration only (the `tenants` table). It does not deliver the full database schema. The reasoning, drawn from the schema document itself: Row-Level Security policies require the application to set `current_setting('app.current_tenant_id')` before each query, a mechanism that does not exist yet and arrives with the connection layer in a later story; introducing RLS policies before the mechanism that exercises them creates the appearance of a security layer without its substance, which is worse than not having the policies at all. View tables (`_view` suffix) are populated by projectors that do not exist yet; creating them empty serves no purpose. The honest discipline is migrations follow code, not migrations precede code by months.

Two executor adaptations were reported. The local variable name `fs` in `runMigrate` was renamed to `flags` to avoid shadowing the imported `io/fs` package; the executor's stated reason ("the prompt's exact code wouldn't compile") was slightly inaccurate (the shadowing was localized and the code as written did compile), but the rename is good defensive practice and was accepted. The deferred `db.Close()` was wrapped in a closure to handle the returned error, satisfying `errcheck` without disabling the rule.

A coordinator bug in the prompt was caught: action validation in `runMigrate` happened after the database connection was established. As a consequence, a bogus action like `migrate bogus` against a broken DSN reported the DSN parse error first, before reaching the action validation. The fix was a five-line micro-correction prompt that moved action validation upfront. Post-fix verification confirmed that an unknown action with no `OPENGATE_DATABASE_URL` set now correctly reports `unknown action "bogus"` and exits non-zero, without any database connection attempt.

The validation also surfaced a Postgres operational gotcha worth recording for future reference. The Postgres Docker image honors the `POSTGRES_PASSWORD` environment variable only at first initialization of the data volume; subsequent changes to `.env` do not propagate to the existing volume's authentication state. This caused a password mismatch between the human's `.env` value and the password the container actually accepted. The executor handled this resourcefully via `ALTER USER opengate WITH PASSWORD '...'`, but the underlying gotcha applies to anyone who modifies `.env` after the first stack startup. The clean reset is `docker compose down -v` followed by `up -d`, which uses the current `.env` for fresh initialization. This should be documented in the README when US-14.x lands.

A second small finding: the Docker bridge network IP of the Postgres container is not stable across `down -v` and `up` cycles, because the network is recreated and IP assignment is not deterministic. Validation commands that hardcode the bridge IP must re-inspect after any network teardown.

## Decisions made during sprint that revise the Implementation Plan

The following items represent conscious deviations from the Implementation Plan v1.0 that must be folded into a v1.1 revision when planning documents are next updated. They are listed here so the revision pass has a single source of truth.

First, the module path in System Design section 7 and Implementation Plan US-01.01 technical notes should be corrected from `github.com/jefimarjanovic/opengate` to `github.com/JelenaMarjanovic/opengate`, with case preserved exactly.

Second, the hook framework reference in Implementation Plan US-01.02 technical notes should be changed from `pre-commit/pre-commit` to `lefthook`, with the substitution rationale recorded.

Third, the `golangci-lint` installation pattern should be documented as "official install.sh into project-local ./bin/", explicitly noting upstream's guidance against `go install` and the `tool` directive for this specific tool.

Fourth, host port mappings should be noted as environment-dependent: `80/443` is the default but environments with conflicting services (such as host Apache or Nginx) require remapping to `8080/8443` with corresponding adjustment of any `ROOT_URL`-style environment variables.

Fifth, the Grafana image pin should be updated to `13.0.1` (current accepted pin), with a note that minor patch bumps are accepted consciously and not silently.

Sixth, the Tempo image pin should be recorded as `grafana/tempo:2.10.5` (latest 2.x at sprint time), with the explicit choice to stay on the 2.x line until the 3.0 config-schema breaking changes are evaluated.

Seventh, the Caddy image pin should be recorded as `caddy:2.11.3-alpine`.

Eighth, the scope of US-01.04 should be clarified in the technical notes: it delivers the migration mechanism plus the first migration only, not the full schema. Subsequent table migrations land in their respective domain epics under a "migrations follow code" discipline.

Independently of these story-level revisions, the numerical inconsistency previously identified in the Implementation Plan (narrative passages cite 61 stories and 196 total points; actual values are 55 stories and 201 points) remains pending fix in v1.1.

## Coordinator and executor dynamics

The three-party protocol from the planning-phase handoff (human as arbiter and validator, coordinator Claude as synthesizer of prompts, executor Claude in the IDE) functioned well across all four stories. Several observations are worth carrying into Sprint 2.

The coordinator's habit of articulating substantive decisions explicitly before writing prompts and waiting for human sign-off produced higher-quality prompts than would have been possible by guessing. The two-step pattern (decisions discussed → sign-off → prompt written) consumed an extra round-trip per story but eliminated executor confusion and rework. The cost is real but the return is higher; the pattern should continue.

The executor's report-back protocol — listing resolved dependency versions, noting adaptations from prompt code, and giving exact command outputs per acceptance criterion — was the basis on which the human could validate work without re-reading the entire codebase. The protocol should be reinforced for Sprint 2 with one additional ask: when the executor makes a unilateral substantive deviation (not just an adaptation forced by environment), the report should highlight the deviation explicitly rather than embed it in the commit body. The silent Grafana pin bump in US-01.03 is the case study; it was caught at validation but should have been surfaced earlier.

Coordinator errors during the sprint clustered in validation recipes rather than in prompt design: the port `443` versus `8443` mismatch in US-01.03 validation, the wrong prediction about `ROOT_URL` affecting the health endpoint, and the action-validation ordering bug in US-01.04. These are recoverable failure modes that show up in the loop, not show-stoppers. None of them required throwing away executor work; all were absorbed by re-verification or micro-correction prompts.

Executor resourcefulness was a positive surprise on two occasions. The bridge-IP technique discovered during US-01.04 AC2 validation (connecting directly to the Postgres container's Docker bridge IP rather than opening a compose port) was a cleaner solution than the coordinator's prompt suggested. The diagnosis and remediation of the password drift via `ALTER USER` was also unprompted and correct. Both were disclosed in the executor's summary report. This level of agency from the executor is desirable and should be encouraged in Sprint 2.

## Errors and how they were caught

Five distinct errors or failure modes occurred during Sprint 1, all caught and resolved before commit.

Error one was a coordinator validation recipe using port `443` for Caddy URLs after the executor had remapped Caddy to `8443`. It was caught when the validation commands returned silent empty output and the human flagged the missing data. Lesson: `wget -q` (quiet mode) hides connection errors and should not be used in validation recipes without paired exit-code checks. Going forward, validation recipes will use `; echo "exit=$?"` after every silent-on-success command, which was already the established discipline for build and lint commands but not yet applied to network commands.

Error two was a latent inconsistency in `GF_SERVER_ROOT_URL` after the port remap. It was caught during diagnosis of error one, by tracing through the related configuration. Lesson: when one environment-driven config value changes, the immediate next step is to sweep all related config for ripple effects. The discipline of "if you change X, name everything that references X" should be a standing reflex.

Error three was a coordinator wrong prediction: the claim that the Grafana health endpoint would fail due to the `ROOT_URL` mismatch. The prediction was wrong; the health endpoint passed. The underlying issue (mismatch is real, affects generated absolute URLs) was correctly identified, but the specific predicted symptom was wrong. Lesson: predictions about library behavior should be hedged or sourced. In this case, ROOT_URL's actual effect (generated absolute URLs for redirects, OAuth callbacks, share links) is documented in Grafana's own docs and could have been checked rather than reasoned about from first principles.

Error four was the action-validation ordering bug in `cmd/opengate/migrate.go`. The bug manifested as `migrate bogus` reporting a DSN parse error rather than an unknown-action error when the DSN was broken. It was caught when the human's validation showed a DSN error for an action that should have failed pre-connection. The fix was a five-line micro-correction prompt that moved action validation upfront. Lesson: argument validation should always precede resource acquisition; this is general engineering hygiene and the case study reinforces the principle.

Error five was the human's environmental issue with password drift between `.env` and the existing Postgres volume. The image's `POSTGRES_PASSWORD` semantic (honored only at first init) caused the running container to retain a previous password while `.env` held a different one. It was caught when the URL-format DSN failed to parse, then revealed during diagnosis. Resolution: `docker compose down -v` to wipe the volume, followed by fresh `up`. Lesson: the gotcha needs README documentation, and post-initial-init password rotation must be done via `ALTER USER` rather than environment variable change.

## Recommendations for Sprint 2

The cadence that produced Sprint 1's delivery should be continued without modification. The three-party protocol, the substantive-decisions-before-prompt pattern, the explicit acceptance criteria with verification commands, the human-as-final-validator role, and the executor's report-back format are all working. The temptation to streamline by collapsing the decision-discussion step into the prompt itself should be resisted — that step is where coordinator and human catch design problems before they become executor work.

Sprint 2 should begin in a fresh Claude session that reads only the Sprint 1 Handoff document, not the full planning corpus. This is the formal application of the context-compression discipline: the Handoff is the entry point, and any details beyond what it captures should be retrieved on demand from the source documents (PRD, PFD, System Architecture, System Design, Database Schema, Implementation Plan).

The R-01 risk trigger should be checked at the end of Sprint 3 against the 21-SP cumulative delivery threshold. Sprint 1 alone does not justify dismissing R-01; the next two sprints will reveal whether the velocity is sustained or whether the initial sprint's compressed timeline was an artifact of bootstrap simplicity.

The Implementation Plan v1.1 revision pass (eight items enumerated above plus the pre-existing numerical inconsistencies) should be batched into a single editing pass rather than spread across multiple sprints. It is technical debt on the planning side and does not block code work, but leaving it indefinitely creates the risk that a future Claude session reads the stale documents as authoritative. A reasonable scheduling target is the gap between Sprint 2 and Sprint 3.

For ad-hoc operational work against the compose stack, the bridge-IP technique discovered during US-01.04 should be used in preference to opening compose ports temporarily — it does not require modifying tracked configuration files. Documentation of this technique should land in the README when US-14.x ships.

## Closing note

Sprint 1 delivered its committed scope cleanly. Four story commits, zero carry-over, two micro-corrections, one coordinator wrong prediction, and a tidy git history. The codebase at sprint close is in a state where Sprint 2 can begin the first domain epic without preparatory work beyond reading the Handoff document. The work continues in Sprint 2.
