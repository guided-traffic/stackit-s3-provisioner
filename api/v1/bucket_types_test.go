package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newBucket(name, namespace string, opts ...func(*Bucket)) *Bucket {
	b := &Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: BucketSpec{
			BucketName: "my-bucket",
			SecretRef:  SecretReference{Name: name + "-s3"},
		},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

func TestSecretNamespace(t *testing.T) {
	tests := []struct {
		name      string
		bucketNS  string
		secretNS  string
		expectsNS string
	}{
		{name: "defaults to bucket namespace", bucketNS: "team-a", secretNS: "", expectsNS: "team-a"},
		{name: "explicit secret namespace wins", bucketNS: "team-a", secretNS: "team-b", expectsNS: "team-b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBucket("x", tc.bucketNS, func(b *Bucket) { b.Spec.SecretRef.Namespace = tc.secretNS })
			assert.Equal(t, tc.expectsNS, b.SecretNamespace())
		})
	}
}

func TestGetRegion(t *testing.T) {
	b := newBucket("x", "default")
	assert.Equal(t, "eu01", b.GetRegion(), "empty region defaults to eu01")

	b.Spec.Region = "eu02"
	assert.Equal(t, "eu02", b.GetRegion(), "explicit region is returned")
}

func TestBucketDeepCopy(t *testing.T) {
	b := newBucket("x", "default", func(b *Bucket) {
		b.Status.Conditions = []metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}}
	})
	clone := b.DeepCopy()
	clone.Status.Conditions[0].Status = metav1.ConditionFalse
	assert.Equal(t, metav1.ConditionTrue, b.Status.Conditions[0].Status, "deepcopy must not alias the original conditions slice")
}
