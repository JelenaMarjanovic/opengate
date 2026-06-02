package http

import (
	"context"
	"errors"
	"net/http"

	"github.com/JelenaMarjanovic/opengate/internal/application/auth"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// principalCtxKey is the unexported context key under which the session
// middleware stores the authenticated Principal. Being unexported and a distinct
// struct type, it cannot collide with any other context value (including the
// tenant package's own key).
type principalCtxKey struct{}

// contextWithPrincipal returns a copy of ctx carrying the authenticated
// Principal. The middleware calls it once a session validates.
func contextWithPrincipal(ctx context.Context, p auth.Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the authenticated Principal placed in ctx by the
// session middleware, and true if present. Protected handlers (whoami, logout)
// read identity through it. It returns false only when called outside the
// protected group — a programming error the handlers translate to a 500.
func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(auth.Principal)
	return p, ok
}

// sessionMiddleware guards the protected route group. For every request it runs
// the security spine of US-02.03 — authenticate, set context, refresh, proceed —
// in exactly that order:
//
//  1. Extract the session cookie. No cookie means no session: respond 401
//     (ErrSessionInvalid) without calling Authenticate or the next handler.
//  2. Authenticate the opaque token. On failure respond via WriteProblem (which
//     maps ErrSessionInvalid→401, ErrTenantSuspended→403, internal→500) and stop
//     — crucially WITHOUT calling Refresh, so an expired/invalid session never
//     touches last_seen_at (AC-2).
//  3. On success, build a context carrying BOTH the tenant and the Principal,
//     THEN call Refresh on that tenant-bearing context, THEN call next. The
//     ordering is mandatory: Refresh runs on the regular RLS-bound pool whose
//     adapter reads the tenant via tenant.FromContext, which PANICS if the tenant
//     is absent. Setting the tenant before Refresh turns that panic from a live
//     failure mode into a pure programming-error guard (and middleware.Recoverer
//     backstops it regardless). A valid session updates last_seen_at (AC-3).
//
// secure selects the cookie name (the __Host- prefix in production); it must
// match the value the login handler used to set the cookie.
func sessionMiddleware(authn Authenticator, secure bool) func(http.Handler) http.Handler {
	cookieName := sessionCookieName(secure)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(cookieName)
			if err != nil {
				// http.ErrNoCookie is the only error r.Cookie returns; either way the
				// request carries no session and is unauthenticated.
				WriteProblem(w, r, auth.ErrSessionInvalid)
				return
			}

			principal, err := authn.Authenticate(r.Context(), cookie.Value)
			if err != nil {
				// Maps to 401 (ErrSessionInvalid), 403 (ErrTenantSuspended) or 500
				// (wrapped apperr.ErrInternal). No Refresh on this path — AC-2.
				WriteProblem(w, r, err)
				return
			}

			// Tenant FIRST, then principal — both before Refresh (see step 3 above).
			ctx := tenant.NewContext(r.Context(), tenant.ID(principal.TenantID))
			ctx = contextWithPrincipal(ctx, principal)

			if err := authn.Refresh(ctx, principal.SessionID, principal.SessionTimeout); err != nil {
				if errors.Is(err, ports.ErrSessionNotFound) {
					// The session vanished between validation and refresh (e.g. a
					// concurrent logout or cleanup): treat the request as unauthenticated.
					WriteProblem(w, r, auth.ErrSessionInvalid)
					return
				}
				WriteProblem(w, r, err) // wrapped apperr.ErrInternal → 500
				return
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
