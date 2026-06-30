package v1

import (
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
const BucketFinalizer = "s3.gtrfc.com/finalizer"

// SecretReference points to the Kubernetes Secret that receives the bucket's S3
// access key and secret. The secret is created and kept in sync by the operator.
type SecretReference struct {
	// Name of the Secret to write the S3 credentials to.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the Secret. Defaults to the Bucket's own namespace when empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`
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
