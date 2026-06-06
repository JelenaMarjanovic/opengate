package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/authz"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
)

// TestCasbinPolicyLoaderAndAuthorizer exercises the full step-2 chain against a
// real Postgres: the ListCasbinPolicyRules query, the bypass-pool loader, the
// authorizer's atomic enforcer, and the refresh loop. It proves the seed is
// enforced as written, then INSERTs a new rule and asserts that the refresh loop
// picks it up and atomically swaps it in — the mechanism behind AC3.
//
// The runtime INSERT goes through the SUPERUSER pool, not an application role:
// the policy is migration-managed and "not mutated at runtime" (the casbin_rules
// migration), so no application role is granted INSERT. Simulating a policy
// change as an out-of-band superuser mutation mirrors how the query-adapter test
// flips tenant status, and proves the loader observes a change made entirely
// outside the application's own privileges.
func TestCasbinPolicyLoaderAndAuthorizer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startMigratedContainer(ctx, t)
	bypassPool := openBypassPool(ctx, t, container)
	superPool := openSuperuserPool(ctx, t, container)

	logger := observability.NewLogger(io.Discard, slog.LevelError)
	loader := postgres.NewCasbinPolicyLoader(bypassPool, logger)

	// The loader reads the seed off the bypass pool and adapts it to [][]string.
	rules, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("loader.Load returned zero rules; the seed should be present")
	}
	for _, r := range rules {
		if len(r) != 3 {
			t.Fatalf("rule %v has %d fields, want exactly 3 (sub, obj, act)", r, len(r))
		}
	}

	// A short refresh interval so the test does not wait long for the swap.
	const refreshInterval = 100 * time.Millisecond
	authorizer, err := authz.NewCasbinAuthorizer(loader.Load, authz.ModelText, refreshInterval, logger)
	if err != nil {
		t.Fatalf("NewCasbinAuthorizer: %v", err)
	}
	authorizer.Start(ctx)
	t.Cleanup(authorizer.Close)

	// The seed, enforced as written: owner wildcard, manager read+write, and the
	// auditor read-only (members write is NOT in the seed).
	seedCases := []struct {
		role, resource, action string
		want                   bool
	}{
		{"owner", "billing", "delete", true}, // wildcard: resource/action in no rule
		{"manager", "members", "write", true},
		{"auditor", "members", "read", true},
		{"auditor", "audit", "read", true},
		{"auditor", "members", "write", false}, // about to be granted below
	}
	for _, c := range seedCases {
		got, err := authorizer.Enforce(c.role, c.resource, c.action)
		if err != nil {
			t.Fatalf("Enforce(%q,%q,%q): %v", c.role, c.resource, c.action, err)
		}
		if got != c.want {
			t.Errorf("seed Enforce(%q,%q,%q) = %v, want %v", c.role, c.resource, c.action, got, c.want)
		}
	}

	// Grant the auditor members write via an out-of-band superuser INSERT.
	if _, err := superPool.Exec(ctx,
		`INSERT INTO casbin_rules (ptype, v0, v1, v2) VALUES ('p', 'auditor', 'members', 'write')`,
	); err != nil {
		t.Fatalf("insert new policy rule: %v", err)
	}

	// With the refresh loop running, the decision must flip from denied to allowed
	// once the loop re-loads and atomically swaps in the new enforcer. Poll with a
	// generous deadline (many refresh intervals) so the test is robust on slow CI,
	// but assert the flip actually happens.
	deadline := time.Now().Add(5 * time.Second)
	flipped := false
	for time.Now().Before(deadline) {
		got, err := authorizer.Enforce("auditor", "members", "write")
		if err != nil {
			t.Fatalf("Enforce(auditor,members,write) during poll: %v", err)
		}
		if got {
			flipped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !flipped {
		t.Error("Enforce(auditor,members,write) never flipped to allowed after the refresh interval")
	}

	// Sanity: a decision NOT touched by the new rule is unchanged after refresh.
	if got, err := authorizer.Enforce("auditor", "audit", "write"); err != nil {
		t.Fatalf("Enforce(auditor,audit,write) post-refresh: %v", err)
	} else if got {
		t.Error("Enforce(auditor,audit,write) = true post-refresh, want false (audit stays read-only)")
	}
}
