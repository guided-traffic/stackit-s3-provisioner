//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// TestGrantReadAccess_SelfReferenceRejected pins the CEL rule that guards
// spec.grantReadAccess against a Bucket naming itself. The rule lives at the
// schema root because only there does CEL see metadata.name, and a root rule
// that fails to compile makes the whole CRD uninstallable — so it is verified
// against a real API server rather than assumed.
func TestGrantReadAccess_SelfReferenceRejected(t *testing.T) {
	bucket := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-grant-self", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName:      "it-grant-self-bucket",
			SecretRef:       s3v1.SecretReference{Name: "it-grant-self-s3"},
			GrantReadAccess: []s3v1.LocalBucketReference{{Name: "it-grant-self"}},
		},
	}
	err := k8sClient.Create(testCtx, bucket)
	require.Error(t, err, "a Bucket granting read access to itself must be rejected")
	assert.Contains(t, err.Error(), "must not reference the Bucket itself")
}

// TestGrantReadAccess_Accepted verifies the positive path of the same schema:
// referencing a different Bucket is accepted and round-trips unchanged, and the
// listMapKey uniqueness constraint rejects a duplicated reference.
func TestGrantReadAccess_Accepted(t *testing.T) {
	bucket := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-grant-ok", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: "it-grant-ok-bucket",
			SecretRef:  s3v1.SecretReference{Name: "it-grant-ok-s3"},
			GrantReadAccess: []s3v1.LocalBucketReference{
				{Name: "it-grant-reader-a"},
				{Name: "it-grant-reader-b"},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })

	// k8sClient reads through the manager's cache, which lags the write by a
	// moment, so poll rather than racing the informer.
	var got s3v1.Bucket
	key := types.NamespacedName{Name: "it-grant-ok", Namespace: "default"}
	require.Eventually(t, func() bool {
		return k8sClient.Get(testCtx, key, &got) == nil
	}, 30*time.Second, 250*time.Millisecond, "created Bucket should become visible")
	require.Len(t, got.Spec.GrantReadAccess, 2)
	assert.Equal(t, "it-grant-reader-a", got.Spec.GrantReadAccess[0].Name)
	assert.Equal(t, "it-grant-reader-b", got.Spec.GrantReadAccess[1].Name)

	dup := &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "it-grant-dup", Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: "it-grant-dup-bucket",
			SecretRef:  s3v1.SecretReference{Name: "it-grant-dup-s3"},
			GrantReadAccess: []s3v1.LocalBucketReference{
				{Name: "it-grant-reader-a"},
				{Name: "it-grant-reader-a"},
			},
		},
	}
	err := k8sClient.Create(testCtx, dup)
	require.Error(t, err, "duplicate grantReadAccess entries must be rejected by the listMapKey")
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, dup) })
}
