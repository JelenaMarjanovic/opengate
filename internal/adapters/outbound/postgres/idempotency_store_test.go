package postgres_test

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// TestPostgresIdempotencyStore exercises the Postgres IdempotencyStore adapter
// directly against the real command_idempotency_keys table on a testcontainers
// Postgres. It runs on the RLS-bound (opengate_app) pool — the production command
// path — so the tenant_isolation policy is in force; a fresh tenant per subtest,
// bound into ctx, scopes each subtest's rows. command_idempotency_keys has no FK to
// tenants, so no tenant row needs seeding — only the bound tenant in context, which
// the pool's hook turns into app.current_tenant_id.
//
// The HTTP middleware integration test (internal/adapters/inbound/http) covers the
// full request flow; this is the focused adapter contract: a miss returns nil, a
// record round-trips, ON CONFLICT DO NOTHING reports inserted=false WITHOUT
// overwriting (the Pattern 2 concurrent-winner semantics), and RLS hides another
// tenant's row even when the explicit predicate names it (defense in depth).
func TestPostgresIdempotencyStore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startMigratedContainer(ctx, t)
	regularPool := openRegularPool(ctx, t, container) // opengate_app, RLS-bound
	store := postgres.NewIdempotencyStore(regularPool)

	// A miss — no row for this (tenant, key) — is (nil, nil), not an error.
	t.Run("lookup of an unknown key returns nil", func(t *testing.T) {
		tenantID := uuid.New()
		ctxT := tenant.NewContext(ctx, tenant.ID(tenantID))

		got, err := store.Lookup(ctxT, tenantID, "never-seen")
		if err != nil {
			t.Fatalf("Lookup: unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("Lookup of an absent key = %+v, want nil (cache miss)", got)
		}
	})

	// Record then Lookup round-trips the status, headers, body, and request hash
	// verbatim — including a multi-valued header, since the column's shape mirrors
	// http.Header (name -> list of values) rather than name -> single value.
	t.Run("record then lookup round-trips the stored response", func(t *testing.T) {
		tenantID := uuid.New()
		ctxT := tenant.NewContext(ctx, tenant.ID(tenantID))
		key := uuid.NewString()
		hash := []byte("request-hash-A")
		body := []byte(`{"id":"created"}`)
		headers := map[string][]string{
			"Content-Type": {"application/json"},
			"Location":     {"/api/v1/members/018f"},
			"Etag":         {`W/"1"`, `W/"2"`},
		}

		inserted, err := store.Record(ctxT, tenantID, key, hash, http.StatusCreated, headers, body)
		if err != nil {
			t.Fatalf("Record: unexpected error: %v", err)
		}
		if !inserted {
			t.Fatalf("Record inserted = false, want true (first write of this key)")
		}

		got, err := store.Lookup(ctxT, tenantID, key)
		if err != nil {
			t.Fatalf("Lookup: unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("Lookup after Record = nil, want the stored response")
		}
		if got.Status != http.StatusCreated {
			t.Errorf("Status = %d, want %d", got.Status, http.StatusCreated)
		}
		if !maps.EqualFunc(got.Headers, headers, slices.Equal) {
			t.Errorf("Headers = %v, want %v", got.Headers, headers)
		}
		if !bytes.Equal(got.Body, body) {
			t.Errorf("Body = %q, want %q", got.Body, body)
		}
		if !bytes.Equal(got.RequestHash, hash) {
			t.Errorf("RequestHash = %q, want %q", got.RequestHash, hash)
		}
	})

	// A body-less success (204, or a 201 carried entirely by Location) with no
	// whitelisted headers still records: the '{}' header default and an empty bytea,
	// NOT the SQL NULLs that both NOT NULL columns would reject. Before the nil
	// coercion in Record this combination failed with a not-null violation, so such
	// a command silently lost its idempotency protection.
	t.Run("record with no headers and no body round-trips as empty, not null", func(t *testing.T) {
		tenantID := uuid.New()
		ctxT := tenant.NewContext(ctx, tenant.ID(tenantID))
		key := uuid.NewString()

		if _, err := store.Record(
			ctxT, tenantID, key, []byte("hash"), http.StatusNoContent, nil, nil,
		); err != nil {
			t.Fatalf("Record with nil headers: unexpected error: %v", err)
		}

		got, err := store.Lookup(ctxT, tenantID, key)
		if err != nil {
			t.Fatalf("Lookup: unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("Lookup = nil, want the stored response")
		}
		if len(got.Headers) != 0 {
			t.Errorf("Headers = %v, want empty", got.Headers)
		}
		if len(got.Body) != 0 {
			t.Errorf("Body = %q, want empty", got.Body)
		}
		if got.Status != http.StatusNoContent {
			t.Errorf("Status = %d, want %d", got.Status, http.StatusNoContent)
		}
	})

	// The Pattern 2 concurrent-winner semantics at the SQL boundary: a second Record
	// for an existing (tenant, key) reports inserted=false via ON CONFLICT DO NOTHING
	// and leaves the FIRST row intact — it does not overwrite. This is what lets the
	// middleware's losing racer safely re-read and return the winner's response.
	t.Run("duplicate record is a no-op that preserves the first row", func(t *testing.T) {
		tenantID := uuid.New()
		ctxT := tenant.NewContext(ctx, tenant.ID(tenantID))
		key := uuid.NewString()
		firstHash, firstBody := []byte("hash-first"), []byte(`{"winner":true}`)
		secondHash, secondBody := []byte("hash-second"), []byte(`{"loser":true}`)
		firstHeaders := map[string][]string{"Content-Type": {"application/json"}}
		secondHeaders := map[string][]string{"Content-Type": {"text/plain"}}

		inserted, err := store.Record(ctxT, tenantID, key, firstHash, http.StatusOK, firstHeaders, firstBody)
		if err != nil || !inserted {
			t.Fatalf("first Record: inserted=%t err=%v, want inserted=true nil", inserted, err)
		}

		// Second writer for the same key, with a DIFFERENT payload, headers, and status.
		inserted, err = store.Record(
			ctxT, tenantID, key, secondHash, http.StatusInternalServerError, secondHeaders, secondBody)
		if err != nil {
			t.Fatalf("second Record: unexpected error: %v", err)
		}
		if inserted {
			t.Errorf("second Record inserted = true, want false (ON CONFLICT DO NOTHING)")
		}

		// The stored row is still the FIRST one: DO NOTHING did not overwrite it.
		got, err := store.Lookup(ctxT, tenantID, key)
		if err != nil {
			t.Fatalf("Lookup: unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("Lookup = nil, want the first stored response")
		}
		if got.Status != http.StatusOK || !bytes.Equal(got.Body, firstBody) || !bytes.Equal(got.RequestHash, firstHash) {
			t.Errorf("stored row = {status:%d body:%q hash:%q}, want the FIRST {200 %q %q} (no overwrite)",
				got.Status, got.Body, got.RequestHash, firstBody, firstHash)
		}
		if !maps.EqualFunc(got.Headers, firstHeaders, slices.Equal) {
			t.Errorf("stored headers = %v, want the FIRST %v (no overwrite)", got.Headers, firstHeaders)
		}
	})

	// Defense in depth: a row written under tenant A is invisible to a lookup bound to
	// tenant B EVEN when the explicit predicate names tenant A. The two lookups differ
	// only in the bound tenant (context); the predicate is fixed at A, so visible vs
	// nil isolates RLS — not the predicate — as the cause.
	t.Run("RLS hides another tenant's row even when the predicate names it", func(t *testing.T) {
		tenantA, tenantB := uuid.New(), uuid.New()
		ctxA := tenant.NewContext(ctx, tenant.ID(tenantA))
		ctxB := tenant.NewContext(ctx, tenant.ID(tenantB))
		key := uuid.NewString()

		if _, err := store.Record(
			ctxA, tenantA, key, []byte("hash"), http.StatusOK, nil, []byte(`{}`),
		); err != nil {
			t.Fatalf("Record under tenant A: %v", err)
		}

		// Predicate A, bound tenant A: the row is visible.
		got, err := store.Lookup(ctxA, tenantA, key)
		if err != nil {
			t.Fatalf("Lookup under tenant A: %v", err)
		}
		if got == nil {
			t.Fatal("Lookup under tenant A = nil, want the stored row")
		}

		// Predicate STILL A, but bound tenant B: RLS scopes the connection to B, so the
		// tenant-A row is hidden despite the matching predicate.
		got, err = store.Lookup(ctxB, tenantA, key)
		if err != nil {
			t.Fatalf("cross-tenant Lookup (predicate A, bound B): %v", err)
		}
		if got != nil {
			t.Errorf("cross-tenant Lookup = %+v, want nil (RLS must hide tenant A's row under tenant B)", got)
		}
	})
}
