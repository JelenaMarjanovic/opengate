package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/queue"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

// runMigrate implements the `opengate migrate <action>` subcommand.
// Supported actions mirror a subset of goose's surface: up, down, status,
// version. The DSN is read from OPENGATE_DATABASE_URL. The logger is injected
// from the composition root so failures surface as structured records.
func runMigrate(ctx context.Context, logger *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: opengate migrate <up|down|status|version>")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}

	action := flags.Arg(0)
	if action == "" {
		flags.Usage()
		return errors.New("migrate: no action specified")
	}

	// Validate action against the known set before establishing any
	// database connection. Catching argument errors here means an invalid
	// action does not require a working Postgres to report itself.
	switch action {
	case "up", "down", "status", "version":
		// valid
	default:
		return fmt.Errorf("migrate: unknown action %q", action)
	}

	dsn := os.Getenv("OPENGATE_DATABASE_URL")
	if dsn == "" {
		return errors.New("migrate: OPENGATE_DATABASE_URL is not set")
	}

	// Open a database/sql handle backed by the pgx stdlib driver. The
	// driver name "pgx" is registered by the blank import above.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrate: open database: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			logger.ErrorContext(ctx, "migrate: close database", slog.Any("error", cerr))
		}
	}()

	// Verify connectivity early with a short timeout so a bad DSN fails
	// fast with a clear error rather than hanging on the first query.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("migrate: ping database: %w", err)
	}

	// Phase 1: goose -- the application schema and its grants. gooseVersion is the
	// resulting DB version, used below to gate the River teardown on `down`.
	gooseVersion, err := dispatchMigrate(ctx, db, action)
	if err != nil {
		return err
	}

	// River phase: River mirrors goose per action over a pgx pool (decisions D1,
	// D2). The pool is opened per action because rivermigrate cannot use the
	// database/sql handle the goose phase uses.
	switch action {
	case "up":
		// Bring the river schema up to the state goose just brought the app schema.
		return withRiverPool(ctx, dsn, func(pool *pgxpool.Pool) error {
			return queue.MigrateRiver(ctx, pool, logger)
		})
	case "down":
		// D2: tear River down ONLY when the app schema reached version 0. goose
		// `down` rolls back a single migration, so a non-zero post-down version is
		// a partial teardown and must leave River intact.
		if gooseVersion != 0 {
			return nil
		}
		return withRiverPool(ctx, dsn, func(pool *pgxpool.Pool) error {
			return queue.TeardownRiver(ctx, pool, logger)
		})
	case "status":
		return withRiverPool(ctx, dsn, func(pool *pgxpool.Pool) error {
			return printRiverStatus(ctx, pool)
		})
	case "version":
		return withRiverPool(ctx, dsn, func(pool *pgxpool.Pool) error {
			return printRiverVersion(ctx, pool)
		})
	}

	return nil
}

// withRiverPool opens a pgx pool from the migration DSN (the goose phase uses a
// database/sql handle, which rivermigrate cannot consume), runs fn against it, and
// closes the pool. fn's error is wrapped to match the subcommand's other failures.
func withRiverPool(ctx context.Context, dsn string, fn func(*pgxpool.Pool) error) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("migrate: open river pool: %w", err)
	}
	defer pool.Close()

	if err := fn(pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// printRiverStatus reports River's migration state after the goose status, in a
// clearly labeled section (decision D1). An absent `river` schema is reported as
// such, not as an error.
func printRiverStatus(ctx context.Context, pool *pgxpool.Pool) error {
	st, err := queue.RiverStatus(ctx, pool)
	if err != nil {
		return err
	}

	fmt.Println("river:")
	switch {
	case !st.SchemaPresent:
		fmt.Printf("not present (0/%d migrations applied)\n", st.TotalCount)
	case len(st.Pending) > 0:
		fmt.Printf("%d/%d migrations applied; pending: %s\n",
			st.AppliedCount, st.TotalCount, joinVersions(st.Pending))
	default:
		fmt.Printf("%d/%d migrations applied\n", st.AppliedCount, st.TotalCount)
	}
	return nil
}

// printRiverVersion reports River's current migration version after the goose
// version (decision D1). It is 0 when River is absent.
func printRiverVersion(ctx context.Context, pool *pgxpool.Pool) error {
	st, err := queue.RiverStatus(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Printf("river version: %d\n", st.AppliedVersion)
	return nil
}

// joinVersions renders migration versions as a comma-separated list, e.g. "5, 6".
func joinVersions(versions []int) string {
	parts := make([]string, len(versions))
	for i, v := range versions {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

// dispatchMigrate constructs a goose Provider over the embedded migrations and
// runs the requested action, printing goose's output under a clear engine label
// (decision D1). It returns the resulting goose DB version for `down` (used by the
// caller to gate the River teardown on version 0, decision D2); the other actions
// return 0, which they do not need. The Provider API is used (rather than goose's
// global functions) so no process-global state is mutated, keeping the function
// safe to call from tests in parallel.
func dispatchMigrate(ctx context.Context, db *sql.DB, action string) (int64, error) {
	provider, err := newGooseProvider(db)
	if err != nil {
		return 0, err
	}

	switch action {
	case "up":
		results, err := provider.Up(ctx)
		if err != nil {
			return 0, fmt.Errorf("migrate up: %w", err)
		}
		for _, r := range results {
			fmt.Printf("OK  %s (%s)\n", r.Source.Path, r.Duration)
		}
		return 0, nil
	case "down":
		result, err := provider.Down(ctx)
		if err != nil {
			return 0, fmt.Errorf("migrate down: %w", err)
		}
		fmt.Printf("OK  rolled back %s\n", result.Source.Path)
		// Report the post-rollback version so the caller can decide whether the app
		// schema reached version 0 and River should be torn down (decision D2).
		v, err := provider.GetDBVersion(ctx)
		if err != nil {
			return 0, fmt.Errorf("migrate: db version: %w", err)
		}
		return v, nil
	case "status":
		fmt.Println("goose:")
		status, err := provider.Status(ctx)
		if err != nil {
			return 0, fmt.Errorf("migrate status: %w", err)
		}
		for _, s := range status {
			fmt.Printf("%-10s %s\n", s.State, s.Source.Path)
		}
		return 0, nil
	case "version":
		v, err := provider.GetDBVersion(ctx)
		if err != nil {
			return 0, fmt.Errorf("migrate version: %w", err)
		}
		fmt.Printf("goose version: %d\n", v)
		return v, nil
	default:
		return 0, fmt.Errorf("migrate: unknown action %q", action)
	}
}

// newGooseProvider builds a goose Provider over the embedded application
// migrations. fs.Sub narrows the embedded FS to the migrations subdirectory so the
// Provider sees the .sql files at the root of the FS it is given.
func newGooseProvider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: sub filesystem: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		return nil, fmt.Errorf("migrate: new provider: %w", err)
	}
	return provider, nil
}
