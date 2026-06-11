package queue

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestNewRiverClient asserts the Step 1 constructor builds both roles without
// error over a real pgx pool's driver. Construction does not touch the database
// (river.NewClient only assembles the client struct), so no migration is needed
// here -- the pool merely has to be a valid *pgxpool.Pool so riverpgxv5 can infer
// the pgx.Tx transaction type.
//
// Step 1 leaves Queues/Workers/PeriodicJobs empty for BOTH roles, so neither
// client "will execute jobs"; the assertion is simply that NewClient accepts the
// insert-only shape. River does not expose Config off a built client, so Schema
// cannot be read back through the public API; the migration-sequence test
// (migrate_test.go) proves the `river` schema is the one actually targeted.
func TestNewRiverClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := testsupport.StartPostgres(ctx, t)

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, role := range []RiverRole{RoleAPI, RoleWorker} {
		client, err := newRiverClient(role, pool, logger)
		if err != nil {
			t.Fatalf("newRiverClient(%s): %v", role, err)
		}
		if client == nil {
			t.Fatalf("newRiverClient(%s) returned nil client", role)
		}
	}
}

// TestNewRiverClientGuards covers the constructor's fail-fast guards: a nil pool
// or nil logger is a composition-root wiring error, caught before River is asked
// to build anything.
func TestNewRiverClientGuards(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := newRiverClient(RoleAPI, nil, logger); err == nil {
		t.Error("newRiverClient with nil pool: want error, got nil")
	}

	dummyPool := &pgxpool.Pool{}
	if _, err := newRiverClient(RoleAPI, dummyPool, nil); err == nil {
		t.Error("newRiverClient with nil logger: want error, got nil")
	}
}
