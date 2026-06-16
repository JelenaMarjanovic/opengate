package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// lookupIdempotencySQL reads the cached response for one (tenant, key). tenant_id
// is passed explicitly as $1 even though row-level security already scopes the
// table on the RLS-bound pool: the explicit predicate and the RLS policy agree
// (defense in depth), so a misconfigured pool or a future read on a different
// pool cannot silently widen the lookup. No row -> pgx.ErrNoRows, mapped to a nil
// CachedResponse (a cache miss, not an error).
const lookupIdempotencySQL = `
SELECT response_status, response_headers, response_body, request_hash
FROM command_idempotency_keys
WHERE tenant_id = $1 AND idempotency_key = $2`

// recordIdempotencySQL persists a completed command's response. ON CONFLICT DO
// NOTHING makes the insert a no-op when a row for (tenant_id, idempotency_key)
// already exists — the Pattern 2 concurrent case, where a competing request
// recorded first. The command tag's RowsAffected then reports 0, which the
// adapter surfaces as inserted=false so the caller re-reads the winner's stored
// response. The grant migration deliberately gives opengate_app no UPDATE: DO
// NOTHING has no UPDATE branch, unlike DO UPDATE.
const recordIdempotencySQL = `
INSERT INTO command_idempotency_keys
    (tenant_id, idempotency_key, request_hash, response_status, response_headers, response_body)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`

// emptyJSONObject is the response_headers value written when a response carried
// none of the whitelisted headers. It matches the column's DEFAULT, so a row with
// no headers is indistinguishable from one written before the column existed —
// and never NULL, which the NOT NULL column would reject.
var emptyJSONObject = []byte(`{}`)

// IdempotencyStore is the Postgres adapter implementing ports.IdempotencyStore.
// Like the EventStore adapter it holds a db.DBTX (which *pgxpool.Pool satisfies)
// and is pool-agnostic — it never references RLS or tenant context directly. The
// composition root constructs it over the RLS-bound opengate_app pool, so the
// pool's tenant-binding hook sets app.current_tenant_id from the request context
// and the command_idempotency_keys tenant_isolation policy scopes every statement
// to the bound tenant. The adapter additionally passes tenant_id explicitly, so
// the policy and the predicate enforce the same scope.
type IdempotencyStore struct {
	db db.DBTX
}

// Compile-time assertion that the adapter satisfies the port.
var _ ports.IdempotencyStore = (*IdempotencyStore)(nil)

// NewIdempotencyStore returns an IdempotencyStore backed by the given pool. The
// argument is a db.DBTX so the adapter stays pool-agnostic; a *pgxpool.Pool
// satisfies it. The production composition root passes the RLS-bound pool.
func NewIdempotencyStore(pool db.DBTX) *IdempotencyStore {
	return &IdempotencyStore{db: pool}
}

// Lookup returns the cached response for (tenantID, key), or nil when no row
// exists. The no-row case is a cache miss, not a failure, so it returns
// (nil, nil); any other query error is wrapped as an internal error. The bound
// tenant in ctx and the explicit tenantID argument always name the same tenant.
func (s *IdempotencyStore) Lookup(ctx context.Context, tenantID uuid.UUID, key string) (*ports.CachedResponse, error) {
	// response_status is an int4 column, decoded into int32 and widened to the
	// port's int; the values are HTTP status codes, so the widening never loses data.
	var status int32
	var headersJSON, body, requestHash []byte

	err := s.db.QueryRow(ctx, lookupIdempotencySQL, tenantID, key).Scan(&status, &headersJSON, &body, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // cache miss: never seen this (tenant, key)
	}
	if err != nil {
		return nil, fmt.Errorf("lookup idempotency key: %w: %w", apperr.ErrInternal, err)
	}

	// jsonb scans into []byte as the raw document; decode it into the port's
	// http.Header-shaped map. A malformed document is a store fault, not a cache
	// miss: returning a partially-decoded response would replay the original's
	// status and body under the wrong headers, so fail loudly instead.
	headers, err := decodeResponseHeaders(headersJSON)
	if err != nil {
		return nil, err
	}

	return &ports.CachedResponse{
		Status:      int(status),
		Headers:     headers,
		Body:        body,
		RequestHash: requestHash,
	}, nil
}

// decodeResponseHeaders turns the stored jsonb document into the port's
// map[string][]string. An empty document (or the column DEFAULT '{}') decodes to
// a nil map, which the middleware treats as "no headers to restore".
func decodeResponseHeaders(raw []byte) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var headers map[string][]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil, fmt.Errorf("decode cached response headers: %w: %w", apperr.ErrInternal, err)
	}
	if len(headers) == 0 {
		return nil, nil
	}
	return headers, nil
}

// Record inserts the response for (tenantID, key) with ON CONFLICT DO NOTHING and
// reports whether this call wrote the row. inserted=false means a concurrent
// writer won the race (the row already existed), so the caller re-reads the
// stored response via Lookup instead of returning its own. status is passed as a
// Go int; pgx encodes it as bigint and Postgres assignment-casts it into the
// int4 response_status column (HTTP status codes are well within int4 range).
//
// headers arrives already filtered by the middleware's allow-list; the adapter
// serializes it verbatim into the response_headers jsonb column and applies no
// policy of its own.
func (s *IdempotencyStore) Record(
	ctx context.Context, tenantID uuid.UUID, key string,
	requestHash []byte, status int, headers map[string][]string, body []byte,
) (bool, error) {
	// Marshal explicitly rather than handing pgx the map: it keeps the encoded
	// shape under this adapter's control and turns the no-headers case into the
	// column's own '{}' default instead of a JSON `null`, which the NOT NULL
	// column would reject.
	headersJSON := emptyJSONObject
	if len(headers) > 0 {
		encoded, err := json.Marshal(headers)
		if err != nil {
			return false, fmt.Errorf("encode cached response headers: %w: %w", apperr.ErrInternal, err)
		}
		headersJSON = encoded
	}

	// A nil []byte encodes as SQL NULL, which response_body (bytea NOT NULL) rejects
	// — so a legitimately body-less success (a 204, or a 201 whose result is carried
	// entirely by Location) would fail to record and silently lose its idempotency
	// protection. Coerce nil to an empty slice, which encodes as an empty bytea.
	if body == nil {
		body = []byte{}
	}

	tag, err := s.db.Exec(ctx, recordIdempotencySQL, tenantID, key, requestHash, status, headersJSON, body)
	if err != nil {
		return false, fmt.Errorf("record idempotency key: %w: %w", apperr.ErrInternal, err)
	}
	// RowsAffected is 1 when this statement inserted the row, 0 when ON CONFLICT DO
	// NOTHING skipped it because a row for (tenant_id, idempotency_key) already existed.
	return tag.RowsAffected() == 1, nil
}
