package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// fakeIdempotencyStore is a scripted IdempotencyStore for the paths the
// container-backed integration test cannot drive deterministically: a store fault,
// and the Pattern 2 concurrent case where a competing request records BETWEEN this
// request's upfront lookup and its own resolution.
//
// lookups is consumed one entry per Lookup call, so a test can say "miss first,
// then a winner's row" — the exact interleaving the re-lookup exists for. Racing
// two real requests could produce that interleaving, but not reliably; scripting it
// makes the assertion deterministic.
type fakeIdempotencyStore struct {
	lookups     []lookupResult // consumed in order; the final entry repeats once exhausted
	lookupCalls int

	recordInserted bool
	recordErr      error
	recordCalls    int
	// recorded captures the arguments of the last Record call, so a test can assert
	// what would have been persisted (notably: which headers survived the allow-list).
	recorded recordedCall
}

// lookupResult is one scripted Lookup outcome: a cached response, an error, or
// neither (a miss).
type lookupResult struct {
	cached *ports.CachedResponse
	err    error
}

// recordedCall is the argument set of one Record call.
type recordedCall struct {
	status  int
	headers map[string][]string
	body    []byte
	hash    []byte
}

func (f *fakeIdempotencyStore) Lookup(_ context.Context, _ uuid.UUID, _ string) (*ports.CachedResponse, error) {
	idx := f.lookupCalls
	f.lookupCalls++
	if len(f.lookups) == 0 {
		return nil, nil // never scripted: always a miss
	}
	if idx >= len(f.lookups) {
		idx = len(f.lookups) - 1 // past the script: repeat the final outcome
	}
	return f.lookups[idx].cached, f.lookups[idx].err
}

func (f *fakeIdempotencyStore) Record(
	_ context.Context, _ uuid.UUID, _ string,
	requestHash []byte, status int, headers map[string][]string, body []byte,
) (bool, error) {
	f.recordCalls++
	f.recorded = recordedCall{status: status, headers: headers, body: body, hash: requestHash}
	return f.recordInserted, f.recordErr
}

// newFakeChainAs wraps h in the production-shaped chain over a fake store under an
// explicit actor. The actor is explicit because the request hash covers it, so a
// test that scripts a MATCHING stored row must be able to compute the same hash the
// middleware will (via idempotencyRequestHash).
//
// It passes no metrics handle, which also exercises the documented nil tolerance:
// every behavioral assertion below must hold whether or not a registry is wired. The
// fail-open counter tests supply a real handle via newFakeChainWithMetrics.
func newFakeChainAs(store IdempotencyStore, actorID uuid.UUID, h http.Handler) http.Handler {
	return newFakeChainWithMetrics(store, actorID, nil, h)
}

// newFakeChainWithMetrics is newFakeChainAs with the metrics handle injected, for
// the tests that assert on the fail-open counter.
func newFakeChainWithMetrics(
	store IdempotencyStore, actorID uuid.UUID, metrics *idempotencyMetrics, h http.Handler,
) http.Handler {
	return injectSession(uuid.New(), actorID)(idempotencyMiddleware(store, metrics)(h))
}

// newIsolatedIdempotencyMetrics builds a metrics handle over a FRESH registry, never
// prometheus.DefaultRegisterer. Isolation is what lets each subtest read absolute
// counter values instead of deltas, and keeps one subtest's increments out of
// another's assertions (US-03.05 metrics pattern).
func newIsolatedIdempotencyMetrics(t *testing.T) *idempotencyMetrics {
	t.Helper()
	metrics, err := newIdempotencyMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("build idempotency metrics: %v", err)
	}
	return metrics
}

// assertFailOpen asserts the exact fail-open count for every stage, so a test proves
// both that ITS stage was counted and that the other two were not — which is what
// makes the stage label meaningful rather than decorative.
func assertFailOpen(t *testing.T, metrics *idempotencyMetrics, want map[string]float64) {
	t.Helper()
	for _, stage := range failOpenStages {
		got := testutil.ToFloat64(metrics.failOpen.WithLabelValues(stage))
		if got != want[stage] {
			t.Errorf("%s{stage=%q} = %v, want %v", failOpenMetricName, stage, got, want[stage])
		}
	}
}

// TestIdempotencyMiddlewareWithFakeStore covers the middleware's store-dependent
// branches with a scripted store: the concurrent re-lookup (and its hash guard),
// the two error paths, and the capture-time header allow-list. These are unit tests
// — no container — so they run in short mode alongside the rest of the package.
func TestIdempotencyMiddlewareWithFakeStore(t *testing.T) {
	// B1 re-lookup: the handler's transaction lost an optimistic-concurrency race
	// and returned 409, but a concurrent winner already recorded a 201 under the SAME
	// key and hash. The operation DID succeed under this request's key, so the client
	// must receive the stored 201 — not the handler's conflict error. Without this
	// re-lookup, dropping Record on a non-2xx would have told the client 409 for an
	// operation that succeeded.
	t.Run("a non-2xx is replaced by a concurrent winner's stored response", func(t *testing.T) {
		actorID := uuid.New()
		body := []byte(`{"name":"widget"}`)
		winner := &ports.CachedResponse{
			Status:      http.StatusCreated,
			Headers:     map[string][]string{"Content-Type": {"application/json"}},
			Body:        []byte(`{"id":"winner"}`),
			RequestHash: idempotencyRequestHash(actorID, body), // the SAME fingerprint
		}
		store := &fakeIdempotencyStore{lookups: []lookupResult{
			{},               // upfront lookup: miss, so the handler runs
			{cached: winner}, // the re-lookup: the concurrent winner's row
		}}
		h := &countingHandler{status: http.StatusConflict, body: []byte(`{"error":"version conflict"}`)}
		chain := newFakeChainAs(store, actorID, h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), body)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (the concurrent winner's stored response); body %s", rec.Code, rec.Body)
		}
		if got := rec.Body.String(); got != `{"id":"winner"}` {
			t.Errorf("body = %q, want the winner's stored body", got)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json (from the stored headers)", got)
		}
		if store.recordCalls != 0 {
			t.Errorf("Record calls = %d, want 0 (a non-2xx is never recorded)", store.recordCalls)
		}
		if got := h.count.Load(); got != 1 {
			t.Errorf("handler invocations = %d, want 1", got)
		}
		t.Logf("re-lookup: handler returned 409; client received status=%d body=%q; Record calls=%d",
			rec.Code, rec.Body.String(), store.recordCalls)
	})

	// B1 hash guard: the same interleaving, but the row that appeared concurrently
	// carries a DIFFERENT hash — a different payload, or a different actor, under the
	// same key. It must never be flushed to this client, so the middleware falls back
	// to this request's own response.
	t.Run("a concurrent row with a different hash is never flushed to the client", func(t *testing.T) {
		store := &fakeIdempotencyStore{lookups: []lookupResult{
			{}, // upfront lookup: miss
			{cached: &ports.CachedResponse{
				Status:      http.StatusCreated,
				Body:        []byte(`{"id":"someone-elses"}`),
				RequestHash: []byte("a-hash-that-does-not-match"),
			}},
		}}
		h := &countingHandler{status: http.StatusConflict, body: []byte(`{"error":"version conflict"}`)}
		chain := newFakeChainAs(store, uuid.New(), h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"widget"}`))

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (the handler's own response); body %s", rec.Code, rec.Body)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("someone-elses")) {
			t.Error("a stored row with a mismatched hash leaked to the client")
		}
		t.Logf("hash guard: client received status=%d body=%q (its own, not the mismatched row)",
			rec.Code, rec.Body.String())
	})

	// A 2xx whose Record loses the insert race (inserted=false) and whose winner row
	// carries a matching hash: the client gets the winner's response and the handler
	// is not re-run. This is the original Pattern 2 path, now sharing one resolution
	// helper with the non-2xx case above.
	t.Run("a 2xx that loses the insert race returns the winner's stored response", func(t *testing.T) {
		actorID := uuid.New()
		body := []byte(`{"name":"widget"}`)
		store := &fakeIdempotencyStore{
			recordInserted: false,
			lookups: []lookupResult{
				{}, // upfront lookup: miss
				{cached: &ports.CachedResponse{
					Status:      http.StatusCreated,
					Body:        []byte(`{"id":"winner"}`),
					RequestHash: idempotencyRequestHash(actorID, body),
				}},
			},
		}
		h := &countingHandler{status: http.StatusCreated, body: []byte(`{"id":"mine"}`)}
		chain := newFakeChainAs(store, actorID, h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), body)

		if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":"winner"}` {
			t.Errorf(`response = {status:%d body:%q}, want the winner's {201 {"id":"winner"}}`,
				rec.Code, rec.Body.String())
		}
		if got := h.count.Load(); got != 1 {
			t.Errorf("handler invocations = %d, want 1 (the loser must NOT re-run the handler)", got)
		}
	})

	// A Lookup fault is fail-closed: we cannot tell whether this is a retry, so the
	// possibly non-idempotent handler must not run. 500, handler not invoked.
	t.Run("a Lookup error is a 500 and the handler is not invoked", func(t *testing.T) {
		store := &fakeIdempotencyStore{lookups: []lookupResult{
			{err: fmt.Errorf("lookup idempotency key: %w: connection reset", apperr.ErrInternal)},
		}}
		h := &countingHandler{status: http.StatusOK, body: []byte(`{}`)}
		chain := newFakeChainAs(store, uuid.New(), h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"widget"}`))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body)
		}
		if got := h.count.Load(); got != 0 {
			t.Errorf("handler invocations = %d, want 0 (a store fault must fail closed)", got)
		}
		if store.recordCalls != 0 {
			t.Errorf("Record calls = %d, want 0", store.recordCalls)
		}
	})

	// A Record fault is the mirror image: the handler ALREADY ran, so its response is
	// returned (simply uncached) rather than turned into a 500 that would invite the
	// client to retry an executed command. We cannot un-run a handler.
	t.Run("a Record error returns the handler's own response, uncached", func(t *testing.T) {
		store := &fakeIdempotencyStore{
			recordErr: fmt.Errorf("record idempotency key: %w: write failed", apperr.ErrInternal),
		}
		h := &countingHandler{status: http.StatusCreated, body: []byte(`{"id":"abc"}`)}
		chain := newFakeChainAs(store, uuid.New(), h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"widget"}`))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (the handler's own response); body %s", rec.Code, rec.Body)
		}
		if got := rec.Body.String(); got != `{"id":"abc"}` {
			t.Errorf("body = %q, want the handler's own", got)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json (the handler's own header must still reach the client)", got)
		}
		if got := h.count.Load(); got != 1 {
			t.Errorf("handler invocations = %d, want 1", got)
		}
	})

	// The allow-list is applied at CAPTURE time, before anything reaches the store:
	// the arguments Record receives already exclude Set-Cookie. Asserting here rather
	// than on the replayed response proves the filtering is not merely a read-side
	// omission.
	t.Run("Record receives only whitelisted headers", func(t *testing.T) {
		store := &fakeIdempotencyStore{recordInserted: true}
		h := &countingHandler{
			status: http.StatusCreated,
			body:   []byte(`{"id":"abc"}`),
			headers: map[string]string{
				"Location":         "/api/v1/members/018f",
				"ETag":             `W/"7"`,
				"Set-Cookie":       "og_session=secret",
				"X-Internal-Debug": "leak-me",
			},
		}
		chain := newFakeChainAs(store, uuid.New(), h)

		doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"widget"}`))

		if store.recordCalls != 1 {
			t.Fatalf("Record calls = %d, want 1", store.recordCalls)
		}
		got := store.recorded.headers
		want := map[string][]string{
			"Content-Type": {"application/json"},
			"Location":     {"/api/v1/members/018f"},
			"Etag":         {`W/"7"`}, // http.CanonicalHeaderKey("ETag") is "Etag"
		}
		if len(got) != len(want) {
			t.Fatalf("recorded headers = %v, want exactly %v", got, want)
		}
		for name, values := range want {
			if len(got[name]) != 1 || got[name][0] != values[0] {
				t.Errorf("recorded header %s = %v, want %v", name, got[name], values)
			}
		}
		for _, forbidden := range []string{"Set-Cookie", "X-Internal-Debug"} {
			if _, present := got[http.CanonicalHeaderKey(forbidden)]; present {
				t.Errorf("recorded headers contain %s, which is not on the allow-list", forbidden)
			}
		}
		// The recorded fingerprint is a SHA-256 digest (32 bytes) over the actor id
		// and the body — never the bare body hash.
		if len(store.recorded.hash) != sha256DigestLen {
			t.Errorf("recorded hash length = %d, want %d", len(store.recorded.hash), sha256DigestLen)
		}
	})
}

// TestIdempotencyFailOpenCounter covers the three paths that continue WITHOUT
// idempotency protection rather than failing the request. Each is correct — a
// handler that already ran cannot be un-run, and an oversized response cannot be
// held — and each was previously silent, which is the whole problem: the request
// succeeds, the client sees nothing wrong, and a guarantee is gone.
//
// Every subtest asserts the count for ALL THREE stages, not just its own. A counter
// that increments on the right event but under the wrong label is worse than no
// counter, because a dashboard reads it as evidence.
//
// The fail-CLOSED path (a failed upfront Lookup) is deliberately absent here: it
// returns a 500 without running the handler, so it is already visible in the HTTP
// metrics and in the Problem Details log line, and is covered as a behavior by
// TestIdempotencyMiddlewareWithFakeStore.
func TestIdempotencyFailOpenCounter(t *testing.T) {
	// stage=record: Record failed after the handler ran. The command executed and its
	// key is now unbound, so a retry will execute it a second time.
	t.Run("a Record error increments stage=record", func(t *testing.T) {
		metrics := newIsolatedIdempotencyMetrics(t)
		store := &fakeIdempotencyStore{
			recordErr: fmt.Errorf("record idempotency key: %w: write failed", apperr.ErrInternal),
		}
		h := &countingHandler{status: http.StatusCreated, body: []byte(`{"id":"abc"}`)}
		chain := newFakeChainWithMetrics(store, uuid.New(), metrics, h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"widget"}`))

		// The response is unchanged by the instrumentation: counting a fail-open must
		// never turn it into a failure.
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (the handler's own response, uncached); body %s", rec.Code, rec.Body)
		}
		assertFailOpen(t, metrics, map[string]float64{
			failOpenStageRecord:   1,
			failOpenStageRelookup: 0,
			failOpenStageOversize: 0,
		})
		t.Logf("stage=record: status=%d (handler's own response, uncached); %s{stage=record}=%v",
			rec.Code, failOpenMetricName,
			testutil.ToFloat64(metrics.failOpen.WithLabelValues(failOpenStageRecord)))
	})

	// stage=relookup: the post-handler re-Lookup faulted. The handler returned a
	// non-2xx, so nothing is recorded and the re-Lookup is the only chance to discover
	// a concurrent winner's stored response; failing it means this client may be given
	// a second, divergent answer for one logical operation.
	t.Run("a re-Lookup error increments stage=relookup", func(t *testing.T) {
		metrics := newIsolatedIdempotencyMetrics(t)
		store := &fakeIdempotencyStore{lookups: []lookupResult{
			{}, // upfront lookup: miss, so the handler runs
			{err: fmt.Errorf("lookup idempotency key: %w: connection reset", apperr.ErrInternal)},
		}}
		h := &countingHandler{status: http.StatusConflict, body: []byte(`{"error":"version conflict"}`)}
		chain := newFakeChainWithMetrics(store, uuid.New(), metrics, h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"widget"}`))

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (this request's own response); body %s", rec.Code, rec.Body)
		}
		if store.recordCalls != 0 {
			t.Errorf("Record calls = %d, want 0 (a non-2xx is never recorded)", store.recordCalls)
		}
		// stage=record must be zero: Record was never called, so attributing this to it
		// would misreport a failing read path as a failing write path.
		assertFailOpen(t, metrics, map[string]float64{
			failOpenStageRecord:   0,
			failOpenStageRelookup: 1,
			failOpenStageOversize: 0,
		})
	})

	// stage=oversize: the response exceeded 1 MiB and was delivered but not cached. A
	// designed outcome, not a fault — but the same silent loss of protection, and the
	// count is the evidence maxIdempotencyBodyBytes would be retuned against.
	t.Run("an oversized response increments stage=oversize", func(t *testing.T) {
		metrics := newIsolatedIdempotencyMetrics(t)
		store := &fakeIdempotencyStore{recordInserted: true}
		big := bytes.Repeat([]byte("b"), maxIdempotencyBodyBytes+100) // > 1 MiB
		h := &countingHandler{status: http.StatusOK, body: big}
		chain := newFakeChainWithMetrics(store, uuid.New(), metrics, h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"big"}`))

		if rec.Code != http.StatusOK || rec.Body.Len() != len(big) {
			t.Fatalf("response = {status:%d len:%d}, want {200, %d} (the full body must still reach the client)",
				rec.Code, rec.Body.Len(), len(big))
		}
		if store.recordCalls != 0 {
			t.Errorf("Record calls = %d, want 0 (an oversized response is never cached)", store.recordCalls)
		}
		assertFailOpen(t, metrics, map[string]float64{
			failOpenStageRecord:   0,
			failOpenStageRelookup: 0,
			failOpenStageOversize: 1,
		})
	})

	// The happy path must leave every series at zero — and the series must EXIST, so
	// an alert on rate(...) reads a real zero rather than "no data" (which is
	// indistinguishable from the metric never having been wired).
	t.Run("a fully successful request counts nothing, but the series exist at zero", func(t *testing.T) {
		metrics := newIsolatedIdempotencyMetrics(t)
		store := &fakeIdempotencyStore{recordInserted: true}
		h := &countingHandler{status: http.StatusCreated, body: []byte(`{"id":"abc"}`)}
		chain := newFakeChainWithMetrics(store, uuid.New(), metrics, h)

		rec := doIdempotentRequest(t, chain, http.MethodPost, uuid.NewString(), []byte(`{"name":"widget"}`))

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
		}
		if got := testutil.CollectAndCount(metrics.failOpen); got != len(failOpenStages) {
			t.Errorf("collected %s series = %d, want %d (all stages pre-created at zero)",
				failOpenMetricName, got, len(failOpenStages))
		}
		assertFailOpen(t, metrics, map[string]float64{
			failOpenStageRecord:   0,
			failOpenStageRelookup: 0,
			failOpenStageOversize: 0,
		})
	})
}

// sha256DigestLen is the byte length of a SHA-256 digest, asserted on the recorded
// request hash.
const sha256DigestLen = 32

// TestIdempotencyRequestHashFoldsInTheActor pins the property B4 rests on, at the
// hash function itself: the SAME body under two different actors produces two
// DIFFERENT fingerprints. Everything else in the cross-actor defense (the 409, the
// refusal to replay) follows from this inequality.
func TestIdempotencyRequestHashFoldsInTheActor(t *testing.T) {
	body := []byte(`{"name":"widget"}`)
	actorA, actorB := uuid.New(), uuid.New()

	hashA := idempotencyRequestHash(actorA, body)
	hashB := idempotencyRequestHash(actorB, body)

	if bytes.Equal(hashA, hashB) {
		t.Error("the same body under two different actors hashed identically; the actor is not folded in")
	}
	if !bytes.Equal(hashA, idempotencyRequestHash(actorA, body)) {
		t.Error("the hash is not deterministic for one (actor, body) pair")
	}
	if len(hashA) != sha256DigestLen {
		t.Errorf("hash length = %d, want %d", len(hashA), sha256DigestLen)
	}
	// A different body under the SAME actor must also differ — the original AC-2
	// property, still intact after the actor was folded in.
	if bytes.Equal(hashA, idempotencyRequestHash(actorA, []byte(`{"name":"other"}`))) {
		t.Error("two different bodies under one actor hashed identically")
	}
}
