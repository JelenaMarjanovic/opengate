// Package outbound defines the interfaces through which the application
// reaches external systems: persistence, message delivery, telemetry sinks.
//
// Import constraint: this package may import internal/domain. It must not
// import internal/adapters or internal/application.
//
// Adapters in internal/adapters/outbound implement these interfaces against
// concrete technologies (Postgres, River, OTLP, etc.).
package outbound
