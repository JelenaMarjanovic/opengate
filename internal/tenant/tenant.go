package tenant

import (
	"context"

	"github.com/google/uuid"
)

// ID is the tenant identifier carried in the request context. It is a distinct
// type so it cannot be confused with any other UUID or string flowing through
// the context. US-02.01 promoted it from a plain string to UUID-backed now that
// the DB column type is concrete (uuid, DB §5.1).
type ID uuid.UUID

// String returns the canonical RFC 4122 string form of the tenant ID.
func (id ID) String() string { return uuid.UUID(id).String() }

type ctxKey struct{}

// NewContext returns a copy of ctx carrying the given tenant ID.
func NewContext(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the tenant ID in ctx, panicking if none is set: a missing
// tenant on a tenant-scoped path is a programming error (System Design §7).
func FromContext(ctx context.Context) ID {
	id, ok := IDFromContext(ctx)
	if !ok {
		panic("tenant: no tenant ID in context")
	}
	return id
}

// IDFromContext returns the tenant ID and true if set, else the zero ID and
// false. Used by code that legitimately runs outside a tenant scope.
func IDFromContext(ctx context.Context) (ID, bool) {
	id, ok := ctx.Value(ctxKey{}).(ID)
	return id, ok
}
