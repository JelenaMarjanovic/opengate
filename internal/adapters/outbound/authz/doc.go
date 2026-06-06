// Package authz contains the outbound authorization adapter: the Casbin model,
// the policy-refreshing CasbinAuthorizer, and the PolicyLoaderFunc seam through
// which policy rules are supplied.
//
// The authorizer is decoupled from Postgres by design. It depends only on an
// injected PolicyLoaderFunc that yields [][]string rules ([sub, obj, act]); the
// production wiring passes the Postgres loader (running on the BYPASSRLS pool),
// while the enforce tests pass a fake — exactly as the US-02.03 use case injects
// VerifierFunc/HasherFunc. This package therefore imports neither pgx nor the
// postgres adapter package.
//
// Import constraint: as an outbound adapter it may import internal/domain and
// internal/ports/outbound. It must not import internal/application or
// internal/adapters/inbound.
package authz
