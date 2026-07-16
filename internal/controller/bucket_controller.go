// Package controller contains the Bucket reconciler for the StackIT S3 provisioner.
package controller

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "stackit-s3-provisioner"

	// bucketVisibleTimeout bounds the wait for a freshly created bucket to appear
	// in the project listing (bucket creation is eventually consistent).
	bucketVisibleTimeout = 60 * time.Second
)

// adminGroupName is the display name of the operator-wide bootstrap credentials
// group whose access key sets bucket policies (INIT-SETUP.md §4.1). It is shared
// across all Bucket CRs in the project and is never torn down per-bucket.
const adminGroupName = "operator-admin"

// Data-key names inside the operator-owned admin credentials Secret. These name
// the fields of the bootstrap S3 admin credential, not any workload secret.
const (
	adminSecretKeyAccessKeyID     = "accessKeyID"
	adminSecretKeySecretAccessKey = "secretAccessKey" // #nosec G101 -- data-key name, not a secret
	adminSecretKeyURN             = "urn"
	adminSecretKeyGroupID         = "credentialsGroupID"
)

// adminCreds is the bootstrap S3 admin credential used to manage bucket policies
// and to inspect bucket contents for the empty-only delete guard.
type adminCreds struct {
	accessKeyID     string
	secretAccessKey string
	urn             string // admin credentials-group URN, kept in every policy's NotPrincipal
	groupID         string
}

// BucketReconciler reconciles a Bucket object against StackIT Object Storage.
//
// One Bucket CR maps to a StackIT bucket, a dedicated credentials group, an
// access key, an isolation policy (INIT-SETUP.md §4.1) and a workload
// credentials Secret. The reconciler is idempotent and self-healing: cloud
// resources are looked up by deterministic name (so a crash never leaks a
// duplicate), the workload Secret is the source of truth for the live
// credential, and the bucket policy is re-applied on drift.
type BucketReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Stackit is the StackIT Object Storage client bound to this operator's project.
	// It is nil when the operator runs without a service-account key (skeleton mode);
	// in that case the reconciler keeps the CR in a NotImplemented state instead of
	// touching the cloud.
	Stackit *stackit.Client

	// OperatorVersion is stamped into Bucket status for observability.
	OperatorVersion string

	// Naming is the operator-wide policy for composing the physical bucket name
	// from a Bucket CR. The composed name is frozen per CR at first provisioning,
	// so changing this policy only affects buckets created afterwards.
	Naming s3v1.BucketNaming

	// AdminSecretName / AdminSecretNamespace locate the operator-owned Secret that
	// persists the bootstrap S3 admin credentials. The namespace is the operator's
	// own namespace (POD_NAMESPACE).
	AdminSecretName      string
	AdminSecretNamespace string

	// OwnershipName is the value written into every provisioned bucket's
	// "managed-by" tag and required to match before the operator adopts or deletes
	// a pre-existing bucket. It is the operator/fleet identity (configurable via
	// Helm), NOT a per-CR identity. Empty falls back to defaultOwnershipName.
	//
	// Because it is part of the bucket ownership key, changing it after buckets
	// exist makes the operator treat its own buckets as foreign (collision).
	OwnershipName string

	// EnableWipeOnDelete is the operator-wide feature gate for spec.wipeOnDelete
	// (Helm value wipeOnDelete.enabled). When false, a CR requesting a wipe
	// degrades to the safe empty-only delete guard and a warning event is
	// emitted instead of destroying data.
	EnableWipeOnDelete bool

	adminMu sync.Mutex
	admin   *adminCreds // cached after the first successful bootstrap
}

// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives a Bucket towards its desired state.
func (r *BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var bucket s3v1.Bucket
	if err := r.Get(ctx, req.NamespacedName, &bucket); err != nil {
		// Ignore not-found: the object was deleted after the reconcile was queued.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: release StackIT resources, then drop the finalizer.
	if !bucket.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&bucket, s3v1.BucketFinalizer) {
			return ctrl.Result{}, nil
		}
		if r.Stackit != nil {
			// Surface that teardown is in progress (visible while a blocked delete —
			// e.g. a non-empty bucket — keeps the finalizer). Skip re-writing once
			// already Deleting or once a Failed teardown reason is recorded, so a
			// blocked delete does not flip-flop Deleting<->Failed and self-trigger
			// reconciles via the status watch.
			if bucket.Status.Phase != s3v1.PhaseDeleting && bucket.Status.Phase != s3v1.PhaseFailed {
				bucket.Status.Phase = s3v1.PhaseDeleting
				bucket.Status.Message = "releasing StackIT resources"
				if err := r.Status().Update(ctx, &bucket); err != nil {
					return ctrl.Result{}, client.IgnoreNotFound(err)
				}
			}
			if err := r.teardown(ctx, &bucket); err != nil {
				logger.Error(err, "teardown failed; keeping finalizer", "bucket", bucket.EffectiveBucketName())
				// Keep the finalizer and surface the reason; a non-empty bucket
				// must not be deleted (data-loss guard, INIT-SETUP.md §0).
				return r.fail(ctx, &bucket, err)
			}
		} else {
			logger.Info("deleting bucket (skeleton mode: no StackIT teardown)", "bucket", bucket.EffectiveBucketName())
		}
		controllerutil.RemoveFinalizer(&bucket, s3v1.BucketFinalizer)
		if err := r.Update(ctx, &bucket); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present before doing any provisioning work.
	if !controllerutil.ContainsFinalizer(&bucket, s3v1.BucketFinalizer) {
		controllerutil.AddFinalizer(&bucket, s3v1.BucketFinalizer)
		if err := r.Update(ctx, &bucket); err != nil {
			return ctrl.Result{}, err
		}
		// The update re-triggers reconcile; return to work on a fresh object.
		return ctrl.Result{}, nil
	}

	// Skeleton mode: no service-account key configured, so no cloud calls.
	if r.Stackit == nil {
		meta.SetStatusCondition(&bucket.Status.Conditions, metav1.Condition{
			Type:    s3v1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  s3v1.ReasonNotImplemented,
			Message: "operator skeleton: no StackIT service-account key configured",
		})
		bucket.Status.Phase = s3v1.PhasePending
		bucket.Status.Message = "operator skeleton: no StackIT service-account key configured"
		bucket.Status.ObservedGeneration = bucket.Generation
		bucket.Status.OperatorVersion = r.OperatorVersion
		if err := r.Status().Update(ctx, &bucket); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	return r.reconcileNormal(ctx, &bucket)
}

// reconcileNormal provisions the bucket, credentials and isolation policy. Every
// step is idempotent so repeated reconciles converge without creating duplicate
// cloud resources.
func (r *BucketReconciler) reconcileNormal(ctx context.Context, b *s3v1.Bucket) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Guard against Secret data-key collisions before writing anything: two
	// logical fields mapping to the same key would silently drop a value. This is
	// a configuration error, so surface it without hammering a requeue.
	if err := b.ValidateSecretKeys(); err != nil {
		return r.failNoRequeue(ctx, b, err)
	}

	// Refuse to target the operator's own admin credentials Secret: otherwise the
	// workload write would pollute it and the finalizer would later delete it,
	// destroying the (unrecoverable) bootstrap admin key.
	if r.isAdminSecret(b) {
		return r.failNoRequeue(ctx, b, fmt.Errorf(
			"secretRef %s/%s targets the operator's admin credentials Secret; refusing to provision",
			b.SecretNamespace(), b.Spec.SecretRef.Name))
	}

	// This operator is bound to a single region; it cannot provision in another.
	// Writing spec.region into the Secret while provisioning in the operator's
	// region would advertise a region the bucket is not in, so reject the mismatch.
	if b.GetRegion() != r.Stackit.Region() {
		return r.failNoRequeue(ctx, b, fmt.Errorf(
			"spec.region %q does not match this operator's region %q; provisioning is limited to %q",
			b.GetRegion(), r.Stackit.Region(), r.Stackit.Region()))
	}

	// Resolve the physical bucket name once and freeze it (annotation now, status
	// at the end). A freshly composed name is validated here; if the prefix or
	// namespace push it out of the DNS/length range, that is a configuration fault
	// a retry cannot fix, so fail without a requeue hammer.
	name, fresh := decideBucketName(r.Naming, b)
	if fresh {
		if err := s3v1.ValidateBucketName(name); err != nil {
			return r.failNoRequeue(ctx, b, fmt.Errorf("composed bucket name is invalid: %w", err))
		}
	}
	if err := r.persistResolvedName(ctx, b, name); err != nil {
		return r.fail(ctx, b, fmt.Errorf("persist resolved bucket name: %w", err))
	}

	r.markProvisioning(ctx, b)

	admin, err := r.ensureAdmin(ctx)
	if err != nil {
		return r.fail(ctx, b, fmt.Errorf("bootstrap admin credentials: %w", err))
	}

	if err := r.Stackit.EnsureService(ctx); err != nil {
		return r.fail(ctx, b, fmt.Errorf("enable object storage: %w", err))
	}

	if err := r.ensureBucket(ctx, b, name, admin); err != nil {
		return r.failEnsureBucket(ctx, b, err)
	}

	host, bucketURL, err := r.Stackit.BucketConnInfo(ctx, name)
	if err != nil {
		return r.fail(ctx, b, fmt.Errorf("bucket connection info: %w", err))
	}

	gid, urn, err := r.Stackit.EnsureCredentialsGroup(ctx, workloadGroupName(b))
	if err != nil {
		return r.fail(ctx, b, fmt.Errorf("ensure credentials group: %w", err))
	}

	accessKeyID, err := r.ensureAccessKeyAndSecret(ctx, b, gid, host, bucketURL)
	if err != nil {
		return r.fail(ctx, b, fmt.Errorf("ensure workload credentials: %w", err))
	}

	if err := r.ensureBucketPolicy(ctx, name, admin, urn); err != nil {
		return r.fail(ctx, b, fmt.Errorf("ensure bucket policy: %w", err))
	}

	// A pending rotation was just performed by ensureAccessKeyAndSecret (its
	// skip path is disabled while a rotation is pending). Record the handled
	// trigger so the annotation goes back to level-triggered no-op; this is part
	// of the same terminal status write below, so a crash in between simply
	// rotates again on the next reconcile (harmless: hard rotation).
	if trigger := b.PendingRotationTrigger(); trigger != "" {
		b.Status.LastRotationTrigger = trigger
		now := metav1.Now()
		b.Status.LastRotationTime = &now
		logger.Info("workload credentials rotated", "bucket", name, "trigger", trigger)
		r.event(b, corev1.EventTypeNormal, reasonRotated, "workload access key rotated (rotate-credentials-at annotation)")
	}

	// Success: record observed state and mark Ready.
	b.Status.ResolvedBucketName = name
	b.Status.BucketURL = bucketURL
	b.Status.CredentialsGroupID = gid
	b.Status.CredentialsGroupURN = urn
	b.Status.AccessKeyID = accessKeyID
	b.Status.ObservedGeneration = b.Generation
	b.Status.OperatorVersion = r.OperatorVersion
	b.Status.Phase = s3v1.PhaseReady
	b.Status.Message = fmt.Sprintf("bucket %q provisioned with isolated workload credentials", name)
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:    s3v1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  s3v1.ReasonProvisioned,
		Message: fmt.Sprintf("bucket %q provisioned with isolated workload credentials", name),
	})
	if err := r.Status().Update(ctx, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("bucket provisioned", "bucket", name, "requested", b.Spec.BucketName, "credentialsGroup", gid)
	r.event(b, corev1.EventTypeNormal, s3v1.ReasonProvisioned, "bucket and isolated workload credentials provisioned")
	return ctrl.Result{}, nil
}

// ensureBucket makes the bucket exist, is idempotent, and enforces ownership.
// STACKIT has no native bucket tags, so ownership is recorded as an S3 bucket tag
// (managed-by + owner) via the admin data-plane key:
//
//   - A bucket this operator creates is stamped with its ownership tags.
//   - A pre-existing bucket is only adopted when its tags match this operator; a
//     mismatch is a collision (ownershipCollisionError) so the operator never
//     manages a bucket it did not provision.
//   - An untagged pre-existing bucket is claimed only when empty (a crash between
//     create and tag-write leaves exactly this state); a non-empty untagged bucket
//     is treated as foreign and refused.
func (r *BucketReconciler) ensureBucket(ctx context.Context, b *s3v1.Bucket, name string, admin *adminCreds) error {
	ok, err := r.Stackit.HasBucket(ctx, r.Stackit.ProjectID(), name)
	if err != nil {
		return err
	}
	if ok {
		return r.adoptOrCollide(ctx, b, name, admin)
	}
	if err := r.Stackit.CreateBucket(ctx, name); err != nil {
		// Tolerate a create race (bucket appeared between the check and the create):
		// fall through to the ownership check rather than blindly stamping our tags.
		if stackit.StatusCode(err) != 409 {
			return err
		}
		if err := r.Stackit.WaitBucketVisible(ctx, name, bucketVisibleTimeout); err != nil {
			return err
		}
		return r.adoptOrCollide(ctx, b, name, admin)
	}
	if err := r.Stackit.WaitBucketVisible(ctx, name, bucketVisibleTimeout); err != nil {
		return err
	}
	// Freshly created by us: stamp ownership so later reconciles (and other
	// operators/fleets) recognize it.
	s3admin, err := r.newS3Admin(ctx, name, admin)
	if err != nil {
		return err
	}
	return s3admin.SetBucketTags(ctx, name, r.ownershipTags(b))
}

// failEnsureBucket maps an ensureBucket error onto the right terminal state: an
// ownership collision is a human-actionable fault that must not requeue-hammer,
// while any other error is transient and retried.
func (r *BucketReconciler) failEnsureBucket(ctx context.Context, b *s3v1.Bucket, err error) (ctrl.Result, error) {
	var collision *ownershipCollisionError
	if errors.As(err, &collision) {
		r.event(b, corev1.EventTypeWarning, s3v1.ReasonFailed,
			"bucket ownership collision: a bucket with this name exists but was not provisioned by this operator")
		return r.failNoRequeue(ctx, b, err)
	}
	return r.fail(ctx, b, fmt.Errorf("ensure bucket: %w", err))
}

// adoptOrCollide inspects a pre-existing bucket's ownership tags and decides
// whether this operator may adopt it. It returns an *ownershipCollisionError when
// the bucket belongs to someone else (a non-requeuing, human-actionable fault).
func (r *BucketReconciler) adoptOrCollide(ctx context.Context, b *s3v1.Bucket, name string, admin *adminCreds) error {
	s3admin, err := r.newS3Admin(ctx, name, admin)
	if err != nil {
		return err
	}
	tagSet, err := s3admin.BucketTags(ctx, name)
	if err != nil {
		return err
	}
	if len(tagSet) == 0 {
		// Untagged: either our own crash between create and tag-write, or a foreign
		// bucket sharing this name. Claim it only if empty (no data to endanger).
		empty, err := s3admin.BucketEmpty(ctx, name)
		if err != nil {
			return err
		}
		if !empty {
			return &ownershipCollisionError{name: name, detail: "pre-existing non-empty bucket carries no ownership tags"}
		}
		return s3admin.SetBucketTags(ctx, name, r.ownershipTags(b))
	}
	if r.isOwnedByUs(tagSet, b) {
		return nil
	}
	return &ownershipCollisionError{
		name:   name,
		detail: fmt.Sprintf("owned by managed-by=%q owner=%q", tagSet[tagOwnershipManagedBy], tagSet[tagOwnershipOwner]),
	}
}

// newS3Admin builds an admin data-plane client for the bucket's region-uniform
// endpoint. The bucket must already exist (the endpoint is derived from it).
func (r *BucketReconciler) newS3Admin(ctx context.Context, name string, admin *adminCreds) (*stackit.S3Admin, error) {
	endpoint, err := r.Stackit.BucketEndpoint(ctx, name)
	if err != nil {
		return nil, err
	}
	return stackit.NewS3Admin(endpoint, admin.accessKeyID, admin.secretAccessKey, r.Stackit.Region())
}

// defaultOwnershipName is the fallback managed-by value when OwnershipName is
// unset (e.g. tests constructing the reconciler directly). Production sets it via
// the --ownership-name flag / Helm value.
const defaultOwnershipName = "stackit-s3-provisioner"

// Ownership tag keys attached to every provisioned bucket. managed-by is the
// operator/fleet identity (configurable); owner is the DR-stable per-CR identity.
const (
	tagOwnershipManagedBy = "managed-by"
	tagOwnershipOwner     = "owner"
)

// ownershipName returns the effective managed-by value.
func (r *BucketReconciler) ownershipName() string {
	if r.OwnershipName != "" {
		return r.OwnershipName
	}
	return defaultOwnershipName
}

// ownerTagValue is the DR-stable owner identity of a Bucket CR: its
// namespace/name. It deliberately excludes metadata.uid, which is reassigned when
// the CR is restored into a fresh cluster, so a disaster-recovery restore that
// re-applies the same manifests still recognizes its own buckets. This mirrors
// workloadGroupName's stable-identity choice.
func ownerTagValue(b *s3v1.Bucket) string {
	return b.Namespace + "/" + b.Name
}

// ownershipTags is the tag set this operator stamps on buckets it owns.
func (r *BucketReconciler) ownershipTags(b *s3v1.Bucket) map[string]string {
	return map[string]string{
		tagOwnershipManagedBy: r.ownershipName(),
		tagOwnershipOwner:     ownerTagValue(b),
	}
}

// isOwnedByUs reports whether an existing bucket's tag set proves this operator
// provisioned it for this CR (both managed-by and owner must match).
func (r *BucketReconciler) isOwnedByUs(tagSet map[string]string, b *s3v1.Bucket) bool {
	return tagSet[tagOwnershipManagedBy] == r.ownershipName() &&
		tagSet[tagOwnershipOwner] == ownerTagValue(b)
}

// ownershipCollisionError signals that a bucket with the target name already
// exists but is not owned by this operator, so it must not be adopted or deleted.
// It is a configuration/operational fault a requeue cannot fix.
type ownershipCollisionError struct {
	name   string
	detail string
}

func (e *ownershipCollisionError) Error() string {
	return fmt.Sprintf("bucket %q already exists and is not owned by this operator (%s); refusing to adopt", e.name, e.detail)
}

// ensureAccessKeyAndSecret guarantees that the workload credentials group holds
// exactly the access key materialised in the workload Secret, and returns its
// access key id.
//
// The Secret is treated as the source of truth for the live credential (the
// secret_access_key is only ever returned once, at create time, so it cannot be
// re-fetched). If the Secret already carries a credential and the group still has
// a key, nothing changes. Otherwise the group's keys are cleared (their secrets
// are unrecoverable) and a fresh key is created and written — this heals a lost
// Secret and, because clearing precedes creation, never leaves an orphan key.
func (r *BucketReconciler) ensureAccessKeyAndSecret(ctx context.Context, b *s3v1.Bucket, groupID, host, bucketURL string) (string, error) {
	secretKey := types.NamespacedName{Name: b.Spec.SecretRef.Name, Namespace: b.SecretNamespace()}

	var sec corev1.Secret
	getErr := r.Get(ctx, secretKey, &sec)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return "", fmt.Errorf("get credentials secret %s: %w", secretKey, getErr)
	}

	keyIDs, err := r.Stackit.ListAccessKeyIDs(ctx, groupID)
	if err != nil {
		return "", err
	}

	if getErr == nil && secretHasCreds(&sec, b) && len(keyIDs) > 0 && b.PendingRotationTrigger() == "" {
		// Already provisioned, the group still backs the credential and no
		// rotation is requested.
		return secretAccessKeyID(&sec, b), nil
	}

	// (Re)provision. Clear any stale keys first so a crash-orphaned key cannot
	// accumulate, then create the single fresh key.
	if err := r.Stackit.DeleteAllAccessKeys(ctx, groupID); err != nil {
		return "", fmt.Errorf("clear stale access keys: %w", err)
	}
	ak, err := r.Stackit.CreateAccessKey(ctx, groupID)
	if err != nil {
		return "", err
	}
	data := b.SecretData(s3v1.SecretValues{
		AccessKeyID:     ak.AccessKeyID,
		SecretAccessKey: ak.SecretAccessKey,
		Endpoint:        host,
		BucketURL:       bucketURL,
	})
	if err := r.upsertSecret(ctx, b, secretKey, data); err != nil {
		// The secret_access_key cannot be recovered, so a key whose Secret write
		// failed is worthless — delete it to avoid an orphan.
		if delErr := r.Stackit.DeleteAccessKey(ctx, groupID, ak.KeyID); delErr != nil {
			log.FromContext(ctx).Error(delErr, "failed to roll back orphaned access key", "group", groupID)
		}
		return "", fmt.Errorf("write credentials secret %s: %w", secretKey, err)
	}
	return ak.AccessKeyID, nil
}

// ensureBucketPolicy applies the isolation policy (INIT-SETUP.md §4.1) via the
// admin S3 key, re-writing it only when it drifts from the desired document.
func (r *BucketReconciler) ensureBucketPolicy(ctx context.Context, name string, admin *adminCreds, workloadURN string) error {
	s3admin, err := r.newS3Admin(ctx, name, admin)
	if err != nil {
		return err
	}
	desired := stackit.BuildIsolationPolicy(name, admin.urn, workloadURN)
	if current, err := s3admin.GetBucketPolicy(ctx, name); err == nil && stackit.PoliciesEquivalent(current, desired) {
		return nil
	}
	return s3admin.SetBucketPolicy(ctx, name, desired)
}

// teardown releases the StackIT resources backing a Bucket during finalization,
// enforcing the empty-only delete guard before removing anything. Order:
// empty-check → workload keys → workload group → bucket → Secret. The shared
// admin group is never touched.
func (r *BucketReconciler) teardown(ctx context.Context, b *s3v1.Bucket) error {
	name := b.EffectiveBucketName()

	bucketExists, err := r.Stackit.HasBucket(ctx, r.Stackit.ProjectID(), name)
	if err != nil {
		return err
	}

	// Empty-only guard (INIT-SETUP.md §0), optionally preceded by a requested
	// wipe: refuse deletion while the bucket holds data. Done first, before any
	// credential is removed, so a blocked delete leaves the workload fully
	// functional.
	if bucketExists {
		if err := r.prepareBucketForDelete(ctx, b, name); err != nil {
			return err
		}
	}

	groupID, err := r.resolveWorkloadGroupID(ctx, b)
	if err != nil {
		return err
	}
	if groupID != "" {
		if err := r.Stackit.DeleteAllAccessKeys(ctx, groupID); err != nil {
			return err
		}
		if err := r.Stackit.DeleteCredentialsGroup(ctx, groupID); err != nil && stackit.StatusCode(err) != 404 {
			return err
		}
	}

	if bucketExists {
		if err := r.deleteBucketIfOwned(ctx, b, name); err != nil {
			return err
		}
	}

	// Defense in depth: never delete the operator's own admin credentials Secret,
	// even if a CR was (mis)configured to reference it (reconcileNormal already
	// refuses to provision such a CR).
	if r.isAdminSecret(b) {
		return nil
	}
	return r.deleteSecret(ctx, b)
}

// isAdminSecret reports whether a Bucket's resolved credentials Secret is the
// operator-owned bootstrap admin Secret.
func (r *BucketReconciler) isAdminSecret(b *s3v1.Bucket) bool {
	return b.Spec.SecretRef.Name == r.AdminSecretName && b.SecretNamespace() == r.AdminSecretNamespace
}

// prepareBucketForDelete enforces the data-loss guard before teardown removes
// anything. Default is the empty-only guard: a non-empty bucket blocks
// deletion. When the CR requests a wipe (spec.wipeOnDelete) AND the operator's
// wipe feature gate is enabled AND the ownership tags prove this operator
// provisioned the bucket, all objects are deleted first instead. A requested
// wipe that the gate disables, or that ownership cannot authorize, degrades to
// the empty-only guard with a warning event — never to silent data loss.
func (r *BucketReconciler) prepareBucketForDelete(ctx context.Context, b *s3v1.Bucket, name string) error {
	if b.Spec.WipeOnDelete {
		switch {
		case !r.EnableWipeOnDelete:
			r.event(b, corev1.EventTypeWarning, reasonWipeDisabled,
				"spec.wipeOnDelete requested but the wipe feature is disabled by operator config (wipeOnDelete.enabled); falling back to empty-only delete guard")
		default:
			owned, err := r.bucketOwnedByUs(ctx, b, name)
			if err != nil {
				return err
			}
			if !owned {
				r.event(b, corev1.EventTypeWarning, reasonWipeDisabled,
					"refusing to wipe: bucket is not owned by this operator (no matching ownership tags); falling back to empty-only delete guard")
				break
			}
			return r.wipeBucket(ctx, b, name)
		}
	}
	return r.assertBucketEmpty(ctx, b, name)
}

// reasonWipeDisabled is the event reason for a requested wipe that was degraded
// to the empty-only delete guard (feature gate off, or ownership not proven).
const reasonWipeDisabled = "WipeOnDeleteSkipped"

// reasonWiping is the event reason emitted when a wipe starts.
const reasonWiping = "WipingBucket"

// reasonRotated is the event reason emitted after an annotation-triggered
// credentials rotation completed.
const reasonRotated = "CredentialsRotated"

// wipeBucket deletes all objects (including versions and delete markers) from
// an owned bucket during teardown, as explicitly requested via spec.wipeOnDelete.
func (r *BucketReconciler) wipeBucket(ctx context.Context, b *s3v1.Bucket, name string) error {
	admin, err := r.ensureAdmin(ctx)
	if err != nil {
		return err
	}
	s3admin, err := r.newS3Admin(ctx, name, admin)
	if err != nil {
		return err
	}
	log.FromContext(ctx).Info("wiping bucket contents before deletion (spec.wipeOnDelete)", "bucket", name)
	r.event(b, corev1.EventTypeNormal, reasonWiping, "deleting all objects before bucket removal (spec.wipeOnDelete)")
	// Best-effort progress hint; the wipe can take a while on large buckets.
	b.Status.Message = fmt.Sprintf("wiping bucket %q before deletion", name)
	if err := r.Status().Update(ctx, b); err != nil {
		log.FromContext(ctx).V(1).Info("wipe status update did not apply", "error", err.Error())
	}
	return s3admin.WipeBucket(ctx, name)
}

// assertBucketEmpty returns an error (blocking deletion) unless the bucket holds
// no objects, using the admin S3 credential to inspect its contents.
func (r *BucketReconciler) assertBucketEmpty(ctx context.Context, b *s3v1.Bucket, name string) error {
	admin, err := r.ensureAdmin(ctx)
	if err != nil {
		return err
	}
	s3admin, err := r.newS3Admin(ctx, name, admin)
	if err != nil {
		return err
	}
	empty, err := s3admin.BucketEmpty(ctx, name)
	if err != nil {
		return err
	}
	if !empty {
		r.event(b, corev1.EventTypeWarning, s3v1.ReasonFailed, "refusing to delete non-empty bucket")
		return fmt.Errorf("bucket %q is not empty; refusing to delete (data-loss guard)", name)
	}
	return nil
}

// deleteBucketIfOwned is the teardown ownership guard (defense in depth on top of
// the empty-check): it deletes the bucket only when its ownership tags prove this
// operator provisioned it. A foreign bucket that shares this name, or one we
// created but crashed before tagging, is left in place and surfaced rather than
// removed.
func (r *BucketReconciler) deleteBucketIfOwned(ctx context.Context, b *s3v1.Bucket, name string) error {
	owned, err := r.bucketOwnedByUs(ctx, b, name)
	if err != nil {
		return err
	}
	if !owned {
		log.FromContext(ctx).Info("skipping bucket deletion: bucket is not owned by this operator", "bucket", name)
		r.event(b, corev1.EventTypeWarning, s3v1.ReasonFailed,
			"not deleting bucket: it is not owned by this operator (no matching ownership tags)")
		return nil
	}
	if err := r.Stackit.DeleteBucket(ctx, name); err != nil && stackit.StatusCode(err) != 404 {
		return err
	}
	return nil
}

// bucketOwnedByUs reports whether the existing bucket's ownership tags prove this
// operator provisioned it for this CR. An untagged bucket returns false, so the
// teardown guard leaves it in place rather than deleting a bucket it cannot claim.
func (r *BucketReconciler) bucketOwnedByUs(ctx context.Context, b *s3v1.Bucket, name string) (bool, error) {
	admin, err := r.ensureAdmin(ctx)
	if err != nil {
		return false, err
	}
	s3admin, err := r.newS3Admin(ctx, name, admin)
	if err != nil {
		return false, err
	}
	tagSet, err := s3admin.BucketTags(ctx, name)
	if err != nil {
		return false, err
	}
	return r.isOwnedByUs(tagSet, b), nil
}

// resolveWorkloadGroupID returns the workload credentials-group id for teardown,
// preferring the recorded status and falling back to a lookup by deterministic
// name so a lost status still cleans up. Returns "" when no group exists.
func (r *BucketReconciler) resolveWorkloadGroupID(ctx context.Context, b *s3v1.Bucket) (string, error) {
	if b.Status.CredentialsGroupID != "" {
		return b.Status.CredentialsGroupID, nil
	}
	id, _, found, err := r.Stackit.FindCredentialsGroupByName(ctx, workloadGroupName(b))
	if err != nil {
		return "", err
	}
	if found {
		return id, nil
	}
	return "", nil
}

// ensureAdmin loads or bootstraps the operator-wide S3 admin credential used to
// set bucket policies. It is cached after the first success. A missing or
// incomplete admin Secret triggers a (re)bootstrap: the admin group is looked up
// or created by name, its stale keys are cleared, a fresh key is created and the
// Secret is written.
func (r *BucketReconciler) ensureAdmin(ctx context.Context) (*adminCreds, error) {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()

	if r.admin != nil {
		return r.admin, nil
	}
	if r.AdminSecretNamespace == "" {
		return nil, fmt.Errorf("operator namespace unknown (set POD_NAMESPACE); cannot manage admin credentials")
	}

	secretKey := types.NamespacedName{Name: r.AdminSecretName, Namespace: r.AdminSecretNamespace}
	var sec corev1.Secret
	err := r.Get(ctx, secretKey, &sec)
	switch {
	case err == nil:
		if ac := adminFromSecret(&sec); ac != nil {
			r.admin = ac
			return ac, nil
		}
		// Secret exists but is incomplete; fall through to (re)bootstrap in place.
	case apierrors.IsNotFound(err):
		// Fall through to bootstrap.
	default:
		return nil, fmt.Errorf("get admin secret %s: %w", secretKey, err)
	}

	gid, urn, err := r.Stackit.EnsureCredentialsGroup(ctx, adminGroupName)
	if err != nil {
		return nil, fmt.Errorf("ensure admin credentials group: %w", err)
	}
	// Any pre-existing admin key has an unrecoverable secret; replace it.
	if err := r.Stackit.DeleteAllAccessKeys(ctx, gid); err != nil {
		return nil, fmt.Errorf("clear admin access keys: %w", err)
	}
	ak, err := r.Stackit.CreateAccessKey(ctx, gid)
	if err != nil {
		return nil, fmt.Errorf("create admin access key: %w", err)
	}
	ac := &adminCreds{accessKeyID: ak.AccessKeyID, secretAccessKey: ak.SecretAccessKey, urn: urn, groupID: gid}
	if err := r.writeAdminSecret(ctx, secretKey, ac); err != nil {
		if delErr := r.Stackit.DeleteAccessKey(ctx, gid, ak.KeyID); delErr != nil {
			log.FromContext(ctx).Error(delErr, "failed to roll back orphaned admin access key", "group", gid)
		}
		return nil, fmt.Errorf("persist admin secret %s: %w", secretKey, err)
	}
	r.admin = ac
	return ac, nil
}

// upsertSecret creates or updates the workload credentials Secret, merging the
// provisioned data keys in without disturbing unrelated entries. A controller
// owner reference is set only when the Secret shares the Bucket's namespace
// (cross-namespace owner references are not permitted).
func (r *BucketReconciler) upsertSecret(ctx context.Context, b *s3v1.Bucket, key types.NamespacedName, data map[string][]byte) error {
	sec := &corev1.Secret{}
	sec.Name = key.Name
	sec.Namespace = key.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels[managedByLabel] = managedByValue
		sec.Type = corev1.SecretTypeOpaque
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		for k, v := range data {
			sec.Data[k] = v
		}
		if key.Namespace == b.Namespace {
			return controllerutil.SetControllerReference(b, sec, r.Scheme)
		}
		return nil
	})
	return err
}

// writeAdminSecret persists the bootstrap admin credential to the operator-owned
// Secret.
func (r *BucketReconciler) writeAdminSecret(ctx context.Context, key types.NamespacedName, ac *adminCreds) error {
	sec := &corev1.Secret{}
	sec.Name = key.Name
	sec.Namespace = key.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels[managedByLabel] = managedByValue
		sec.Type = corev1.SecretTypeOpaque
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[adminSecretKeyAccessKeyID] = []byte(ac.accessKeyID)
		sec.Data[adminSecretKeySecretAccessKey] = []byte(ac.secretAccessKey)
		sec.Data[adminSecretKeyURN] = []byte(ac.urn)
		sec.Data[adminSecretKeyGroupID] = []byte(ac.groupID)
		return nil
	})
	return err
}

// deleteSecret removes the workload credentials Secret, tolerating its absence.
func (r *BucketReconciler) deleteSecret(ctx context.Context, b *s3v1.Bucket) error {
	sec := &corev1.Secret{}
	sec.Name = b.Spec.SecretRef.Name
	sec.Namespace = b.SecretNamespace()
	if err := r.Delete(ctx, sec); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// fail records a failed reconcile (Ready=False, reason Failed) and requeues via
// the returned error so the controller retries with backoff.
func (r *BucketReconciler) fail(ctx context.Context, b *s3v1.Bucket, err error) (ctrl.Result, error) {
	r.markFailed(ctx, b, err)
	return ctrl.Result{}, err
}

// failNoRequeue records a failed reconcile without returning an error, for
// configuration faults that a retry cannot fix (they re-reconcile on spec change).
func (r *BucketReconciler) failNoRequeue(ctx context.Context, b *s3v1.Bucket, err error) (ctrl.Result, error) {
	r.markFailed(ctx, b, err)
	return ctrl.Result{}, nil
}

func (r *BucketReconciler) markFailed(ctx context.Context, b *s3v1.Bucket, err error) {
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:    s3v1.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  s3v1.ReasonFailed,
		Message: err.Error(),
	})
	b.Status.Phase = s3v1.PhaseFailed
	b.Status.Message = err.Error()
	b.Status.ObservedGeneration = b.Generation
	b.Status.OperatorVersion = r.OperatorVersion
	r.event(b, corev1.EventTypeWarning, s3v1.ReasonFailed, err.Error())
	if uerr := r.Status().Update(ctx, b); uerr != nil {
		log.FromContext(ctx).V(1).Info("status update after failure did not apply", "error", uerr.Error())
	}
}

// event records a Kubernetes event when a recorder is configured. The note is
// passed as a %s argument, never as the format string, so a literal '%' in an
// error message cannot corrupt the event.
func (r *BucketReconciler) event(b *s3v1.Bucket, eventtype, reason, note string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(b, nil, eventtype, reason, "Reconcile", "%s", note)
	}
}

// SetupWithManager registers the reconciler with the manager.
//
// Besides owning Bucket objects, it watches the workload credentials Secrets it
// provisions: if such a Secret is deleted or altered out from under the
// operator, the owning Bucket is re-queued and ensureAccessKeyAndSecret mints a
// fresh key and re-writes the Secret. The mapping matches on the resolved secret
// name+namespace, so it covers cross-namespace secretRefs too (where an owner
// reference cannot exist). The predicate limits the watch to operator-managed
// Secrets so unrelated Secret churn does not wake the controller.
func (r *BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("bucket-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&s3v1.Bucket{}).
		Named("bucket").
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.bucketsForSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(isManagedSecret)),
		).
		Complete(r)
}

// isManagedSecret reports whether a Secret was provisioned by this operator
// (it carries the managed-by label). Used to scope the Secret watch.
func isManagedSecret(obj client.Object) bool {
	return obj.GetLabels()[managedByLabel] == managedByValue
}

// bucketsForSecret maps a Secret event to the Bucket(s) whose resolved
// secretRef targets that Secret, so a deleted or mutated credentials Secret
// re-triggers reconcile. Matching by name+namespace (rather than owner
// reference) also covers cross-namespace secretRefs.
func (r *BucketReconciler) bucketsForSecret(ctx context.Context, obj client.Object) []ctrl.Request {
	var buckets s3v1.BucketList
	if err := r.List(ctx, &buckets); err != nil {
		log.FromContext(ctx).Error(err, "listing buckets for secret-triggered reconcile",
			"secret", client.ObjectKeyFromObject(obj))
		return nil
	}
	var reqs []ctrl.Request
	for i := range buckets.Items {
		b := &buckets.Items[i]
		if b.Spec.SecretRef.Name == obj.GetName() && b.SecretNamespace() == obj.GetNamespace() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: b.Namespace, Name: b.Name,
			}})
		}
	}
	return reqs
}

// markProvisioning flips the coarse phase to Provisioning exactly once per spec
// change (and on first reconcile / recovery from Failed) so the slow cloud steps
// are visible in status without a per-step write. It is a best-effort progress
// hint: a failed status write is logged and ignored, because the terminal
// Ready/Failed write at the end of the reconcile sets the authoritative state.
// The settled-for-generation guard stops a converged Ready/Failed object being
// flipped back to Provisioning, which would self-trigger an endless reconcile via
// the Bucket status watch.
func (r *BucketReconciler) markProvisioning(ctx context.Context, b *s3v1.Bucket) {
	if provisioningSettled(b) || b.Status.Phase == s3v1.PhaseProvisioning {
		return
	}
	b.Status.Phase = s3v1.PhaseProvisioning
	b.Status.Message = "provisioning bucket and workload credentials"
	if err := r.Status().Update(ctx, b); err != nil {
		log.FromContext(ctx).V(1).Info("provisioning status update did not apply", "error", err.Error())
	}
}

// provisioningSettled reports whether the operator has already driven the current
// spec generation to a terminal phase (Ready or Failed). The Provisioning marker
// is skipped in that case so a re-reconcile that observes no spec change — e.g.
// the echo of our own status write, or a watched Secret event — does not flip the
// phase back to Provisioning and self-trigger an endless reconcile loop.
func provisioningSettled(b *s3v1.Bucket) bool {
	return b.Status.ObservedGeneration == b.Generation &&
		(b.Status.Phase == s3v1.PhaseReady || b.Status.Phase == s3v1.PhaseFailed)
}

// decideBucketName selects the physical StackIT bucket name for a CR without any
// I/O, in priority order:
//  1. status.resolvedBucketName — already frozen; authoritative.
//  2. the resolved-name annotation — the durable backup, used when status was
//     lost (CR restored from backup, status wiped).
//  3. a pre-feature bucket (status.bucketURL set but no frozen name) — keep the
//     raw spec.bucketName so an upgrade never re-maps an existing bucket.
//  4. otherwise compose a fresh name from the operator's current naming policy.
//
// The bool reports whether the name was freshly composed (case 4) and therefore
// still needs length/DNS validation before it is frozen.
func decideBucketName(naming s3v1.BucketNaming, b *s3v1.Bucket) (name string, fresh bool) {
	switch {
	case b.Status.ResolvedBucketName != "":
		return b.Status.ResolvedBucketName, false
	case b.Annotations[s3v1.ResolvedBucketNameAnnotation] != "":
		return b.Annotations[s3v1.ResolvedBucketNameAnnotation], false
	case b.Status.BucketURL != "":
		return b.Spec.BucketName, false
	default:
		return naming.ComposeBucketName(b), true
	}
}

// persistResolvedName freezes the resolved bucket name into the durable
// annotation before any cloud resource is created. Writing it here (rather than
// only into status at the end) means a crash between bucket creation and the
// final status write cannot lose the name: the next reconcile reads it back from
// the annotation instead of recomposing from a possibly-changed policy. It is a
// no-op once the annotation already carries the name.
func (r *BucketReconciler) persistResolvedName(ctx context.Context, b *s3v1.Bucket, name string) error {
	if b.Annotations[s3v1.ResolvedBucketNameAnnotation] == name {
		return nil
	}
	if b.Annotations == nil {
		b.Annotations = map[string]string{}
	}
	b.Annotations[s3v1.ResolvedBucketNameAnnotation] = name
	return r.Update(ctx, b)
}

// maxGroupNameLen is the maximum length StackIT's Object Storage API accepts for
// a credentials-group displayName. Exceeding it yields a 422 string_too_long.
const maxGroupNameLen = 32

// workloadGroupName derives the deterministic display name of a Bucket's
// dedicated credentials group. The suffix hashes the Bucket's namespace/name
// identity (not its metadata.uid), so the name is stable across a
// disaster-recovery restore that re-creates the CR with a fresh UID: the
// operator then re-uses the surviving cloud group by name instead of creating a
// duplicate and orphaning the old one (which would keep a live, un-invalidated
// access key). The suffix also keeps the name unique when the namespace/name
// portion is truncated to the length budget.
func workloadGroupName(b *s3v1.Bucket) string {
	suffix := shortHash(b.Namespace + "/" + b.Name)
	base := fmt.Sprintf("s3op-%s-%s", b.Namespace, b.Name)
	if keep := maxGroupNameLen - len(suffix) - 1; len(base) > keep {
		base = base[:keep]
	}
	return base + "-" + suffix
}

// shortHash returns an 8-hex-digit FNV-1a hash of s (non-cryptographic; used only
// for a stable, collision-resistant name suffix).
func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// secretHasCreds reports whether the Secret already carries both credential
// values under the Bucket's resolved key names.
func secretHasCreds(sec *corev1.Secret, b *s3v1.Bucket) bool {
	keys := b.Spec.SecretRef.Keys
	return len(sec.Data[keys.AccessKeyIDKey()]) > 0 && len(sec.Data[keys.SecretAccessKeyKey()]) > 0
}

// secretAccessKeyID returns the access key id stored in the Secret under the
// Bucket's resolved key name.
func secretAccessKeyID(sec *corev1.Secret, b *s3v1.Bucket) string {
	return string(sec.Data[b.Spec.SecretRef.Keys.AccessKeyIDKey()])
}

// adminFromSecret extracts admin credentials from the operator-owned Secret,
// returning nil when a required field is missing (an incomplete Secret triggers a
// rebootstrap).
func adminFromSecret(sec *corev1.Secret) *adminCreds {
	ak := string(sec.Data[adminSecretKeyAccessKeyID])
	sk := string(sec.Data[adminSecretKeySecretAccessKey])
	urn := string(sec.Data[adminSecretKeyURN])
	if ak == "" || sk == "" || urn == "" {
		return nil
	}
	return &adminCreds{
		accessKeyID:     ak,
		secretAccessKey: sk,
		urn:             urn,
		groupID:         string(sec.Data[adminSecretKeyGroupID]),
	}
}
