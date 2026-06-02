package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	httpadapter "github.com/JelenaMarjanovic/opengate/internal/adapters/inbound/http"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/config"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
)

// spyPool is a poolCloser that records how many times Close was called, so the
// graceful-shutdown test can assert the pool is closed exactly once without a
// live database.
type spyPool struct {
	mu     sync.Mutex
	closed int
}

func (s *spyPool) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
}

func (s *spyPool) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// TestServeAPIGracefulShutdown drives the shutdown path directly (no real
// signal): it starts serveAPI on an ephemeral port with a trivial handler and a
// spy pool, confirms the server is serving, cancels the shutdown context, and
// asserts serveAPI returns nil within the timeout and closed the pool exactly
// once. No database is needed — this isolates the lifecycle logic.
func TestServeAPIGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	pool := &spyPool{}

	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo)

	shutdownCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveAPI(shutdownCtx, logger, srv, ln, pool) }()

	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + ln.Addr().String()
	waitForReachable(t, client, base+"/")

	cancel() // trigger graceful shutdown

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveAPI returned error on clean shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveAPI did not return within 10s of shutdown")
	}

	if n := pool.closeCount(); n != 1 {
		t.Errorf("pool Close called %d time(s), want exactly 1", n)
	}
	if logs := buf.String(); !strings.Contains(logs, "shutdown complete") {
		t.Errorf("expected a 'shutdown complete' log line; got:\n%s", logs)
	}
	if logs := buf.String(); strings.Contains(logs, "deadline exceeded") {
		t.Errorf("unexpected deadline-exceeded warning on a clean shutdown:\n%s", logs)
	}
}

// TestRunAPIBindErrorSurfaces proves a bind failure surfaces immediately as an
// error from runAPI rather than hanging: it occupies an ephemeral port, then
// points the api command at the same address. The DSN is syntactically valid but
// never dialed (pgxpool connects lazily), so the failure is purely the bind.
func TestRunAPIBindErrorSurfaces(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	cfg := config.Config{
		BypassRLSURL: "postgres://test:test@127.0.0.1:5432/opengate_test?sslmode=disable",
		HTTPAddr:     occupied.Addr().String(),
	}
	logger := observability.NewLogger(io.Discard, slog.LevelError)

	err = runAPI(context.Background(), logger, cfg)
	if err == nil {
		t.Fatal("expected a bind error, got nil")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error %q does not identify the bind failure", err)
	}
}

// TestRunAPIRequiresBypassDSN proves the api subcommand validates its required
// config (the bypass DSN) before acquiring any resource.
func TestRunAPIRequiresBypassDSN(t *testing.T) {
	logger := observability.NewLogger(io.Discard, slog.LevelError)
	err := runAPI(context.Background(), logger, config.Config{HTTPAddr: ":0"})
	if err == nil {
		t.Fatal("expected an error when BYPASS_RLS_DATABASE_URL is unset")
	}
	if !strings.Contains(err.Error(), "BYPASS_RLS_DATABASE_URL") {
		t.Errorf("error %q does not name the missing variable", err)
	}
}

// TestAPIServerHealthEndpoints is the end-to-end server-boot + health test
// against a real (testcontainers) Postgres on the bypass pool. It asserts:
//   - /livez returns 200 without touching the DB (it still returns 200 after the
//     pool is closed);
//   - /readyz returns 200 while the DB is reachable, and a Problem Details 503
//     once the bypass pool is closed (the real ping-failure path);
//   - the server shuts down cleanly when the shutdown context is canceled.
func TestAPIServerHealthEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	dsn := startBypassPostgres(ctx, t)

	pool, err := postgres.NewBypassPool(ctx, dsn)
	if err != nil {
		t.Fatalf("new bypass pool: %v", err)
	}
	// Idempotent (pgxpool.Pool.Close is guarded by sync.Once); serveAPI also
	// closes it, and the test closes it mid-run to force a ping failure.
	t.Cleanup(pool.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{
		Handler:           httpadapter.NewRouter(pool),
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger := observability.NewLogger(io.Discard, slog.LevelInfo)

	shutdownCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveAPI(shutdownCtx, logger, srv, ln, pool) }()

	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + ln.Addr().String()
	waitForReachable(t, client, base+httpadapter.LivenessPath)

	// Healthy: liveness and readiness both 200.
	if code, _, body := get(t, client, base+httpadapter.LivenessPath); code != http.StatusOK || body != `{"status":"ok"}` {
		t.Errorf("GET /livez = %d %q, want 200 {\"status\":\"ok\"}", code, body)
	}
	if code, _, _ := get(t, client, base+httpadapter.ReadinessPath); code != http.StatusOK {
		t.Errorf("GET /readyz (DB up) = %d, want 200", code)
	}

	// Make the database unreachable by closing the pool. Readiness must flip to a
	// Problem Details 503; liveness must stay 200 (it never touches the DB).
	pool.Close()

	if code, _, _ := get(t, client, base+httpadapter.LivenessPath); code != http.StatusOK {
		t.Errorf("GET /livez (DB down) = %d, want 200 (liveness must not depend on the DB)", code)
	}
	code, hdr, _ := get(t, client, base+httpadapter.ReadinessPath)
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz (DB down) = %d, want 503", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("readyz 503 Content-Type = %q, want application/problem+json", ct)
	}

	// Graceful shutdown completes cleanly.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveAPI returned error on clean shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveAPI did not return within 10s of shutdown")
	}
}

// startBypassPostgres starts a throwaway Postgres container and returns a DSN.
// No migrations are run: the readiness probe only pings, which needs no schema.
func startBypassPostgres(ctx context.Context, t *testing.T) string {
	t.Helper()
	container, err := tcpostgres.Run(ctx,
		"postgres:16.14-bookworm",
		tcpostgres.WithDatabase("opengate_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

// waitForReachable polls url until it answers (any response) or a short deadline
// elapses, smoothing over the brief gap before the serve goroutine begins
// accepting.
func waitForReachable(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not become reachable at %s", url)
}

// get performs a GET and returns the status, headers, and body as a string.
func get(t *testing.T, client *http.Client, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body from %s: %v", url, err)
	}
	return resp.StatusCode, resp.Header, string(b)
}
