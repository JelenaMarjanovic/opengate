package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// probeTenantSQL reads the session-scoped tenant variable. The missing-ok form
// (second arg true) returns NULL instead of erroring if the variable was never
// set, so the probe itself never fails; in practice the hooks always set it to a
// value or the empty string before a caller can run this.
const probeTenantSQL = `SELECT current_setting('app.current_tenant_id', true)`

// TestNewPoolTenantHooks exercises the regular pool's tenant-binding hooks
// against real Postgres, WITHOUT any RLS policy: the hooks are independently
// observable by reading app.current_tenant_id. It covers bind-on-acquire,
// reset-on-release, and the fail-closed missing-tenant path (binds empty and warns).
// The reset-FAILURE path and the exact statements/return values are covered by
// the white-box unit tests in pool_internal_test.go.
func TestNewPoolTenantHooks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	dsn := startPostgresPool(ctx, t)

	// Bind on acquire: a tenant in context is set on the checked-out connection.
	t.Run("binds tenant on acquire", func(t *testing.T) {
		var buf bytes.Buffer
		pool, err := postgres.NewPool(ctx, dsn, observability.NewLogger(&buf, slog.LevelDebug))
		if err != nil {
			t.Fatalf("new pool: %v", err)
		}
		t.Cleanup(pool.Close)

		tid := uuid.New()
		conn, err := pool.Acquire(tenant.NewContext(ctx, tenant.ID(tid)))
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()

		var got string
		if err := conn.QueryRow(ctx, probeTenantSQL).Scan(&got); err != nil {
			t.Fatalf("probe current_setting: %v", err)
		}
		if got != tid.String() {
			t.Errorf("app.current_tenant_id = %q, want %q", got, tid.String())
		}
		if buf.Len() != 0 {
			t.Errorf("unexpected log output on the bind path: %s", buf.String())
		}
	})

	// Reset on release: after a tenant-bound connection is released and reused
	// (pool_max_conns=1 forces the same backend), the variable reads back empty.
	t.Run("resets tenant on release", func(t *testing.T) {
		var buf bytes.Buffer
		pool, err := postgres.NewPool(ctx, dsn, observability.NewLogger(&buf, slog.LevelDebug))
		if err != nil {
			t.Fatalf("new pool: %v", err)
		}
		t.Cleanup(pool.Close)

		tid := uuid.New()
		c1, err := pool.Acquire(tenant.NewContext(ctx, tenant.ID(tid)))
		if err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		var bound string
		if err := c1.QueryRow(ctx, probeTenantSQL).Scan(&bound); err != nil {
			t.Fatalf("probe after first acquire: %v", err)
		}
		if bound != tid.String() {
			t.Fatalf("setup: bound tenant = %q, want %q", bound, tid.String())
		}
		c1.Release() // AfterRelease -> releaseReset clears the binding to ''.

		// Re-acquire without a tenant; with one connection in the pool this is the
		// same backend. The probe must read empty. NOTE: this end-to-end check does
		// not on its own ISOLATE the release reset from PrepareConn's missing-tenant
		// empty-bind (both write ''). The isolated proofs are the releaseReset unit
		// tests plus the real-DB clear exercised below in the missing-tenant subtest.
		c2, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("second acquire: %v", err)
		}
		defer c2.Release()
		var got string
		if err := c2.QueryRow(ctx, probeTenantSQL).Scan(&got); err != nil {
			t.Fatalf("probe after re-acquire: %v", err)
		}
		if got != "" {
			t.Errorf("after release and re-acquire, app.current_tenant_id = %q, want empty", got)
		}
	})

	// Missing tenant on the RLS-bound pool: fail closed. The variable is bound to
	// '' on the real connection (proving clearTenantSQL empties a live session
	// variable) and a warning is emitted through the real pool path.
	t.Run("missing tenant warns and binds empty", func(t *testing.T) {
		var buf bytes.Buffer
		pool, err := postgres.NewPool(ctx, dsn, observability.NewLogger(&buf, slog.LevelDebug))
		if err != nil {
			t.Fatalf("new pool: %v", err)
		}
		t.Cleanup(pool.Close)

		conn, err := pool.Acquire(ctx) // no tenant in context
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()

		var got string
		if err := conn.QueryRow(ctx, probeTenantSQL).Scan(&got); err != nil {
			t.Fatalf("probe current_setting: %v", err)
		}
		if got != "" {
			t.Errorf("app.current_tenant_id = %q, want empty (fail-closed bind)", got)
		}

		rec := findWarnLine(t, buf.Bytes())
		if rec["event"] != "missing_tenant" {
			t.Errorf("warn event = %v, want missing_tenant", rec["event"])
		}
		if rec["hook"] != "prepare_conn" {
			t.Errorf("warn hook = %v, want prepare_conn", rec["hook"])
		}
	})
}

// startPostgresPool starts a throwaway Postgres container and returns a DSN for
// the regular pool. pool_max_conns=1 pins the pool to a single backend so an
// acquire/release/re-acquire cycle reuses the same connection, which is what
// makes the reset-on-release behavior observable. No migrations are run: the
// hooks only touch the app.current_tenant_id session variable, which needs no
// table or role.
func startPostgresPool(ctx context.Context, t *testing.T) string {
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

	dsn, err := container.ConnectionString(ctx, "sslmode=disable", "pool_max_conns=1")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

// findWarnLine returns the first WARN-level JSON log record, failing if none is
// present. It asserts via the structured level field, not message prose.
func findWarnLine(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\nline: %s", err, line)
		}
		if rec[slog.LevelKey] == "WARN" {
			return rec
		}
	}
	t.Fatalf("no WARN log line found in output: %s", raw)
	return nil
}
