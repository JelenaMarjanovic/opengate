package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestIdentityWriterCreateTenantWithOwner exercises the bootstrap adapter
// against a real Postgres with every migration applied. It covers AC1 (atomic
// tenant + owner insert with correct fields) and AC3 (duplicate name rejected
// via ErrTenantExists with no extra rows).
func TestIdentityWriterCreateTenantWithOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()

	container := testsupport.StartPostgres(ctx, t)

	// Run ALL migrations up as the container superuser. The superuser is needed
	// because create_app_roles issues CREATE ROLE.
	superDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("super connection string: %v", err)
	}
	migrateUp(ctx, t, superDSN)

	// Derive the opengate_bypass DSN: same host/port/db as the superuser, but
	// the bypass role created by the migration (password 'placeholder').
	pool, err := pgxpool.New(ctx, deriveBypassDSN(ctx, t, container))
	if err != nil {
		t.Fatalf("open bypass pool: %v", err)
	}
	t.Cleanup(pool.Close)

	writer := postgres.NewIdentityWriter(pool)

	// --- AC1: a single CreateTenantWithOwner writes one tenant and one owner. ---
	tenantID, ownerID := uuid.New(), uuid.New()
	tn, err := domain.NewTenant(tenantID, "Acme Climbing", "acme-climbing")
	if err != nil {
		t.Fatalf("new tenant: %v", err)
	}
	tn.ContactEmail = "ops@acme.test"

	const ownerHash = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA"
	owner, err := domain.NewOwnerUser(ownerID, tenantID, "owner@acme.test", ownerHash)
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}

	if err := writer.CreateTenantWithOwner(ctx, tn, owner); err != nil {
		t.Fatalf("CreateTenantWithOwner: %v", err)
	}

	assertRowCount(ctx, t, pool, "tenants", 1)
	assertRowCount(ctx, t, pool, "users", 1)

	// Tenant fields, including the slug and session_timeout decoded back into a
	// time.Duration.
	var (
		gotName, gotSlug, gotContact, gotTimezone, gotTenantStatus string
		gotTimeout                                                 time.Duration
	)
	if err := pool.QueryRow(ctx,
		`SELECT name, slug, contact_email, timezone, session_timeout, status FROM tenants WHERE id = $1`, tenantID,
	).Scan(&gotName, &gotSlug, &gotContact, &gotTimezone, &gotTimeout, &gotTenantStatus); err != nil {
		t.Fatalf("read tenant row: %v", err)
	}
	if gotName != "Acme Climbing" || gotSlug != "acme-climbing" || gotContact != "ops@acme.test" ||
		gotTimezone != "UTC" || gotTimeout != 60*time.Minute || gotTenantStatus != "active" {
		t.Errorf("tenant row = (%q, slug=%q, %q, %q, %s, %q); want (Acme Climbing, acme-climbing, ops@acme.test, UTC, 1h0m0s, active)",
			gotName, gotSlug, gotContact, gotTimezone, gotTimeout, gotTenantStatus)
	}

	// Owner fields — role MUST be 'owner' (AC1).
	var (
		gotTenantFK                            uuid.UUID
		gotEmail, gotHash, gotRole, gotUStatus string
		gotMustChange                          bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT tenant_id, email, password_hash, role, status, must_change_password FROM users WHERE id = $1`, ownerID,
	).Scan(&gotTenantFK, &gotEmail, &gotHash, &gotRole, &gotUStatus, &gotMustChange); err != nil {
		t.Fatalf("read user row: %v", err)
	}
	if gotTenantFK != tenantID || gotEmail != "owner@acme.test" || gotHash != ownerHash ||
		gotRole != "owner" || gotUStatus != "active" || gotMustChange {
		t.Errorf("user row = (%s, %q, role=%q, status=%q, mustChange=%v); want owner of tenant %s, active, false",
			gotTenantFK, gotEmail, gotRole, gotUStatus, gotMustChange, tenantID)
	}

	// --- AC3: a second create with the SAME name is rejected, nothing written. ---
	// A distinct slug ensures the in-tx name pre-check (not the slug constraint) is
	// what rejects this duplicate, keeping the test focused on the name axis.
	dupTenantID, dupOwnerID := uuid.New(), uuid.New()
	dup, err := domain.NewTenant(dupTenantID, "Acme Climbing", "acme-climbing-2") // same name, different slug
	if err != nil {
		t.Fatalf("new duplicate tenant: %v", err)
	}
	dupOwner, err := domain.NewOwnerUser(dupOwnerID, dupTenantID, "second@acme.test", ownerHash)
	if err != nil {
		t.Fatalf("new duplicate owner: %v", err)
	}

	err = writer.CreateTenantWithOwner(ctx, dup, dupOwner)
	if !errors.Is(err, ports.ErrTenantExists) {
		t.Fatalf("duplicate create: err = %v, want ErrTenantExists", err)
	}
	// Row counts unchanged: the duplicate was rejected before any insert.
	assertRowCount(ctx, t, pool, "tenants", 1)
	assertRowCount(ctx, t, pool, "users", 1)
}

// migrateUp applies every embedded migration up against the given DSN.
func migrateUp(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

// deriveBypassDSN builds the opengate_bypass connection string from the
// container's host/port and the well-known bypass credentials created by the
// create_app_roles migration (user opengate_bypass, password 'placeholder').
func deriveBypassDSN(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
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

// assertRowCount fails unless the table holds exactly want rows. The table name
// is a trusted in-test constant, never user input.
func assertRowCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}
