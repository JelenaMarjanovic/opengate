package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/application/auth"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// TestIdempotencyMiddlewareIntegration exercises the command idempotency
// middleware (US-03.06, Pattern 2 — execute-then-record) end to end against the
// REAL Postgres adapter on a testcontainers Postgres, closing the acceptance
// criteria at the HTTP boundary. A tenant is injected directly (a small middleware
// standing in for the session/tenant-binding middleware US-02.03 covers end to
// end) so the test isolates the idempotency seam: the upfront lookup, the replay,
// the payload-mismatch 409, the record-on-miss, and the concurrent record race.
//
// One migrated container, one RLS-bound (opengate_app) pool, and one superuser
// pool (for out-of-band row counts — opengate_app holds no SELECT path the test
// needs beyond the adapter, and opengate_bypass is granted only DELETE here) are
// shared across subtests for speed; each subtest uses a fresh tenant id so RLS
// keeps prior subtests' rows invisible and the counts stay independent.
func TestIdempotencyMiddlewareIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	ctx := context.Background()
	container := startIdempotencyPostgres(ctx, t)
	rlsPool := openRLSPool(ctx, t, container)                             // opengate_app, RLS-bound
	superPool := openTestPool(ctx, t, superConnString(ctx, t, container)) // owns the table; counts rows

	// AC-1: the same key + same body twice replays the cached response and runs the
	// handler exactly once. The upfront lookup on the second request hits and the
	// handler is never invoked.
	t.Run("AC-1 replay returns the cache and the handler runs once", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusCreated, body: []byte(`{"id":"abc"}`)}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		body := []byte(`{"name":"widget"}`)

		first := doIdempotentRequest(t, chain, http.MethodPost, key, body)
		second := doIdempotentRequest(t, chain, http.MethodPost, key, body)

		if got := h.count.Load(); got != 1 {
			t.Errorf("handler invocations = %d, want 1 (a replay must not re-run the handler)", got)
		}
		// Status and body equality is the core of the replay; the whitelisted headers
		// are asserted separately by the header-replay subtest below.
		if first.Code != http.StatusCreated || second.Code != first.Code {
			t.Errorf("status: first = %d, second = %d, want both %d", first.Code, second.Code, http.StatusCreated)
		}
		if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
			t.Errorf("replay body mismatch:\n first  = %q\n second = %q", first.Body.Bytes(), second.Body.Bytes())
		}
		if n := countCommandRows(ctx, t, superPool, tenantID); n != 1 {
			t.Errorf("cached rows = %d, want 1", n)
		}
		t.Logf("AC-1: handler invocations=%d (want 1); first=%d second=%d; bodies equal=%t; rows=1",
			h.count.Load(), first.Code, second.Code, bytes.Equal(first.Body.Bytes(), second.Body.Bytes()))
	})

	// AC-2: the same key with a DIFFERENT body is a client error (the key was reused
	// for a different request), answered with 409 Problem Details, and the handler is
	// not invoked on the second call.
	t.Run("AC-2 same key, different body returns 409", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusOK, body: []byte(`{"ok":true}`)}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()

		first := doIdempotentRequest(t, chain, http.MethodPost, key, []byte(`{"name":"a"}`))
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d, want 200; body %s", first.Code, first.Body)
		}

		second := doIdempotentRequest(t, chain, http.MethodPost, key, []byte(`{"name":"DIFFERENT"}`))
		if second.Code != http.StatusConflict {
			t.Fatalf("second status = %d, want 409; body %s", second.Code, second.Body)
		}
		pd := decodeProblemBody(t, second.Body.String())
		if pd.Status != http.StatusConflict {
			t.Errorf("problem status = %d, want 409", pd.Status)
		}
		if ct := second.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("content-type = %q, want application/problem+json", ct)
		}
		if got := h.count.Load(); got != 1 {
			t.Errorf("handler invocations = %d, want 1 (a payload mismatch must not re-run the handler)", got)
		}
		t.Logf("AC-2: second status=%d (Problem Details type=%q); handler invocations=%d (want 1)",
			second.Code, pd.Type, h.count.Load())
	})

	// Concurrent (Pattern 2): two requests with the same key fire concurrently. A
	// barrier in the handler holds both inside it until both have passed the upfront
	// lookup as a miss, forcing the genuine concurrent path — both run the handler,
	// one Record inserts and the other re-looks-up. Both clients receive the SAME
	// stored (winner's) response and exactly one row is cached: the single logical
	// result. Honest Pattern 2 note: execute-then-record runs the handler twice here
	// (the accepted trade-off); the idempotency guarantee is one cached response
	// returned to both clients, not one execution.
	t.Run("concurrent requests return one stored response (Pattern 2)", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{
			status:   http.StatusOK,
			distinct: true, // each invocation writes a per-call unique body
			entered:  make(chan struct{}, 2),
			proceed:  make(chan struct{}),
		}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		body := []byte(`{"name":"concurrent"}`)

		results := make(chan *httptest.ResponseRecorder, 2)
		for i := 0; i < 2; i++ {
			go func() { results <- doIdempotentRequest(t, chain, http.MethodPost, key, body) }()
		}

		// Wait until BOTH requests are inside the handler (both saw a lookup miss)
		// before releasing them, so neither could have recorded before the other looked up.
		for i := 0; i < 2; i++ {
			select {
			case <-h.entered:
			case <-time.After(15 * time.Second):
				t.Fatal("both requests did not enter the handler in time")
			}
		}
		close(h.proceed)

		r1, r2 := <-results, <-results

		if got := h.count.Load(); got != 2 {
			t.Errorf("handler invocations = %d, want 2 (both racers passed the lookup and ran)", got)
		}
		if r1.Code != http.StatusOK || r2.Code != http.StatusOK {
			t.Errorf("status: r1 = %d, r2 = %d, want both 200", r1.Code, r2.Code)
		}
		if !bytes.Equal(r1.Body.Bytes(), r2.Body.Bytes()) {
			t.Errorf("concurrent clients got different bodies (both must be the stored winner):\n r1 = %q\n r2 = %q",
				r1.Body.Bytes(), r2.Body.Bytes())
		}
		if n := countCommandRows(ctx, t, superPool, tenantID); n != 1 {
			t.Errorf("cached rows = %d, want exactly 1 (one logical result)", n)
		}
		t.Logf("Pattern 2: handler invocations=%d (both ran); both clients body=%q; cached rows=1 (one stored response)",
			h.count.Load(), r1.Body.Bytes())
	})

	// A mutating request with NO Idempotency-Key is a 400 Problem Details and the
	// handler is never invoked (the contract requires the header on wrapped endpoints).
	t.Run("missing Idempotency-Key returns 400, handler not called", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusOK, body: []byte(`{}`)}
		chain := newIdempotencyChain(store, tenantID, h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, "", []byte(`{"name":"x"}`)) // no key

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
		}
		pd := decodeProblemBody(t, rec.Body.String())
		if pd.Status != http.StatusBadRequest {
			t.Errorf("problem status = %d, want 400", pd.Status)
		}
		if got := h.count.Load(); got != 0 {
			t.Errorf("handler invocations = %d, want 0 (a missing key must not run the handler)", got)
		}
	})

	// A request body over 1 MiB is a 413 Problem Details and the handler is never
	// invoked: a body too large to hash cannot be idempotency-protected.
	t.Run("request body over 1 MiB returns 413, handler not called", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusOK, body: []byte(`{}`)}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		big := bytes.Repeat([]byte("a"), maxIdempotencyBodyBytes+1) // 1 MiB + 1

		rec := doIdempotentRequest(t, chain, http.MethodPost, key, big)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body %s", rec.Code, rec.Body)
		}
		pd := decodeProblemBody(t, rec.Body.String())
		if pd.Status != http.StatusRequestEntityTooLarge {
			t.Errorf("problem status = %d, want 413", pd.Status)
		}
		if got := h.count.Load(); got != 0 {
			t.Errorf("handler invocations = %d, want 0 (an oversized body must not run the handler)", got)
		}
	})

	// Mutating-only: a GET passes straight through (handler runs) and writes no
	// idempotency row — the middleware is safe to mount on a mixed route group.
	t.Run("GET passes through with no idempotency row", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusOK, body: []byte(`ok`)}
		chain := newIdempotencyChain(store, tenantID, h)

		rec := doIdempotentRequest(t, chain, http.MethodGet, "", nil) // safe method, no key

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
		}
		if got := h.count.Load(); got != 1 {
			t.Errorf("handler invocations = %d, want 1 (a GET must pass through to the handler)", got)
		}
		if n := countCommandRows(ctx, t, superPool, tenantID); n != 0 {
			t.Errorf("cached rows = %d, want 0 (a safe method must not be recorded)", n)
		}
	})

	// A response over 1 MiB is returned in full to the client but not cached: a
	// later retry re-runs the handler precisely because nothing was stored.
	t.Run("response over 1 MiB is returned but not cached", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		big := bytes.Repeat([]byte("b"), maxIdempotencyBodyBytes+100) // > 1 MiB
		h := &countingHandler{status: http.StatusOK, body: big}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		reqBody := []byte(`{"name":"big"}`)

		first := doIdempotentRequest(t, chain, http.MethodPost, key, reqBody)
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d, want 200", first.Code)
		}
		if first.Body.Len() != len(big) {
			t.Errorf("response body length = %d, want %d (the full oversized body must reach the client)",
				first.Body.Len(), len(big))
		}
		if n := countCommandRows(ctx, t, superPool, tenantID); n != 0 {
			t.Errorf("cached rows = %d, want 0 (an oversized response must not be cached)", n)
		}

		// The second identical request re-runs the handler because the upfront lookup
		// misses again (nothing was cached).
		second := doIdempotentRequest(t, chain, http.MethodPost, key, reqBody)
		if second.Code != http.StatusOK || second.Body.Len() != len(big) {
			t.Errorf("second response = {status:%d len:%d}, want {200, %d}", second.Code, second.Body.Len(), len(big))
		}
		if got := h.count.Load(); got != 2 {
			t.Errorf("handler invocations = %d, want 2 (an uncached oversized response must re-run on retry)", got)
		}
		t.Logf("oversized response: first len=%d (full body delivered); cached rows=0; retry invocations=%d (want 2)",
			first.Body.Len(), h.count.Load())
	})

	// B2/B3: a replay reproduces the whitelisted response headers, so it is
	// EQUIVALENT to the original rather than merely sharing its status and body.
	// Before the whitelist landed the table stored status + body only, so the replay
	// carried no headers at all and net/http sniffed a content type off the body —
	// a JSON body sniffing as text/plain; charset=utf-8.
	t.Run("replay reproduces the whitelisted response headers", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{
			status: http.StatusCreated,
			body:   []byte(`{"id":"abc"}`),
			headers: map[string]string{
				"Location": "/api/v1/members/018f",
				"ETag":     `W/"7"`,
			},
		}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		body := []byte(`{"name":"widget"}`)

		first := doIdempotentRequest(t, chain, http.MethodPost, key, body)
		second := doIdempotentRequest(t, chain, http.MethodPost, key, body)

		if got := h.count.Load(); got != 1 {
			t.Fatalf("handler invocations = %d, want 1 (the second call must be a replay)", got)
		}
		for _, name := range []string{"Content-Type", "Location", "ETag"} {
			original, replayed := first.Header().Get(name), second.Header().Get(name)
			if replayed != original {
				t.Errorf("replayed %s = %q, want %q (same as the original)", name, replayed, original)
			}
		}
		t.Logf("header replay: Content-Type=%q Location=%q ETag=%q (original vs replay identical)",
			second.Header().Get("Content-Type"), second.Header().Get("Location"), second.Header().Get("ETag"))
	})

	// B3: the whitelist is an ALLOW-LIST, and this case pins the TWO DISTINCT
	// PROPERTIES it produces — which are easy to conflate and have opposite failure
	// modes:
	//
	//	DELIVERED IN FULL     the original response carries every header the handler
	//	                      set, Set-Cookie included. commitBufferedToClient copies
	//	                      the recorder's ENTIRE header map to the real writer.
	//	CACHED SELECTIVELY    only cachedHeaders() applies the allow-list, so nothing
	//	                      unnamed is written to the store or reproduced on a replay.
	//
	// The allow-list therefore governs what is CACHED, never what is DELIVERED. Get
	// the second property wrong and a session cookie minted for one request is
	// replayed to another ten minutes later — a session-handling bug with security
	// consequences. Get the FIRST one wrong (by "simplifying" commitBufferedToClient
	// to copy only the whitelist, which looks like a tidy-up) and login silently stops
	// setting its session cookie, with no test failing anywhere. Both are asserted
	// below, on one request pair, precisely because the difference is the point.
	//
	// The stored jsonb is inspected directly for the second property — asserting only
	// on the replayed response would not prove the value never reached the database.
	t.Run("Set-Cookie is neither stored nor replayed", func(t *testing.T) {
		const handlerCookie = "og_session=super-secret; Path=/; HttpOnly"

		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{
			status:  http.StatusOK,
			body:    []byte(`{"ok":true}`),
			headers: map[string]string{"Set-Cookie": handlerCookie},
		}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		body := []byte(`{"name":"widget"}`)

		first := doIdempotentRequest(t, chain, http.MethodPost, key, body)
		second := doIdempotentRequest(t, chain, http.MethodPost, key, body)

		// Property 1 — DELIVERED IN FULL. Compared by exact value, not merely for
		// presence: the header must arrive unaltered on the request that actually ran.
		if got := first.Header().Get("Set-Cookie"); got != handlerCookie {
			t.Fatalf("original response Set-Cookie = %q, want %q; a non-whitelisted header "+
				"must still reach the client on the original request (the allow-list caps what is "+
				"CACHED, not what is DELIVERED)", got, handlerCookie)
		}
		// Property 2 — CACHED SELECTIVELY: absent from the replay, and absent from the row.
		if got := second.Header().Get("Set-Cookie"); got != "" {
			t.Errorf("replayed Set-Cookie = %q, want empty (a cached cookie must never be replayed)", got)
		}
		stored := readStoredHeaders(ctx, t, superPool, tenantID, key)
		if strings.Contains(strings.ToLower(stored), "cookie") || strings.Contains(stored, "super-secret") {
			t.Errorf("stored response_headers = %s, want no Set-Cookie (the allow-list must drop it before the write)", stored)
		}
		t.Logf("Set-Cookie: original Set-Cookie=%q (delivered in full); replayed Set-Cookie=%q (empty); "+
			"stored response_headers=%s (cached selectively)",
			first.Header().Get("Set-Cookie"), second.Header().Get("Set-Cookie"), stored)
	})

	// B1: a 500 is returned to the client but NEVER cached, and a retry with the same
	// key re-invokes the handler. Caching a transient 5xx would replay that failure to
	// every retry for the full retention window — the poison-cache failure mode
	// Pattern 1 was rejected for.
	t.Run("a 500 is not cached and a retry re-invokes the handler", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusInternalServerError, body: []byte(`{"error":"transient"}`)}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		body := []byte(`{"name":"widget"}`)

		first := doIdempotentRequest(t, chain, http.MethodPost, key, body)
		if first.Code != http.StatusInternalServerError {
			t.Fatalf("first status = %d, want 500; body %s", first.Code, first.Body)
		}
		if n := countCommandRows(ctx, t, superPool, tenantID); n != 0 {
			t.Fatalf("cached rows after a 500 = %d, want 0 (a 5xx must never be cached)", n)
		}

		second := doIdempotentRequest(t, chain, http.MethodPost, key, body)
		if second.Code != http.StatusInternalServerError {
			t.Errorf("retry status = %d, want 500", second.Code)
		}
		if got := h.count.Load(); got != 2 {
			t.Errorf("handler invocations = %d, want 2 (an uncached 500 must re-run on retry)", got)
		}
		t.Logf("500 not cached: first=%d rows=0; retry=%d invocations=%d (want 2)",
			first.Code, second.Code, h.count.Load())
	})

	// B1, same rule for a 4xx: only 2xx is recorded. A 422 from a validation failure
	// is not a completed operation either, and the client fixing its payload must
	// reach the handler again — which it can, because the key was never bound to the
	// rejected attempt.
	t.Run("a 4xx is not cached and a retry re-invokes the handler", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusUnprocessableEntity, body: []byte(`{"error":"invalid"}`)}
		chain := newIdempotencyChain(store, tenantID, h)
		key := uuid.NewString()
		body := []byte(`{"name":"widget"}`)

		first := doIdempotentRequest(t, chain, http.MethodPost, key, body)
		second := doIdempotentRequest(t, chain, http.MethodPost, key, body)

		if first.Code != http.StatusUnprocessableEntity || second.Code != http.StatusUnprocessableEntity {
			t.Errorf("status: first = %d, second = %d, want both 422", first.Code, second.Code)
		}
		if n := countCommandRows(ctx, t, superPool, tenantID); n != 0 {
			t.Errorf("cached rows after a 422 = %d, want 0", n)
		}
		if got := h.count.Load(); got != 2 {
			t.Errorf("handler invocations = %d, want 2 (an uncached 4xx must re-run on retry)", got)
		}
	})

	// B4 SECURITY: the cache key is (tenant_id, idempotency_key) with no actor
	// scoping, so within one tenant a caller who learned another caller's key could
	// otherwise replay it and be served THAT caller's response — which may carry
	// actor-derived data. The actor id is folded into request_hash, so the second
	// actor's hash cannot match and the replay is refused with a 409.
	//
	// Note the request body is IDENTICAL between the two actors: the only difference
	// is who is asking, which is precisely what must be enough to refuse.
	t.Run("a second actor reusing the first actor's key is refused, not served their response", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		actorA, actorB := uuid.New(), uuid.New()
		hA := &countingHandler{status: http.StatusCreated, body: []byte(`{"secret":"actor-A-data"}`)}
		hB := &countingHandler{status: http.StatusCreated, body: []byte(`{"secret":"actor-B-data"}`)}
		chainA := newIdempotencyChainAs(store, tenantID, actorA, hA)
		chainB := newIdempotencyChainAs(store, tenantID, actorB, hB)
		key := uuid.NewString()
		body := []byte(`{"name":"widget"}`) // the SAME payload for both actors

		first := doIdempotentRequest(t, chainA, http.MethodPost, key, body)
		if first.Code != http.StatusCreated {
			t.Fatalf("actor A status = %d, want 201; body %s", first.Code, first.Body)
		}

		second := doIdempotentRequest(t, chainB, http.MethodPost, key, body)
		if second.Code != http.StatusConflict {
			t.Fatalf("actor B status = %d, want 409; body %s", second.Code, second.Body)
		}
		if bytes.Contains(second.Body.Bytes(), []byte("actor-A-data")) {
			t.Error("actor B's response leaked actor A's cached body")
		}
		if got := hB.count.Load(); got != 0 {
			t.Errorf("actor B handler invocations = %d, want 0 (the mismatch is decided before the handler)", got)
		}
		pd := decodeProblemBody(t, second.Body.String())
		if pd.Status != http.StatusConflict {
			t.Errorf("problem status = %d, want 409", pd.Status)
		}
		t.Logf("cross-actor: actor B status=%d (Problem Details type=%q); A's body leaked=%t; B handler invocations=%d",
			second.Code, pd.Type, bytes.Contains(second.Body.Bytes(), []byte("actor-A-data")), hB.count.Load())
	})

	// B4 fail-closed: no principal in context means the middleware is mounted outside
	// (or ahead of) the session middleware. That is a programming error, surfaced as a
	// 500 with the handler never invoked — exactly as a missing tenant already is.
	// Falling back to hashing the body alone would silently disable the cross-actor
	// protection precisely when the auth chain is misconfigured.
	t.Run("no actor in context returns 500 and the handler is not called", func(t *testing.T) {
		tenantID := uuid.New()
		store := postgres.NewIdempotencyStore(rlsPool)
		h := &countingHandler{status: http.StatusOK, body: []byte(`{}`)}
		// Tenant but NO principal: the misconfigured chain.
		chain := injectTenant(tenantID)(idempotencyMiddleware(store, nil)(h))

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"x"}`))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body)
		}
		if got := h.count.Load(); got != 0 {
			t.Errorf("handler invocations = %d, want 0 (a missing actor must fail closed)", got)
		}
		if n := countCommandRows(ctx, t, superPool, tenantID); n != 0 {
			t.Errorf("cached rows = %d, want 0", n)
		}
		t.Logf("fail-closed: status=%d; handler invocations=%d (want 0)", rec.Code, h.count.Load())
	})
}

// countingHandler is the observable test handler: every invocation increments an
// atomic counter so a test can assert the handler ran exactly N times. It writes a
// fixed body, or — when distinct is set — a per-invocation unique body so the
// concurrent test can prove both clients received the SAME stored (winner's) one.
// The optional entered/proceed channels form a barrier the concurrent test uses to
// hold both requests inside the handler until both have passed the upfront lookup.
type countingHandler struct {
	count    atomic.Int32
	status   int               // status to write; 0 means leave net/http's implicit 200
	body     []byte            // fixed response body (used when distinct is false)
	distinct bool              // when true, write a per-invocation unique body instead of body
	headers  map[string]string // extra response headers set before the status line

	entered chan struct{} // receives a signal as each invocation enters (nil => no signal)
	proceed chan struct{} // each invocation blocks until this is closed (nil => no block)
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	n := h.count.Add(1)
	if h.entered != nil {
		h.entered <- struct{}{}
	}
	if h.proceed != nil {
		<-h.proceed
	}
	w.Header().Set("Content-Type", "application/json")
	for name, value := range h.headers {
		w.Header().Set(name, value)
	}
	if h.status != 0 {
		w.WriteHeader(h.status)
	}
	if h.distinct {
		_, _ = fmt.Fprintf(w, `{"invocation":%d}`, n)
		return
	}
	_, _ = w.Write(h.body)
}

// newIdempotencyChain wraps h in the production-shaped chain under a fresh actor.
// Most subtests exercise a single caller, so the actor identity is incidental to
// them; the cross-actor test uses newIdempotencyChainAs to pin it.
func newIdempotencyChain(store IdempotencyStore, tenantID uuid.UUID, h http.Handler) http.Handler {
	return newIdempotencyChainAs(store, tenantID, uuid.New(), h)
}

// newIdempotencyChainAs wraps h in the production-shaped chain: the session
// injector (standing in for the session/tenant-binding middleware) followed by the
// idempotency middleware. The injector runs FIRST so the middleware sees both the
// tenant and the principal in context, and the RLS-bound pool's bind hook sees the
// tenant.
// The metrics handle is nil: these subtests assert BEHAVIOR against the real store,
// and the middleware tolerates a nil handle by recording nothing. The fail-open
// counter itself is asserted in TestIdempotencyFailOpenCounter, over an isolated
// registry and a scripted store, because faults are not reproducible against a
// healthy container.
func newIdempotencyChainAs(store IdempotencyStore, tenantID, actorID uuid.UUID, h http.Handler) http.Handler {
	return injectSession(tenantID, actorID)(idempotencyMiddleware(store, nil)(h))
}

// injectSession is a test middleware mimicking the two things the upstream session
// middleware does that the idempotency middleware depends on: it places the tenant
// id in the request context (which both the middleware and the RLS-bound pool's
// bind hook read) and the authenticated Principal (whose UserID the middleware
// folds into the request hash).
func injectSession(tenantID, actorID uuid.UUID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := tenant.NewContext(r.Context(), tenant.ID(tenantID))
			ctx = contextWithPrincipal(ctx, auth.Principal{TenantID: tenantID, UserID: actorID})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// injectTenant places ONLY the tenant in context — no principal. It stands in for a
// misconfigured chain (the idempotency middleware mounted outside, or ahead of, the
// session middleware) so the fail-closed actor check can be exercised.
func injectTenant(tenantID uuid.UUID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := tenant.NewContext(r.Context(), tenant.ID(tenantID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// doIdempotentRequest issues one request through h and returns the recorder. A
// non-empty key sets the Idempotency-Key header; a nil body sends no body. It does
// not call t.Fatal, so it is safe to invoke from a goroutine (the concurrent test).
func doIdempotentRequest(t *testing.T, h http.Handler, method, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "/resource", reader)
	if key != "" {
		req.Header.Set(idempotencyHeader, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// readStoredHeaders returns the raw response_headers jsonb document stored for
// (tenant, key), as text. It reads through the superuser pool so the assertion sees
// what actually landed in the column, not what the middleware chose to replay —
// the difference matters for the Set-Cookie negative, which must prove the value
// never reached the database at all.
func readStoredHeaders(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, key string) string {
	t.Helper()
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT response_headers::text FROM command_idempotency_keys
		 WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key).Scan(&stored); err != nil {
		t.Fatalf("read stored response_headers: %v", err)
	}
	return stored
}

// countCommandRows counts the cached rows for one tenant via the superuser pool,
// which owns command_idempotency_keys and so may SELECT it (the application roles
// hold only the SELECT+INSERT used through the adapter and a bypass DELETE). The
// tenant predicate scopes the count to the subtest's tenant.
func countCommandRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM command_idempotency_keys WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count command_idempotency_keys: %v", err)
	}
	return n
}

// --- Container + pool helpers ----------------------------------------------

// startIdempotencyPostgres starts a throwaway Postgres with every embedded
// migration applied as the superuser: create_app_roles (the opengate_app role the
// RLS pool connects as) plus the commit-1 command_idempotency_keys table, its RLS
// policy, and the opengate_app SELECT+INSERT grant. It reuses the package's shared
// migrate-up.
func startIdempotencyPostgres(ctx context.Context, t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	container := testsupport.StartPostgres(ctx, t)
	migrateAuthzUp(ctx, t, superConnString(ctx, t, container))
	return container
}

// openRLSPool opens the RLS-bound pool as opengate_app, with the real
// tenant-binding hooks installed (postgres.NewPool). A discard logger is used
// because the test always supplies a tenant, so the bind path emits no warning.
func openRLSPool(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.NewPool(ctx, appRoleDSN(ctx, t, c), observability.NewLogger(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("open RLS pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// appRoleDSN builds the opengate_app connection string from the container's
// host/port and the well-known app credentials created by create_app_roles (user
// opengate_app, password 'placeholder').
func appRoleDSN(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
	t.Helper()
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return fmt.Sprintf("postgres://opengate_app:placeholder@%s:%s/opengate_test?sslmode=disable",
		host, port.Port())
}
