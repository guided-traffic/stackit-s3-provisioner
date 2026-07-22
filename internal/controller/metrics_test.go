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

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ready, failed, cloning, cloneFailed, fresh).Build()

	c := &bucketMetricsCollector{reader: cl, skeletonMode: true, wipeGateEnabled: false}
	expected := `
# HELP stackit_s3_provisioner_buckets Number of Bucket resources per status phase.
# TYPE stackit_s3_provisioner_buckets gauge
stackit_s3_provisioner_buckets{phase="Pending"} 0
stackit_s3_provisioner_buckets{phase="Provisioning"} 2
stackit_s3_provisioner_buckets{phase="Ready"} 1
stackit_s3_provisioner_buckets{phase="Failed"} 1
stackit_s3_provisioner_buckets{phase="Deleting"} 0
stackit_s3_provisioner_buckets{phase="Unknown"} 1
# HELP stackit_s3_provisioner_buckets_clone Number of Bucket resources per clone phase (only Buckets with a clone).
# TYPE stackit_s3_provisioner_buckets_clone gauge
stackit_s3_provisioner_buckets_clone{phase="Running"} 1
stackit_s3_provisioner_buckets_clone{phase="Completed"} 0
stackit_s3_provisioner_buckets_clone{phase="Failed"} 1
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
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected metrics: %v", err)
	}
}
