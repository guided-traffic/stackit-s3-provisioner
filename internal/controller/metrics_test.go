package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

func TestBucketMetricsCollector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := s3v1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	ready := newBucket("ns-a", "ready", "uid-1")
	ready.Spec.WipeOnDelete = true
	ready.Status.Phase = s3v1.PhaseReady
	ready.Status.LastRotationTime = &metav1.Time{Time: time.Unix(1700000000, 0)}

	failed := newBucket("ns-a", "failed", "uid-2")
	failed.Spec.WipeOnDelete = true
	failed.Status.Phase = s3v1.PhaseFailed

	cloning := newBucket("ns-b", "cloning", "uid-3")
	cloning.Status.Phase = s3v1.PhaseProvisioning
	cloning.Status.Clone = &s3v1.CloneStatus{Phase: s3v1.ClonePhaseRunning}

	cloneFailed := newBucket("ns-b", "clone-failed", "uid-4")
	cloneFailed.Status.Phase = s3v1.PhaseProvisioning
	cloneFailed.Status.Clone = &s3v1.CloneStatus{Phase: s3v1.ClonePhaseFailed}

	fresh := newBucket("ns-c", "fresh", "uid-5") // no status yet -> Unknown

	// A Bucket whose Ready state is being held through provider failures: it
	// still counts as Ready in the phase gauge, which is precisely why the
	// degraded gauges have to exist alongside it.
	degraded := newBucket("ns-c", "degraded", "uid-6")
	degraded.Status.Phase = s3v1.PhaseReady
	degraded.Status.DegradedSince = &metav1.Time{Time: time.Unix(1700000500, 0)}

	// A second degradation proves the timestamp series is emitted per Bucket.
	degradedOlder := newBucket("ns-c", "degraded-older", "uid-7")
	degradedOlder.Status.Phase = s3v1.PhaseReady
	degradedOlder.Status.DegradedSince = &metav1.Time{Time: time.Unix(1700000100, 0)}

	// A Bucket that already gave up the hold: degradedSince survives to record
	// when the trouble started, but it must not be counted as held any more.
	gaveUp := newBucket("ns-c", "gave-up", "uid-8")
	gaveUp.Status.Phase = s3v1.PhaseFailed
	gaveUp.Status.DegradedSince = &metav1.Time{Time: time.Unix(1700000200, 0)}

	// A measured Bucket: every usage series is emitted per Bucket, and only for
	// Buckets that actually carry a measurement — an unmeasured Bucket must be
	// ABSENT rather than reported as zero bytes.
	measured := newBucket("ns-d", "measured", "uid-9")
	measured.Status.Phase = s3v1.PhaseReady
	measured.Status.Usage = &s3v1.UsageStatus{
		Bytes:                     3_000_000_000,
		Objects:                   12,
		VersionBytes:              1_000_000_000,
		VersionObjects:            4,
		BillableBytes:             4_000_000_000,
		EstimatedMonthlyCostCents: 11,
		Currency:                  "EUR",
		LastMeasurementTime:       &metav1.Time{Time: time.Unix(1700000900, 0)},
	}

	// A truncated measurement: the values are lower bounds, which is what the
	// truncated gauge exists to say.
	capped := newBucket("ns-d", "capped", "uid-10")
	capped.Status.Phase = s3v1.PhaseReady
	capped.Status.Usage = &s3v1.UsageStatus{
		Bytes:               1,
		Objects:             1,
		BillableBytes:       1,
		Truncated:           true,
		LastMeasurementTime: &metav1.Time{Time: time.Unix(1700000950, 0)},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ready, failed, cloning, cloneFailed, fresh, degraded, degradedOlder, gaveUp,
			measured, capped).Build()

	c := &bucketMetricsCollector{reader: cl, skeletonMode: true, wipeGateEnabled: false, usageGateEnabled: true}
	expected := `
# HELP stackit_s3_provisioner_buckets Number of Bucket resources per status phase.
# TYPE stackit_s3_provisioner_buckets gauge
stackit_s3_provisioner_buckets{phase="Pending"} 0
stackit_s3_provisioner_buckets{phase="Provisioning"} 2
stackit_s3_provisioner_buckets{phase="Ready"} 5
stackit_s3_provisioner_buckets{phase="Failed"} 2
stackit_s3_provisioner_buckets{phase="Deleting"} 0
stackit_s3_provisioner_buckets{phase="Unknown"} 1
# HELP stackit_s3_provisioner_buckets_clone Number of Bucket resources per clone phase (only Buckets with a clone).
# TYPE stackit_s3_provisioner_buckets_clone gauge
stackit_s3_provisioner_buckets_clone{phase="Running"} 1
stackit_s3_provisioner_buckets_clone{phase="Completed"} 0
stackit_s3_provisioner_buckets_clone{phase="Failed"} 1
# HELP stackit_s3_provisioner_buckets_provider_degraded Number of Bucket resources whose Ready state is being held while reconciles keep failing for a non-definitive reason.
# TYPE stackit_s3_provisioner_buckets_provider_degraded gauge
stackit_s3_provisioner_buckets_provider_degraded 2
# HELP stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds Unix time at which this Bucket started degrading; absent for Buckets that are not degraded.
# TYPE stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds gauge
stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds{name="degraded",namespace="ns-c"} 1.7000005e+09
stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds{name="degraded-older",namespace="ns-c"} 1.7000001e+09
# HELP stackit_s3_provisioner_buckets_wipe_on_delete Number of Bucket resources with spec.wipeOnDelete set to true.
# TYPE stackit_s3_provisioner_buckets_wipe_on_delete gauge
stackit_s3_provisioner_buckets_wipe_on_delete 2
# HELP stackit_s3_provisioner_skeleton_mode 1 when the operator runs without a StackIT service-account key and therefore provisions nothing.
# TYPE stackit_s3_provisioner_skeleton_mode gauge
stackit_s3_provisioner_skeleton_mode 1
# HELP stackit_s3_provisioner_wipe_on_delete_gate_enabled 1 when the operator-wide --enable-wipe-on-delete feature gate is on.
# TYPE stackit_s3_provisioner_wipe_on_delete_gate_enabled gauge
stackit_s3_provisioner_wipe_on_delete_gate_enabled 0
# HELP stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds Unix time of the Bucket's last credentials rotation; absent for Buckets that were never rotated.
# TYPE stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds gauge
stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds{name="ready",namespace="ns-a"} 1.7e+09
# HELP stackit_s3_provisioner_usage_measurement_gate_enabled 1 when the operator-wide bucket size measurement gate is on.
# TYPE stackit_s3_provisioner_usage_measurement_gate_enabled gauge
stackit_s3_provisioner_usage_measurement_gate_enabled 1
# HELP stackit_s3_provisioner_buckets_usage_measured Number of Bucket resources that carry a size measurement.
# TYPE stackit_s3_provisioner_buckets_usage_measured gauge
stackit_s3_provisioner_buckets_usage_measured 2
# HELP stackit_s3_provisioner_bucket_size_bytes Size in bytes of the Bucket's current objects at the last measurement.
# TYPE stackit_s3_provisioner_bucket_size_bytes gauge
stackit_s3_provisioner_bucket_size_bytes{name="capped",namespace="ns-d"} 1
stackit_s3_provisioner_bucket_size_bytes{name="measured",namespace="ns-d"} 3e+09
# HELP stackit_s3_provisioner_bucket_objects Number of current objects in the Bucket at the last measurement.
# TYPE stackit_s3_provisioner_bucket_objects gauge
stackit_s3_provisioner_bucket_objects{name="capped",namespace="ns-d"} 1
stackit_s3_provisioner_bucket_objects{name="measured",namespace="ns-d"} 12
# HELP stackit_s3_provisioner_bucket_version_size_bytes Size in bytes of the Bucket's non-current object versions at the last measurement; 0 unless version counting is enabled.
# TYPE stackit_s3_provisioner_bucket_version_size_bytes gauge
stackit_s3_provisioner_bucket_version_size_bytes{name="capped",namespace="ns-d"} 0
stackit_s3_provisioner_bucket_version_size_bytes{name="measured",namespace="ns-d"} 1e+09
# HELP stackit_s3_provisioner_bucket_version_objects Number of non-current object versions and delete markers at the last measurement; 0 unless version counting is enabled.
# TYPE stackit_s3_provisioner_bucket_version_objects gauge
stackit_s3_provisioner_bucket_version_objects{name="capped",namespace="ns-d"} 0
stackit_s3_provisioner_bucket_version_objects{name="measured",namespace="ns-d"} 4
# HELP stackit_s3_provisioner_bucket_billable_size_bytes Size in bytes the cost estimate is computed from (current objects plus counted versions).
# TYPE stackit_s3_provisioner_bucket_billable_size_bytes gauge
stackit_s3_provisioner_bucket_billable_size_bytes{name="capped",namespace="ns-d"} 1
stackit_s3_provisioner_bucket_billable_size_bytes{name="measured",namespace="ns-d"} 4e+09
# HELP stackit_s3_provisioner_bucket_estimated_monthly_cost Estimated monthly storage cost of the Bucket at the operator's configured price, in whole currency units; absent when no price is configured.
# TYPE stackit_s3_provisioner_bucket_estimated_monthly_cost gauge
stackit_s3_provisioner_bucket_estimated_monthly_cost{currency="EUR",name="measured",namespace="ns-d"} 0.11
# HELP stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds Unix time of the Bucket's last successful size measurement; absent for Buckets that were never measured.
# TYPE stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds gauge
stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds{name="capped",namespace="ns-d"} 1.70000095e+09
stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds{name="measured",namespace="ns-d"} 1.7000009e+09
# HELP stackit_s3_provisioner_bucket_usage_truncated 1 when the Bucket's last measurement stopped at the operator's object cap, so its size and cost are lower bounds.
# TYPE stackit_s3_provisioner_bucket_usage_truncated gauge
stackit_s3_provisioner_bucket_usage_truncated{name="capped",namespace="ns-d"} 1
stackit_s3_provisioner_bucket_usage_truncated{name="measured",namespace="ns-d"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected metrics: %v", err)
	}
}
