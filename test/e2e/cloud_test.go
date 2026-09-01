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

// usageSpec is the optional spec.usage block of a Bucket CR.
type usageSpec struct {
	enabled         bool
	includeVersions bool
	interval        string
}

// cloneSpec is the optional spec.cloneFrom block of a Bucket CR.
type cloneSpec struct {
	endpoint   string
	bucket     string
	region     string
	secretName string
}

// bucketSpec is the minimum a Bucket CR needs plus the optional bits these tests
// exercise.
type bucketSpec struct {
	namespace    string
	name         string
	grantRead    []string
	wipeOnDelete bool
	usage        *usageSpec
	cloneFrom    *cloneSpec
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
	if u := b.usage; u != nil {
		usage := map[string]any{"enabled": u.enabled}
		if u.includeVersions {
			usage["includeVersions"] = true
		}
		if u.interval != "" {
			usage["interval"] = u.interval
		}
		spec["usage"] = usage
	}
	if c := b.cloneFrom; c != nil {
		clone := map[string]any{
			"endpoint":  c.endpoint,
			"bucket":    c.bucket,
			"secretRef": map[string]any{"name": c.secretName},
		}
		if c.region != "" {
			clone["region"] = c.region
		}
		spec["cloneFrom"] = clone
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

// definitiveError marks a failure eventually must not keep retrying.
type definitiveError struct{ err error }

func (d definitiveError) Error() string { return d.err.Error() }

// giveUp wraps an error as definitive: waiting longer cannot turn it into a
// success, so eventually returns it immediately instead of burning the timeout.
func giveUp(err error) error { return definitiveError{err} }

// eventually retries fn until it returns nil or the timeout elapses. Bucket
// policies take effect asynchronously, so every assertion about access has to
// tolerate a short lag in BOTH directions (granted -> visible, revoked -> gone).
// An error wrapped with giveUp aborts the retry loop at once.
func eventually(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = fn(); last == nil {
			return nil
		}
		if d, ok := last.(definitiveError); ok {
			return d.err
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

// --- bucket size measurement (spec.usage) ---------------------------------

// usageTimeout bounds waiting for a measurement. The operator measures a Bucket
// as soon as it becomes Ready and then every bucketUsage.interval, which the e2e
// values shorten to seconds; the slack is for the listing round-trip and the
// status write.
const usageTimeout = 3 * time.Minute

// usageStatus is the status.usage block, read through the dynamic client.
type usageStatus struct {
	bytes         int64
	objects       int64
	versionBytes  int64
	versionObject int64
	billableBytes int64
	humanReadable string
	cost          string
	costCents     int64
	currency      string
	duration      string
	measuredAt    string
	truncated     bool
	message       string
}

// readUsage reads status.usage off a Bucket CR. found is false while the Bucket
// carries no measurement at all.
func readUsage(t *testing.T, ctx context.Context, dyn dynamic.Interface, ns, name string) (usageStatus, bool) {
	t.Helper()
	got, err := dyn.Resource(bucketGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	raw, found, _ := unstructured.NestedMap(got.Object, "status", "usage")
	if !found {
		return usageStatus{}, false
	}
	num := func(k string) int64 {
		v, ok, _ := unstructured.NestedInt64(raw, k)
		if !ok {
			return 0
		}
		return v
	}
	str := func(k string) string {
		v, _, _ := unstructured.NestedString(raw, k)
		return v
	}
	b, _, _ := unstructured.NestedBool(raw, "truncated")
	return usageStatus{
		bytes:         num("bytes"),
		objects:       num("objects"),
		versionBytes:  num("versionBytes"),
		versionObject: num("versionObjects"),
		billableBytes: num("billableBytes"),
		humanReadable: str("humanReadable"),
		cost:          str("estimatedMonthlyCost"),
		costCents:     num("estimatedMonthlyCostCents"),
		currency:      str("currency"),
		duration:      str("measurementDuration"),
		measuredAt:    str("lastMeasurementTime"),
		truncated:     b,
		message:       str("message"),
	}, true
}

// waitUsage blocks until a measurement satisfies want, and reports the last seen
// value on timeout so a failure says what the operator actually measured.
func waitUsage(t *testing.T, ctx context.Context, dyn dynamic.Interface, ns, name string,
	what string, want func(usageStatus) bool,
) usageStatus {
	t.Helper()
	var last usageStatus
	var seen bool
	err := eventually(usageTimeout, func() error {
		u, ok := readUsage(t, ctx, dyn, ns, name)
		last, seen = u, ok
		if !ok {
			return fmt.Errorf("no status.usage yet")
		}
		if !want(u) {
			return fmt.Errorf("measurement does not match yet: %+v", u)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Bucket %s/%s: %s: %v (last seen: %+v, present=%t)", ns, name, what, err, last, seen)
	}
	return last
}

// TestCloudBucketUsage measures a real bucket end to end: the operator lists it
// with the admin credentials, writes the size to status.usage and prices it.
//
// This is the part the offline suite cannot reach — it runs against an in-memory
// fake of the S3 listing. Here the listing is StorageGRID's.
func TestCloudBucketUsage(t *testing.T) {
	requireCloud(t)
	kube, dyn := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), cloudTimeout)
	defer cancel()

	ensureNamespace(t, ctx, kube, nsAlpha)
	// The e2e values keep bucketUsage.defaultEnabled off, so measuring this
	// Bucket at all proves the per-CR opt-in path.
	createBucket(t, ctx, dyn, bucketSpec{
		namespace: nsAlpha, name: "sized", wipeOnDelete: true,
		usage: &usageSpec{enabled: true},
	})
	waitReady(t, ctx, dyn, nsAlpha, "sized")

	// An empty bucket must still produce a measurement — "0 bytes" and "never
	// measured" have to stay distinguishable.
	empty := waitUsage(t, ctx, dyn, nsAlpha, "sized", "first measurement of the empty bucket",
		func(u usageStatus) bool { return u.measuredAt != "" })
	assert.Zero(t, empty.bytes, "a fresh bucket holds no bytes")
	assert.Zero(t, empty.objects, "a fresh bucket holds no objects")
	assert.False(t, empty.truncated, "a tiny bucket must not hit the object cap")
	assert.NotEmpty(t, empty.duration, "measurementDuration is the honest price of the interval")
	t.Logf("OK: empty bucket measured in %s", empty.duration)

	// Write a known amount through the workload credentials, exactly as a
	// consuming workload would.
	mc, bucket := s3For(t, ctx, kube, nsAlpha, "sized-s3")
	const payloadSize = 5 * 1024 * 1024 // 5 MiB
	payload := bytes.Repeat([]byte("x"), payloadSize)
	require.NoError(t, eventually(policyTimeout, func() error {
		return s3Put(ctx, mc, bucket, "blob.bin", payload)
	}), "workload credentials should be able to write")

	grown := waitUsage(t, ctx, dyn, nsAlpha, "sized", "re-measurement after the bucket grew",
		func(u usageStatus) bool { return u.objects == 1 })
	assert.Equal(t, int64(payloadSize), grown.bytes, "measured size must match what was written")
	assert.Equal(t, int64(payloadSize), grown.billableBytes,
		"without version counting the billable size is the current-object size")
	assert.Zero(t, grown.versionBytes, "versions are not counted unless asked for")
	assert.Equal(t, "5.0 MiB", grown.humanReadable)
	assert.False(t, grown.truncated)

	// 5 MiB is one STARTED gigabyte: 1 * 0.00003697772 * 720 h = 0.0266 EUR -> 3 cents.
	assert.Equal(t, int64(3), grown.costCents, "cost is rounded to whole cents")
	assert.Equal(t, "0.03 EUR", grown.cost)
	assert.Equal(t, "EUR", grown.currency)
	assert.Empty(t, grown.message, "a clean measurement carries no message")
	t.Logf("OK: %d bytes / %d objects measured, estimated %s per month", grown.bytes, grown.objects, grown.cost)

	// Switching measurement off must remove the values: nothing refreshes them
	// any more, so leaving them on display would assert a currency they lost.
	patch := []byte(`[{"op":"replace","path":"/spec/usage/enabled","value":false}]`)
	_, err := dyn.Resource(bucketGVR).Namespace(nsAlpha).Patch(
		ctx, "sized", types.JSONPatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err, "disable measurement on the CR")

	require.NoError(t, eventually(usageTimeout, func() error {
		if _, ok := readUsage(t, ctx, dyn, nsAlpha, "sized"); ok {
			return fmt.Errorf("status.usage still present")
		}
		return nil
	}), "disabling measurement must clear the stale size and cost")
	t.Log("OK: measurement disabled and status.usage cleared")
}

// TestCloudBucketUsageWithVersions exercises the version-counting listing
// against the real backend.
//
// It is here rather than only offline because the interesting question is a
// live one: STACKIT buckets are not versioned by default, and this asks
// StorageGRID for a VERSION listing of a bucket that has no versioning. The
// offline fake answers that happily by construction; the real backend is the
// only place where "does this path work at all" can be settled.
func TestCloudBucketUsageWithVersions(t *testing.T) {
	requireCloud(t)
	kube, dyn := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), cloudTimeout)
	defer cancel()

	ensureNamespace(t, ctx, kube, nsAlpha)
	createBucket(t, ctx, dyn, bucketSpec{
		namespace: nsAlpha, name: "versioned", wipeOnDelete: true,
		usage: &usageSpec{enabled: true, includeVersions: true},
	})
	waitReady(t, ctx, dyn, nsAlpha, "versioned")

	mc, bucket := s3For(t, ctx, kube, nsAlpha, "versioned-s3")
	const payloadSize = 1024
	payload := bytes.Repeat([]byte("v"), payloadSize)
	require.NoError(t, eventually(policyTimeout, func() error {
		return s3Put(ctx, mc, bucket, "versioned.bin", payload)
	}))

	u := waitUsage(t, ctx, dyn, nsAlpha, "versioned", "measurement with version counting",
		func(u usageStatus) bool { return u.objects == 1 })
	assert.Equal(t, int64(payloadSize), u.bytes,
		"the version listing must classify the live object as current, not as a version")
	assert.Zero(t, u.versionBytes, "an unversioned bucket has no non-current versions")
	assert.Zero(t, u.versionObject, "an unversioned bucket has no delete markers either")
	assert.Equal(t, int64(payloadSize), u.billableBytes)
	assert.Empty(t, u.message, "the version listing must not report a failure")
	t.Log("OK: version-counting measurement works against a non-versioned bucket")
}

// --- bucket clone (spec.cloneFrom) ----------------------------------------

// cloneTimeout bounds a clone: the Job has to be scheduled, rclone has to start
// and copy, and the operator polls the Job every 15s. The rclone image is
// preloaded into the Kind node by the make target, so no registry pull is on
// this path.
const cloneTimeout = 6 * time.Minute

// cloneStatus is the status.clone block, read through the dynamic client.
type cloneStatus struct {
	phase       string
	totalBytes  int64
	bytesCopied int64
	progress    string
	message     string
	completedAt string
}

func readClone(t *testing.T, ctx context.Context, dyn dynamic.Interface, ns, name string) (cloneStatus, bool) {
	t.Helper()
	got, err := dyn.Resource(bucketGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	raw, found, _ := unstructured.NestedMap(got.Object, "status", "clone")
	if !found {
		return cloneStatus{}, false
	}
	str := func(k string) string {
		v, _, _ := unstructured.NestedString(raw, k)
		return v
	}
	num := func(k string) int64 {
		v, ok, _ := unstructured.NestedInt64(raw, k)
		if !ok {
			return 0
		}
		return v
	}
	return cloneStatus{
		phase:       str("phase"),
		totalBytes:  num("totalBytes"),
		bytesCopied: num("bytesCopied"),
		progress:    str("progress"),
		message:     str("message"),
		completedAt: str("completedAt"),
	}, true
}

// secretExists reports whether the workload credentials Secret is present.
func secretExists(t *testing.T, ctx context.Context, kube kubernetes.Interface, ns, name string) bool {
	t.Helper()
	_, err := kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		t.Logf("get Secret %s/%s: %v", ns, name, err)
	}
	return false
}

// conditionStatus returns a condition's status ("" when absent).
func conditionStatus(u *unstructured.Unstructured, condType string) string {
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found {
		return ""
	}
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != condType {
			continue
		}
		s, _ := cond["status"].(string)
		return s
	}
	return ""
}

// TestCloudClone copies a real bucket into a freshly provisioned one with the
// real rclone image, which the offline suite cannot do — there the Job lifecycle
// is simulated and rclone never runs.
//
// The source is another Bucket CR: its workload Secret already carries exactly
// the keys spec.cloneFrom.secretRef expects, so this also proves the documented
// "a Secret written by this operator works as a clone source" claim.
func TestCloudClone(t *testing.T) {
	requireCloud(t)
	kube, dyn := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()

	ensureNamespace(t, ctx, kube, nsAlpha)

	// --- source bucket with content ---
	createBucket(t, ctx, dyn, bucketSpec{namespace: nsAlpha, name: "clonesrc", wipeOnDelete: true})
	waitReady(t, ctx, dyn, nsAlpha, "clonesrc")

	srcMC, srcBucket := s3For(t, ctx, kube, nsAlpha, "clonesrc-s3")
	objects := map[string][]byte{
		"top.txt":           []byte("top level object"),
		"nested/deep/a.bin": bytes.Repeat([]byte("a"), 4096),
		"nested/deep/b.bin": bytes.Repeat([]byte("b"), 8192),
	}
	var srcBytes int64
	for key, data := range objects {
		require.NoError(t, eventually(policyTimeout, func() error {
			return s3Put(ctx, srcMC, srcBucket, key, data)
		}), "seed source object %s", key)
		srcBytes += int64(len(data))
	}
	t.Logf("source bucket %s seeded with %d objects / %d bytes", srcBucket, len(objects), srcBytes)

	srcSecret, err := kube.CoreV1().Secrets(nsAlpha).Get(ctx, "clonesrc-s3", metav1.GetOptions{})
	require.NoError(t, err)
	srcEndpoint := string(srcSecret.Data["S3_ENDPOINT"])
	require.NotEmpty(t, srcEndpoint)

	// --- destination bucket that clones it ---
	createBucket(t, ctx, dyn, bucketSpec{
		namespace: nsAlpha, name: "clonedst", wipeOnDelete: true,
		cloneFrom: &cloneSpec{
			endpoint:   srcEndpoint,
			bucket:     srcBucket,
			region:     string(srcSecret.Data["S3_REGION"]),
			secretName: "clonesrc-s3",
		},
	})

	// Wait for the copy, and check the hold invariant on every poll:
	// holdSecretUntilCloned defaults to true, and the operator persists the
	// terminal clone state BEFORE it writes the workload Secret. Observing a
	// Secret while the clone is not Completed would mean a workload could start
	// against a half-copied bucket.
	var last cloneStatus
	require.NoError(t, eventually(cloneTimeout, func() error {
		c, ok := readClone(t, ctx, dyn, nsAlpha, "clonedst")
		last = c
		if secretExists(t, ctx, kube, nsAlpha, "clonedst-s3") && c.phase != "Completed" {
			return giveUp(fmt.Errorf(
				"HOLD BROKEN: workload Secret exists while clone phase is %q", c.phase))
		}
		if !ok {
			return fmt.Errorf("no status.clone yet")
		}
		if c.phase == "Failed" {
			return giveUp(fmt.Errorf("clone failed: %s", c.message))
		}
		if c.phase != "Completed" {
			return fmt.Errorf("clone phase %q (%s)", c.phase, c.progress)
		}
		return nil
	}), "clone should complete")

	assert.Equal(t, srcBytes, last.totalBytes,
		"totalBytes is measured on the source before copying and must match what was seeded")
	assert.Equal(t, srcBytes, last.bytesCopied)
	assert.NotEmpty(t, last.completedAt)
	t.Logf("OK: clone completed, %d bytes (%s)", last.totalBytes, last.progress)

	cr := waitReady(t, ctx, dyn, nsAlpha, "clonedst")
	assert.Equal(t, "True", conditionStatus(cr, "CloneCompleted"),
		"a finished clone must report the CloneCompleted condition")

	// --- the data actually arrived, byte for byte ---
	dstMC, dstBucket := s3For(t, ctx, kube, nsAlpha, "clonedst-s3")
	for key, want := range objects {
		got, err := s3Get(ctx, dstMC, dstBucket, key)
		require.NoError(t, err, "copied object %s should be readable in the destination", key)
		assert.Equal(t, want, got, "object %s must be copied byte for byte", key)
	}
	t.Logf("OK: all %d objects present in %s with identical content", len(objects), dstBucket)

	// --- the clone is one-shot ---
	// Completed is terminal: nothing re-runs the copy, so the completion stamp
	// must not move even after further reconciles (the drift resync is 1m here).
	time.Sleep(75 * time.Second)
	after, ok := readClone(t, ctx, dyn, nsAlpha, "clonedst")
	require.True(t, ok)
	assert.Equal(t, "Completed", after.phase)
	assert.Equal(t, last.completedAt, after.completedAt,
		"a completed clone must never run again")
	t.Log("OK: clone is one-shot across subsequent reconciles")
}
