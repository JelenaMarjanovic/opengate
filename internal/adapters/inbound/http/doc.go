// Package http is the inbound HTTP adapter: it translates HTTP requests into
// calls on application use cases and translates results — including domain
// errors — back into HTTP responses.
//
// This is the ONLY place domain errors are mapped to HTTP semantics (System
// Design §22). The mapping table in problem.go is that single seam, so the
// adapter deliberately imports the application layer's error sentinels
// (internal/application/auth) and internal/apperr. It must not be imported by
// internal/domain or internal/application.
//
// Naming: the package is named http to mirror the outbound postgres adapter
// (named for its technology). Inside the package, the imported standard-library
// net/http is the identifier "http"; external importers alias this package
// (e.g. httpadapter) to disambiguate.
package http
