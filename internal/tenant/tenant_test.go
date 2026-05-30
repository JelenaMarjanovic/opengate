package tenant_test

import (
	"context"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// TestRoundTrip proves a stored ID is read back by both accessors.
func TestRoundTrip(t *testing.T) {
	ctx := tenant.NewContext(context.Background(), "acme")

	if got := tenant.FromContext(ctx); got != "acme" {
		t.Errorf("FromContext = %q, want acme", got)
	}
	got, ok := tenant.IDFromContext(ctx)
	if !ok || got != "acme" {
		t.Errorf("IDFromContext = %q, %v; want acme, true", got, ok)
	}
}

// TestIDFromContextAbsent proves the safe accessor reports absence rather than
// panicking — the path used by cross-cutting code such as logging.
func TestIDFromContextAbsent(t *testing.T) {
	got, ok := tenant.IDFromContext(context.Background())
	if ok || got != "" {
		t.Errorf("IDFromContext on empty ctx = %q, %v; want \"\", false", got, ok)
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
