package queue

import (
	"context"
	"encoding/json"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
)

// workerTracerName is the OpenTelemetry instrumentation-scope name for spans the
// worker opens around each job. It identifies the producer of the span in any
// exporter's output and is kept distinct from any HTTP-side tracer.
const workerTracerName = "river.worker"

// traceMiddleware is the work-side half of the trace-context propagation the
// enqueuer (Step 2) started. It is a global River WorkerMiddleware (decision B3):
// registered once on the worker client's Config.Middleware, it wraps EVERY worked
// job rather than being attached per worker, mirroring the single insert-side
// injection point.
//
// For each job it:
//
//  1. parses the W3C carrier the enqueuer persisted under metadataTraceKey
//     ("trace") in the job metadata;
//  2. Extracts that carrier through the GLOBAL propagator, rebuilding the parent
//     SpanContext on the context so the worker span continues the originating
//     trace instead of starting a fresh, disconnected one;
//  3. opens a child span "river.work <kind>" around the inner work and records
//     any error on it.
//
// Embedding river.MiddlewareDefaults supplies the IsMiddleware sentinel (and the
// no-op InsertMany), so this type only has to implement Work to satisfy
// rivertype.WorkerMiddleware.
//
// Production today wires only the OTel API, not the SDK, so otel.Tracer returns a
// no-op tracer and no span is actually produced — but the extract/wrap is in
// place, so the moment the SDK is installed (deferred backlog item) worker spans
// become real children of the originating span with zero further changes. The
// AC-3 test installs an in-memory SDK to prove the parent/child linkage.
type traceMiddleware struct {
	river.MiddlewareDefaults
}

// Compile-time assertion that traceMiddleware satisfies River's work-side
// middleware interface, so a signature drift in rivertype is caught at build
// time rather than when the worker pool is assembled.
var _ rivertype.WorkerMiddleware = (*traceMiddleware)(nil)

// Work is the WorkerMiddleware hook. It MUST call doInner(ctx) — returning before
// it would silently drop the job's actual work — and returns doInner's error so
// River's retry/discard machinery sees the real outcome. The only behavior added
// here is trace continuation and error recording on the span.
func (m *traceMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	// 1. Rebuild the parent context from the persisted carrier, when present.
	//    A job enqueued without an active span (production today) carries an empty
	//    carrier; Extract is then a no-op and the worker span simply has no parent.
	if carrier := traceCarrierFromMetadata(job.Metadata); len(carrier) > 0 {
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}

	// 2. Open the child span around the inner work. With no SDK wired this tracer
	//    is a no-op and Start returns the ctx unchanged plus a non-recording span;
	//    span.End / RecordError are then cheap no-ops. The kind is included in the
	//    span name so traces are legible per job type.
	ctx, span := otel.Tracer(workerTracerName).Start(ctx, "river.work "+job.Kind)
	defer span.End()

	// 3. Run the real work and record any failure on the span before returning it
	//    verbatim — River, not this middleware, decides retry vs discard.
	err := doInner(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// traceCarrierFromMetadata extracts the W3C carrier the enqueuer stored under
// metadataTraceKey from a job's raw metadata jsonb. It is deliberately lenient:
// metadata that is empty, not an object, or missing the trace key yields an empty
// carrier (and thus no parent span) rather than failing the job — a malformed or
// absent carrier must never stop work from running. The shape it reads,
// {"trace": {<carrier>}}, is the exact envelope Enqueue writes (Step 2).
func traceCarrierFromMetadata(metadata []byte) propagation.MapCarrier {
	if len(metadata) == 0 {
		return nil
	}
	var envelope map[string]propagation.MapCarrier
	if err := json.Unmarshal(metadata, &envelope); err != nil {
		return nil
	}
	return envelope[metadataTraceKey]
}
