package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// bucketPhaseUnknown is the phase label for Buckets whose status.phase is not
// (yet) set — freshly created CRs before the first status write.
const bucketPhaseUnknown = "Unknown"

// bucketPhases enumerates every phase label the buckets gauge emits. All of
// them are always exported (with 0 when empty) so alert expressions never race
// an absent series.
var bucketPhases = []s3v1.BucketPhase{
	s3v1.PhasePending,
	s3v1.PhaseProvisioning,
	s3v1.PhaseReady,
	s3v1.PhaseFailed,
	s3v1.PhaseDeleting,
	bucketPhaseUnknown,
}

// clonePhases enumerates every phase label the clone gauge emits; Buckets
// without spec.cloneFrom / status.clone are not counted at all.
var clonePhases = []s3v1.ClonePhase{
	s3v1.ClonePhaseRunning,
	s3v1.ClonePhaseCompleted,
	s3v1.ClonePhaseFailed,
}

// bucketLabels identifies a per-Bucket series. Shared by every per-Bucket
// descriptor so the label set cannot drift apart between them.
var bucketLabels = []string{"namespace", "name"}

var (
	bucketsDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_buckets",
		"Number of Bucket resources per status phase.",
		[]string{"phase"}, nil,
	)
	bucketsCloneDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_buckets_clone",
		"Number of Bucket resources per clone phase (only Buckets with a clone).",
		[]string{"phase"}, nil,
	)
	bucketsDegradedDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_buckets_provider_degraded",
		"Number of Bucket resources whose Ready state is being held while reconciles keep failing for a non-definitive reason.",
		nil, nil,
	)
	bucketDegradedSinceDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds",
		"Unix time at which this Bucket started degrading; absent for Buckets that are not degraded.",
		bucketLabels, nil,
	)
	bucketsWipeOnDeleteDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_buckets_wipe_on_delete",
		"Number of Bucket resources with spec.wipeOnDelete set to true.",
		nil, nil,
	)
	skeletonModeDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_skeleton_mode",
		"1 when the operator runs without a StackIT service-account key and therefore provisions nothing.",
		nil, nil,
	)
	wipeGateDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_wipe_on_delete_gate_enabled",
		"1 when the operator-wide --enable-wipe-on-delete feature gate is on.",
		nil, nil,
	)
	lastRotationDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds",
		"Unix time of the Bucket's last credentials rotation; absent for Buckets that were never rotated.",
		bucketLabels, nil,
	)

	// Bucket size and cost. Every one of these is absent for a Bucket that has
	// not been measured, so an alert on a size threshold never fires on a
	// missing measurement, and `absent()` distinguishes the two cases.
	bucketSizeDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_size_bytes",
		"Size in bytes of the Bucket's current objects at the last measurement.",
		bucketLabels, nil,
	)
	bucketObjectsDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_objects",
		"Number of current objects in the Bucket at the last measurement.",
		bucketLabels, nil,
	)
	bucketVersionSizeDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_version_size_bytes",
		"Size in bytes of the Bucket's non-current object versions at the last measurement; 0 unless version counting is enabled.",
		bucketLabels, nil,
	)
	bucketVersionObjectsDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_version_objects",
		"Number of non-current object versions and delete markers at the last measurement; 0 unless version counting is enabled.",
		bucketLabels, nil,
	)
	bucketBillableSizeDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_billable_size_bytes",
		"Size in bytes the cost estimate is computed from (current objects plus counted versions).",
		bucketLabels, nil,
	)
	bucketCostDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_estimated_monthly_cost",
		"Estimated monthly storage cost of the Bucket at the operator's configured price, in whole currency units; absent when no price is configured.",
		append(append([]string{}, bucketLabels...), "currency"), nil,
	)
	bucketUsageMeasuredDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds",
		"Unix time of the Bucket's last successful size measurement; absent for Buckets that were never measured.",
		bucketLabels, nil,
	)
	bucketUsageTruncatedDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_bucket_usage_truncated",
		"1 when the Bucket's last measurement stopped at the operator's object cap, so its size and cost are lower bounds.",
		bucketLabels, nil,
	)
	bucketsUsageEnabledDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_buckets_usage_measured",
		"Number of Bucket resources that carry a size measurement.",
		nil, nil,
	)
	usageGateDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_usage_measurement_gate_enabled",
		"1 when the operator-wide bucket size measurement gate is on.",
		nil, nil,
	)

	// Provider circuit breaker. These two are the honest replacement for
	// counting a provider outage once per retry: the operator stops reconciling
	// while the breaker is open, so the outage shows up as a state with a
	// duration rather than as a rising error counter.
	providerCircuitOpenDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_provider_circuit_open",
		"1 while the fleet-wide provider circuit breaker is open and Bucket reconciles are being held.",
		nil, nil,
	)
	providerCircuitOpenedSinceDesc = prometheus.NewDesc(
		"stackit_s3_provisioner_provider_circuit_opened_timestamp_seconds",
		"Unix time at which the provider circuit breaker last opened; absent while it is closed.",
		nil, nil,
	)
)

// Process-level measurement metrics. Unlike the gauges above these cannot be
// derived from the Bucket cache: a failed measurement leaves no trace on the CR
// beyond a message, and the duration of a pass is gone once it finished.
var (
	// usageMeasurementFailures counts measurements that could not complete.
	// Measurement failures deliberately do NOT return a reconcile error (they
	// must not be confused with provisioning failures), so this counter is the
	// only place they are aggregated.
	usageMeasurementFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "stackit_s3_provisioner_usage_measurement_failures_total",
		Help: "Total number of bucket size measurements that failed.",
	})
	// usageMeasurementDuration records how long a listing pass took. It is the
	// honest price of the configured interval: the number to look at before
	// lowering it, and the one that shows a bucket outgrowing its cap.
	usageMeasurementDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "stackit_s3_provisioner_usage_measurement_duration_seconds",
		Help:    "Duration of a successful bucket size measurement (one full listing pass).",
		Buckets: []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 1800},
	})
)

// bucketMetricsCollector computes Bucket gauges from the manager's cache on
// every scrape, so the values self-heal and need no per-reconcile bookkeeping.
type bucketMetricsCollector struct {
	reader client.Reader
	// breaker is the fleet-wide provider circuit breaker; nil when disabled.
	breaker *ProviderBreaker
	// skeletonMode is true when the operator has no StackIT client.
	skeletonMode bool
	// wipeGateEnabled mirrors the --enable-wipe-on-delete feature gate.
	wipeGateEnabled bool
	// usageGateEnabled mirrors the operator-wide bucket size measurement gate.
	usageGateEnabled bool
}

// RegisterBucketMetrics registers the Bucket collector with the
// controller-runtime metrics registry served on the metrics endpoint. Call it
// once per process.
func RegisterBucketMetrics(reader client.Reader, breaker *ProviderBreaker, skeletonMode, wipeGateEnabled, usageGateEnabled bool) {
	ctrlmetrics.Registry.MustRegister(&bucketMetricsCollector{
		reader:           reader,
		breaker:          breaker,
		skeletonMode:     skeletonMode,
		wipeGateEnabled:  wipeGateEnabled,
		usageGateEnabled: usageGateEnabled,
	})
	ctrlmetrics.Registry.MustRegister(usageMeasurementFailures, usageMeasurementDuration)
}

func (c *bucketMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- bucketsDesc
	ch <- bucketsCloneDesc
	ch <- bucketsDegradedDesc
	ch <- bucketDegradedSinceDesc
	ch <- bucketsWipeOnDeleteDesc
	ch <- skeletonModeDesc
	ch <- wipeGateDesc
	ch <- lastRotationDesc
	ch <- bucketSizeDesc
	ch <- bucketObjectsDesc
	ch <- bucketVersionSizeDesc
	ch <- bucketVersionObjectsDesc
	ch <- bucketBillableSizeDesc
	ch <- bucketCostDesc
	ch <- bucketUsageMeasuredDesc
	ch <- bucketUsageTruncatedDesc
	ch <- bucketsUsageEnabledDesc
	ch <- usageGateDesc
	ch <- providerCircuitOpenDesc
	ch <- providerCircuitOpenedSinceDesc
}

func (c *bucketMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(skeletonModeDesc, prometheus.GaugeValue, boolGauge(c.skeletonMode))
	ch <- prometheus.MustNewConstMetric(wipeGateDesc, prometheus.GaugeValue, boolGauge(c.wipeGateEnabled))
	ch <- prometheus.MustNewConstMetric(usageGateDesc, prometheus.GaugeValue, boolGauge(c.usageGateEnabled))

	// Emitted unconditionally (0 while closed) so an alert never races an absent
	// series; the timestamp is absent while closed so `time() - <series>` is the
	// age of the outage wherever it exists.
	openedAt, open := c.breaker.OpenedAt()
	ch <- prometheus.MustNewConstMetric(providerCircuitOpenDesc, prometheus.GaugeValue, boolGauge(open))
	if open {
		ch <- prometheus.MustNewConstMetric(providerCircuitOpenedSinceDesc, prometheus.GaugeValue, float64(openedAt.Unix()))
	}

	// The cached client blocks until the informer cache syncs; bound the wait
	// so a scrape during startup cannot hang the metrics handler.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var buckets s3v1.BucketList
	if err := c.reader.List(ctx, &buckets); err != nil {
		// Omit the Bucket-derived metrics on error; an absent series is easier
		// to alert on than a silently stale value.
		return
	}

	byPhase := map[s3v1.BucketPhase]int{}
	byClonePhase := map[s3v1.ClonePhase]int{}
	wipeOnDelete := 0
	degraded := 0
	measured := 0
	for i := range buckets.Items {
		b := &buckets.Items[i]
		phase := b.Status.Phase
		if phase == "" {
			phase = bucketPhaseUnknown
		}
		byPhase[phase]++
		if b.Status.Clone != nil && b.Status.Clone.Phase != "" {
			byClonePhase[b.Status.Clone.Phase]++
		}
		if b.Spec.WipeOnDelete {
			wipeOnDelete++
		}
		// Both degraded metrics describe a hold that is actually in effect, so
		// they require phase Ready as well. status.degradedSince deliberately
		// survives into phase Failed once the grace elapses — it records when the
		// trouble began — but at that point the Bucket is no longer being held
		// and is covered by the Failed phase gauge instead. Counting it here too
		// would report a hold that has already been given up.
		if t := b.Status.DegradedSince; t != nil && phase == s3v1.PhaseReady {
			degraded++
			// Per Bucket rather than one aggregate timestamp: alerting on how
			// long a degradation has lasted is only actionable if it names the
			// Bucket. Absent while healthy, so `time() - <series>` is the age of
			// the hold wherever the series exists.
			ch <- prometheus.MustNewConstMetric(bucketDegradedSinceDesc, prometheus.GaugeValue,
				float64(t.Unix()), b.Namespace, b.Name)
		}
		if t := b.Status.LastRotationTime; t != nil {
			ch <- prometheus.MustNewConstMetric(lastRotationDesc, prometheus.GaugeValue,
				float64(t.Unix()), b.Namespace, b.Name)
		}
		if u := b.Status.Usage; u != nil && u.LastMeasurementTime != nil {
			measured++
			collectUsage(ch, b.Namespace, b.Name, u)
		}
	}

	for _, phase := range bucketPhases {
		ch <- prometheus.MustNewConstMetric(bucketsDesc, prometheus.GaugeValue,
			float64(byPhase[phase]), string(phase))
	}
	for _, phase := range clonePhases {
		ch <- prometheus.MustNewConstMetric(bucketsCloneDesc, prometheus.GaugeValue,
			float64(byClonePhase[phase]), string(phase))
	}
	ch <- prometheus.MustNewConstMetric(bucketsWipeOnDeleteDesc, prometheus.GaugeValue, float64(wipeOnDelete))
	ch <- prometheus.MustNewConstMetric(bucketsDegradedDesc, prometheus.GaugeValue, float64(degraded))
	ch <- prometheus.MustNewConstMetric(bucketsUsageEnabledDesc, prometheus.GaugeValue, float64(measured))
}

// collectUsage emits one Bucket's measured size and cost. It is only called for
// a Bucket that actually carries a measurement, so every series below means "was
// measured at least once" rather than "is zero bytes".
func collectUsage(ch chan<- prometheus.Metric, namespace, name string, u *s3v1.UsageStatus) {
	gauge := func(desc *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, namespace, name)
	}
	gauge(bucketSizeDesc, float64(u.Bytes))
	gauge(bucketObjectsDesc, float64(u.Objects))
	gauge(bucketVersionSizeDesc, float64(u.VersionBytes))
	gauge(bucketVersionObjectsDesc, float64(u.VersionObjects))
	gauge(bucketBillableSizeDesc, float64(u.BillableBytes))
	gauge(bucketUsageMeasuredDesc, float64(u.LastMeasurementTime.Unix()))
	gauge(bucketUsageTruncatedDesc, boolGauge(u.Truncated))
	if u.Currency != "" {
		// The estimate is rounded to whole cents, so cents are the canonical
		// value and this converts back to currency units for the metric.
		ch <- prometheus.MustNewConstMetric(bucketCostDesc, prometheus.GaugeValue,
			float64(u.EstimatedMonthlyCostCents)/100, namespace, name, u.Currency)
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
