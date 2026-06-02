package http

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/application/auth"
)

// Route paths for the auth surface (System Design §9). Login is tenant-scoped by
// a path slug — the tenant is resolved pre-authentication from the URL, not from
// a session that does not exist yet. Logout and whoami are tenant-agnostic in the
// URL: their tenant comes from the authenticated session.
const (
	// LoginPath is the pre-authentication login endpoint. {tenant} is the slug.
	LoginPath = "/api/v1/tenants/{tenant}/auth/login"
	// LogoutPath is the protected logout endpoint.
	LogoutPath = "/api/v1/auth/logout"
	// WhoamiPath is the protected identity endpoint.
	WhoamiPath = "/api/v1/auth/whoami"
)

// maxLoginBodyBytes caps the login request body. An {email, password} JSON is a
// few hundred bytes at most; the cap turns an oversized or never-ending body
// into a fast validation failure instead of unbounded memory.
const maxLoginBodyBytes = 4 << 10 // 4 KiB

// loginRequest is the JSON body of a login request.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginResponse is the JSON body of a successful login. It conveys
// must_change_password so the dashboard can route a forced password change, and
// expires_at (informational) so the client knows the initial idle deadline. It
// carries NO secret: the session token travels only in the HttpOnly cookie.
type loginResponse struct {
	MustChangePassword bool      `json:"must_change_password"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// whoamiResponse is the JSON body of GET /whoami: the non-secret identity fields
// the dashboard needs. It deliberately omits the session id and every secret.
type whoamiResponse struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
	TenantID string `json:"tenant_id"`
}

// handleLogin serves POST /api/v1/tenants/{tenant}/auth/login (public,
// pre-authentication). It resolves the tenant from the path slug, validates the
// body, calls the Login use case, and on success sets the session cookie and
// returns 200. Every credential failure is the uniform 401 from
// auth.ErrInvalidCredentials — the enumeration defense reaches the HTTP layer.
func handleLogin(authn Authenticator, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "tenant")

		var body loginRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBodyBytes))
		if err := dec.Decode(&body); err != nil {
			// Malformed, oversized, or absent JSON. This is a body-level validation
			// failure (422), not a credential outcome, so detail is safe to reveal.
			WriteValidationProblem(w, r, []FieldError{{
				Pointer: "",
				Code:    "malformed",
				Detail:  "Request body must be a valid JSON object with email and password.",
			}})
			return
		}

		if fieldErrors := validateLogin(body); len(fieldErrors) > 0 {
			WriteValidationProblem(w, r, fieldErrors)
			return
		}

		result, err := authn.Login(r.Context(), auth.LoginParams{
			Slug:      slug,
			Email:     body.Email,
			Password:  body.Password,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
		if err != nil {
			// auth.ErrInvalidCredentials → 401 (uniform); apperr.ErrInternal → 500.
			WriteProblem(w, r, err)
			return
		}

		// Set-Cookie BEFORE writing the status line (headers must precede the body).
		setSessionCookie(w, result.Token, secure)
		writeJSON(w, r, http.StatusOK, loginResponse{
			MustChangePassword: result.MustChangePassword,
			ExpiresAt:          result.ExpiresAt,
		})
	}
}

// validateLogin returns the field-level errors for a login body: email and
// password must both be present and non-empty. It is intentionally the ONLY
// place these are checked at the HTTP layer — the credential decision itself
// stays uniform in the use case. Password is checked for exact emptiness (never
// trimmed: a password is opaque bytes), email is trimmed for the presence check.
func validateLogin(body loginRequest) []FieldError {
	var fieldErrors []FieldError
	if strings.TrimSpace(body.Email) == "" {
		fieldErrors = append(fieldErrors, FieldError{
			Pointer: "/email", Code: "required", Detail: "Email is required.",
		})
	}
	if body.Password == "" {
		fieldErrors = append(fieldErrors, FieldError{
			Pointer: "/password", Code: "required", Detail: "Password is required.",
		})
	}
	return fieldErrors
}

// handleLogout serves POST /api/v1/auth/logout (protected). The middleware has
// already authenticated the request and set both the tenant context (required by
// the regular-pool Delete) and the Principal. It deletes the session (idempotent
// in the use case), clears the cookie, and returns 204.
//
// Note on the moot refresh: because logout sits behind the refresh-on-every-
// request middleware, the session's window is slid forward an instant before the
// handler deletes it. That write is harmless and not worth special-casing.
func handleLogout(authn Authenticator, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		if !ok {
			// Unreachable behind the middleware; a 500 surfaces the programming error.
			WriteProblem(w, r, fmt.Errorf("logout: no principal in context: %w", apperr.ErrInternal))
			return
		}

		if err := authn.Logout(r.Context(), principal.SessionID); err != nil {
			WriteProblem(w, r, err) // wrapped apperr.ErrInternal → 500
			return
		}

		clearSessionCookie(w, secure)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleWhoami serves GET /api/v1/auth/whoami (protected). It echoes the
// non-secret identity from the Principal the middleware placed in context. It is
// the authenticated endpoint AC-2 and AC-3 exercise, and never exposes the
// session id or any secret.
func handleWhoami(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		WriteProblem(w, r, fmt.Errorf("whoami: no principal in context: %w", apperr.ErrInternal))
		return
	}
	writeJSON(w, r, http.StatusOK, whoamiResponse{
		UserID:   principal.UserID.String(),
		Role:     string(principal.Role),
		TenantID: principal.TenantID.String(),
	})
}

// clientIP extracts the originating client IP for the session's forensic record.
//
// Trusted-proxy assumption: Caddy terminates TLS in front of the app (US-01.03),
// so r.RemoteAddr is the proxy and the real client is the FIRST entry of
// X-Forwarded-For. This is only sound because the app is never exposed directly;
// were it reachable without the proxy, X-Forwarded-For would be client-spoofable
// and must not be trusted. We prefer the XFF client, fall back to RemoteAddr, and
// return the zero Addr if neither parses — the use case maps a zero IP to SQL NULL.
func clientIP(r *http.Request) netip.Addr {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if addr, err := netip.ParseAddr(strings.TrimSpace(first)); err == nil {
			return addr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr // RemoteAddr may already be a bare host (e.g. in some tests)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr
	}
	return netip.Addr{}
}

// writeJSON marshals payload and writes it with the given status and the JSON
// content type. A marshal failure (not expected for these small fixed structs)
// renders a generic 500 via WriteProblem. The body write is best-effort: the
// status is already committed by then.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		WriteProblem(w, r, fmt.Errorf("marshal response: %w: %w", apperr.ErrInternal, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
