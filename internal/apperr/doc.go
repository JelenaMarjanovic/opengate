// Package apperr holds cross-cutting application error sentinels and documents
// the error-wrapping convention.
//
// Import constraint: this package depends only on the Go standard library so
// any layer may import it.
//
// Convention: domain/port sentinels are declared with errors.New and matched
// with errors.Is; adapters wrap their underlying cause with %w so it stays
// reachable via errors.Unwrap; an adapter that hits an unexpected, non-domain
// failure returns ErrInternal wrapping that cause. Port-specific sentinels live
// alongside their ports (System Design §7); this package holds only the
// generic ones.
package apperr
