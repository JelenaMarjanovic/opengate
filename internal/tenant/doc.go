// Package tenant carries the current tenant identity through the request
// context.
//
// Import constraint: this package depends only on the Go standard library and
// the dependency-free google/uuid package (which backs the ID type). It is
// imported both by tenant-scoped application/adapter code and by cross-cutting
// infrastructure (e.g. internal/observability), so it must stay free of any
// infrastructure dependency.
//
// Tenant-scoped port methods read the ID via FromContext, which panics if none
// is set (a missing tenant on a tenant-scoped path is a programming error,
// System Design §7). Cross-cutting code that may legitimately run outside a
// tenant scope (e.g. logging) uses IDFromContext, which never panics.
package tenant
