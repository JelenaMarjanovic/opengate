// Package events defines the domain event model for OpenGate's event-sourced
// aggregates: the Event envelope, its EventMetadata block, and the sentinel
// errors the EventStore port returns.
//
// Import constraint: this package sits at the bottom of the dependency graph.
// It may import only the Go standard library and google/uuid. It must not
// import internal/ports, internal/application, or internal/adapters, so that
// the application -> ports -> domain dependency direction stays acyclic (the
// EventStore port in internal/ports/outbound imports this package, never the
// reverse).
package events
