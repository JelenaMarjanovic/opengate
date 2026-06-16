package http

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// failOpenMetricName counts the requests the command idempotency middleware served
// WITHOUT idempotency protection instead of failing them.
//
// The principle it encodes: THE FAIL-OPEN PATHS NEED COUNTING; THE FAIL-CLOSED ONES
// DO NOT. A failed upfront Lookup fails closed — the handler never runs, the client
// gets a 500, and that 500 is already visible in the HTTP metrics and in the Problem
// Details log line. It needs no counter of its own. The paths below are the opposite:
// the request SUCCEEDS while a guarantee is silently dropped, so without a counter
// there is nothing to see at all.
//
// This is not hypothetical. A nil response body encoded as SQL NULL, which
// response_body (bytea NOT NULL) rejected, so every body-less success — a 204 above
// all — failed to record and lost its protection with no signal whatsoever. That
// defect was found by reading the code; this counter is how the next one is found by
// looking at a dashboard.
const failOpenMetricName = "opengate_idempotency_fail_open_total"

// The `stage` label values: WHICH guarantee was dropped and where.
//
// Each names a distinct operational question, which is why they are one label on one
// counter rather than three metrics: a sustained `record` rate is a failing write
// path, a sustained `relookup` rate is a failing read path, and a sustained
// `oversize` rate is a 1 MiB ceiling set too low for what this endpoint returns.
const (
	// failOpenStageRecord: Record returned an error. The handler already ran, so its
	// response is returned to the client, simply uncached.
	failOpenStageRecord = "record"
	// failOpenStageRelookup: the post-handler re-Lookup returned an error, so this
	// request's own response is returned instead of a possible concurrent winner's.
	failOpenStageRelookup = "relookup"
	// failOpenStageOversize: the response exceeded 1 MiB and was delivered but not
	// cached. A designed outcome rather than a fault — but the same silent loss of
	// protection, and counting it is what makes the ceiling tunable against evidence.
	failOpenStageOversize = "oversize"
)

// failOpenStages is every stage label, used to materialize all three time series at
// construction (see newIdempotencyMetrics).
var failOpenStages = []string{failOpenStageRecord, failOpenStageRelookup, failOpenStageOversize}

// idempotencyMetrics holds the command idempotency middleware's Prometheus
// instruments, constructed once and shared by every request. A nil *idempotencyMetrics
// is tolerated (recordFailOpen becomes a no-op), so a caller with no registry wired
// yet can pass nil.
type idempotencyMetrics struct {
	failOpen *prometheus.CounterVec
}

// newIdempotencyMetrics builds the middleware's metrics and registers them with reg.
//
// reg is injected rather than defaulting to prometheus.DefaultRegisterer so tests
// register into an isolated registry (prometheus.NewRegistry) while the composition
// root registers into the default one -- the same shape as projection.NewMetrics and
// maintenance.NewMetrics (US-03.05). Re-registering the identical collector into the
// same registry is tolerated: prometheus reports AlreadyRegisteredError and we reuse
// the existing counter, so constructing this more than once per registry is safe.
//
// The middleware is mounted on no route today (no command endpoint exists until E4),
// so nothing calls this from the composition root yet; the route that mounts the
// middleware wires newIdempotencyMetrics(prometheus.DefaultRegisterer) alongside it.
func newIdempotencyMetrics(reg prometheus.Registerer) (*idempotencyMetrics, error) {
	failOpen := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: failOpenMetricName,
		Help: "Requests served without command idempotency protection rather than failed, by stage.",
	}, []string{"stage"})

	if err := reg.Register(failOpen); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, fmt.Errorf("idempotency metrics: register %s: %w", failOpenMetricName, err)
		}
		// The metric is already registered (an earlier call on this registry). Reuse
		// that collector so both handles drive the same time series.
		existing, ok := already.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, fmt.Errorf("idempotency metrics: %s already registered as a different collector type", failOpenMetricName)
		}
		failOpen = existing
	}

	// Materialize all three series at zero. A CounterVec creates a child only on first
	// use, so an alert on rate(...) would otherwise read "no data" until the first
	// fail-open ever happened -- indistinguishable from "the metric was never wired".
	// Pre-creating them makes a healthy deployment scrape a real zero from boot.
	for _, stage := range failOpenStages {
		failOpen.WithLabelValues(stage).Add(0)
	}

	return &idempotencyMetrics{failOpen: failOpen}, nil
}

// recordFailOpen increments the fail-open counter for stage.
//
// The nil receiver is deliberate and safe: it lets the three call sites read as one
// unconditional line each instead of repeating a nil guard, which matters because a
// forgotten guard on this path would panic inside a request that has already run its
// handler.
func (m *idempotencyMetrics) recordFailOpen(stage string) {
	if m == nil {
		return
	}
	m.failOpen.WithLabelValues(stage).Inc()
}
