// Command opengate is the OpenGate access-control SaaS executable.
//
// The binary exposes operational modes through subcommands. Currently
// "migrate", "bootstrap", "api", and "worker" are implemented; simulator
// arrives in a later epic. Running without a subcommand is a usage error.
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

	// Install the global W3C trace-context propagator once at startup. It defines
	// how trace context serializes into and out of carriers — e.g. River job
	// metadata on enqueue (US-03.04) — so the worker can continue the trace.
	// Without it the global propagator is a no-op that drops context. This is the
	// OTel API only; span production/export (the SDK) is wired separately.
	observability.SetGlobalTracePropagator()

	ctx := context.Background()
	name := os.Args[1]
	logger.InfoContext(ctx, "opengate starting", slog.String("subcommand", name))

	switch name {
	case "migrate":
		if err := runMigrate(ctx, logger, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "opengate:", err)
			os.Exit(1)
		}
	case "bootstrap":
		if err := runBootstrap(ctx, logger, cfg, os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, "opengate:", err)
			os.Exit(1)
		}
	case "api":
		if err := runAPI(ctx, logger, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "opengate:", err)
			os.Exit(1)
		}
	case "worker":
		if err := runWorker(ctx, logger, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "opengate:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "opengate: unknown subcommand %q\n", name)
		os.Exit(2)
	}
}
