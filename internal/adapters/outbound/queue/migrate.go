package queue

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// riverGrants are the idempotent privilege grants applied to the `river` schema
// after rivermigrate has created its tables (migrate phase 4, decision R1). They
// run as a re-runnable step on every `migrate up` -- NOT as goose migrations --
// because goose runs entirely in phase 1, before the River tables exist (phase
// 3). Re-running them after a future River upgrade re-covers the schema,
// including any newly added tables (the ALL TABLES / ALL SEQUENCES forms expand
// over whatever exists at execution time).
//
// The grant intent (decision R2) was verified empirically against the River
// 0.39.0 schema (riverdriver/riverpgxv5 migrations 001-006):
//
//	river_job        -- bigserial id => sequence river_job_id_seq, the job table
//	river_leader     -- UNLOGGED, leadership election
//	river_queue      -- queue metadata
//	river_client     -- UNLOGGED, active-client tracking
//	river_client_queue -- UNLOGGED, per-client queue state
//	river_migration  -- rivermigrate's own version ledger ((line, version) PK,
//	                    no id sequence after migration 005)
//
// The only sequence in the schema is river_job_id_seq (river_migration's id
// sequence is dropped when migration 005 rebuilds that table without an id).
var riverGrants = []string{
	// --- opengate_app: the RLS-bound command path (Step 2's InsertTx). ---
	// USAGE on the schema is the prerequisite for referencing any object in it;
	// without it every river.* reference fails with "permission denied for schema".
	`GRANT USAGE ON SCHEMA river TO opengate_app`,
	// INSERT enqueues jobs. SELECT is required too: River's insert uses
	// `RETURNING` the inserted row, and Postgres needs SELECT on a column to
	// return it (the same reason the events grant pairs SELECT with INSERT).
	`GRANT SELECT, INSERT ON river.river_job TO opengate_app`,
	// Column-level UPDATE on river_job.kind. River's InsertTx (the fast insert
	// path, JobInsertFastMany) is an upsert -- `INSERT ... ON CONFLICT (unique_key)
	// DO UPDATE SET kind = EXCLUDED.kind` -- and Postgres requires UPDATE privilege
	// for any statement that CONTAINS a DO UPDATE action, checked statically at
	// plan time even when no conflict can occur (our jobs carry a NULL unique_key,
	// so the arbiter never fires, yet the privilege is still demanded). Without
	// this, every enqueue fails with "permission denied for table river_job"
	// (42501) -- verified empirically; Step 1's grant set missed it.
	//
	// The grant is scoped to the single column the upsert writes (kind), NOT the
	// whole table, so it stays least-privilege: has_table_privilege(.,'UPDATE')
	// remains false (the negative assertion below still holds), and opengate_app
	// still cannot UPDATE state/attempt/etc. -- the worker (opengate_bypass) keeps
	// exclusive ownership of the job lifecycle. If a future River version's DO
	// UPDATE writes more columns, enqueue will fail the same visible way and this
	// column list is where it gets widened.
	`GRANT UPDATE (kind) ON river.river_job TO opengate_app`,
	// river_job.id is bigserial, so an INSERT that omits id calls nextval() on
	// river_job_id_seq, which requires USAGE on that sequence. The named sequence
	// is the bigserial-derived <table>_<column>_seq; granting it (rather than ALL
	// SEQUENCES) keeps opengate_app's reach minimal -- it gets nothing on any
	// worker-only sequence a future River version might add.
	`GRANT USAGE ON SEQUENCE river.river_job_id_seq TO opengate_app`,

	// --- opengate_bypass: cross-tenant worker infra (Step 3's worker client). ---
	// The worker runs the full job lifecycle plus leadership/queue bookkeeping
	// across tenants, so it gets USAGE on the schema and full DML on every table.
	// ALL TABLES / ALL SEQUENCES expand over the current table set and are re-run
	// on each upgrade to cover newly added tables (idempotent).
	`GRANT USAGE ON SCHEMA river TO opengate_bypass`,
	`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA river TO opengate_bypass`,
	`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA river TO opengate_bypass`,
}

// MigrateRiver runs the River-schema phase of the `migrate up` sequence
// (decision R1, phases 2-4), each in its own transaction and each idempotent:
//
//  2. CREATE SCHEMA IF NOT EXISTS river -- rivermigrate creates the tables WITHIN
//     the schema but never the schema itself, so the migration role must create
//     it first (it already owns CREATE here: it creates the application schema).
//  3. rivermigrate up -- creates the River tables in the `river` schema. Idempotent
//     in River 0.39.0 (a re-run targeting the latest version is a no-op).
//  4. River-schema grants -- the idempotent GRANT block above, applied AFTER the
//     tables exist (phase 3), in a single transaction.
//
// pool must be the migration role's pool (built from the migration DSN), and it
// is the caller's responsibility to close it. MigrateRiver does not own the pool
// lifecycle.
func MigrateRiver(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	// Phase 2: ensure the schema exists. pool.Exec runs in autocommit, i.e. its
	// own transaction, satisfying the "separate transactions" requirement.
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS river"); err != nil {
		return fmt.Errorf("queue: create river schema: %w", err)
	}
	logger.InfoContext(ctx, "river schema ensured", slog.String("schema", riverSchema))

	// Phase 3: run River's own migrations into the dedicated schema. The migrator
	// manages its own transaction(s) internally.
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{
		Schema: riverSchema,
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("queue: new river migrator: %w", err)
	}
	// nil opts = migrate fully up; idempotent in 0.39.0. We call Migrate, never
	// Validate, so 0.39.0's Validate-signature change does not affect us.
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("queue: river migrate up: %w", err)
	}
	logger.InfoContext(ctx, "river migrations applied",
		slog.Int("versions_applied", len(res.Versions)))

	// Phase 4: apply the schema grants in one transaction. They are individually
	// idempotent, but a single transaction makes the grant step all-or-nothing.
	if err := applyRiverGrants(ctx, pool); err != nil {
		return fmt.Errorf("queue: apply river grants: %w", err)
	}
	logger.InfoContext(ctx, "river schema grants applied",
		slog.Int("statements", len(riverGrants)))

	return nil
}

// applyRiverGrants executes the river grant block inside a single transaction so
// the grants commit together. Every statement is parameterless DDL, so each is
// sent on its own Exec (pgx's extended protocol does not batch multi-statement
// strings).
func applyRiverGrants(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin grant tx: %w", err)
	}
	// Rollback is a no-op after a successful Commit; on any early return it undoes
	// the partial grant block.
	defer func() { _ = tx.Rollback(ctx) }()

	for _, stmt := range riverGrants {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("grant %q: %w", stmt, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit grant tx: %w", err)
	}
	return nil
}
