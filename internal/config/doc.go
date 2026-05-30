// Package config loads configuration from environment variables at startup.
//
// Import constraint: this package depends only on the Go standard library and
// the envconfig parser. It is read once at the composition root
// (cmd/opengate); other packages receive resolved values by injection, not by
// importing this package.
//
// The Config struct is the single source of truth (System Design §23). It
// carries only LogLevel today; add fields here only when the code consuming
// them lands, so the binary never requires unused config.
package config
