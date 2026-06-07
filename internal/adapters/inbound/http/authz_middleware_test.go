package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/application/auth"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
)

// fakeAuthorizer is a programmable Authorizer for the middleware-logic tests: it
// returns a fixed (allowed, err) and records the last call, so a test can assert
// the middleware forwards the principal's role and the route's (resource, action)
// — all without a database or a real Casbin enforcer. It is the authz-layer analog
// of US-02.03's injected fakes (VerifierFunc/HasherFunc).
type fakeAuthorizer struct {
	allowed bool
	err     error

	calls                           int
	gotRole, gotResource, gotAction string
}

func (f *fakeAuthorizer) Enforce(role, resource, action string) (bool, error) {
	f.calls++
	f.gotRole, f.gotResource, f.gotAction = role, resource, action
	return f.allowed, f.err
}

// injectPrincipal is a test middleware mimicking the one thing the session
// middleware does that the authorization middleware depends on: it places an
// authenticated Principal carrying the given role in the request context. Only the
// role matters to authorization, so the other Principal fields are left zero. This
// is exactly the "inject the principal directly rather than driving the full
// cookie→session path" the step calls for (US-02.03 already covers that path).
func injectPrincipal(role domain.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := contextWithPrincipal(r.Context(), auth.Principal{Role: role})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// serveRequireChain builds the chain a protected route would have — an OPTIONAL
// principal injector (nil role ⇒ omitted, modeling a route mistakenly placed
// outside the session middleware), then require(resource, action) — in front of a
// stub handler that records whether it ran and writes 200. It returns the recorder
// and whether the stub executed, so the fail-closed assertions can prove the stub
// never ran.
func serveRequireChain(t *testing.T, authorizer Authorizer, injectRole *domain.Role, resource, action string) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	stubRan := false
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stubRan = true
		w.WriteHeader(http.StatusOK)
	})

	require := requirePermission(authorizer)
	handler := require(resource, action)(stub)
	if injectRole != nil {
		handler = injectPrincipal(*injectRole)(handler)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	handler.ServeHTTP(rec, req)
	return rec, stubRan
}

// decodeProblemBody decodes an RFC 9457 body, failing the test if it is not JSON.
func decodeProblemBody(t *testing.T, raw string) ProblemDetails {
	t.Helper()
	var pd ProblemDetails
	if err := json.Unmarshal([]byte(raw), &pd); err != nil {
		t.Fatalf("response body is not JSON: %v\nbody: %s", err, raw)
	}
	return pd
}

// TestRequirePermissionDenied asserts a denying decision yields a generic 403:
// the problem+json content type, body status 403, the static "Forbidden" title,
// and — the security crux — a body that names NEITHER the resource NOR the action
// that was denied, so a denial does not disclose the authorization model. The stub
// must not run.
func TestRequirePermissionDenied(t *testing.T) {
	role := domain.RoleAuditor
	authorizer := &fakeAuthorizer{allowed: false}

	rec, stubRan := serveRequireChain(t, authorizer, &role, "members", "write")

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeProblemJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblemJSON)
	}
	if stubRan {
		t.Error("protected stub ran on a denied request")
	}

	raw := rec.Body.String()
	pd := decodeProblemBody(t, raw)
	if pd.Status != http.StatusForbidden {
		t.Errorf("body status = %d, want 403", pd.Status)
	}
	if pd.Title != "Forbidden" {
		t.Errorf("body title = %q, want %q", pd.Title, "Forbidden")
	}
	// The denied (resource, action) must never appear anywhere in the body.
	if strings.Contains(raw, "members") || strings.Contains(raw, "write") {
		t.Errorf("403 body disclosed the denied resource/action:\n%s", raw)
	}
	// Sanity: the middleware consulted the enforcer with the principal's role and the
	// route's permission exactly once.
	if authorizer.calls != 1 || authorizer.gotRole != "auditor" ||
		authorizer.gotResource != "members" || authorizer.gotAction != "write" {
		t.Errorf("Enforce called %d time(s) with (%q,%q,%q), want 1 with (auditor,members,write)",
			authorizer.calls, authorizer.gotRole, authorizer.gotResource, authorizer.gotAction)
	}
}

// TestRequirePermissionAllowed asserts an allowing decision runs the stub and
// yields 200, and that the middleware forwarded the principal's role and the
// route's (resource, action) to the enforcer.
func TestRequirePermissionAllowed(t *testing.T) {
	role := domain.RoleOwner
	authorizer := &fakeAuthorizer{allowed: true}

	rec, stubRan := serveRequireChain(t, authorizer, &role, "members", "write")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !stubRan {
		t.Error("protected stub did not run on an allowed request")
	}
	if authorizer.gotRole != "owner" || authorizer.gotResource != "members" || authorizer.gotAction != "write" {
		t.Errorf("Enforce got (%q,%q,%q), want (owner,members,write)",
			authorizer.gotRole, authorizer.gotResource, authorizer.gotAction)
	}
}

// TestRequirePermissionEnforceErrorFailsClosed asserts the fail-closed contract
// for an enforcer fault: an Enforce error yields 500 (not a 403), the stub does
// NOT run, and the underlying error string never leaks into the generic body.
func TestRequirePermissionEnforceErrorFailsClosed(t *testing.T) {
	const enforceErrText = "enforcer matcher is broken"
	role := domain.RoleManager
	authorizer := &fakeAuthorizer{err: errors.New(enforceErrText)}

	rec, stubRan := serveRequireChain(t, authorizer, &role, "members", "write")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (fail-closed on enforce error)", rec.Code)
	}
	if stubRan {
		t.Error("fail-closed violated: protected stub ran despite an enforce error")
	}
	raw := rec.Body.String()
	pd := decodeProblemBody(t, raw)
	if pd.Status != http.StatusInternalServerError {
		t.Errorf("body status = %d, want 500", pd.Status)
	}
	if pd.Title != "Internal server error" {
		t.Errorf("body title = %q, want the generic internal title", pd.Title)
	}
	if strings.Contains(raw, enforceErrText) {
		t.Errorf("enforce error leaked into the response body:\n%s", raw)
	}
}

// TestRequirePermissionMissingPrincipalFailsClosed asserts that a route guarded by
// require(...) but NOT behind the session middleware (so no principal in context)
// is treated as an internal error (500), never running the stub — and that the
// enforcer is not even consulted, since there is no role to authorize.
func TestRequirePermissionMissingPrincipalFailsClosed(t *testing.T) {
	// allowed:true so that, were the principal check skipped, the request would wrongly
	// succeed — making the 500 a real proof the missing-principal guard fired first.
	authorizer := &fakeAuthorizer{allowed: true}

	rec, stubRan := serveRequireChain(t, authorizer, nil, "members", "read")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (missing principal is a routing misconfiguration)", rec.Code)
	}
	if stubRan {
		t.Error("protected stub ran with no principal in context")
	}
	if authorizer.calls != 0 {
		t.Errorf("Enforce was called %d time(s) with no principal; want 0 (no role to authorize)", authorizer.calls)
	}
}
