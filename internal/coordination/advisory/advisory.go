// Package advisory provides Postgres transaction-scoped advisory locks for
// coordinating singleton work across multiple worker instances. A given lock
// name maps deterministically to a 64-bit identifier; two transactions that
// request the same name contend, and the second blocks until the first's
// transaction commits or rolls back.
//
// Every identifier OpenGate generates is negative. River, which shares the same
// Postgres 64-bit advisory-lock space, cordons its own locks under a positive
// 32-bit prefix and therefore only ever uses non-negative keys. The two key
// bands are disjoint by construction, so OpenGate and River advisory locks
// never collide. See LockID for the detail.
package advisory

import (
	"context"
	"fmt"

	"github.com/dchest/siphash"
	"github.com/jackc/pgx/v5"
)

// advisoryHashKey is the fixed 128-bit SipHash key used to derive advisory
// lock identifiers from lock names. It is NOT a secret: advisory lock IDs are
// coordination keys, not security tokens, and are visible to anyone who can
// query pg_locks. The only requirement is that the key be constant across every
// process in a deployment, so that a given lock name always maps to the same
// integer. SipHash is used for its uniform dispersion across the 64-bit space
// (which keeps the collision probability among lock names negligible), not for
// any cryptographic property. The key must be exactly 16 bytes; a unit test
// asserts this.
var advisoryHashKey = []byte("opengate-advlock")

// signBit is the most significant bit of a 64-bit integer. LockID sets it on
// every identifier so that all OpenGate advisory-lock keys are negative.
const signBit uint64 = 1 << 63

// RiverAdvisoryLockPrefix is the prefix River must be configured with
// (river.Config.AdvisoryLockPrefix) for the disjointness invariant in LockID to
// hold. It is positive and below 2^31, so every River advisory-lock key is
// non-negative (bit 63 = 0), while OpenGate's keys (see LockID) are negative.
// The two bands therefore cannot overlap, even though both share Postgres's
// single global 64-bit advisory-lock space. Changing this value is a
// correctness decision, not a cosmetic one: a value that placed bit 63 in the
// River band would reintroduce collisions between River's internal locks and
// OpenGate's projector and job locks.
const RiverAdvisoryLockPrefix int32 = 0x4F47

// LockID computes a deterministic, always-negative int64 advisory-lock
// identifier from a structured lock name (for example "projector.audit_log",
// "credential.generate:<tenant_id>", or "job.cleanup_idempotency_keys").
//
// The high bit is forced to 1, making every result negative. This places
// OpenGate's lock keys in a number-space band provably disjoint from River's.
// River cordons its own advisory locks under a configured 32-bit prefix;
// OpenGate sets that prefix to 0x4F47, a positive value below 2^31, so every
// River key has its high bit clear (bit 63 = 0) and is non-negative. Forcing
// bit 63 = 1 here guarantees OpenGate keys (negative) and River keys
// (non-negative) can never collide, even though both share Postgres's single
// global 64-bit advisory-lock space. The lower 63 bits carry SipHash's
// dispersion, so the probability that two distinct names collide is ~2^-63.
//
// LockID is safe for concurrent use: each call hashes with its own local state.
func LockID(name string) int64 {
	h := siphash.New(advisoryHashKey)
	// A hash.Hash's Write never returns an error; the result is ignored per the
	// io.Writer contract for hash functions.
	_, _ = h.Write([]byte(name))
	// The uint64->int64 conversion is an intentional bit reinterpretation, not a
	// lossy narrowing: signBit is set, so the result is meant to be negative.
	// gosec's G115 overflow check cannot express "I want the sign overflow".
	return int64(h.Sum64() | signBit) //nolint:gosec // G115: deliberate sign-bit reinterpretation, see comment
}

// WithLock acquires the transaction-scoped advisory lock identified by name on
// the given transaction, then runs fn. The lock is acquired by Postgres inside
// tx via pg_advisory_xact_lock and is released automatically when the caller
// commits or rolls back tx; WithLock does not commit or roll back tx itself.
//
// If another transaction already holds the same lock, the underlying SQL call
// blocks inside Postgres until that transaction ends. pgx waits on the call,
// and the wait is interruptible: canceling ctx cancels the in-flight query,
// which unblocks the wait and returns an error rather than hanging. This is
// what lets a worker blocked on a lock shut down promptly.
//
// fn's error is returned unwrapped so callers can inspect it.
func WithLock(ctx context.Context, tx pgx.Tx, name string, fn func() error) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", LockID(name)); err != nil {
		return fmt.Errorf("acquire advisory lock %q: %w", name, err)
	}
	return fn()
}

// TryWithLock attempts to acquire the transaction-scoped advisory lock identified
// by name on the given transaction WITHOUT blocking. If the lock is free it is
// acquired (held until the caller commits or rolls back tx), fn is run, and
// (true, fn's error) is returned. If the lock is already held by another
// transaction, fn is NOT run and (false, nil) is returned immediately.
//
// This is the non-blocking counterpart to WithLock, used by periodic singleton
// work (projectors, maintenance jobs) that should SKIP an iteration rather than
// wait when another instance already holds the lock. pg_try_advisory_xact_lock
// never waits: it reports the outcome in the boolean it returns, so the caller can
// no-op a contended iteration cheaply instead of queueing behind the holder.
//
// fn's error is returned unwrapped (when acquired) so callers can inspect it.
func TryWithLock(ctx context.Context, tx pgx.Tx, name string, fn func() error) (bool, error) {
	var acquired bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", LockID(name)).Scan(&acquired); err != nil {
		return false, fmt.Errorf("try advisory lock %q: %w", name, err)
	}
	if !acquired {
		return false, nil
	}
	return true, fn()
}
