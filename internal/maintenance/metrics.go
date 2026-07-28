package maintenance

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// deletedMetricName counts idempotency-key rows the cleanup job has deleted, split
// by table. It is the signal that the retention job is alive: a cleanup that
// silently stops deleting (a lost grant, a wedged lock holder, a worker that never
// schedules the tick) is otherwise invisible until the table itself grows.
const deletedMetricName = "opengate_idempotency_keys_deleted_total"

// Metrics holds the maintenance jobs' Prometheus instruments, constructed once over
// the project's registry and shared by every iteration. A nil *Metrics is tolerated
// by the jobs (they simply record nothing), so a caller with no registry wired yet
// can pass nil.
type Metrics struct {
	deleted *prometheus.CounterVec
}

// NewMetrics builds the maintenance metrics and registers them with reg, returning a
// handle the jobs record into.
//
// reg is injected rather than defaulting to prometheus.DefaultRegisterer so tests
// register into an isolated registry (prometheus.NewRegistry) while the composition
// root registers into the default one. Re-registering the identical collector into
// the same registry is tolerated -- prometheus reports AlreadyRegisteredError and we
// reuse the existing counter -- so constructing Metrics more than once per registry
// is safe (the production worker may be built repeatedly in tests).
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	deleted := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: deletedMetricName,
		Help: "Idempotency-key rows deleted by the cleanup.idempotency_keys job, by table.",
	}, []string{"table"})

	if err := reg.Register(deleted); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, fmt.Errorf("maintenance metrics: register %s: %w", deletedMetricName, err)
		}
		// The metric is already registered (an earlier NewMetrics on this registry).
		// Reuse that collector so both handles drive the same time series.
		existing, ok := already.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, fmt.Errorf("maintenance metrics: %s already registered as a different collector type", deletedMetricName)
		}
		deleted = existing
	}

	return &Metrics{deleted: deleted}, nil
}

// RecordDeleted adds rows to the delete counter for table. It is called only after a
// successful commit, and only by the instance that held the advisory lock.
//
// A tick that deleted nothing still calls this with rows = 0. Adding zero is not a
// no-op for Prometheus: it CREATES the time series if it does not exist yet, so the
// counter is scrapeable from the first tick onward instead of appearing only once
// something expires. An alert on rate(...) therefore reads a real zero rather than
// "no data", which is the difference between "nothing expired" and "the job is gone".
func (m *Metrics) RecordDeleted(table string, rows int64) {
	m.deleted.WithLabelValues(table).Add(float64(rows))
}
