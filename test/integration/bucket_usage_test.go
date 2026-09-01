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

func usagePtr(v bool) *bool { return &v }

// mustReadBack fetches a freshly created Bucket. The shared client is the
// manager's CACHED client, so a read straight after a Create races the informer;
// polling is what the other integration tests do for the same reason.
func mustReadBack(t *testing.T, b *s3v1.Bucket) *s3v1.Bucket {
	t.Helper()
	key := types.NamespacedName{Name: b.Name, Namespace: b.Namespace}
	var got s3v1.Bucket
	require.Eventually(t, func() bool {
		return k8sClient.Get(testCtx, key, &got) == nil
	}, 30*time.Second, 100*time.Millisecond, "Bucket %s should become readable", key)
	return &got
}

// usageBucket builds a Bucket CR with the given usage block.
func usageBucket(name string, usage *s3v1.UsageSpec) *s3v1.Bucket {
	return &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: s3v1.BucketSpec{
			BucketName: name,
			SecretRef:  s3v1.SecretReference{Name: name + "-s3"},
			Usage:      usage,
		},
	}
}

// TestBucketUsage_AcceptsValidSpec verifies the generated CRD accepts the usage
// block and round-trips every field through a real API server.
func TestBucketUsage_AcceptsValidSpec(t *testing.T) {
	bucket := usageBucket("it-usage-ok", &s3v1.UsageSpec{
		Enabled:         usagePtr(true),
		Interval:        "6h",
		IncludeVersions: usagePtr(true),
	})
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })

	got := mustReadBack(t, bucket)

	require.NotNil(t, got.Spec.Usage)
	// The tri-state has to survive the API server: "unset" must stay
	// distinguishable from "explicitly false", or a Bucket could no longer opt
	// out of an operator default that is on.
	require.NotNil(t, got.Spec.Usage.Enabled)
	assert.True(t, *got.Spec.Usage.Enabled)
	assert.Equal(t, "6h", got.Spec.Usage.Interval)
	require.NotNil(t, got.Spec.Usage.IncludeVersions)
	assert.True(t, *got.Spec.Usage.IncludeVersions)
}

// TestBucketUsage_OmittedBlockStaysNil verifies that a Bucket without a usage
// block reads back as nil rather than as a defaulted-to-false block, which is
// what lets the operator-wide default apply to it.
func TestBucketUsage_OmittedBlockStaysNil(t *testing.T) {
	bucket := usageBucket("it-usage-absent", nil)
	require.NoError(t, k8sClient.Create(testCtx, bucket))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bucket) })

	got := mustReadBack(t, bucket)
	assert.Nil(t, got.Spec.Usage)
}

// TestBucketUsage_RejectsInvalidInterval verifies the generated CRD pattern
// rejects an interval without a unit at admission time. A bare number is the
// classic mistake, and catching it at the API server means it never reaches the
// operator as a silently ignored value.
func TestBucketUsage_RejectsInvalidInterval(t *testing.T) {
	for _, bad := range []string{"60", "soon", "1 h", ""} {
		bucket := usageBucket("it-usage-bad", &s3v1.UsageSpec{Interval: bad})
		err := k8sClient.Create(testCtx, bucket)
		if bad == "" {
			// An empty interval is legal: it means "use the operator default".
			require.NoError(t, err, "an empty interval must be accepted")
			_ = k8sClient.Delete(testCtx, bucket)
			continue
		}
		require.Error(t, err, "API server should reject interval %q", bad)
	}
}
