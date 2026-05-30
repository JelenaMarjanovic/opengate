package tenant_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// TestRoundTrip proves a stored ID is read back by both accessors.
func TestRoundTrip(t *testing.T) {
	id := tenant.ID(uuid.New())
	ctx := tenant.NewContext(context.Background(), id)

	if got := tenant.FromContext(ctx); got != id {
		t.Errorf("FromContext = %s, want %s", got, id)
	}
	got, ok := tenant.IDFromContext(ctx)
	if !ok || got != id {
		t.Errorf("IDFromContext = %s, %v; want %s, true", got, ok, id)
	}
}

// TestIDFromContextAbsent proves the safe accessor reports absence rather than
// panicking — the path used by cross-cutting code such as logging. The zero ID
// is the zero UUID since ID is now UUID-backed.
func TestIDFromContextAbsent(t *testing.T) {
	got, ok := tenant.IDFromContext(context.Background())
	if ok || got != (tenant.ID{}) {
		t.Errorf("IDFromContext on empty ctx = %s, %v; want zero ID, false", got, ok)
	}
}

// TestFromContextPanics proves FromContext panics on a tenant-scoped path with
// no tenant set (System Design §7).
func TestFromContextPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("FromContext did not panic on missing tenant")
		}
	}()
	tenant.FromContext(context.Background())
}
