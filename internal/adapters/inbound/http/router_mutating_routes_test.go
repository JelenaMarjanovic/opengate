package http

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

// exemptMutatingRoutes is the EXPLICIT allow-list of registered mutating routes
// (POST/PUT/PATCH/DELETE) that are permitted to run WITHOUT the command idempotency
// middleware. Every other mutating route must be mounted behind it.
//
// A route belongs here only with a written reason, and only when idempotency
// protection is genuinely inapplicable — not merely inconvenient. Adding a command
// endpoint to this list to make the build pass is the failure this test exists to
// prevent.
//
// The map is keyed "METHOD path" exactly as chi.Walk reports it.
var exemptMutatingRoutes = map[string]string{
	// Login is PRE-authentication. The idempotency middleware reads the tenant and
	// the authenticated principal from context, neither of which exists yet at this
	// point in the chain, so mounting it here would fail closed with a 500 on every
	// login. Login also creates no domain command: a repeated login mints a new
	// session, which is the intended behavior, not a duplicate side effect.
	"POST /api/v1/tenants/{tenant}/auth/login": "pre-authentication: no tenant or principal in context yet",
	// Logout is exempt because an idempotency cache could not change what a retry
	// observes, and because a retry has no duplicate side effect to suppress.
	//
	// MEASURED, not assumed: a second POST to this route with the same cookie returns
	// 401, not 204 (TestAuthAPIIntegration/"AC-4 logout is 204, deletes the row, and
	// re-rejects the stale cookie" drives it end to end). The first call deleted the
	// session, so the session middleware fails to authenticate the now-stale cookie
	// and answers 401 before the handler — or an idempotency middleware mounted behind
	// it — is ever reached. A cached 204 would therefore be unreachable.
	//
	// Authenticator.Logout is genuinely idempotent (ErrSessionNotFound → nil), but that
	// is the use case, not the endpoint: the retry never gets there. The endpoint is
	// TOLERANT, not idempotent — it refuses a repeat rather than repeating the result.
	// The earlier version of this comment cited the use case's doc comment as evidence
	// for the endpoint's behavior, which is the same class of claim this project
	// rejects for schema and grants: a doc comment is not an observation.
	//
	// Exempt either way, since the retry is rejected upstream and logout deletes only
	// the caller's own session. TRACKED FOR E4: whether a retried logout SHOULD answer
	// 204 is a question about the auth surface's contract, not about idempotency, and
	// changing it belongs to its own story.
	"POST /api/v1/auth/logout": "a retry is rejected 401 by the session middleware before any cached response could apply",
}

// TestNoUnprotectedMutatingRoutes is a TRIPWIRE for the idempotency middleware's
// mounting contract. The middleware is currently mounted on no route — correctly,
// since no command endpoint exists yet — and its doc comment plus its fail-closed
// 500 document that contract. Neither of those fires in the case that actually
// matters: someone registers a mutating route in E4 and forgets the middleware.
// There is no 500 then, just a silently unprotected command endpoint.
//
// So the contract is made mechanical: every registered mutating route must appear
// in exemptMutatingRoutes. The list holds only the two pre-existing auth routes
// today, so the test passes now and fails the moment the first command route is
// registered — forcing a deliberate decision rather than a silent omission.
//
// Route enumeration uses chi.Walk (github.com/go-chi/chi/v5), which visits every
// registered method/pattern pair on the mux, including those inside sub-groups.
// NewRouter returns an http.Handler, so the test asserts it back to chi.Routes —
// the interface chi.Walk consumes and *chi.Mux implements.
func TestNoUnprotectedMutatingRoutes(t *testing.T) {
	// A Config with nil dependencies is enough: the test never issues a request, it
	// only inspects the routing table. The route set does not depend on these values.
	handler := NewRouter(Config{})

	routes, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter returned %T, which does not implement chi.Routes; "+
			"route enumeration is impossible and this tripwire cannot protect the mounting contract", handler)
	}

	var unprotected []string
	mutatingCount := 0
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !isMutatingMethod(method) {
			return nil
		}
		mutatingCount++
		key := method + " " + route
		if _, exempt := exemptMutatingRoutes[key]; !exempt {
			unprotected = append(unprotected, key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	if len(unprotected) > 0 {
		t.Errorf(`%d mutating route(s) are registered without an idempotency exemption: %v

To fix, for EACH route above:
  1. Mount the command idempotency middleware on it, AFTER the session middleware
     (the middleware reads the tenant and the principal that middleware puts in
     context; mounted earlier it fails closed with a 500):
         pr.Use(idempotencyMiddleware(cfg.IdempotencyStore, cfg.IdempotencyMetrics))
  2. Add IdempotencyStore and IdempotencyMetrics fields to Config (neither exists
     yet — no route needed them) and wire them at the composition root
     (cmd/opengate) to postgres.NewIdempotencyStore(<RLS-bound opengate_app pool>)
     and newIdempotencyMetrics(prometheus.DefaultRegisterer). The metrics handle
     may be nil, but then the fail-open paths are counted nowhere; wire it.
  3. Check whether the route ALSO needs requirePermission(cfg.Authorizer). That is
     US-02.04's contract, equally unmounted today and equally silent when omitted;
     this test names it because it is the same class of mistake, but enforcing it is
     out of scope here.
  4. Only if idempotency genuinely does not apply, add the route to
     exemptMutatingRoutes WITH a written reason.`, len(unprotected), unprotected)
	}

	t.Logf("route tripwire (chi.Walk): %d mutating route(s) registered, %d exempt by declaration, %d unprotected",
		mutatingCount, len(exemptMutatingRoutes), len(unprotected))
}

// TestMutatingRouteExemptionsAreRegistered keeps exemptMutatingRoutes honest in the
// other direction: an entry naming a route that no longer exists is stale, and a
// stale entry could silently exempt a future route that happens to reuse the path.
func TestMutatingRouteExemptionsAreRegistered(t *testing.T) {
	routes, ok := NewRouter(Config{}).(chi.Routes)
	if !ok {
		t.Fatal("NewRouter does not implement chi.Routes")
	}

	registered := make(map[string]bool)
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	for key, reason := range exemptMutatingRoutes {
		if !registered[key] {
			t.Errorf("exemptMutatingRoutes names %q (%s), which is not a registered route; remove the stale entry", key, reason)
		}
	}
}
