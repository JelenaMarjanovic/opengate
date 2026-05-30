package postgres_test

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"time"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestMigrationsRoundTrip applies every migration up, rolls every
// migration down, then applies up again. It exercises the Down sections,
// which would otherwise rot untested. The test runs against a throwaway
// Postgres container so it pollutes no developer database.
func TestMigrationsRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16.14-bookworm",
		tcpostgres.WithDatabase("opengate_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}

	newProvider := func() *goose.Provider {
		p, err := goose.NewProvider(goose.DialectPostgres, db, sub)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		return p
	}

	// First up: apply all migrations, then assert the full surface they create.
	if _, err := newProvider().Up(ctx); err != nil {
		t.Fatalf("first up: %v", err)
	}
	assertSchemaPresent(t, db, true)

	// Down to zero: roll EVERY migration back, exercising all Down sections
	// (including DROP OWNED BY / DROP ROLE in create_app_roles) rather than only
	// the most recent migration.
	if _, err := newProvider().DownTo(ctx, 0); err != nil {
		t.Fatalf("down to zero: %v", err)
	}
	assertSchemaPresent(t, db, false)

	// Second up: re-apply everything. Proves the Up path — including the
	// idempotent role-creation DO blocks — is repeatable after a full Down.
	if _, err := newProvider().Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	assertSchemaPresent(t, db, true)
}

// assertSchemaPresent asserts the presence (want=true) or absence (want=false)
// of the tenants and users tables and the opengate_bypass role together, so the
// round-trip verifies the full surface the migrations create and tear down.
func assertSchemaPresent(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	assertTableExists(t, db, "tenants", want)
	assertTableExists(t, db, "users", want)
	assertRoleExists(t, db, "opengate_bypass", want)
}

// assertTableExists checks the presence (or absence) of a public-schema table
// via the information schema, the portable way to test for a table without
// depending on it having rows.
func assertTableExists(t *testing.T, db *sql.DB, table string, want bool) {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)`, table,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("query table %q existence: %v", table, err)
	}
	if exists != want {
		t.Fatalf("table %q exists = %v, want %v", table, exists, want)
	}
}

// assertRoleExists checks the presence (or absence) of a Postgres role via
// pg_roles, confirming create_app_roles created (and its Down dropped) the role.
func assertRoleExists(t *testing.T, db *sql.DB, role string, want bool) {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("query role %q existence: %v", role, err)
	}
	if exists != want {
		t.Fatalf("role %q exists = %v, want %v", role, exists, want)
	}
}
