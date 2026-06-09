package inmemory_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/eventstorecontract"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/inmemory"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// TestInMemoryEventStoreContract runs the shared EventStore contract (US-03.03,
// AC4) against the in-memory double. The factory needs no infrastructure: a fresh
// store, a plain context, and a generated tenantID per subtest. The double has no
// RLS, so the tenant is just the explicit value the range read filters on.
func TestInMemoryEventStoreContract(t *testing.T) {
	eventstorecontract.RunEventStoreContract(t, func(t *testing.T) (ports.EventStore, context.Context, uuid.UUID) {
		return inmemory.NewEventStore(), context.Background(), uuid.New()
	})
}
