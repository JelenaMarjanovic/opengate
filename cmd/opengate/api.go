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
	appauth "github.com/JelenaMarjanovic/opengate/internal/application/auth"
	"github.com/JelenaMarjanovic/opengate/internal/auth"
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

// runAPI implements `opengate api`: it serves the full US-02.03 HTTP surface —
// the health probes plus the authenticated login/logout/whoami chain — until a
// SIGTERM/SIGINT triggers a graceful shutdown. It is the composition root (System
// Design §7): the only place that imports both the outbound adapters and the
// application layer, assembling the Authenticator from ports and injected
// collaborators and handing it to the inbound HTTP adapter.
//
// Two pools (System Design §10):
//   - The BYPASS pool backs the readiness probe and every pre-authentication
//     lookup (tenant resolve, user read/write, session create + by-token find). It
//     installs no tenant-binding hooks, so the tenant-less readiness ping does not
//     spam a "no tenant in context" warning.
//   - The regular RLS-bound pool (postgres.NewPool) backs the post-authentication
//     writes (session refresh + delete). Its acquire hook binds the tenant the
//     session middleware set on the request context. It is acquired ONLY on those
//     tenant-scoped paths, never for readiness — so the warning above never fires
//     in normal operation.
//
// Both pools are closed as the final shutdown phase (see serveAPI), or eagerly
// below if a later construction step fails, so neither is leaked on any path.
func runAPI(ctx context.Context, logger *slog.Logger, cfg config.Config) error {
	// Validate both DSNs before acquiring any resource. They are optional in
	// config (migrate must not require them), so the api subcommand validates the
	// pair it needs here, mirroring bootstrap.
	if cfg.BypassRLSURL == "" {
		return errors.New("api: BYPASS_RLS_DATABASE_URL is not set")
	}
	if cfg.DatabaseURL == "" {
		return errors.New("api: DATABASE_URL is not set")
	}

	bypass, err := postgres.NewBypassPool(ctx, cfg.BypassRLSURL)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	// NOTE: the pools are closed by serveAPI as the final shutdown phase (so the
	// closes are logged in order), or eagerly on the error paths below. They are
	// deliberately NOT deferred here, to avoid a double Close on the happy path.

	regular, err := postgres.NewPool(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		bypass.Close()
		return fmt.Errorf("api: %w", err)
	}

	// Composition root: assemble the Authenticator from the outbound adapters and
	// the injected collaborators. VerifyPassword/HashPassword/MustDummyHash come
	// from internal/auth (crypto); VerifierFunc/HasherFunc/CryptoRandToken from the
	// application layer. time.Now is the production clock.
	authenticator := appauth.NewAuthenticator(
		postgres.NewTenantResolver(bypass),
		postgres.NewUserReader(bypass),
		postgres.NewUserWriter(bypass),
		postgres.NewSessionStore(bypass, regular),
		appauth.VerifierFunc(auth.VerifyPassword),
		appauth.HasherFunc(auth.HashPassword),
		time.Now,
		appauth.CryptoRandToken,
		auth.MustDummyHash(),
		logger,
	)

	router := httpadapter.NewRouter(httpadapter.Config{
		Pinger:        bypass,
		Authenticator: authenticator,
		CookieSecure:  cfg.CookieSecure,
	})

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
		regular.Close()
		bypass.Close()
		return fmt.Errorf("api: listen on %s: %w", cfg.HTTPAddr, err)
	}

	// signal.NotifyContext cancels the returned context on SIGTERM/SIGINT. All
	// shutdown phases hang off this cancellation, which keeps the shutdown logic
	// testable without sending real signals (System Design §21, phase one).
	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.InfoContext(ctx, "api: http server listening", slog.String("addr", ln.Addr().String()))
	// Close order at shutdown: regular (request) pool first, then bypass.
	return serveAPI(signalCtx, logger, srv, ln, regular, bypass)
}

// serveAPI runs srv on ln until shutdownCtx is canceled (in production, by a
// signal), then performs the minimal §21 shutdown subset for this step: drain
// the HTTP server with a bounded timeout, then close the pools (in the order
// given). The full multi-phase sequence (River worker drain, SSE drain, OTel
// flush, readiness flip) accretes as those components land.
//
// pools is variadic so the api command can pass BOTH the regular and the bypass
// pool while the lifecycle tests pass a single spy poolCloser. Each is closed
// exactly once, in argument order, as the final phase.
//
// It is split from runAPI so a test can drive shutdown by canceling shutdownCtx
// and inject a spy poolCloser. shutdownCtx is the cancellation trigger, not a
// deadline; the drain deadline is a fresh background-derived context so it is not
// already-canceled when shutdown begins.
func serveAPI(shutdownCtx context.Context, logger *slog.Logger, srv *http.Server, ln net.Listener, pools ...poolCloser) error {
	closeAll := func() {
		for _, p := range pools {
			p.Close()
		}
	}

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
		// Close the pools and surface the error so the process exits non-zero.
		closeAll()
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
	logger.Info("api: closing database pools")
	closeAll()
	logger.Info("api: shutdown complete")
	return nil
}

// Compile-time assertion that the concrete pool satisfies the shutdown seam, so
// a signature drift in pgxpool is caught at build time, not in the test.
var _ poolCloser = (*pgxpool.Pool)(nil)
