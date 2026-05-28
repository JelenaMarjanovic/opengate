// Package application orchestrates use cases by composing domain logic
// with ports.
//
// Import constraint: this package may import internal/domain and
// internal/ports, but must not import internal/adapters directly. Concrete
// adapters are wired in cmd/opengate at startup time.
//
// Use cases in this package are typically named CommandHandler or
// QueryHandler and expose Execute(ctx, input) (output, error) signatures.
package application
