package queue_test

import (
	"context"
	"database/sql"
	"io"
	"io/fs"
	"log/slog"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/queue"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// wantRiverTables is the table set River 0.39.0 creates in the dedicated schema
// (riverdriver/riverpgxv5 migrations 001-006). The migration test asserts the
// full set is present, not merely river_job, so a future River upgrade that adds
// or renames a table surfaces here rather than silently slipping past the grants.
var wantRiverTables = []string{
	"river_client",
	"river_client_queue",
	"river_job",
	"river_leader",
	"river_migration",
	"river_queue",
}

// TestRiverMigrationSequence runs the full migrate `up` sequence -- goose up,
// then queue.MigrateRiver (CREATE SCHEMA, rivermigrate up, grants) -- against a
// fresh Postgres and asserts the resulting schema, tables, and privileges
// (Step 1 test 1, decisions R1 + R2).
func TestRiverMigrationSequence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := testsupport.StartPostgres(ctx, t)

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	runFullSequence(ctx, t, dsn)

	db := openSQL(ctx, t, dsn)

	// --- Schema and tables exist. ---
	if !schemaExists(t, db, "river") {
		t.Fatal("river schema does not exist after MigrateRiver")
	}
	got := riverTables(t, db)
	assertTableSet(t, got, wantRiverTables)

	// --- opengate_app: command-path grants (Step 2 InsertTx). ---
	assertSchemaPrivilege(t, db, "opengate_app", "river", "USAGE", true)
	assertTablePrivilege(t, db, "opengate_app", "river.river_job", "INSERT", true)
	// SELECT is needed because River's insert RETURNINGs the row.
	assertTablePrivilege(t, db, "opengate_app", "river.river_job", "SELECT", true)
	// bigserial id => nextval() on the sequence requires USAGE.
	assertSequencePrivilege(t, db, "opengate_app", "river.river_job_id_seq", "USAGE", true)
	// The command path must NOT be able to mutate or delete jobs; that is worker
	// territory (opengate_bypass). Proves the app grant stayed minimal.
	assertTablePrivilege(t, db, "opengate_app", "river.river_job", "UPDATE", false)
	assertTablePrivilege(t, db, "opengate_app", "river.river_job", "DELETE", false)

	// --- opengate_bypass: worker lifecycle grants (Step 3 worker client). ---
	assertSchemaPrivilege(t, db, "opengate_bypass", "river", "USAGE", true)
	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		assertTablePrivilege(t, db, "opengate_bypass", "river.river_job", priv, true)
		// Spot-check a second, non-job table to prove the ALL TABLES grant landed
		// across the schema, not only on river_job.
		assertTablePrivilege(t, db, "opengate_bypass", "river.river_queue", priv, true)
	}
	assertSequencePrivilege(t, db, "opengate_bypass", "river.river_job_id_seq", "USAGE", true)
}

// TestRiverMigrationIdempotency runs the full sequence twice and asserts the
// second run is a clean no-op with a stable end state (Step 1 test 2): CREATE
// SCHEMA IF NOT EXISTS, rivermigrate (idempotent in 0.39.0), and GRANT are all
// re-runnable.
func TestRiverMigrationIdempotency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := testsupport.StartPostgres(ctx, t)

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// First run.
	runFullSequence(ctx, t, dsn)
	db := openSQL(ctx, t, dsn)
	firstTables := riverTables(t, db)

	// Second run -- must not error and must not change the schema.
	runFullSequence(ctx, t, dsn)
	secondTables := riverTables(t, db)

	assertTableSet(t, secondTables, wantRiverTables)
	if len(firstTables) != len(secondTables) {
		t.Errorf("river table count changed across runs: first=%d second=%d",
			len(firstTables), len(secondTables))
	}

	// Privileges remain exactly as the first run left them (GRANT is idempotent).
	assertTablePrivilege(t, db, "opengate_app", "river.river_job", "INSERT", true)
	assertSchemaPrivilege(t, db, "opengate_bypass", "river", "USAGE", true)
}

// runFullSequence runs the complete migrate `up` sequence the subcommand runs:
// goose up (phase 1) followed by queue.MigrateRiver (phases 2-4). The DSN is the
// container superuser, which owns CREATE ROLE / CREATE SCHEMA -- the same role
// the migrate subcommand uses operationally.
func runFullSequence(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	gooseUp(ctx, t, dsn)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open river pool: %v", err)
	}
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := queue.MigrateRiver(ctx, pool, logger); err != nil {
		t.Fatalf("MigrateRiver: %v", err)
	}
}

// gooseUp applies every embedded application migration up against dsn, mirroring
// phase 1 of the migrate subcommand.
func gooseUp(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	db := openSQL(ctx, t, dsn)
	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

// openSQL opens a database/sql handle over the pgx stdlib driver, registered for
// cleanup. Used for goose and for catalog assertions.
func openSQL(ctx context.Context, t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// schemaExists reports whether a schema is present via information_schema.
func schemaExists(t *testing.T, db *sql.DB, schema string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		schema,
	).Scan(&exists); err != nil {
		t.Fatalf("query schema %q existence: %v", schema, err)
	}
	return exists
}

// riverTables returns the sorted base-table names in the river schema.
func riverTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'river' AND table_type = 'BASE TABLE'
		 ORDER BY table_name`,
	)
	if err != nil {
		t.Fatalf("enumerate river tables: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate river tables: %v", err)
	}
	return names
}

// assertTableSet fails unless got contains exactly the wanted table names.
func assertTableSet(t *testing.T, got, want []string) {
	t.Helper()
	gotSet := make(map[string]bool, len(got))
	for _, n := range got {
		gotSet[n] = true
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("river table %q missing; got %v", w, got)
		}
	}
	if len(got) != len(want) {
		sortedWant := append([]string(nil), want...)
		sort.Strings(sortedWant)
		t.Errorf("river table set = %v, want %v", got, sortedWant)
	}
}

// assertTablePrivilege checks has_table_privilege(role, table, priv) == want. The
// role/table/priv are static in-test constants, safe as has_*_privilege literals.
func assertTablePrivilege(t *testing.T, db *sql.DB, role, table, priv string, want bool) {
	t.Helper()
	var has bool
	if err := db.QueryRow(
		`SELECT has_table_privilege($1, $2, $3)`, role, table, priv,
	).Scan(&has); err != nil {
		t.Fatalf("has_table_privilege(%s, %s, %s): %v", role, table, priv, err)
	}
	if has != want {
		t.Errorf("has_table_privilege(%s, %s, %s) = %v, want %v", role, table, priv, has, want)
	}
}

// assertSchemaPrivilege checks has_schema_privilege(role, schema, priv) == want.
func assertSchemaPrivilege(t *testing.T, db *sql.DB, role, schema, priv string, want bool) {
	t.Helper()
	var has bool
	if err := db.QueryRow(
		`SELECT has_schema_privilege($1, $2, $3)`, role, schema, priv,
	).Scan(&has); err != nil {
		t.Fatalf("has_schema_privilege(%s, %s, %s): %v", role, schema, priv, err)
	}
	if has != want {
		t.Errorf("has_schema_privilege(%s, %s, %s) = %v, want %v", role, schema, priv, has, want)
	}
}

// assertSequencePrivilege checks has_sequence_privilege(role, seq, priv) == want.
func assertSequencePrivilege(t *testing.T, db *sql.DB, role, seq, priv string, want bool) {
	t.Helper()
	var has bool
	if err := db.QueryRow(
		`SELECT has_sequence_privilege($1, $2, $3)`, role, seq, priv,
	).Scan(&has); err != nil {
		t.Fatalf("has_sequence_privilege(%s, %s, %s): %v", role, seq, priv, err)
	}
	if has != want {
		t.Errorf("has_sequence_privilege(%s, %s, %s) = %v, want %v", role, seq, priv, has, want)
	}
}
