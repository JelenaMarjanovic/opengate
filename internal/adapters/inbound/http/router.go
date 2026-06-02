package http

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/application/auth"
)

// Health endpoint paths. Liveness and readiness are split per their distinct
// questions (process up vs. able to serve); see the handlers in health.go.
const (
	// LivenessPath answers "is the process up" without touching the database.
	LivenessPath = "/livez"
	// ReadinessPath answers "can the process serve requests" by pinging the DB.
	ReadinessPath = "/readyz"
)

// Authenticator is the subset of the application-layer *auth.Authenticator the
// HTTP surface depends on. Depending on this seam (not the concrete type) keeps
// the middleware and handlers unit-testable with a fake and documents exactly
// which use-case methods the adapter calls. *auth.Authenticator satisfies it.
type Authenticator interface {
	// Login is the pre-authentication credential check + session mint.
	Login(ctx context.Context, params auth.LoginParams) (auth.LoginResult, error)
	// Authenticate validates an opaque session token into a Principal.
	Authenticate(ctx context.Context, token string) (auth.Principal, error)
	// Refresh slides the session's idle window; runs post-authentication on the
	// regular pool, so the caller must set the tenant context first.
	Refresh(ctx context.Context, sessionID uuid.UUID, sessionTimeout time.Duration) error
	// Logout deletes the session (idempotent); post-authentication, regular pool.
	Logout(ctx context.Context, sessionID uuid.UUID) error
}

// Config is the dependency set the api router needs. It is assembled at the
// composition root (cmd/opengate) — the only place that may import both the
// adapters and the application layer.
type Config struct {
	// Pinger is the readiness probe's database handle: the BYPASS pool, which has
	// no tenant-binding hooks, so the tenant-less readiness ping does not spam the
	// regular pool's "no tenant in context" warning (see api.go for the rationale).
	Pinger Pinger
	// Authenticator is the auth use case backing login/logout/whoami and the
	// session middleware.
	Authenticator Authenticator
	// CookieSecure marks the session cookie Secure and applies the __Host- name
	// prefix. True in production (HTTPS via Caddy); false only for the plain-HTTP
	// integration tests. See cookie.go.
	CookieSecure bool
}

// NewRouter builds the chi router for the api subcommand: the health probes plus
// the US-02.03 auth surface. It returns an http.Handler so the composition root
// stays decoupled from chi.
//
// Three route groups:
//
//   - Cross-cutting middleware (all routes): RequestID then Recoverer. RequestID
//     stamps each request with a correlatable id (surfaced to the panic logger and
//     available for future log/Problem-Details correlation); Recoverer converts a
//     stray panic into a logged 500 with a stack trace rather than crashing the
//     process. Recoverer also backstops the session middleware's tenant-panic
//     guard: were a post-auth port ever reached without a tenant context,
//     tenant.FromContext would panic and Recoverer would turn it into a logged 500
//     — the bug still surfaces in logs, but one bad request does not take the
//     server down. RequestID is registered before Recoverer so the recovered
//     panic log carries the id.
//   - Public (no session middleware): the health endpoints and login. Login is
//     pre-authentication — it must not sit behind the session middleware.
//   - Protected (behind the session middleware): logout and whoami. The middleware
//     establishes the authenticated session and tenant context they require.
func NewRouter(cfg Config) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	// Public routes.
	r.Get(LivenessPath, handleLiveness)
	r.Get(ReadinessPath, handleReadiness(cfg.Pinger))
	r.Post(LoginPath, handleLogin(cfg.Authenticator, cfg.CookieSecure))

	// Protected routes: the session middleware authenticates, sets the tenant +
	// principal context, and refreshes the window before the handler runs.
	r.Group(func(pr chi.Router) {
		pr.Use(sessionMiddleware(cfg.Authenticator, cfg.CookieSecure))
		pr.Post(LogoutPath, handleLogout(cfg.Authenticator, cfg.CookieSecure))
		pr.Get(WhoamiPath, handleWhoami)
	})

	return r
}
