package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// These are white-box (package postgres) unit tests for the tenant-binding hook
// bodies. They exercise every branch of bindTenant and releaseReset through the
// execer seam with a stub, so the security-critical control flow — including the
// reset-failure path that must DESTROY the connection — is verified without a
// live database. The end-to-end behavior against real Postgres lives in
// pool_test.go (package postgres_test).

// recordedExec captures one Exec call so a test can assert the exact statement
// and bound arguments the hook issued.
type recordedExec struct {
	sql  string
	args []any
}

// stubExecer is a test double for the execer seam. err is returned from every
// Exec (nil for the success path), and every call is recorded.
type stubExecer struct {
	err   error
	calls []recordedExec
}

func (s *stubExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.calls = append(s.calls, recordedExec{sql: sql, args: args})
	return pgconn.CommandTag{}, s.err
}

// findLogLine returns the first JSON log record at the given level, failing the
// test if none is present. Assertions target stable structured attributes rather
// than the human-readable message text.
func findLogLine(t *testing.T, raw []byte, level string) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\nline: %s", err, line)
		}
		if rec[slog.LevelKey] == level {
			return rec
		}
	}
	t.Fatalf("no %s log line found in output: %s", level, raw)
	return nil
}

// TestBindTenantBindsPresentTenant: a tenant in context is bound via the
// injection-safe $1 form, the hook reports the connection valid, and nothing is
// logged on the happy path.
func TestBindTenantBindsPresentTenant(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)
	tid := tenant.ID(uuid.New())
	ctx := tenant.NewContext(context.Background(), tid)
	stub := &stubExecer{}

	if ok := bindTenant(ctx, stub, logger); !ok {
		t.Fatal("bindTenant returned false on a successful bind; want true")
	}
	if len(stub.calls) != 1 {
		t.Fatalf("Exec called %d times, want 1", len(stub.calls))
	}
	if stub.calls[0].sql != setTenantSQL {
		t.Errorf("bind sql = %q, want setTenantSQL %q", stub.calls[0].sql, setTenantSQL)
	}
	if len(stub.calls[0].args) != 1 {
		t.Fatalf("bind args = %v, want exactly one ($1)", stub.calls[0].args)
	}
	got, ok := stub.calls[0].args[0].(string)
	if !ok || got != tid.String() {
		t.Errorf("bind $1 = %v, want tenant id %q", stub.calls[0].args[0], tid.String())
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log output on the success path: %s", buf.String())
	}
}

// TestBindTenantPresentExecErrorReturnsFalse: when binding a present tenant
// fails, the hook returns false (pool discards the connection) and logs an error.
func TestBindTenantPresentExecErrorReturnsFalse(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)
	ctx := tenant.NewContext(context.Background(), tenant.ID(uuid.New()))
	stub := &stubExecer{err: errors.New("bind failed")}

	if ok := bindTenant(ctx, stub, logger); ok {
		t.Fatal("bindTenant returned true despite a failed bind; want false (discard connection)")
	}
	rec := findLogLine(t, buf.Bytes(), "ERROR")
	if rec["hook"] != "prepare_conn" {
		t.Errorf("error hook = %v, want prepare_conn", rec["hook"])
	}
	if _, ok := rec["error"]; !ok {
		t.Error("error record is missing the structured \"error\" attribute")
	}
}

// TestBindTenantMissingTenantWarnsAndBindsEmpty: with no tenant in context the
// hook fails closed — it binds the empty string, warns, and returns true (it must
// NOT return false, which would loop the pool over a context-borne condition).
func TestBindTenantMissingTenantWarnsAndBindsEmpty(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)
	stub := &stubExecer{}

	if ok := bindTenant(context.Background(), stub, logger); !ok {
		t.Fatal("bindTenant returned false on a missing tenant; want true (fail-closed empty bind, no acquire loop)")
	}
	if len(stub.calls) != 1 || stub.calls[0].sql != clearTenantSQL {
		t.Fatalf("calls = %+v, want exactly one clearTenantSQL", stub.calls)
	}
	if len(stub.calls[0].args) != 0 {
		t.Errorf("clearTenantSQL issued with args %v, want none", stub.calls[0].args)
	}
	rec := findLogLine(t, buf.Bytes(), "WARN")
	if rec["event"] != "missing_tenant" {
		t.Errorf("warn event = %v, want missing_tenant", rec["event"])
	}
	if rec["hook"] != "prepare_conn" {
		t.Errorf("warn hook = %v, want prepare_conn", rec["hook"])
	}
}

// TestBindTenantMissingTenantExecErrorReturnsFalse: if even the empty-bind fails,
// that is a broken connection (not the missing-tenant condition), so the hook
// returns false to discard it. This cannot loop — a healthy replacement binds
// empty without error.
func TestBindTenantMissingTenantExecErrorReturnsFalse(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)
	stub := &stubExecer{err: errors.New("connection closed")}

	if ok := bindTenant(context.Background(), stub, logger); ok {
		t.Fatal("bindTenant returned true despite a failed empty-bind on a broken connection; want false")
	}
	rec := findLogLine(t, buf.Bytes(), "ERROR")
	if rec["hook"] != "prepare_conn" {
		t.Errorf("error hook = %v, want prepare_conn", rec["hook"])
	}
}

// TestReleaseResetSuccessReturnsTrue: a clean reset issues clearTenantSQL, keeps
// the connection (returns true), and logs nothing.
func TestReleaseResetSuccessReturnsTrue(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)
	stub := &stubExecer{}

	if ok := releaseReset(context.Background(), stub, logger); !ok {
		t.Fatal("releaseReset returned false on a successful reset; want true")
	}
	if len(stub.calls) != 1 || stub.calls[0].sql != clearTenantSQL {
		t.Fatalf("calls = %+v, want exactly one clearTenantSQL", stub.calls)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log output on the success path: %s", buf.String())
	}
}

// TestReleaseResetFailureDestroysConnection is the security-critical leak guard:
// when the reset Exec errors, AfterRelease must NOT recycle a connection it
// cannot prove is clean. releaseReset returns false (the pool destroys the
// connection) and logs an error. Tested via the stub seam because forcing a live
// pooled connection's reset to fail is not cleanly reproducible.
func TestReleaseResetFailureDestroysConnection(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)
	stub := &stubExecer{err: errors.New("reset failed")}

	if ok := releaseReset(context.Background(), stub, logger); ok {
		t.Fatal("releaseReset returned true on a failed reset; want false (destroy the connection)")
	}
	if len(stub.calls) != 1 || stub.calls[0].sql != clearTenantSQL {
		t.Fatalf("calls = %+v, want exactly one clearTenantSQL", stub.calls)
	}
	rec := findLogLine(t, buf.Bytes(), "ERROR")
	if rec["hook"] != "after_release" {
		t.Errorf("error hook = %v, want after_release", rec["hook"])
	}
	if _, ok := rec["error"]; !ok {
		t.Error("error record is missing the structured \"error\" attribute")
	}
}
