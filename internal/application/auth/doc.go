// Package auth holds the dashboard authentication use cases: Login (credential
// verification and session minting), Authenticate (per-request, read-only
// session validation), Refresh (sliding-window expiry) and Logout (idempotent
// session deletion). It is the application-layer orchestration over the outbound
// ports built in US-02.03 Steps 3–4a; it owns no HTTP, no cookies, and no SQL.
//
// The pre/post-authentication pool split is inherited through the adapters, not
// re-implemented here: tenant resolution, user lookup, the user mutations,
// session create, and session find-by-token run pre-authentication on the
// bypass pool with explicit arguments; session refresh and delete run
// post-authentication on the RLS-bound pool with the tenant read from context.
// These use cases simply call the right port; the middleware (Step 5) sets the
// tenant context before the post-authentication calls.
//
// Import constraint: this package may import internal/domain, internal/ports,
// internal/apperr, and internal/tenant. It must NOT import internal/adapters,
// and it must NOT call internal/auth directly — the password verifier, hasher,
// clock, and token source are injected so the use case is deterministically
// testable; cmd/opengate wires the concrete implementations.
package auth
