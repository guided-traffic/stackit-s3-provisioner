package stackit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// effectDeny is the S3 policy Effect used by both isolation statements.
const effectDeny = "Deny"

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
				"Effect":       effectDeny,
				"NotPrincipal": map[string]any{"AWS": []string{adminURN, workloadURN}},
				"Action":       []string{"s3:*"},
				"Resource":     res,
			},
			map[string]any{
				"Sid":       "workload-objects-only",
				"Effect":    effectDeny,
				"Principal": map[string]any{"AWS": workloadURN},
				// The multipart-management actions are required by clients that
				// resume or clean up chunked uploads (e.g. the Docker/GitLab
				// registry S3 driver lists in-progress multipart uploads on every
				// blob commit and 500s without them). Uploading parts itself maps
				// to s3:PutObject, but ListMultipartUploads/ListParts/Abort are
				// distinct IAM actions and must be exempted explicitly.
				"NotAction": []string{
					"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:GetBucketLocation",
					"s3:ListBucketMultipartUploads", "s3:ListMultipartUploadParts", "s3:AbortMultipartUpload",
				},
				"Resource": res,
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

// NewS3Admin builds an S3 admin client for the given endpoint using SigV4
// path-style addressing, matching STACKIT eu01. The endpoint is either a bare
// host (TLS is assumed, the production case) or a scheme-qualified URL — an
// explicit http:// endpoint (a local test fake) disables TLS.
func NewS3Admin(endpoint, accessKeyID, secretAccessKey, region string) (*S3Admin, error) {
	return newS3Client(endpoint, accessKeyID, secretAccessKey, region, minio.BucketLookupPath)
}

// NewS3VirtualHosted builds an S3 client that addresses buckets
// virtual-hosted style (bucket.endpoint.host, AWS's preferred style). Used for
// clone sources that request it; StackIT itself stays path-style.
func NewS3VirtualHosted(endpoint, accessKeyID, secretAccessKey, region string) (*S3Admin, error) {
	return newS3Client(endpoint, accessKeyID, secretAccessKey, region, minio.BucketLookupDNS)
}

// newS3Client is the shared constructor behind both addressing styles.
func newS3Client(endpoint, accessKeyID, secretAccessKey, region string, lookup minio.BucketLookupType) (*S3Admin, error) {
	host := endpoint
	secure := true
	switch {
	case strings.HasPrefix(endpoint, "http://"):
		host, secure = strings.TrimPrefix(endpoint, "http://"), false
	case strings.HasPrefix(endpoint, "https://"):
		host = strings.TrimPrefix(endpoint, "https://")
	}
	mc, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("init s3 client for %s: %w", endpoint, err)
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

// SetBucketTags replaces the bucket's tag set with the given key/value pairs.
// STACKIT/StorageGRID supports S3 bucket tagging (verified by the tagging
// integration test), so a bucket tag can carry the operator's ownership marker.
func (s *S3Admin) SetBucketTags(ctx context.Context, bucket string, kv map[string]string) error {
	t, err := tags.MapToBucketTags(kv)
	if err != nil {
		return fmt.Errorf("build bucket tags for %q: %w", bucket, err)
	}
	if err := s.mc.SetBucketTagging(ctx, bucket, t); err != nil {
		return fmt.Errorf("set bucket tagging on %q: %w", bucket, err)
	}
	return nil
}

// BucketTags returns the bucket's current tag set as a map. A bucket with no tag
// set returns an empty map (not an error), so callers can treat "untagged" and
// "tagged" uniformly when deciding ownership.
func (s *S3Admin) BucketTags(ctx context.Context, bucket string) (map[string]string, error) {
	t, err := s.mc.GetBucketTagging(ctx, bucket)
	if err != nil {
		if r := minio.ToErrorResponse(err); r.Code == "NoSuchTagSet" || r.StatusCode == 404 {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("get bucket tagging on %q: %w", bucket, err)
	}
	return t.ToMap(), nil
}

// WipeBucket deletes every object in the bucket, including all object versions
// and delete markers, so the bucket can subsequently be removed. It is only
// called during finalizer teardown when the Bucket CR explicitly requested a
// wipe (spec.wipeOnDelete) AND the operator-wide wipe feature gate is enabled;
// it must never run on a bucket this operator does not own. Idempotent: an
// already-empty bucket is a no-op.
func (s *S3Admin) WipeBucket(ctx context.Context, bucket string) error {
	// Cancel the listing on early return so minio's producer goroutine and our
	// forwarder never block on unread channels.
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()

	listCh := s.mc.ListObjects(lctx, bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true})

	// Forward listed objects to the deleter, stopping at the first listing
	// error. listErr is read only after RemoveObjects' result channel closed,
	// which happens after objCh closed, so the access is ordered.
	objCh := make(chan minio.ObjectInfo)
	var listErr error
	go func() {
		defer close(objCh)
		for obj := range listCh {
			if obj.Err != nil {
				listErr = obj.Err
				return
			}
			select {
			case objCh <- obj:
			case <-lctx.Done():
				return
			}
		}
	}()

	for rmErr := range s.mc.RemoveObjects(lctx, bucket, objCh, minio.RemoveObjectsOptions{}) {
		if rmErr.Err != nil {
			return fmt.Errorf("wipe bucket %q: delete object %q: %w", bucket, rmErr.ObjectName, rmErr.Err)
		}
	}
	if listErr != nil {
		return fmt.Errorf("wipe bucket %q: list objects: %w", bucket, listErr)
	}
	return nil
}

// BucketUsage returns the total size in bytes of all current objects in the
// bucket (one recursive listing pass). The clone feature measures the source
// bucket once before copying so the progress percentage has a stable
// denominator; S3Admin doubles as the client for arbitrary S3-compatible
// clone-source endpoints here.
func (s *S3Admin) BucketUsage(ctx context.Context, bucket string) (int64, error) {
	// Cancel the listing on early return so minio's producer goroutine does not
	// block on an unread channel.
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var total int64
	for obj := range s.mc.ListObjects(lctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return 0, fmt.Errorf("list objects in %q: %w", bucket, obj.Err)
		}
		total += obj.Size
	}
	return total, nil
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
