// Package queue holds the River job-queue adapter (US-03.04). It owns the single
// source of truth for River client configuration and the River-schema migration
// phase that the `migrate` subcommand runs after the application (goose) schema.
//
// River is integrated under a dedicated `river` Postgres schema (decision A1) so
// its tables never collide with the application schema and its grants stay
// auditable in isolation. The constructor below is role-parameterised (decision
// A2): one place builds the shared config, and the caller picks the role and the
// pool whose driver backs the client.
//
// Step 1 (this file) builds only the foundation: the constructor skeleton with
// empty Queues/Workers/PeriodicJobs. The tx-scoped JobEnqueuer adapter (Step 2)
// and the worker pool lifecycle (Step 3) land in later steps.
package queue

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// riverAdvisoryLockPrefix namespaces River's internal advisory locks to the
// high 32 bits = 0x4F47 ("OG" for OpenGate). River shifts this prefix into the
// top half of the 64-bit advisory-lock key space, so every River lock shares
// this band and cannot collide with locks taken elsewhere in the application.
//
// OpenGate's own application advisory locks come from internal/coordination/
// advisory.LockID (built in US-03.05) and will be made PROVABLY disjoint from
// this band by forcing the sign bit on that side — out of scope here. This
// constant only pins River's band; it must stay a fixed positive int32 so the
// band never moves and never overlaps a future application lock prefix.
const riverAdvisoryLockPrefix int32 = 0x4F47

// riverSchema is the dedicated Postgres schema that holds every River table
// (decision A1). It is set as Config.Schema on every client and on the migrator,
// so River never reads or writes outside this schema regardless of search_path.
const riverSchema = "river"

// RiverRole selects which side of the queue a client serves. The two roles use
// the SAME shared config (schema, logger, advisory-lock band) and differ only in
// the pool whose driver backs them and in whether they will work jobs:
//
//   - RoleAPI    -- insert-only. Built on the RLS-bound application pool's driver.
//     It is never Start()ed; Step 2's InsertTx runs on a passed transaction, not
//     on the client's own pool, so the pool is nominal here. River still requires
//     a driver (hence a pool) at construction even for an insert-only client.
//   - RoleWorker -- the future worker. Built on the BYPASSRLS pool's driver, since
//     the worker processes jobs across tenants outside any single tenant's RLS
//     scope. Its Queues/Workers/PeriodicJobs are empty in Step 1 and populated in
//     Step 3 and later stories; it is not Start()ed until Step 3.
type RiverRole int

const (
	// RoleAPI is the insert-only client used by the command path.
	RoleAPI RiverRole = iota
	// RoleWorker is the job-processing client used by the worker subcommand.
	RoleWorker
)

// String renders the role for logs and error messages.
func (r RiverRole) String() string {
	switch r {
	case RoleAPI:
		return "api"
	case RoleWorker:
		return "worker"
	default:
		return fmt.Sprintf("RiverRole(%d)", int(r))
	}
}

// workerConfig carries the worker-role-only River configuration that Step 1's
// shared config deliberately left empty. It is the extension shape chosen for
// decision A2 (extend the single constructor rather than fork it): the shared
// config (Schema/Logger/AdvisoryLockPrefix) stays the constructor's single
// source, and the worker branch overlays exactly the four fields that turn an
// inert client into a job-processing pool.
//
//   - Queues:   which queues to poll and with how many concurrent workers.
//   - Workers:  the worker registry. May be EMPTY (river.NewWorkers() with no
//     AddWorker calls) — that is the intended production foundation in Step 3:
//     the pool runs and polls the default queue, but no real job kind has a
//     worker yet, so nothing is processed until later stories register one.
//   - Middleware: the global middleware chain (the trace-extract WorkerMiddleware
//     lives here, registered once for every worked job — decision B3).
//   - SoftStopTimeout: River's graceful-stop budget. On a soft stop, in-flight
//     jobs get this long to finish before River cancels their contexts; the
//     worker subcommand sets it to the shutdown grace budget minus a margin
//     (decision A3).
//
// It is a *workerConfig: nil (and required-nil) for RoleAPI, non-nil for
// RoleWorker. Keeping it a pointer makes the "worker-only" nature explicit at
// every call site and lets newRiverClient reject a role/config mismatch as a
// composition-root wiring error.
type workerConfig struct {
	queues          map[string]river.QueueConfig
	workers         *river.Workers
	middleware      []rivertype.Middleware
	softStopTimeout time.Duration
}

// newRiverClient builds a River client for the given role over pool's driver.
// It is the single source of truth for shared River client configuration
// (decision A2): both roles get the dedicated `river` schema, the project slog
// logger, and the pinned advisory-lock band.
//
// The two roles diverge only in the worker overlay:
//
//   - RoleAPI is insert-only by construction. It leaves Queues/Workers/
//     Middleware/SoftStopTimeout at their zero values, so River builds an inert
//     client that never works jobs. wc MUST be nil — a non-nil worker config on
//     the insert-only role is a wiring error and is rejected.
//   - RoleWorker overlays wc (required non-nil): its Queues, Workers (possibly an
//     empty registry — the Step 3 foundation), global Middleware, and
//     SoftStopTimeout. This is the client the worker subcommand Start()s.
//
// The pool is supplied by the caller (the composition root) so this constructor
// stays free of any decision about which DSN/role a pool is bound to: the caller
// passes the RLS-bound pool for RoleAPI and the bypass pool for RoleWorker.
func newRiverClient(role RiverRole, pool *pgxpool.Pool, logger *slog.Logger, wc *workerConfig) (*river.Client[pgx.Tx], error) {
	if pool == nil {
		return nil, fmt.Errorf("queue: nil pool for river %s client", role)
	}
	if logger == nil {
		return nil, fmt.Errorf("queue: nil logger for river %s client", role)
	}

	// riverpgxv5.New infers TTx as pgx.Tx from the pgx pool, so the returned
	// client is *river.Client[pgx.Tx] -- the transaction type Step 2's InsertTx
	// accepts.
	driver := riverpgxv5.New(pool)

	// Shared config -- identical for both roles and the single source for these
	// three fields per A2.
	cfg := &river.Config{
		Schema:             riverSchema,
		Logger:             logger,
		AdvisoryLockPrefix: riverAdvisoryLockPrefix,
	}

	// Worker overlay. Each role accepts exactly one valid config shape, so a
	// mismatch (RoleWorker without a config, or RoleAPI with one) is caught here
	// as a fail-fast wiring error rather than producing a silently-wrong client.
	switch role {
	case RoleWorker:
		if wc == nil {
			return nil, fmt.Errorf("queue: nil worker config for river %s client", role)
		}
		cfg.Queues = wc.queues
		cfg.Workers = wc.workers
		cfg.Middleware = wc.middleware
		cfg.SoftStopTimeout = wc.softStopTimeout
	case RoleAPI:
		if wc != nil {
			return nil, fmt.Errorf("queue: worker config supplied for insert-only river %s client", role)
		}
	default:
		return nil, fmt.Errorf("queue: unknown river role %s", role)
	}

	client, err := river.NewClient(driver, cfg)
	if err != nil {
		return nil, fmt.Errorf("queue: new river %s client: %w", role, err)
	}
	return client, nil
}
