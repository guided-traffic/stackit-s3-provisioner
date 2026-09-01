package stackit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
)

const (
	// effectDeny is the S3 policy Effect used by every isolation statement.
	effectDeny = "Deny"
	// actionAll is the wildcard action of statement 1: every principal outside
	// the exempted set is denied everything on the bucket.
	actionAll = "s3:*"
)

// JSON keys of an S3 policy statement, named so the same key spelled two ways
// cannot slip into different statements of the same document.
const (
	keySid          = "Sid"
	keyEffect       = "Effect"
	keyPrincipal    = "Principal"
	keyNotPrincipal = "NotPrincipal"
	keyAction       = "Action"
	keyNotAction    = "NotAction"
	keyResource     = "Resource"
	keyAWS          = "AWS"
)

// S3 action names that appear in more than one of the action lists below.
// Naming them makes a typo in one list a build failure rather than a silently
// over- or under-permissive policy, and keeps the two lists spelling the same
// action the same way.
//
// It does NOT enforce that readerAllowedActions stays a read-only subset of
// workloadAllowedActions: any of these constants — including the multipart ones
// the reader list deliberately omits — could be added to either list and still
// compile. That invariant is enforced by TestReaderAllowedActions_ReadOnly.
const (
	actGetObject                        = "s3:GetObject"
	actGetObjectVersion                 = "s3:GetObjectVersion"
	actListBucket                       = "s3:ListBucket"
	actListBucketVersions               = "s3:ListBucketVersions"
	actGetBucketLocation                = "s3:GetBucketLocation"
	actGetObjectTagging                 = "s3:GetObjectTagging"
	actGetObjectVersionTagging          = "s3:GetObjectVersionTagging"
	actGetBucketVersioning              = "s3:GetBucketVersioning"
	actGetBucketObjectLockConfiguration = "s3:GetBucketObjectLockConfiguration"

	actListBucketMultipartUploads = "s3:ListBucketMultipartUploads"
	actListMultipartUploadParts   = "s3:ListMultipartUploadParts"
	actAbortMultipartUpload       = "s3:AbortMultipartUpload"
)

// workloadAllowedActions is the exemption list of statement 2 in
// BuildIsolationPolicy. Because that statement is a Deny with NotAction, this is
// an inverted whitelist: every S3 action NOT listed here is denied to the
// workload credentials.
//
// Inclusion criterion: an action operates on object data, object metadata or
// object listings. Everything that changes *who* may access the bucket, routes
// its contents elsewhere, pins its lifetime, or rewrites history stays denied —
// see deniedByDesign below for the reasoning per group.
//
// The backend is NetApp StorageGRID, which supports a superset of the AWS action
// names plus its own s3:PutOverwriteObject. Actions that AWS folds into
// s3:PutObject are separate here, so a merely "reasonable" list silently breaks
// clients; keep this list aligned with StorageGRID's documented action set:
// https://docs.netapp.com/us-en/storagegrid-117/s3/bucket-and-group-access-policies.html
var workloadAllowedActions = []string{
	// Object data. s3:PutOverwriteObject is a StorageGRID-specific action that
	// gates any write to an *already existing* key (data, user metadata or
	// tags). Without it a plain PutObject on an existing key fails with
	// AccessDenied — which breaks every client that rewrites a key, e.g. barman
	// (CNPG backups write base/<id>/backup.info twice per backup).
	actGetObject, "s3:PutObject", "s3:PutOverwriteObject", "s3:DeleteObject",

	// Listing and endpoint discovery.
	actListBucket, actListBucketVersions, actGetBucketLocation,

	// Multipart management. Uploading parts itself maps to s3:PutObject, but
	// clients that resume or clean up chunked uploads call these distinct
	// actions (the Docker/GitLab registry S3 driver lists in-progress multipart
	// uploads on every blob commit and 500s without them).
	actListBucketMultipartUploads, actListMultipartUploadParts, actAbortMultipartUpload,

	// Object tagging. Commonly set in passing by rclone --metadata, Velero and
	// `aws s3 cp --tagging`.
	//
	// INVARIANT: this policy must never gain tag-based Condition keys
	// (s3:ExistingObjectTag/*, s3:RequestObjectTag/*). Granting the workload
	// s3:PutObjectTagging is only harmless because no access decision depends on
	// tags; with such a condition the workload could rewrite its own permissions.
	actGetObjectTagging, "s3:PutObjectTagging", "s3:DeleteObjectTagging",

	// Version-aware *reads*. Dormant while versioning is off (the workload
	// cannot enable it — s3:PutBucketVersioning is denied), but harmless and
	// present so a later versioning feature does not need another policy fix.
	actGetObjectVersion, actGetObjectVersionTagging,

	// Read-only bucket configuration probes issued by several SDKs before a
	// write. They expose no secret; denying them only produces confusing
	// AccessDenied errors on the client side.
	actGetBucketVersioning, actGetBucketObjectLockConfiguration,
}

// readerAllowedActions is the exemption list of the optional third statement in
// BuildIsolationPolicy (see the readerURNs parameter). Like statement 2 it is a
// Deny with NotAction, so this is an inverted whitelist: a granted reader may do
// exactly what is listed here and nothing else.
//
// It is the strict read-only subset of workloadAllowedActions. Every entry was
// checked against that list: an action only qualifies if it cannot create,
// modify or destroy state — neither object data nor metadata, tags, versions or
// bucket configuration. Consequences of that rule, recorded so extending the
// list stays a conscious decision:
//
//   - No s3:PutObject / PutOverwriteObject / DeleteObject / *ObjectTagging
//     writes: a reader must never be able to alter the grantor's data. This is
//     the whole point of the grant being read-only.
//   - No s3:ListBucketMultipartUploads / ListMultipartUploadParts: they expose
//     the *owner's* in-flight uploads (key names and part layout of data that is
//     not yet committed). Reading finished objects does not require them, and
//     s3cmd/aws-cli only issue them for their own uploads.
//   - No s3:AbortMultipartUpload: it destroys the owner's in-flight upload.
//
// s3:GetBucketLocation is included because SigV4 clients (s3cmd, aws-cli, minio)
// resolve the bucket region before the first request and fail confusingly
// without it. It leaks nothing beyond the region the reader already addresses.
var readerAllowedActions = []string{
	// Object reads, current and historic versions.
	actGetObject, actGetObjectVersion,

	// Listing and endpoint discovery.
	actListBucket, actListBucketVersions, actGetBucketLocation,

	// Read-only metadata probes several SDKs issue before a GET.
	actGetObjectTagging, actGetObjectVersionTagging,
	actGetBucketVersioning, actGetBucketObjectLockConfiguration,
}

// Denied by design — actions deliberately absent from workloadAllowedActions,
// recorded so that extending the list stays a conscious decision:
//
//   - s3:GetBucketPolicy / PutBucketPolicy / DeleteBucketPolicy — the workload
//     could lift its own restrictions; Get additionally leaks the admin URN.
//   - s3:PutReplicationConfiguration / PutBucketNotification /
//     PutBucketMetadataNotification — exfiltration channels: they mirror bucket
//     contents to a destination the workload chooses.
//   - s3:PutObjectRetention / PutObjectLegalHold /
//     PutBucketObjectLockConfiguration / PutBucketCompliance /
//     s3:BypassGovernanceRetention — a workload could pin objects permanently
//     and make the bucket undeletable, breaking finalizer teardown.
//   - s3:DeleteObjectVersion / DeleteObjectVersionTagging /
//     PutObjectVersionTagging — mutating or destroying historic versions. Only
//     current-version writes are granted, so versioning keeps its recovery value.
//   - s3:PutBucketVersioning / PutLifecycleConfiguration /
//     PutEncryptionConfiguration / PutBucketCORS / PutBucketTagging /
//     s3:DeleteBucket — bucket reconfiguration; owned by the operator.
//   - s3:GetObjectAcl / GetBucketAcl — not needed; ACLs are unused here.

// BuildIsolationPolicy returns the validated per-bucket S3 bucket policy (see
// INIT-SETUP.md §4.1). It confines the bucket to a small, explicit principal set:
//
//   - adminURN keeps full control (lockout protection + management/cleanup),
//   - workloadURN is restricted to object operations only,
//   - each entry of readerURNs is restricted to read-only operations.
//
// STACKIT/StorageGRID default access is *open* within a project, so isolation
// requires explicit Deny statements: statement 1 (Deny + NotPrincipal) locks out
// every other credentials group; statement 2 (Deny + NotAction) limits the
// workload group to object operations, overriding the implicit project-wide
// Allow. The admin group is always kept in NotPrincipal to avoid a lockout.
//
// readerURNs implements spec.grantReadAccess: the workload groups of other
// Bucket CRs in the grantor's namespace that were granted read-only access. It
// is optional — with no readers the returned document is byte-identical to the
// two-statement policy that predates the feature, so enabling the feature never
// rewrites the policy of a bucket that does not use it.
//
// SECURITY: readerURNs is sanitized here rather than only at the call site,
// because the consequences of a bad entry are severe and irreversible-ish:
//
//   - The admin URN appearing as a reader would confine the operator's own key
//     to read-only on this bucket — including PutBucketPolicy, i.e. a permanent
//     lockout that no later reconcile could repair (StorageGRID can lock out
//     even the account root, see the NotPrincipal invariant above).
//   - The workload URN appearing as a reader would add a second, *narrower*
//     Deny on the bucket owner. Deny is not overridden by another statement, so
//     the intersection wins and the owner would silently lose write access to
//     its own bucket.
//
// Both are therefore filtered out unconditionally, which also makes a
// self-grant (a Bucket naming itself in spec.grantReadAccess) a harmless no-op.
// Empty entries are dropped, duplicates collapsed and the result sorted so the
// document is deterministic and the drift check in ensureBucketPolicy does not
// see spurious changes from map/slice ordering.
//
// This is the single source of truth for the policy shape; the Layer-2
// integration test delegates to it.
func BuildIsolationPolicy(bucket, adminURN, workloadURN string, readerURNs []string) string {
	res := []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"}
	readers := sanitizeReaderURNs(readerURNs, adminURN, workloadURN)

	// Statement 1 exempts admin, workload and every reader from the blanket deny;
	// without the exemption a reader would be denied by this statement regardless
	// of statement 3.
	exempt := append([]string{adminURN, workloadURN}, readers...)

	statements := []any{
		map[string]any{
			keySid:          "deny-all-except-admin-and-workload",
			keyEffect:       effectDeny,
			keyNotPrincipal: map[string]any{keyAWS: exempt},
			keyAction:       []string{actionAll},
			keyResource:     res,
		},
		map[string]any{
			keySid:       "workload-objects-only",
			keyEffect:    effectDeny,
			keyPrincipal: map[string]any{keyAWS: workloadURN},
			keyNotAction: workloadAllowedActions,
			keyResource:  res,
		},
	}
	if len(readers) > 0 {
		statements = append(statements, map[string]any{
			keySid:       "granted-readers-read-only",
			keyEffect:    effectDeny,
			keyPrincipal: map[string]any{keyAWS: readers},
			keyNotAction: readerAllowedActions,
			keyResource:  res,
		})
	}

	doc := map[string]any{"Statement": statements}
	b, _ := json.Marshal(doc)
	return string(b)
}

// sanitizeReaderURNs normalizes the reader principal list of
// BuildIsolationPolicy: it drops empty entries, drops the admin and workload
// URNs (see the SECURITY note there), collapses duplicates and sorts the rest.
// It returns nil for an empty result so callers can test with len() and the
// policy stays two statements.
func sanitizeReaderURNs(readerURNs []string, adminURN, workloadURN string) []string {
	if len(readerURNs) == 0 {
		return nil
	}
	// Compare trimmed against trimmed. The candidate is trimmed anyway (a URN
	// with surrounding whitespace is the same principal to StorageGRID), so
	// comparing it against a raw adminURN would let a padded value slip past the
	// exclusion that guards against the admin lockout — adminFromSecret reads the
	// admin URN straight out of Secret data and does not trim it either.
	adminURN = strings.TrimSpace(adminURN)
	workloadURN = strings.TrimSpace(workloadURN)

	seen := make(map[string]struct{}, len(readerURNs))
	out := make([]string, 0, len(readerURNs))
	for _, urn := range readerURNs {
		urn = strings.TrimSpace(urn)
		if urn == "" || urn == adminURN || urn == workloadURN {
			continue
		}
		if _, dup := seen[urn]; dup {
			continue
		}
		seen[urn] = struct{}{}
		out = append(out, urn)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
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

// BucketStats is the measured content of a bucket: the size and number of its
// current objects, and — when versions were counted — of the non-current
// versions and delete markers that share the bucket's billed storage.
type BucketStats struct {
	// Bytes is the total size of the current objects.
	Bytes int64
	// Objects is the number of current objects.
	Objects int64
	// VersionBytes is the total size of non-current object versions. It stays
	// zero when versions were not counted.
	VersionBytes int64
	// VersionObjects is the number of non-current versions and delete markers.
	// Delete markers carry no bytes but do occupy a version.
	VersionObjects int64
	// Truncated reports that the listing stopped at the caller's entry cap, so
	// every number above is a lower bound.
	Truncated bool
}

// BillableBytes is the figure a storage bill is computed from: current objects
// plus whatever non-current versions were counted. Without version counting it
// equals Bytes and understates a versioned bucket.
func (b BucketStats) BillableBytes() int64 { return b.Bytes + b.VersionBytes }

// BucketStats measures a bucket by listing it. There is no cheaper way: the
// Object Storage control-plane API exposes no usage or statistics endpoint
// (verified against objectstorage SDK v1.9.1, INIT-SETUP.md 8.3), so the size is
// the sum over one listing pass — roughly one request per 1000 keys.
//
// With includeVersions the listing switches to the version listing, which
// returns every version and delete marker rather than only current objects. That
// is what a versioned bucket is actually billed for, and it is proportionally
// more expensive to walk.
//
// maxEntries caps how many listing entries are consumed; the pass then stops
// early and reports Truncated, so an unexpectedly huge bucket costs a bounded
// amount of time instead of an open-ended one. A value <= 0 means no cap.
func (s *S3Admin) BucketStats(ctx context.Context, bucket string, includeVersions bool, maxEntries int64) (BucketStats, error) {
	// Cancel the listing on early return (cap hit or error) so minio's producer
	// goroutine does not block on an unread channel.
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var st BucketStats
	var seen int64
	for obj := range s.mc.ListObjects(lctx, bucket, minio.ListObjectsOptions{
		Recursive:    true,
		WithVersions: includeVersions,
	}) {
		if obj.Err != nil {
			return BucketStats{}, fmt.Errorf("list objects in %q: %w", bucket, obj.Err)
		}
		switch {
		case obj.IsDeleteMarker:
			// No bytes of its own, but it is why the versions underneath survive.
			st.VersionObjects++
		case includeVersions && !obj.IsLatest:
			// IsLatest is only populated by the version listing; without it every
			// entry is a current object, which is why this arm is gated.
			st.VersionObjects++
			st.VersionBytes += obj.Size
		default:
			st.Objects++
			st.Bytes += obj.Size
		}
		seen++
		if maxEntries > 0 && seen >= maxEntries {
			st.Truncated = true
			break
		}
	}
	return st, nil
}

// BucketUsage returns the total size in bytes of all current objects in the
// bucket (one recursive listing pass). The clone feature measures the source
// bucket once before copying so the progress percentage has a stable
// denominator; S3Admin doubles as the client for arbitrary S3-compatible
// clone-source endpoints here.
func (s *S3Admin) BucketUsage(ctx context.Context, bucket string) (int64, error) {
	st, err := s.BucketStats(ctx, bucket, false, 0)
	if err != nil {
		return 0, err
	}
	return st.Bytes, nil
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
