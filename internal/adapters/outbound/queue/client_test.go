package queue

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

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

	// RoleWorker now requires a worker config (Step 3); a minimal one with an
	// empty registry is the production foundation shape. RoleAPI stays nil.
	cases := []struct {
		role RiverRole
		wc   *workerConfig
	}{
		{RoleAPI, nil},
		{RoleWorker, &workerConfig{
			queues:          map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
			workers:         river.NewWorkers(),
			middleware:      []rivertype.Middleware{&traceMiddleware{}},
			softStopTimeout: time.Second,
		}},
	}
	for _, tc := range cases {
		client, err := newRiverClient(tc.role, pool, logger, tc.wc)
		if err != nil {
			t.Fatalf("newRiverClient(%s): %v", tc.role, err)
		}
		if client == nil {
			t.Fatalf("newRiverClient(%s) returned nil client", tc.role)
		}
	}
}

// TestNewRiverClientGuards covers the constructor's fail-fast guards: a nil pool,
// a nil logger, or a role/worker-config mismatch is a composition-root wiring
// error, caught before River is asked to build anything.
func TestNewRiverClientGuards(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := newRiverClient(RoleAPI, nil, logger, nil); err == nil {
		t.Error("newRiverClient with nil pool: want error, got nil")
	}

	dummyPool := &pgxpool.Pool{}
	if _, err := newRiverClient(RoleAPI, dummyPool, nil, nil); err == nil {
		t.Error("newRiverClient with nil logger: want error, got nil")
	}

	// RoleWorker without a worker config is a wiring error.
	if _, err := newRiverClient(RoleWorker, dummyPool, logger, nil); err == nil {
		t.Error("newRiverClient(RoleWorker) with nil worker config: want error, got nil")
	}

	// RoleAPI with a worker config is a wiring error: the insert-only role must
	// not be handed worker overlay.
	if _, err := newRiverClient(RoleAPI, dummyPool, logger, &workerConfig{}); err == nil {
		t.Error("newRiverClient(RoleAPI) with non-nil worker config: want error, got nil")
	}
}
