package http

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Pinger is the narrow capability the readiness probe depends on: a
// context-bounded round-trip to the database. Depending on this seam rather than
// on *pgxpool.Pool keeps the handler unit-testable with a fake and documents
// that readiness needs nothing more than reachability. *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// readinessPingTimeout bounds the readiness DB round-trip. A readiness probe
// must answer quickly; if the database does not respond within this window the
// process is treated as not ready rather than the probe hanging.
const readinessPingTimeout = 2 * time.Second

// okBody is the tiny shared payload for healthy 200 responses.
var okBody = []byte(`{"status":"ok"}`)

// handleLiveness answers "is the process up". It returns 200 unconditionally and
// MUST NOT touch the database: a DB outage must not fail liveness, or an
// orchestrator would kill an otherwise-healthy process. The request is unused.
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeOK(w)
}

// handleReadiness answers "can the process serve requests", for which database
// reachability is a precondition. It pings pinger (the BYPASS pool — see api.go)
// under a short timeout, returning 200 on success and a Problem Details 503 on
// failure. The ping error is reported to WriteProblem for server-side logging
// only; the client sees the static, generic 503 detail.
func handleReadiness(pinger Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessPingTimeout)
		defer cancel()

		if err := pinger.Ping(ctx); err != nil {
			// Wrap errNotReady (→ 503 in the mapping table) AND the underlying ping
			// error: errors.Is finds errNotReady for the status, and err.Error() is
			// logged server-side. The body uses only the static 503 detail.
			WriteProblem(w, r, fmt.Errorf("readiness: database ping failed: %w: %w", errNotReady, err))
			return
		}
		writeOK(w)
	}
}

// writeOK writes a 200 with the tiny JSON health body. Best-effort on the write:
// the status is already committed, so a write failure is not actionable here.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(okBody)
}
