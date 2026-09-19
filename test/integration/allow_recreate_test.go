//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// allowRecreateBucket builds a minimal Bucket CR with the given opt-in.
func allowRecreateBucket(name string, allow bool) *s3v1.Bucket {
	return &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName:    name,
			SecretRef:     s3v1.SecretReference{Name: name + "-s3"},
			AllowRecreate: allow,
		},
	}
}

// TestAllowRecreate_RoundTrips verifies the generated CRD carries
// spec.allowRecreate and that a real API server preserves it. The field decides
// whether a bucket that vanished is rebuilt unattended or reported, so a value
// dropped by a stale CRD would silently restore the behaviour the guard removes.
func TestAllowRecreate_RoundTrips(t *testing.T) {
	bucket := allowRecreateBucket("it-allow-recreate-on", true)
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })

	got := mustReadBack(t, bucket)
	assert.True(t, got.Spec.AllowRecreate)
}

// TestAllowRecreate_DefaultsToRefusing verifies that omitting the field reads
// back as false. This is the safety-relevant direction: the default has to be
// "report the loss", never "rebuild over it".
func TestAllowRecreate_DefaultsToRefusing(t *testing.T) {
	bucket := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-allow-recreate-absent", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: "it-allow-recreate-absent",
			SecretRef:  s3v1.SecretReference{Name: "it-allow-recreate-absent-s3"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })

	got := mustReadBack(t, bucket)
	assert.False(t, got.Spec.AllowRecreate)
}

// TestAllowRecreate_IsMutable verifies the field can be turned on and off after
// creation, unlike spec.bucketName. Authorising a rebuild is a decision somebody
// takes when the incident happens, so requiring a new CR to make it would defeat
// the purpose.
func TestAllowRecreate_IsMutable(t *testing.T) {
	bucket := allowRecreateBucket("it-allow-recreate-toggle", false)
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })

	mustReadBack(t, bucket)

	// The reconciler is live in this suite and adds the finalizer in its own
	// Update, so a read-modify-write here races it and loses on resourceVersion.
	// Retrying on conflict is the point of the test as much as the field is: the
	// value has to survive a write that collides with the operator's own.
	key := types.NamespacedName{Name: bucket.Name, Namespace: bucket.Namespace}
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var b s3v1.Bucket
		if err := k8sClient.Get(testCtx, key, &b); err != nil {
			return err
		}
		b.Spec.AllowRecreate = true
		return k8sClient.Update(testCtx, &b)
	}))

	// The shared client is the manager's CACHED client, so a read straight after
	// the Update races the informer and can still serve the pre-update object —
	// poll for the new VALUE, not just for readability the way mustReadBack does.
	require.Eventually(t, func() bool {
		var b s3v1.Bucket
		return k8sClient.Get(testCtx, key, &b) == nil && b.Spec.AllowRecreate
	}, 30*time.Second, 100*time.Millisecond, "spec.allowRecreate should read back as true")
}
