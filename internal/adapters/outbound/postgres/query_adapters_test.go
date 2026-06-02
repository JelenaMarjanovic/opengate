package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// TestQueryAdapters exercises the Step 3 query-layer adapters against a real
// Postgres with every migration applied and RLS NOT yet enabled (it arrives in
// US-02.05). A single container is shared across subtests for speed; data is
// seeded through the BYPASSRLS pool, which is the only role able to INSERT into
// users and sessions. The post-authentication methods (Refresh/Delete) run on
// the regular RLS-bound pool as opengate_app, so the test also proves the
// Step 2 grant plus the Step 3 column-level SELECT grant are sufficient.
func TestQueryAdapters(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startMigratedContainer(ctx, t)
	bypassPool := openBypassPool(ctx, t, container)
	regularPool := openRegularPool(ctx, t, container)

	tenantResolver := postgres.NewTenantResolver(bypassPool)
	userReader := postgres.NewUserReader(bypassPool)
	sessions := postgres.NewSessionStore(bypassPool, regularPool)

	// A single instant, truncated to Postgres' microsecond resolution so
	// round-tripped timestamps compare exactly via time.Time.Equal.
	base := time.Now().UTC().Truncate(time.Microsecond)

	// Two tenants with DISTINCT session timeouts (so ResolveBySlug is shown to
	// return the row's actual value, not a default) and one shared-email user in
	// each (so FindByEmail's explicit-tenant scoping is observable).
	tenantA, tenantB := uuid.New(), uuid.New()
	seedTenant(ctx, t, bypassPool, tenantA, "Acme Climbing", "acme-climbing", 45*time.Minute)
	seedTenant(ctx, t, bypassPool, tenantB, "Beta Boulders", "beta-boulders", 60*time.Minute)

	userA, userB, userBOnly := uuid.New(), uuid.New(), uuid.New()
	seedUser(ctx, t, bypassPool, userA, tenantA, "shared@example.test", "hash-A", false)
	seedUser(ctx, t, bypassPool, userB, tenantB, "shared@example.test", "hash-B", true)
	seedUser(ctx, t, bypassPool, userBOnly, tenantB, "tenantb-only@example.test", "hash-BO", false)

	t.Run("ResolveBySlug", func(t *testing.T) {
		ref, err := tenantResolver.ResolveBySlug(ctx, "acme-climbing")
		if err != nil {
			t.Fatalf("ResolveBySlug(acme-climbing): %v", err)
		}
		if ref.ID != tenantA || ref.Status != domain.StatusActive || ref.SessionTimeout != 45*time.Minute {
			t.Errorf("ref = {id:%s status:%s timeout:%s}; want {%s active 45m0s}",
				ref.ID, ref.Status, ref.SessionTimeout, tenantA)
		}

		_, err = tenantResolver.ResolveBySlug(ctx, "no-such-slug")
		if !errors.Is(err, ports.ErrTenantNotFound) {
			t.Errorf("unknown slug: err = %v, want ErrTenantNotFound", err)
		}
	})

	t.Run("FindByEmail", func(t *testing.T) {
		// Same email, two tenants: the explicit tenantID argument selects the
		// correct row (different hash, different must_change_password).
		au, err := userReader.FindByEmail(ctx, tenantA, "shared@example.test")
		if err != nil {
			t.Fatalf("FindByEmail(A): %v", err)
		}
		if au.ID != userA || au.TenantID != tenantA || au.PasswordHash != "hash-A" ||
			au.Role != domain.RoleOwner || au.Status != domain.UserStatusActive || au.MustChangePassword {
			t.Errorf("A user = %+v; want id=%s tenant=%s hash=hash-A owner active mustChange=false", au, userA, tenantA)
		}

		bu, err := userReader.FindByEmail(ctx, tenantB, "shared@example.test")
		if err != nil {
			t.Fatalf("FindByEmail(B): %v", err)
		}
		if bu.ID != userB || bu.PasswordHash != "hash-B" || !bu.MustChangePassword {
			t.Errorf("B user = %+v; want id=%s hash=hash-B mustChange=true", bu, userB)
		}

		// An email that exists ONLY in tenant B is invisible from tenant A: the
		// explicit tenant_id predicate scopes the lookup (per-tenant isolation).
		_, err = userReader.FindByEmail(ctx, tenantA, "tenantb-only@example.test")
		if !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("cross-tenant email from A: err = %v, want ErrUserNotFound", err)
		}

		// A wholly nonexistent email is also not found.
		_, err = userReader.FindByEmail(ctx, tenantA, "ghost@example.test")
		if !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("nonexistent email: err = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("session create and find round-trip", func(t *testing.T) {
		sid := uuid.New()
		hash := tokenHash(sid.String())
		exp := base.Add(time.Hour)
		ns := ports.NewSession{
			ID: sid, TenantID: tenantA, UserID: userA,
			TokenHash: hash, Role: domain.RoleOwner,
			IssuedAt: base, LastSeenAt: base, ExpiresAt: exp,
			IssuedFromIP: netip.MustParseAddr("203.0.113.7"),
			UserAgent:    "Mozilla/5.0 (round-trip)",
		}
		if err := sessions.Create(ctx, ns); err != nil {
			t.Fatalf("Create: %v", err)
		}

		rec, err := sessions.FindByTokenHash(ctx, hash)
		if err != nil {
			t.Fatalf("FindByTokenHash: %v", err)
		}
		if rec.ID != sid || rec.TenantID != tenantA || rec.UserID != userA || rec.Role != domain.RoleOwner {
			t.Errorf("rec identity = {id:%s tenant:%s user:%s role:%s}; want {%s %s %s owner}",
				rec.ID, rec.TenantID, rec.UserID, rec.Role, sid, tenantA, userA)
		}
		if !rec.IssuedAt.Equal(base) || !rec.LastSeenAt.Equal(base) || !rec.ExpiresAt.Equal(exp) {
			t.Errorf("rec times = {issued:%s seen:%s exp:%s}; want {%s %s %s}",
				rec.IssuedAt, rec.LastSeenAt, rec.ExpiresAt, base, base, exp)
		}

		// An unknown hash is ErrSessionNotFound.
		_, err = sessions.FindByTokenHash(ctx, tokenHash("never-issued"))
		if !errors.Is(err, ports.ErrSessionNotFound) {
			t.Errorf("unknown hash: err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("create maps zero IP and empty user-agent to NULL", func(t *testing.T) {
		sid := uuid.New()
		hash := tokenHash(sid.String())
		ns := ports.NewSession{
			ID: sid, TenantID: tenantA, UserID: userA,
			TokenHash: hash, Role: domain.RoleOwner,
			IssuedAt: base, LastSeenAt: base, ExpiresAt: base.Add(time.Hour),
			// IssuedFromIP left as the zero netip.Addr, UserAgent left "".
		}
		if err := sessions.Create(ctx, ns); err != nil {
			t.Fatalf("Create: %v", err)
		}
		var ipNull, uaNull bool
		if err := bypassPool.QueryRow(ctx,
			`SELECT issued_from_ip IS NULL, user_agent IS NULL FROM sessions WHERE id = $1`, sid,
		).Scan(&ipNull, &uaNull); err != nil {
			t.Fatalf("probe nulls: %v", err)
		}
		if !ipNull || !uaNull {
			t.Errorf("issued_from_ip NULL = %v, user_agent NULL = %v; want both true", ipNull, uaNull)
		}
	})

	t.Run("RefreshSession", func(t *testing.T) {
		sid, hash := mintSession(ctx, t, sessions, tenantA, userA, base)
		ctxA := tenant.NewContext(ctx, tenant.ID(tenantA))

		newSeen := base.Add(10 * time.Minute)
		newExp := base.Add(2 * time.Hour)
		if err := sessions.Refresh(ctxA, sid, newSeen, newExp); err != nil {
			t.Fatalf("Refresh: %v", err)
		}

		rec, err := sessions.FindByTokenHash(ctx, hash)
		if err != nil {
			t.Fatalf("FindByTokenHash after refresh: %v", err)
		}
		if !rec.LastSeenAt.Equal(newSeen) || !rec.ExpiresAt.Equal(newExp) {
			t.Errorf("after refresh times = {seen:%s exp:%s}; want {%s %s}",
				rec.LastSeenAt, rec.ExpiresAt, newSeen, newExp)
		}

		// Refreshing a nonexistent session id reports zero rows -> not found.
		if err := sessions.Refresh(ctxA, uuid.New(), newSeen, newExp); !errors.Is(err, ports.ErrSessionNotFound) {
			t.Errorf("refresh nonexistent: err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("DeleteSession", func(t *testing.T) {
		sid, hash := mintSession(ctx, t, sessions, tenantA, userA, base)
		ctxA := tenant.NewContext(ctx, tenant.ID(tenantA))

		if err := sessions.Delete(ctxA, sid); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := sessions.FindByTokenHash(ctx, hash); !errors.Is(err, ports.ErrSessionNotFound) {
			t.Errorf("after delete, find: err = %v, want ErrSessionNotFound", err)
		}
		// Deleting again reports zero rows -> already gone.
		if err := sessions.Delete(ctxA, sid); !errors.Is(err, ports.ErrSessionNotFound) {
			t.Errorf("delete again: err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("cross-tenant refresh and delete blocked without RLS", func(t *testing.T) {
		// A session owned by tenant B.
		sidB, hashB := mintSession(ctx, t, sessions, tenantB, userB, base)
		// Tenant A is in context; the explicit tenant_id predicate (NOT RLS,
		// which is not enabled yet) must block A from touching B's session.
		ctxA := tenant.NewContext(ctx, tenant.ID(tenantA))

		if err := sessions.Refresh(ctxA, sidB, base.Add(time.Hour), base.Add(time.Hour)); !errors.Is(err, ports.ErrSessionNotFound) {
			t.Errorf("cross-tenant refresh: err = %v, want ErrSessionNotFound", err)
		}
		if err := sessions.Delete(ctxA, sidB); !errors.Is(err, ports.ErrSessionNotFound) {
			t.Errorf("cross-tenant delete: err = %v, want ErrSessionNotFound", err)
		}

		// B's session is untouched and still resolvable.
		if _, err := sessions.FindByTokenHash(ctx, hashB); err != nil {
			t.Errorf("B's session should survive A's cross-tenant attempts: %v", err)
		}
	})
}

// tokenHash returns the SHA-256 of seed as a byte slice, mirroring how the
// Step 4 use case will hash a session token before storage.
func tokenHash(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

// mintSession creates a session for (tenantID, userID) via the pre-auth Create
// path and returns its id and token hash for later assertions.
func mintSession(ctx context.Context, t *testing.T, store *postgres.SessionStore, tenantID, userID uuid.UUID, base time.Time) (uuid.UUID, []byte) {
	t.Helper()
	sid := uuid.New()
	hash := tokenHash(sid.String())
	err := store.Create(ctx, ports.NewSession{
		ID: sid, TenantID: tenantID, UserID: userID,
		TokenHash: hash, Role: domain.RoleOwner,
		IssuedAt: base, LastSeenAt: base, ExpiresAt: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("mintSession: %v", err)
	}
	return sid, hash
}

// seedTenant inserts an active tenant with the given slug and session timeout via
// the bypass pool. Values are trusted in-test constants, never user input.
func seedTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID, name, slug string, timeout time.Duration) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, slug, status, session_timeout)
		 VALUES ($1, $2, $3, 'active', make_interval(mins => $4))`,
		id, name, slug, int(timeout.Minutes()))
	if err != nil {
		t.Fatalf("seed tenant %q: %v", slug, err)
	}
}

// seedUser inserts an active owner-role user via the bypass pool.
func seedUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, tenantID uuid.UUID, email, hash string, mustChange bool) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, status, must_change_password)
		 VALUES ($1, $2, $3, $4, 'owner', 'active', $5)`,
		id, tenantID, email, hash, mustChange)
	if err != nil {
		t.Fatalf("seed user %q in %s: %v", email, tenantID, err)
	}
}

// startMigratedContainer starts a throwaway Postgres and applies every embedded
// migration as the superuser (needed for the CREATE ROLE in create_app_roles).
func startMigratedContainer(ctx context.Context, t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	container, err := tcpostgres.Run(ctx,
		"postgres:16.14-bookworm",
		tcpostgres.WithDatabase("opengate_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("super connection string: %v", err)
	}
	migrateUp(ctx, t, superDSN) // defined in identity_writer_test.go
	return container
}

// openBypassPool opens the opengate_bypass pool used for seeding and the
// pre-authentication adapter methods.
func openBypassPool(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, deriveBypassDSN(ctx, t, c)) // helper in identity_writer_test.go
	if err != nil {
		t.Fatalf("open bypass pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// openRegularPool opens the regular RLS-bound pool as opengate_app, with the
// real tenant-binding hooks installed (NewPool). The post-authentication adapter
// methods run on it; a discard logger is used because the test always supplies a
// tenant, so no warning is expected on the bind path.
func openRegularPool(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.NewPool(ctx, deriveAppDSN(ctx, t, c), observability.NewLogger(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("open regular pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// deriveAppDSN builds the opengate_app connection string from the container's
// host/port and the well-known app credentials created by create_app_roles
// (user opengate_app, password 'placeholder').
func deriveAppDSN(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
	t.Helper()
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return fmt.Sprintf("postgres://opengate_app:placeholder@%s:%s/opengate_test?sslmode=disable",
		host, port.Port())
}
