package v1

import (
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConditionReady is the condition type signalling that the bucket, its
	// credentials group, access key and isolation policy are fully provisioned
	// and the workload credentials Secret is in place.
	ConditionReady = "Ready"

	// ReasonProvisioned indicates the bucket and its credentials are ready.
	ReasonProvisioned = "Provisioned"
	// ReasonProvisioning indicates provisioning is in progress.
	ReasonProvisioning = "Provisioning"
	// ReasonFailed indicates the last reconcile attempt failed.
	ReasonFailed = "Failed"
	// ReasonNotImplemented is set by the operator skeleton: the controller wiring
	// is in place but the StackIT provisioning flow is not yet implemented.
	ReasonNotImplemented = "NotImplemented"
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

// SecretReference points to the Kubernetes Secret that receives the bucket's S3
// access key and secret. The secret is created and kept in sync by the operator.
type SecretReference struct {
	// Name of the Secret to write the S3 credentials to.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the Secret. Defaults to the Bucket's own namespace when empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Keys overrides the data-key names written into the Secret. All fields are
	// optional and default to env-var-style names (see SecretKeys).
	// +optional
	Keys SecretKeys `json:"keys,omitempty"`
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

	// Region is the StackIT region the bucket is provisioned in.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default=eu01
	// +optional
	Region string `json:"region,omitempty"`

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

	// LastRotationTrigger is the rotate-credentials-at annotation value the
	// operator last acted upon. A differing (non-empty) annotation value requests
	// a new rotation; recording it here makes the trigger level-based and
	// GitOps-safe (the operator never mutates the annotation itself).
	// +optional
	LastRotationTrigger string `json:"lastRotationTrigger,omitempty"`

	// LastRotationTime is when the last credentials rotation completed.
	// +optional
	LastRotationTime *metav1.Time `json:"lastRotationTime,omitempty"`

	// OperatorVersion is the version of the operator that last reconciled this Bucket.
	// +optional
	OperatorVersion string `json:"operatorVersion,omitempty"`

	// Conditions represent the latest available observations of the bucket's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bkt
// +kubebuilder:printcolumn:name="Bucket",type="string",JSONPath=".spec.bucketName",description="Requested bucket name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Coarse reconcile lifecycle state"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Whether the bucket is fully provisioned"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.message",description="Current provisioning step or short failure reason"
// +kubebuilder:printcolumn:name="Region",type="string",JSONPath=".spec.region",description="StackIT region"
// +kubebuilder:printcolumn:name="Resolved",type="string",JSONPath=".status.resolvedBucketName",description="Physical bucket name in StackIT Object Storage",priority=1
// +kubebuilder:printcolumn:name="Secret",type="string",JSONPath=".spec.secretRef.name",description="Secret holding the workload credentials",priority=1
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

// SecretNamespace returns the namespace the credentials Secret should live in,
// defaulting to the Bucket's own namespace when spec.secretRef.namespace is empty.
func (b *Bucket) SecretNamespace() string {
	if b.Spec.SecretRef.Namespace != "" {
		return b.Spec.SecretRef.Namespace
	}
	return b.Namespace
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
