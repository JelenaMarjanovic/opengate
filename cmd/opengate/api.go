package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	httpadapter "github.com/JelenaMarjanovic/opengate/internal/adapters/inbound/http"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/config"
)

// apiShutdownTimeout bounds the graceful HTTP drain (System Design §21). New
// connections are refused immediately; in-flight requests have this long to
// finish before they are cut short (which logs a warning).
const apiShutdownTimeout = 30 * time.Second

// HTTP server hardening timeouts. These bound slow/abusive clients (e.g. the
// Slowloris attack the ReadHeaderTimeout defends against) and keep idle
// connections from accumulating. They are generous enough for the dashboard's
// real requests; long-running operations (future exports) belong on the worker,
// not on a synchronous HTTP request.
const (
	apiReadHeaderTimeout = 10 * time.Second
	apiReadTimeout       = 30 * time.Second
	apiWriteTimeout      = 30 * time.Second
	apiIdleTimeout       = 120 * time.Second
)

// poolCloser is the narrow shutdown capability serveAPI needs from the database
// pool: Close, called as the final shutdown phase. *pgxpool.Pool satisfies it.
// Depending on the seam (not the concrete pool) lets the graceful-shutdown test
// assert the pool is closed with a spy and no live database.
type poolCloser interface {
	Close()
}

// runAPI implements `opengate api`: it serves the HTTP surface (this step: the
// health endpoints) until a SIGTERM/SIGINT triggers a graceful shutdown.
//
// Pool note (US-02.03 Step 5a): this step builds ONLY the bypass pool and uses
// it for the readiness probe. The api command will ultimately need both pools —
// the bypass pool for pre-auth lookups and the regular RLS-bound pool
// (postgres.NewPool) for post-auth refresh/delete — but the regular pool is
// deferred to Step 5b for two reasons. First, this step has no consumer for it,
// and a constructed-but-unused pool is dead code the linter rejects. Second, and
// more importantly, the regular pool's acquire hook logs a "no tenant in
// context" warning on every tenant-less acquire; a readiness probe has no tenant,
// so routing it through the regular pool would spam that warning every few
// seconds. Readiness therefore pings the bypass pool, which installs no hooks.
// General principle for this command: the regular pool is acquired only on
// tenant-scoped post-auth paths where the session middleware has set the tenant
// context; tenant-less operations (readiness, future operator paths) use the
// bypass pool. The regular pool joins the composition root in Step 5b when the
// Authenticator (and its DATABASE_URL config) lands.
func runAPI(ctx context.Context, logger *slog.Logger, cfg config.Config) error {
	// BypassRLSURL is optional in config (migrate must not require it), so the
	// subcommand that needs it validates its presence here, mirroring bootstrap.
	if cfg.BypassRLSURL == "" {
		return errors.New("api: BYPASS_RLS_DATABASE_URL is not set")
	}

	pool, err := postgres.NewBypassPool(ctx, cfg.BypassRLSURL)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	// NOTE: the pool is closed by serveAPI as the final shutdown phase (so the
	// close is logged in order), or below if binding the listener fails. It is
	// deliberately NOT deferred here, to avoid a double Close.

	router := httpadapter.NewRouter(pool)

	srv := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: apiReadHeaderTimeout,
		ReadTimeout:       apiReadTimeout,
		WriteTimeout:      apiWriteTimeout,
		IdleTimeout:       apiIdleTimeout,
	}

	// Bind the listener up front so a bind failure (e.g. address in use) surfaces
	// immediately and synchronously, rather than after entering the serve
	// goroutine. It also exposes the resolved address (useful when :0 is used in
	// tests). srv.Serve(ln) — not ListenAndServe — consumes this listener.
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		pool.Close()
		return fmt.Errorf("api: listen on %s: %w", cfg.HTTPAddr, err)
	}

	// signal.NotifyContext cancels the returned context on SIGTERM/SIGINT. All
	// shutdown phases hang off this cancellation, which keeps the shutdown logic
	// testable without sending real signals (System Design §21, phase one).
	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.InfoContext(ctx, "api: http server listening", slog.String("addr", ln.Addr().String()))
	return serveAPI(signalCtx, logger, srv, ln, pool)
}

// serveAPI runs srv on ln until shutdownCtx is canceled (in production, by a
// signal), then performs the minimal §21 shutdown subset for this step: drain
// the HTTP server with a bounded timeout, then close the pool. The full
// multi-phase sequence (River worker drain, SSE drain, OTel flush, readiness
// flip) accretes as those components land.
//
// It is split from runAPI so a test can drive shutdown by canceling shutdownCtx
// and inject a spy poolCloser. shutdownCtx is the cancellation trigger, not a
// deadline; the drain deadline is a fresh background-derived context so it is not
// already-canceled when shutdown begins.
func serveAPI(shutdownCtx context.Context, logger *slog.Logger, srv *http.Server, ln net.Listener, pool poolCloser) error {
	// Buffered so the serve goroutine never blocks on send even if we have already
	// taken the shutdown branch of the select.
	serverErr := make(chan error, 1)
	go func() {
		// Serve returns ErrServerClosed on a normal Shutdown; that is not a failure.
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		// A serve failure with no shutdown in progress (e.g. the listener died).
		// Close the pool and surface the error so the process exits non-zero.
		pool.Close()
		return fmt.Errorf("api: serve: %w", err)
	case <-shutdownCtx.Done():
		logger.Info("api: shutdown signal received, draining in-flight requests")
	}

	// Phase: HTTP server drain. Shutdown stops accepting new connections and waits
	// for in-flight requests up to the timeout. The deadline rides on a fresh
	// context derived from Background — shutdownCtx is already canceled.
	drainCtx, cancel := context.WithTimeout(context.Background(), apiShutdownTimeout)
	defer cancel()

	switch err := srv.Shutdown(drainCtx); {
	case err == nil:
		logger.Info("api: http server drained cleanly")
	case errors.Is(err, context.DeadlineExceeded):
		logger.Warn("api: shutdown deadline exceeded; cutting in-flight requests",
			slog.Duration("timeout", apiShutdownTimeout))
	default:
		logger.Error("api: http server shutdown error", slog.String("error", err.Error()))
	}

	// Phase: database pool close. By now no checkouts are in progress.
	logger.Info("api: closing database pool")
	pool.Close()
	logger.Info("api: shutdown complete")
	return nil
}

// Compile-time assertion that the concrete pool satisfies the shutdown seam, so
// a signature drift in pgxpool is caught at build time, not in the test.
var _ poolCloser = (*pgxpool.Pool)(nil)
