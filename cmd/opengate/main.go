// Command opengate is the OpenGate access-control SaaS executable.
//
// The binary exposes operational modes through subcommands. Currently
// only "migrate" is implemented; api, worker, simulator, and bootstrap
// arrive in later epics. Running without a subcommand is a usage error.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/JelenaMarjanovic/opengate/internal/config"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "opengate: no subcommand specified")
		os.Exit(2)
	}

	// Load configuration before anything else; a parse failure is a
	// programmer/operator error, so fail fast with a clear message.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "opengate:", err)
		os.Exit(1)
	}

	// Hybrid logger delivery (D-A): build a configured logger and inject it
	// as the primary mechanism, and also register it as the slog default so
	// stray library logs flow through the same JSON pipeline.
	logger := observability.NewLogger(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	ctx := context.Background()
	name := os.Args[1]
	logger.InfoContext(ctx, "opengate starting", slog.String("subcommand", name))

	switch name {
	case "migrate":
		if err := runMigrate(ctx, logger, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "opengate:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "opengate: unknown subcommand %q\n", name)
		os.Exit(2)
	}
}
