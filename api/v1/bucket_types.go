package v1

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConditionReady is the condition type signalling that the bucket, its
	// credentials group, access key and isolation policy are fully provisioned
	// and the workload credentials Secret is in place.
	ConditionReady = "Ready"

	// ConditionCloneCompleted tracks the spec.cloneFrom feature: it is True once
	// the source bucket's contents have been copied into this bucket. It is only
	// set on Buckets that request a clone.
	ConditionCloneCompleted = "CloneCompleted"

	// ConditionProviderReachable is set to False while an already-provisioned
	// Bucket keeps failing to reconcile for a reason that says nothing about the
	// Bucket itself — the StackIT or Kubernetes API being unreachable or
	// answering with a gateway error. The Ready condition deliberately stays
	// True during that window (see BucketStatus.DegradedSince), so this is the
	// condition that carries the degradation.
	//
	// It is absent on a healthy Bucket rather than present-and-True: a Bucket
	// that has never degraded and one that has recovered look the same, and
	// nothing is written to Buckets that were provisioned before this existed.
	ConditionProviderReachable = "ProviderReachable"

	// ReasonProvisioned indicates the bucket and its credentials are ready.
	ReasonProvisioned = "Provisioned"
	// ReasonProvisioning indicates provisioning is in progress.
	ReasonProvisioning = "Provisioning"
	// ReasonFailed indicates the last reconcile attempt failed.
	ReasonFailed = "Failed"
	// ReasonNotImplemented is set by the operator skeleton: the controller wiring
	// is in place but the StackIT provisioning flow is not yet implemented.
	ReasonNotImplemented = "NotImplemented"

	// ReasonCloning indicates the clone job is copying the source bucket.
	ReasonCloning = "Cloning"
	// ReasonCloned indicates the clone completed successfully.
	ReasonCloned = "Cloned"
	// ReasonCloneFailed indicates the last clone attempt failed (it is retried).
	ReasonCloneFailed = "CloneFailed"

	// ReasonProviderUnreachable indicates the last reconcile of an already
	// provisioned Bucket failed for a non-definitive reason and its Ready state
	// is being held. It is the reason on ConditionProviderReachable.
	ReasonProviderUnreachable = "ProviderUnreachable"
)

// BucketPhase is a coarse, human-readable summary of where a Bucket is in its
// reconcile lifecycle, surfaced as a printer column for at-a-glance status
// (e.g. in Lens). It complements the Ready condition: the condition carries the
// machine-readable truth, the phase is the friendly one-word state that pairs
// with status.message (the current provisioning step or a short failure reason).
type BucketPhase string

const (
	// PhasePending is the initial state before provisioning starts, and the state
	// of a bucket handled by the operator skeleton (no service-account key).
	PhasePending BucketPhase = "Pending"
	// PhaseProvisioning means the operator is actively creating or reconciling the
	// bucket, credentials and isolation policy.
	PhaseProvisioning BucketPhase = "Provisioning"
	// PhaseReady means the bucket, credentials and policy are fully provisioned.
	PhaseReady BucketPhase = "Ready"
	// PhaseFailed means the last reconcile (or teardown) attempt failed;
	// status.message carries the short reason.
	PhaseFailed BucketPhase = "Failed"
	// PhaseDeleting means the finalizer teardown is in progress.
	PhaseDeleting BucketPhase = "Deleting"
)

// BucketFinalizer guards Bucket deletion so the operator can release the StackIT
// resources (access key, credentials group, bucket) before the CR is removed.
const BucketFinalizer = "stackit-bucket.gtrfc.com/finalizer"

// DefaultRegion is the StackIT region used when spec.region is empty (mirrors
// the CRD default on the field).
const DefaultRegion = "eu01"

// RotateCredentialsAtAnnotation requests a hard rotation of the workload access
// key. Its value is an opaque trigger (by convention an RFC3339 timestamp, like
// kubectl.kubernetes.io/restartedAt): whenever it differs from
// status.lastRotationTrigger, the operator replaces the workload access key and
// re-writes the credentials Secret, then records the value in status. The old
// key is invalidated immediately, so consuming workloads must re-read the
// Secret (e.g. via pod restart). Removing the annotation never triggers
// anything; re-adding the last recorded value does not re-rotate.
const RotateCredentialsAtAnnotation = "stackit-bucket.gtrfc.com/rotate-credentials-at"

// ResolvedBucketNameAnnotation records the physical StackIT bucket name that was
// frozen for a Bucket CR at first provisioning. It is the crash- and
// restore-durable backup of status.resolvedBucketName: the operator writes it
// before creating the bucket and reads it back when status has been lost, so a
// later change to the operator's naming policy never re-maps an existing bucket.
const ResolvedBucketNameAnnotation = "stackit-bucket.gtrfc.com/resolved-bucket-name"

// Bucket-name constraints enforced by StackIT Object Storage (DNS-style, S3
// path-compatible). Mirrors the CRD validation on spec.bucketName, but is also
// applied to the *composed* physical name, which the CRD cannot validate because
// the prefix/namespace parts come from the operator's configuration.
const (
	minBucketNameLen = 3
	maxBucketNameLen = 63
)

// bucketNameRe matches a DNS-compliant, S3 path-style bucket name.
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)

// bucketNamePrefixRe matches a valid name prefix component: a lowercase DNS-1123
// label (no dots, no leading/trailing dash), so it composes cleanly with a '-'
// separator into a valid bucket name.
var bucketNamePrefixRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// BucketNaming is the operator-wide policy for composing the physical StackIT
// bucket name from a Bucket CR. It is configured once per operator deployment
// (Helm/flags), not per CR. The composed name is frozen per CR at first
// provisioning, so changing this policy only affects buckets created afterwards.
type BucketNaming struct {
	// Prefix is prepended to every composed bucket name (e.g. a cluster id).
	// Empty disables the prefix.
	Prefix string
	// IncludeNamespace appends the Bucket's namespace after the prefix.
	IncludeNamespace bool
}

// ComposeBucketName builds the physical bucket name for a Bucket CR under this
// naming policy: <prefix>-<namespace>-<spec.bucketName>, dropping any empty part
// and joining the rest with '-'. All inputs are already lowercase (prefix is
// validated, namespace is a DNS-1123 label, spec.bucketName is CRD-validated), so
// no case folding is required.
func (n BucketNaming) ComposeBucketName(b *Bucket) string {
	parts := make([]string, 0, 3)
	if n.Prefix != "" {
		parts = append(parts, n.Prefix)
	}
	if n.IncludeNamespace {
		parts = append(parts, b.Namespace)
	}
	parts = append(parts, b.Spec.BucketName)
	return strings.Join(parts, "-")
}

// Validate reports whether the naming policy's prefix is usable. An empty prefix
// is valid (it is simply omitted); a non-empty prefix must be a lowercase
// DNS-1123 label so composed names stay DNS-compliant.
func (n BucketNaming) Validate() error {
	if n.Prefix == "" {
		return nil
	}
	if len(n.Prefix) > maxBucketNameLen || !bucketNamePrefixRe.MatchString(n.Prefix) {
		return fmt.Errorf(
			"bucket name prefix %q is invalid: must be a lowercase DNS-1123 label "+
				"(letters, digits and '-'; no leading/trailing '-'; max %d chars)",
			n.Prefix, maxBucketNameLen)
	}
	return nil
}

// ValidateBucketName checks a composed physical bucket name against StackIT's
// length and DNS constraints. The reconciler calls it on freshly composed names
// and fails the CR (without a requeue hammer) when the prefix/namespace push the
// name out of range — a configuration fault a retry cannot fix.
func ValidateBucketName(name string) error {
	if len(name) < minBucketNameLen || len(name) > maxBucketNameLen {
		return fmt.Errorf("bucket name %q must be %d-%d characters long (got %d)",
			name, minBucketNameLen, maxBucketNameLen, len(name))
	}
	if !bucketNameRe.MatchString(name) {
		return fmt.Errorf("bucket name %q is not DNS-compliant "+
			"(allowed: lowercase letters, digits, '.', '-'; must start and end alphanumeric)", name)
	}
	return nil
}

// Default data-key names used inside the workload credentials Secret when the
// user does not override them via spec.secretRef.keys. They are uppercase
// env-var style so the Secret can be consumed directly via `envFrom` (the AWS_*
// names are also picked up automatically by AWS/minio SDKs).
const (
	// DefaultAccessKeyIDKey is the default key holding the S3 access key id.
	DefaultAccessKeyIDKey = "AWS_ACCESS_KEY_ID"
	// DefaultSecretAccessKeyKey is the default key holding the S3 secret.
	// This is the Secret data-key *name*, not a credential value.
	DefaultSecretAccessKeyKey = "AWS_SECRET_ACCESS_KEY" // #nosec G101 -- data-key name, not a secret
	// DefaultBucketNameKey is the default key holding the bucket name.
	DefaultBucketNameKey = "S3_BUCKET"
	// DefaultRegionKey is the default key holding the StackIT region.
	DefaultRegionKey = "S3_REGION"
	// DefaultEndpointKey is the default key holding the S3 endpoint host.
	DefaultEndpointKey = "S3_ENDPOINT"
	// DefaultBucketURLKey is the default key holding the full path-style bucket URL.
	DefaultBucketURLKey = "S3_BUCKET_URL"
)

// SecretKeys overrides the data-key names used inside the workload credentials
// Secret. Every field is optional; an empty value falls back to the documented
// default (see the Default*Key constants). This lets a workload consume the
// Secret with whatever key/env-var names it expects.
type SecretKeys struct {
	// AccessKeyID overrides the key holding the S3 access key id.
	// Defaults to AWS_ACCESS_KEY_ID.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	AccessKeyID string `json:"accessKeyID,omitempty"`

	// SecretAccessKey overrides the key holding the S3 secret.
	// Defaults to AWS_SECRET_ACCESS_KEY.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	SecretAccessKey string `json:"secretAccessKey,omitempty"`

	// BucketName overrides the key holding the bucket name.
	// Defaults to S3_BUCKET.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// Region overrides the key holding the StackIT region.
	// Defaults to S3_REGION.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	Region string `json:"region,omitempty"`

	// Endpoint overrides the key holding the S3 endpoint host (no scheme).
	// Defaults to S3_ENDPOINT.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// BucketURL overrides the key holding the full path-style bucket URL.
	// Defaults to S3_BUCKET_URL.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	BucketURL string `json:"bucketURL,omitempty"`
}

// AccessKeyIDKey returns the effective data key for the access key id.
func (k SecretKeys) AccessKeyIDKey() string { return orDefault(k.AccessKeyID, DefaultAccessKeyIDKey) }

// SecretAccessKeyKey returns the effective data key for the secret access key.
func (k SecretKeys) SecretAccessKeyKey() string {
	return orDefault(k.SecretAccessKey, DefaultSecretAccessKeyKey)
}

// BucketNameKey returns the effective data key for the bucket name.
func (k SecretKeys) BucketNameKey() string { return orDefault(k.BucketName, DefaultBucketNameKey) }

// RegionKey returns the effective data key for the region.
func (k SecretKeys) RegionKey() string { return orDefault(k.Region, DefaultRegionKey) }

// EndpointKey returns the effective data key for the endpoint host.
func (k SecretKeys) EndpointKey() string { return orDefault(k.Endpoint, DefaultEndpointKey) }

// BucketURLKey returns the effective data key for the full bucket URL.
func (k SecretKeys) BucketURLKey() string { return orDefault(k.BucketURL, DefaultBucketURLKey) }

// orDefault returns v when non-empty, else def.
func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// CloneSourceSecretKeys overrides the data-key names the operator reads the
// clone-source credentials from. Empty fields fall back to the same env-var
// style defaults as the workload Secret (AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY), so a Secret written by this operator for another
// Bucket works as a clone source without any key configuration.
type CloneSourceSecretKeys struct {
	// AccessKeyID overrides the key holding the source S3 access key id.
	// Defaults to AWS_ACCESS_KEY_ID.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	AccessKeyID string `json:"accessKeyID,omitempty"`

	// SecretAccessKey overrides the key holding the source S3 secret.
	// Defaults to AWS_SECRET_ACCESS_KEY.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +optional
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
}

// AccessKeyIDKey returns the effective data key for the source access key id.
func (k CloneSourceSecretKeys) AccessKeyIDKey() string {
	return orDefault(k.AccessKeyID, DefaultAccessKeyIDKey)
}

// SecretAccessKeyKey returns the effective data key for the source secret.
func (k CloneSourceSecretKeys) SecretAccessKeyKey() string {
	return orDefault(k.SecretAccessKey, DefaultSecretAccessKeyKey)
}

// CloneSourceSecretRef points to the Secret holding the credentials for the
// clone source. It must live in the Bucket's own namespace — a namespace field
// is deliberately not offered, because it would let a CR author read arbitrary
// Secrets from foreign namespaces through the operator's privileges.
type CloneSourceSecretRef struct {
	// Name of the Secret (in the Bucket's namespace) holding the source
	// credentials.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Keys overrides the data-key names the credentials are read from.
	// +optional
	Keys CloneSourceSecretKeys `json:"keys,omitempty"`
}

// CloneFrom requests that the contents of an existing S3 bucket (any
// S3-compatible endpoint) are copied into this bucket once, right after it is
// provisioned. The copy is performed by an rclone Job and is a one-shot
// operation: after status.clone.phase reaches Completed, later changes to this
// field have no effect.
type CloneFrom struct {
	// Endpoint is the S3 endpoint of the source bucket, as a bare host
	// (TLS assumed, e.g. object.storage.eu01.onstackit.cloud) or a
	// scheme-qualified URL.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Bucket is the name of the source bucket at the endpoint.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Region is the region of the source endpoint, when it requires one for
	// SigV4 signing (e.g. eu01 for StackIT, eu-central-1 for AWS).
	// +optional
	Region string `json:"region,omitempty"`

	// AddressingStyle selects how the source bucket is addressed: "path"
	// (endpoint.host/bucket — the S3-compatible default, works for StackIT,
	// MinIO, Ceph, …) or "virtual-hosted" (bucket.endpoint.host — AWS's
	// preferred style). Defaults to path.
	// +kubebuilder:validation:Enum=path;virtual-hosted
	// +kubebuilder:default=path
	// +optional
	AddressingStyle string `json:"addressingStyle,omitempty"`

	// SecretRef selects the Secret (in the Bucket's namespace) holding the
	// credentials that can read the source bucket.
	SecretRef CloneSourceSecretRef `json:"secretRef"`

	// HoldSecretUntilCloned delays writing the workload credentials Secret until
	// the clone completed successfully, so consuming workloads never start
	// against a partially copied bucket. Disable to publish the credentials
	// immediately; the Ready condition still waits for the clone either way.
	// +kubebuilder:default=true
	// +optional
	HoldSecretUntilCloned *bool `json:"holdSecretUntilCloned,omitempty"`
}

// Values of CloneFrom.AddressingStyle.
const (
	// CloneAddressingPath addresses the source as endpoint.host/bucket.
	CloneAddressingPath = "path"
	// CloneAddressingVirtualHosted addresses the source as bucket.endpoint.host.
	CloneAddressingVirtualHosted = "virtual-hosted"
)

// VirtualHosted reports whether the source bucket must be addressed
// virtual-hosted style. An empty AddressingStyle means path style (the CRD
// default; offline clients see the zero value).
func (c *CloneFrom) VirtualHosted() bool {
	return c.AddressingStyle == CloneAddressingVirtualHosted
}

// HoldSecret reports whether the workload Secret must be withheld until the
// clone completed. Defaults to true; a nil receiver (no clone requested) means
// the Secret is never held back.
func (c *CloneFrom) HoldSecret() bool {
	if c == nil {
		return false
	}
	return c.HoldSecretUntilCloned == nil || *c.HoldSecretUntilCloned
}

// EndpointURL returns the source endpoint as a scheme-qualified URL, assuming
// https for a bare host (mirrors the production S3 endpoint convention).
func (c *CloneFrom) EndpointURL() string {
	if strings.HasPrefix(c.Endpoint, "http://") || strings.HasPrefix(c.Endpoint, "https://") {
		return c.Endpoint
	}
	return "https://" + c.Endpoint
}

// EndpointHost returns the source endpoint host without a scheme.
func (c *CloneFrom) EndpointHost() string {
	host := strings.TrimPrefix(c.Endpoint, "https://")
	return strings.TrimPrefix(host, "http://")
}

// ClonePhase is the coarse lifecycle state of the one-shot clone operation.
type ClonePhase string

const (
	// ClonePhaseRunning means the clone job is copying (or about to).
	ClonePhaseRunning ClonePhase = "Running"
	// ClonePhaseCompleted means the source bucket was copied successfully.
	// This state is terminal: the clone never runs again for this Bucket.
	ClonePhaseCompleted ClonePhase = "Completed"
	// ClonePhaseFailed means the last clone attempt failed; it is retried with
	// backoff (rclone resumes, already-copied objects are skipped).
	ClonePhaseFailed ClonePhase = "Failed"
)

// CloneStatus is the observed state of the spec.cloneFrom operation.
type CloneStatus struct {
	// Phase is the coarse clone lifecycle state.
	// +kubebuilder:validation:Enum=Running;Completed;Failed
	// +optional
	Phase ClonePhase `json:"phase,omitempty"`

	// StartedAt is when the first clone job was created.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the clone finished successfully.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// TotalBytes is the total size of the source bucket, measured once before
	// the copy starts so the progress percentage has a stable denominator.
	// +optional
	TotalBytes int64 `json:"totalBytes,omitempty"`

	// BytesCopied is the number of bytes transferred so far (from rclone's
	// remote-control stats, refreshed while the clone runs).
	// +optional
	BytesCopied int64 `json:"bytesCopied,omitempty"`

	// Progress is a human-readable transfer summary, e.g. "2.0 GiB / 18.0 GiB (11%)".
	// +optional
	Progress string `json:"progress,omitempty"`

	// Rate is the current transfer rate, e.g. "42.0 MiB/s".
	// +optional
	Rate string `json:"rate,omitempty"`

	// ETA is rclone's estimated time to completion, e.g. "6m30s".
	// +optional
	ETA string `json:"eta,omitempty"`

	// Message carries a short failure reason while Phase is Failed.
	// +optional
	Message string `json:"message,omitempty"`
}

// SecretReference points to the Kubernetes Secret that receives the bucket's S3
// access key and secret. The secret is created and kept in sync by the operator.
//
// The Secret always lives in the Bucket's own namespace; there is deliberately
// no way to direct it elsewhere. A cross-namespace target would let anyone who
// may create a Bucket write into, and on deletion remove, a Secret in a
// namespace they otherwise cannot touch (ADR 0001,
// docs/adr/0001-a-bucket-only-affects-its-own-namespace.md).
type SecretReference struct {
	// Name of the Secret to write the S3 credentials to. The Secret is created
	// in the Bucket's namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Keys overrides the data-key names written into the Secret. All fields are
	// optional and default to env-var-style names (see SecretKeys).
	// +optional
	Keys SecretKeys `json:"keys,omitempty"`
}

// LocalBucketReference names another Bucket CR in the same namespace as the
// referencing Bucket. Cross-namespace references are deliberately not
// expressible: the namespace is this operator's trust boundary, and a reference
// resolved by CR name (rather than by physical bucket name or by a raw
// credentials-group URN) cannot be pointed at a resource outside it.
type LocalBucketReference struct {
	// Name is the metadata.name of a Bucket CR in this Bucket's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// UsageSpec configures the periodic bucket-size measurement for this Bucket.
// Every field is optional and falls back to the operator-wide default
// (Helm bucketUsage.*), so a Bucket that omits the block entirely follows the
// cluster policy.
//
// Measuring costs no money at STACKIT — Object Storage is billed per started
// gigabyte per started hour and the price list carries no request, operation or
// traffic component (see INIT-SETUP.md 8.3) — but it costs TIME: the size can
// only be obtained by listing the bucket, which is one request per 1000 object
// keys. A bucket with millions of objects therefore takes minutes per pass,
// which is what the operator-wide interval floor and object cap bound.
type UsageSpec struct {
	// Enabled turns the size measurement on or off for this Bucket, overriding
	// the operator-wide default (Helm bucketUsage.defaultEnabled). Leaving it
	// unset inherits that default, so a cluster-wide policy change reaches this
	// Bucket without editing it.
	//
	// It cannot switch the feature on when the operator-wide gate
	// (Helm bucketUsage.enabled) is off: that gate is the hard kill switch, and
	// a Bucket asking for a measurement under it is skipped with a warning event.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Interval overrides how often this bucket is measured. It is a Go duration
	// string with a unit (e.g. "30m", "6h") and is clamped up to the operator's
	// floor (Helm bucketUsage.minInterval) — a Bucket cannot ask to be measured
	// more often than the cluster allows. Empty inherits the operator default.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +optional
	Interval string `json:"interval,omitempty"`

	// IncludeVersions additionally measures non-current object versions and
	// delete markers, which occupy billed storage but are invisible in a plain
	// object listing. It makes the measurement match the invoice on a versioned
	// bucket, at the price of listing every version rather than every current
	// object. Unset inherits the operator default
	// (Helm bucketUsage.includeVersions).
	// +optional
	IncludeVersions *bool `json:"includeVersions,omitempty"`
}

// UsageEnabled reports whether size measurement is requested for this Bucket,
// given the operator-wide default for Buckets that do not decide themselves.
func (u *UsageSpec) UsageEnabled(defaultEnabled bool) bool {
	if u == nil || u.Enabled == nil {
		return defaultEnabled
	}
	return *u.Enabled
}

// UsageIncludeVersions reports whether non-current versions count towards this
// Bucket's size, given the operator-wide default.
func (u *UsageSpec) UsageIncludeVersions(defaultInclude bool) bool {
	if u == nil || u.IncludeVersions == nil {
		return defaultInclude
	}
	return *u.IncludeVersions
}

// UsageInterval returns the measurement interval requested by this Bucket, or 0
// when it does not request one (the caller then applies the operator default).
// A syntactically invalid value is reported as an error rather than silently
// ignored, so a typo surfaces on the CR instead of quietly restoring the default.
func (u *UsageSpec) UsageInterval() (time.Duration, error) {
	if u == nil || u.Interval == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(u.Interval)
	if err != nil {
		return 0, fmt.Errorf("spec.usage.interval %q is not a Go duration (e.g. \"30m\", \"6h\"): %w", u.Interval, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("spec.usage.interval %q must be positive", u.Interval)
	}
	return d, nil
}

// BucketSpec defines the desired state of a StackIT Object Storage bucket and its
// dedicated, isolated workload credentials (one CR = one isolated workload, see
// INIT-SETUP.md §8).
type BucketSpec struct {
	// BucketName is the DNS-compliant name of the bucket in StackIT Object Storage.
	// It is immutable: changing it after creation is rejected.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bucketName is immutable"
	BucketName string `json:"bucketName"`

	// SecretRef selects the Secret that receives the provisioned S3 access key and
	// secret. The operator writes accessKeyID and secretAccessKey into this Secret.
	SecretRef SecretReference `json:"secretRef"`

	// GrantReadAccess lists other Bucket CRs in THIS Bucket's namespace whose
	// workload credentials additionally receive read-only access to this bucket.
	// It is set on the bucket that owns the data (the grantor), so a bucket's
	// full access list is visible in its own spec rather than scattered across
	// the namespace.
	//
	// Granted readers may list the bucket and get objects and their tags; every
	// write, delete and bucket-management action stays denied, as does access
	// from any other principal. Entries name a Bucket CR and are resolved through
	// the Kubernetes API in this Bucket's namespace: a Bucket in another
	// namespace cannot be named here, and a same-named Bucket elsewhere in the
	// cluster resolves to a different credentials group.
	//
	// The principal that ends up in the bucket policy is that Bucket's
	// credentials group, resolved through that Bucket's physical bucket: the
	// bucket must carry the referenced Bucket's ownership tags, and the group is
	// the one named by the bucket's own credentials-group tag (or, for buckets
	// provisioned before that tag existed, by its isolation policy). Nothing
	// writable by a namespace user — status, annotations, Secrets, the group's
	// display name — takes part in that resolution.
	//
	// A referenced Bucket that does not exist yet, or whose credentials group is
	// not provisioned yet, is skipped with a warning event and picked up
	// automatically once it appears; it never blocks this bucket from becoming
	// Ready. Deleting a referenced Bucket revokes its access on the next
	// reconcile. Referencing this Bucket itself is rejected and, should it slip
	// through, ignored when the policy is built.
	//
	// Leaving the list empty preserves the previous behavior exactly: the
	// bucket's policy is then byte-identical to a bucket that never used the
	// feature.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	GrantReadAccess []LocalBucketReference `json:"grantReadAccess,omitempty"`

	// Region is the StackIT region the bucket is provisioned in.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default=eu01
	// +optional
	Region string `json:"region,omitempty"`

	// CloneFrom requests a one-shot copy of an existing S3 bucket's contents
	// into this bucket after provisioning. See CloneFrom for details; by default
	// the workload credentials Secret is only written once the copy succeeded.
	// +optional
	CloneFrom *CloneFrom `json:"cloneFrom,omitempty"`

	// WipeOnDelete requests that the operator deletes ALL objects (including
	// object versions and delete markers) from the bucket before removing it
	// when this CR is deleted. Without it, deletion of a non-empty bucket is
	// blocked (data-loss guard). The field is mutable, so it can be set right
	// before deleting the CR.
	//
	// It is only honored when the operator is deployed with the wipe feature
	// enabled (Helm value wipeOnDelete.enabled / --enable-wipe-on-delete);
	// otherwise it degrades to the safe empty-only behavior and a warning
	// event is emitted.
	// +optional
	WipeOnDelete bool `json:"wipeOnDelete,omitempty"`

	// Usage configures the periodic bucket-size measurement and the monthly cost
	// estimate derived from it. Omitting the block follows the operator-wide
	// policy (Helm bucketUsage.*); see UsageSpec for the individual overrides.
	// +optional
	Usage *UsageSpec `json:"usage,omitempty"`
}

// UsageStatus is the observed size of a bucket and the cost estimate derived
// from it. Every value is a MEASUREMENT taken at LastMeasurementTime, not a live
// figure: it is as old as the configured interval allows.
type UsageStatus struct {
	// Bytes is the total size of the bucket's current objects.
	// +optional
	Bytes int64 `json:"bytes,omitempty"`

	// Objects is the number of current objects.
	// +optional
	Objects int64 `json:"objects,omitempty"`

	// VersionBytes is the total size of non-current object versions. It is only
	// measured when version counting is enabled (spec.usage.includeVersions);
	// otherwise it stays zero and BillableBytes equals Bytes.
	// +optional
	VersionBytes int64 `json:"versionBytes,omitempty"`

	// VersionObjects is the number of non-current object versions and delete
	// markers. Delete markers carry no bytes but are counted here, because they
	// are what makes a "deleted" object still occupy a version.
	// +optional
	VersionObjects int64 `json:"versionObjects,omitempty"`

	// BillableBytes is Bytes plus VersionBytes: the figure the cost estimate is
	// computed from. Without version counting it equals Bytes, which UNDERSTATES
	// a versioned bucket's invoice.
	// +optional
	BillableBytes int64 `json:"billableBytes,omitempty"`

	// HumanReadable renders BillableBytes for display (e.g. "18.0 GiB"). A
	// measurement that hit the operator's object cap is prefixed with ">=",
	// because the real bucket is larger than what was counted.
	// +optional
	HumanReadable string `json:"humanReadable,omitempty"`

	// EstimatedMonthlyCost is the ESTIMATED monthly storage cost for this bucket
	// at the price the operator is configured with (Helm bucketUsage.pricing),
	// rendered for display (e.g. "1.23 EUR"). Empty when no price is configured.
	//
	// It is an estimate, not an invoice: it prices the measured size as if it
	// were held for a whole 30-day month, uses the operator's configured list
	// price rather than the contract actually in force, and excludes taxes and
	// anything the bucket is not billed for by size.
	// +optional
	EstimatedMonthlyCost string `json:"estimatedMonthlyCost,omitempty"`

	// EstimatedMonthlyCostCents is the same estimate in minor currency units
	// (cents), which is the canonical value: the estimate is always rounded to
	// whole cents, and EstimatedMonthlyCost is its rendering.
	// +optional
	EstimatedMonthlyCostCents int64 `json:"estimatedMonthlyCostCents,omitempty"`

	// Currency is the currency of the estimate (Helm bucketUsage.pricing.currency).
	// +optional
	Currency string `json:"currency,omitempty"`

	// LastMeasurementTime is when the values above were measured.
	// +optional
	LastMeasurementTime *metav1.Time `json:"lastMeasurementTime,omitempty"`

	// MeasurementDuration is how long the last measurement took (e.g. "4.2s").
	// A listing pass costs one request per 1000 keys, so this is the honest
	// price of the configured interval and the number to look at before lowering it.
	// +optional
	MeasurementDuration string `json:"measurementDuration,omitempty"`

	// Truncated reports that the measurement stopped at the operator's object cap
	// (Helm bucketUsage.maxObjects). Every size and cost value is then a LOWER
	// BOUND, not the bucket's actual size.
	// +optional
	Truncated bool `json:"truncated,omitempty"`

	// Message carries a short reason when the last measurement did not succeed,
	// or a note about the effective configuration (e.g. a clamped interval). A
	// failed measurement leaves the previous values in place and never affects
	// the Bucket's Ready condition.
	// +optional
	Message string `json:"message,omitempty"`
}

// BucketStatus defines the observed state of Bucket.
type BucketStatus struct {
	// Phase is a coarse, human-readable lifecycle summary (Pending, Provisioning,
	// Ready, Failed, Deleting) for at-a-glance display in tools like Lens. The
	// authoritative, machine-readable state stays in Conditions.
	// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;Deleting
	// +optional
	Phase BucketPhase `json:"phase,omitempty"`

	// Message is a short, human-readable description of the current reconcile
	// state: the provisioning step in progress, or a concise reason the last
	// attempt failed.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the .metadata.generation the operator last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedBucketName is the physical StackIT bucket name the operator froze for
	// this CR at first provisioning (spec.bucketName composed with the operator's
	// naming policy). Once set it is authoritative and never recomputed, so a later
	// change to the operator's prefix/namespace policy leaves this bucket untouched.
	// +optional
	ResolvedBucketName string `json:"resolvedBucketName,omitempty"`

	// BucketURL is the path-style S3 endpoint URL of the provisioned bucket.
	// +optional
	BucketURL string `json:"bucketURL,omitempty"`

	// CredentialsGroupID is the StackIT credentials-group id backing this bucket's
	// workload access key.
	// +optional
	CredentialsGroupID string `json:"credentialsGroupID,omitempty"`

	// CredentialsGroupURN is the credentials-group URN used as the bucket-policy
	// principal for workload isolation (INIT-SETUP.md §4.1).
	// +optional
	CredentialsGroupURN string `json:"credentialsGroupURN,omitempty"`

	// AccessKeyID is the S3 access key id provisioned for the workload. The matching
	// secret is only ever stored in the referenced Secret, never in status.
	// +optional
	AccessKeyID string `json:"accessKeyID,omitempty"`

	// GrantedReadTo lists the spec.grantReadAccess entries that are currently
	// reflected in the bucket policy, i.e. the referenced Buckets whose
	// credentials group was found and granted read-only access. Entries that
	// could not be resolved are absent, which makes a pending or revoked grant
	// visible without reading the policy from S3.
	// +optional
	// +listType=atomic
	GrantedReadTo []string `json:"grantedReadTo,omitempty"`

	// Clone is the observed state of the spec.cloneFrom operation. It is only
	// set on Buckets that request a clone; once Phase is Completed it is
	// terminal and the clone never runs again.
	// +optional
	Clone *CloneStatus `json:"clone,omitempty"`

	// LastRotationTrigger is the rotate-credentials-at annotation value the
	// operator last acted upon. A differing (non-empty) annotation value requests
	// a new rotation; recording it here makes the trigger level-based and
	// GitOps-safe (the operator never mutates the annotation itself).
	// +optional
	LastRotationTrigger string `json:"lastRotationTrigger,omitempty"`

	// LastRotationTime is when the last credentials rotation completed.
	// +optional
	LastRotationTime *metav1.Time `json:"lastRotationTime,omitempty"`

	// DegradedSince is when the operator first failed to reconcile this already
	// provisioned Bucket for a reason that carries no information about the
	// Bucket itself — an unreachable provider, a gateway error page, a
	// Kubernetes API blip. While it is set, Ready is deliberately held at its
	// last verified value and ConditionProviderReachable is False; the Bucket
	// only drops to Failed once the degradation outlives the operator's grace
	// window (--provider-degraded-grace). It is cleared on the next successful
	// reconcile.
	//
	// A reconcile can therefore report Ready while the provider is unreachable.
	// That is the intended trade: Ready describes the last verified state of the
	// bucket, not the outcome of the last attempt to verify it, so a provider
	// blip no longer marks every Bucket in the cluster unhealthy at once.
	// +optional
	DegradedSince *metav1.Time `json:"degradedSince,omitempty"`

	// OperatorVersion is the version of the operator that last reconciled this Bucket.
	// +optional
	OperatorVersion string `json:"operatorVersion,omitempty"`

	// Usage is the result of the last bucket-size measurement and the monthly
	// cost estimate derived from it. It is absent while measurement is disabled
	// for this Bucket and is removed again when it is switched off, so a stale
	// size never lingers on the CR.
	// +optional
	Usage *UsageStatus `json:"usage,omitempty"`

	// Conditions represent the latest available observations of the bucket's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bkt
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.grantReadAccess) || self.spec.grantReadAccess.all(g, g.name != self.metadata.name)",message="spec.grantReadAccess must not reference the Bucket itself"
// +kubebuilder:printcolumn:name="Bucket",type="string",JSONPath=".spec.bucketName",description="Requested bucket name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Coarse reconcile lifecycle state"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Whether the bucket is fully provisioned"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.message",description="Current provisioning step or short failure reason"
// +kubebuilder:printcolumn:name="Region",type="string",JSONPath=".spec.region",description="StackIT region"
// +kubebuilder:printcolumn:name="Size",type="string",JSONPath=".status.usage.humanReadable",description="Measured bucket size at the last measurement"
// +kubebuilder:printcolumn:name="Cost/Month",type="string",JSONPath=".status.usage.estimatedMonthlyCost",description="Estimated monthly storage cost at the operator's configured price"
// +kubebuilder:printcolumn:name="Degraded",type="string",JSONPath=".status.degradedSince",description="Since when reconciles keep failing while Ready is held",priority=1
// +kubebuilder:printcolumn:name="Clone",type="string",JSONPath=".status.clone.progress",description="Clone transfer progress",priority=1
// +kubebuilder:printcolumn:name="Resolved",type="string",JSONPath=".status.resolvedBucketName",description="Physical bucket name in StackIT Object Storage",priority=1
// +kubebuilder:printcolumn:name="Secret",type="string",JSONPath=".spec.secretRef.name",description="Secret holding the workload credentials",priority=1
// +kubebuilder:printcolumn:name="Objects",type="integer",JSONPath=".status.usage.objects",description="Number of current objects at the last measurement",priority=1
// +kubebuilder:printcolumn:name="Measured",type="date",JSONPath=".status.usage.lastMeasurementTime",description="When the bucket size was last measured",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Bucket is the Schema for the buckets API. One Bucket maps to a StackIT bucket,
// a dedicated credentials group, an access key and an isolation policy.
type Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BucketSpec   `json:"spec,omitempty"`
	Status BucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BucketList contains a list of Bucket.
type BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Bucket `json:"items"`
}

// GetRegion returns the configured region, defaulting to eu01 when unset.
func (b *Bucket) GetRegion() string {
	if b.Spec.Region != "" {
		return b.Spec.Region
	}
	return DefaultRegion
}

// EffectiveBucketName returns the physical StackIT bucket name for this CR: the
// frozen status value if present, else the annotation backup (used when status
// was lost), else the raw spec.bucketName. It is the single accessor every
// consumer (Secret contents, teardown) must use so a name, once frozen, stays
// stable regardless of the operator's current naming policy.
func (b *Bucket) EffectiveBucketName() string {
	if b.Status.ResolvedBucketName != "" {
		return b.Status.ResolvedBucketName
	}
	if v := b.Annotations[ResolvedBucketNameAnnotation]; v != "" {
		return v
	}
	return b.Spec.BucketName
}

// CloneCompleted reports whether the requested clone has finished successfully
// (terminal: the clone never runs again for this Bucket).
func (b *Bucket) CloneCompleted() bool {
	return b.Status.Clone != nil && b.Status.Clone.Phase == ClonePhaseCompleted
}

// ClonePending reports whether a requested clone still has to run.
func (b *Bucket) ClonePending() bool {
	return b.Spec.CloneFrom != nil && !b.CloneCompleted()
}

// PendingRotationTrigger returns the rotate-credentials-at annotation value
// when it requests a rotation that has not been performed yet, and "" when no
// rotation is pending (annotation absent/empty, or already recorded in status).
func (b *Bucket) PendingRotationTrigger() string {
	v := b.Annotations[RotateCredentialsAtAnnotation]
	if v == "" || v == b.Status.LastRotationTrigger {
		return ""
	}
	return v
}

// SecretValues carries the provisioned values that only the operator knows at
// reconcile time. The bucket name and region are taken from the Bucket spec, so
// they are not part of this struct.
type SecretValues struct {
	// AccessKeyID is the provisioned S3 access key id.
	AccessKeyID string
	// SecretAccessKey is the provisioned S3 secret (only available once, at create).
	SecretAccessKey string
	// Endpoint is the S3 endpoint host (no scheme), e.g. object.storage.eu01.onstackit.cloud.
	Endpoint string
	// BucketURL is the full path-style bucket URL incl. scheme and bucket.
	BucketURL string
}

// SecretData builds the data map for the workload credentials Secret, honoring
// the configured (or default) key-name overrides. The credentials, bucket name
// and region are always written; the optional connection fields (endpoint,
// bucket URL) are written only when a non-empty value is supplied.
func (b *Bucket) SecretData(v SecretValues) map[string][]byte {
	keys := b.Spec.SecretRef.Keys
	data := map[string][]byte{
		keys.AccessKeyIDKey():     []byte(v.AccessKeyID),
		keys.SecretAccessKeyKey(): []byte(v.SecretAccessKey),
		keys.BucketNameKey():      []byte(b.EffectiveBucketName()),
		keys.RegionKey():          []byte(b.GetRegion()),
	}
	if v.Endpoint != "" {
		data[keys.EndpointKey()] = []byte(v.Endpoint)
	}
	if v.BucketURL != "" {
		data[keys.BucketURLKey()] = []byte(v.BucketURL)
	}
	return data
}

// ValidateSecretKeys reports an error if two logical fields resolve to the same
// Secret data key, which would silently overwrite one value. All six logical
// fields are considered, independent of whether the optional connection values
// are populated at reconcile time.
func (b *Bucket) ValidateSecretKeys() error {
	keys := b.Spec.SecretRef.Keys
	seen := make(map[string]string, 6)
	for _, kv := range []struct{ field, key string }{
		{"accessKeyID", keys.AccessKeyIDKey()},
		{"secretAccessKey", keys.SecretAccessKeyKey()},
		{"bucketName", keys.BucketNameKey()},
		{"region", keys.RegionKey()},
		{"endpoint", keys.EndpointKey()},
		{"bucketURL", keys.BucketURLKey()},
	} {
		if other, ok := seen[kv.key]; ok {
			return fmt.Errorf("secretRef.keys: %q and %q both map to data key %q", other, kv.field, kv.key)
		}
		seen[kv.key] = kv.field
	}
	return nil
}
