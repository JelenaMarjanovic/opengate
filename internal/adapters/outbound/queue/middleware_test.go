package queue

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/JelenaMarjanovic/opengate/internal/observability"
)

// TestTraceMiddlewareChildSpan is the AC-3 verification: the worker span the
// middleware opens is a CHILD of the span that was active when the job was
// enqueued. It is a pure unit test — no database, no River pool — that proves the
// trace-continuation mechanism end to end:
//
//	parent span -> inject (global propagator) -> {"trace": carrier} metadata
//	  -> traceMiddleware.Work -> extract -> child worker span
//
// The metadata is built EXACTLY as the Step 2 enqueuer builds it (inject the
// parent ctx through the global propagator into a MapCarrier, marshal it under
// metadataTraceKey), so this also pins the enqueue/work contract.
func TestTraceMiddlewareChildSpan(t *testing.T) {
	// In-memory SDK so spans are actually recorded (production wires only the API,
	// which is a no-op producer). Install it as the global provider for the duration
	// of the test and restore the prior one after, so no global state leaks.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	// The global W3C propagator must be installed or Inject/Extract are no-ops and
	// the carrier would be empty — exactly the production wiring (main.go).
	observability.SetGlobalTracePropagator()

	// Start the parent span — the "originating" span the enqueue side would have had
	// active. Capture its SpanContext for the child-linkage assertions.
	ctx, parent := tp.Tracer("test.enqueue").Start(context.Background(), "enqueue test.noop")
	parentSC := parent.SpanContext()

	// Build the job metadata identically to the enqueuer (Step 2): inject the parent
	// ctx into a carrier, wrap it under metadataTraceKey, marshal.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	meta, err := json.Marshal(map[string]propagation.MapCarrier{metadataTraceKey: carrier})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	job := &rivertype.JobRow{Kind: testJobKind, Metadata: meta}

	// Run the middleware with a FRESH root context (context.Background) — the worker
	// has no in-process link to the enqueuer, so the only path from parent to child
	// is the persisted carrier. doInner captures the ctx the middleware hands the
	// inner work, so we can confirm a span is active on it.
	var innerSC trace.SpanContext
	doInner := func(innerCtx context.Context) error {
		innerSC = trace.SpanContextFromContext(innerCtx)
		return nil
	}
	if err := (&traceMiddleware{}).Work(context.Background(), job, doInner); err != nil {
		t.Fatalf("middleware Work: %v", err)
	}

	// End the parent so both spans are flushed to the recorder.
	parent.End()

	// Locate the worker span by name.
	var worker sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == "river.work "+testJobKind {
			worker = s
			break
		}
	}
	if worker == nil {
		t.Fatalf("worker span %q not recorded; got %d spans", "river.work "+testJobKind, len(recorder.Ended()))
	}

	// AC-3 core assertions: same trace, and the worker's parent IS the originating
	// span. This is the mechanism-level proof of trace continuation.
	if got, want := worker.SpanContext().TraceID(), parentSC.TraceID(); got != want {
		t.Errorf("worker span TraceID = %s, want %s (not in the originating trace)", got, want)
	}
	if got, want := worker.Parent().SpanID(), parentSC.SpanID(); got != want {
		t.Errorf("worker span parent SpanID = %s, want %s (not a child of the originating span)", got, want)
	}

	// The span the inner work saw must be the worker span — i.e. the middleware
	// actually put its span on the context it passes down.
	if got := innerSC.SpanID(); got != worker.SpanContext().SpanID() {
		t.Errorf("inner ctx span SpanID = %s, want worker span %s", got, worker.SpanContext().SpanID())
	}
}

// TestTraceMiddlewareNoCarrierRunsWork covers the production-today path: a job
// enqueued with no active span carries an empty carrier, so there is no parent to
// extract. The middleware must still run the inner work (and not fail the job) —
// trace continuation is best-effort, never a precondition for working a job.
func TestTraceMiddlewareNoCarrierRunsWork(t *testing.T) {
	// Empty trace envelope, exactly what the enqueuer writes with no active span.
	meta, err := json.Marshal(map[string]propagation.MapCarrier{metadataTraceKey: {}})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	job := &rivertype.JobRow{Kind: testJobKind, Metadata: meta}

	ran := false
	doInner := func(context.Context) error {
		ran = true
		return nil
	}
	if err := (&traceMiddleware{}).Work(context.Background(), job, doInner); err != nil {
		t.Fatalf("middleware Work: %v", err)
	}
	if !ran {
		t.Error("middleware did not call doInner; the job's work was dropped")
	}
}
