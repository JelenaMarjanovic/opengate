package advisory_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/coordination/advisory"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// The contention and cancellation tests need two locks held on *different*
// Postgres sessions, so each test uses two independent pools (poolA, poolB) and
// one transaction per pool. Two transactions multiplexed onto a single
// connection could not demonstrate cross-session blocking — Postgres would let
// the same session re-enter its own advisory lock.

// advisoryEnv holds two independent pools against one shared container.
type advisoryEnv struct {
	poolA *pgxpool.Pool
	poolB *pgxpool.Pool
}

// setupAdvisory starts one Postgres container and opens two independent pools.
// No schema is needed: advisory locks require only a connection.
func setupAdvisory(ctx context.Context, t *testing.T) *advisoryEnv {
	t.Helper()

	container := testsupport.StartPostgres(ctx, t)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	return &advisoryEnv{
		poolA: openPool(ctx, t, dsn),
		poolB: openPool(ctx, t, dsn),
	}
}

// openPool opens a pgx pool for dsn, registered for cleanup.
func openPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// I1 — acquire and release on commit. A takes the lock, runs fn, commits; B then
// acquires the same lock immediately, proving it was freed on A's commit.
func TestWithLock_AcquireAndReleaseOnCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const name = "projector.commit_release"

	txA, err := env.poolA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx A: %v", err)
	}
	ran := false
	if err := advisory.WithLock(ctx, txA, name, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("A WithLock: %v", err)
	}
	if !ran {
		t.Fatal("A fn did not run")
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit tx A: %v", err)
	}

	txB, err := env.poolB.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx B: %v", err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	ranB := false
	if err := advisory.WithLock(ctx, txB, name, func() error { ranB = true; return nil }); err != nil {
		t.Fatalf("B WithLock: %v", err)
	}
	if !ranB {
		t.Fatal("B fn did not run after A committed")
	}
}

// I2 — contention (core mutual-exclusion test). While A holds the lock, B blocks
// inside Postgres; B acquires only after A's transaction commits.
func TestWithLock_Contention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const name = "projector.contention"

	var aHolds, bAcquired atomic.Bool
	aHeld := make(chan struct{})   // closed once A is inside fn holding the lock
	release := make(chan struct{}) // closed to let A's fn return
	aDone := make(chan error, 1)

	// A: hold the lock, signal, then block until released, then commit.
	go func() {
		txA, err := env.poolA.Begin(ctx)
		if err != nil {
			aDone <- err
			return
		}
		err = advisory.WithLock(ctx, txA, name, func() error {
			aHolds.Store(true)
			close(aHeld)
			<-release
			return nil
		})
		if err != nil {
			_ = txA.Rollback(ctx)
			aDone <- err
			return
		}
		aDone <- txA.Commit(ctx)
	}()

	<-aHeld
	if !aHolds.Load() {
		t.Fatal("A does not hold the lock after signaling")
	}

	// B: attempt to acquire the same lock; it must block until A commits.
	bDone := make(chan error, 1)
	go func() {
		txB, err := env.poolB.Begin(ctx)
		if err != nil {
			bDone <- err
			return
		}
		err = advisory.WithLock(ctx, txB, name, func() error {
			bAcquired.Store(true)
			return nil
		})
		if err != nil {
			_ = txB.Rollback(ctx)
			bDone <- err
			return
		}
		bDone <- txB.Commit(ctx)
	}()

	// B must still be blocked: it cannot have acquired while A holds the lock.
	time.Sleep(200 * time.Millisecond)
	if bAcquired.Load() {
		t.Fatal("B acquired the lock while A still held it")
	}

	// Release A, let its tx commit; B must now acquire.
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A goroutine: %v", err)
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B goroutine: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B did not acquire the lock within 5s after A committed")
	}
	if !bAcquired.Load() {
		t.Fatal("B fn never ran after A released the lock")
	}
}

// I3 — distinct names do not contend. A holds N1; B takes a different name N2 and
// must proceed immediately, well before A is released.
func TestWithLock_DistinctNamesDoNotContend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const n1 = "projector.alpha"
	const n2 = "projector.beta"

	aHeld := make(chan struct{})
	release := make(chan struct{})
	aDone := make(chan error, 1)

	go func() {
		txA, err := env.poolA.Begin(ctx)
		if err != nil {
			aDone <- err
			return
		}
		err = advisory.WithLock(ctx, txA, n1, func() error {
			close(aHeld)
			<-release
			return nil
		})
		if err != nil {
			_ = txA.Rollback(ctx)
			aDone <- err
			return
		}
		aDone <- txA.Commit(ctx)
	}()

	<-aHeld

	// B on a distinct name must not block on A.
	bDone := make(chan error, 1)
	go func() {
		txB, err := env.poolB.Begin(ctx)
		if err != nil {
			bDone <- err
			return
		}
		err = advisory.WithLock(ctx, txB, n2, func() error { return nil })
		if err != nil {
			_ = txB.Rollback(ctx)
			bDone <- err
			return
		}
		bDone <- txB.Commit(ctx)
	}()

	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B goroutine: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B blocked on a distinct lock name; distinct names must not contend")
	}

	// Clean up A.
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A goroutine: %v", err)
	}
}

// I4 — release on rollback. A takes the lock then rolls back (not commit); B must
// acquire immediately, proving the tx-scoped lock releases on rollback too.
func TestWithLock_ReleaseOnRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const name = "projector.rollback_release"

	txA, err := env.poolA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx A: %v", err)
	}
	if err := advisory.WithLock(ctx, txA, name, func() error { return nil }); err != nil {
		t.Fatalf("A WithLock: %v", err)
	}
	if err := txA.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx A: %v", err)
	}

	txB, err := env.poolB.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx B: %v", err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	ranB := false
	if err := advisory.WithLock(ctx, txB, name, func() error { ranB = true; return nil }); err != nil {
		t.Fatalf("B WithLock: %v", err)
	}
	if !ranB {
		t.Fatal("B fn did not run after A rolled back")
	}
}

// I6 — TryWithLock on a FREE lock acquires and runs fn. The non-blocking path's
// happy case: nothing else holds the lock, so the try succeeds and fn executes.
func TestTryWithLock_AcquiresFreeLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const name = "projector.try_free"

	tx, err := env.poolA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ran := false
	acquired, err := advisory.TryWithLock(ctx, tx, name, func() error { ran = true; return nil })
	if err != nil {
		t.Fatalf("TryWithLock: %v", err)
	}
	if !acquired {
		t.Fatal("TryWithLock did not acquire a free lock")
	}
	if !ran {
		t.Fatal("fn did not run after a successful try-acquire")
	}
}

// I7 — TryWithLock SKIPS a held lock without blocking, then a fresh try acquires it
// once the holder releases. This is the projector singleton property: a second
// instance that finds the lock taken must no-op immediately (fn not run,
// acquired=false) rather than queue, and the lock must become acquirable again the
// moment the holder's transaction ends.
func TestTryWithLock_SkipsWhenHeldThenAcquiresAfterRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const name = "projector.try_held"

	aHeld := make(chan struct{})   // closed once A holds the lock
	release := make(chan struct{}) // closed to let A's fn return
	aDone := make(chan error, 1)

	// A: hold the lock, signal, block until released, then commit (releasing it).
	go func() {
		txA, err := env.poolA.Begin(ctx)
		if err != nil {
			aDone <- err
			return
		}
		err = advisory.WithLock(ctx, txA, name, func() error {
			close(aHeld)
			<-release
			return nil
		})
		if err != nil {
			_ = txA.Rollback(ctx)
			aDone <- err
			return
		}
		aDone <- txA.Commit(ctx)
	}()

	<-aHeld

	// B: try the held lock on a second session. The call must return IMMEDIATELY
	// with acquired=false and must not run fn — no goroutine needed precisely
	// because TryWithLock never blocks.
	txB, err := env.poolB.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx B: %v", err)
	}
	var bFnRan atomic.Bool
	acquired, err := advisory.TryWithLock(ctx, txB, name, func() error { bFnRan.Store(true); return nil })
	if err != nil {
		t.Fatalf("B TryWithLock: %v", err)
	}
	if acquired {
		t.Fatal("B acquired a lock already held by A")
	}
	if bFnRan.Load() {
		t.Fatal("B fn ran even though the lock was not acquired")
	}
	_ = txB.Rollback(ctx)

	// Release A and wait for its commit, freeing the lock.
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A goroutine: %v", err)
	}

	// C: a fresh try on the same name now succeeds, proving the lock was released.
	txC, err := env.poolB.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx C: %v", err)
	}
	defer func() { _ = txC.Rollback(ctx) }()
	ranC := false
	acquiredC, err := advisory.TryWithLock(ctx, txC, name, func() error { ranC = true; return nil })
	if err != nil {
		t.Fatalf("C TryWithLock: %v", err)
	}
	if !acquiredC {
		t.Fatal("C did not acquire the lock after A released it")
	}
	if !ranC {
		t.Fatal("C fn did not run after acquiring the released lock")
	}
}

// I8 — distinct names do not contend under TryWithLock either: while A holds N1, a
// try on N2 acquires immediately.
func TestTryWithLock_DistinctNamesBothAcquire(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const n1 = "projector.try_alpha"
	const n2 = "projector.try_beta"

	aHeld := make(chan struct{})
	release := make(chan struct{})
	aDone := make(chan error, 1)

	go func() {
		txA, err := env.poolA.Begin(ctx)
		if err != nil {
			aDone <- err
			return
		}
		err = advisory.WithLock(ctx, txA, n1, func() error {
			close(aHeld)
			<-release
			return nil
		})
		if err != nil {
			_ = txA.Rollback(ctx)
			aDone <- err
			return
		}
		aDone <- txA.Commit(ctx)
	}()

	<-aHeld

	// B tries a DIFFERENT name; it must acquire without blocking on A.
	txB, err := env.poolB.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx B: %v", err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	ranB := false
	acquired, err := advisory.TryWithLock(ctx, txB, n2, func() error { ranB = true; return nil })
	if err != nil {
		t.Fatalf("B TryWithLock: %v", err)
	}
	if !acquired {
		t.Fatal("B failed to acquire a distinct lock name while A held another")
	}
	if !ranB {
		t.Fatal("B fn did not run after acquiring a distinct lock")
	}

	// Clean up A.
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A goroutine: %v", err)
	}
}

// I5 — cancellation interrupts the wait (graceful-shutdown property). While A
// holds the lock, B waits with a cancellable context; canceling it must unblock
// B promptly with an error, and B's fn must never run (acquisition failed).
func TestWithLock_CancellationInterruptsWait(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}
	ctx := context.Background()
	env := setupAdvisory(ctx, t)
	const name = "projector.cancellation"

	aHeld := make(chan struct{})
	release := make(chan struct{})
	aDone := make(chan error, 1)

	go func() {
		txA, err := env.poolA.Begin(ctx)
		if err != nil {
			aDone <- err
			return
		}
		err = advisory.WithLock(ctx, txA, name, func() error {
			close(aHeld)
			<-release
			return nil
		})
		if err != nil {
			_ = txA.Rollback(ctx)
			aDone <- err
			return
		}
		aDone <- txA.Commit(ctx)
	}()

	<-aHeld

	// B waits on the held lock with a cancellable context.
	bCtx, cancel := context.WithCancel(ctx)
	txB, err := env.poolB.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx B: %v", err)
	}

	var bFnCalled atomic.Bool
	bDone := make(chan error, 1)
	go func() {
		bDone <- advisory.WithLock(bCtx, txB, name, func() error {
			bFnCalled.Store(true)
			return nil
		})
	}()

	// Give B time to block inside pg_advisory_xact_lock, then cancel.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-bDone:
		if err == nil {
			t.Fatal("B WithLock returned nil after cancellation; want a non-nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B WithLock did not return within 2s of cancellation")
	}
	if bFnCalled.Load() {
		t.Fatal("B fn ran even though acquisition was canceled; fn must not run when the lock is not held")
	}

	// Clean up: roll back B's failed tx, release A, drain A.
	_ = txB.Rollback(ctx)
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A goroutine: %v", err)
	}
}
