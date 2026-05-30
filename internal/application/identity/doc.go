// Package identity holds the identity-provisioning use cases. Today it provides
// the bootstrap use case that creates the first tenant and its owner user in a
// single transaction (System Design §11).
//
// Import constraint: this package may import internal/domain, internal/ports,
// and the dependency-free internal/auth helper. It must not import
// internal/adapters; the concrete IdentityWriter is wired in cmd/opengate.
package identity
