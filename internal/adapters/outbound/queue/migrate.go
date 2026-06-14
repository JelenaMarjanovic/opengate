package queue

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"
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
	migrator, err := newRiverMigrator(pool, logger)
	if err != nil {
		return err
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

// newRiverMigrator builds a rivermigrate.Migrator bound to the dedicated `river`
// schema over the given pool. River's migrator needs a pgxpool-backed driver (the
// goose path uses database/sql, which rivermigrate cannot consume), so every
// migrate-phase entry point -- up, teardown, and status -- constructs it the same
// way here. The migrator's transaction type is pgx.Tx, inferred from the driver.
func newRiverMigrator(pool *pgxpool.Pool, logger *slog.Logger) (*rivermigrate.Migrator[pgx.Tx], error) {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{
		Schema: riverSchema,
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: new river migrator: %w", err)
	}
	return migrator, nil
}

// TeardownRiver reverses MigrateRiver: it removes every River table and then the
// `river` schema itself. It is the River half of a full `migrate down` to version
// 0 (decision D2). The migrate subcommand calls it ONLY when the goose phase has
// brought the application schema all the way down to version 0; a partial
// `migrate down` leaves River untouched, because River's tables are a unit and a
// partial River teardown would leave a broken schema.
//
// The sequence is the reverse of MigrateRiver's up (which was CREATE SCHEMA ->
// rivermigrate up -> grants):
//
//  1. rivermigrate down-all -- rolls back every River migration, dropping every
//     River table (including river_migration).
//  2. DROP SCHEMA IF EXISTS river -- removes the now-empty schema.
//
// Grants are not revoked explicitly: every schema- and table-scoped privilege is
// dropped together with the schema and its tables (decision D2.3).
//
// TeardownRiver is idempotent. down-all on an absent schema is a no-op: River
// resolves river_migration through to_regclass, which reports "absent" (rather
// than erroring) when the schema is gone, so ExistingVersions comes back empty and
// nothing is rolled back; DROP SCHEMA IF EXISTS then tolerates the missing schema.
//
// pool must be the migration role's pool; TeardownRiver does not own its lifecycle.
//
// NB on the down-all opts: down-all REQUIRES MigrateOpts{TargetVersion: -1}.
// rivermigrate defaults the down direction to a SINGLE step "for safety" (in
// v0.39.0 applyMigrations sets maxSteps=1 when TargetVersion==0, which is the
// nil-opts default), so passing nil here would roll back only the latest River
// migration and leave the schema non-empty, making the subsequent DROP SCHEMA
// fail. -1 is River's documented "remove the schema completely" target.
func TeardownRiver(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	migrator, err := newRiverMigrator(pool, logger)
	if err != nil {
		return err
	}

	// Phase 1 (reverse of up phase 3): roll back every applied River migration.
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionDown, &rivermigrate.MigrateOpts{
		TargetVersion: -1,
	})
	if err != nil {
		return fmt.Errorf("queue: river migrate down-all: %w", err)
	}
	logger.InfoContext(ctx, "river migrations rolled back",
		slog.Int("versions_removed", len(res.Versions)))

	// Phase 2 (reverse of up phase 2): drop the now-empty schema. IF EXISTS keeps
	// this a no-op when River was never migrated. No CASCADE is needed -- down-all
	// already removed every table, type, and function River created -- and omitting
	// it avoids silently dropping anything unexpected that might live in the schema.
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS river"); err != nil {
		return fmt.Errorf("queue: drop river schema: %w", err)
	}
	logger.InfoContext(ctx, "river schema dropped", slog.String("schema", riverSchema))

	return nil
}

// RiverMigrationStatus is a read-only snapshot of River's migration state, used by
// `migrate status` and `migrate version` to report River alongside goose (decision
// D1). It is derived from rivermigrate's ExistingVersions (applied in the database)
// and AllVersions (every migration this River build knows about).
type RiverMigrationStatus struct {
	// SchemaPresent is false when the `river` schema is absent -- River was never
	// migrated, or has been torn down. AppliedVersion, AppliedCount, and Pending are
	// then zero/empty, while TotalCount still reflects the migrations the build ships.
	SchemaPresent bool
	// AppliedVersion is the highest applied River migration version, or 0 if none.
	AppliedVersion int
	// AppliedCount is the number of River migrations applied in the database.
	AppliedCount int
	// TotalCount is the number of River migrations this build knows about.
	TotalCount int
	// Pending lists known-but-unapplied migration versions, ascending. It is empty
	// after a full `migrate up`, and also empty when SchemaPresent is false (an
	// absent schema is reported as "not present", not as a list of pending work).
	Pending []int
}

// RiverStatus reports River's migration state for the read-only status/version
// actions (decision D1). It never mutates the database and never fails merely
// because River is absent: a missing `river` schema yields SchemaPresent=false with
// a zero version, not an error.
//
// pool must be the migration role's pool; RiverStatus does not own its lifecycle.
func RiverStatus(ctx context.Context, pool *pgxpool.Pool) (RiverMigrationStatus, error) {
	// A quiet migrator: the read-only queries below never log, but a discard logger
	// guarantees the status/version output stays free of stray River log lines.
	migrator, err := newRiverMigrator(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return RiverMigrationStatus{}, err
	}

	// AllVersions reads the embedded migration set -- no database access -- so the
	// total is known even when River is absent.
	all := migrator.AllVersions()
	status := RiverMigrationStatus{TotalCount: len(all)}

	// Resolve schema presence explicitly so an absent River is reported as such
	// (decision D1) rather than as "0 applied". This is the same signal the teardown
	// round-trip asserts against (information_schema.schemata).
	present, err := riverSchemaExists(ctx, pool)
	if err != nil {
		return RiverMigrationStatus{}, err
	}
	if !present {
		return status, nil
	}
	status.SchemaPresent = true

	// ExistingVersions returns the applied migrations ordered ascending by version,
	// so the last element is the current River version.
	existing, err := migrator.ExistingVersions(ctx)
	if err != nil {
		return RiverMigrationStatus{}, fmt.Errorf("queue: river existing versions: %w", err)
	}
	status.AppliedCount = len(existing)
	if len(existing) > 0 {
		status.AppliedVersion = existing[len(existing)-1].Version
	}

	applied := make(map[int]struct{}, len(existing))
	for _, m := range existing {
		applied[m.Version] = struct{}{}
	}
	for _, m := range all {
		if _, ok := applied[m.Version]; !ok {
			status.Pending = append(status.Pending, m.Version)
		}
	}

	return status, nil
}

// riverSchemaExists reports whether the dedicated `river` schema is present. It is
// the clean-absence probe for RiverStatus: querying information_schema avoids
// touching river_migration, which may not exist.
func riverSchemaExists(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		riverSchema,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("queue: check river schema: %w", err)
	}
	return exists, nil
}
