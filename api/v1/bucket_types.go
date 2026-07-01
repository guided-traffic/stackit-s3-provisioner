package v1

import (
	"fmt"

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

// BucketFinalizer guards Bucket deletion so the operator can release the StackIT
// resources (access key, credentials group, bucket) before the CR is removed.
const BucketFinalizer = "stackit-bucket.gtrfc.com/finalizer"

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
}

// BucketStatus defines the observed state of Bucket.
type BucketStatus struct {
	// ObservedGeneration is the .metadata.generation the operator last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

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
// +kubebuilder:printcolumn:name="Bucket",type="string",JSONPath=".spec.bucketName",description="Bucket name in StackIT Object Storage"
// +kubebuilder:printcolumn:name="Region",type="string",JSONPath=".spec.region",description="StackIT region"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Whether the bucket is fully provisioned"
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
	return "eu01"
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
		keys.BucketNameKey():      []byte(b.Spec.BucketName),
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
