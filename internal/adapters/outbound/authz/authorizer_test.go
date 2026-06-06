package authz_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/authz"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
)

// seededRules is a representative slice of the v1 seed (the full set lives in the
// casbin_rules migration). It is enough to assert the matcher semantics the model
// encodes without a database: the owner wildcard, the manager's read/write split
// with audit read-only, and the auditor's read-only access.
func seededRules() [][]string {
	return [][]string{
		{"owner", "*", "*"},
		{"manager", "members", "read"},
		{"manager", "members", "write"},
		{"manager", "audit", "read"},
		{"auditor", "members", "read"},
		{"auditor", "credentials", "read"},
		{"auditor", "audit", "read"},
	}
}

// fakeLoader returns a PolicyLoaderFunc that always yields the given rules. It is
// the authz-layer analog of US-02.03's injected fakes: no container needed to
// assert enforce decisions.
func fakeLoader(rules [][]string) authz.PolicyLoaderFunc {
	return func(context.Context) ([][]string, error) { return rules, nil }
}

func discardLogger() *slog.Logger {
	return observability.NewLogger(io.Discard, slog.LevelError)
}

// newTestAuthorizer builds an authorizer over a fake loader, failing the test if
// construction (which does the fail-fast initial load) errors.
func newTestAuthorizer(t *testing.T, rules [][]string) *authz.CasbinAuthorizer {
	t.Helper()
	a, err := authz.NewCasbinAuthorizer(fakeLoader(rules), authz.ModelText, time.Minute, discardLogger())
	if err != nil {
		t.Fatalf("NewCasbinAuthorizer: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// TestEnforceDecisions asserts the matcher semantics the seed encodes, with a
// fake loader and no container. Each case is (role, resource, action) -> want.
func TestEnforceDecisions(t *testing.T) {
	t.Parallel()
	a := newTestAuthorizer(t, seededRules())

	cases := []struct {
		name                   string
		role, resource, action string
		want                   bool
	}{
		// The owner's `*, *` rule matches ANY (resource, action), including a
		// resource/action named in no rule at all — that is the whole point of the
		// wildcard, and the matcher's `p.obj == "*" || ...` clauses encode it.
		{"owner reads members", "owner", "members", "read", true},
		{"owner writes audit (no explicit rule)", "owner", "audit", "write", true},
		{"owner does an unnamed action on an unnamed resource", "owner", "billing", "delete", true},

		// The manager may read and write members but audit is READ-ONLY in the
		// seed: there is a (manager, audit, read) rule and no (manager, audit, write).
		{"manager writes members", "manager", "members", "write", true},
		{"manager reads audit", "manager", "audit", "read", true},
		{"manager writes audit (denied: audit is read-only)", "manager", "audit", "write", false},

		// The auditor is read-only: members read is allowed, members write is not.
		{"auditor reads members", "auditor", "members", "read", true},
		{"auditor writes members (denied)", "auditor", "members", "write", false},

		// An unknown role matches no `p.sub`, so every request is denied.
		{"unknown role reads members", "intern", "members", "read", false},
		{"unknown role writes anything", "intern", "doors", "write", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.Enforce(tc.role, tc.resource, tc.action)
			if err != nil {
				t.Fatalf("Enforce(%q,%q,%q): %v", tc.role, tc.resource, tc.action, err)
			}
			if got != tc.want {
				t.Errorf("Enforce(%q,%q,%q) = %v, want %v",
					tc.role, tc.resource, tc.action, got, tc.want)
			}
		})
	}
}

// TestNewCasbinAuthorizerFailsFastOnLoadError proves the constructor refuses to
// build an authorizer whose initial policy load errors — the process must not
// start with an authorizer that cannot authorize.
func TestNewCasbinAuthorizerFailsFastOnLoadError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("loader boom")
	loader := func(context.Context) ([][]string, error) { return nil, sentinel }

	a, err := authz.NewCasbinAuthorizer(loader, authz.ModelText, time.Minute, discardLogger())
	if err == nil {
		t.Fatal("NewCasbinAuthorizer: want error on failing initial load, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the loader error", err)
	}
	if a != nil {
		t.Errorf("authorizer = %v, want nil on failed construction", a)
	}
}

// TestEnforceFailClosedOnEmptyPolicy proves the fail-closed default: an initial
// load that yields ZERO rules is not an error (the constructor succeeds), but the
// resulting empty enforcer denies everything.
func TestEnforceFailClosedOnEmptyPolicy(t *testing.T) {
	t.Parallel()
	a := newTestAuthorizer(t, [][]string{})

	got, err := a.Enforce("owner", "members", "read")
	if err != nil {
		t.Fatalf("Enforce on empty policy: %v", err)
	}
	if got {
		t.Error("Enforce(owner,members,read) = true on empty policy, want false (fail-closed)")
	}
}

// TestNewCasbinAuthorizerRejectsNilLoader guards the obvious programmer error: a
// nil loader can never authorize, so construction must refuse it.
func TestNewCasbinAuthorizerRejectsNilLoader(t *testing.T) {
	t.Parallel()
	if _, err := authz.NewCasbinAuthorizer(nil, authz.ModelText, time.Minute, discardLogger()); err == nil {
		t.Fatal("NewCasbinAuthorizer(nil loader): want error, got nil")
	}
}
