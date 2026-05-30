// Package observability configures structured logging (and later tracing and
// metrics).
//
// Import constraint: this package is infrastructure. It may import the OTel
// trace API (go.opentelemetry.io/otel/trace) and the tenant package; it must
// NOT be imported by the domain layer. The configured *slog.Logger is injected
// into callers at the composition root, never reached through a package-global.
package observability
