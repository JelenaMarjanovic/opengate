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

	// First up: apply all migrations.
	if _, err := newProvider().Up(ctx); err != nil {
		t.Fatalf("first up: %v", err)
	}
	assertTenantsExists(t, db, true)

	// Down: roll back the most recent migration (we have one).
	if _, err := newProvider().Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertTenantsExists(t, db, false)

	// Second up: re-apply. Proves the Up is repeatable after a Down.
	if _, err := newProvider().Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	assertTenantsExists(t, db, true)
}

// assertTenantsExists checks the presence (or absence) of the tenants
// table via the information schema, which is the portable way to test
// for a table without depending on it having rows.
func assertTenantsExists(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'tenants'
		)`,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("query table existence: %v", err)
	}
	if exists != want {
		t.Fatalf("tenants table exists = %v, want %v", exists, want)
	}
}
