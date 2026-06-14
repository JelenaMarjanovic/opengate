package advisory

import "testing"

// U1 — determinism: the same name must always map to the same identifier, since
// every process in a deployment relies on that stable mapping to contend on the
// same lock.
func TestLockID_Deterministic(t *testing.T) {
	const name = "projector.audit_log"
	first := LockID(name)
	for i := 0; i < 100; i++ {
		if got := LockID(name); got != first {
			t.Fatalf("LockID(%q) not deterministic: call %d returned %d, want %d", name, i, got, first)
		}
	}
}

// U2 — sign bit forced: every identifier OpenGate generates must be negative,
// including the empty string and each of the three structured name shapes the
// codebase uses.
func TestLockID_AlwaysNegative(t *testing.T) {
	names := []string{
		"",
		"projector.audit_log",
		"credential.generate:11111111-2222-3333-4444-555555555555",
		"job.cleanup_idempotency_keys",
	}
	for _, name := range names {
		if id := LockID(name); id >= 0 {
			t.Errorf("LockID(%q) = %d; want negative (sign bit forced)", name, id)
		}
	}
}

// U3 — distinctness: distinct names map to distinct identifiers. This is a
// sanity check on the hash, not a collision proof.
func TestLockID_Distinct(t *testing.T) {
	names := []string{
		"projector.audit_log",
		"projector.credential_index",
		"credential.generate:tenant-a",
		"credential.generate:tenant-b",
		"job.cleanup_idempotency_keys",
		"job.expire_sessions",
	}
	seen := make(map[int64]string, len(names))
	for _, name := range names {
		id := LockID(name)
		if prev, ok := seen[id]; ok {
			t.Errorf("collision: LockID(%q) == LockID(%q) == %d", name, prev, id)
		}
		seen[id] = name
	}
}

// U4 — disjointness invariant: codify why OpenGate and River keys can never
// collide. River shifts its 32-bit AdvisoryLockPrefix into the high 32 bits of
// the 64-bit key, so for the configured prefix the resulting key is non-negative
// at both suffix extremes (all-zero and all-one low 32 bits). OpenGate forces the
// sign bit and is therefore negative. The two bands cannot overlap.
//
// RiverAdvisoryLockPrefix is the value the River client is actually configured
// with (queue/client.go points river.Config.AdvisoryLockPrefix at it). Asserting
// against the live constant — not a hardcoded copy — means this test fails if the
// prefix ever changes to a value that breaks disjointness: a prefix with bit 31
// set would shift into bit 63 and make River keys negative.
func TestLockID_DisjointFromRiverBand(t *testing.T) {
	const prefix = int64(RiverAdvisoryLockPrefix)

	// River key at the low extreme: suffix = 0x00000000.
	riverLow := (prefix << 32) | int64(uint32(0x00000000))
	if riverLow < 0 {
		t.Errorf("River key at low suffix = %d; want non-negative", riverLow)
	}

	// River key at the high extreme: suffix = 0xFFFFFFFF.
	riverHigh := (prefix << 32) | int64(uint32(0xFFFFFFFF))
	if riverHigh < 0 {
		t.Errorf("River key at high suffix = %d; want non-negative", riverHigh)
	}

	// OpenGate keys sit in the negative band.
	if id := LockID("projector.audit_log"); id >= 0 {
		t.Errorf("OpenGate LockID = %d; want negative (disjoint from River band)", id)
	}
}

// U5 — key length: SipHash-128 requires exactly a 16-byte key; a shorter slice
// panics inside siphash.New.
func TestAdvisoryHashKey_Length(t *testing.T) {
	if got := len(advisoryHashKey); got != 16 {
		t.Fatalf("len(advisoryHashKey) = %d; want 16", got)
	}
}
