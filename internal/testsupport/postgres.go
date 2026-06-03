// Package testsupport holds test infrastructure shared across packages.
//
// It lives in a regular (non-_test.go) file on purpose: _test.go helpers are not
// importable from other packages, and the Postgres container startup is needed by
// tests in at least cmd/opengate and internal/adapters/outbound/postgres. A
// per-package _test.go helper would therefore leave the readiness wait strategy
// duplicated in two packages — exactly the drift that produced the initdb-race
// flake. Centralizing it here makes the wait a single source of truth.
//
// No production code imports this package, so testcontainers (and testing) never
// reach the shipped binary, even though the package compiles under
// `go build ./...`. No build tag is used: a custom tag would be the simplest way
// to keep the package out of `go build ./...`, but Go does not apply any tag to
// _test.go files automatically, so a tagged testsupport would not be importable by
// the tests that need it without every test file and the test invocation opting in
// — friction that buys nothing, since an unimported package is never linked anyway.
package testsupport

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PostgresImage pins the Postgres image used by every container-backed test, so a
// version bump happens in one place.
const PostgresImage = "postgres:16.14-bookworm"

// StartPostgres starts a throwaway Postgres container with the project's standard
// test credentials (database "opengate_test", superuser "test"/"test") and the
// robust readiness wait, registering termination via t.Cleanup.
//
// It returns the container rather than a connection string so each call site can
// derive exactly what it needs — the superuser DSN, the same DSN with extra params
// (e.g. pool_max_conns), or the host/port for role-scoped DSNs — without this
// helper imposing a rigid signature on divergent per-test setup. Migrations, role
// wiring, and pool construction stay at the call sites by design.
func StartPostgres(ctx context.Context, t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()

	container, err := tcpostgres.Run(ctx,
		PostgresImage,
		tcpostgres.WithDatabase("opengate_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			// Wait for the readiness log to appear TWICE — the postgres entrypoint
			// starts a temporary server for initdb (first occurrence) then restarts
			// the real one (second). Waiting only for the listening port races that
			// restart and yields "connection reset by peer" on the first query; the
			// occurrence-2 log wait (the testcontainers postgres-module default) is robust.
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
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

	return container
}
