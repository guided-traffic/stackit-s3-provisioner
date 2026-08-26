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
		[]string{"namespace", "name"}, nil,
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
		[]string{"namespace", "name"}, nil,
	)
)

// bucketMetricsCollector computes Bucket gauges from the manager's cache on
// every scrape, so the values self-heal and need no per-reconcile bookkeeping.
type bucketMetricsCollector struct {
	reader client.Reader
	// skeletonMode is true when the operator has no StackIT client.
	skeletonMode bool
	// wipeGateEnabled mirrors the --enable-wipe-on-delete feature gate.
	wipeGateEnabled bool
}

// RegisterBucketMetrics registers the Bucket collector with the
// controller-runtime metrics registry served on the metrics endpoint. Call it
// once per process.
func RegisterBucketMetrics(reader client.Reader, skeletonMode, wipeGateEnabled bool) {
	ctrlmetrics.Registry.MustRegister(&bucketMetricsCollector{
		reader:          reader,
		skeletonMode:    skeletonMode,
		wipeGateEnabled: wipeGateEnabled,
	})
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
}

func (c *bucketMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(skeletonModeDesc, prometheus.GaugeValue, boolGauge(c.skeletonMode))
	ch <- prometheus.MustNewConstMetric(wipeGateDesc, prometheus.GaugeValue, boolGauge(c.wipeGateEnabled))

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
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
