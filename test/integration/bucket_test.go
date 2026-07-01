//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// TestBucketReconcile_SkeletonFlow verifies that the controller wiring works end
// to end against a real API server: the reconciler adds its finalizer, sets the
// observedGeneration and reports a NotImplemented Ready condition. This is the
// integration-level contract of the operator skeleton.
func TestBucketReconcile_SkeletonFlow(t *testing.T) {
	bucket := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-bucket", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: "it-test-bucket",
			SecretRef:  s3v1.SecretReference{Name: "it-bucket-s3"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() {
		_ = k8sClient.Delete(testCtx, bucket)
	})

	key := types.NamespacedName{Name: "it-bucket", Namespace: "default"}

	// Wait for the reconciler to add the finalizer and set the Ready condition.
	require.Eventually(t, func() bool {
		var got s3v1.Bucket
		if err := k8sClient.Get(testCtx, key, &got); err != nil {
			return false
		}
		ready := apimeta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionReady)
		return controllerHasFinalizer(&got) && ready != nil
	}, 30*time.Second, 250*time.Millisecond, "bucket should get a finalizer and Ready condition")

	var got s3v1.Bucket
	require.NoError(t, k8sClient.Get(testCtx, key, &got))

	ready := apimeta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, s3v1.ReasonNotImplemented, ready.Reason)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
	assert.Equal(t, "test", got.Status.OperatorVersion)
}

// TestBucketReconcile_DeletionRemovesFinalizer verifies that deleting a Bucket
// lets the reconciler drop its finalizer so the object is garbage-collected.
func TestBucketReconcile_DeletionRemovesFinalizer(t *testing.T) {
	bucket := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-bucket-del", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: "it-test-bucket-del",
			SecretRef:  s3v1.SecretReference{Name: "it-bucket-del-s3"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, bucket))

	key := types.NamespacedName{Name: "it-bucket-del", Namespace: "default"}
	require.Eventually(t, func() bool {
		var got s3v1.Bucket
		if err := k8sClient.Get(testCtx, key, &got); err != nil {
			return false
		}
		return controllerHasFinalizer(&got)
	}, 30*time.Second, 250*time.Millisecond, "bucket should get a finalizer before deletion")

	require.NoError(t, k8sClient.Delete(testCtx, bucket))

	require.Eventually(t, func() bool {
		var got s3v1.Bucket
		err := k8sClient.Get(testCtx, key, &got)
		return client.IgnoreNotFound(err) == nil && err != nil
	}, 30*time.Second, 250*time.Millisecond, "bucket should be fully removed after finalizer is dropped")
}

// TestBucketReconcile_CustomSecretKeys verifies the CRD accepts a Bucket with
// per-key Secret name overrides and that they survive a round-trip through the
// API server, while the reconciler still wires the skeleton path.
func TestBucketReconcile_CustomSecretKeys(t *testing.T) {
	bucket := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-bucket-keys", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: "it-test-bucket-keys",
			Region:     "eu01",
			SecretRef: s3v1.SecretReference{
				Name: "it-bucket-keys-s3",
				Keys: s3v1.SecretKeys{
					AccessKeyID:     "ACCESS_KEY",
					SecretAccessKey: "SECRET_KEY",
					BucketName:      "BUCKET",
					Region:          "REGION",
					Endpoint:        "ENDPOINT",
					BucketURL:       "BUCKET_URL",
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })

	key := types.NamespacedName{Name: "it-bucket-keys", Namespace: "default"}
	require.Eventually(t, func() bool {
		var got s3v1.Bucket
		if err := k8sClient.Get(testCtx, key, &got); err != nil {
			return false
		}
		return controllerHasFinalizer(&got)
	}, 30*time.Second, 250*time.Millisecond, "bucket with custom keys should be reconciled")

	var got s3v1.Bucket
	require.NoError(t, k8sClient.Get(testCtx, key, &got))
	assert.Equal(t, "ACCESS_KEY", got.Spec.SecretRef.Keys.AccessKeyID)
	assert.Equal(t, "BUCKET_URL", got.Spec.SecretRef.Keys.BucketURL)
	assert.NoError(t, got.ValidateSecretKeys())
	// The resolver/builder must round-trip with the overrides applied.
	data := got.SecretData(s3v1.SecretValues{AccessKeyID: "AKIA", SecretAccessKey: "s"})
	assert.Equal(t, []byte("AKIA"), data["ACCESS_KEY"])
	assert.Equal(t, []byte("it-test-bucket-keys"), data["BUCKET"])
}

// TestBucket_RejectsInvalidSecretKey verifies the generated CRD pattern rejects a
// Secret data key with illegal characters (e.g. a space) at admission time.
func TestBucket_RejectsInvalidSecretKey(t *testing.T) {
	bucket := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-bucket-badkey", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: "it-test-bucket-badkey",
			SecretRef: s3v1.SecretReference{
				Name: "it-bucket-badkey-s3",
				Keys: s3v1.SecretKeys{AccessKeyID: "bad key"}, // space is illegal
			},
		},
	}
	err := k8sClient.Create(testCtx, bucket)
	require.Error(t, err, "API server should reject an invalid Secret data key")
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })
}

func controllerHasFinalizer(b *s3v1.Bucket) bool {
	for _, f := range b.Finalizers {
		if f == s3v1.BucketFinalizer {
			return true
		}
	}
	return false
}
