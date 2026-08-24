//go:build e2e

// Cloud e2e: unlike the skeleton smoke tests in e2e_test.go, these run against a
// deployment that holds a REAL STACKIT service-account key, so the operator
// actually creates buckets, credentials groups, access keys and bucket policies.
// They are the only tests that exercise the whole chain — CR -> reconciler ->
// STACKIT control plane -> S3 data plane -> workload Secret -> real S3 access.
//
// Skipped unless E2E_STACKIT=1, so `make e2e-local` (skeleton mode) is unaffected.
// Run them with `make e2e-stackit`, which also guarantees teardown: the tests
// delete their CRs and wait for the finalizer to release the cloud resources
// before the Kind cluster is destroyed.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	// nsAlpha holds the grantor and the workloads it shares with; nsBeta exists
	// to prove a grant cannot cross a namespace even when the CR names collide.
	nsAlpha = "s3e2e-alpha"
	nsBeta  = "s3e2e-beta"

	// Provisioning talks to a real cloud API (bucket creation alone is eventually
	// consistent with a 60s visibility wait), and a policy change needs a moment
	// to take effect, so these are far more generous than the skeleton timeouts.
	cloudTimeout     = 8 * time.Minute
	provisionTimeout = 4 * time.Minute
	policyTimeout    = 2 * time.Minute
)

// requireCloud skips unless the operator under test was deployed with a real
// service-account key.
func requireCloud(t *testing.T) {
	t.Helper()
	if os.Getenv("E2E_STACKIT") != "1" {
		t.Skip("E2E_STACKIT != 1: operator runs in skeleton mode, no cloud resources")
	}
}

// bucketSpec is the minimum a Bucket CR needs plus the optional bits these tests
// exercise.
type bucketSpec struct {
	namespace    string
	name         string
	grantRead    []string
	wipeOnDelete bool
}

func (b bucketSpec) object() *unstructured.Unstructured {
	spec := map[string]any{
		"bucketName": b.name,
		"secretRef":  map[string]any{"name": b.name + "-s3"},
	}
	if len(b.grantRead) > 0 {
		grants := make([]any, 0, len(b.grantRead))
		for _, g := range b.grantRead {
			grants = append(grants, map[string]any{"name": g})
		}
		spec["grantReadAccess"] = grants
	}
	if b.wipeOnDelete {
		spec["wipeOnDelete"] = true
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stackit-bucket.gtrfc.com/v1",
		"kind":       "Bucket",
		"metadata":   map[string]any{"name": b.name, "namespace": b.namespace},
		"spec":       spec,
	}}
}

// ensureNamespace creates a namespace if it does not exist.
func ensureNamespace(t *testing.T, ctx context.Context, kube kubernetes.Interface, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create namespace %s", name)
	}
}

// createBucket creates the CR and registers the teardown that must run before
// the cluster goes away, otherwise the finalizer never releases the real bucket,
// credentials group and access key.
func createBucket(t *testing.T, ctx context.Context, dyn dynamic.Interface, b bucketSpec) {
	t.Helper()
	_, err := dyn.Resource(bucketGVR).Namespace(b.namespace).Create(ctx, b.object(), metav1.CreateOptions{})
	require.NoError(t, err, "create Bucket %s/%s", b.namespace, b.name)
	t.Cleanup(func() { deleteBucketAndWait(t, dyn, b.namespace, b.name) })
}

// deleteBucketAndWait deletes a Bucket CR and blocks until the object is gone,
// i.e. until the operator's finalizer finished releasing the cloud resources. A
// failure here is reported but not fatal — the sweep in hack/e2ecleanup is the
// backstop — because a t.Cleanup that fails the test would mask the real cause.
func deleteBucketAndWait(t *testing.T, dyn dynamic.Interface, ns, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()

	if err := dyn.Resource(bucketGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete Bucket %s/%s: %v", ns, name, err)
		}
		return
	}
	err := wait.PollUntilContextTimeout(ctx, pollInterval, provisionTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := dyn.Resource(bucketGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		got, gerr := dyn.Resource(bucketGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
		msg := ""
		if gerr == nil {
			msg, _, _ = unstructured.NestedString(got.Object, "status", "message")
		}
		t.Errorf("cleanup: Bucket %s/%s still present after delete (finalizer stuck, cloud resources may leak): %v (status: %s)",
			ns, name, err, msg)
		return
	}
	t.Logf("cleanup: Bucket %s/%s torn down", ns, name)
}

// waitReady blocks until the Bucket reports Ready=True and returns the CR.
func waitReady(t *testing.T, ctx context.Context, dyn dynamic.Interface, ns, name string) *unstructured.Unstructured {
	t.Helper()
	var last *unstructured.Unstructured
	err := wait.PollUntilContextTimeout(ctx, pollInterval, provisionTimeout, true, func(ctx context.Context) (bool, error) {
		got, err := dyn.Resource(bucketGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		last = got
		return readyStatus(got) == "True", nil
	})
	if err != nil {
		msg, phase := "", ""
		if last != nil {
			msg, _, _ = unstructured.NestedString(last.Object, "status", "message")
			phase, _, _ = unstructured.NestedString(last.Object, "status", "phase")
		}
		t.Fatalf("Bucket %s/%s did not become Ready: %v (phase %q, message %q)", ns, name, err, phase, msg)
	}
	return last
}

// readyStatus returns the Ready condition's status ("" when absent).
func readyStatus(u *unstructured.Unstructured) string {
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return ""
	}
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		s, _ := cond["status"].(string)
		return s
	}
	return ""
}

// grantedReadTo reads status.grantedReadTo off a Bucket CR.
func grantedReadTo(t *testing.T, ctx context.Context, dyn dynamic.Interface, ns, name string) []string {
	t.Helper()
	got, err := dyn.Resource(bucketGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	vals, _, _ := unstructured.NestedStringSlice(got.Object, "status", "grantedReadTo")
	return vals
}

// s3For builds an S3 client from the workload Secret the operator wrote, i.e.
// exactly what a consuming workload would use via envFrom. It also returns the
// physical bucket name from that same Secret.
func s3For(t *testing.T, ctx context.Context, kube kubernetes.Interface, ns, secretName string) (*minio.Client, string) {
	t.Helper()
	var sec *corev1.Secret
	err := wait.PollUntilContextTimeout(ctx, pollInterval, provisionTimeout, true, func(ctx context.Context) (bool, error) {
		s, err := kube.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if len(s.Data["AWS_ACCESS_KEY_ID"]) == 0 || len(s.Data["AWS_SECRET_ACCESS_KEY"]) == 0 {
			return false, nil
		}
		sec = s
		return true, nil
	})
	require.NoError(t, err, "credentials Secret %s/%s should be populated", ns, secretName)

	endpoint := string(sec.Data["S3_ENDPOINT"])
	bucket := string(sec.Data["S3_BUCKET"])
	require.NotEmpty(t, endpoint, "Secret must carry S3_ENDPOINT")
	require.NotEmpty(t, bucket, "Secret must carry S3_BUCKET")

	mc, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(
			string(sec.Data["AWS_ACCESS_KEY_ID"]),
			string(sec.Data["AWS_SECRET_ACCESS_KEY"]), ""),
		Secure:       true,
		Region:       string(sec.Data["S3_REGION"]),
		BucketLookup: minio.BucketLookupPath,
	})
	require.NoError(t, err, "build S3 client from Secret %s/%s", ns, secretName)
	return mc, bucket
}

func s3Denied(err error) bool {
	if err == nil {
		return false
	}
	r := minio.ToErrorResponse(err)
	return r.StatusCode == 403 || r.StatusCode == 401 || r.Code == "AccessDenied"
}

func s3Put(ctx context.Context, mc *minio.Client, bucket, key string, data []byte) error {
	putCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := mc.PutObject(putCtx, bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "text/plain"})
	return err
}

func s3Get(ctx context.Context, mc *minio.Client, bucket, key string) ([]byte, error) {
	getCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	o, err := mc.GetObject(getCtx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer o.Close()
	return io.ReadAll(o) // GetObject is lazy; the auth error surfaces here
}

func s3List(ctx context.Context, mc *minio.Client, bucket string) error {
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for obj := range mc.ListObjects(lctx, bucket, minio.ListObjectsOptions{}) {
		if obj.Err != nil {
			return obj.Err
		}
		break
	}
	return nil
}

// eventually retries fn until it returns nil or the timeout elapses. Bucket
// policies take effect asynchronously, so every assertion about access has to
// tolerate a short lag in BOTH directions (granted -> visible, revoked -> gone).
func eventually(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = fn(); last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(3 * time.Second)
	}
}

// TestCloudProvisioning is the baseline: a Bucket CR becomes a real bucket whose
// Secret carries credentials that actually work.
func TestCloudProvisioning(t *testing.T) {
	requireCloud(t)
	kube, dyn := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), cloudTimeout)
	defer cancel()

	ensureNamespace(t, ctx, kube, nsAlpha)
	createBucket(t, ctx, dyn, bucketSpec{namespace: nsAlpha, name: "solo", wipeOnDelete: true})

	cr := waitReady(t, ctx, dyn, nsAlpha, "solo")
	resolved, _, _ := unstructured.NestedString(cr.Object, "status", "resolvedBucketName")
	assert.Equal(t, "s3e2e-"+nsAlpha+"-solo", resolved, "physical name should follow the configured naming policy")

	mc, bucket := s3For(t, ctx, kube, nsAlpha, "solo-s3")
	payload := []byte("hello from e2e")
	require.NoError(t, eventually(policyTimeout, func() error {
		return s3Put(ctx, mc, bucket, "probe.txt", payload)
	}), "workload credentials should be able to write to their own bucket")

	got, err := s3Get(ctx, mc, bucket, "probe.txt")
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	t.Log("OK: bucket provisioned and its Secret credentials work")
}

// TestCloudReadGrant is the ticket's acceptance criteria end to end, through the
// operator rather than against a hand-built policy: a sibling Bucket in the same
// namespace can list and get, cannot write or delete, and a Bucket of the same
// name in another namespace is not granted anything.
func TestCloudReadGrant(t *testing.T) {
	requireCloud(t)
	kube, dyn := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), cloudTimeout)
	defer cancel()

	ensureNamespace(t, ctx, kube, nsAlpha)
	ensureNamespace(t, ctx, kube, nsBeta)

	// The reader must exist before the grantor so the grant resolves on the
	// grantor's first reconcile; the late-grantee case is covered separately.
	createBucket(t, ctx, dyn, bucketSpec{namespace: nsAlpha, name: "backups"})
	// Same CR name, other namespace: must never be granted anything.
	createBucket(t, ctx, dyn, bucketSpec{namespace: nsBeta, name: "backups"})
	createBucket(t, ctx, dyn, bucketSpec{
		namespace: nsAlpha, name: "artifacts", grantRead: []string{"backups"}, wipeOnDelete: true,
	})

	waitReady(t, ctx, dyn, nsAlpha, "backups")
	waitReady(t, ctx, dyn, nsBeta, "backups")
	waitReady(t, ctx, dyn, nsAlpha, "artifacts")

	assert.Equal(t, []string{"backups"}, grantedReadTo(t, ctx, dyn, nsAlpha, "artifacts"),
		"status should report the grant that is in effect")

	ownerMC, dataBucket := s3For(t, ctx, kube, nsAlpha, "artifacts-s3")
	readerMC, _ := s3For(t, ctx, kube, nsAlpha, "backups-s3")
	foreignMC, _ := s3For(t, ctx, kube, nsBeta, "backups-s3")

	payload := []byte("artifact payload")
	require.NoError(t, eventually(policyTimeout, func() error {
		return s3Put(ctx, ownerMC, dataBucket, "artifact.bin", payload)
	}), "owner must keep write access to its own bucket after granting a reader")

	// --- the grant works ---
	require.NoError(t, eventually(policyTimeout, func() error {
		return s3List(ctx, readerMC, dataBucket)
	}), "granted reader must be able to list the grantor's bucket")

	got, err := s3Get(ctx, readerMC, dataBucket, "artifact.bin")
	require.NoError(t, err, "granted reader must be able to get objects")
	assert.Equal(t, payload, got)
	t.Log("OK: granted reader lists and gets")

	// --- the grant is read-only ---
	assert.True(t, s3Denied(s3Put(ctx, readerMC, dataBucket, "reader.txt", []byte("nope"))),
		"granted reader must not be able to write")
	assert.True(t, s3Denied(s3Put(ctx, readerMC, dataBucket, "artifact.bin", []byte("nope"))),
		"granted reader must not be able to overwrite the owner's object")
	rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
	defer rmCancel()
	assert.True(t, s3Denied(readerMC.RemoveObject(rmCtx, dataBucket, "artifact.bin", minio.RemoveObjectOptions{})),
		"granted reader must not be able to delete")
	t.Log("OK: granted reader denied write, overwrite and delete")

	// --- the grant does not cross the namespace ---
	require.NoError(t, eventually(policyTimeout, func() error {
		_, gerr := s3Get(ctx, foreignMC, dataBucket, "artifact.bin")
		if s3Denied(gerr) {
			return nil
		}
		if gerr == nil {
			return fmt.Errorf("ISOLATION BREACH: same-named Bucket in %s could read %s", nsBeta, dataBucket)
		}
		return fmt.Errorf("unexpected error (want AccessDenied): %w", gerr)
	}), "a Bucket with the same CR name in another namespace must not be granted access")
	assert.True(t, s3Denied(s3List(ctx, foreignMC, dataBucket)),
		"cross-namespace namesake must not be able to list either")
	t.Log("OK: grant is namespace-scoped")

	// --- the owner's data survived every denied attempt ---
	got, err = s3Get(ctx, ownerMC, dataBucket, "artifact.bin")
	require.NoError(t, err)
	assert.Equal(t, payload, got, "reader must not have altered the owner's object")
}

// TestCloudReadGrantLifecycle covers the two moving parts the offline tests can
// only simulate: a grant that resolves after the grantor was already provisioned
// (the grantee watch), and a grant removed from the spec (revocation).
func TestCloudReadGrantLifecycle(t *testing.T) {
	requireCloud(t)
	kube, dyn := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), cloudTimeout)
	defer cancel()

	ensureNamespace(t, ctx, kube, nsAlpha)

	// Grantor first, referencing a Bucket that does not exist yet.
	createBucket(t, ctx, dyn, bucketSpec{
		namespace: nsAlpha, name: "reports", grantRead: []string{"late"}, wipeOnDelete: true,
	})
	waitReady(t, ctx, dyn, nsAlpha, "reports")
	assert.Empty(t, grantedReadTo(t, ctx, dyn, nsAlpha, "reports"),
		"an unresolvable grant must not be reported as in effect, and must not block Ready")
	t.Log("OK: grantor is Ready with a pending grant")

	ownerMC, dataBucket := s3For(t, ctx, kube, nsAlpha, "reports-s3")
	payload := []byte("report payload")
	require.NoError(t, eventually(policyTimeout, func() error {
		return s3Put(ctx, ownerMC, dataBucket, "report.txt", payload)
	}))

	// Now the grantee appears. Nothing touches the grantor's spec: the grantee
	// watch has to wake it.
	createBucket(t, ctx, dyn, bucketSpec{namespace: nsAlpha, name: "late"})
	waitReady(t, ctx, dyn, nsAlpha, "late")

	require.NoError(t, eventually(policyTimeout, func() error {
		if got := grantedReadTo(t, ctx, dyn, nsAlpha, "reports"); len(got) == 1 && got[0] == "late" {
			return nil
		}
		return fmt.Errorf("grant not applied yet")
	}), "the grantor must pick up the grant once the grantee exists, without its spec changing")

	lateMC, _ := s3For(t, ctx, kube, nsAlpha, "late-s3")
	require.NoError(t, eventually(policyTimeout, func() error {
		_, gerr := s3Get(ctx, lateMC, dataBucket, "report.txt")
		return gerr
	}), "the late grantee must gain read access")
	t.Log("OK: grant applied after the grantee appeared")

	// --- revocation by removing the entry from the spec ---
	patch := []byte(`[{"op":"remove","path":"/spec/grantReadAccess"}]`)
	_, err := dyn.Resource(bucketGVR).Namespace(nsAlpha).Patch(
		ctx, "reports", types.JSONPatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err, "remove the grant from the grantor's spec")

	require.NoError(t, eventually(policyTimeout, func() error {
		_, gerr := s3Get(ctx, lateMC, dataBucket, "report.txt")
		if s3Denied(gerr) {
			return nil
		}
		if gerr == nil {
			return fmt.Errorf("REVOCATION FAILED: reader still reads after the grant was removed")
		}
		return fmt.Errorf("unexpected error (want AccessDenied): %w", gerr)
	}), "removing the grant must lock the reader out again")

	// status converges separately from the S3 policy: the reconcile that rewrites
	// the policy also writes status, but that write can lose a resourceVersion
	// race against the patch that triggered it and only lands on the requeue.
	// Poll rather than assuming the two are visible at the same instant.
	require.NoError(t, eventually(policyTimeout, func() error {
		if got := grantedReadTo(t, ctx, dyn, nsAlpha, "reports"); len(got) != 0 {
			return fmt.Errorf("status.grantedReadTo still %v", got)
		}
		return nil
	}), "status.grantedReadTo must stop advertising the revoked grant")
	t.Log("OK: grant revoked")
}
