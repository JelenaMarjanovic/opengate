package tenant

import "context"

// ID is the tenant identifier carried in the request context. Distinct type so
// it cannot be confused with any other string. Representation is string for now;
// US-02.01 may promote it to UUID-backed once the DB column type is concrete.
type ID string

// String returns the tenant ID as a plain string.
func (id ID) String() string { return string(id) }

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
