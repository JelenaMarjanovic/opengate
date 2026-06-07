package http

import (
	"fmt"
	"net/http"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
)

// Authorizer is the subset of the outbound authorization adapter the HTTP surface
// depends on: a single permission decision for a (role, resource, action) triple.
// It is defined here, next to its sole consumer (requirePermission), as Go idiom
// prefers — depending on this seam rather than authz.*CasbinAuthorizer keeps the
// middleware unit-testable with a fake and documents exactly what the inbound
// layer needs. *authz.CasbinAuthorizer satisfies it structurally; the composition
// root injects it via Config.Authorizer.
//
// The error return is distinct from a deny: (false, nil) means "denied", whereas
// (_, non-nil) means "could not decide" — an enforcer fault, not a verdict. The
// middleware fails CLOSED on the latter (500), never running the protected handler.
type Authorizer interface {
	Enforce(role, resource, action string) (bool, error)
}

// requirePermission binds the per-route authorization middleware to an Authorizer,
// returning a `require(resource, action)` factory the router uses to declare each
// protected route's required permission — mirroring how sessionMiddleware is bound
// to the Authenticator (a closure created once in the router setup, capturing the
// dependency):
//
//	require := requirePermission(cfg.Authorizer)
//	r.With(require("members", "write")).Put("/.../members/{id}", handler)
//
// The returned middleware runs AFTER the session middleware — it reads the
// authenticated principal that middleware placed in context — and decides on the
// principal's snapshotted role (US-02.03), adding NO database call: it consults the
// in-memory enforcer through the injected interface.
//
//   - principal absent → 500. The route was placed outside the session-middleware
//     group, a routing misconfiguration rather than a client error. An explicit
//     check is cleaner than relying on the nil-principal path to panic (which
//     middleware.Recoverer would still backstop into a logged 500).
//   - enforce error → 500, fail-closed. The verdict is unknown, so the protected
//     handler must never run.
//   - not allowed → 403 (apperr.ErrForbidden), rendered with a generic body that
//     never names the denied (resource, action).
//   - allowed → the protected handler runs.
func requirePermission(authorizer Authorizer) func(resource, action string) func(http.Handler) http.Handler {
	return func(resource, action string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				principal, ok := PrincipalFromContext(r.Context())
				if !ok {
					// Authorization applied to a route the session middleware does not
					// guard: a programming error, surfaced as a 500 (never as a client
					// 4xx). The handler is not reached.
					WriteProblem(w, r, fmt.Errorf(
						"authz: no principal in context (route not behind the session middleware): %w",
						apperr.ErrInternal))
					return
				}

				allowed, err := authorizer.Enforce(string(principal.Role), resource, action)
				switch {
				case err != nil:
					// Fail-closed: an enforce error means we could not determine
					// authorization, so never run the protected handler. Maps to 500;
					// the (resource, action) stay in the server log, never the body.
					WriteProblem(w, r, fmt.Errorf(
						"authz: enforce %q/%q failed: %w: %w",
						resource, action, apperr.ErrInternal, err))
				case !allowed:
					// Generic 403 — the body never names the denied (resource, action).
					WriteProblem(w, r, apperr.ErrForbidden)
				default:
					next.ServeHTTP(w, r)
				}
			})
		}
	}
}
