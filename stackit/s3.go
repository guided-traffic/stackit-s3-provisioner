package stackit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// BuildIsolationPolicy returns the validated per-bucket S3 bucket policy (see
// INIT-SETUP.md §4.1). It confines the bucket to exactly two principals:
//
//   - adminURN keeps full control (lockout protection + management/cleanup),
//   - workloadURN is restricted to object operations only.
//
// STACKIT/StorageGRID default access is *open* within a project, so isolation
// requires explicit Deny statements: statement 1 (Deny + NotPrincipal) locks out
// every other credentials group; statement 2 (Deny + NotAction) limits the
// workload group to object operations, overriding the implicit project-wide
// Allow. The admin group is always kept in NotPrincipal to avoid a lockout.
//
// This is the single source of truth for the policy shape; the Layer-2
// integration test delegates to it.
func BuildIsolationPolicy(bucket, adminURN, workloadURN string) string {
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

// PoliciesEquivalent reports whether two bucket-policy JSON documents are
// semantically equal, ignoring insignificant whitespace and object-key order.
// It is used to avoid re-writing an already-correct policy on every reconcile.
// If either input is not valid JSON, it falls back to a byte comparison.
func PoliciesEquivalent(a, b string) bool {
	na, err1 := normalizeJSON(a)
	nb, err2 := normalizeJSON(b)
	if err1 != nil || err2 != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return na == nb
}

// normalizeJSON parses then re-marshals a JSON document. encoding/json marshals
// object keys in sorted order, so two documents that differ only in key order
// (or whitespace) normalize to the same string.
func normalizeJSON(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// S3Admin is an S3 data-plane client authenticated with the operator's bootstrap
// admin access key. It is the only credential that can set bucket policies
// (PutBucketPolicy is not exposed by the control-plane SDK, see INIT-SETUP.md
// §3/§4.1) and it is used to inspect bucket contents for the empty-only delete
// guard. The endpoint host is region-uniform, so one client serves every bucket
// in the region.
type S3Admin struct {
	mc *minio.Client
}

// NewS3Admin builds an S3 admin client for the given endpoint host (no scheme)
// using SigV4 path-style addressing, matching STACKIT eu01.
func NewS3Admin(endpointHost, accessKeyID, secretAccessKey, region string) (*S3Admin, error) {
	mc, err := minio.New(endpointHost, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure:       true,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("init s3 admin client for %s: %w", endpointHost, err)
	}
	return &S3Admin{mc: mc}, nil
}

// SetBucketPolicy applies the given policy JSON to the bucket.
func (s *S3Admin) SetBucketPolicy(ctx context.Context, bucket, policy string) error {
	if err := s.mc.SetBucketPolicy(ctx, bucket, policy); err != nil {
		return fmt.Errorf("set bucket policy on %q: %w", bucket, err)
	}
	return nil
}

// GetBucketPolicy returns the bucket's current policy JSON. A bucket without a
// policy returns an error (NoSuchBucketPolicy), which callers treat as "needs to
// be set".
func (s *S3Admin) GetBucketPolicy(ctx context.Context, bucket string) (string, error) {
	return s.mc.GetBucketPolicy(ctx, bucket)
}

// BucketEmpty reports whether the bucket holds no objects. It is used to enforce
// the empty-only delete guard (INIT-SETUP.md §0) before any teardown, so a
// non-empty bucket never loses its credentials or data.
func (s *S3Admin) BucketEmpty(ctx context.Context, bucket string) (bool, error) {
	// Cancel the listing once we have our answer so minio's producer goroutine
	// does not block on an unread channel.
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()

	obj, ok := <-s.mc.ListObjects(lctx, bucket, minio.ListObjectsOptions{Recursive: true, MaxKeys: 1})
	if !ok {
		return true, nil // channel closed with no objects
	}
	if obj.Err != nil {
		return false, fmt.Errorf("list objects in %q: %w", bucket, obj.Err)
	}
	return false, nil // at least one object present
}
