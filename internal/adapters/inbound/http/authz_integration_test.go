package http

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/authz"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestAuthzMiddlewareIntegration exercises the per-route authorization middleware
// against the REAL authorizer over the seeded v1 policy on a testcontainers
// Postgres, closing the three acceptance criteria at the HTTP boundary. It injects
// the principal directly (a small middleware standing in for the session middleware
// US-02.03 already covers end to end) so the test isolates the authorization seam:
// the role in context, the require(...) gate, the static 403/200 outcomes, and the
// refresh loop's effect on a live decision.
func TestAuthzMiddlewareIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startAuthzPostgres(ctx, t)
	bypassPool := openTestPool(ctx, t, bypassRoleDSN(ctx, t, container))
	superPool := openTestPool(ctx, t, superConnString(ctx, t, container))

	logger := observability.NewLogger(io.Discard, slog.LevelError)
	loader := postgres.NewCasbinPolicyLoader(bypassPool, logger)

	// A short refresh interval so AC3 does not wait long for the policy swap.
	const refreshInterval = 100 * time.Millisecond
	authorizer, err := authz.NewCasbinAuthorizer(loader.Load, authz.ModelText, refreshInterval, logger)
	if err != nil {
		t.Fatalf("NewCasbinAuthorizer: %v", err)
	}
	authorizer.Start(ctx)
	t.Cleanup(authorizer.Close)

	// AC1: an auditor may NOT write members — the seed grants the auditor read-only.
	// The denial is a generic 403 whose body names neither the resource nor the action.
	t.Run("AC1 auditor members/write is forbidden", func(t *testing.T) {
		code, body := hitProbe(t, newProbeRouter(authorizer, domain.RoleAuditor, "members", "write"))
		if code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body: %s", code, body)
		}
		pd := decodeProblemBody(t, body)
		if pd.Status != http.StatusForbidden || pd.Title != "Forbidden" {
			t.Errorf("problem = {status:%d title:%q}, want {403, Forbidden}", pd.Status, pd.Title)
		}
		if strings.Contains(body, "members") || strings.Contains(body, "write") {
			t.Errorf("403 body disclosed the denied resource/action:\n%s", body)
		}
	})

	// AC2: an owner may write members via the seed's wildcard (owner, *, *) → 200.
	t.Run("AC2 owner members/write is allowed", func(t *testing.T) {
		if code, body := hitProbe(t, newProbeRouter(authorizer, domain.RoleOwner, "members", "write")); code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", code, body)
		}
	})

	// The middleware passes legitimate access through: an auditor may READ members.
	t.Run("auditor members/read is allowed", func(t *testing.T) {
		if code, body := hitProbe(t, newProbeRouter(authorizer, domain.RoleAuditor, "members", "read")); code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", code, body)
		}
	})

	// AC3: with the refresh loop running, granting (auditor, members, write) via an
	// out-of-band superuser INSERT flips the previously-403 auditor request to 200
	// once the refresh interval elapses — the HTTP-level confirmation of the refresh
	// that step 2 proved at the authorizer level.
	t.Run("AC3 policy refresh flips the denied request to allowed", func(t *testing.T) {
		h := newProbeRouter(authorizer, domain.RoleAuditor, "members", "write")

		// Precondition: still denied before the grant.
		if code, body := hitProbe(t, h); code != http.StatusForbidden {
			t.Fatalf("pre-grant status = %d, want 403; body: %s", code, body)
		}

		// The runtime grant goes through the SUPERUSER pool: the policy is
		// migration-managed and no application role (not even opengate_bypass) holds
		// INSERT on casbin_rules, so simulating a policy change as an out-of-band
		// superuser mutation proves the loop observes a change made entirely outside
		// the application's own privileges.
		if _, err := superPool.Exec(ctx,
			`INSERT INTO casbin_rules (ptype, v0, v1, v2) VALUES ('p', 'auditor', 'members', 'write')`,
		); err != nil {
			t.Fatalf("insert policy rule: %v", err)
		}

		// Poll until the refresh loop re-loads and the request flips to 200. A
		// generous deadline (many refresh intervals) keeps it robust on slow CI while
		// still asserting the flip actually happens.
		deadline := time.Now().Add(5 * time.Second)
		lastCode, lastBody := http.StatusForbidden, ""
		for time.Now().Before(deadline) {
			if lastCode, lastBody = hitProbe(t, h); lastCode == http.StatusOK {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if lastCode != http.StatusOK {
			t.Errorf("auditor members/write never flipped to 200 after the refresh; last = %d body: %s", lastCode, lastBody)
		}
	})
}

// newProbeRouter builds a one-route chi router mirroring a production protected
// resource route: the principal injector (standing in for the session middleware)
// followed by require(resource, action), guarding a stub that writes 200. Using
// chi's .With proves the require closure composes with chi exactly as the route
// declarations in later epics will read (r.With(require("members","write")).Put(...)).
func newProbeRouter(authorizer Authorizer, role domain.Role, resource, action string) http.Handler {
	require := requirePermission(authorizer)
	r := chi.NewRouter()
	r.With(injectPrincipal(role), require(resource, action)).Get("/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return r
}

// hitProbe issues GET /probe against h and returns the status and body.
func hitProbe(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// --- Container + pool helpers ----------------------------------------------

// startAuthzPostgres starts a throwaway Postgres and applies every embedded
// migration as the superuser. The casbin_rules migration seeds the v1 policy the
// loader reads, so no extra seeding is needed here.
func startAuthzPostgres(ctx context.Context, t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	container := testsupport.StartPostgres(ctx, t)
	migrateAuthzUp(ctx, t, superConnString(ctx, t, container))
	return container
}

// migrateAuthzUp runs every embedded migration up as the superuser (needed for the
// CREATE ROLE in create_app_roles and the casbin_rules grant to opengate_bypass).
func migrateAuthzUp(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

// bypassRoleDSN builds the opengate_bypass connection string from the container's
// host/port and the well-known role credentials created by create_app_roles. The
// loader runs on this role because only opengate_bypass is granted SELECT on the
// global casbin_rules table.
func bypassRoleDSN(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
	t.Helper()
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return fmt.Sprintf("postgres://opengate_bypass:placeholder@%s:%s/opengate_test?sslmode=disable",
		host, port.Port())
}

// superConnString returns the container superuser DSN, used to apply migrations and
// to perform the AC3 out-of-band policy INSERT no application role is granted.
func superConnString(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
	t.Helper()
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("superuser connection string: %v", err)
	}
	return dsn
}

// openTestPool opens a pgx pool against dsn and registers its close.
func openTestPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
