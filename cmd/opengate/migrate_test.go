package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestMigrateRiverDownTeardownWiring exercises the `migrate down` wiring end to end
// through runMigrate (decision D2): every PARTIAL down (goose still above version
// 0) must leave the river schema intact, and only the down that lands the app
// schema on version 0 may tear River down. goose `down` is one step, so the test
// drives it down one migration at a time and checks the invariant at each step.
func TestMigrateRiverDownTeardownWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := testsupport.StartPostgres(ctx, t)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	t.Setenv("OPENGATE_DATABASE_URL", dsn)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := openMigrateDB(ctx, t, dsn)

	// Full up: goose + River.
	if err := runMigrate(ctx, logger, []string{"up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !riverSchemaPresent(ctx, t, db) {
		t.Fatal("river schema absent after migrate up")
	}

	// Drive the app schema down one migration at a time through the CLI. The first
	// iteration is the canonical partial down (test #4); the loop then confirms the
	// invariant holds for every step until version 0 triggers the teardown.
	const maxDowns = 100 // safety cap, far above the migration count
	reachedZero := false
	for i := 0; i < maxDowns; i++ {
		if err := runMigrate(ctx, logger, []string{"down"}); err != nil {
			t.Fatalf("migrate down (step %d): %v", i, err)
		}
		v := currentGooseVersion(ctx, t, db)
		if v == 0 {
			reachedZero = true
			break
		}
		// Still a partial teardown -- River must remain untouched (decision D2).
		if !riverSchemaPresent(ctx, t, db) {
			t.Fatalf("river schema torn down on a partial down (goose version %d)", v)
		}
	}

	if !reachedZero {
		t.Fatalf("goose did not reach version 0 within %d downs", maxDowns)
	}
	// Reaching version 0 triggers the River teardown (decision D2).
	if riverSchemaPresent(ctx, t, db) {
		t.Fatal("river schema still present after a full down to version 0")
	}
}

// TestMigrateRiverStatusVersionOutput asserts the CLI prints a clearly labeled
// River section for both `status` and `version` after a full up (decision D1), by
// capturing stdout.
func TestMigrateRiverStatusVersionOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := testsupport.StartPostgres(ctx, t)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	t.Setenv("OPENGATE_DATABASE_URL", dsn)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runMigrate(ctx, logger, []string{"up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// status: both engines reported, clearly labeled; River fully applied after up.
	statusOut := captureStdout(t, func() {
		if err := runMigrate(ctx, logger, []string{"status"}); err != nil {
			t.Fatalf("migrate status: %v", err)
		}
	})
	for _, want := range []string{"goose:", "river:", "migrations applied"} {
		if !strings.Contains(statusOut, want) {
			t.Errorf("status output missing %q\n--- output ---\n%s", want, statusOut)
		}
	}

	// version: a labeled line per engine.
	versionOut := captureStdout(t, func() {
		if err := runMigrate(ctx, logger, []string{"version"}); err != nil {
			t.Fatalf("migrate version: %v", err)
		}
	})
	for _, want := range []string{"goose version:", "river version:"} {
		if !strings.Contains(versionOut, want) {
			t.Errorf("version output missing %q\n--- output ---\n%s", want, versionOut)
		}
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns what was
// written. The migrate subcommand prints status/version via fmt, so the CLI-level
// output is asserted by capturing it here. Not safe under t.Parallel(); these
// container-backed tests run sequentially.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out)
}

// currentGooseVersion returns the current goose DB version, reusing the
// subcommand's own provider constructor so the test reads the version exactly as
// the wiring does.
func currentGooseVersion(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	provider, err := newGooseProvider(db)
	if err != nil {
		t.Fatalf("new goose provider: %v", err)
	}
	v, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatalf("goose db version: %v", err)
	}
	return v
}

// riverSchemaPresent reports whether the dedicated `river` schema exists.
func riverSchemaPresent(ctx context.Context, t *testing.T, db *sql.DB) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = 'river')`,
	).Scan(&exists); err != nil {
		t.Fatalf("check river schema: %v", err)
	}
	return exists
}

// openMigrateDB opens a database/sql handle over the pgx stdlib driver (registered
// by the blank import in migrate.go), registered for cleanup.
func openMigrateDB(_ context.Context, t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
