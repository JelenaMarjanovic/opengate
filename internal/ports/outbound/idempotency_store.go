package outbound

import (
	"context"

	"github.com/google/uuid"
)

// CachedResponse is the stored outcome of a completed command: the HTTP status,
// whitelisted response headers, and body the command middleware previously
// returned for a (tenant, key) pair, plus the hash of the request that produced
// it.
//
// RequestHash lets the middleware distinguish a genuine retry (same key, same
// actor, same body — replay the cached response) from a key reused with a
// different payload, or replayed by a different actor inside the same tenant
// (a client error, answered with 409). Body is the raw response bytes; the
// middleware writes them verbatim on a replay.
type CachedResponse struct {
	// Status is the cached HTTP status code. Only 2xx responses are ever cached,
	// so a stored status is always a success (see the middleware's rationale: a
	// transient 5xx is not evidence that the operation completed, and replaying
	// one for the full retention window would poison the cache).
	Status int
	// Headers is the cached subset of the original response's headers, restored
	// on a replay so the replayed response is equivalent to the original rather
	// than losing its content type to net/http's sniffing.
	//
	// It mirrors http.Header semantics — canonical header name (as produced by
	// http.CanonicalHeaderKey) to the ordered list of values — but is declared as
	// a plain map so this port carries no net/http dependency. The middleware
	// applies an ALLOW-LIST before populating it, so anything not explicitly
	// named (Set-Cookie above all) is never stored here. Nil or empty means the
	// original response had none of the whitelisted headers.
	Headers map[string][]string
	// Body is the cached HTTP response body, written verbatim on a replay.
	Body []byte
	// RequestHash is the SHA-256 over the authenticated actor id and the original
	// request body. The middleware compares it against the current request's hash
	// to detect a key reused with a different payload or by a different actor.
	RequestHash []byte
}

// IdempotencyStore is the outbound port through which the command idempotency
// middleware (US-03.06, Pattern 2 — execute-then-record) de-duplicates retried
// mutating requests. The production implementation is the Postgres adapter in
// internal/adapters/outbound/postgres, backed by command_idempotency_keys.
//
// Both methods are tenant-scoped: tenantID is passed explicitly so the SQL
// predicate names it AND, on the RLS-bound pool, row-level security independently
// enforces the same scope (defense in depth). The tenant must also be present in
// ctx so the pool's tenant-binding hook sets app.current_tenant_id; the explicit
// argument and the bound tenant always agree.
type IdempotencyStore interface {
	// Lookup returns the cached response for (tenantID, key), or nil if no row
	// exists. A nil CachedResponse with a nil error means "not seen before" (a
	// cache miss), distinct from a non-nil error (a store fault).
	Lookup(ctx context.Context, tenantID uuid.UUID, key string) (*CachedResponse, error)

	// Record inserts the response for (tenantID, key) via INSERT ... ON CONFLICT
	// DO NOTHING. It returns inserted=true when this call wrote the row, and
	// inserted=false when a row for (tenantID, key) already existed — the Pattern 2
	// concurrent case, in which a competing request recorded first and the caller
	// must re-read the stored response via Lookup rather than return its own.
	//
	// headers carries the already-whitelisted response headers (see
	// CachedResponse.Headers); the store persists them verbatim and performs no
	// filtering of its own — the allow-list is the middleware's responsibility, so
	// this port never has to know which headers are safe to keep.
	Record(ctx context.Context, tenantID uuid.UUID, key string, requestHash []byte, status int, headers map[string][]string, body []byte) (inserted bool, err error)
}
