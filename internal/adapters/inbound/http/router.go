package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Health endpoint paths. Liveness and readiness are split per their distinct
// questions (process up vs. able to serve); see the handlers in health.go.
const (
	// LivenessPath answers "is the process up" without touching the database.
	LivenessPath = "/livez"
	// ReadinessPath answers "can the process serve requests" by pinging the DB.
	ReadinessPath = "/readyz"
)

// NewRouter builds the chi router for the api subcommand. This step mounts only
// the health endpoints; the authenticated HTTP surface (session middleware,
// login/logout, whoami) attaches here in US-02.03 Step 5b.
//
// pinger is the readiness probe's database handle — the BYPASS pool, which has
// no tenant-binding hooks, so the tenant-less readiness ping does not spam the
// regular pool's "no tenant in context" warning (see api.go for the full
// rationale). It returns an http.Handler so the composition root stays decoupled
// from chi.
func NewRouter(pinger Pinger) http.Handler {
	r := chi.NewRouter()
	r.Get(LivenessPath, handleLiveness)
	r.Get(ReadinessPath, handleReadiness(pinger))
	return r
}
