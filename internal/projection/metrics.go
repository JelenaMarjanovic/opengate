package projection

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// lagMetricName is the projector lag gauge. Its value for a given projector is
// now minus the occurred_at of the latest event that projector has consumed.
const lagMetricName = "opengate_projection_lag_seconds"

// Metrics holds the projector framework's Prometheus instruments, constructed once
// over the project's registry and shared by every projector iteration. A nil
// *Metrics is tolerated by the runner (it simply records nothing), so a caller that
// has no registry wired yet can pass nil.
type Metrics struct {
	lag *prometheus.GaugeVec
}

// NewMetrics builds the projector metrics and registers them with reg, returning a
// handle the runner records into.
//
// reg is injected rather than defaulting to prometheus.DefaultRegisterer so tests
// register into an isolated registry (prometheus.NewRegistry) while the composition
// root registers into the default one. Re-registering the identical collector into
// the same registry is tolerated -- prometheus reports AlreadyRegisteredError and
// we reuse the existing gauge -- so constructing Metrics more than once per registry
// is safe (the production worker may be built repeatedly in tests).
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	lag := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: lagMetricName,
		Help: "Projection lag in seconds: now minus the occurred_at of the latest event the projector has consumed.",
	}, []string{"projector"})

	if err := reg.Register(lag); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, fmt.Errorf("projection metrics: register %s: %w", lagMetricName, err)
		}
		// The metric is already registered (an earlier NewMetrics on this registry).
		// Reuse that collector so both handles drive the same time series.
		existing, ok := already.ExistingCollector.(*prometheus.GaugeVec)
		if !ok {
			return nil, fmt.Errorf("projection metrics: %s already registered as a different collector type", lagMetricName)
		}
		lag = existing
	}

	return &Metrics{lag: lag}, nil
}

// RecordLag sets the lag gauge for projector to seconds. It is called by the runner
// only after a successful commit, and only by the instance that held the lock.
func (m *Metrics) RecordLag(projector string, seconds float64) {
	m.lag.WithLabelValues(projector).Set(seconds)
}
