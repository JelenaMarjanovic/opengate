package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"
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

	// US-02.03 Step 1 additions: the add_tenant_slug migration's column shape and
	// its format CHECK behavior, asserted once on the fully-migrated schema.
	assertSlugColumnConstraints(t, db)
	assertSlugFormatCheck(t, db)

	// US-02.03 Step 2 addition: the grant_app_sessions migration's privileges on
	// the application role. Asserted after the second up (so the round-trip also
	// exercised this migration's REVOKE on the way down and GRANT on the way up).
	assertAppSessionGrants(t, db)

	// US-02.03 Step 4a addition: the grant_bypass_users_update migration's UPDATE
	// privilege on users for the bypass role. Asserted after the second up, so the
	// round-trip also exercised its REVOKE (down) and GRANT (up).
	assertBypassUsersUpdateGrant(t, db)
}

// assertAppSessionGrants verifies the grant_app_sessions migration gave
// opengate_app exactly UPDATE and DELETE on sessions — the privileges the
// authenticated session paths need — and withheld INSERT and SELECT (sessions
// are minted and looked up on the bypass pool, so the application role must not
// be able to forge or read them yet). Read from the catalog via
// has_table_privilege, independent of any row data.
func assertAppSessionGrants(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, c := range []struct {
		// query is a static, in-test constant (no user input), so it is safe to
		// pass to has_table_privilege as a literal.
		query string
		want  bool
	}{
		{`SELECT has_table_privilege('opengate_app', 'sessions', 'UPDATE')`, true},
		{`SELECT has_table_privilege('opengate_app', 'sessions', 'DELETE')`, true},
		{`SELECT has_table_privilege('opengate_app', 'sessions', 'INSERT')`, false},
		{`SELECT has_table_privilege('opengate_app', 'sessions', 'SELECT')`, false},
	} {
		var has bool
		if err := db.QueryRow(c.query).Scan(&has); err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if has != c.want {
			t.Errorf("%s = %v, want %v", c.query, has, c.want)
		}
	}
}

// assertBypassUsersUpdateGrant verifies the grant_bypass_users_update migration
// gave opengate_bypass UPDATE on users — needed by the login flow's
// rehash-on-login and last-login writes — alongside the SELECT and INSERT it
// already held from create_users. Read from the catalog via has_table_privilege,
// independent of any row data.
func assertBypassUsersUpdateGrant(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, c := range []struct {
		// query is a static, in-test constant (no user input), so it is safe to
		// pass to has_table_privilege as a literal.
		query string
		want  bool
	}{
		{`SELECT has_table_privilege('opengate_bypass', 'users', 'UPDATE')`, true},
		{`SELECT has_table_privilege('opengate_bypass', 'users', 'SELECT')`, true},
		{`SELECT has_table_privilege('opengate_bypass', 'users', 'INSERT')`, true},
	} {
		var has bool
		if err := db.QueryRow(c.query).Scan(&has); err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if has != c.want {
			t.Errorf("%s = %v, want %v", c.query, has, c.want)
		}
	}
}

// assertSchemaPresent asserts the presence (want=true) or absence (want=false)
// of the tenants, users, and sessions tables and the opengate_bypass role
// together, so the round-trip verifies the full surface the migrations create
// and tear down.
func assertSchemaPresent(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	assertTableExists(t, db, "tenants", want)
	assertTableExists(t, db, "users", want)
	assertTableExists(t, db, "sessions", want)
	assertRoleExists(t, db, "opengate_bypass", want)
}

// assertSlugColumnConstraints verifies the add_tenant_slug migration shaped the
// tenants.slug column as required: present, NOT NULL, and covered by the
// tenants_slug_unique UNIQUE constraint. These are structural assertions read
// from the catalog, independent of any row data.
func assertSlugColumnConstraints(t *testing.T, db *sql.DB) {
	t.Helper()

	// The column exists and is NOT NULL.
	var isNullable string
	err := db.QueryRow(
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'tenants' AND column_name = 'slug'`,
	).Scan(&isNullable)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("tenants.slug column is missing")
	}
	if err != nil {
		t.Fatalf("query tenants.slug nullability: %v", err)
	}
	if isNullable != "NO" {
		t.Errorf("tenants.slug is_nullable = %q, want NO (NOT NULL)", isNullable)
	}

	// A UNIQUE constraint named tenants_slug_unique exists on tenants.
	var hasUnique bool
	err = db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.table_constraints
			WHERE table_schema = 'public' AND table_name = 'tenants'
			  AND constraint_type = 'UNIQUE' AND constraint_name = 'tenants_slug_unique'
		)`,
	).Scan(&hasUnique)
	if err != nil {
		t.Fatalf("query tenants_slug_unique existence: %v", err)
	}
	if !hasUnique {
		t.Error("tenants_slug_unique UNIQUE constraint is missing")
	}
}

// assertSlugFormatCheck proves tenants_slug_format_check accepts a well-formed
// slug and rejects a malformed one (uppercase plus a space). It seeds via the
// superuser test connection, which is bypass-capable; RLS is not enabled on
// tenants at this step.
func assertSlugFormatCheck(t *testing.T, db *sql.DB) {
	t.Helper()

	// A valid slug is accepted. gen_random_uuid() (Postgres core) supplies the PK
	// so the test needs no uuid import; name/slug are the only non-default columns.
	if _, err := db.Exec(
		`INSERT INTO tenants (id, name, slug) VALUES (gen_random_uuid(), $1, $2)`,
		"Acme Gym", "acme-gym",
	); err != nil {
		t.Fatalf("insert valid slug 'acme-gym': %v", err)
	}

	// A malformed slug (uppercase and a space) must be rejected by the CHECK.
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, slug) VALUES (gen_random_uuid(), $1, $2)`,
		"Bad", "Bad Slug",
	)
	if err == nil {
		t.Fatal("insert of slug 'Bad Slug' succeeded; want a tenants_slug_format_check violation")
	}
	if !strings.Contains(err.Error(), "tenants_slug_format_check") {
		t.Errorf("insert of slug 'Bad Slug' failed with %v; want a tenants_slug_format_check violation", err)
	}
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
