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

	// Phase 1: goose -- the application schema and its grants. Unchanged.
	if err := dispatchMigrate(ctx, db, action); err != nil {
		return err
	}

	// River phase (R1 phases 2-4) runs only on `up`: it brings the dedicated
	// `river` schema to the same state goose just brought the app schema to.
	// down/status/version stay goose-only in Step 1 -- River teardown and status
	// are not in this step's scope.
	if action == "up" {
		if err := riverPhase(ctx, logger, dsn); err != nil {
			return err
		}
	}

	return nil
}

// riverPhase runs the River-schema migration phase (CREATE SCHEMA, rivermigrate
// up, grants) over a pgx pool built from the migration DSN. rivermigrate needs a
// pgxpool.Pool (the goose path uses a database/sql handle, which River's pgx
// driver cannot use), so the pool is opened here, scoped to this phase, and
// closed before returning.
func riverPhase(ctx context.Context, logger *slog.Logger, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("migrate: open river pool: %w", err)
	}
	defer pool.Close()

	if err := queue.MigrateRiver(ctx, pool, logger); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// dispatchMigrate constructs a goose Provider over the embedded
// migrations and runs the requested action. The Provider API is used
// (rather than goose's global functions) so that no process-global state
// is mutated; this keeps the function safe to call from tests in parallel.
func dispatchMigrate(ctx context.Context, db *sql.DB, action string) error {
	// fs.Sub narrows the embedded FS to the migrations subdirectory so
	// the Provider sees the .sql files at the root of the FS it is given.
	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("migrate: sub filesystem: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		return fmt.Errorf("migrate: new provider: %w", err)
	}

	switch action {
	case "up":
		results, err := provider.Up(ctx)
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		for _, r := range results {
			fmt.Printf("OK  %s (%s)\n", r.Source.Path, r.Duration)
		}
		return nil
	case "down":
		result, err := provider.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		fmt.Printf("OK  rolled back %s\n", result.Source.Path)
		return nil
	case "status":
		status, err := provider.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		for _, s := range status {
			fmt.Printf("%-10s %s\n", s.State, s.Source.Path)
		}
		return nil
	case "version":
		v, err := provider.GetDBVersion(ctx)
		if err != nil {
			return fmt.Errorf("migrate version: %w", err)
		}
		fmt.Printf("current version: %d\n", v)
		return nil
	default:
		return fmt.Errorf("migrate: unknown action %q", action)
	}
}
