package observability

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// SetGlobalTracePropagator installs the process-wide OTel text-map propagator as
// the W3C standard pair: TraceContext (the traceparent / tracestate headers) and
// Baggage. It is called once at app init (cmd/opengate main) and defines how
// trace context serializes wherever the application injects or extracts it —
// today, into River job metadata on enqueue (US-03.04) and back out on the work
// side.
//
// This is the OTel API, NOT the OTel SDK: it only fixes the wire format for
// context propagation. It neither produces nor exports spans — wiring the SDK and
// an exporter (so spans are actually emitted) is a separate, deferred task and is
// unaffected by this call. Without it the global propagator defaults to a no-op
// that drops all context, so setting it is the prerequisite for trace continuity
// across the enqueue/work boundary even before the SDK exists.
func SetGlobalTracePropagator() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}
