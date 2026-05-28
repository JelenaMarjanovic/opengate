// Package outbound contains adapters that implement outbound ports against
// concrete external systems (PostgreSQL via pgx, River for job queueing,
// OpenTelemetry for telemetry export, etc.).
//
// Import constraint: this package may import internal/domain and
// internal/ports/outbound. It must not import internal/application or
// internal/adapters/inbound.
package outbound
