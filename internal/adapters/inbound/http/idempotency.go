package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

const (
	// idempotencyHeader is the request header carrying the client's idempotency key.
	idempotencyHeader = "Idempotency-Key"

	// maxIdempotencyKeyLen bounds the accepted key length. A key is an opaque client
	// token (a UUID is 36 chars); anything beyond this is rejected as invalid rather
	// than stored, capping the text the client can drive into the primary key.
	maxIdempotencyKeyLen = 255

	// maxIdempotencyBodyBytes is the 1 MiB ceiling on both the request body the
	// middleware will hash and the response body it will cache. A request body over
	// this is rejected (413); a response body over it is streamed to the client but
	// not cached.
	maxIdempotencyBodyBytes = 1 << 20
)

// cacheableResponseHeaders is the ALLOW-LIST of response headers that are stored
// with a cached response and restored on a replay. It is deliberately an
// allow-list, not a deny-list: the failure mode of forgetting to name a header is
// a missing header on a replay, never a leaked one.
//
// The three entries are the headers that make a replay equivalent to the original
// response — its media type, the resource a command created, and its validator.
// Everything else is dropped on capture: Set-Cookie above all, because a session
// cookie minted for one request and replayed to a different one ten minutes later
// is a session-handling bug with security consequences, but equally
// Authorization, WWW-Authenticate, and any header a future handler invents.
//
// Names are in canonical form (http.CanonicalHeaderKey), which is how net/http
// keys its header maps — note that canonicalizing "ETag" yields "Etag", so that
// is the stored key, and a client reading the replay through http.Header.Get is
// unaffected because lookups canonicalize too.
var cacheableResponseHeaders = []string{
	"Content-Type",
	"Location",
	"Etag",
}

// IdempotencyStore is the subset of ports.IdempotencyStore the command middleware
// depends on. Defining the seam here — next to its sole consumer — mirrors the
// Authenticator/Authorizer/Pinger seams in this package: the middleware depends on
// this interface, not on the concrete postgres adapter, so it stays testable and
// documents exactly which methods it needs. *postgres.IdempotencyStore satisfies
// it structurally; the composition root injects it.
type IdempotencyStore interface {
	Lookup(ctx context.Context, tenantID uuid.UUID, key string) (*ports.CachedResponse, error)
	Record(ctx context.Context, tenantID uuid.UUID, key string,
		requestHash []byte, status int, headers map[string][]string, body []byte) (inserted bool, err error)
}

// idempotencyDeps bundles the middleware's collaborators — the store plus the
// metrics handle the fail-open paths report through — so the resolution helpers take
// one parameter instead of two. It carries no per-request state; it is built once,
// when the middleware is constructed.
//
// There is deliberately no logger here. The fail-open log goes to slog.Default(),
// which is exactly where this package already sends its server-side records (see
// WriteProblem in problem.go): at the composition root that default is the
// trace/tenant-enriching, secret-redacting handler, so the log line correlates with
// the Problem Details records around it instead of forming a second, differently
// configured stream.
type idempotencyDeps struct {
	store   IdempotencyStore
	metrics *idempotencyMetrics
}

// idempotencyMiddleware de-duplicates retried mutating requests (US-03.06,
// Pattern 2 — execute-then-record). It must be ordered AFTER the session and
// tenant-binding middleware: it reads BOTH the tenant and the authenticated
// principal from context and queries on the tenant-bound (opengate_app) pool, so
// either one missing is a mounting error, surfaced as a 500 (never a client 4xx).
//
// NOTE: this middleware is not mounted on any route today — no mutating command
// endpoint exists yet (they arrive in E4). The contract that it MUST be mounted on
// every future mutating route is held by the router tripwire test in
// router_mutating_routes_test.go, which fails the moment such a route is
// registered outside its allow-list.
//
// The method gate makes it safe to mount broadly: GET/HEAD/OPTIONS pass straight
// through, so only POST/PUT/PATCH/DELETE are protected. For a mutating request
// the flow is:
//
//  1. Require an Idempotency-Key header (400 if absent/invalid).
//  2. Buffer the body (<= 1 MiB; 413 if larger), hash it together with the actor
//     id (SHA-256), and replace r.Body with a fresh reader so the handler still
//     reads it.
//  3. Lookup(tenant, key):
//     - hit, hash matches    -> replay the stored response, skip the handler (AC-1).
//     - hit, hash differs    -> 409, the key was reused with a different payload (AC-2).
//     - miss                 -> run the handler, capture the response, and Record it.
//
// The cache key is (tenant_id, idempotency_key), which does NOT scope by actor. The
// actor id is therefore folded into request_hash instead (see idempotencyRequestHash):
// a second caller inside the same tenant who learned the first caller's key cannot
// be served the first caller's response, because the hashes cannot match. The
// resulting 409's Problem Details says the key was reused with a different payload,
// which is imprecise for the cross-actor case — the payload may well be identical —
// but it is deliberately accepted: it fails closed and discloses nothing about the
// other caller or their request.
//
// Only 2xx responses are recorded (see runAndCache). The upfront lookup is what
// makes a retry that arrives after the first request recorded skip the handler
// entirely.
//
// metrics counts the paths that continue WITHOUT idempotency protection rather than
// failing the request (see idempotency_metrics.go). It may be nil, in which case
// nothing is counted; the fail-open logs still go out.
func idempotencyMiddleware(store IdempotencyStore, metrics *idempotencyMetrics) func(http.Handler) http.Handler {
	deps := idempotencyDeps{store: store, metrics: metrics}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Method gate: only mutating methods are idempotency-protected.
			if !isMutatingMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			// 2. The Idempotency-Key header is mandatory on a wrapped mutating endpoint.
			key := r.Header.Get(idempotencyHeader)
			if key == "" || len(key) > maxIdempotencyKeyLen {
				WriteProblem(w, r, errIdempotencyKeyRequired) // 400
				return
			}

			// 3. Buffer the body, then restore r.Body for the handler. A body too large
			//    to hash cannot be idempotency-protected, so reject it (413) rather than
			//    silently skip protection.
			body, tooLarge, err := readLimitedBody(r.Body, maxIdempotencyBodyBytes)
			if err != nil {
				WriteProblem(w, r, fmt.Errorf("idempotency: read request body: %w: %w", apperr.ErrInternal, err))
				return
			}
			if tooLarge {
				WriteProblem(w, r, errRequestBodyTooLarge) // 413
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			// 4. Tenant and actor both come from context (the upstream session/tenant-
			//    binding middleware). Either being absent means this middleware is
			//    mounted in the wrong place — a programming error surfaced as 500.
			tenantID, ok := tenant.IDFromContext(r.Context())
			if !ok {
				WriteProblem(w, r, fmt.Errorf(
					"idempotency: no tenant in context (middleware mounted outside the tenant binding): %w",
					apperr.ErrInternal))
				return
			}
			actorID, err := actorIDFromContext(r.Context())
			if err != nil {
				WriteProblem(w, r, err)
				return
			}
			requestHash := idempotencyRequestHash(actorID, body)

			// 5. Upfront lookup: the device that makes a retry skip the handler.
			cached, err := deps.store.Lookup(r.Context(), uuid.UUID(tenantID), key)
			if err != nil {
				// Fail closed: we cannot tell whether this is a retry, so we must not run
				// the (possibly non-idempotent) handler. 500, handler not invoked. This
				// path needs no fail-open counter: it IS the failure, visible as a 500 in
				// the HTTP metrics and in the Problem Details log line.
				WriteProblem(w, r, err)
				return
			}
			if cached != nil {
				// DEVIATION FROM THE DRAFT, recorded here so the code does not read as
				// though the draft was never consulted. The IETF Idempotency-Key header
				// draft (draft-ietf-httpapi-idempotency-key-header-07, §2.7) recommends
				// 422 Unprocessable Content for a key reused with a DIFFERENT payload,
				// and reserves 409 Conflict for a request retried while the original is
				// still being processed. OpenGate returns 409 for the payload mismatch,
				// deliberately, for two reasons:
				//
				//   - The committed acceptance criteria for US-03.06 specify 409 (AC-2).
				//     The status code is the wire contract clients are written against,
				//     and it was agreed before the draft's recommendation was weighed.
				//   - Pattern 2 never emits the draft's concurrent-in-flight 409. There is
				//     no reservation row, so a racer is not rejected: it runs the handler
				//     and is resolved through the store (see runAndCache). Nothing else in
				//     this API answers 409, which leaves the code unambiguous — here it
				//     means exactly one thing, "this key is already bound to a different
				//     request" — instead of the two meanings the draft's split implies.
				if !bytes.Equal(cached.RequestHash, requestHash) {
					WriteProblem(w, r, errIdempotencyKeyMismatch) // 409 (AC-2)
					return
				}
				writeCachedResponse(w, cached) // replay (AC-1)
				return
			}

			// 6. Miss: run the handler, capture the response, and record it.
			runAndCache(w, r, next, deps, uuid.UUID(tenantID), key, requestHash)
		})
	}
}

// actorIDFromContext returns the authenticated actor's user id from ctx, or an
// internal error when no principal is present.
//
// It FAILS CLOSED by design. Falling back to hashing the body alone would keep
// requests flowing while silently disabling the cross-actor protection — and it
// would do so precisely when the auth chain is misconfigured, which is exactly
// when the protection matters. A nil user id is treated the same way: a principal
// without an identity cannot scope a hash.
func actorIDFromContext(ctx context.Context) (uuid.UUID, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return uuid.Nil, fmt.Errorf(
			"idempotency: no principal in context (middleware mounted outside the session middleware): %w",
			apperr.ErrInternal)
	}
	if principal.UserID == uuid.Nil {
		return uuid.Nil, fmt.Errorf(
			"idempotency: principal in context carries a nil user id: %w", apperr.ErrInternal)
	}
	return principal.UserID, nil
}

// idempotencyRequestHash computes the request fingerprint stored alongside a
// cached response: SHA-256 over the actor id followed by the request body.
//
// Folding the actor in closes a data-leak the (tenant_id, idempotency_key) cache
// key leaves open — the IETF Idempotency-Key draft (§5, Security Considerations)
// recommends exactly this kind of composite key. Within one tenant, a caller who
// obtained another caller's key (from a log, a proxy, an insider) and replayed the
// same body would otherwise be served that caller's cached response, which may
// carry actor-derived data. With the actor in the hash the two fingerprints differ,
// so the replay is refused with a 409 instead.
//
// The actor id is written as its fixed-length 16-byte binary form, so no separator
// is needed: a fixed-width prefix cannot be confused with the start of the body
// the way a variable-length text prefix could.
func idempotencyRequestHash(actorID uuid.UUID, body []byte) []byte {
	h := sha256.New()
	// hash.Hash.Write never returns an error (documented on the interface), so the
	// returns are discarded rather than checked.
	_, _ = h.Write(actorID[:])
	_, _ = h.Write(body)
	return h.Sum(nil)
}

// runAndCache executes the wrapped handler on a capturing recorder and resolves
// what the client receives (the Pattern 2 miss path). It is split out of the
// middleware so the happy-path lookup and this record-and-resolve logic each stay
// simple. The resolution table:
//
//	oversized (> 1 MiB captured)
//	    -> flush as-is; not cached, no re-lookup.
//	2xx
//	    -> Record: on error flush uncached; on inserted flush; on !inserted
//	       re-lookup and, if the stored row's hash matches, flush that instead.
//	non-2xx
//	    -> never recorded; re-lookup and, if the stored row's hash matches, flush
//	       that instead of this request's own response.
//
// Only 2xx is recorded. Caching a transient 5xx would replay that failure to every
// retry for the full retention window — the poison-cache failure mode Pattern 1
// was rejected for — and a 5xx is not evidence that the operation completed, so
// there is nothing to replay. This is a deliberate, narrower rule than the IETF
// draft's §2.6 ("a retry after the original completed gets that result, success or
// error"): "completed" is the load-bearing word, and a transient failure did not.
//
// The re-lookup on a non-2xx is the necessary companion to that rule, not an
// optimization. Two racers under one key: A succeeds and records a 201, while B's
// transaction loses the optimistic-concurrency race and its handler returns 409.
// Without the re-lookup, B's client is told 409 for an operation that DID succeed
// under its own key. With it, B is served A's stored 201.
//
// Both re-lookups verify the stored row's request hash before flushing it. That
// check is not optional: a row that appeared concurrently under a different
// payload (or a different actor) must never be written to this client.
//
// Two of the branches below FAIL OPEN — they return a response while dropping the
// idempotency guarantee — and each is counted (and, where it is a fault, logged) via
// observeFailOpen. See idempotency_metrics.go for why only these need a signal.
func runAndCache(
	w http.ResponseWriter, r *http.Request, next http.Handler,
	deps idempotencyDeps, tenantID uuid.UUID, key string, requestHash []byte,
) {
	rec := newResponseRecorder(w, maxIdempotencyBodyBytes)
	next.ServeHTTP(rec, r)

	// Oversized responses are streamed to the client (already on the wire via the
	// recorder's overflow spill) but never cached: we cannot hold them, and a later
	// retry re-running the handler is the accepted trade-off for a rare large response.
	//
	// COUNTED, not logged. It is a designed outcome rather than a fault, so a log line
	// per request would be pure noise on an endpoint that legitimately returns large
	// responses — but it is the same silent loss of protection as the two faults, and
	// the count is what makes maxIdempotencyBodyBytes tunable against evidence.
	if rec.overflowed() {
		deps.metrics.recordFailOpen(failOpenStageOversize)
		rec.flush() // no-op here: the body already streamed during the spill
		return
	}

	// Non-2xx: never recorded, but a concurrent winner may already have completed
	// this operation under the same key.
	if !isCacheableStatus(rec.status()) {
		flushStoredOrOwn(w, r, rec, deps, tenantID, key, requestHash)
		return
	}

	inserted, err := deps.store.Record(
		r.Context(), tenantID, key, requestHash, rec.status(), rec.cachedHeaders(), rec.cachedBody())
	if err != nil {
		// The handler already ran; only the caching failed. Return its response (do
		// not fail the client and invite a retry of an executed command); it is simply
		// not cached. We cannot un-run a handler that already executed.
		//
		// The command DID execute and its key is now unprotected: a retry will re-run
		// it. That is the whole point of counting this — the outcome is correct and
		// completely invisible from the response.
		deps.observeFailOpen(r, failOpenStageRecord, key, err)
		rec.flush()
		return
	}
	if inserted {
		rec.flush() // we own the row: write the handler's captured response to the client
		return
	}

	// inserted == false: a concurrent request recorded first (Pattern 2). Discard
	// this request's own response and return the stored (winner's) response, so both
	// racers' clients observe one logical result. Do NOT re-run the handler.
	flushStoredOrOwn(w, r, rec, deps, tenantID, key, requestHash)
}

// observeFailOpen counts and logs one fail-open outcome: the request was served
// WITHOUT idempotency protection instead of being failed. It is used for the two
// FAULTS (a Record error, a re-Lookup error); the oversize case is counted directly
// because it is a designed outcome and not worth a per-request log line.
//
// What the record carries, and what it must never carry: the stage, the idempotency
// key, and the error. NOT the request body, NOT the response body, and NOT the
// request hash — the first two are exactly the client and handler data this
// middleware exists to keep out of anything but the cache table, and the hash is a
// fingerprint of them. The TENANT ID is attached by the logging handler from the
// request context (see observability.contextHandler), which is the same mechanism
// renderProblem relies on, so it is not passed again here — doing so would emit the
// key twice in the JSON record.
//
// Error level, not warn: each of these means a command executed while its
// idempotency key was silently left unbound, which is a durable correctness gap for
// that request, not a transient condition that resolved itself.
func (d idempotencyDeps) observeFailOpen(r *http.Request, stage string, key string, cause error) {
	d.metrics.recordFailOpen(stage)
	slog.Default().ErrorContext(r.Context(),
		"idempotency: fail-open, request served without idempotency protection",
		slog.String("stage", stage),
		slog.String("idempotency_key", key),
		slog.String("error", cause.Error()),
	)
}

// flushStoredOrOwn re-reads (tenant, key) and writes the STORED response when one
// exists whose request hash matches this request's; otherwise it writes this
// request's own captured response.
//
// The hash comparison is the safety property: a row written concurrently under a
// different payload or by a different actor must never be flushed to this client,
// so a mismatch (like a fault or an absent row) falls back to the response this
// request actually produced. Nothing here re-runs the handler.
//
// Only the FAULT is a fail-open worth counting. An absent row and a mismatched hash
// are ordinary, correct outcomes — there was nothing better to serve — whereas a
// Lookup error means a concurrent winner's response may exist and this client is
// being given a second, divergent answer instead.
func flushStoredOrOwn(
	w http.ResponseWriter, r *http.Request, rec *responseRecorder,
	deps idempotencyDeps, tenantID uuid.UUID, key string, requestHash []byte,
) {
	cached, err := deps.store.Lookup(r.Context(), tenantID, key)
	switch {
	case err != nil:
		deps.observeFailOpen(r, failOpenStageRelookup, key, err)
		rec.flush()
	case cached == nil || !bytes.Equal(cached.RequestHash, requestHash):
		rec.flush()
	default:
		writeCachedResponse(w, cached)
	}
}

// isCacheableStatus reports whether a response status may be stored: 2xx only.
// See runAndCache for why every other class — a transient 5xx above all — is
// returned to the client uncached.
func isCacheableStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// isMutatingMethod reports whether the HTTP method mutates state and therefore
// warrants idempotency protection. GET/HEAD/OPTIONS (and any other safe method)
// pass through the middleware untouched.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// readLimitedBody buffers body up to limit bytes and reports whether it exceeded
// the limit. It reads one byte past the limit so an over-limit body is detected
// without reading (and buffering) all of it — the cap is a DoS guard, not just a
// cache constraint. The original body is closed once buffered; the caller installs
// a fresh reader over the returned bytes for the handler.
func readLimitedBody(body io.ReadCloser, limit int) (buf []byte, tooLarge bool, err error) {
	if body == nil {
		return nil, false, nil
	}
	defer func() { _ = body.Close() }()

	buf, err = io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(buf) > limit {
		return nil, true, nil // over the limit; the surplus byte is discarded
	}
	return buf, false, nil
}

// whitelistedHeaders returns the subset of src named by cacheableResponseHeaders,
// which is what gets stored with a cached response. Anything not named is dropped
// here, at the single capture point, so an unnamed header can never reach the
// store — see cacheableResponseHeaders for why this is an allow-list.
//
// Values are cloned so the stored map cannot alias (and later observe mutations
// of) the live header map. No whitelisted header present returns nil, which the
// store writes as the column's '{}' default.
func whitelistedHeaders(src http.Header) map[string][]string {
	var out map[string][]string
	for _, name := range cacheableResponseHeaders {
		values, ok := src[name]
		if !ok || len(values) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string][]string, len(cacheableResponseHeaders))
		}
		out[name] = slices.Clone(values)
	}
	return out
}

// writeCachedResponse writes a stored response — its whitelisted headers, status,
// and body — straight to the client. It is used on a replay (no handler ran) and
// when a concurrent loser returns the winner's stored response.
//
// Headers are written BEFORE WriteHeader, as net/http requires: a header set after
// the status line is silently ignored. Restoring them is what makes a replay
// equivalent to the original — without them net/http sniffs the content type off
// the replayed body via http.DetectContentType, and a JSON body sniffs as
// text/plain; charset=utf-8.
//
// The body is a previously-recorded response produced by a trusted application
// handler, written back verbatim — never client input reflected into the
// response. The client-controlled request body is consumed only as a SHA-256 hash
// for the lookup key and is never written here, so the gosec G705 (XSS) taint
// report on the Write is a false positive.
func writeCachedResponse(w http.ResponseWriter, cached *ports.CachedResponse) {
	header := w.Header()
	for name, values := range cached.Headers {
		header[name] = slices.Clone(values)
	}
	w.WriteHeader(cached.Status)
	if len(cached.Body) > 0 {
		// Best-effort: the status line is already sent, so a write failure here just
		// means the client went away (mirrors renderProblem's body-write handling).
		_, _ = w.Write(cached.Body) //nolint:gosec // G705: replays a trusted handler's own response, not reflected client input
	}
}

// responseRecorder wraps the real ResponseWriter to capture a handler's status,
// headers, and body for caching while still delivering the full response to the
// client.
//
// It buffers up to limit bytes WITHOUT writing to the client, so the middleware can
// decide after the handler returns whether to flush the captured response (it won
// the INSERT) or instead return a concurrent winner's stored response. If the body
// exceeds limit it "spills": it flushes the captured headers, status, and buffered
// prefix to the client and switches to live pass-through for the remainder
// (overflow=true), since an oversized response cannot be held in memory — and an
// oversized response is never cached, so the deferred-flush override is not needed
// for it.
//
// The recorder owns a PRIVATE header map rather than exposing the real writer's.
// That is what makes the deferred decision honest: when the middleware discards
// this request's response in favor of a stored one, the handler's headers were
// never copied to the client, so they cannot leak into the replayed response —
// a handler-set Set-Cookie included.
type responseRecorder struct {
	w     http.ResponseWriter
	limit int // maximum bytes to buffer for caching

	hdr            http.Header  // private header map the handler writes into
	statusCode     int          // captured status; HTTP 200 until WriteHeader is called
	headerCaptured bool         // WriteHeader was called (first call wins, as in net/http)
	buf            bytes.Buffer // buffered body, up to limit bytes
	overflow       bool         // body exceeded limit -> not cacheable
	wroteToClient  bool         // the status + body have begun streaming to the client
}

// newResponseRecorder returns a recorder over w that buffers up to limit bytes.
func newResponseRecorder(w http.ResponseWriter, limit int) *responseRecorder {
	return &responseRecorder{w: w, limit: limit, hdr: make(http.Header), statusCode: http.StatusOK}
}

// Header exposes the recorder's OWN header map. The handler's mutations land here
// and reach the client only if the middleware decides to flush this response (see
// commitBufferedToClient); if it instead returns a stored response, they are
// discarded with the rest of the captured response.
func (rec *responseRecorder) Header() http.Header { return rec.hdr }

// WriteHeader captures the status code. While buffering it does NOT forward to the
// client (the flush is deferred); the first call wins, matching net/http.
func (rec *responseRecorder) WriteHeader(code int) {
	if rec.wroteToClient || rec.headerCaptured {
		return
	}
	rec.statusCode = code
	rec.headerCaptured = true
}

// Write buffers up to limit bytes for caching. Once the buffered size would exceed
// limit it spills the captured headers, status, and buffered bytes to the client
// and passes the remainder (and all later writes) straight through.
func (rec *responseRecorder) Write(b []byte) (int, error) {
	if rec.wroteToClient {
		return rec.w.Write(b) // already streaming after an overflow spill
	}
	if rec.buf.Len()+len(b) <= rec.limit {
		return rec.buf.Write(b) // fits in the cache buffer; bytes.Buffer.Write never errors
	}
	// Over the limit: spill what is buffered, then this chunk, and stream the rest.
	rec.overflow = true
	if err := rec.commitBufferedToClient(); err != nil {
		return 0, err
	}
	return rec.w.Write(b)
}

// commitBufferedToClient copies the captured headers onto the real writer, sends
// the captured status line and any buffered body, and marks the response as
// streaming. After it returns, Write passes through and flush is a no-op.
//
// Headers are copied first because net/http ignores any header set after
// WriteHeader. Copying (rather than replacing) preserves anything an outer
// middleware already placed on the real writer.
func (rec *responseRecorder) commitBufferedToClient() error {
	maps.Copy(rec.w.Header(), rec.hdr)
	rec.w.WriteHeader(rec.statusCode)
	rec.wroteToClient = true
	if rec.buf.Len() == 0 {
		return nil
	}
	// The buffered bytes are the wrapped handler's own response, forwarded verbatim
	// — identical to the handler writing directly to the ResponseWriter — so the
	// gosec G705 (XSS) taint report here is a false positive (see writeCachedResponse).
	_, err := rec.w.Write(rec.buf.Bytes()) //nolint:gosec // G705: forwards the wrapped handler's own response bytes
	rec.buf.Reset()
	return err
}

// flush writes the captured (buffered) response to the client. It is the terminal
// step when the middleware keeps the handler's own response. It is a no-op once the
// response has already streamed (the overflow spill path) or already flushed.
func (rec *responseRecorder) flush() {
	if rec.wroteToClient {
		return
	}
	_ = rec.commitBufferedToClient() // best-effort; status is already on the wire
}

// overflowed reports whether the body exceeded the cache limit (and so must not be
// cached). status, cachedHeaders, and cachedBody return the captured status, the
// whitelisted subset of the captured headers, and the buffered body; all three are
// meaningful only when overflowed() is false.
func (rec *responseRecorder) overflowed() bool { return rec.overflow }
func (rec *responseRecorder) status() int      { return rec.statusCode }
func (rec *responseRecorder) cachedHeaders() map[string][]string {
	return whitelistedHeaders(rec.hdr)
}
func (rec *responseRecorder) cachedBody() []byte { return rec.buf.Bytes() }
