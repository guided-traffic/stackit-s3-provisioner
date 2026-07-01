package v1

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newBucket(namespace string, opts ...func(*Bucket)) *Bucket {
	b := &Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: namespace},
		Spec: BucketSpec{
			BucketName: "my-bucket",
			SecretRef:  SecretReference{Name: "x-s3"},
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
			b := newBucket(tc.bucketNS, func(b *Bucket) { b.Spec.SecretRef.Namespace = tc.secretNS })
			assert.Equal(t, tc.expectsNS, b.SecretNamespace())
		})
	}
}

func TestGetRegion(t *testing.T) {
	b := newBucket("default")
	assert.Equal(t, "eu01", b.GetRegion(), "empty region defaults to eu01")

	b.Spec.Region = "eu02"
	assert.Equal(t, "eu02", b.GetRegion(), "explicit region is returned")
}

func TestBucketDeepCopy(t *testing.T) {
	b := newBucket("default", func(b *Bucket) {
		b.Status.Conditions = []metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}}
		b.Spec.SecretRef.Keys = SecretKeys{AccessKeyID: "CUSTOM_AK"}
	})
	clone := b.DeepCopy()
	clone.Status.Conditions[0].Status = metav1.ConditionFalse
	clone.Spec.SecretRef.Keys.AccessKeyID = "MUTATED"
	assert.Equal(t, metav1.ConditionTrue, b.Status.Conditions[0].Status, "deepcopy must not alias the original conditions slice")
	assert.Equal(t, "CUSTOM_AK", b.Spec.SecretRef.Keys.AccessKeyID, "deepcopy must not alias the SecretRef keys")
}

// TestDeepCopyRoundTrips exercises the generated deepcopy helpers (incl. the new
// SecretKeys type) for value equality, mutation isolation and nil handling.
func TestDeepCopyRoundTrips(t *testing.T) {
	b := newBucket("team-a", func(b *Bucket) {
		b.Spec.Region = "eu02"
		b.Spec.SecretRef.Namespace = "team-b"
		b.Spec.SecretRef.Keys = SecretKeys{AccessKeyID: "AK", BucketURL: "URL"}
		b.Status.AccessKeyID = "AKIA"
		b.Status.Conditions = []metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}}
	})

	// Whole-object round-trip via the runtime.Object path.
	got, ok := b.DeepCopyObject().(*Bucket)
	require.True(t, ok)
	assert.Equal(t, b, got)

	// Per-type deep copies must equal their source.
	assert.Equal(t, b.Spec.SecretRef.Keys, *b.Spec.SecretRef.Keys.DeepCopy())
	assert.Equal(t, b.Spec.SecretRef, *b.Spec.SecretRef.DeepCopy())
	assert.Equal(t, b.Spec, *b.Spec.DeepCopy())
	assert.Equal(t, b.Status, *b.Status.DeepCopy())

	// List round-trip + mutation isolation.
	list := &BucketList{Items: []Bucket{*b}}
	gotList, ok := list.DeepCopyObject().(*BucketList)
	require.True(t, ok)
	assert.Equal(t, list, gotList)
	gotList.Items[0].Spec.BucketName = "mutated"
	assert.NotEqual(t, "mutated", b.Spec.BucketName, "deepcopied list item must not alias the source")

	// nil receivers return nil, not a panic.
	assert.Nil(t, (*Bucket)(nil).DeepCopy())
	assert.Nil(t, (*BucketList)(nil).DeepCopy())
	assert.Nil(t, (*BucketSpec)(nil).DeepCopy())
	assert.Nil(t, (*BucketStatus)(nil).DeepCopy())
	assert.Nil(t, (*SecretReference)(nil).DeepCopy())
	assert.Nil(t, (*SecretKeys)(nil).DeepCopy())
}

// TestComposeBucketName verifies the physical name is assembled from the enabled
// parts, joined by '-', with empty parts dropped.
func TestComposeBucketName(t *testing.T) {
	b := newBucket("monitoring") // spec.bucketName = my-bucket
	tests := []struct {
		name             string
		prefix           string
		includeNamespace bool
		want             string
	}{
		{"prefix and namespace", "my-cluster", true, "my-cluster-monitoring-my-bucket"},
		{"prefix only", "my-cluster", false, "my-cluster-my-bucket"},
		{"namespace only", "", true, "monitoring-my-bucket"},
		{"neither (legacy default)", "", false, "my-bucket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := BucketNaming{Prefix: tc.prefix, IncludeNamespace: tc.includeNamespace}
			assert.Equal(t, tc.want, n.ComposeBucketName(b))
		})
	}
}

// TestValidateBucketName covers the length and DNS constraints applied to the
// composed physical name.
func TestValidateBucketName(t *testing.T) {
	assert.NoError(t, ValidateBucketName("my-cluster-monitoring-my-bucket"))
	assert.NoError(t, ValidateBucketName("abc"))                   // exactly min length
	assert.NoError(t, ValidateBucketName(strings.Repeat("a", 63))) // exactly max length

	assert.Error(t, ValidateBucketName("ab"), "below min length")
	assert.Error(t, ValidateBucketName(strings.Repeat("a", 64)), "above max length")
	assert.Error(t, ValidateBucketName("My-Bucket"), "uppercase not allowed")
	assert.Error(t, ValidateBucketName("-leading"), "leading dash not allowed")
	assert.Error(t, ValidateBucketName("trailing-"), "trailing dash not allowed")
	assert.Error(t, ValidateBucketName("has_underscore"), "underscore not allowed")
}

// TestBucketNamingValidate verifies prefix validation: empty is allowed, a
// non-empty prefix must be a lowercase DNS-1123 label.
func TestBucketNamingValidate(t *testing.T) {
	assert.NoError(t, BucketNaming{Prefix: ""}.Validate(), "empty prefix is valid")
	assert.NoError(t, BucketNaming{Prefix: "my-cluster"}.Validate())
	assert.NoError(t, BucketNaming{Prefix: "c1"}.Validate())

	assert.Error(t, BucketNaming{Prefix: "My"}.Validate(), "uppercase")
	assert.Error(t, BucketNaming{Prefix: "-x"}.Validate(), "leading dash")
	assert.Error(t, BucketNaming{Prefix: "x-"}.Validate(), "trailing dash")
	assert.Error(t, BucketNaming{Prefix: "a.b"}.Validate(), "dot is not a label char")
	assert.Error(t, BucketNaming{Prefix: strings.Repeat("a", 64)}.Validate(), "too long")
}

// TestEffectiveBucketName verifies the precedence status > annotation > spec.
func TestEffectiveBucketName(t *testing.T) {
	// spec only
	b := newBucket("monitoring")
	assert.Equal(t, "my-bucket", b.EffectiveBucketName())

	// annotation backup wins over spec
	b.Annotations = map[string]string{ResolvedBucketNameAnnotation: "anno-name"}
	assert.Equal(t, "anno-name", b.EffectiveBucketName())

	// status wins over annotation
	b.Status.ResolvedBucketName = "status-name"
	assert.Equal(t, "status-name", b.EffectiveBucketName())
}

// TestSecretDataUsesEffectiveName verifies the workload Secret advertises the
// physical (resolved) bucket name, not the raw spec name.
func TestSecretDataUsesEffectiveName(t *testing.T) {
	b := newBucket("monitoring")
	b.Status.ResolvedBucketName = "my-cluster-monitoring-my-bucket"

	data := b.SecretData(SecretValues{AccessKeyID: "AKIA", SecretAccessKey: "s"})
	assert.Equal(t, []byte("my-cluster-monitoring-my-bucket"), data["S3_BUCKET"])
}

// TestGroupIdentity guards the project-wide API group and finalizer against an
// accidental revert of the rename to stackit-bucket.gtrfc.com.
func TestGroupIdentity(t *testing.T) {
	assert.Equal(t, "stackit-bucket.gtrfc.com", GroupVersion.Group)
	assert.Equal(t, "v1", GroupVersion.Version)
	assert.Equal(t, "stackit-bucket.gtrfc.com/finalizer", BucketFinalizer)
}

// TestSecretKeysDefaults verifies each resolver falls back to its documented
// default when the override field is empty.
func TestSecretKeysDefaults(t *testing.T) {
	var k SecretKeys // zero value: no overrides
	assert.Equal(t, DefaultAccessKeyIDKey, k.AccessKeyIDKey())
	assert.Equal(t, DefaultSecretAccessKeyKey, k.SecretAccessKeyKey())
	assert.Equal(t, DefaultBucketNameKey, k.BucketNameKey())
	assert.Equal(t, DefaultRegionKey, k.RegionKey())
	assert.Equal(t, DefaultEndpointKey, k.EndpointKey())
	assert.Equal(t, DefaultBucketURLKey, k.BucketURLKey())

	// Defaults must stay env-var style (consumable via envFrom).
	assert.Equal(t, "AWS_ACCESS_KEY_ID", DefaultAccessKeyIDKey)
	assert.Equal(t, "AWS_SECRET_ACCESS_KEY", DefaultSecretAccessKeyKey)
}

// TestSecretKeysOverrides verifies each resolver returns the configured override.
func TestSecretKeysOverrides(t *testing.T) {
	k := SecretKeys{
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
		BucketName:      "BKT",
		Region:          "REG",
		Endpoint:        "EP",
		BucketURL:       "URL",
	}
	assert.Equal(t, "AK", k.AccessKeyIDKey())
	assert.Equal(t, "SK", k.SecretAccessKeyKey())
	assert.Equal(t, "BKT", k.BucketNameKey())
	assert.Equal(t, "REG", k.RegionKey())
	assert.Equal(t, "EP", k.EndpointKey())
	assert.Equal(t, "URL", k.BucketURLKey())
}

// TestSecretDataDefaults verifies the default (env-var-style) data map with all
// connection values present.
func TestSecretDataDefaults(t *testing.T) {
	b := newBucket("team-a")
	b.Spec.BucketName = "my-bucket"
	b.Spec.Region = "eu02"

	data := b.SecretData(SecretValues{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "s3cr3t",
		Endpoint:        "object.storage.eu01.onstackit.cloud",
		BucketURL:       "https://object.storage.eu01.onstackit.cloud/my-bucket",
	})

	assert.Equal(t, []byte("AKIA"), data["AWS_ACCESS_KEY_ID"])
	assert.Equal(t, []byte("s3cr3t"), data["AWS_SECRET_ACCESS_KEY"])
	assert.Equal(t, []byte("my-bucket"), data["S3_BUCKET"])
	assert.Equal(t, []byte("eu02"), data["S3_REGION"])
	assert.Equal(t, []byte("object.storage.eu01.onstackit.cloud"), data["S3_ENDPOINT"])
	assert.Equal(t, []byte("https://object.storage.eu01.onstackit.cloud/my-bucket"), data["S3_BUCKET_URL"])
	assert.Len(t, data, 6)
}

// TestSecretDataRegionDefault verifies the region falls back to eu01 from
// GetRegion when spec.region is empty.
func TestSecretDataRegionDefault(t *testing.T) {
	b := newBucket("team-a") // no region set
	data := b.SecretData(SecretValues{AccessKeyID: "AKIA", SecretAccessKey: "s"})
	assert.Equal(t, []byte("eu01"), data["S3_REGION"])
}

// TestSecretDataOmitsEmptyOptional verifies optional connection fields are
// skipped when empty, while the always-present fields remain.
func TestSecretDataOmitsEmptyOptional(t *testing.T) {
	b := newBucket("team-a")
	b.Spec.BucketName = "my-bucket"

	data := b.SecretData(SecretValues{AccessKeyID: "AKIA", SecretAccessKey: "s3cr3t"})

	_, hasEndpoint := data["S3_ENDPOINT"]
	_, hasURL := data["S3_BUCKET_URL"]
	assert.False(t, hasEndpoint, "empty endpoint must be omitted")
	assert.False(t, hasURL, "empty bucketURL must be omitted")
	// Required fields stay, even though their values may be empty strings.
	assert.Contains(t, data, "AWS_ACCESS_KEY_ID")
	assert.Contains(t, data, "AWS_SECRET_ACCESS_KEY")
	assert.Equal(t, []byte("my-bucket"), data["S3_BUCKET"])
	assert.Equal(t, []byte("eu01"), data["S3_REGION"])
	assert.Len(t, data, 4)
}

// TestSecretDataCustomKeys verifies overrides route values to the configured keys
// and the defaults no longer appear.
func TestSecretDataCustomKeys(t *testing.T) {
	b := newBucket("team-a")
	b.Spec.BucketName = "my-bucket"
	b.Spec.Region = "eu01"
	b.Spec.SecretRef.Keys = SecretKeys{
		AccessKeyID:     "ACCESS_KEY",
		SecretAccessKey: "SECRET_KEY",
		BucketName:      "BUCKET",
		Region:          "REGION",
		Endpoint:        "ENDPOINT",
		BucketURL:       "BUCKET_URL",
	}

	data := b.SecretData(SecretValues{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "s3cr3t",
		Endpoint:        "host",
		BucketURL:       "https://host/my-bucket",
	})

	assert.Equal(t, []byte("AKIA"), data["ACCESS_KEY"])
	assert.Equal(t, []byte("s3cr3t"), data["SECRET_KEY"])
	assert.Equal(t, []byte("my-bucket"), data["BUCKET"])
	assert.Equal(t, []byte("eu01"), data["REGION"])
	assert.Equal(t, []byte("host"), data["ENDPOINT"])
	assert.Equal(t, []byte("https://host/my-bucket"), data["BUCKET_URL"])
	assert.NotContains(t, data, "AWS_ACCESS_KEY_ID", "default key must not appear when overridden")
	assert.Len(t, data, 6)
}

// TestValidateSecretKeysOK verifies the default and a fully-custom-but-distinct
// configuration pass validation.
func TestValidateSecretKeysOK(t *testing.T) {
	assert.NoError(t, newBucket("team-a").ValidateSecretKeys(), "defaults must not collide")

	b := newBucket("team-a")
	b.Spec.SecretRef.Keys = SecretKeys{AccessKeyID: "A", SecretAccessKey: "B"}
	assert.NoError(t, b.ValidateSecretKeys(), "distinct overrides must pass")
}

// TestValidateSecretKeysCollision verifies that two logical fields mapping to the
// same data key are rejected with a message naming both fields and the key.
func TestValidateSecretKeysCollision(t *testing.T) {
	tests := []struct {
		name string
		keys SecretKeys
		key  string
	}{
		{
			name: "two overrides collide",
			keys: SecretKeys{AccessKeyID: "SHARED", SecretAccessKey: "SHARED"},
			key:  "SHARED",
		},
		{
			name: "override collides with another field default",
			keys: SecretKeys{BucketName: DefaultRegionKey}, // bucketName -> S3_REGION == region default
			key:  DefaultRegionKey,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBucket("team-a")
			b.Spec.SecretRef.Keys = tc.keys
			err := b.ValidateSecretKeys()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)
		})
	}
}
