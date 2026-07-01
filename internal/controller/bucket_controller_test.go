package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

func newBucket(namespace, name, uid string) *s3v1.Bucket {
	return &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid)},
		Spec: s3v1.BucketSpec{
			BucketName: "bkt",
			SecretRef:  s3v1.SecretReference{Name: "creds"},
		},
	}
}

func TestDecideBucketName(t *testing.T) {
	naming := s3v1.BucketNaming{Prefix: "my-cluster", IncludeNamespace: true}

	t.Run("fresh CR composes from policy", func(t *testing.T) {
		b := newBucket("monitoring", "my-bucket", "uid")
		b.Spec.BucketName = "my-bucket"
		name, fresh := decideBucketName(naming, b)
		if name != "my-cluster-monitoring-my-bucket" || !fresh {
			t.Fatalf("got (%q, %v), want (my-cluster-monitoring-my-bucket, true)", name, fresh)
		}
	})

	t.Run("status wins and is not fresh", func(t *testing.T) {
		b := newBucket("monitoring", "my-bucket", "uid")
		b.Status.ResolvedBucketName = "frozen-name"
		name, fresh := decideBucketName(naming, b)
		if name != "frozen-name" || fresh {
			t.Fatalf("got (%q, %v), want (frozen-name, false)", name, fresh)
		}
	})

	t.Run("annotation backup wins when status lost", func(t *testing.T) {
		b := newBucket("monitoring", "my-bucket", "uid")
		b.Annotations = map[string]string{s3v1.ResolvedBucketNameAnnotation: "anno-name"}
		name, fresh := decideBucketName(naming, b)
		if name != "anno-name" || fresh {
			t.Fatalf("got (%q, %v), want (anno-name, false)", name, fresh)
		}
	})

	t.Run("pre-feature bucket keeps raw spec name", func(t *testing.T) {
		// Provisioned before the naming feature: bucketURL set, no frozen name.
		b := newBucket("monitoring", "my-bucket", "uid")
		b.Spec.BucketName = "legacy-bucket"
		b.Status.BucketURL = "https://host/legacy-bucket"
		name, fresh := decideBucketName(naming, b)
		if name != "legacy-bucket" || fresh {
			t.Fatalf("got (%q, %v), want (legacy-bucket, false)", name, fresh)
		}
	})
}

func TestPersistResolvedName(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := s3v1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	seed := newBucket("monitoring", "my-bucket", "uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed).Build()
	r := &BucketReconciler{Client: cl}
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "monitoring", Name: "my-bucket"}

	var b s3v1.Bucket
	if err := cl.Get(ctx, key, &b); err != nil {
		t.Fatalf("get: %v", err)
	}

	// First call freezes the name into the durable annotation, in memory and in the cluster.
	if err := r.persistResolvedName(ctx, &b, "frozen-name"); err != nil {
		t.Fatalf("persistResolvedName: %v", err)
	}
	if b.Annotations[s3v1.ResolvedBucketNameAnnotation] != "frozen-name" {
		t.Errorf("in-memory annotation = %q, want frozen-name", b.Annotations[s3v1.ResolvedBucketNameAnnotation])
	}
	var stored s3v1.Bucket
	if err := cl.Get(ctx, key, &stored); err != nil {
		t.Fatalf("get after persist: %v", err)
	}
	if got := stored.Annotations[s3v1.ResolvedBucketNameAnnotation]; got != "frozen-name" {
		t.Errorf("persisted annotation = %q, want frozen-name", got)
	}

	// Second call with the same name is an idempotent no-op (annotation already set).
	if err := r.persistResolvedName(ctx, &b, "frozen-name"); err != nil {
		t.Fatalf("idempotent persistResolvedName: %v", err)
	}
}

func TestWorkloadGroupName(t *testing.T) {
	b := newBucket("team-a", "reports", "abc-123")

	got := workloadGroupName(b)
	if got != workloadGroupName(b) {
		t.Fatal("workloadGroupName is not deterministic")
	}
	if !strings.HasPrefix(got, "s3op-team-a-reports-") {
		t.Errorf("unexpected name %q", got)
	}
	if len(got) > 63 {
		t.Errorf("name exceeds 63 chars: %d (%q)", len(got), got)
	}

	// Different UID -> different suffix (avoids collisions after truncation).
	other := newBucket("team-a", "reports", "def-456")
	if workloadGroupName(other) == got {
		t.Error("different UIDs must yield different group names")
	}

	// Different CR identity -> different name.
	if workloadGroupName(newBucket("team-b", "reports", "abc-123")) == got {
		t.Error("different namespace must yield a different group name")
	}
}

func TestWorkloadGroupName_LongInputsTruncatedButUnique(t *testing.T) {
	long := strings.Repeat("x", 200)
	a := workloadGroupName(newBucket(long, long, "uid-a"))
	b := workloadGroupName(newBucket(long, long, "uid-b"))
	if len(a) > 63 || len(b) > 63 {
		t.Fatalf("truncation failed: len(a)=%d len(b)=%d", len(a), len(b))
	}
	if a == b {
		t.Error("truncated names collided despite different UIDs")
	}
}

func TestSecretHasCredsAndAccessKeyID(t *testing.T) {
	b := newBucket("ns", "n", "uid")

	// Default key names.
	full := &corev1.Secret{Data: map[string][]byte{
		s3v1.DefaultAccessKeyIDKey:     []byte("AKIA"),
		s3v1.DefaultSecretAccessKeyKey: []byte("shhh"),
	}}
	if !secretHasCreds(full, b) {
		t.Error("secretHasCreds = false for a complete secret")
	}
	if got := secretAccessKeyID(full, b); got != "AKIA" {
		t.Errorf("secretAccessKeyID = %q, want AKIA", got)
	}

	// Missing secret value -> not complete.
	partial := &corev1.Secret{Data: map[string][]byte{s3v1.DefaultAccessKeyIDKey: []byte("AKIA")}}
	if secretHasCreds(partial, b) {
		t.Error("secretHasCreds = true when secretAccessKey is missing")
	}

	// Empty value -> not complete.
	empty := &corev1.Secret{Data: map[string][]byte{
		s3v1.DefaultAccessKeyIDKey:     []byte(""),
		s3v1.DefaultSecretAccessKeyKey: []byte("shhh"),
	}}
	if secretHasCreds(empty, b) {
		t.Error("secretHasCreds = true for an empty access key id")
	}
}

func TestSecretHasCreds_HonorsKeyOverrides(t *testing.T) {
	b := newBucket("ns", "n", "uid")
	b.Spec.SecretRef.Keys = s3v1.SecretKeys{AccessKeyID: "ACCESS_KEY", SecretAccessKey: "SECRET_KEY"}

	sec := &corev1.Secret{Data: map[string][]byte{
		"ACCESS_KEY": []byte("AKIA"),
		"SECRET_KEY": []byte("shhh"),
	}}
	if !secretHasCreds(sec, b) {
		t.Error("secretHasCreds must read the overridden key names")
	}
	if got := secretAccessKeyID(sec, b); got != "AKIA" {
		t.Errorf("secretAccessKeyID = %q, want AKIA", got)
	}

	// The default key names must not satisfy the check when overrides are set.
	def := &corev1.Secret{Data: map[string][]byte{
		s3v1.DefaultAccessKeyIDKey:     []byte("AKIA"),
		s3v1.DefaultSecretAccessKeyKey: []byte("shhh"),
	}}
	if secretHasCreds(def, b) {
		t.Error("secretHasCreds matched default keys despite overrides")
	}
}

func TestIsAdminSecret(t *testing.T) {
	r := &BucketReconciler{AdminSecretName: "stackit-s3-provisioner-admin", AdminSecretNamespace: "operator-ns"}

	// Exact match (same name, resolved namespace) -> admin secret.
	hit := newBucket("operator-ns", "b", "uid")
	hit.Spec.SecretRef = s3v1.SecretReference{Name: "stackit-s3-provisioner-admin", Namespace: "operator-ns"}
	if !r.isAdminSecret(hit) {
		t.Error("isAdminSecret = false for a CR targeting the admin secret by name+namespace")
	}

	// SecretRef.Namespace defaults to the Bucket's namespace when empty.
	implicit := newBucket("operator-ns", "b", "uid")
	implicit.Spec.SecretRef = s3v1.SecretReference{Name: "stackit-s3-provisioner-admin"}
	if !r.isAdminSecret(implicit) {
		t.Error("isAdminSecret = false when SecretRef.Namespace defaults to the operator namespace")
	}

	// Different name -> not the admin secret.
	nameMiss := newBucket("operator-ns", "b", "uid")
	nameMiss.Spec.SecretRef = s3v1.SecretReference{Name: "team-a-creds", Namespace: "operator-ns"}
	if r.isAdminSecret(nameMiss) {
		t.Error("isAdminSecret = true for a differently-named secret in the operator namespace")
	}

	// Same name, different namespace -> not the admin secret.
	nsMiss := newBucket("team-a", "b", "uid")
	nsMiss.Spec.SecretRef = s3v1.SecretReference{Name: "stackit-s3-provisioner-admin", Namespace: "team-a"}
	if r.isAdminSecret(nsMiss) {
		t.Error("isAdminSecret = true for the admin name in a foreign namespace")
	}
}

func TestAdminFromSecret(t *testing.T) {
	complete := &corev1.Secret{Data: map[string][]byte{
		adminSecretKeyAccessKeyID:     []byte("AKIA"),
		adminSecretKeySecretAccessKey: []byte("shhh"),
		adminSecretKeyURN:             []byte("urn:admin"),
		adminSecretKeyGroupID:         []byte("gid-1"),
	}}
	ac := adminFromSecret(complete)
	if ac == nil {
		t.Fatal("adminFromSecret = nil for a complete secret")
	}
	if ac.accessKeyID != "AKIA" || ac.secretAccessKey != "shhh" || ac.urn != "urn:admin" || ac.groupID != "gid-1" {
		t.Errorf("adminFromSecret mis-parsed: %+v", ac)
	}

	// Any missing required field (ak/sk/urn) -> nil, forcing a rebootstrap.
	for _, drop := range []string{adminSecretKeyAccessKeyID, adminSecretKeySecretAccessKey, adminSecretKeyURN} {
		sec := &corev1.Secret{Data: map[string][]byte{
			adminSecretKeyAccessKeyID:     []byte("AKIA"),
			adminSecretKeySecretAccessKey: []byte("shhh"),
			adminSecretKeyURN:             []byte("urn:admin"),
		}}
		delete(sec.Data, drop)
		if adminFromSecret(sec) != nil {
			t.Errorf("adminFromSecret returned non-nil when %q missing", drop)
		}
	}

	// groupID is optional (only used for logging/management).
	noGID := &corev1.Secret{Data: map[string][]byte{
		adminSecretKeyAccessKeyID:     []byte("AKIA"),
		adminSecretKeySecretAccessKey: []byte("shhh"),
		adminSecretKeyURN:             []byte("urn:admin"),
	}}
	if adminFromSecret(noGID) == nil {
		t.Error("adminFromSecret = nil despite all required fields present")
	}
}
