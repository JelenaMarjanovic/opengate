// Package inbound defines the interfaces through which external actors
// drive the application: HTTP handlers, CLI commands, scheduled jobs.
//
// Import constraint: this package may import internal/domain. It must not
// import internal/adapters or internal/application.
//
// Adapters in internal/adapters/inbound implement these interfaces and
// translate transport-specific input into application use case calls.
package inbound
