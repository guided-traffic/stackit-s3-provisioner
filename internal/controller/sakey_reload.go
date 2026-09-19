package controller

import (
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// The service-account key reload surface (ADR 0016 D8). Four series, and the
// reason all four exist is that a rejected reload is otherwise invisible: the
// operator carries on with the key it already holds, so a rotation that never
// landed looks exactly like a healthy operator right up to the moment the old
// key is deleted.
//
// These are process metrics, like the measurement counters above them and
// unlike every gauge derived from the Bucket cache — the state they describe
// lives in one pod's memory and cannot be recomputed from any resource.
var (
	saKeyReloadsDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_sa_key_reload_total",
		"Total number of service-account key polls, by what the poll did.",
		[]string{"result"}, nil,
	)
	saKeyLoadedDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_sa_key_loaded_timestamp_seconds",
		"Unix time at which the service-account key currently in use was loaded.",
		nil, nil,
	)
	saKeyReloadFailingDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_sa_key_reload_failing",
		"1 while the operator keeps rejecting the service-account key on disk and carries on with the key it already holds.",
		nil, nil,
	)
	saKeyValidUntilDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds",
		"Unix time at which the service-account key currently in use expires; absent when the key file carries no validUntil.",
		nil, nil,
	)
)

// saKeyResults enumerates every value of the result label, so all three series
// exist from the first scrape and no alert expression races an absent one.
var saKeyResults = []stackit.ReloadOutcome{
	stackit.ReloadApplied,
	stackit.ReloadRejected,
	stackit.ReloadUnchanged,
}

// SAKeyReloadObserver turns the result of each key poll into logs, metrics and
// — on a successful swap — a circuit-breaker reset. It is the whole reason the
// reload machinery in the provider client needs neither a logger nor
// Prometheus.
//
// It is also a prometheus.Collector over its own state, which is what makes
// the expiry gauge absent rather than zero for a key file that carries no
// validUntil: there is nothing to emit, so nothing is emitted.
type SAKeyReloadObserver struct {
	log logr.Logger
	// breaker is reset by a successful reload, because the validation has just
	// made a successful authenticated call — the exact evidence the breaker
	// waits for (ADR 0016 D6). Nil-safe.
	breaker *ProviderBreaker

	applied   atomic.Int64
	rejected  atomic.Int64
	unchanged atomic.Int64
	// loadedAt and validUntil are Unix seconds; validUntil is 0 when the key
	// carries no expiry.
	loadedAt   atomic.Int64
	validUntil atomic.Int64
	failing    atomic.Bool
}

// NewSAKeyReloadObserver seeds the observer with the key the process started
// on, so the loaded-at and expiry gauges are right from the first scrape rather
// than only after the first rotation.
func NewSAKeyReloadObserver(log logr.Logger, breaker *ProviderBreaker, initial stackit.Account) *SAKeyReloadObserver {
	o := &SAKeyReloadObserver{log: log, breaker: breaker}
	o.adopt(initial, time.Now())
	return o
}

// adopt records the key now in use.
func (o *SAKeyReloadObserver) adopt(acc stackit.Account, at time.Time) {
	o.loadedAt.Store(at.Unix())
	if acc.ValidUntil != nil {
		o.validUntil.Store(acc.ValidUntil.Unix())
	} else {
		o.validUntil.Store(0)
	}
}

// Observe records one poll. It is called from the reload goroutine only.
func (o *SAKeyReloadObserver) Observe(res stackit.ReloadResult) {
	switch res.Outcome {
	case stackit.ReloadApplied:
		o.applied.Add(1)
		o.failing.Store(false)
		o.adopt(res.Account, time.Now())
		o.breaker.Success()
		// The project and the issuer identify the key; its content never
		// appears here, in an event, in status or in a metric label.
		o.log.Info("loaded a new StackIT service-account key",
			"project", res.Account.ProjectID,
			"issuer", res.Account.Issuer,
			"validUntil", validUntilForLog(res.Account),
		)
	case stackit.ReloadRejected:
		o.rejected.Add(1)
		o.failing.Store(true)
		if !res.Repeated {
			// Once per distinct file content, not once per poll: the alert is
			// what keeps this visible, the log is what explains it.
			o.log.Error(res.Err, "rejected a candidate StackIT service-account key; carrying on with the key in use",
				"definitive", res.Definitive,
				"candidateHash", res.Hash,
			)
		}
	case stackit.ReloadUnchanged:
		o.unchanged.Add(1)
		o.failing.Store(false)
	}
}

// validUntilForLog renders the key's expiry for a log line, or a placeholder
// when the provider stamped none into the file.
func validUntilForLog(acc stackit.Account) string {
	if acc.ValidUntil == nil {
		return "none"
	}
	return acc.ValidUntil.UTC().Format(time.RFC3339)
}

// Describe implements prometheus.Collector.
func (o *SAKeyReloadObserver) Describe(ch chan<- *prometheus.Desc) {
	ch <- saKeyReloadsDesc
	ch <- saKeyLoadedDesc
	ch <- saKeyReloadFailingDesc
	ch <- saKeyValidUntilDesc
}

// Collect implements prometheus.Collector.
func (o *SAKeyReloadObserver) Collect(ch chan<- prometheus.Metric) {
	for _, result := range saKeyResults {
		ch <- prometheus.MustNewConstMetric(saKeyReloadsDesc, prometheus.CounterValue,
			float64(o.count(result)), string(result))
	}
	ch <- prometheus.MustNewConstMetric(saKeyLoadedDesc, prometheus.GaugeValue, float64(o.loadedAt.Load()))
	ch <- prometheus.MustNewConstMetric(saKeyReloadFailingDesc, prometheus.GaugeValue, boolGauge(o.failing.Load()))
	// Absent, not zero: a key file without validUntil says nothing about when
	// the key expires, and an expiry alert must not fire on that silence.
	if v := o.validUntil.Load(); v != 0 {
		ch <- prometheus.MustNewConstMetric(saKeyValidUntilDesc, prometheus.GaugeValue, float64(v))
	}
}

func (o *SAKeyReloadObserver) count(result stackit.ReloadOutcome) int64 {
	switch result {
	case stackit.ReloadApplied:
		return o.applied.Load()
	case stackit.ReloadRejected:
		return o.rejected.Load()
	case stackit.ReloadUnchanged:
		return o.unchanged.Load()
	default:
		return 0
	}
}

// RegisterSAKeyReloadMetrics registers the observer on the metrics endpoint.
//
// It is separate from RegisterBucketMetrics and is called only when the reload
// is actually running, so an operator in skeleton mode — or one that has turned
// the reload off — exports none of these series rather than exporting a
// reassuring zero for a mechanism that is not there.
func RegisterSAKeyReloadMetrics(o *SAKeyReloadObserver) {
	ctrlmetrics.Registry.MustRegister(o)
}
