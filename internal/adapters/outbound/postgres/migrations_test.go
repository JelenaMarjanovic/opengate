package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// grantEventsVersion is the goose version (timestamp) of the grant_events
// migration (20260608090300_grant_events.sql), the one immediately below
// add_events_insert_xid. TestInsertXidMigrationRollback rolls down to it to
// isolate the xid pair's Down without depending on how many migrations sit above.
const grantEventsVersion int64 = 20260608090300

// TestMigrationsRoundTrip applies every migration up, rolls every
// migration down, then applies up again. It exercises the Down sections,
// which would otherwise rot untested. The test runs against a throwaway
// Postgres container so it pollutes no developer database.
func TestMigrationsRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()

	container := testsupport.StartPostgres(ctx, t)

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

	// US-02.04 Step 1 addition: the create_casbin_rules migration's seeded v1
	// policy. Asserted after the second up so the round-trip also re-applied the
	// INSERT after a full Down, proving the seed is part of the repeatable Up.
	assertCasbinPolicySeeded(t, db)

	// US-03.05 additions: the add_events_insert_xid columns/index and the
	// grant_projection_progress privileges. Asserted after the second up, so the
	// round-trip also exercised their Down (DROP COLUMN/INDEX, REVOKE) on the way to
	// zero and re-applied them on the way back up.
	assertProjectionXidSchema(t, db)

	// US-03.06 commit 1 additions: the three idempotency cache tables' RLS and the
	// command/cleanup grants. Asserted after the second up, so the round-trip also
	// exercised their Down (DROP POLICY / DISABLE RLS / REVOKE) on the way to zero
	// and re-applied them on the way back up.
	assertIdempotencyKeysSchema(t, db)
}

// TestInsertXidMigrationRollback proves the US-03.05 migrations' Down sections in
// isolation: rolling back grant_projection_progress and add_events_insert_xid
// removes exactly the two columns and the index while leaving the events table (and
// the rest of the schema below them) intact, and a subsequent Up re-creates them.
// The full round-trip drops events entirely on its way to zero, so it cannot show
// the column "present then absent while events persists" — this test does.
//
// It rolls down to grantEventsVersion (the migration immediately below
// add_events_insert_xid) rather than counting a fixed number of Down steps, so it
// stays correct as later stories stack further migrations on top of the xid pair —
// e.g. US-03.06 adds the idempotency tables above it.
func TestInsertXidMigrationRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := testsupport.StartPostgres(ctx, t)
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

	// Up: the columns and index are present.
	if _, err := newProvider().Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !columnExists(t, db, "events", "insert_xid") {
		t.Fatal("events.insert_xid missing after up")
	}
	if !indexExists(t, db, "events_insert_xid_idx") {
		t.Fatal("events_insert_xid_idx missing after up")
	}
	if !columnExists(t, db, "projection_progress", "last_consumed_xid") {
		t.Fatal("projection_progress.last_consumed_xid missing after up")
	}

	// Roll down to the migration immediately below add_events_insert_xid. This
	// reverses every migration above grant_events — including the xid pair under
	// test (and any later stories stacked on top, such as the US-03.06 idempotency
	// tables) — while keeping events itself, which is created below the target.
	// Targeting a version rather than counting Down steps keeps the test robust as
	// the migration stack grows.
	if _, err := newProvider().DownTo(ctx, grantEventsVersion); err != nil {
		t.Fatalf("down to grant_events (%d): %v", grantEventsVersion, err)
	}

	// The columns and index are gone, but events itself survives.
	assertTableExists(t, db, "events", true)
	if columnExists(t, db, "events", "insert_xid") {
		t.Error("events.insert_xid still present after its migration's Down")
	}
	if indexExists(t, db, "events_insert_xid_idx") {
		t.Error("events_insert_xid_idx still present after its migration's Down")
	}
	if columnExists(t, db, "projection_progress", "last_consumed_xid") {
		t.Error("projection_progress.last_consumed_xid still present after its migration's Down")
	}

	// Up again re-creates them, proving the Up is repeatable after a partial Down.
	if _, err := newProvider().Up(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if !columnExists(t, db, "events", "insert_xid") {
		t.Fatal("events.insert_xid missing after re-up")
	}
	if !indexExists(t, db, "events_insert_xid_idx") {
		t.Fatal("events_insert_xid_idx missing after re-up")
	}
}

// columnExists reports whether table.column exists (and is not a dropped column),
// read from pg_attribute so it is independent of any row data.
func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	// table is an in-test constant, safe to cast to ::regclass.
	if err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM pg_attribute
			WHERE attrelid = ($1)::regclass AND attname = $2 AND NOT attisdropped
		)`, table, column,
	).Scan(&exists); err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}
	return exists
}

// indexExists reports whether a public-schema index of the given name exists.
func indexExists(t *testing.T, db *sql.DB, index string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, index,
	).Scan(&exists); err != nil {
		t.Fatalf("check index %s: %v", index, err)
	}
	return exists
}

// assertProjectionXidSchema verifies the US-03.05 schema: events.insert_xid and
// projection_progress.last_consumed_xid are both xid8 NOT NULL, the
// events_insert_xid_idx boundary index exists, and opengate_bypass holds exactly
// SELECT and UPDATE (not INSERT/DELETE) on projection_progress. Types are read from
// pg_catalog via format_type, which names xid8 exactly (information_schema would
// report a generic USER-DEFINED for it).
func assertProjectionXidSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	for _, c := range []struct {
		table, column string
	}{
		{"events", "insert_xid"},
		{"projection_progress", "last_consumed_xid"},
	} {
		var (
			typeName string
			notNull  bool
		)
		// The table name is an in-test constant, safe to interpolate into ::regclass.
		err := db.QueryRow(
			`SELECT format_type(a.atttypid, a.atttypmod), a.attnotnull
			 FROM pg_attribute a
			 WHERE a.attrelid = ($1)::regclass AND a.attname = $2 AND NOT a.attisdropped`,
			c.table, c.column,
		).Scan(&typeName, &notNull)
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s.%s column is missing", c.table, c.column)
		}
		if err != nil {
			t.Fatalf("query %s.%s type: %v", c.table, c.column, err)
		}
		if typeName != "xid8" {
			t.Errorf("%s.%s type = %q, want xid8", c.table, c.column, typeName)
		}
		if !notNull {
			t.Errorf("%s.%s is nullable, want NOT NULL", c.table, c.column)
		}
	}

	// The boundary index exists.
	var hasIndex bool
	if err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = 'public' AND tablename = 'events'
			  AND indexname = 'events_insert_xid_idx'
		)`,
	).Scan(&hasIndex); err != nil {
		t.Fatalf("query events_insert_xid_idx existence: %v", err)
	}
	if !hasIndex {
		t.Error("events_insert_xid_idx index is missing")
	}

	// opengate_bypass holds SELECT and UPDATE — and only those — on projection_progress.
	for _, c := range []struct {
		// query is a static, in-test constant, safe to pass to has_table_privilege.
		query string
		want  bool
	}{
		{`SELECT has_table_privilege('opengate_bypass', 'projection_progress', 'SELECT')`, true},
		{`SELECT has_table_privilege('opengate_bypass', 'projection_progress', 'UPDATE')`, true},
		{`SELECT has_table_privilege('opengate_bypass', 'projection_progress', 'INSERT')`, false},
		{`SELECT has_table_privilege('opengate_bypass', 'projection_progress', 'DELETE')`, false},
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

// assertIdempotencyKeysSchema verifies the US-03.06 commit 1 schema: each of the
// three idempotency cache tables carries tenant_isolation RLS (ENABLE + FORCE plus
// a policy named tenant_isolation), command_idempotency_keys carries the
// response_headers column the middleware replays from, and the command/cleanup
// grants are exactly those the later commits need. RLS state is read from pg_class
// (relrowsecurity/relforcerowsecurity), the policy from pg_policies, the column
// type from pg_catalog via format_type, and grants from has_table_privilege. All
// are catalog reads, independent of any row data.
//
// The grant probe also pins the negative space the prompt calls out: opengate_app
// has SELECT and INSERT but NOT UPDATE (the record statement is INSERT ... ON
// CONFLICT DO NOTHING, which needs no UPDATE) and NOT DELETE on
// command_idempotency_keys; opengate_bypass has DELETE on the command and decision
// tables but no app grant leaks onto the decision table (its app grants are E4).
func assertIdempotencyKeysSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	// Every idempotency table has RLS enabled, forced, and a tenant_isolation policy.
	for _, table := range []string{
		"command_idempotency_keys",
		"decision_idempotency_keys",
		"reconciliation_idempotency_keys",
	} {
		var (
			rowSecurity   bool
			forceSecurity bool
		)
		if err := db.QueryRow(
			`SELECT relrowsecurity, relforcerowsecurity
			 FROM pg_class WHERE oid = ($1)::regclass`, table,
		).Scan(&rowSecurity, &forceSecurity); err != nil {
			t.Fatalf("query RLS state for %s: %v", table, err)
		}
		if !rowSecurity {
			t.Errorf("%s does not have ROW LEVEL SECURITY enabled", table)
		}
		if !forceSecurity {
			t.Errorf("%s does not have FORCE ROW LEVEL SECURITY", table)
		}

		var hasPolicy bool
		if err := db.QueryRow(
			`SELECT EXISTS (
				SELECT 1 FROM pg_policies
				WHERE schemaname = 'public' AND tablename = $1 AND policyname = 'tenant_isolation'
			)`, table,
		).Scan(&hasPolicy); err != nil {
			t.Fatalf("query tenant_isolation policy for %s: %v", table, err)
		}
		if !hasPolicy {
			t.Errorf("%s is missing the tenant_isolation policy", table)
		}
	}

	// command_idempotency_keys.response_headers is jsonb NOT NULL DEFAULT '{}'. The
	// middleware serializes the whitelisted response headers into it, so a replay
	// reproduces the original's Content-Type/Location/ETag instead of letting
	// net/http sniff a content type from the replayed body. The default matters: it
	// makes the column safe for any writer that does not set it (and made the column
	// addable to the existing table without a backfill).
	//
	// Scope check: ONLY the command table has it. The decision path is an
	// in-transaction use-case concern (E4) storing decision/reason_code/response_body,
	// not an HTTP response cache, so a response_headers column appearing there would
	// be a scope error, not a convenience.
	var (
		headersType    string
		headersNotNull bool
		headersDefault *string
	)
	if err := db.QueryRow(
		`SELECT format_type(a.atttypid, a.atttypmod), a.attnotnull,
		        pg_get_expr(d.adbin, d.adrelid)
		 FROM pg_attribute a
		 LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE a.attrelid = ('command_idempotency_keys')::regclass
		   AND a.attname = 'response_headers' AND NOT a.attisdropped`,
	).Scan(&headersType, &headersNotNull, &headersDefault); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatal("command_idempotency_keys.response_headers column is missing")
		}
		t.Fatalf("query command_idempotency_keys.response_headers: %v", err)
	}
	if headersType != "jsonb" {
		t.Errorf("command_idempotency_keys.response_headers type = %q, want jsonb", headersType)
	}
	if !headersNotNull {
		t.Error("command_idempotency_keys.response_headers is nullable, want NOT NULL")
	}
	if headersDefault == nil || *headersDefault != `'{}'::jsonb` {
		got := "<none>"
		if headersDefault != nil {
			got = *headersDefault
		}
		t.Errorf("command_idempotency_keys.response_headers default = %s, want '{}'::jsonb", got)
	}
	if columnExists(t, db, "decision_idempotency_keys", "response_headers") {
		t.Error("decision_idempotency_keys has a response_headers column; the header cache is the command path only")
	}

	// The exact grant set. opengate_app: SELECT, INSERT on command (no UPDATE/DELETE);
	// opengate_bypass: DELETE on command and decision (and nothing more leaked onto
	// the decision table's app role).
	//
	// Re-probed after response_headers was added: the grant is table-level, so a new
	// column needs no grant edit — this pins that the grant set did not shift.
	for _, c := range []struct {
		// query is a static, in-test constant (no user input), safe to pass to
		// has_table_privilege as a literal.
		query string
		want  bool
	}{
		{`SELECT has_table_privilege('opengate_app', 'command_idempotency_keys', 'SELECT')`, true},
		{`SELECT has_table_privilege('opengate_app', 'command_idempotency_keys', 'INSERT')`, true},
		{`SELECT has_table_privilege('opengate_app', 'command_idempotency_keys', 'UPDATE')`, false},
		{`SELECT has_table_privilege('opengate_app', 'command_idempotency_keys', 'DELETE')`, false},
		{`SELECT has_table_privilege('opengate_bypass', 'command_idempotency_keys', 'DELETE')`, true},
		{`SELECT has_table_privilege('opengate_bypass', 'decision_idempotency_keys', 'DELETE')`, true},
		// Deferred grants: the decision table's app access is E4, so opengate_app
		// must hold nothing on it yet.
		{`SELECT has_table_privilege('opengate_app', 'decision_idempotency_keys', 'SELECT')`, false},
		{`SELECT has_table_privilege('opengate_app', 'decision_idempotency_keys', 'INSERT')`, false},

		// US-03.06 commit 3 (20260615091300): the cleanup job's DELETE reads created_at
		// in its qualifier, and Postgres checks SELECT per column on read columns — so
		// DELETE alone made the purge fail with "permission denied". The corrective
		// grant is COLUMN-level: created_at is readable, the table as a whole is not,
		// so the worker role can find expired rows but cannot read the cached response
		// bodies they hold. The false table-level probes below are the least-privilege
		// half of that pair, not an oversight.
		{`SELECT has_column_privilege('opengate_bypass', 'command_idempotency_keys', 'created_at', 'SELECT')`, true},
		{`SELECT has_column_privilege('opengate_bypass', 'decision_idempotency_keys', 'created_at', 'SELECT')`, true},
		{`SELECT has_column_privilege('opengate_bypass', 'command_idempotency_keys', 'response_body', 'SELECT')`, false},
		{`SELECT has_column_privilege('opengate_bypass', 'decision_idempotency_keys', 'response_body', 'SELECT')`, false},
		{`SELECT has_table_privilege('opengate_bypass', 'command_idempotency_keys', 'SELECT')`, false},
		{`SELECT has_table_privilege('opengate_bypass', 'decision_idempotency_keys', 'SELECT')`, false},

		// reconciliation_idempotency_keys is never purged, so the worker role holds
		// nothing on it — the grant that makes a mistaken purge fail loudly.
		{`SELECT has_table_privilege('opengate_bypass', 'reconciliation_idempotency_keys', 'DELETE')`, false},
		{`SELECT has_column_privilege('opengate_bypass', 'reconciliation_idempotency_keys', 'created_at', 'SELECT')`, false},
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

// assertCasbinPolicySeeded verifies the create_casbin_rules migration seeded the
// v1 authorization policy that step 3's enforcer will rest on. The spot-checks
// are the meaningful part: they assert specific (role, resource, action)
// semantics, not merely that some rows exist. casbin_rules is a global table (no
// tenant_id, no RLS), so the superuser test connection reads every row directly.
func assertCasbinPolicySeeded(t *testing.T, db *sql.DB) {
	t.Helper()

	// Each case is a presence/absence check on a policy ('p') row matched on
	// v0=role, v1=resource (object), v2=action -- the four semantics step 3 will
	// enforce: the owner wildcard grants everything; a manager may write members;
	// an auditor may read members but must NOT be able to write them.
	for _, c := range []struct {
		role, resource, action string
		want                   bool
	}{
		{"owner", "*", "*", true},              // owner wildcard -> full access
		{"manager", "members", "write", true},  // manager may write members
		{"auditor", "members", "read", true},   // auditor may read members
		{"auditor", "members", "write", false}, // auditor must NOT write members
	} {
		var exists bool
		err := db.QueryRow(
			`SELECT EXISTS (
				SELECT 1 FROM casbin_rules
				WHERE ptype = 'p' AND v0 = $1 AND v1 = $2 AND v2 = $3
			)`, c.role, c.resource, c.action,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("query casbin rule (%s, %s, %s): %v", c.role, c.resource, c.action, err)
		}
		if exists != c.want {
			t.Errorf("casbin rule (p, %s, %s, %s) present = %v, want %v",
				c.role, c.resource, c.action, exists, c.want)
		}
	}

	// The v1 seed is fixed at seventeen 'p' rows. The count alone is not the point
	// (a wrong rule swapped for a right one would keep it), but it cheaply catches
	// a rule accidentally added to or dropped from a seed that is meant to be fixed.
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM casbin_rules WHERE ptype = 'p'`).Scan(&count); err != nil {
		t.Fatalf("count casbin 'p' rules: %v", err)
	}
	if want := 17; count != want {
		t.Errorf("casbin 'p' rule count = %d, want %d", count, want)
	}
}

// assertSchemaPresent asserts the presence (want=true) or absence (want=false)
// of the tenants, users, sessions, and casbin_rules tables and the
// opengate_bypass role together, so the round-trip verifies the full surface the
// migrations create and tear down. Including casbin_rules here is what proves the
// create_casbin_rules Down leaves no table behind after a full DownTo(0).
func assertSchemaPresent(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	assertTableExists(t, db, "tenants", want)
	assertTableExists(t, db, "users", want)
	assertTableExists(t, db, "sessions", want)
	assertTableExists(t, db, "casbin_rules", want)
	assertTableExists(t, db, "command_idempotency_keys", want)
	assertTableExists(t, db, "decision_idempotency_keys", want)
	assertTableExists(t, db, "reconciliation_idempotency_keys", want)
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
