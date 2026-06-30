//go:build integration

// Layer-2 test: the provisioner creates per-bucket workload credentials whose
// access is confined to a single bucket (read/write objects only, no bucket
// management), and a second workload's credentials cannot read that bucket.
//
//	go test -tags integration ./stackit/ -run IntegrationWorkloadCredentials -v -timeout 12m
package stackit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// bucketIsolationPolicy confines a bucket to two principals:
//   - adminURN keeps full control (lockout protection + management/cleanup),
//   - workloadURN is restricted to object operations only.
//
// StackIT default access is open within a project, so restriction requires
// explicit Deny statements (NotPrincipal denies all outsiders; NotAction limits
// the workload to object ops).
func bucketIsolationPolicy(bucket, adminURN, workloadURN string) string {
	res := []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"}
	doc := map[string]any{
		"Statement": []any{
			map[string]any{
				"Sid":          "deny-all-except-admin-and-workload",
				"Effect":       "Deny",
				"NotPrincipal": map[string]any{"AWS": []string{adminURN, workloadURN}},
				"Action":       []string{"s3:*"},
				"Resource":     res,
			},
			map[string]any{
				"Sid":       "workload-objects-only",
				"Effect":    "Deny",
				"Principal": map[string]any{"AWS": workloadURN},
				"NotAction": []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:GetBucketLocation"},
				"Resource":  res,
			},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func newMinio(t *testing.T, endpoint string, ak AccessKey) *minio.Client {
	t.Helper()
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(ak.AccessKeyID, ak.SecretAccessKey, ""),
		Secure:       true,
		Region:       RegionEU01,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("minio client (%s): %v", endpoint, err)
	}
	return mc
}

func isS3Denied(err error) bool {
	if err == nil {
		return false
	}
	r := minio.ToErrorResponse(err)
	return r.StatusCode == 403 || r.StatusCode == 401 || r.Code == "AccessDenied"
}

func putObject(mc *minio.Client, bucket, key string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := mc.PutObject(ctx, bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "text/plain"})
	return err
}

func getObject(mc *minio.Client, bucket, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	o, err := mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer o.Close()
	return io.ReadAll(o) // GetObject is lazy; auth errors surface here
}

// listDenied reports the error from attempting to list a bucket (nil if allowed).
func listErr(mc *minio.Client, bucket string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{}) {
		if obj.Err != nil {
			return obj.Err
		}
		break // listing permitted; one item is enough to know
	}
	return nil
}

func emptyBucket(ctx context.Context, mc *minio.Client, bucket string) {
	for obj := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return
		}
		_ = mc.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{})
	}
}

// retry runs fn until it returns nil or the timeout elapses (bucket policies are
// eventually consistent after SetBucketPolicy).
func retry(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = fn(); last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(2 * time.Second)
	}
}

func TestIntegrationWorkloadCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	c1, _ := integrationClients(t)
	if err := c1.EnsureService(ctx); err != nil {
		t.Fatalf("ensure service: %v", err)
	}

	type keyRef struct{ group, key string }
	var (
		buckets []string
		keys    []keyRef
		groups  []string
		adminMC *minio.Client
	)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		for _, b := range buckets {
			if adminMC != nil {
				emptyBucket(cctx, adminMC, b)
			}
			if err := c1.DeleteBucket(cctx, b); err != nil {
				t.Logf("cleanup: delete bucket %s (status %d): %v", b, StatusCode(err), err)
			}
		}
		for _, k := range keys {
			if err := c1.DeleteAccessKey(cctx, k.group, k.key); err != nil {
				t.Logf("cleanup: delete key %s: %v", k.key, err)
			}
		}
		for _, g := range groups {
			if err := c1.DeleteCredentialsGroup(cctx, g); err != nil {
				t.Logf("cleanup: delete group %s: %v", g, err)
			}
		}
	})

	sfx := fmt.Sprintf("%06d", rand.Intn(1_000_000))

	// --- two buckets ---
	bucketA := bucketName(c1.ProjectID())
	bucketB := bucketName(c1.ProjectID())
	for _, b := range []string{bucketA, bucketB} {
		if err := c1.CreateBucket(ctx, b); err != nil {
			t.Fatalf("create bucket %s: %v", b, err)
		}
		buckets = append(buckets, b)
		if err := c1.WaitBucketVisible(ctx, b, 60*time.Second); err != nil {
			t.Fatalf("wait bucket %s: %v", b, err)
		}
	}
	t.Logf("buckets: A=%s B=%s", bucketA, bucketB)

	// --- admin credentials (set policies, cleanup) ---
	adminGID, adminURN, err := c1.CreateCredentialsGroup(ctx, "operator-admin-"+sfx)
	if err != nil {
		t.Fatalf("create admin group: %v", err)
	}
	groups = append(groups, adminGID)
	adminAK, err := c1.CreateAccessKey(ctx, adminGID)
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	keys = append(keys, keyRef{adminGID, adminAK.KeyID})

	// --- per-bucket workload credentials ---
	gA, urnA, err := c1.CreateCredentialsGroup(ctx, "workload-a-"+sfx)
	if err != nil {
		t.Fatalf("create group A: %v", err)
	}
	groups = append(groups, gA)
	akA, err := c1.CreateAccessKey(ctx, gA)
	if err != nil {
		t.Fatalf("create key A: %v", err)
	}
	keys = append(keys, keyRef{gA, akA.KeyID})

	gB, urnB, err := c1.CreateCredentialsGroup(ctx, "workload-b-"+sfx)
	if err != nil {
		t.Fatalf("create group B: %v", err)
	}
	groups = append(groups, gB)
	akB, err := c1.CreateAccessKey(ctx, gB)
	if err != nil {
		t.Fatalf("create key B: %v", err)
	}
	keys = append(keys, keyRef{gB, akB.KeyID})

	// --- S3 clients ---
	endpoint, err := c1.BucketEndpointHost(ctx, bucketA)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	t.Logf("S3 endpoint: %s", endpoint)
	adminMC = newMinio(t, endpoint, adminAK)
	mcA := newMinio(t, endpoint, akA)
	mcB := newMinio(t, endpoint, akB)

	// --- apply isolation policies (admin) ---
	if err := adminMC.SetBucketPolicy(ctx, bucketA, bucketIsolationPolicy(bucketA, adminURN, urnA)); err != nil {
		t.Fatalf("set policy A: %v", err)
	}
	if err := adminMC.SetBucketPolicy(ctx, bucketB, bucketIsolationPolicy(bucketB, adminURN, urnB)); err != nil {
		t.Fatalf("set policy B: %v", err)
	}

	obj := "hello.txt"
	payload := []byte("workload-a-secret-" + sfx)

	// 1. workload A: write to its own bucket (retry until policy active).
	if err := retry(90*time.Second, func() error { return putObject(mcA, bucketA, obj, payload) }); err != nil {
		t.Fatalf("workload A write to bucket A failed: %v", err)
	}
	t.Log("OK: workload A wrote object to bucket A")

	// 2. workload A: read it back.
	got, err := getObject(mcA, bucketA, obj)
	if err != nil {
		t.Fatalf("workload A read from bucket A: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("workload A read mismatch: got %q want %q", got, payload)
	}
	t.Log("OK: workload A read its object back")

	// 3. workload B: must NOT read / list / write bucket A.
	if err := retry(90*time.Second, func() error {
		_, gerr := getObject(mcB, bucketA, obj)
		if isS3Denied(gerr) {
			return nil
		}
		if gerr == nil {
			return fmt.Errorf("ISOLATION BREACH: workload B read object in bucket A")
		}
		return fmt.Errorf("workload B read bucket A: unexpected error (want AccessDenied): %v", gerr)
	}); err != nil {
		t.Fatal(err)
	}
	if err := listErr(mcB, bucketA); !isS3Denied(err) {
		t.Fatalf("workload B list bucket A not denied: %v", err)
	}
	if err := putObject(mcB, bucketA, "intruder.txt", []byte("x")); !isS3Denied(err) {
		t.Fatalf("workload B write to bucket A not denied: %v", err)
	}
	t.Log("OK: workload B denied read/list/write on bucket A")

	// 4. workload A: object rights only — must NOT manage bucket A.
	if err := mcA.SetBucketPolicy(ctx, bucketA, bucketIsolationPolicy(bucketA, adminURN, urnA)); !isS3Denied(err) {
		t.Fatalf("workload A SetBucketPolicy on bucket A not denied: %v", err)
	}
	if err := mcA.RemoveBucket(ctx, bucketA); !isS3Denied(err) {
		t.Fatalf("workload A RemoveBucket on bucket A not denied: %v", err)
	}
	t.Log("OK: workload A denied management (SetBucketPolicy, RemoveBucket) on bucket A")

	// 5. symmetric sanity: workload A must not access bucket B.
	if err := retry(60*time.Second, func() error {
		lerr := listErr(mcA, bucketB)
		if isS3Denied(lerr) {
			return nil
		}
		if lerr == nil {
			return fmt.Errorf("ISOLATION BREACH: workload A listed bucket B")
		}
		return fmt.Errorf("workload A list bucket B: unexpected error (want AccessDenied): %v", lerr)
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("OK: workload A denied on bucket B")
}
