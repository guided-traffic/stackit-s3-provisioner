package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/internal/stackitfake"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

const (
	testProject  = "11111111-2222-3333-4444-555555555555"
	testRegion   = "eu01"
	testOpNS     = "operator-ns"
	testAdminSec = "admin-credentials"
)

// recordedEvent is one event captured by fakeRecorder.
type recordedEvent struct {
	Type   string
	Reason string
	Note   string
}

// fakeRecorder captures events for assertions.
type fakeRecorder struct {
	events []recordedEvent
}

func (f *fakeRecorder) Eventf(_ runtime.Object, _ runtime.Object, eventtype, reason, _, note string, args ...interface{}) {
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			note = s
		}
	}
	f.events = append(f.events, recordedEvent{Type: eventtype, Reason: reason, Note: note})
}

func (f *fakeRecorder) hasReason(reason string) bool {
	for _, e := range f.events {
		if e.Reason == reason {
			return true
		}
	}
	return false
}

// testEnv bundles everything a reconciler test needs.
type testEnv struct {
	r    *BucketReconciler
	k8s  client.Client
	fake *stackitfake.Server
	rec  *fakeRecorder
}

// newTestEnv builds a reconciler wired to an in-memory StackIT fake and an
// in-memory Kubernetes client.
func newTestEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()

	fakeStackit := stackitfake.New(testProject, testRegion)
	t.Cleanup(fakeStackit.Close)

	sc, err := stackit.NewClientWithEndpoint(testProject, testRegion, fakeStackit.CP.URL)
	if err != nil {
		t.Fatalf("NewClientWithEndpoint: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := s3v1.AddToScheme(scheme); err != nil {
		t.Fatalf("add s3v1 scheme: %v", err)
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&s3v1.Bucket{}).
		WithObjects(toRuntime(objs)...).
		Build()

	rec := &fakeRecorder{}
	return &testEnv{
		r: &BucketReconciler{
			Client:               k8s,
			Scheme:               scheme,
			Recorder:             rec,
			Stackit:              sc,
			OperatorVersion:      "test",
			AdminSecretName:      testAdminSec,
			AdminSecretNamespace: testOpNS,
		},
		k8s:  k8s,
		fake: fakeStackit,
		rec:  rec,
	}
}

func toRuntime(objs []client.Object) []client.Object { return objs }

// newBucketCR returns a minimal valid Bucket CR.
func newBucketCR(ns, name string) *s3v1.Bucket {
	return &s3v1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: s3v1.BucketSpec{
			BucketName: name,
			SecretRef:  s3v1.SecretReference{Name: name + "-s3"},
		},
	}
}

// reconcile runs one Reconcile for the named Bucket and returns the result.
func (e *testEnv) reconcile(t *testing.T, ns, name string) (ctrl.Result, error) {
	t.Helper()
	return e.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
}

// reconcileN runs Reconcile n times, failing the test on any error.
func (e *testEnv) reconcileN(t *testing.T, ns, name string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := e.reconcile(t, ns, name); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
}

// getBucket fetches the Bucket CR, failing on error.
func (e *testEnv) getBucket(t *testing.T, ns, name string) *s3v1.Bucket {
	t.Helper()
	var b s3v1.Bucket
	if err := e.k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &b); err != nil {
		t.Fatalf("get bucket CR %s/%s: %v", ns, name, err)
	}
	return &b
}

// provision creates the CR and reconciles it to Ready.
func (e *testEnv) provision(t *testing.T, b *s3v1.Bucket) *s3v1.Bucket {
	t.Helper()
	if err := e.k8s.Create(context.Background(), b); err != nil {
		t.Fatalf("create bucket CR: %v", err)
	}
	// 1st reconcile adds the finalizer, 2nd provisions.
	e.reconcileN(t, b.Namespace, b.Name, 2)
	got := e.getBucket(t, b.Namespace, b.Name)
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase after provisioning = %q (message %q), want Ready", got.Status.Phase, got.Status.Message)
	}
	return got
}

func TestReconcileProvisionsBucket(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))

	// Finalizer and frozen name.
	if !strings.Contains(strings.Join(b.Finalizers, ","), s3v1.BucketFinalizer) {
		t.Errorf("finalizer missing: %v", b.Finalizers)
	}
	if b.Status.ResolvedBucketName != "app-data" {
		t.Errorf("resolvedBucketName = %q, want app-data", b.Status.ResolvedBucketName)
	}
	if b.Annotations[s3v1.ResolvedBucketNameAnnotation] != "app-data" {
		t.Errorf("resolved-name annotation = %q, want app-data", b.Annotations[s3v1.ResolvedBucketNameAnnotation])
	}
	if b.Status.AccessKeyID == "" || b.Status.CredentialsGroupID == "" || b.Status.CredentialsGroupURN == "" {
		t.Errorf("status incomplete: %+v", b.Status)
	}

	// Cloud state: bucket, ownership tags, isolation policy.
	if got := e.fake.BucketNames(); len(got) != 1 || got[0] != "app-data" {
		t.Errorf("cloud buckets = %v, want [app-data]", got)
	}
	tags := e.fake.Tags("app-data")
	if tags[tagOwnershipManagedBy] != defaultOwnershipName || tags[tagOwnershipOwner] != "team-a/app-data" {
		t.Errorf("ownership tags = %v", tags)
	}
	if e.fake.Policy("app-data") == "" {
		t.Error("no bucket policy set")
	}

	// Groups: shared admin group + per-bucket workload group, one key each.
	if got := e.fake.KeyCount(adminGroupName); got != 1 {
		t.Errorf("admin group key count = %d, want 1", got)
	}
	if got := e.fake.KeyCount(workloadGroupName(b)); got != 1 {
		t.Errorf("workload group key count = %d, want 1", got)
	}

	// Workload Secret: credentials + connection info, host without scheme.
	var sec corev1.Secret
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &sec); err != nil {
		t.Fatalf("get workload secret: %v", err)
	}
	for _, k := range []string{
		s3v1.DefaultAccessKeyIDKey, s3v1.DefaultSecretAccessKeyKey,
		s3v1.DefaultBucketNameKey, s3v1.DefaultRegionKey,
		s3v1.DefaultEndpointKey, s3v1.DefaultBucketURLKey,
	} {
		if len(sec.Data[k]) == 0 {
			t.Errorf("secret data key %q missing", k)
		}
	}
	if ep := string(sec.Data[s3v1.DefaultEndpointKey]); strings.Contains(ep, "://") {
		t.Errorf("S3_ENDPOINT %q must be host-only (no scheme)", ep)
	}

	// Admin bootstrap Secret exists in the operator namespace.
	var adminSec corev1.Secret
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: testAdminSec}, &adminSec); err != nil {
		t.Fatalf("get admin secret: %v", err)
	}
	if len(adminSec.Data[adminSecretKeyURN]) == 0 {
		t.Error("admin secret misses urn")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	keyBefore := b.Status.AccessKeyID

	e.reconcileN(t, "team-a", "app-data", 2)
	b = e.getBucket(t, "team-a", "app-data")
	if b.Status.AccessKeyID != keyBefore {
		t.Errorf("access key changed on idempotent reconcile: %q -> %q", keyBefore, b.Status.AccessKeyID)
	}
	if got := e.fake.KeyCount(workloadGroupName(b)); got != 1 {
		t.Errorf("workload group key count = %d, want 1", got)
	}
}

func TestReconcileHealsLostSecret(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	keyBefore := b.Status.AccessKeyID

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app-data-s3"}}
	if err := e.k8s.Delete(ctx, sec); err != nil {
		t.Fatalf("delete workload secret: %v", err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	var fresh corev1.Secret
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &fresh); err != nil {
		t.Fatalf("secret not recreated: %v", err)
	}
	b = e.getBucket(t, "team-a", "app-data")
	if b.Status.AccessKeyID == keyBefore {
		t.Error("expected a fresh access key after secret loss")
	}
	if got := e.fake.KeyCount(workloadGroupName(b)); got != 1 {
		t.Errorf("workload group key count = %d, want exactly 1 (old key cleared)", got)
	}
}

func TestReconcileGuards(t *testing.T) {
	t.Run("secretRef targets admin secret", func(t *testing.T) {
		e := newTestEnv(t)
		b := newBucketCR(testOpNS, "evil")
		b.Spec.SecretRef.Name = testAdminSec
		if err := e.k8s.Create(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, testOpNS, "evil", 2) // no requeue error expected
		got := e.getBucket(t, testOpNS, "evil")
		if got.Status.Phase != s3v1.PhaseFailed {
			t.Errorf("phase = %q, want Failed", got.Status.Phase)
		}
	})

	t.Run("region mismatch", func(t *testing.T) {
		e := newTestEnv(t)
		b := newBucketCR("team-a", "wrong-region")
		b.Spec.Region = "eu02"
		if err := e.k8s.Create(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "wrong-region", 2)
		got := e.getBucket(t, "team-a", "wrong-region")
		if got.Status.Phase != s3v1.PhaseFailed || !strings.Contains(got.Status.Message, "region") {
			t.Errorf("phase/message = %q/%q, want Failed with region hint", got.Status.Phase, got.Status.Message)
		}
	})

	t.Run("secret key collision", func(t *testing.T) {
		e := newTestEnv(t)
		b := newBucketCR("team-a", "collide")
		b.Spec.SecretRef.Keys = s3v1.SecretKeys{AccessKeyID: "X", SecretAccessKey: "X"}
		if err := e.k8s.Create(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "collide", 2)
		got := e.getBucket(t, "team-a", "collide")
		if got.Status.Phase != s3v1.PhaseFailed {
			t.Errorf("phase = %q, want Failed", got.Status.Phase)
		}
	})

	t.Run("composed name too long", func(t *testing.T) {
		e := newTestEnv(t)
		e.r.Naming = s3v1.BucketNaming{Prefix: strings.Repeat("p", 60), IncludeNamespace: true}
		b := newBucketCR("team-a", "long-name")
		if err := e.k8s.Create(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "long-name", 2)
		got := e.getBucket(t, "team-a", "long-name")
		if got.Status.Phase != s3v1.PhaseFailed {
			t.Errorf("phase = %q, want Failed", got.Status.Phase)
		}
	})
}

func TestReconcileNamingPolicy(t *testing.T) {
	e := newTestEnv(t)
	e.r.Naming = s3v1.BucketNaming{Prefix: "clu", IncludeNamespace: true}
	b := e.provision(t, newBucketCR("team-a", "app-data"))

	want := "clu-team-a-app-data"
	if b.Status.ResolvedBucketName != want {
		t.Errorf("resolvedBucketName = %q, want %q", b.Status.ResolvedBucketName, want)
	}
	if got := e.fake.BucketNames(); len(got) != 1 || got[0] != want {
		t.Errorf("cloud buckets = %v, want [%s]", got, want)
	}
	// The physical name is frozen: a later policy change must not re-map.
	e.r.Naming = s3v1.BucketNaming{Prefix: "other"}
	e.reconcileN(t, "team-a", "app-data", 1)
	b = e.getBucket(t, "team-a", "app-data")
	if b.Status.ResolvedBucketName != want {
		t.Errorf("resolved name re-mapped to %q after policy change", b.Status.ResolvedBucketName)
	}
}

func TestReconcileOwnershipCollision(t *testing.T) {
	t.Run("foreign tagged bucket", func(t *testing.T) {
		e := newTestEnv(t)
		e.fake.SeedBucket("app-data", map[string]string{
			tagOwnershipManagedBy: "someone-else",
			tagOwnershipOwner:     "x/y",
		})
		b := newBucketCR("team-a", "app-data")
		if err := e.k8s.Create(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 2) // collision: Failed without requeue error
		got := e.getBucket(t, "team-a", "app-data")
		if got.Status.Phase != s3v1.PhaseFailed || !strings.Contains(got.Status.Message, "not owned") {
			t.Errorf("phase/message = %q/%q, want Failed ownership collision", got.Status.Phase, got.Status.Message)
		}
	})

	t.Run("untagged empty bucket is adopted", func(t *testing.T) {
		e := newTestEnv(t)
		e.fake.SeedBucket("app-data", nil)
		b := e.provision(t, newBucketCR("team-a", "app-data"))
		tags := e.fake.Tags("app-data")
		if tags[tagOwnershipOwner] != "team-a/app-data" {
			t.Errorf("adopted bucket not stamped: %v", tags)
		}
		if b.Status.Phase != s3v1.PhaseReady {
			t.Errorf("phase = %q, want Ready", b.Status.Phase)
		}
	})

	t.Run("untagged non-empty bucket is refused", func(t *testing.T) {
		e := newTestEnv(t)
		e.fake.SeedBucket("app-data", nil)
		e.fake.SeedObject("app-data", "data.bin", "v1", false)
		b := newBucketCR("team-a", "app-data")
		if err := e.k8s.Create(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 2)
		got := e.getBucket(t, "team-a", "app-data")
		if got.Status.Phase != s3v1.PhaseFailed {
			t.Errorf("phase = %q, want Failed", got.Status.Phase)
		}
	})
}

func TestReconcileTransientCloudError(t *testing.T) {
	e := newTestEnv(t)
	b := newBucketCR("team-a", "app-data")
	if err := e.k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1) // finalizer

	e.fake.FailNext("ListBuckets", 500)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile with cloud failure succeeded, want error (requeue)")
	}
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}

	// Next reconcile self-heals.
	e.reconcileN(t, "team-a", "app-data", 1)
	got = e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase after retry = %q, want Ready", got.Status.Phase)
	}
}

func TestReconcilePolicySelfHealing(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	want := e.fake.Policy("app-data")

	// Simulate manual drift.
	e.fake.SeedBucket("app-data", e.fake.Tags("app-data")) // reset resets policy+objects
	if e.fake.Policy("app-data") != "" {
		t.Fatal("test setup: policy not cleared")
	}
	e.reconcileN(t, "team-a", "app-data", 1)
	if got := e.fake.Policy("app-data"); !stackit.PoliciesEquivalent(got, want) {
		t.Errorf("policy not restored:\ngot  %s\nwant %s", got, want)
	}
	_ = b
}

func TestTeardown(t *testing.T) {
	ctx := context.Background()

	t.Run("empty bucket: full cleanup", func(t *testing.T) {
		e := newTestEnv(t)
		b := e.provision(t, newBucketCR("team-a", "app-data"))
		groupName := workloadGroupName(b)

		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 1)

		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data"}, &s3v1.Bucket{}); !apierrors.IsNotFound(err) {
			t.Errorf("bucket CR still present (err %v)", err)
		}
		if got := e.fake.BucketNames(); len(got) != 0 {
			t.Errorf("cloud buckets after teardown = %v, want none", got)
		}
		if got := e.fake.KeyCount(groupName); got != -1 {
			t.Errorf("workload group still exists (keys %d)", got)
		}
		if got := e.fake.KeyCount(adminGroupName); got != 1 {
			t.Errorf("admin group touched during teardown (keys %d, want 1)", got)
		}
		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Errorf("workload secret still present (err %v)", err)
		}
	})

	t.Run("non-empty bucket blocks deletion", func(t *testing.T) {
		e := newTestEnv(t)
		b := e.provision(t, newBucketCR("team-a", "app-data"))
		e.fake.SeedObject("app-data", "keep.bin", "v1", false)

		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatal("teardown of non-empty bucket succeeded, want blocking error")
		}
		got := e.getBucket(t, "team-a", "app-data") // CR must still exist
		if got.Status.Phase != s3v1.PhaseFailed {
			t.Errorf("phase = %q, want Failed", got.Status.Phase)
		}
		if len(e.fake.BucketNames()) != 1 {
			t.Error("bucket was deleted despite data-loss guard")
		}
	})

	t.Run("status group id lost: teardown finds group by name", func(t *testing.T) {
		e := newTestEnv(t)
		b := e.provision(t, newBucketCR("team-a", "app-data"))
		groupName := workloadGroupName(b)

		b.Status.CredentialsGroupID = ""
		if err := e.k8s.Status().Update(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 1)
		if got := e.fake.KeyCount(groupName); got != -1 {
			t.Errorf("workload group not cleaned up via name fallback (keys %d)", got)
		}
	})

	t.Run("foreign bucket is never deleted", func(t *testing.T) {
		e := newTestEnv(t)
		b := e.provision(t, newBucketCR("team-a", "app-data"))
		// Re-tag the bucket as foreign after provisioning (simulates operator
		// identity change / foreign ownership).
		e.fake.SeedBucket("app-data", map[string]string{
			tagOwnershipManagedBy: "someone-else",
			tagOwnershipOwner:     "x/y",
		})
		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 1)
		if len(e.fake.BucketNames()) != 1 {
			t.Error("foreign bucket was deleted")
		}
		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data"}, &s3v1.Bucket{}); !apierrors.IsNotFound(err) {
			t.Errorf("CR should be gone (bucket left in place), err %v", err)
		}
	})
}

func TestTeardownWipeOnDelete(t *testing.T) {
	ctx := context.Background()

	seedData := func(e *testEnv) {
		e.fake.SeedObject("app-data", "a.txt", "v1", false)
		e.fake.SeedObject("app-data", "a.txt", "v2", false)
		e.fake.SeedObject("app-data", "gone.txt", "v3", true)
	}

	t.Run("wipe enabled: bucket wiped then deleted", func(t *testing.T) {
		e := newTestEnv(t)
		e.r.EnableWipeOnDelete = true
		b := newBucketCR("team-a", "app-data")
		b.Spec.WipeOnDelete = true
		b = e.provision(t, b)
		seedData(e)

		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 1)

		if got := e.fake.BucketNames(); len(got) != 0 {
			t.Errorf("bucket not deleted after wipe: %v", got)
		}
		if !e.rec.hasReason(reasonWiping) {
			t.Errorf("missing %s event; events: %+v", reasonWiping, e.rec.events)
		}
	})

	t.Run("gate disabled: degrade to empty-only guard", func(t *testing.T) {
		e := newTestEnv(t) // EnableWipeOnDelete = false
		b := newBucketCR("team-a", "app-data")
		b.Spec.WipeOnDelete = true
		b = e.provision(t, b)
		seedData(e)

		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatal("teardown succeeded, want blocked (gate disabled)")
		}
		if e.fake.ObjectCount("app-data") != 3 {
			t.Error("objects were deleted although the wipe gate is disabled")
		}
		if !e.rec.hasReason(reasonWipeDisabled) {
			t.Errorf("missing %s event; events: %+v", reasonWipeDisabled, e.rec.events)
		}
	})

	t.Run("foreign bucket: wipe refused, empty-guard blocks", func(t *testing.T) {
		e := newTestEnv(t)
		e.r.EnableWipeOnDelete = true
		b := newBucketCR("team-a", "app-data")
		b.Spec.WipeOnDelete = true
		b = e.provision(t, b)
		seedData(e)
		// Foreign ownership after provisioning.
		tags := map[string]string{tagOwnershipManagedBy: "someone-else", tagOwnershipOwner: "x/y"}
		e.fake.SeedBucket("app-data", tags)
		seedData(e)

		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatal("teardown succeeded, want blocked (foreign bucket, no wipe)")
		}
		if e.fake.ObjectCount("app-data") != 3 {
			t.Error("foreign bucket was wiped")
		}
		if !e.rec.hasReason(reasonWipeDisabled) {
			t.Errorf("missing %s event; events: %+v", reasonWipeDisabled, e.rec.events)
		}
	})

	t.Run("wipe enabled but bucket empty: plain delete", func(t *testing.T) {
		e := newTestEnv(t)
		e.r.EnableWipeOnDelete = true
		b := newBucketCR("team-a", "app-data")
		b.Spec.WipeOnDelete = true
		b = e.provision(t, b)

		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 1)
		if got := e.fake.BucketNames(); len(got) != 0 {
			t.Errorf("bucket not deleted: %v", got)
		}
	})
}

func TestEnsureAdminRebootstrap(t *testing.T) {
	ctx := context.Background()

	t.Run("incomplete admin secret triggers rebootstrap", func(t *testing.T) {
		incomplete := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: testOpNS, Name: testAdminSec},
			Data:       map[string][]byte{adminSecretKeyAccessKeyID: []byte("stale")},
		}
		e := newTestEnv(t, incomplete)
		e.provision(t, newBucketCR("team-a", "app-data"))

		var sec corev1.Secret
		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: testAdminSec}, &sec); err != nil {
			t.Fatal(err)
		}
		if string(sec.Data[adminSecretKeyAccessKeyID]) == "stale" || len(sec.Data[adminSecretKeyURN]) == 0 {
			t.Errorf("admin secret not rebootstrapped: %v", sec.Data)
		}
	})

	t.Run("existing admin credentials are reused", func(t *testing.T) {
		e := newTestEnv(t)
		e.provision(t, newBucketCR("team-a", "app-data"))

		var before corev1.Secret
		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: testAdminSec}, &before); err != nil {
			t.Fatal(err)
		}
		// New reconciler instance (cache empty) must reuse the persisted secret.
		e.r.admin = nil
		e.provision(t, newBucketCR("team-b", "other-bucket"))
		var after corev1.Secret
		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: testAdminSec}, &after); err != nil {
			t.Fatal(err)
		}
		if string(before.Data[adminSecretKeyAccessKeyID]) != string(after.Data[adminSecretKeyAccessKeyID]) {
			t.Error("admin credentials were rotated although the secret was complete")
		}
	})

	t.Run("missing operator namespace is a hard error", func(t *testing.T) {
		e := newTestEnv(t)
		e.r.AdminSecretNamespace = ""
		b := newBucketCR("team-a", "app-data")
		if err := e.k8s.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 1) // finalizer
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatal("reconcile without operator namespace succeeded, want error")
		}
	})
}
