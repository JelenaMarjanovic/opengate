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
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
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
	superPool := openSuperuserPool(ctx, t, container)

	tenantResolver := postgres.NewTenantResolver(bypassPool)
	userReader := postgres.NewUserReader(bypassPool)
	userWriter := postgres.NewUserWriter(bypassPool)
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

	t.Run("UpdatePasswordHash", func(t *testing.T) {
		// A dedicated user so the mutation cannot disturb the shared fixtures the
		// other subtests read.
		uid := uuid.New()
		seedUser(ctx, t, bypassPool, uid, tenantA, "rehash@example.test", "old-hash", false)

		// Capture updated_at before the write so we can prove it advances. now()
		// is the transaction timestamp, so the seed and the update — separate pool
		// round trips, hence separate transactions — get strictly increasing values.
		var beforeUpdatedAt time.Time
		if err := bypassPool.QueryRow(ctx,
			`SELECT updated_at FROM users WHERE id = $1`, uid,
		).Scan(&beforeUpdatedAt); err != nil {
			t.Fatalf("read updated_at before: %v", err)
		}

		const newHash = "$argon2id$v=19$m=65536,t=3,p=4$bmV3c2FsdG5ld3NhbHQ$bmV3aGFzaG5ld2hhc2g"
		if err := userWriter.UpdatePasswordHash(ctx, tenantA, uid, newHash); err != nil {
			t.Fatalf("UpdatePasswordHash: %v", err)
		}

		var gotHash string
		var afterUpdatedAt time.Time
		if err := bypassPool.QueryRow(ctx,
			`SELECT password_hash, updated_at FROM users WHERE id = $1`, uid,
		).Scan(&gotHash, &afterUpdatedAt); err != nil {
			t.Fatalf("read user after update: %v", err)
		}
		if gotHash != newHash {
			t.Errorf("password_hash = %q, want %q", gotHash, newHash)
		}
		if !afterUpdatedAt.After(beforeUpdatedAt) {
			t.Errorf("updated_at = %s, want strictly after %s", afterUpdatedAt, beforeUpdatedAt)
		}

		// A nonexistent user id reports zero rows -> ErrUserNotFound.
		if err := userWriter.UpdatePasswordHash(ctx, tenantA, uuid.New(), newHash); !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("nonexistent id: err = %v, want ErrUserNotFound", err)
		}
		// The right user id under the WRONG tenant also reports zero rows: the
		// explicit tenant_id predicate scopes the write before RLS exists.
		if err := userWriter.UpdatePasswordHash(ctx, tenantB, uid, newHash); !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("wrong tenant: err = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("RecordLastLogin", func(t *testing.T) {
		// A dedicated user; seedUser does not set last_login_at, so it starts NULL.
		uid := uuid.New()
		seedUser(ctx, t, bypassPool, uid, tenantA, "lastlogin@example.test", "hash-LL", false)

		var nullBefore bool
		if err := bypassPool.QueryRow(ctx,
			`SELECT last_login_at IS NULL FROM users WHERE id = $1`, uid,
		).Scan(&nullBefore); err != nil {
			t.Fatalf("probe last_login_at NULL: %v", err)
		}
		if !nullBefore {
			t.Fatalf("seeded user last_login_at is not NULL; want NULL")
		}

		// A known instant (base is already UTC, microsecond-truncated) so the
		// round-tripped timestamptz compares exactly via time.Time.Equal.
		when := base.Add(7 * time.Minute)
		if err := userWriter.RecordLastLogin(ctx, tenantA, uid, when); err != nil {
			t.Fatalf("RecordLastLogin: %v", err)
		}

		var got time.Time
		if err := bypassPool.QueryRow(ctx,
			`SELECT last_login_at FROM users WHERE id = $1`, uid,
		).Scan(&got); err != nil {
			t.Fatalf("read last_login_at: %v", err)
		}
		if !got.Equal(when) {
			t.Errorf("last_login_at = %s, want %s", got, when)
		}

		// Wrong id / wrong tenant -> ErrUserNotFound.
		if err := userWriter.RecordLastLogin(ctx, tenantA, uuid.New(), when); !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("nonexistent id: err = %v, want ErrUserNotFound", err)
		}
		if err := userWriter.RecordLastLogin(ctx, tenantB, uid, when); !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("wrong tenant: err = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("cross-tenant mutation blocked without RLS", func(t *testing.T) {
		// Distinct users in each tenant. A mutation issued with tenant A's explicit
		// tenantID but aimed at tenant B's user must match no row — the explicit
		// tenant_id predicate (NOT RLS, which is not enabled yet) blocks it — and
		// must leave B's row untouched. This mirrors the Step 3 session test.
		uidA, uidB := uuid.New(), uuid.New()
		seedUser(ctx, t, bypassPool, uidA, tenantA, "xtenant-a@example.test", "hash-XA", false)
		seedUser(ctx, t, bypassPool, uidB, tenantB, "xtenant-b@example.test", "hash-XB", false)

		if err := userWriter.UpdatePasswordHash(ctx, tenantA, uidB, "intruder-hash"); !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("cross-tenant rehash: err = %v, want ErrUserNotFound", err)
		}
		if err := userWriter.RecordLastLogin(ctx, tenantA, uidB, base); !errors.Is(err, ports.ErrUserNotFound) {
			t.Errorf("cross-tenant last-login: err = %v, want ErrUserNotFound", err)
		}

		// B's user is intact: hash unchanged, last_login_at still NULL.
		var gotHash string
		var lastLoginNull bool
		if err := bypassPool.QueryRow(ctx,
			`SELECT password_hash, last_login_at IS NULL FROM users WHERE id = $1`, uidB,
		).Scan(&gotHash, &lastLoginNull); err != nil {
			t.Fatalf("read B user: %v", err)
		}
		if gotHash != "hash-XB" || !lastLoginNull {
			t.Errorf("B user after cross-tenant attempts = {hash:%q lastLoginNull:%v}; want {hash-XB true}",
				gotHash, lastLoginNull)
		}
	})

	t.Run("FindByTokenHash folds in tenant session_timeout and status", func(t *testing.T) {
		// A dedicated tenant (45-minute timeout, active) and user, so suspending
		// the tenant below cannot disturb the shared fixtures other subtests read.
		jt := uuid.New()
		seedTenant(ctx, t, bypassPool, jt, "Join Test Gym", "join-test-gym", 45*time.Minute)
		ju := uuid.New()
		seedUser(ctx, t, bypassPool, ju, jt, "join@example.test", "hash-J", false)
		sid, hash := mintSession(ctx, t, sessions, jt, ju, base)

		// The JOIN yields the tenant's session_timeout (as a time.Duration) and its
		// CURRENT status in the same round trip.
		rec, err := sessions.FindByTokenHash(ctx, hash)
		if err != nil {
			t.Fatalf("FindByTokenHash: %v", err)
		}
		if rec.ID != sid {
			t.Fatalf("rec.ID = %s, want %s", rec.ID, sid)
		}
		if rec.SessionTimeout != 45*time.Minute {
			t.Errorf("SessionTimeout = %s, want 45m0s", rec.SessionTimeout)
		}
		if rec.TenantStatus != domain.StatusActive {
			t.Errorf("TenantStatus = %q, want active", rec.TenantStatus)
		}

		// Suspend the tenant out-of-band; the SAME lookup must now reflect it,
		// because status is read live via the JOIN, not snapshotted at issue. This
		// is what lets the validate use case (Step 4b) reject a session whose tenant
		// was suspended after the session was minted. The flip runs on the superuser
		// pool: opengate_bypass deliberately holds only SELECT+INSERT on tenants (a
		// tenant's status is changed by an operator, not the app), so the test uses
		// the bypass-capable superuser for this out-of-band write.
		if _, err := superPool.Exec(ctx,
			`UPDATE tenants SET status = 'suspended' WHERE id = $1`, jt,
		); err != nil {
			t.Fatalf("suspend tenant: %v", err)
		}
		rec2, err := sessions.FindByTokenHash(ctx, hash)
		if err != nil {
			t.Fatalf("FindByTokenHash after suspend: %v", err)
		}
		if rec2.TenantStatus != domain.StatusSuspended {
			t.Errorf("after suspend, TenantStatus = %q, want suspended", rec2.TenantStatus)
		}
		// The status flip does not perturb the timeout the same row carries.
		if rec2.SessionTimeout != 45*time.Minute {
			t.Errorf("after suspend, SessionTimeout = %s, want 45m0s", rec2.SessionTimeout)
		}

		// A random unknown hash is still ErrSessionNotFound — the JOIN did not
		// change the not-found contract.
		if _, err := sessions.FindByTokenHash(ctx, tokenHash("join-never-issued")); !errors.Is(err, ports.ErrSessionNotFound) {
			t.Errorf("unknown hash: err = %v, want ErrSessionNotFound", err)
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
	container := testsupport.StartPostgres(ctx, t)
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

// openSuperuserPool opens a pool as the container superuser. It is
// bypass-capable and — unlike opengate_bypass, which holds only SELECT+INSERT on
// tenants — may UPDATE tenants. Tests use it for out-of-band tenant mutations
// (e.g. flipping status to 'suspended') that no application role is granted,
// mirroring how the migration round-trip test seeds via the superuser
// connection. The DSN is the container's own connection string.
func openSuperuserPool(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) *pgxpool.Pool {
	t.Helper()
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("superuser connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open superuser pool: %v", err)
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
