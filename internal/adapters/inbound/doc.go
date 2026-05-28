// Package inbound contains adapters that translate external transport
// signals (HTTP requests, SSE streams, scheduled triggers) into calls on
// application use cases.
//
// Import constraint: this package may import internal/domain,
// internal/ports/inbound, and internal/application. It must not be
// imported by internal/domain or by internal/application.
package inbound
