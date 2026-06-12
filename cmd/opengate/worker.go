package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/queue"
	"github.com/JelenaMarjanovic/opengate/internal/config"
)

// runWorker implements `opengate worker`: it constructs and runs the River worker
// pool until a SIGTERM/SIGINT triggers a graceful drain. It is a composition root
// like runAPI — the only place that opens the worker's pool and assembles the
// pool from the queue adapter.
//
// The worker runs as opengate_bypass (the BYPASSRLS pool): it fetches, works, and
// completes jobs across tenants, outside any single tenant's RLS scope. It needs
// ONLY that pool — no regular RLS pool, no DATABASE_URL — so it validates and
// opens just BYPASS_RLS_DATABASE_URL.
//
// This is the pool FOUNDATION (Step 3): NewWorkerPool registers no workers, so the
// pool polls the default queue but processes nothing until a later story adds a
// real job kind and its worker. The full enqueue->process path is already granted
// (Steps 1-2); nothing is added here.
func runWorker(ctx context.Context, logger *slog.Logger, cfg config.Config) error {
	// Validate the one DSN the worker needs before acquiring any resource, mirroring
	// runAPI's validate-before-acquire. The DSNs are optional in config (migrate must
	// not require them), so the subcommand checks the one it uses.
	if cfg.BypassRLSURL == "" {
		return errors.New("worker: BYPASS_RLS_DATABASE_URL is not set")
	}

	bypass, err := postgres.NewBypassPool(ctx, cfg.BypassRLSURL)
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	// The pool is closed by serveWorker as the final shutdown phase, or eagerly on
	// the construction error path below — never leaked, never double-closed.

	pool, err := queue.NewWorkerPool(bypass, logger)
	if err != nil {
		bypass.Close()
		return fmt.Errorf("worker: %w", err)
	}

	// signal.NotifyContext cancels signalCtx on SIGTERM/SIGINT — the same shutdown
	// trigger the api subcommand uses, so both subcommands drain on one idiom and
	// the sequence stays testable without sending real signals.
	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return serveWorker(ctx, signalCtx, logger, pool, bypass)
}

// serveWorker starts the pool and blocks until shutdownCtx is canceled (in
// production, by a signal), then runs the graceful drain (decision A3, inside
// WorkerPool.Stop) and closes the pools as the final phase.
//
// startCtx is the pool's run context — deliberately NOT shutdownCtx: River would
// otherwise initiate its own soft stop when the signal fires, taking over the
// escalation timing. Driving Start from a non-signal context keeps shutdown
// orchestration explicit in Stop.
//
// pools is variadic and each is closed exactly once in argument order, mirroring
// serveAPI so a future test can drive shutdown with a spy poolCloser.
func serveWorker(startCtx, shutdownCtx context.Context, logger *slog.Logger, pool *queue.WorkerPool, pools ...poolCloser) error {
	closeAll := func() {
		for _, p := range pools {
			p.Close()
		}
	}

	if err := pool.Start(startCtx); err != nil {
		closeAll()
		return fmt.Errorf("worker: start: %w", err)
	}
	logger.Info("worker: pool started, polling default queue")

	<-shutdownCtx.Done()
	logger.Info("worker: shutdown signal received, draining in-flight jobs")

	// Phase: graceful drain (Stop, then StopAndCancel fallback).
	pool.Stop()

	// Phase: database pool close. By now the pool has stopped working jobs.
	logger.Info("worker: closing database pool")
	closeAll()
	logger.Info("worker: shutdown complete")
	return nil
}
