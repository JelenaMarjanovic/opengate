package http

import "net/http"

// Session-cookie naming. The base name identifies the cookie; in production it
// is hardened with the __Host- prefix (see sessionCookieName).
const (
	// sessionCookieBaseName is the cookie name used when the deployment is NOT
	// serving the cookie over HTTPS (i.e. the integration tests over httptest's
	// plain HTTP). It carries the opaque session token.
	sessionCookieBaseName = "opengate_session"

	// hostCookiePrefix is the RFC 6265bis __Host- prefix. A __Host--prefixed
	// cookie is accepted by browsers ONLY when it is Secure, has Path=/, and
	// carries no Domain attribute — which structurally prevents a subdomain (or a
	// network attacker who can forge a sibling host) from setting or overriding
	// our session cookie. We apply it in production (CookieSecure=true) where all
	// three preconditions hold.
	hostCookiePrefix = "__Host-"
)

// sessionCookieName returns the cookie name for the current Secure mode. The
// name is a FUNCTION of secure on purpose: the __Host- prefix mandates Secure,
// so it can only be used when we actually set Secure (production). The
// integration tests run secure=false over plain HTTP, where a __Host- cookie
// would be rejected by the client, so they fall back to the bare name. Every
// site that touches the cookie — the login handler (set), the logout handler
// (clear), and the session middleware (read) — derives the name through this one
// helper so the set/read names can never drift apart.
func sessionCookieName(secure bool) string {
	if secure {
		return hostCookiePrefix + sessionCookieBaseName
	}
	return sessionCookieBaseName
}

// setSessionCookie writes the freshly minted session token as the session
// cookie. The attributes implement System Design §9: HttpOnly (no script
// access), SameSite=Lax (sent on top-level navigations, blocked on cross-site
// subrequests — CSRF mitigation), Path=/ (visible to the whole API), and Secure
// per the deployment.
//
// It is deliberately a SESSION cookie — no Max-Age and no Expires. The server is
// the single source of truth for expiry via the sliding expires_at the refresh
// path maintains; the cookie therefore lives for the browser session and the
// server enforces the idle timeout. Encoding the lifetime in the cookie too
// would duplicate that authority and risk the two disagreeing.
func setSessionCookie(w http.ResponseWriter, value string, secure bool) {
	// G124: Secure is config-driven — true in production (HTTPS via Caddy), false
	// only for the plain-HTTP integration tests; HttpOnly and SameSite are always
	// set. The conditional Secure is the deliberate testability design, not a flaw.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is config-driven; HttpOnly+SameSite always set
		Name:     sessionCookieName(secure),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie expires the session cookie immediately (MaxAge < 0 emits
// Max-Age=0, instructing the client to delete it). The name, path, and security
// attributes mirror setSessionCookie so the client matches and removes the same
// cookie. The empty value ensures nothing usable lingers even if a client
// ignores Max-Age.
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	// G124: see setSessionCookie — Secure is config-driven; the other attributes
	// mirror the set path so the client matches and deletes the same cookie.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is config-driven; HttpOnly+SameSite always set
		Name:     sessionCookieName(secure),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
