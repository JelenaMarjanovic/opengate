// Package domain contains the core business types and rules for OpenGate.
//
// Import constraint: this package may import only the Go standard library
// and small utility packages that have no infrastructure dependencies
// (e.g., google/uuid). It must not import any package under
// internal/adapters, internal/ports, or internal/application.
//
// The domain core models tenants, members, credentials, doors, access
// policies, and the events emitted by aggregate state transitions.
package domain
