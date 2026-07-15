package controller

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/internal/stackitfake"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// failNextCloudOp drives a fresh CR into reconcile with one injected
// control-plane/S3 failure and expects a requeue error plus phase Failed.
func failNextCloudOp(t *testing.T, op string, status int) {
	t.Helper()
	e := newTestEnv(t)
	b := newBucketCR("team-a", "app-data")
	if err := e.k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1) // finalizer only

	e.fake.FailNext(op, status)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatalf("reconcile with injected %s=%d succeeded, want error", op, status)
	}
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcileProvisioningErrorPaths(t *testing.T) {
	cases := []struct {
		name   string
		op     string
		status int
	}{
		{"enable service fails", "EnableService", 500},
		{"create group fails", "CreateGroup", 500},
		{"create bucket fails", "CreateBucket", 500},
		{"get bucket (conn info) fails", "GetBucket", 500},
		{"create access key fails", "CreateKey", 500},
		{"list access keys fails", "ListKeys", 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.op == "EnableService" {
				// EnableService is only called when the status probe fails first.
				e := newTestEnv(t)
				e.fake.DisableService()
				e.fake.FailNext("EnableService", 500)
				b := newBucketCR("team-a", "app-data")
				if err := e.k8s.Create(context.Background(), b); err != nil {
					t.Fatal(err)
				}
				e.reconcileN(t, "team-a", "app-data", 1)
				if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
					t.Fatal("reconcile succeeded, want error")
				}
				return
			}
			failNextCloudOp(t, tc.op, tc.status)
		})
	}
}

func TestTeardownErrorPaths(t *testing.T) {
	ctx := context.Background()

	provisionAndDelete := func(t *testing.T, e *testEnv, wipe bool) *s3v1.Bucket {
		t.Helper()
		b := newBucketCR("team-a", "app-data")
		b.Spec.WipeOnDelete = wipe
		b = e.provision(t, b)
		if err := e.k8s.Delete(ctx, b); err != nil {
			t.Fatal(err)
		}
		return b
	}

	cases := []struct {
		name string
		wipe bool
		prep func(e *testEnv)
	}{
		{"empty check fails", false, func(e *testEnv) { e.fake.FailNext("S3ListObjects", 403) }},
		{"ownership check fails", false, func(e *testEnv) { e.fake.FailNext("S3GetTagging", 403) }},
		{"key deletion fails", false, func(e *testEnv) { e.fake.FailNext("DeleteKey", 500) }},
		{"group deletion fails", false, func(e *testEnv) { e.fake.FailNext("DeleteGroup", 500) }},
		{"bucket deletion fails", false, func(e *testEnv) { e.fake.FailNext("DeleteBucket", 500) }},
		{"listing buckets fails", false, func(e *testEnv) { e.fake.FailNext("ListBuckets", 500) }},
		{"wipe delete fails", true, func(e *testEnv) {
			e.fake.SeedObject("app-data", "a.txt", "v1", false)
			e.fake.FailNext("S3Delete", 403)
		}},
		{"wipe ownership check fails", true, func(e *testEnv) { e.fake.FailNext("S3GetTagging", 403) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.r.EnableWipeOnDelete = true
			provisionAndDelete(t, e, tc.wipe)
			tc.prep(e)
			if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
				t.Fatal("teardown succeeded, want error (finalizer kept)")
			}
			// CR must survive a failed teardown.
			e.getBucket(t, "team-a", "app-data")

			// The failure is transient: the next reconcile finishes the teardown.
			e.reconcileN(t, "team-a", "app-data", 1)
		})
	}

	t.Run("bucket delete 404 is tolerated", func(t *testing.T) {
		e := newTestEnv(t)
		provisionAndDelete(t, e, false)
		e.fake.FailNext("DeleteBucket", 404)
		e.reconcileN(t, "team-a", "app-data", 1) // must succeed
	})
}

// newEnvWithInterceptor builds a testEnv whose Kubernetes client fails Secret
// writes according to shouldFail.
func newEnvWithInterceptor(t *testing.T, shouldFail func(obj client.Object) bool) *testEnv {
	t.Helper()

	fakeStackit := stackitfake.New(testProject, testRegion)
	t.Cleanup(fakeStackit.Close)
	sc, err := stackit.NewClientWithEndpoint(testProject, testRegion, fakeStackit.CP.URL)
	if err != nil {
		t.Fatalf("NewClientWithEndpoint: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := s3v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	failWrite := func(obj client.Object) error {
		if shouldFail(obj) {
			return fmt.Errorf("injected secret write failure")
		}
		return nil
	}
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&s3v1.Bucket{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := failWrite(obj); err != nil {
					return err
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := failWrite(obj); err != nil {
					return err
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
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

func TestSecretWriteFailureRollsBackAccessKey(t *testing.T) {
	isWorkloadSecret := func(obj client.Object) bool {
		s, ok := obj.(*corev1.Secret)
		return ok && s.Name == "app-data-s3"
	}
	e := newEnvWithInterceptor(t, isWorkloadSecret)

	b := newBucketCR("team-a", "app-data")
	if err := e.k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1) // finalizer
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded although the secret write fails, want error")
	}
	// The freshly minted key must have been rolled back: an unrecoverable
	// credential without its Secret is worthless and must not leak.
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	if n := e.fake.KeyCount(workloadGroupName(got)); n != 0 {
		t.Errorf("workload group holds %d keys after failed secret write, want 0 (rollback)", n)
	}
}

func TestAdminSecretWriteFailureRollsBackAdminKey(t *testing.T) {
	isAdminSecret := func(obj client.Object) bool {
		s, ok := obj.(*corev1.Secret)
		return ok && s.Name == testAdminSec
	}
	e := newEnvWithInterceptor(t, isAdminSecret)

	b := newBucketCR("team-a", "app-data")
	if err := e.k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded although the admin secret write fails, want error")
	}
	if n := e.fake.KeyCount(adminGroupName); n != 0 {
		t.Errorf("admin group holds %d keys after failed secret write, want 0 (rollback)", n)
	}
}
