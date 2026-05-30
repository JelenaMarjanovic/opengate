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

	return dispatchMigrate(ctx, db, action)
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
