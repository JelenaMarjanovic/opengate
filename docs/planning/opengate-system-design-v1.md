# OpenGate — System Design Document

**Version:** 1.1
**Status:** Draft for review
**Document type:** System design (implementation-level patterns, code-level contracts, algorithmic decisions)
**Author:** Jelena Marjanović
**Date:** May 2026
**Predecessor documents:** opengate-prd-v1.md, opengate-pfd-v1.md, opengate-system-architecture-v1.md
**Successor document:** opengate-database-schema-v1.md (to be produced after this document is accepted)

---

## 1. How to read this document

This document is the fourth and most implementation-detailed layer in the OpenGate documentation set. The Product Requirements Document fixed the goals, the Product Feature Document decomposed those goals into capability areas, the System Architecture Document specified the components that realize the capabilities and the contracts between them, and the present document specifies how each component is implemented at the level of concrete patterns, algorithms, and code-level contracts. The document one layer below this one will be the Database Schema document, which specifies the exact SQL tables, indexes, and constraints, and the document below that will be the implementation plan that breaks the work into user stories and tasks.

The document is organized as a two-pass structure that follows an approach common in mature engineering organizations. The first pass, named _Foundations_ and occupying sections two through ten, is organized by architectural pattern. Each section in this pass takes one pattern (event sourcing, read model projection, idempotency, advisory-lock coordination, background job processing, hexagonal port contracts, observability instrumentation, authentication and session management, multi-tenant isolation) and specifies how that pattern is realized in OpenGate at the implementation level, independent of any specific component that uses it. The second pass, named _Component design_ and occupying sections eleven through twenty, is organized by the components defined in the System Architecture document. Each section in this pass is intentionally short and describes how the corresponding component composes the foundational patterns; it refers back to the first pass by section number rather than repeating the explanations. The third pass, named _Cross-cutting concerns at the implementation level_ and occupying sections twenty-one through twenty-three, covers a few topics that fit neither category cleanly: graceful shutdown sequencing, error handling, and configuration. The fourth pass is the closing section enumerating deferred decisions and document status. An index at the end of the document maps every significant concept to the section in which it is most fully developed, supporting both navigation by concept and navigation by component.

The audience for this document is the engineer who will implement OpenGate, which in the immediate term is the author herself and in the future is any reviewer who wants to verify that the implementation matches the design. The document does not assume that the reader knows the details of any specific Go library used in the implementation; references to libraries such as `pgx`, `sqlc`, `chi`, `river`, `casbin`, and `otel` include enough context that a reader unfamiliar with one of them can still understand the design choice being made. The document does assume that the reader has read the three predecessor documents; concepts introduced there are used here without re-explanation.

A note on the relationship between this document and the codebase. Several sections contain short Go code excerpts to illustrate a specific design decision such as a port interface shape or a struct field arrangement. These excerpts are illustrative and not normative; the normative source for the implementation is the Go code itself once it exists. When the code and the document disagree, the code wins, and the document is updated to match. The reverse direction, in which the document is authoritative and the code is expected to conform, applies only to the decisions explicitly stated as such in the architectural and product documents above this one.

---

# Part I — Foundations

## 2. Event sourcing implementation

Event sourcing in OpenGate means that every state-changing action in the domain produces an immutable event, the events are appended to a durable log in commit order, and current state is derived by folding the events for a given aggregate. The implementation in OpenGate follows the discipline closely and avoids the common shortcuts that compromise the audit guarantee that motivated the choice of pattern in the first place.

**The event envelope.** Every event in the system shares a common envelope independent of its specific payload. The envelope carries the event identifier (a UUIDv7, which combines timestamp ordering with random uniqueness and is the appropriate choice for an append-only log), the aggregate identifier and type to which the event belongs, the monotonically increasing sequence number of the event within its aggregate (the first event being sequence one), the global stream position assigned by the event store on append (used for ordering across aggregates when projectors consume), the tenant identifier (denormalized into the envelope for performance and for the row-level security policies described in section ten), the wall-clock timestamp at which the event was recorded, the event type as a stable string identifier such as `member.created.v1`, and the schema version embedded in the type name so that future migrations can coexist with old events. The envelope also carries a metadata block that records the correlation identifier of the originating command, the identity of the user or system that triggered the event, the OpenTelemetry trace context that was active at the time of the event, and the application version that produced the event. The metadata block exists for observability and forensic reasons; it does not participate in domain logic.

A simplified Go representation of the envelope, illustrative only and subject to refinement during implementation, looks like the following.

```go
// Event is the common envelope for every event in the system.
// The envelope is stable across event type versions; only the payload changes.
type Event struct {
    ID            uuid.UUID       // UUIDv7, time-ordered
    AggregateID   uuid.UUID       // the aggregate this event belongs to
    AggregateType string          // e.g. "member", "credential", "access"
    Sequence      int64           // 1-based, monotonic within aggregate
    StreamPosition int64          // global ordering, assigned on append
    TenantID      uuid.UUID       // denormalized for RLS and queries
    OccurredAt    time.Time       // event timestamp, UTC
    Type          string          // e.g. "member.created.v1"
    Payload       json.RawMessage // event-specific data, decoded by handler
    Metadata      EventMetadata   // correlation, trace context, user, version
}
```

**Loading an aggregate.** An aggregate is loaded by reading all events for its identifier in sequence order and applying each event to a zero-valued aggregate struct via a fold operation. The fold is implemented as a switch on the event type that invokes a dedicated handler per type; the handlers are pure and produce a new aggregate state from the previous state and the event. Loading is therefore deterministic and reproducible: the same event log produces the same aggregate state every time. This determinism is what makes event sourcing useful as an audit substrate; the same property also makes testing trivial because a test can construct any aggregate state by appending a sequence of events.

**Concurrency control on append.** When a command handler modifies an aggregate, it loads the aggregate, validates the command against the current state, and produces one or more new events. The append to the event store is conditional on the expected next sequence number being the one the handler computed; if another command modified the same aggregate between the load and the append, the expected sequence will be wrong and the append will fail with a concurrency conflict. The command handler responds to a concurrency conflict by re-loading the aggregate, re-validating, and re-attempting; this is optimistic concurrency control, which is appropriate because contention on a single aggregate is rare in the OpenGate domain (one member's credentials are rarely modified concurrently from multiple sessions). The number of retries is bounded; after five failed retries the command fails with a serialization error and the client retries the entire command.

**Event type versioning and upcasting.** Event types carry an explicit version in the type identifier, allowing the schema of an event payload to evolve over the lifetime of the system. When the implementation deserializes an event of an older version, an upcaster transforms the older payload into the current shape before the fold handler processes it. The upcaster is invisible to the aggregate logic; the aggregate sees only current-version events. The upcaster chain is the only place in the system that knows about old event versions, and the events themselves are never rewritten in the event store. This is the discipline that preserves the audit guarantee: the bytes that were committed to the event store are the bytes that an auditor can read out years later, and any necessary transformation happens at read time.

**No snapshots in this version.** Some event-sourced systems use snapshots, periodic captures of an aggregate's current state, to avoid re-folding the entire event history on every load. OpenGate does not implement snapshots in this version. The reasoning is that the aggregates in the OpenGate domain are small (members, credentials, policies, doors have small fixed sets of events per aggregate) and the load cost is negligible at the projected data volume. If the implementation later reveals a hot aggregate whose event count grows without bound (the most likely candidate is the Tenant aggregate if every configuration change is an event), snapshots can be introduced for that aggregate type without touching any other code, because the EventStore port already abstracts the load operation. The snapshot decision is therefore deferred to operational data rather than made in advance.

**The EventStore port.** The port that the application uses to interact with the event store is small and shaped to make the discipline above natural to express in code. The port has two principal methods: append, which atomically appends a slice of events to a single aggregate stream conditional on the expected sequence number, and load, which returns all events for a given aggregate identifier ordered by sequence. Both methods accept a context for tracing and cancellation. The port also exposes a read-events-by-position method used by projectors to consume events globally rather than per-aggregate, and a read-events-by-tenant-and-time method used by the export capability to range over a tenant's events efficiently.

```go
// EventStore is the outbound port through which the application persists and
// retrieves domain events. The implementation in production is PostgresEventStore;
// the test double is InMemoryEventStore.
type EventStore interface {
    // Append commits the given events as the next contiguous sequence on the
    // given aggregate. If the expected sequence does not match the current
    // sequence in the store, ErrConcurrencyConflict is returned and the caller
    // re-loads and retries.
    Append(
        ctx context.Context,
        aggregateID uuid.UUID,
        expectedSequence int64,
        events []Event,
    ) error

    // Load returns all events for the given aggregate in sequence order.
    Load(
        ctx context.Context,
        aggregateID uuid.UUID,
    ) ([]Event, error)

    // ReadAfterPosition returns events with stream position > position, in
    // global order. Used by projector workers.
    ReadAfterPosition(
        ctx context.Context,
        position int64,
        limit int,
    ) ([]Event, error)

    // ReadByTenantAndTimeRange returns events for a tenant within an inclusive
    // time range. Used by the export capability.
    ReadByTenantAndTimeRange(
        ctx context.Context,
        tenantID uuid.UUID,
        from, to time.Time,
    ) ([]Event, error)
}
```

The implementation of the PostgresEventStore adapter uses a single events table with a composite uniqueness constraint on aggregate_id and sequence, which is what causes the optimistic concurrency conflict to surface as a constraint violation on append. The exact column definitions and indexes are deferred to the Database Schema document and discussed in cross-reference from there.

---

## 3. Read model projection strategy

The read model projection strategy is the question of how the denormalized read models that serve queries are kept in sync with the event store. OpenGate uses a deliberate split between read models that are updated synchronously within the command transaction and read models that are updated asynchronously by River projector jobs. This section specifies which read models fall on which side of the split and why.

**The synchronous side.** A read model is materialized synchronously when one of two conditions holds. The first condition is that the read model is consulted during a subsequent command in a way that must reflect the latest state with no perceptible lag. The second condition is that the read model is consulted on the access decision path, which has a fifty-millisecond p99 budget that cannot tolerate projection lag. Three read models meet at least one of these conditions: the credential read model (consulted on every access decision), the member read model (consulted on every access decision, also consulted when the dashboard immediately re-renders after a member operation), and the policy read model (consulted on every access decision). For these three read models, the command handler writes the read model row in the same Postgres transaction that appends the event to the event store. The implementation uses a SQL upsert pattern within the transaction so that the read model is always consistent with the event log after commit.

**The asynchronous side.** Every other read model is materialized by a River projector job that consumes events from the global stream position. The projector job runs as a singleton across all worker instances, coordinated by the advisory-lock mechanism described in section five. The job reads events in batches of one hundred, identifies the events that affect a read model the job is responsible for, applies the corresponding update to the read model, and advances its consumed-position watermark within the same database transaction. The watermark is itself a row in a projection_progress table, keyed on projector identifier. If the projector crashes mid-batch, the watermark is not advanced and the batch is reprocessed on the next iteration; the read model updates are idempotent (upserts based on aggregate identifier) so reprocessing produces the correct end state.

Five read models are on the asynchronous side. The audit log read model is the largest of them; it consumes access decision events and produces denormalized rows with member name and door name joined into the row to support efficient compliance queries. The door status read model consumes door status change events and door heartbeat events and produces a row per door with current status and last heartbeat. The subscription delivery read model consumes webhook delivery attempt events and produces the dead-letter queue view. The export status read model consumes export job events and produces the export status view. The reader connectivity read model consumes reader heartbeat and disconnection events and produces the connectivity overview.

A summary in prose of the split: synchronous projection for read models that the next command needs immediately or that the decision path reads, asynchronous projection for read models that serve compliance and operational views.

**Projector job structure.** Each asynchronous projector is a single River job kind. The job is enqueued for periodic execution every five hundred milliseconds, but in practice each execution either processes a batch and exits immediately, or finds nothing to process and exits immediately. The lightweight scheduling is acceptable because River's job table is in the same Postgres database that holds the event store, and the cost of checking for events to process is a single indexed query. An alternative implementation using Postgres LISTEN/NOTIFY to wake the projector on each new event was considered and rejected; the polling implementation is simpler, latency is acceptable, and the notification implementation would require careful handling of the case where the listener is not connected at the moment a notification fires.

**Projection lag as a first-class metric.** The asynchronous projector emits a gauge metric named `opengate_projection_lag_seconds`, labeled by projector kind, with a value equal to the difference between the current wall-clock time and the timestamp of the latest event the projector has consumed. A projector falling behind in real time produces a growing gauge value that triggers operator attention through the Grafana dashboard. The metric is sampled once per projector iteration.

**The choice not to use materialized views.** Postgres provides materialized views as a native feature, and a naive interpretation of CQRS might suggest using them for read models. OpenGate does not. The reasons are several: materialized views must be refreshed atomically (REFRESH MATERIALIZED VIEW) and the refresh holds a lock; incremental view maintenance is not available in vanilla Postgres; the view definition couples the read shape to a query that must be rewritten when read shapes evolve; and the per-event update pattern that projector jobs implement maps cleanly to incremental update which materialized views do not. Read models in OpenGate are ordinary tables that the projector code writes to via upserts and deletes.

---

## 4. Idempotency mechanisms

Idempotency in OpenGate appears in three variants, each tailored to a specific class of caller and each implemented with awareness of the cost-benefit trade-off for that class. The three variants are command idempotency for dashboard requests, decision idempotency for reader access requests, and reconciliation idempotency for offline event upload from readers.

**Command idempotency.** The dashboard sends an `Idempotency-Key` HTTP header with every state-changing request. The key is a client-generated UUIDv4 that the dashboard persists in its in-memory request context until the request is acknowledged. On the server side, every mutating handler is wrapped by an idempotency middleware that first checks whether the key has been seen before for the current tenant. The lookup is keyed on the tuple (tenant_id, idempotency_key) and returns either nothing (the key is new), the prior response (the key was used and the operation completed), or a sentinel value indicating that a previous request with this key is currently being processed (in which case the new request waits briefly and then returns 409 Conflict). On a new key, the middleware proceeds to call the handler, captures the response, stores the (key, response, hash of the request body) tuple in the idempotency_keys table, and returns the response to the client. On a subsequent retry with the same key, the middleware returns the stored response without re-executing the handler. The hash of the request body is stored so that the middleware can detect the case where a client uses the same key but a different payload, which is treated as an error rather than as an idempotency match.

The retention window for command idempotency keys is ten minutes. The choice of ten minutes is informed by the fact that the dashboard's longest reasonable retry budget is short, and the storage cost of keys with their cached responses can grow unboundedly if retention is too long. A cleanup job runs every five minutes and deletes idempotency_keys rows older than the retention window.

**Decision idempotency.** Reader-originated access requests also carry an `Idempotency-Key` header, generated by the reader when it initiates the request. The mechanism on the server side is the same as for command idempotency, but the table is separate (decision_idempotency_keys) so that the two classes of caller do not contend on the same hot table, and the retention window is also ten minutes. The fact that the table is separate from the command idempotency table also reflects an architectural reality: the decision path is performance-critical and benefits from a focused table with indexes tuned exactly to its lookup pattern.

A consideration that influenced the decision idempotency design is what the reader does when it cannot reach the backend. The reader does not block on retry indefinitely; after a configurable timeout (default three seconds), the reader assumes the backend is unreachable and falls back to its local cache for the decision. If the backend later receives the request, it processes the request normally and returns the decision; the reader, having already proceeded with its local decision, ignores the response. This is by design: the local decision is the authoritative outcome for the door, and the backend's later record of the same decision serves the audit trail. The system is consistent with itself because the reader logs both the local outcome and the backend's outcome on the next reconciliation cycle.

**Reconciliation idempotency.** When a reader uploads a batch of events accumulated during an offline window, each event in the batch carries a composite identifier composed of the reader identifier and the reader-local sequence number. The composite identifier is durable across reader restarts because the reader stores its sequence number in a local file and persists it across power cycles. The server side, in the Reconciliation use case, looks up the composite identifier in a third idempotency table (reconciliation_idempotency_keys) and appends the corresponding event to the event store only if the composite identifier is not already recorded.

The retention window for reconciliation idempotency is the full event store retention, which in practice means the keys are never purged because the events they protect are themselves never purged. The storage cost is acceptable because the keys are small (sixteen bytes of reader_id plus eight bytes of sequence number per row, plus indexes) and the volume is bounded by the rate at which readers produce events, which is itself bounded by the rate of physical access at the door.

**Implementation of the idempotency lookup.** The idempotency middleware uses a single SQL statement to perform the lookup and the insert in one round-trip. The statement is an INSERT with an ON CONFLICT DO NOTHING clause that returns the inserted row if the insert succeeded and nothing if it did not; the middleware then issues a SELECT for the prior response if the insert did nothing. The pattern keeps the latency overhead of idempotency to one or two database round-trips per request, which is acceptable on the command path and tolerable on the decision path. An alternative implementation using Postgres advisory locks per idempotency key was considered and rejected as more expensive without sufficient benefit.

---

## 5. Distributed coordination via advisory locks

Several use cases in OpenGate require that exactly one process at a time execute a particular critical section, even though the system is designed to support multiple worker instances running concurrently. The coordination is implemented via Postgres advisory locks, which are session- or transaction-scoped mutual exclusion primitives that the database engine provides and that participate in the same transaction infrastructure as the rest of the data.

**Two flavors of advisory lock.** Postgres exposes two relevant flavors. The session-scoped advisory lock (acquired via `pg_advisory_lock` and released via `pg_advisory_unlock`) persists for the lifetime of the database connection and must be released explicitly. The transaction-scoped advisory lock (acquired via `pg_advisory_xact_lock`) is automatically released when the surrounding transaction commits or rolls back. OpenGate uses the transaction-scoped variant exclusively because the automatic release avoids the class of bugs where a lock is acquired but never released due to an early return path. The session-scoped variant has legitimate uses (such as long-running coordination outside a single transaction) but none of the OpenGate use cases require it.

**The naming convention.** Advisory locks are identified by 64-bit integers, and acquiring two locks with the same integer from different sessions is what produces the contention. OpenGate uses a deterministic naming convention that maps a string lock name to a 64-bit integer via a stable hash. The lock name is a structured identifier such as `credential.generate:<tenant_id>` or `projector.audit_log` or `job.cleanup_idempotency_keys`. The hash function is SipHash with a constant key, which provides cryptographic-quality dispersion across the 64-bit space and a negligible collision probability. The implementation is a small utility function in the `internal/coordination/advisory` package that takes a string and returns the int64 to pass to the Postgres function.

```go
// LockID computes a deterministic int64 lock identifier from a structured
// string name. The mapping is collision-resistant for any practical number
// of lock names per deployment.
func LockID(name string) int64 {
    h := siphash.New(advisoryHashKey)
    _, _ = h.Write([]byte(name))
    return int64(h.Sum64())
}

// WithLock acquires the named transaction-scoped advisory lock, runs the
// provided function, and releases the lock when the surrounding transaction
// commits or rolls back. The lock is acquired by Postgres, not by Go, so
// blocking on contention does not block the goroutine other than waiting
// for the SQL call to return.
func WithLock(ctx context.Context, tx pgx.Tx, name string, fn func() error) error {
    _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", LockID(name))
    if err != nil {
        return fmt.Errorf("acquire advisory lock %q: %w", name, err)
    }
    return fn()
}
```

**Use sites in OpenGate.** Three categories of use site exist. The first category is credential identifier generation within a tenant. When the backend generates a credential identifier on behalf of an administrator, the generation is wrapped in an advisory lock keyed on `credential.generate:<tenant_id>`. The lock prevents two concurrent generation requests within the same tenant from colliding on the next available identifier. Different tenants use different lock names and do not contend on each other. The second category is singleton projector jobs. Each projector job acquires an advisory lock keyed on `projector.<projector_name>` at the start of its execution and runs only if the lock is acquired; the lock is held for the duration of the projector's batch processing and released on commit. This ensures that exactly one projector instance per kind is processing events at any moment, even though multiple worker processes may attempt to start the same job. The third category is singleton maintenance jobs such as the idempotency-key cleanup and the dead-letter-queue purge, each of which acquires an advisory lock keyed on `job.<job_name>`.

**Lock contention behavior.** When two transactions request the same advisory lock, the second transaction blocks until the first commits or rolls back. The blocking is implemented inside Postgres, not in Go; the Go code waits for the SQL call to return. The wait is interruptible by context cancellation through the `pgx` driver's support for query cancellation. A query that is awaiting an advisory lock and whose context is canceled propagates the cancellation to Postgres, which terminates the wait and returns an error to the Go code. This is important for graceful shutdown: a worker that is blocked on a lock during shutdown does not block the shutdown indefinitely because the context cancellation cuts the wait short.

**Trade-offs and alternatives considered.** Advisory locks are not the only coordination primitive available. The alternatives considered and rejected for OpenGate are Postgres row locks (heavier-weight, require an actual row, semantically conflate locking with row update), distributed lock services such as etcd or Zookeeper (add an operational dependency that the architecture document deliberately avoids), and library-level locks such as Redis-based redlock (also add a dependency, and have known correctness pitfalls under certain failure modes). Advisory locks are correct, lightweight, and free of additional operational surface; they are the right choice for OpenGate's coordination needs.

---

## 6. Background job processing with River

River is the Postgres-backed job queue library used for all background work in OpenGate. The choice of River over alternatives (Asynq backed by Redis, Machinery backed by AMQP, custom queues backed by Postgres SKIP LOCKED) is justified in the System Architecture document on the grounds of operational simplicity (no second persistence technology) and transactional outbox semantics (jobs enqueue in the same transaction as the events that trigger them). This section specifies how River is configured and used in OpenGate at the implementation level.

**Job kinds.** Job kinds are Go types that implement the `river.JobArgs` interface and have a `Kind` method returning a stable string identifier. The job kinds in OpenGate are enumerated as follows. The `projector.audit_log` kind runs the audit log read model projector. The `projector.door_status` kind runs the door status read model projector. The `projector.subscription_delivery` kind runs the subscription delivery (dead-letter) read model projector. The `projector.export_status` kind runs the export status read model projector. The `projector.reader_connectivity` kind runs the reader connectivity read model projector. The `subscription.deliver` kind delivers a single webhook attempt. The `export.run` kind executes a tenant data export job. The `cleanup.idempotency_keys` kind purges expired idempotency keys. The `cleanup.sessions` kind purges expired sessions. The `cleanup.dead_letter` kind purges dead-letter queue entries past their retention window. The `reader.notify` kind fans out a credential or policy update to active SSE streams; this kind is used in cases where the synchronous LISTEN/NOTIFY path is not appropriate, such as when the notification originates from a projector rather than from a command handler.

**Job arguments.** Each job kind defines its arguments as a Go struct that is JSON-serialized when the job is enqueued. The arguments are kept small and immutable; if a job needs more context than fits comfortably in the arguments, it queries the database during execution using identifiers passed in the arguments. The arguments always include the tenant identifier so that the worker can set the tenant session variable on its database connection before executing the job. The arguments also always include a serialized OpenTelemetry trace context so that the worker can continue the trace of the originating command. The trace context propagation is the property that makes a single user-initiated action visible end-to-end in Tempo, including any background work that the action triggered.

```go
// SubscriptionDeliverArgs defines the arguments for a single webhook delivery
// attempt. The arguments are kept small; the deliverer loads the subscription
// and event details from the database using these identifiers.
type SubscriptionDeliverArgs struct {
    TenantID       uuid.UUID         `json:"tenant_id"`
    SubscriptionID uuid.UUID         `json:"subscription_id"`
    EventID        uuid.UUID         `json:"event_id"`
    AttemptNumber  int               `json:"attempt_number"`
    TraceCarrier   propagation.MapCarrier `json:"trace_carrier"`
}

func (SubscriptionDeliverArgs) Kind() string { return "subscription.deliver" }
```

**Retry configuration per job kind.** River allows retry behavior to be configured per job kind. The configuration in OpenGate is as follows. Projector jobs retry indefinitely with exponential backoff capped at thirty seconds; persistent failure of a projector job is an operator-level concern that surfaces through the projection lag metric and is investigated manually. Subscription delivery jobs retry on the schedule documented in the PFD section ten, with a twenty-four-hour total budget and exponential backoff with twenty-percent jitter. Export jobs retry up to three times then fail permanently with the failure surfaced to the requesting user via the export status read model. Cleanup jobs retry up to three times then fail; persistent cleanup failures grow the storage of expired data but do not affect functional correctness.

**Periodic jobs.** River supports periodic jobs via its `PeriodicJob` configuration, which schedules a job kind to be enqueued at a fixed interval. The cleanup jobs are periodic with a five-minute interval. The projector jobs are also periodic with a five-hundred-millisecond interval; the short interval is acceptable because most invocations find no work and return immediately, and because the polling cost in Postgres is bounded by a small set of indexed queries.

**Concurrency configuration.** Each job kind has a concurrency limit specifying the maximum number of workers that may execute the kind simultaneously. The projector kinds are configured with a concurrency limit of one (enforced by the advisory lock described in section five; River's concurrency limit is a safety net rather than the primary mechanism). The subscription delivery kind is configured with a concurrency limit of ten, allowing parallel delivery to many subscribers while bounding the load on the network. The export kind is configured with a concurrency limit of two; exports are expensive and rare and parallelism beyond two would not improve user experience. The cleanup kinds are configured with a concurrency limit of one each, enforced by their advisory locks.

**Trace context restoration.** When a worker picks up a job, the first step of the worker function is to restore the OpenTelemetry trace context from the serialized carrier in the arguments and start a span that is a child of the originating span. The implementation uses the standard `otel.GetTextMapPropagator().Extract(ctx, args.TraceCarrier)` pattern. The resulting context is passed through the worker's execution and propagates further into any database calls, HTTP calls, or sub-jobs that the worker enqueues. The end-to-end trace property described in cross-reference from section eight depends on this restoration.

---

## 7. Hexagonal port contracts at the code level

This section makes concrete the abstract port-and-adapter discipline that the System Architecture document established in its section six. The form that the discipline takes in OpenGate is a set of Go interfaces declared in `internal/ports/` that the application layer uses and that the adapters in `internal/adapters/` implement. The section specifies the conventions that all port interfaces in the system follow, with code examples illustrating the conventions on a representative port.

**Context as the first argument.** Every port method takes a `context.Context` as its first argument, without exception. The context carries the deadline for the operation, the cancellation signal, the OpenTelemetry trace context, and the tenant identifier under which the operation is being performed. The tenant identifier is carried in the context rather than as a separate argument because every port method is tenant-scoped and a positional argument would invite forgetfulness. The implementation extracts the tenant identifier with a helper function `tenant.FromContext(ctx)` that panics if no tenant is set; this is intentional because a missing tenant is a programming error and not a runtime failure mode.

**Errors are typed and matched via errors.Is.** Each port defines a small set of sentinel errors that represent the domain-meaningful failure modes of its operations, and the adapter implementations return these errors wrapped in additional context. Callers identify failures using `errors.Is` rather than by string comparison or type assertion. The pattern is the standard Go convention for error handling since Go 1.13.

```go
// ErrCredentialNotFound is returned by CredentialReadModel.LoadByID and
// related methods when the requested credential does not exist in the
// caller's tenant. Callers identify this case via errors.Is.
var ErrCredentialNotFound = errors.New("credential not found")

// ErrConcurrencyConflict is returned by EventStore.Append when the expected
// sequence does not match the current sequence in the store.
var ErrConcurrencyConflict = errors.New("concurrency conflict on event store append")
```

**Adapters do not return database-driver errors.** A common failure mode in code that uses the standard `database/sql` package or its replacements is leaking driver-specific errors through interfaces that should be agnostic. OpenGate's adapters wrap every error from `pgx` into a port-defined error before returning. A `pgx.ErrNoRows` becomes the appropriate sentinel such as `ErrCredentialNotFound`; a unique constraint violation on an event store append becomes `ErrConcurrencyConflict`; an unexpected error is wrapped in a generic `ErrInternal` that carries the original error in its `Unwrap`. The caller's view of error types is therefore stable regardless of which adapter implementation is in use.

**Adapter packages are flat.** Each driven adapter lives in a flat package under `internal/adapters/outbound/`. The package contains the adapter struct, its constructor, the methods that satisfy the port interface, and any helper functions specific to the adapter. The package does not export anything beyond the constructor and the adapter struct; the methods are accessible only through the port interface, ensuring that no caller depends on adapter-specific details.

```go
// internal/adapters/outbound/postgres/event_store.go

// EventStore is the Postgres adapter implementation of the ports.EventStore
// interface. It is constructed once at application startup and shared across
// all goroutines.
type EventStore struct {
    pool *pgxpool.Pool
}

// NewEventStore returns an EventStore that uses the given Postgres connection
// pool. The pool is expected to have the connection-level configuration that
// sets the tenant session variable on checkout; this adapter does not set it
// itself because doing so would require knowledge of the connection layer.
func NewEventStore(pool *pgxpool.Pool) *EventStore {
    return &EventStore{pool: pool}
}

// Append implements ports.EventStore.Append.
func (s *EventStore) Append(
    ctx context.Context,
    aggregateID uuid.UUID,
    expectedSequence int64,
    events []domain.Event,
) error {
    // implementation omitted; see implementation plan for details
    return nil
}
```

**Use cases depend on port interfaces, not on adapter structs.** A use case in the application layer is a struct whose fields are port interfaces (declared as `ports.EventStore`, `ports.MemberReadModel`, and so on) rather than concrete adapter types. The constructor of the use case takes the ports as parameters; the test suite for the use case passes in-memory implementations of the ports; the production wiring at application startup passes the Postgres implementations. This is the constructor-injection style of dependency injection, which is the idiomatic Go approach and does not require any framework.

```go
// internal/application/credential/issue_command.go

// IssueCommandHandler handles the IssueCredentialCommand. It depends only
// on ports, never on adapter implementations.
type IssueCommandHandler struct {
    store       ports.EventStore
    readModel   ports.CredentialReadModel
    members     ports.MemberReadModel
    enqueuer    ports.JobEnqueuer
    coordinator ports.Coordinator
}

// NewIssueCommandHandler returns an IssueCommandHandler with the given ports.
func NewIssueCommandHandler(
    store ports.EventStore,
    readModel ports.CredentialReadModel,
    members ports.MemberReadModel,
    enqueuer ports.JobEnqueuer,
    coordinator ports.Coordinator,
) *IssueCommandHandler {
    return &IssueCommandHandler{
        store:       store,
        readModel:   readModel,
        members:     members,
        enqueuer:    enqueuer,
        coordinator: coordinator,
    }
}
```

**The composition root.** The wiring of adapters to use cases happens in exactly one place: the `cmd/opengate/main.go` file (and any subcommand-specific main files). This is the composition root, a pattern from the dependency-injection literature that says the entire object graph of the application is constructed in one place at startup, and from that point on no further construction occurs at runtime. The composition root in OpenGate is the only place that imports both `internal/adapters/...` and `internal/application/...` packages; everywhere else, the application code imports only ports, and the adapter code imports only ports plus its own infrastructure dependencies. This separation is what makes the hexagonal discipline verifiable through `go vet`-style static analysis or through explicit architecture tests.

---

## 8. Observability instrumentation patterns

Observability is built into the codebase rather than added as a layer. This section specifies the conventions that the instrumentation follows, the OpenTelemetry semantic conventions that OpenGate emits, and the custom attributes that the OpenGate-specific spans and metrics carry.

**Span naming.** Span names follow the convention `<operation_type>.<operation_name>`, with the operation type drawn from a small enumeration and the operation name specific to the operation. The operation types are `http` for HTTP server spans, `db` for database operation spans, `command` for command handler spans, `query` for query handler spans, `job` for River worker spans, `projector` for read model projector spans, and `webhook` for webhook delivery spans. Examples of span names include `http.POST /api/v1/tenants/{t}/members`, `command.create_member`, `db.event_store.append`, `job.subscription.deliver`, `projector.audit_log.batch`, and `webhook.deliver`. The convention makes spans groupable in Tempo and Grafana by operation type, which is the most common axis of investigation.

**Standard OTel semantic conventions.** HTTP server spans carry the standard `http.method`, `http.route`, `http.status_code`, and `http.user_agent` attributes. Database spans carry `db.system` (always "postgresql" in OpenGate), `db.statement` (the SQL query with parameter placeholders, never with parameter values to avoid leaking sensitive data), and `db.operation` (the verb such as "INSERT", "SELECT"). Outbound HTTP spans (for webhook delivery) carry the analogous `http.method`, `http.url`, and `http.status_code` attributes. Errors are recorded on spans via the `span.RecordError(err)` method and the span status is set to `codes.Error` for any operation that failed.

**OpenGate-specific attributes.** Every span emitted by OpenGate code carries the `opengate.tenant_id` attribute when a tenant context is active. Command spans carry `opengate.command_kind`. Query spans carry `opengate.query_kind`. Job spans carry `opengate.job_kind` and `opengate.job_attempt`. Projector spans carry `opengate.projector_kind` and `opengate.batch_size`. Access decision spans carry the additional attributes `opengate.decision`, `opengate.decision_reason`, `opengate.credential_id`, `opengate.door_id`, and `opengate.member_id` (with the member identifier omitted when the credential resolves to no member). Webhook delivery spans carry `opengate.subscription_id`, `opengate.event_kind`, and `opengate.delivery_attempt`. The set of attributes is finite, documented, and stable; changes to the attribute set are versioned alongside changes to the application.

**Metric naming.** Metrics follow the Prometheus naming convention with the `opengate_` prefix. Histograms have the `_seconds` suffix when they measure time and `_bytes` suffix when they measure size. Counters have the `_total` suffix. Gauges have a descriptive suffix indicating their unit such as `_count` or `_lag_seconds`. Examples include `opengate_http_request_duration_seconds` (histogram, labeled by route and status), `opengate_command_duration_seconds` (histogram, labeled by command kind), `opengate_decision_duration_seconds` (histogram, labeled by outcome), `opengate_projection_lag_seconds` (gauge, labeled by projector kind), `opengate_job_duration_seconds` (histogram, labeled by job kind), `opengate_webhook_delivery_attempts_total` (counter, labeled by subscription and outcome), and `opengate_active_sse_connections` (gauge, labeled by tenant).

**Logging via slog.** Structured logging uses the standard library `log/slog` package introduced in Go 1.21. The logger is configured at process startup to emit JSON-formatted log records to stdout, which the container runtime collects and forwards to the operator's log aggregation. Each log record carries a small set of standard fields including the timestamp, the level, the message, and the source location, plus contextual fields propagated from the request context. The contextual fields include the tenant identifier when available, the trace identifier and span identifier from the active span, and the user identifier for administrative requests. Log levels follow the conventional severity scale: debug for verbose development logs (disabled in production), info for routine operational events, warn for unexpected but recoverable conditions, and error for failures that affect functionality. The convention that no successful operation logs at error level is enforced by code review.

**Sensitive data in logs and traces.** A short list of fields must never appear in logs or traces: passwords (whether plaintext or hashed), session tokens, API keys, webhook shared secrets, and the signing key. The exclusion is enforced by code review and by an explicit deny-list of sensitive field keys that the logger formatter redacts — `password`, `token`, `api_key`, `secret`, and `signing_key` — with all other contextual fields passing through. A deny-list (default-allow with named redactions) was chosen over an allow-list (default-deny, only pre-registered fields admitted) for practicality: structured logs carry many benign contextual fields that would be brittle to enumerate exhaustively in advance. The residual risk — a newly introduced sensitive field not yet added to the deny-list — is mitigated by the redaction test and by code review. A test in the test suite asserts that a known sensitive field, when passed to any logger context, is redacted in the output. The same deny-list applies to span attributes; a test asserts the same for traces.

---

## 9. Authentication and session management

Authentication in OpenGate covers two classes of credential: administrative user passwords for dashboard sessions, and reader API keys for the reader protocol. Session management covers the lifecycle of authenticated dashboard sessions. This section specifies the cryptographic parameters and the implementation details for both.

**Password hashing with Argon2id.** Administrative user passwords are stored as Argon2id hashes with parameters tuned for the deployment environment. The parameters used in OpenGate are sixty-four megabytes of memory, three iterations, and four-way parallelism, with a sixteen-byte random salt per password and a thirty-two-byte output. These parameters follow the OWASP password storage cheat sheet recommendation for Argon2id as of 2024 and produce a hash computation time of approximately one hundred milliseconds on the target deployment hardware. The implementation uses the `golang.org/x/crypto/argon2` package directly; the `IDKey` function is the appropriate Argon2id entry point. The hashed password is stored alongside the parameters in a single string using the standard PHC-formatted encoding so that future parameter changes can coexist with existing hashes; a verification routine reads the parameters from the stored string and computes the candidate hash with those parameters rather than with the current defaults.

```go
// HashPassword hashes the given plaintext password using Argon2id with the
// current parameters and returns a PHC-formatted string containing both the
// hash and the parameters used to produce it. The parameters are stored with
// the hash so that future parameter changes can coexist with existing hashes.
func HashPassword(plaintext string) (string, error) {
    salt := make([]byte, 16)
    if _, err := rand.Read(salt); err != nil {
        return "", fmt.Errorf("generate salt: %w", err)
    }
    hash := argon2.IDKey(
        []byte(plaintext),
        salt,
        argon2TimeCost,      // 3
        argon2MemoryCost,    // 64 * 1024 KB
        argon2Parallelism,   // 4
        argon2OutputLength,  // 32
    )
    return formatPHC(salt, hash, argon2TimeCost, argon2MemoryCost, argon2Parallelism), nil
}
```

**Session tokens.** A successful login produces a session token that the dashboard stores in an HttpOnly, Secure, SameSite=Lax cookie. The token itself is thirty-two bytes of cryptographically random data, base64-encoded for transport. On the server side, the token is stored as a SHA-256 hash in the sessions table along with the user identifier, tenant identifier, role, expiry timestamp, and the IP address and user agent of the issuing request (for forensic purposes). The SHA-256 hash is used rather than Argon2id because session tokens are high-entropy and do not benefit from the slow-hash properties; SHA-256 is fast enough to perform per-request without introducing measurable latency. The session lookup on each request hashes the incoming cookie value and queries the sessions table by hash, returning the session record if it exists and has not expired.

The session expiry is configurable per tenant with a default of sixty minutes of inactivity. The expiry is implemented as a sliding window: every authenticated request updates the session's last_seen_at column to the current time, and the session is considered expired when last_seen_at is older than the inactivity window. A background cleanup job (the `cleanup.sessions` River job from section six) periodically deletes expired sessions. Logout deletes the session record explicitly.

Note (v1.1): the session lookup, `SELECT … FROM sessions WHERE token_hash = $1`, carries no tenant predicate — the cookie holds only the opaque token, and `tenant_id` is read from the matched row. Because section ten forces RLS on `sessions`, this lookup, together with the user-by-email lookup at login, executes on the BYPASSRLS pool (see the pre-authentication carve-out in section ten). Login's tenant resolution is an open decision: `users.email` is unique per tenant (`UNIQUE (tenant_id, email)`), so the same email may exist in several tenants of one deployment, and the flat `/api/v1/auth/login` endpoint carries no tenant. The mechanism — tenant identifier in the request body, host or subdomain resolution, single-tenant-per-deployment, or globally-unique email — is decided in US-02.03's articulation and recorded here once chosen.

**API key hashing.** Reader API keys, issued during reader provisioning, are also stored as Argon2id hashes. The parameters are the same as for passwords because the threat model is the same (an attacker with database read access should not be able to derive the original key). The keys themselves are thirty-two bytes of cryptographically random data, base64-encoded. The reader stores its key in a file in its container's writable volume; on startup, the reader reads the key from the file and uses it in the Authorization header of every request to the API server.

API key verification on the server side is more expensive than session lookup because Argon2id verification is intentionally slow. To mitigate the per-request cost, the server caches the result of a successful API key verification in a short-lived in-memory cache keyed on the SHA-256 of the presented key. The cache entry expires after five minutes; on cache hit, the server skips the Argon2id verification and uses the cached reader identifier. The cache is in-memory only and is not shared across application instances; a reader whose key was just rotated may briefly authenticate against the old key on instances that have not yet seen the rotation, but the rotation overlap window described in section eight of the System Architecture document accommodates this.

**Bootstrap of the first administrative user.** A fresh OpenGate deployment has no users yet, and the dashboard cannot log in until the first user is created. The CLI bootstrap subcommand creates the first tenant and the first owner-role user in one atomic operation; the operator runs the subcommand with environment variables specifying the tenant name and the owner's email and password, and the subcommand writes both records to the database in a single transaction. Subsequent administrative users are created by the existing owner through the dashboard.

**No password reset by email in this version.** The reason was stated in the PFD section two: building a reliable email pipeline is itself a non-trivial engineering exercise that does not contribute to the demonstrated patterns. The user-management workflow that an owner uses to reset another user's password is the substitute: the owner triggers a password reset, the system generates a new random temporary password, and the owner communicates the temporary password to the affected user through whatever channel the gym uses (in person, telephone, or external email). The temporary password forces a change at next login.

---

## 10. Multi-tenant isolation: dual-layer implementation

Multi-tenant isolation in OpenGate is enforced by two independent layers, each of which would on its own be sufficient to prevent cross-tenant data exposure under normal conditions, and which together provide defense in depth against bugs in either layer. The two layers are the application layer (which includes the tenant identifier in every query) and the database layer (which enforces Row-Level Security policies). This section specifies how each layer is implemented and how they interact.

**The connection-level tenant binding.** The first piece of the puzzle is that every database operation occurs in the context of a known tenant. The mechanism is a Postgres session variable named `app.current_tenant_id` that is set on every connection checked out of the pool. The pgx connection pool supports an `AfterAcquire` hook that runs before the connection is returned to the caller; OpenGate registers a hook that extracts the tenant identifier from the request context and issues a `SELECT set_config('app.current_tenant_id', $1, false)` statement to set the variable for the duration of the connection's checkout. When the connection is returned to the pool, a `BeforeRelease` hook resets the variable to empty so that the next checkout starts from a clean state.

```go
// configurePool registers AfterAcquire and BeforeRelease hooks that bind
// the tenant identifier from the request context to the database session
// variable, and reset it on release.
func configurePool(config *pgxpool.Config) {
    config.AfterAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
        tenantID, ok := tenant.FromContext(ctx)
        if !ok {
            // No tenant in context: leave the variable empty, RLS will reject.
            // This is the safe default; missing tenant context is a bug.
            return true
        }
        _, err := conn.Exec(ctx,
            "SELECT set_config('app.current_tenant_id', $1, false)",
            tenantID.String(),
        )
        return err == nil
    }
    config.BeforeRelease = func(ctx context.Context, conn *pgx.Conn) bool {
        _, _ = conn.Exec(ctx, "SELECT set_config('app.current_tenant_id', '', false)")
        return true
    }
}
```

**The application-layer filter.** Every SQL query that targets a tenant-scoped table includes the tenant identifier as a `WHERE tenant_id = $1` predicate. The query is written that way in the SQL source file from which `sqlc` generates the Go function, which means the tenant filter is part of the generated function's signature and cannot be forgotten by the caller. The pattern produces compile-time enforcement of the tenant filter at every call site: a caller who forgets to pass the tenant identifier gets a compile error rather than a runtime data leak.

```sql
-- internal/adapters/outbound/postgres/queries/credential.sql

-- name: LoadCredentialByID :one
-- Loads a credential by its identifier within the given tenant. The tenant
-- filter is mandatory and the query is generated with the tenant parameter
-- as part of its Go function signature.
SELECT id, member_id, status, issued_at, revoked_at, expires_at
FROM credentials
WHERE tenant_id = $1 AND id = $2;
```

**The database-layer policy.** The second layer is Postgres Row-Level Security. Every tenant-scoped table has an RLS policy that constrains the rows visible to a connection based on the session variable. The policy is enabled on the table with `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` and `FORCE ROW LEVEL SECURITY`, and the policy itself is `CREATE POLICY tenant_isolation ON <table> USING (tenant_id = current_setting('app.current_tenant_id')::uuid)`. The FORCE clause is important: by default, RLS does not apply to the table owner, and the application connects to Postgres as the owner of the schema. The FORCE clause makes the policy apply to all roles including the owner, which is what makes the layer effective.

If the session variable is empty (no tenant set), the cast to uuid in the policy fails and the policy returns no rows. This is the safe default: a query that runs without a tenant context returns no data rather than returning all data.

**Bypass for operator-level access.** The bootstrap CLI subcommand and the data export job need to operate across tenant boundaries or to set up the initial state of a tenant before any user has been created. These code paths run as a different database role that is exempted from RLS policies by virtue of being a Postgres BYPASSRLS role. The bypass role is used only by the bootstrap CLI and by the export job, both of which are operator-level rather than user-level execution paths, and the bypass is granted only to those specific code paths via a different connection pool configured at startup. Application code that runs on behalf of an already-authenticated user never reaches the bypass pool, with one carve-out: the pre-authentication identity lookups that _perform_ authentication — the user-by-email lookup at login and the session-by-token lookup on every request — run on the BYPASSRLS pool. They must resolve identity before any tenant context exists, because neither the cookie nor the login body establishes a tenant, and RLS is forced on `users` and `sessions`. These lookups are pre-authentication, not actions taken on behalf of an authenticated user, so the operator-versus-user distinction is preserved.

**A verification test.** The test suite contains an explicit test that asserts the two layers work together. The test creates two tenants and two users, one in each tenant, with the rows seeded via the BYPASSRLS pool because RLS is forced. It then opens a connection bound to tenant A and attempts to query the user belonging to tenant B by its identifier. The test asserts that the query returns no rows. The test then deliberately bypasses the application-layer filter by issuing a raw SQL query without the tenant predicate, and asserts that the RLS layer still filters the rows. Finally the test bypasses the RLS layer by using the BYPASSRLS role and asserts that both users are visible, confirming that the protection comes from RLS specifically rather than from some other accident. The test is part of the contract test suite and runs against the Postgres adapter implementation.

---

# Part II — Component design

## 11. Tenant provisioning and identity components

The tenant provisioning and identity components are realized as the `TenantManagement`, `UserManagement`, and `SessionManagement` use cases in the application layer, together with the `casbin` driven adapter and the bootstrap CLI subcommand. The use cases follow the patterns from sections three (read model projection, where applicable), seven (port contracts), and nine (authentication). They do not follow section two: identity is state-stored, not event-sourced, as the correction below sets out.

Correction (v1.1): the identity aggregates — tenant, user, and session — are state-stored, not event-sourced. Their tables (`tenants`, `users`, `sessions`, and likewise `readers`, `subscriptions`, `casbin_rules`) are authoritative state mutated by ordinary `INSERT` and `UPDATE`, per Database Schema section five, which is authoritative over the event-sourced framing previously written here. Event sourcing applies only to the domain aggregates — members, credentials, policies, doors, and access — and arrives with the event store in Epic E3. Concretely: the tenant management use case exposes creation, configuration-change, and suspension or reactivation operations; the user management use case exposes creation, role-change, password-change, and deactivation; a session row is created at login and thereafter mutated by direct updates of `last_seen_at` and `expires_at`. None of these emit domain events or rebuild from an event stream. Do not build event-sourced tenant or user aggregates.

The Casbin authorizer adapter is initialized at process startup with a model file (`config/rbac_model.conf`) defining the request and policy schemas and a policy adapter that loads the role-permission rules from the `casbin_rules` table in Postgres. The adapter caches the loaded policy in process memory and refreshes it on a thirty-second interval. Policy updates triggered through the user management use case write to the table and also send a refresh signal via Postgres LISTEN/NOTIFY so that other application instances refresh sooner than the polling interval.

The bootstrap CLI subcommand uses the BYPASSRLS connection pool described in section ten to create the first tenant and the first owner user in a single transaction. The subcommand reads its inputs from environment variables (`OPENGATE_BOOTSTRAP_TENANT_NAME`, `OPENGATE_BOOTSTRAP_OWNER_EMAIL`, `OPENGATE_BOOTSTRAP_OWNER_PASSWORD`); the operator sets these before invoking the subcommand and unsets them immediately after.

---

## 12. Member management components

The member management components are realized as the `MemberManagement` use case in the application layer plus the dashboard's member-related pages. The use case follows the patterns from section two for the member aggregate, section three for the asynchronous projection of the member list and detail read models, section four for command idempotency, and section eight for command spans named `command.create_member`, `command.update_member`, and `command.transition_member_status`. The member aggregate has events for creation, identity field updates, and status transitions, with the status transitions explicitly validated by the aggregate (an expired member cannot transition to suspended, for example, as described in PFD section three).

The member list read model is projected asynchronously by the audit log projector since the audit log read model already needs to denormalize member names into its rows; sharing the projector reduces operational complexity and ensures the two read models stay consistent with each other. The member detail read model is also projected asynchronously by the same projector.

The member search index, when the user searches by name or email, is implemented as a trigram index (`pg_trgm`) on the member name and email columns of the member detail read model. The trigram index supports substring matching with reasonable performance up to the data volume expected for the project. An alternative implementation using a dedicated full-text search engine such as OpenSearch was considered and rejected on the operational-simplicity grounds repeated throughout the architecture; trigram search is acceptable for the data scale.

---

## 13. Credential lifecycle components

The credential lifecycle components are realized as the `CredentialManagement` use case in the application layer. The use case is the most pattern-rich of any component in OpenGate: it follows the patterns from section two for the credential aggregate, section three for the synchronous projection of the credential read model (the read model must be fresh for the access decision path, per section sixteen below), section four for command idempotency with the additional case of backend-generated credential identifiers requiring a stable idempotency key for retry-safe generation, section five for advisory-lock coordination on the credential identifier generation path, and section eight for spans named `command.issue_credential` and `command.revoke_credential`.

The credential aggregate has events for issuance, activation (if delayed), revocation, expiration, and modification of expiry date. The aggregate enforces invariants such as the rule that a revoked credential cannot be reactivated and that an expired credential cannot have its expiry pushed forward.

Credential identifier generation in backend-generated mode is wrapped in an advisory-lock-protected critical section keyed on `credential.generate:<tenant_id>`. Within the critical section, the generator queries the highest existing credential identifier suffix in the tenant, increments it, and returns the new identifier. The query and the subsequent credential creation are in the same transaction, which means the advisory lock is held until the transaction commits and is released automatically by Postgres on commit. The generator is therefore safe under concurrent invocation across instances.

A consideration that the implementation must handle is the case where the administrator scans a known card identifier into the issuance form rather than relying on backend generation. In that case, no advisory lock is needed because the uniqueness check is a plain unique constraint on the credentials table; a conflict on the constraint produces an `ErrCredentialIdentifierTaken` sentinel that the use case translates into a user-friendly error response.

---

## 14. Access policy components

The access policy components are realized as the `PolicyManagement` use case for write-side operations (creating, modifying, assigning policies) and as part of the `AccessDecision` use case for read-side evaluation. The policies follow the patterns from section two for the policy aggregate and section three for the synchronous projection of the policy read model (also fresh for the access decision path). The policy aggregate has events for creation, door coverage modification, time-window modification, assignment to a member, and unassignment from a member.

The time-window evaluation algorithm is straightforward but deserves explicit specification because it is the most consequential domain logic in the system. A time window has weekday flags (Monday through Sunday) and a start time and end time in the tenant's local time zone. A given moment is considered within the window if and only if the moment's weekday flag is set on the window and the moment's time-of-day falls within the start-to-end interval. The interval is inclusive of the start and exclusive of the end, following the convention used by most calendaring software. Time windows that cross midnight (start time greater than end time) are not supported in this version; if a tenant needs to allow access from 22:00 to 06:00, the configuration requires two separate windows (22:00 to 24:00 on the relevant weekday, 00:00 to 06:00 on the next weekday). This restriction is documented in the dashboard's policy editor and reduces edge-case complexity in the evaluator.

The DecisionEvaluator domain service is the pure function that takes a member, their assigned policies expanded with door coverage and time windows, the door identifier, and the current moment, and returns a decision with reason code. The function is pure (no I/O) and is therefore trivially testable. The AccessDecision use case is responsible for loading the inputs from the read models; the DecisionEvaluator is responsible for the computation alone.

---

## 15. Reader and door operations components

The reader and door operations components are realized as the `DoorManagement` use case (administrative operations on doors), the SSE Push Server driving adapter (outbound push to readers), and the reader-facing endpoints of the `AccessRequestHandler` use case (inbound from readers). The components follow patterns from sections two for the door aggregate (events for door creation, configuration update, status change), section three for the synchronous projection of the door status read model, and section seven for the dual-direction reader port discipline.

The SSE Push Server is implemented as a chi route handler that, on a GET to the stream endpoint, sets the appropriate SSE headers (`Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no` to disable nginx buffering if any is present), opens a Postgres LISTEN on the `opengate_reader_events` channel, and enters a loop that reads notifications from the channel, filters them by reader identifier, and writes SSE events to the response writer. The handler exits when the client disconnects (detected by the request context being canceled) or when the application is shutting down (detected by a shutdown signal channel).

The push side of the reader port is mediated by the `ReaderNotifier` adapter, which writes notifications to the `opengate_reader_events` channel via `NOTIFY` statements within the originating transaction. The transaction commit makes the notification visible to LISTEN-ing connections; uncommitted notifications are not delivered. The channel payload is a JSON object identifying the tenant, the reader (or wildcard for all readers in the tenant), the event kind, and the event payload. The SSE handler filters by reader identifier (or accepts wildcards) before forwarding to the connected client.

The door heartbeat freshness is maintained by the heartbeat endpoint: each heartbeat updates the `last_heartbeat_at` column of the door status read model and emits a door-status-change event if the door transitions between online, degraded, and offline states. The state transition logic is encapsulated in the door aggregate which compares the new heartbeat timestamp to the previous one and emits an event only on transition.

---

## 16. Access authorization decision component

The access authorization decision component is the runtime-critical component of the system and deserves more design attention than any other. The component is realized as the `AccessDecision` use case in the application layer, called from the chi handler for the reader authentication endpoint. The use case composes patterns from section two (event sourcing for the access decision event itself), section three (synchronous projection of the credential, member, and policy read models that the use case reads), section four (decision-level idempotency), and section eight (the decision span with attributes `opengate.decision`, `opengate.decision_reason`, and so on).

The decision sequence in the use case is as follows. The handler enters the use case with an authenticated reader context and an inbound request payload containing the credential identifier, the door identifier, and the idempotency key. The use case first checks the idempotency cache; on hit, it returns the cached response immediately. On miss, the use case loads the credential from the credential read model, loads the member referenced by the credential from the member read model (or notes that the credential references no member), loads the policies assigned to the member from the policy read model, and constructs the inputs for the DecisionEvaluator domain service. The evaluator returns the decision and reason code; the use case constructs the access-attempted event with the full reasoning context (which policies were evaluated, which time-windows matched or failed to match, which the decision is); the use case opens a transaction, appends the event to the event store, writes the idempotency cache entry, optionally enqueues subscription delivery jobs if any subscriptions filter for access events, and commits. The transaction commit is the moment after which the response can be returned safely.

The total budget for this sequence is fifty milliseconds at p99. The dominant time costs in the sequence are the three read model lookups (each a single indexed query, expected to complete in a few milliseconds), the event store append (a single insert, expected to complete in a few milliseconds), and the idempotency cache insert (also a single insert, expected to complete in a few milliseconds). The DecisionEvaluator computation is sub-millisecond. The remaining margin in the budget accommodates jitter and the small overheads of context propagation, OpenTelemetry instrumentation, and the HTTP middleware stack.

The read-model lookup path uses a single SQL query per read model rather than joining the three tables in a single query. The choice is motivated by clarity and by the fact that the credential read model lookup may short-circuit the rest of the path if the credential is not found or is revoked, in which case no further lookup is needed. A combined query would do more work in the common deny case than the separate-query path.

The synchronous projection of the credential, member, and policy read models is essential to the correctness of the decision path. If the projection were asynchronous, the brief window during which a recent change had not yet propagated to the read model would produce decisions inconsistent with the current state. The choice of synchronous projection for these three read models is therefore mandatory and was articulated in section three.

A subtle point that the implementation must respect is the handling of the case where the credential references a member who has been deleted. The system does not actually delete members; the member aggregate supports a deactivation event that transitions the member to the expired state but does not remove the row. The credential read model retains the member identifier on the credential. The decision use case loads the member; if the member is in the expired state, the decision is deny with reason `deny_member_expired`. If the member is in the suspended state, the decision is deny with reason `deny_member_suspended`. The access record retains the member identifier regardless, allowing the audit log to attribute the attempt even though access was denied.

---

## 17. Offline reconciliation components

The offline reconciliation components are realized as the `Reconciliation` use case in the application layer plus the SSE Push Server's stream-resumption logic. The use case follows patterns from section two for the access-attempted-offline event type (a separate event type from the online access-attempted event, distinguished for audit-trail clarity), section four with the reconciliation composite-key idempotency variant, and section eight with the span `command.reconcile_events`.

The reconciliation endpoint accepts a batch of events from a reader. Each event in the batch contains the composite identifier (reader_id, seq_no), the timestamp at which the reader made the local decision, the credential identifier presented, the door identifier targeted, the local decision (grant or deny), the local reason code, and a flag indicating whether the local cache state at the moment of decision differed from any state that the backend now knows to have applied during the offline window (the reader cannot know this directly; the flag is conservative and may be false even if a divergence existed). The use case iterates the batch, for each event checking the composite identifier against the reconciliation idempotency table, and for each new event appending an access-attempted-offline event to the event store within a single transaction per event. A single transaction per event is the implementation choice because batching multiple events into one transaction would create the risk that a single bad event aborts a large batch; the single-transaction approach handles each event independently.

The push side of reconciliation, where the reader requests events it missed during the offline window, is implemented as the same SSE Push Server endpoint described in section fifteen. The reader includes its watermark (the global stream position of the last event it has applied to its local cache) in the `Last-Event-Id` header on the GET to the stream endpoint. The handler resolves the watermark against the event store and pushes every event since the watermark that is relevant to the reader, in stream order. The reader advances its watermark on the client side for each event it has fully applied; a mid-stream disconnection resumes from the last fully-applied event.

A subtle correctness property of the push side is that the events pushed must be filtered to the reader's tenant. The handler extracts the tenant identifier from the authenticated reader's API key context and includes a tenant filter in the event store query that produces the stream. The filter is in addition to the RLS layer (which would also filter), providing the same defense-in-depth as elsewhere.

---

## 18. Audit log and compliance query components

The audit log and compliance query components are realized as the `AuditQuery` use case for read-side query handling and the `projector.audit_log` River job for read-model maintenance. The components follow patterns from section three for the asynchronous projection of the audit log read model, section six for the projector job structure, and section eight for spans named `query.audit_search` and `projector.audit_log.batch`.

The projector consumes access-attempted and access-attempted-offline events from the global event stream, denormalizes them with the member name and door name (resolved from the member and door read models, which the projector also keeps in sync), and writes rows to the audit_log_view table. The denormalization happens at write time so that read-time queries do not need to join across tables; the cost is that name changes (a member is renamed) require the projector to update historical rows in the audit_log_view table, and the projector handles this case by treating member-renamed events as triggers to update the corresponding audit_log_view rows. The pattern is the standard CQRS approach where read models trade write-time work for read-time simplicity.

The audit search query uses keyset-based pagination rather than offset-based pagination. The cursor is a composite of the timestamp of the last row on the current page and the identifier of that row, used as a tiebreaker for rows with identical timestamps. The next page is requested by passing the cursor as a query parameter; the SQL query is `WHERE (occurred_at, id) < ($cursor_timestamp, $cursor_id) ORDER BY occurred_at DESC, id DESC LIMIT $page_size`. The compound index on (tenant_id, occurred_at DESC, id DESC) supports this query efficiently regardless of how deep into the result set the user has paged. Offset-based pagination, by contrast, would be linear in the offset and would degrade visibly past the first few pages.

The CSV export of audit query results is implemented as a `cleanup.audit_csv_export` River job that streams the result rows from the database, writes them in CSV format to a file in a shared volume, and updates the export status read model with the file path when complete. The dashboard's downloads page reads the export status and offers a download link that streams the file from the volume through a chi handler. The export file is retained for twenty-four hours and then purged by a cleanup job.

---

## 19. Webhook delivery components

The webhook delivery components are realized as the `SubscriptionManagement` use case for managing subscriptions and the `subscription.deliver` River job for executing deliveries. The components follow patterns from section three for the asynchronous projection of the subscription delivery dead-letter read model, section six for the job structure with retry and dead-letter handling, and section eight for spans named `webhook.deliver`.

A delivery job is enqueued by any use case that produces an event matching at least one subscription's filter. The enqueueing happens within the same transaction that appends the event to the event store, satisfying the transactional outbox property. The job arguments include the tenant identifier, the subscription identifier, the event identifier, and the attempt number (initially zero). The worker that picks up the job loads the subscription and the event from the database, constructs the payload with the standard envelope described in section eight of the System Architecture document, computes the HMAC signature, makes the HTTP POST request with the appropriate headers, and inspects the response. A successful delivery is recorded as a `subscription.delivery.attempted` event with outcome success; a failure is recorded as the same event type with outcome failure, and the job is rescheduled by River according to the retry configuration.

The retry budget exhaustion produces a final `subscription.delivery.attempted` event with outcome dead-letter, which the dead-letter projector picks up and includes in the dead-letter read model. The dashboard's dead-letter inspection page reads from this read model. Manual retry of a dead-lettered delivery is an administrative operation that enqueues a new delivery job with the same arguments and attempt number reset; the new attempt is independent of the prior chain.

Secret rotation is handled by the subscription aggregate: the rotate-secret event records the new secret hash and the old secret hash along with an overlap window expiry. During the overlap, the deliverer signs with the new secret but verification on the subscriber side may use either. After the overlap expires, an additional aggregate event rotates the old secret out, and the deliverer signs only with the new secret. The overlap window default is twenty-four hours and is configurable.

---

## 20. Tenant data export components

The tenant data export components are realized as the `Export` use case for initiating exports and the `export.run` River job for executing them. The components follow patterns from section two for the event store as the data source, section three for the inclusion of read model snapshots in the export, section six for the job structure, and section eight for spans named `command.start_export` and `job.export.run`.

The export job runs as a single River worker invocation per export (the concurrency limit is two, but a single export is one job). The job creates a working directory, iterates the event store using the `ReadByTenantAndTimeRange` port method (or the full range if no time bound was specified), writes the events in monthly batches into gzipped JSON files within the events subdirectory, snapshots each read model into the read-models subdirectory by querying the read model tables with a tenant filter, captures the configuration into the config subdirectory with sensitive fields redacted, computes SHA-256 checksums of every file, writes the manifest with the checksums, signs the manifest with the deployment's Ed25519 signing key, packages everything into a tar.gz archive in the shared download volume, updates the export status read model with the file path, and exits.

The job is interruptible: if the worker shuts down mid-export, the partial working directory is discarded on the next run and the export starts fresh. The job is not designed to resume from a partial state because the cost of restarting is acceptable (exports take seconds to minutes for the expected data volume) and the implementation simplification is significant.

The signing key is loaded at process startup from the `OPENGATE_SIGNING_KEY` environment variable or from a mounted secrets file at `/run/secrets/opengate-signing-key`, with the file taking precedence if both are present. The key is a base64-encoded Ed25519 private key. The corresponding public key is published at the well-known URL `/.well-known/opengate-signing-key.pub` by a small chi handler that reads the configured public key from the same configuration source.

The verification procedure documented in the System Architecture document section eight is the contract that a recipient of an export uses. The implementation does not include a verifier tool in this version; the documentation includes example code in Go and Python that a recipient can use to verify, and the test suite includes a verification test that exercises the procedure end-to-end against a freshly produced export.

---

# Part III — Cross-cutting concerns at the implementation level

## 21. Graceful shutdown implementation

Graceful shutdown is implemented as an ordered sequence of phases triggered by the process receiving a `SIGTERM` or `SIGINT` signal. The implementation lives in `cmd/opengate/main.go` and is the orchestrator of the entire process lifecycle. This section specifies the exact sequence.

Phase one is signal handling. The main function registers a signal handler on `SIGTERM` and `SIGINT` that cancels the application's root context. All subsequent phases are triggered by the root-context cancellation rather than by direct signal handling, which means the shutdown logic is testable without sending real signals.

Phase two is the readiness flip. The health check handler is configured with a shared atomic boolean named `isReady`; on root-context cancellation, the boolean is set to false. The readiness endpoint at `/health/ready` reads the boolean and returns 503 Service Unavailable when it is false. Container orchestrators stop routing new traffic to the container within the readiness probe interval (typically a few seconds in Docker Compose's healthcheck stanza).

Phase three is the HTTP server shutdown. The chi router is wrapped in a standard `http.Server` whose `Shutdown` method is called with a context that has a thirty-second timeout. The server stops accepting new connections immediately and waits for in-flight requests to complete up to the timeout. The thirty-second timeout is chosen as a multiple of the longest reasonable request duration in the system; queries with longer expected duration (such as the export job initiation, which can take several seconds) must complete within the timeout or be cut short, and the cut-short case logs a warning.

Phase four is the River worker pool shutdown. The River client's `Stop` method is called with a context that has a sixty-second timeout. River stops dequeuing new jobs immediately and waits for in-flight jobs to complete or to be released back to the queue (whichever the job kind's configuration prescribes). Most jobs in OpenGate are quick (sub-second), but the export job can take longer, and the sixty-second timeout accommodates the worst case.

Phase five is the SSE handler drain. Active SSE connections are tracked in a process-level set; on root-context cancellation, each connection's response writer is closed, signaling the client to reconnect. The drain is bounded by a five-second timeout; connections that have not closed within the timeout are forcibly cut.

Phase six is the OpenTelemetry flush. The trace provider's `Shutdown` method is called with a context that has a ten-second timeout. Buffered spans are exported to the OTel collector within the timeout; spans that fail to export are dropped with a warning logged.

Phase seven is the database connection pool close. The pgx pool's `Close` method is called and waits for all checked-out connections to be returned. By this point in the sequence, all previous phases have completed, and no checkouts are in progress, so the close is effectively immediate.

Phase eight is process exit. The main function returns, the process exits with code zero if all phases completed within their timeouts, or with a non-zero code if any phase had to be forcibly cut short. The exit code is captured in a Docker Compose log line that the operator can inspect after a restart.

A subtle point about the ordering is that the readiness flip is before the HTTP server shutdown. The reason is that the readiness flip causes the orchestrator to stop routing new traffic before the server stops accepting new connections; this avoids the failure mode where new connections arrive in the small window after the server has stopped accepting but before the readiness probe has detected the state. The ordering means that the readiness probe interval determines the gap between the flip and the next phase; the implementation includes a brief sleep (one second by default) between phases two and three to allow the orchestrator to act on the readiness flip before connections are refused.

---

## 22. Error handling and Problem Details

Error responses from the HTTP API follow RFC 9457 Problem Details for HTTP APIs, which standardizes a JSON shape for error responses across endpoints. The shape has fields `type` (a URI identifying the problem class), `title` (a short human-readable summary), `status` (the HTTP status code, duplicated in the response body for clients that cannot inspect headers), `detail` (a longer human-readable explanation), and `instance` (a URI identifying the specific occurrence of the problem, used for correlation with logs). OpenGate adds two extensions: `errors` (a structured array of field-level validation failures, for 422 Unprocessable Entity responses) and `trace_id` (the OpenTelemetry trace identifier of the failing request, for cross-reference with traces in Tempo).

```json
{
  "type": "https://docs.opengate.example/errors/credential-not-found",
  "title": "Credential not found",
  "status": 404,
  "detail": "The credential 7d4f...c8 does not exist in this tenant.",
  "instance": "/api/v1/tenants/abc/credentials/7d4f...c8",
  "trace_id": "abcdef0123456789abcdef0123456789"
}
```

The implementation uses a small `ProblemDetails` helper that constructs the response from a domain-level error and the request context. The helper consults an error-mapping table that translates each port-defined sentinel error to a Problem Details type, title, and status code. The mapping table is the only place in the code where domain errors are translated to HTTP semantics; the rest of the code uses domain errors directly. The mapping pattern means that adding a new domain error requires exactly two changes: adding the sentinel and adding the mapping row.

Validation errors from the OpenAPI request validator are formatted differently because they decompose into multiple field-level failures. The Problem Details `errors` extension carries an array of objects each with `pointer` (a JSON Pointer to the failing field), `code` (a stable validation code such as `format_invalid` or `required`), and `detail` (a human-readable explanation). The dashboard renders the array as inline form-field errors.

Errors are logged on the server side at the warn level (for 4xx errors that are client mistakes) or at the error level (for 5xx errors that are server failures). The log record includes the same trace identifier as the Problem Details response, allowing cross-reference between server logs and the response that the client saw.

---

## 23. Configuration and secrets

Configuration is loaded at process startup from environment variables. The implementation uses the `envconfig` pattern, where a Go struct with tagged fields is populated from environment variables of the corresponding name. The struct is defined in `internal/config/config.go` and is the single source of truth for configurable values in the system; the struct's fields are passed to the components that need them at composition root time, avoiding global state.

```go
// Config is the application's complete configuration. All values are loaded
// from environment variables at startup. The struct is read once and never
// mutated.
type Config struct {
    DatabaseURL       string        `envconfig:"DATABASE_URL" required:"true"`
    BypassRLSURL      string        `envconfig:"BYPASS_RLS_DATABASE_URL" required:"true"`
    HTTPAddr          string        `envconfig:"HTTP_ADDR" default:":8080"`
    SessionTimeout    time.Duration `envconfig:"SESSION_TIMEOUT" default:"60m"`
    SigningKey        []byte        `envconfig:"SIGNING_KEY" required:"true"`
    OTLPEndpoint      string        `envconfig:"OTLP_ENDPOINT" default:"otel-collector:4317"`
    LogLevel          slog.Level    `envconfig:"LOG_LEVEL" default:"INFO"`
    // ... additional fields
}
```

Secrets are loaded from environment variables in the default path, with optional override from mounted secrets files. The override pattern is: for any secret-typed configuration field, if a file exists at `/run/secrets/<lowercase_field_name>`, its contents replace the environment variable value. The file path follows the Docker secrets convention. The pattern allows the same code to work in a Docker Compose deployment (env vars in `.env`), a hardened deployment with Docker Secrets (mounted files), and a Kubernetes deployment with Secret resources (also mounted files).

The configuration struct includes a `Validate` method that the main function calls after loading. The method checks that secrets are at least the expected length, that URLs are well-formed, and that durations are in reasonable ranges. A validation failure causes the process to exit immediately with a clear error message identifying which field failed; this is the fail-fast posture appropriate for configuration errors, which are programmer errors rather than runtime conditions.

---

# Part IV — Closing

## 24. Deferred decisions

This document, like its predecessors, defers certain decisions to the documents that follow it in the documentation set. The Database Schema document, which is the next document to be produced, will own the following decisions.

The exact CREATE TABLE statements for every table referenced in this document, including the column definitions, the column-level constraints, the table-level constraints, the foreign keys, and the storage parameters such as fillfactor. The exact CREATE INDEX statements for every index referenced in this document, including the index type (b-tree, gin, gist, brin) and the column ordering.

The exact RLS policy definitions, including the policy name, the table to which the policy is attached, the USING clause, and the WITH CHECK clause where applicable. The set of tables that have RLS enabled and the set of tables that do not.

The exact migration script structure, including the migration tool to be used (`migrate` or `goose` or `atlas`), the migration file naming convention, the up and down migration patterns, and the policy for breaking versus non-breaking schema changes.

The exact `sqlc` configuration, including the generated package layout, the type mappings between Postgres types and Go types, and the query annotations such as `:one`, `:many`, and `:exec`.

The exact data retention policy implementation, including the retention windows for each event type, the schedule of the retention enforcement job, and the archival strategy for events past their retention window.

The exact event store partitioning strategy, including whether to partition by tenant_id, by occurred_at month, or by some combination, and the partition pruning strategy.

The exact behavior of the connection pool under load, including the maximum connection count, the connection lifetime, the connection acquisition timeout, and the strategy for handling pool exhaustion.

The Implementation Plan document, which follows the Database Schema document, will own the decomposition of the work into user stories with acceptance criteria, the estimation of each user story in t-shirt sizes or story points, the sequencing of the user stories into iterations, and the definition of done that the implementation must meet for each story.

---

## 25. Document status and next steps

This document is version one point zero of the System Design document for OpenGate. Subsequent revisions will be numbered version increments with appended changelogs.

The next document to be produced is the Database Schema document. The Database Schema document treats the design decisions made here as fixed inputs and specifies the concrete SQL realization of every data structure referenced in this document. After the Database Schema document, the Implementation Plan document follows, and after that, the actual implementation begins.

---

## 26. Index of decisions and concepts

The index below maps every significant concept and decision in this document to the section in which it is most fully developed. Where a concept is touched in multiple sections, the section with the primary treatment is listed first and the others as secondary references.

**Access decision algorithm.** Primary in section 16. Also referenced in sections 3, 4, 8, 14.

**Aggregate loading.** Primary in section 2. Also referenced in section 13.

**Advisory locks.** Primary in section 5. Also referenced in sections 6, 13.

**API key hashing and verification.** Primary in section 9. Also referenced in section 8 (cache key sensitivity).

**Argon2id parameters.** Primary in section 9.

**Asynchronous projection.** Primary in section 3. Also referenced in sections 12, 18, 19, 20.

**Audit log read model.** Primary in section 18. Also referenced in section 3.

**Authentication.** Primary in section 9. Also referenced in sections 11, 21.

**Bootstrap CLI.** Primary in section 11. Also referenced in sections 9, 10.

**BYPASSRLS connection pool.** Primary in section 10. Also referenced in section 11.

**Casbin authorizer.** Primary in section 11. Also referenced in section 7.

**Command idempotency.** Primary in section 4.

**Composition root.** Primary in section 7.

**Concurrency control on event append.** Primary in section 2.

**Configuration loading.** Primary in section 23.

**Connection pool tenant binding.** Primary in section 10.

**Context propagation.** Primary in section 7. Also referenced in sections 6, 8.

**Credential identifier generation.** Primary in section 13. Also referenced in section 5.

**CSV audit export.** Primary in section 18.

**Dead-letter queue.** Primary in section 19. Also referenced in sections 3, 6.

**DecisionEvaluator domain service.** Primary in section 14.

**Decision idempotency.** Primary in section 4. Also referenced in section 16.

**Door status read model.** Primary in section 15. Also referenced in section 3.

**Ed25519 signing.** Primary in section 20.

**Error handling via Problem Details.** Primary in section 22.

**Event envelope.** Primary in section 2.

**Event sourcing.** Primary in section 2. Also referenced in most component sections.

**Event store port.** Primary in section 2. Also referenced in section 7.

**Event type versioning and upcasting.** Primary in section 2.

**Export job.** Primary in section 20.

**Graceful shutdown.** Primary in section 21. Also referenced in sections 5, 8.

**Health check endpoints.** Primary in section 21.

**Hexagonal architecture at the code level.** Primary in section 7.

**HMAC signing of webhooks.** Primary in section 19.

**Idempotency.** Primary in section 4. Also referenced in sections 6, 13, 16, 17.

**Job retry configuration.** Primary in section 6.

**Keyset pagination for audit queries.** Primary in section 18.

**LISTEN/NOTIFY for SSE fanout.** Primary in section 15. Also referenced in section 11.

**Logging via slog.** Primary in section 8.

**Manifest signature for exports.** Primary in section 20.

**Member search via trigram index.** Primary in section 12.

**Metric naming convention.** Primary in section 8.

**Migration tooling.** Deferred to the Database Schema document; mentioned in section 24.

**Multi-tenant isolation.** Primary in section 10. Also referenced in most sections.

**OpenAPI request validation.** Mentioned in section 22.

**OpenTelemetry attributes.** Primary in section 8.

**Optimistic concurrency.** Primary in section 2.

**PHC password encoding.** Primary in section 9.

**Port contract conventions.** Primary in section 7.

**Postgres advisory locks.** Primary in section 5.

**Projection lag metric.** Primary in section 3.

**Projector job structure.** Primary in section 6. Also referenced in section 3.

**Reader API key.** Primary in section 9. Also referenced in section 15.

**Reader-driven push (SSE).** Primary in section 15. Also referenced in section 17.

**Reconciliation composite-key idempotency.** Primary in section 4. Also referenced in section 17.

**Retention policy.** Deferred to the Database Schema document; mentioned in section 24.

**RLS policies.** Primary in section 10. Deferred specifics in section 24.

**Row-Level Security force clause.** Primary in section 10.

**Secret rotation for subscriptions.** Primary in section 19.

**Secrets handling.** Primary in section 23. Also referenced in section 20.

**Session expiry (sliding window).** Primary in section 9.

**Session tokens.** Primary in section 9.

**SipHash for advisory lock identifiers.** Primary in section 5.

**Snapshots in event sourcing.** Primary in section 2 (decision to defer).

**Span naming convention.** Primary in section 8.

**`sqlc` configuration.** Deferred to the Database Schema document; mentioned in section 24.

**Synchronous projection.** Primary in section 3. Also referenced in sections 13, 14, 15, 16.

**Tenant identifier propagation.** Primary in section 10. Also referenced in section 7.

**Time window evaluation algorithm.** Primary in section 14.

**Trace context propagation through jobs.** Primary in section 6. Also referenced in section 8.

**Transactional outbox via River.** Primary in section 6. Also referenced in section 19.

**Trigram search index.** Primary in section 12.

**User aggregate.** Primary in section 11.

**Webhook delivery retry.** Primary in section 19. Also referenced in section 6.
