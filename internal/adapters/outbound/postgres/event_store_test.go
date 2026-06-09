package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/eventstorecontract"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// TestPostgresEventStoreContract runs the shared EventStore contract (US-03.03,
// AC4) against the real Postgres adapter. One migrated container and one RLS-bound
// (opengate_app) pool are shared across subtests for speed; the factory gives each
// subtest fresh state by creating a brand-new tenant (a real tenants row so the
// events FK resolves) and binding the returned ctx to it, so RLS scopes the
// command-path reads to that tenant and prior subtests' events stay invisible.
//
// The adapter is constructed over the RLS-bound pool: with a single tenant per
// subtest, Append/Load and the two cross-tenant reads all run correctly on it
// (the production composition root would instead point the read path at the
// BYPASSRLS pool; that pool choice is the composition root's, not the adapter's).
// The production grant_events migration already grants opengate_app the table and
// sequence privileges Append/Load need, so no test-only grant scaffolding is used.
func TestPostgresEventStoreContract(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startMigratedContainer(ctx, t)
	bypassPool := openBypassPool(ctx, t, container) // seeds the per-subtest tenant
	regularPool := openRegularPool(ctx, t, container)

	eventstorecontract.RunEventStoreContract(t, func(t *testing.T) (ports.EventStore, context.Context, uuid.UUID) {
		// A fresh tenant per subtest. The slug is uuid-derived so it satisfies the
		// tenants_slug_format_check grammar and never collides across subtests.
		tenantID := uuid.New()
		slug := "evt-" + tenantID.String()
		seedTenant(ctx, t, bypassPool, tenantID, "Event Store Contract "+slug, slug, 60*time.Minute)

		// Bind the tenant into ctx so the RLS-bound pool's AfterAcquire hook sets
		// app.current_tenant_id and the events policy admits the writes and reads.
		boundCtx := tenant.NewContext(ctx, tenant.ID(tenantID))
		return postgres.NewEventStore(regularPool), boundCtx, tenantID
	})
}
