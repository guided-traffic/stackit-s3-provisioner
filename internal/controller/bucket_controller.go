// Package controller contains the Bucket reconciler for the StackIT S3 provisioner.
package controller

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"golang.org/x/time/rate"

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

	// CloneImage is the container image run by clone Jobs (spec.cloneFrom).
	// Empty falls back to DefaultCloneImage.
	CloneImage string

	// CloneJobResources are the resource requirements applied to clone Job pods
	// (Helm value clone.resources). The zero value applies none.
	CloneJobResources corev1.ResourceRequirements

	// DriftResyncInterval, when > 0, requeues a successfully reconciled Bucket
	// after this duration so configuration drift — most importantly the per-bucket
	// isolation policy — self-heals without waiting for an event. The Bucket watch
	// only fires on generation/annotation changes (the predicate filters out
	// controller-runtime's periodic resync, see SetupWithManager), so a policy
	// change shipped in an operator upgrade would otherwise never reach an
	// already-provisioned, otherwise-unchanged bucket. Zero disables the requeue.
	DriftResyncInterval time.Duration

	// ProviderDegradedGrace, when > 0, is how long an already-provisioned Bucket
	// keeps its Ready state while reconciles fail for a non-definitive reason —
	// an unreachable provider, a gateway error page, a Kubernetes API blip.
	// Ready describes the last verified state of the bucket, not the outcome of
	// the last attempt to verify it, so a short provider outage no longer marks
	// every Bucket in the cluster unhealthy at once and no longer cascades into
	// the health checks that watch them.
	//
	// Once the grace elapses the Bucket drops to Failed as before, so a real
	// outage still becomes visible; the window only decides how fast. Zero
	// disables the hold entirely and restores the previous behavior, which makes
	// it a values-only rollback that needs no new image.
	ProviderDegradedGrace time.Duration

	// Breaker is the fleet-wide provider circuit breaker. While it is open the
	// reconciler does no provider work at all and requeues on the breaker's own
	// cooldown, so one outage costs a handful of API calls instead of one per
	// retry per Bucket. Nil disables it.
	Breaker *ProviderBreaker

	adminMu sync.Mutex
	admin   *adminCreds // cached after the first successful bootstrap

	// cloneStatsFn overrides rc stats fetching in tests; nil uses HTTP against
	// the clone pod's rclone remote-control endpoint.
	cloneStatsFn func(ctx context.Context, baseURL, user, pass string) (*rcloneStats, error)

	cloneHTTPOnce sync.Once
	cloneHTTP     *http.Client
}

// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile drives a Bucket towards its desired state.
func (r *BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var bucket s3v1.Bucket
	if err := r.Get(ctx, req.NamespacedName, &bucket); err != nil {
		// Ignore not-found: the object was deleted after the reconcile was queued.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: release StackIT resources, then drop the finalizer.
	if !bucket.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &bucket)
	}

	// Ensure the finalizer is present before doing any provisioning work, then
	// continue in the same pass (the Update refreshed the in-memory object, and
	// the Bucket watch filters on generation/annotation changes, so this
	// metadata-only update would not re-trigger a reconcile by itself).
	if !controllerutil.ContainsFinalizer(&bucket, s3v1.BucketFinalizer) {
		controllerutil.AddFinalizer(&bucket, s3v1.BucketFinalizer)
		if err := r.Update(ctx, &bucket); err != nil {
			return ctrl.Result{}, err
		}
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

	// Fleet-wide provider outage: skip the provider work entirely rather than
	// re-establishing per Bucket that the provider is still down. See
	// ProviderBreaker for why this is not a per-error classification.
	if wait, allowed := r.Breaker.Allow(); !allowed {
		return r.holdForProvider(ctx, &bucket, wait)
	}

	return r.reconcileNormal(ctx, &bucket)
}

// holdForProvider parks a Bucket for the remainder of the breaker's cooldown
// without touching the provider.
//
// It returns no error on purpose. The provider being unreachable is already
// recorded once, by the failures that opened the breaker; counting it again for
// every Bucket on every probe interval is what turned a single outage into 242
// reconcile errors on mgmt-p on 2026-09-02.
//
// Status is written only when the record would actually change — the first visit
// under an open breaker, and the moment the degradation grace runs out. A Bucket
// already held (or already parked in Failed) is requeued silently, so an outage
// lasting hours costs no status churn.
func (r *BucketReconciler) holdForProvider(ctx context.Context, b *s3v1.Bucket, wait time.Duration) (ctrl.Result, error) {
	if r.providerHoldNeedsWrite(b) {
		err := fmt.Errorf("%w; next provider probe in %s", errProviderUnavailable, wait.Round(time.Second))
		if !r.degrade(ctx, b, err) {
			r.markFailed(ctx, b, err)
		}
	}
	return ctrl.Result{RequeueAfter: wait}, nil
}

// providerHoldNeedsWrite reports whether holdForProvider would record anything
// new about b. Everything else is a repeat of a state already on the object.
func (r *BucketReconciler) providerHoldNeedsWrite(b *s3v1.Bucket) bool {
	if b.Status.Phase == s3v1.PhaseFailed {
		// Already parked; the hold has nothing left to add.
		return false
	}
	if b.Status.DegradedSince == nil {
		// First visit under this outage: record that the hold started.
		return true
	}
	// Held already — the only transition still ahead is the grace running out,
	// which degrade() turns into a drop to Failed.
	return r.ProviderDegradedGrace > 0 &&
		time.Since(b.Status.DegradedSince.Time) >= r.ProviderDegradedGrace
}

// reconcileDelete releases the StackIT resources behind a Bucket being deleted
// and then drops the finalizer. Until the teardown has actually completed the
// finalizer stays, which is what keeps the CR (and with it the record of what
// has to be cleaned up) alive.
func (r *BucketReconciler) reconcileDelete(ctx context.Context, b *s3v1.Bucket) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(b, s3v1.BucketFinalizer) {
		return ctrl.Result{}, nil
	}
	if r.Stackit == nil {
		logger.Info("deleting bucket (skeleton mode: no StackIT teardown)", "bucket", b.EffectiveBucketName())
		return r.dropFinalizer(ctx, b)
	}

	// Surface that teardown is in progress (visible while a blocked delete —
	// e.g. a non-empty bucket — keeps the finalizer). Skip re-writing once
	// already Deleting or once a Failed teardown reason is recorded, so a
	// blocked delete does not flip-flop Deleting<->Failed and self-trigger
	// reconciles via the status watch.
	if b.Status.Phase != s3v1.PhaseDeleting && b.Status.Phase != s3v1.PhaseFailed {
		b.Status.Phase = s3v1.PhaseDeleting
		b.Status.Message = "releasing StackIT resources"
		if err := r.Status().Update(ctx, b); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	// The teardown talks to the provider on every step (empty check, keys,
	// group, bucket), so it is gated by the breaker like any other provider
	// work. The finalizer stays, the phase stays Deleting, and the delete
	// resumes on the next probe.
	if wait, allowed := r.Breaker.Allow(); !allowed {
		logger.V(1).Info("provider circuit open; deferring teardown",
			"bucket", b.EffectiveBucketName(), "retryAfter", wait)
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	if err := r.teardown(ctx, b); err != nil {
		logger.Error(err, "teardown failed; keeping finalizer", "bucket", b.EffectiveBucketName())
		// Keep the finalizer and surface the reason; a non-empty bucket must not
		// be deleted (data-loss guard, INIT-SETUP.md §0).
		return r.fail(ctx, b, err)
	}
	r.Breaker.Success()
	return r.dropFinalizer(ctx, b)
}

// dropFinalizer releases the CR once its cloud resources are gone.
func (r *BucketReconciler) dropFinalizer(ctx context.Context, b *s3v1.Bucket) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(b, s3v1.BucketFinalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, b))
}

// reconcileNormal provisions the bucket, credentials and isolation policy. Every
// step is idempotent so repeated reconciles converge without creating duplicate
// cloud resources.
func (r *BucketReconciler) reconcileNormal(ctx context.Context, b *s3v1.Bucket) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Configuration faults a retry cannot fix are surfaced without a requeue
	// hammer (they re-reconcile on spec change).
	if err := r.specGuardError(b); err != nil {
		return r.failNoRequeue(ctx, b, err)
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

	creds, done, res, err := r.provisionCredentialsAndClone(ctx, b, name, admin, host, bucketURL)
	if !done {
		return res, err
	}

	r.recordPendingRotation(ctx, b, name)

	// Success: record observed state and mark Ready.
	b.Status.ResolvedBucketName = name
	b.Status.BucketURL = bucketURL
	b.Status.CredentialsGroupID = creds.gid
	b.Status.CredentialsGroupURN = creds.urn
	b.Status.AccessKeyID = creds.accessKeyID
	b.Status.GrantedReadTo = creds.grantedTo
	b.Status.ObservedGeneration = b.Generation
	b.Status.OperatorVersion = r.OperatorVersion
	b.Status.Phase = s3v1.PhaseReady
	b.Status.Message = fmt.Sprintf("bucket %q provisioned with isolated workload credentials", name)
	// The provider answered for every step of this pass, so any held-over
	// degradation is over.
	clearDegraded(b)
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:    s3v1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  s3v1.ReasonProvisioned,
		Message: fmt.Sprintf("bucket %q provisioned with isolated workload credentials", name),
	})
	if err := r.Status().Update(ctx, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The provider answered every call in this pass, so it is reachable: close
	// the breaker even if earlier Buckets in this sweep failed.
	r.Breaker.Success()
	logger.Info("bucket provisioned", "bucket", name, "requested", b.Spec.BucketName, "credentialsGroup", creds.gid)
	r.event(b, corev1.EventTypeNormal, s3v1.ReasonProvisioned, "bucket and isolated workload credentials provisioned")
	// Requeue on a timer so drift (notably a policy change from an operator
	// upgrade) self-heals without an event. RequeueAfter <= 0 means no requeue,
	// so a zero interval leaves the behavior unchanged (event-driven only).
	return ctrl.Result{RequeueAfter: r.DriftResyncInterval}, nil
}

// specGuardError checks the Bucket spec for configuration faults that must
// fail the reconcile without a requeue: Secret data-key collisions (silent
// data loss), a secretRef targeting the operator's own admin credentials
// Secret (pollution now, admin-key destruction on delete), and a region this
// single-region operator cannot provision in.
func (r *BucketReconciler) specGuardError(b *s3v1.Bucket) error {
	if err := b.ValidateSecretKeys(); err != nil {
		return err
	}
	if r.isAdminSecret(b) {
		return fmt.Errorf(
			"secretRef %s/%s targets the operator's admin credentials Secret; refusing to provision",
			b.SecretNamespace(), b.Spec.SecretRef.Name)
	}
	if b.GetRegion() != r.Stackit.Region() {
		return fmt.Errorf(
			"spec.region %q does not match this operator's region %q; provisioning is limited to %q",
			b.GetRegion(), r.Stackit.Region(), r.Stackit.Region())
	}
	return nil
}

// workloadCreds bundles the provisioned credential identifiers recorded in
// Bucket status after a successful reconcile.
type workloadCreds struct {
	gid, urn, accessKeyID string

	// grantedTo are the spec.grantReadAccess entries that actually made it into
	// the bucket policy, recorded in status so a pending or revoked grant is
	// visible without reading the policy back from S3.
	grantedTo []string
}

// provisionCredentialsAndClone runs the credential half of the provisioning
// flow: workload credentials group → isolation policy → optional one-shot
// clone → workload access key + Secret. done=false means the reconcile ends
// here with the returned result/error (clone still running, or a failure that
// was already recorded in status).
//
// Ordering: the isolation policy is applied before any workload credential
// exists (and before a clone runs), so the bucket is never open to other
// project members while it is still being populated; the admin group stays
// exempt, which is exactly what the clone job's destination side
// authenticates with. With spec.cloneFrom set, the workload Secret is by
// default held back until the copy succeeded, so workloads never start
// against a partially copied bucket; holdSecretUntilCloned=false publishes it
// up front. Ready always waits for the clone either way.
func (r *BucketReconciler) provisionCredentialsAndClone(
	ctx context.Context, b *s3v1.Bucket, name string, admin *adminCreds, host, bucketURL string,
) (workloadCreds, bool, ctrl.Result, error) {
	var creds workloadCreds
	failed := func(err error) (workloadCreds, bool, ctrl.Result, error) {
		res, rerr := r.fail(ctx, b, err)
		return creds, false, res, rerr
	}

	var err error
	creds.gid, creds.urn, err = r.Stackit.EnsureCredentialsGroup(ctx, workloadGroupName(b))
	if err != nil {
		return failed(fmt.Errorf("ensure credentials group: %w", err))
	}
	// Publish the group identity as soon as it exists rather than only on the
	// terminal status write. It is the signal grantors watch for
	// (granteeCredentialsPredicate), and a Bucket that is itself still cloning
	// never reaches that terminal write — its grantors would then wait for the
	// drift resync instead of being woken.
	b.Status.CredentialsGroupID = creds.gid
	b.Status.CredentialsGroupURN = creds.urn

	// Resolve read grants before the policy is written so a newly added grantee
	// is part of the very first policy the bucket ever gets. Unresolvable
	// entries are skipped (not fatal): a data bucket must not lose its own
	// Ready state because a consumer bucket is missing or not provisioned yet.
	readerURNs, grantedTo, err := r.resolveReadGrants(ctx, b)
	if err != nil {
		return failed(fmt.Errorf("resolve read grants: %w", err))
	}

	// applyPolicy writes the isolation policy with the given reader set and keeps
	// status in step with it. status.grantedReadTo documents who can read the
	// bucket, so it is recorded here, next to the write that makes it true, and
	// not only on the terminal path in reconcileNormal — a pending clone returns
	// before that path, and so does any later failure.
	applyPolicy := func(readers, granted []string) error {
		creds.grantedTo = granted
		b.Status.GrantedReadTo = granted
		return r.ensureBucketPolicy(ctx, name, admin, creds.urn, readers)
	}

	// While a clone is still populating the bucket, granted readers stay out of
	// the policy. holdSecretUntilCloned exists so no workload starts against a
	// half-copied bucket, but it only covers the bucket's OWN workload: a granted
	// reader already holds working credentials of its own, so the policy is the
	// only thing that can hold it back. The isolation policy itself is still
	// written first, so the bucket is never open to the rest of the project while
	// it fills up.
	cloning := b.ClonePending()
	if cloning {
		if err := applyPolicy(nil, nil); err != nil {
			return failed(fmt.Errorf("ensure bucket policy: %w", err))
		}
	} else if err := applyPolicy(readerURNs, grantedTo); err != nil {
		return failed(fmt.Errorf("ensure bucket policy: %w", err))
	}

	if verr := validateCloneSource(b, name, host); verr != nil {
		res, rerr := r.failNoRequeue(ctx, b, verr)
		return creds, false, res, rerr
	}

	if cloning {
		if !b.Spec.CloneFrom.HoldSecret() {
			creds.accessKeyID, err = r.ensureAccessKeyAndSecret(ctx, b, creds.gid, host, bucketURL)
			if err != nil {
				return failed(fmt.Errorf("ensure workload credentials: %w", err))
			}
			// Record a just-performed rotation immediately: the terminal status
			// write is not reached while the clone runs, and the pending trigger
			// would otherwise re-rotate on every clone poll. The clone progress
			// updates persist the recorded value.
			r.recordPendingRotation(ctx, b, name)
		}
		done, res, cerr := r.ensureClone(ctx, b, name, endpointURLFromBucketURL(bucketURL, name))
		if !done {
			return creds, false, res, cerr
		}
		// The copy finished in this very pass, so the reader hold above is over:
		// re-apply the policy, now including the granted readers. Without this the
		// grants would only land on the next reconcile.
		if err := applyPolicy(readerURNs, grantedTo); err != nil {
			return failed(fmt.Errorf("ensure bucket policy: %w", err))
		}
	}

	if creds.accessKeyID == "" {
		creds.accessKeyID, err = r.ensureAccessKeyAndSecret(ctx, b, creds.gid, host, bucketURL)
		if err != nil {
			return failed(fmt.Errorf("ensure workload credentials: %w", err))
		}
	}
	return creds, true, ctrl.Result{}, nil
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

// errCredentialDestroyed marks a failure that happened AFTER the workload's live
// access key was deleted and before a replacement was published. Unlike an
// unreachable provider, this is something the operator knows for certain about
// this Bucket: the credential in the Secret no longer works. It therefore drops
// Ready immediately instead of entering the degraded hold — the same treatment
// the failNoRequeue faults get, only established mid-pass rather than up front.
var errCredentialDestroyed = errors.New("workload credential destroyed and not replaced")

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
//
// Errors raised after the clear are wrapped in errCredentialDestroyed: from that
// point the workload's published credential is known dead, which the degraded
// hold must not paper over.
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
		return "", fmt.Errorf("%w: create replacement access key: %w", errCredentialDestroyed, err)
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
		return "", fmt.Errorf("%w: write credentials secret %s: %w", errCredentialDestroyed, secretKey, err)
	}
	return ak.AccessKeyID, nil
}

// recordPendingRotation stamps a just-performed annotation-triggered rotation
// into status (in-memory; persisted by the caller's terminal status write). A
// pending trigger at this point means ensureAccessKeyAndSecret has already
// rotated (its skip path is disabled while a rotation is pending); recording
// the handled value turns the annotation back into a level-triggered no-op. A
// crash before the status write simply rotates again on the next reconcile
// (harmless: hard rotation). No-op when no rotation is pending.
func (r *BucketReconciler) recordPendingRotation(ctx context.Context, b *s3v1.Bucket, name string) {
	trigger := b.PendingRotationTrigger()
	if trigger == "" {
		return
	}
	b.Status.LastRotationTrigger = trigger
	now := metav1.Now()
	b.Status.LastRotationTime = &now
	log.FromContext(ctx).Info("workload credentials rotated", "bucket", name, "trigger", trigger)
	r.event(b, corev1.EventTypeNormal, reasonRotated, "workload access key rotated (rotate-credentials-at annotation)")
}

// reasonReadGrantPending is the event reason emitted when a spec.grantReadAccess
// entry cannot be resolved yet (the referenced Bucket does not exist, is being
// deleted, or has no credentials group). It is informational, not a failure: the
// grant is simply left out of the policy and applied once the reference resolves.
const reasonReadGrantPending = "ReadGrantPending"

// resolveReadGrants turns spec.grantReadAccess into the credentials-group URNs
// that BuildIsolationPolicy grants read-only access to, plus the names of the
// grants that resolved (for status).
//
// SECURITY — where the URN comes from matters more than anything else here.
// The obvious implementation reads status.credentialsGroupURN off the referenced
// Bucket, which would be wrong: status is writable by anyone holding
// buckets/status update in the namespace, a strictly weaker permission than
// reading the workload Secrets. A forged URN would be pasted verbatim into this
// bucket's policy, which allows two concrete attacks:
//
//   - Naming the operator's admin group would confine the admin key to read-only
//     on this bucket, including PutBucketPolicy — an unrepairable lockout.
//   - Naming any other group in the project would hand read access to a
//     principal outside the namespace.
//
// So the URN is never taken from the referenced object. Only the reference's
// *name* is used, and it is used to derive the group display name the operator
// itself would have created (workloadGroupName, deterministic over
// namespace/name), which is then looked up against the control plane. Nothing an
// unprivileged status writer controls reaches the policy. BuildIsolationPolicy
// additionally filters the admin and workload URNs as a second line of defense.
//
// The reference is resolved with the grantor's own namespace, so it can only
// ever address a Bucket in that namespace — a same-named Bucket elsewhere in the
// cluster is invisible here.
//
// LIMIT, stated precisely because the comment above is easy to over-read: what
// reaches the policy is the group located by workloadGroupName, which is
// "s3op-<ns>-<name>" truncated to 23 characters plus an 8-hex FNV-1a-32 suffix.
// Distinct namespace/name pairs can be made to collide on that name, so the
// namespace scoping above is a property of the CR lookup, not a cryptographic
// guarantee about the principal. This is not specific to grants: the same
// derivation already decides which credentials group a Bucket owns and adopts
// (EnsureCredentialsGroup is find-or-create by that name). Any namespace allowed
// to create Bucket CRs is inside the trust boundary.
//
// Unresolvable entries (missing Bucket, Bucket under deletion, no group yet) are
// skipped with a warning event rather than failing the reconcile: the grantor
// owns the data and must not lose its own provisioning because a consumer is
// absent. The grantee watch in SetupWithManager re-queues the grantor as soon as
// the reference materializes, and a deleted grantee drops out of the policy on
// the next reconcile — that is the revocation path.
//
// One control-plane listing serves all entries, so the cost is independent of
// the number of grants.
func (r *BucketReconciler) resolveReadGrants(ctx context.Context, b *s3v1.Bucket) (urns, granted []string, err error) {
	if len(b.Spec.GrantReadAccess) == 0 {
		return nil, nil, nil
	}

	groups, err := r.Stackit.ListCredentialsGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	// STACKIT does not enforce unique credentials-group display names, and the
	// name is the only handle the operator has on a grantee's group. Two rules
	// follow:
	//
	//   - First match wins, matching FindCredentialsGroupByName (stackit/client.go).
	//     A different tie-break here would hand the grant to a group the grantee
	//     itself does not use, i.e. read access to nobody while the intended
	//     reader stays denied.
	//   - A duplicated name is recorded as ambiguous and grants nothing. Guessing
	//     which of two identically named groups the reference meant is exactly the
	//     kind of silent, wrong principal a read grant must never produce.
	urnByName := make(map[string]string, len(groups))
	ambiguous := make(map[string]bool)
	for _, g := range groups {
		if _, dup := urnByName[g.DisplayName]; dup {
			ambiguous[g.DisplayName] = true
			continue
		}
		urnByName[g.DisplayName] = g.URN
	}

	logger := log.FromContext(ctx)
	for _, ref := range b.Spec.GrantReadAccess {
		// A Bucket cannot grant to itself; BuildIsolationPolicy would filter the
		// resulting URN anyway, but skipping here keeps status truthful.
		if ref.Name == b.Name {
			r.event(b, corev1.EventTypeWarning, reasonReadGrantPending,
				fmt.Sprintf("ignoring self-reference %q in spec.grantReadAccess", ref.Name))
			continue
		}

		var grantee s3v1.Bucket
		key := types.NamespacedName{Namespace: b.Namespace, Name: ref.Name}
		if getErr := r.Get(ctx, key, &grantee); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				logger.Info("read grant pending: referenced Bucket not found", "grantee", key)
				r.event(b, corev1.EventTypeWarning, reasonReadGrantPending,
					fmt.Sprintf("Bucket %q referenced in spec.grantReadAccess does not exist (yet); read access not granted", ref.Name))
				continue
			}
			return nil, nil, fmt.Errorf("get referenced Bucket %s: %w", key, getErr)
		}

		// A grantee under deletion is losing its credentials group; revoke early
		// rather than leaving a soon-to-be-dangling principal in the policy.
		if !grantee.DeletionTimestamp.IsZero() {
			r.event(b, corev1.EventTypeWarning, reasonReadGrantPending,
				fmt.Sprintf("Bucket %q referenced in spec.grantReadAccess is being deleted; read access revoked", ref.Name))
			continue
		}

		groupName := workloadGroupName(&grantee)
		if ambiguous[groupName] {
			logger.Info("read grant refused: credentials-group name is ambiguous", "grantee", key, "group", groupName)
			r.event(b, corev1.EventTypeWarning, reasonReadGrantPending,
				fmt.Sprintf("Bucket %q referenced in spec.grantReadAccess resolves to credentials-group name %q, "+
					"which exists more than once in the project; refusing to grant read access to an ambiguous principal",
					ref.Name, groupName))
			continue
		}
		urn, ok := urnByName[groupName]
		if !ok {
			logger.Info("read grant pending: grantee credentials group not provisioned", "grantee", key)
			r.event(b, corev1.EventTypeWarning, reasonReadGrantPending,
				fmt.Sprintf("Bucket %q referenced in spec.grantReadAccess has no credentials group yet; read access not granted", ref.Name))
			continue
		}
		urns = append(urns, urn)
		granted = append(granted, ref.Name)
	}
	return urns, granted, nil
}

// ensureBucketPolicy applies the isolation policy (INIT-SETUP.md §4.1) via the
// admin S3 key, re-writing it only when it drifts from the desired document.
// readerURNs carries the resolved spec.grantReadAccess principals; nil keeps the
// policy at its two original statements.
func (r *BucketReconciler) ensureBucketPolicy(
	ctx context.Context, name string, admin *adminCreds, workloadURN string, readerURNs []string,
) error {
	s3admin, err := r.newS3Admin(ctx, name, admin)
	if err != nil {
		return err
	}
	desired := stackit.BuildIsolationPolicy(name, admin.urn, workloadURN, readerURNs)
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

	// Stop a still-running clone first (job + staging Secret, tolerating their
	// absence), so nothing keeps writing into the bucket while it is torn down.
	if err := r.deleteCloneArtifacts(ctx, b, true); err != nil {
		return err
	}

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

// fail records a failed reconcile and requeues via the returned error so the
// controller retries with backoff.
//
// An already-provisioned Bucket keeps its Ready state instead of dropping to
// Failed while the failure is non-definitive and the grace window has not
// elapsed — see degrade. The error is returned either way, so the retry, the
// Warning event and controller_runtime_reconcile_errors_total are unaffected;
// only the Bucket's advertised health changes.
func (r *BucketReconciler) fail(ctx context.Context, b *s3v1.Bucket, err error) (ctrl.Result, error) {
	if !r.degrade(ctx, b, err) {
		r.markFailed(ctx, b, err)
	}
	// Every failure routed here is non-definitive by construction (definitive
	// faults use failNoRequeue), which is exactly the input the breaker wants.
	if wait, open := r.Breaker.Failure(); open {
		// The breaker has taken over the retry schedule. Returning the error
		// would additionally requeue this Bucket on the workqueue's own backoff
		// and count the same outage once more; log it here so the reason stays
		// in the operator log despite the nil return.
		log.FromContext(ctx).Error(err, "reconcile failed; provider circuit open",
			"bucket", b.EffectiveBucketName(), "retryAfter", wait)
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, err
}

// failNoRequeue records a failed reconcile without returning an error, for
// configuration faults that a retry cannot fix (they re-reconcile on spec change).
//
// These are the definitive faults — a Secret key collision, a secretRef aimed at
// the admin Secret, a wrong region, an invalid composed name, an ownership
// collision, an unusable clone source. Every one of them is a statement about
// this Bucket that the operator established locally, so they always drop Ready
// and never take the degraded path.
func (r *BucketReconciler) failNoRequeue(ctx context.Context, b *s3v1.Bucket, err error) (ctrl.Result, error) {
	r.markFailed(ctx, b, err)
	return ctrl.Result{}, nil
}

// degrade holds a provisioned Bucket's Ready state through a reconcile failure
// that says nothing about the Bucket, recording the degradation in
// ConditionProviderReachable and status.degradedSince instead. It reports
// whether it took ownership of the failure; false means the caller must fall
// back to markFailed.
//
// The classification is by origin, not by parsing the error: everything routed
// here failed while talking to the StackIT API, the S3 data plane or the
// Kubernetes API, and an unrecognised failure of those is treated as "could not
// verify" rather than "verified bad". That default is deliberate. Getting it
// wrong in this direction costs a delayed signal, bounded by the grace window;
// getting it wrong in the other direction marks every Bucket in the cluster
// unhealthy on the first blip, which is the incident this exists to prevent.
func (r *BucketReconciler) degrade(ctx context.Context, b *s3v1.Bucket, err error) bool {
	if !r.holdsReadyThrough(b, err) {
		return false
	}

	now := metav1.Now()
	if b.Status.DegradedSince == nil {
		b.Status.DegradedSince = &now
	} else if now.Sub(b.Status.DegradedSince.Time) >= r.ProviderDegradedGrace {
		// The provider has been unreachable for longer than the operator is
		// willing to vouch for a state it can no longer verify. Hand back to
		// markFailed, keeping degradedSince so the status records when it began.
		return false
	}

	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:    s3v1.ConditionProviderReachable,
		Status:  metav1.ConditionFalse,
		Reason:  s3v1.ReasonProviderUnreachable,
		Message: err.Error(),
	})
	b.Status.Message = err.Error()
	b.Status.OperatorVersion = r.OperatorVersion
	// The same Warning event as a hard failure: the degradation stays as visible
	// in the event stream as it was before, only the condition differs.
	r.event(b, corev1.EventTypeWarning, s3v1.ReasonFailed, err.Error())
	if uerr := r.Status().Update(ctx, b); uerr != nil {
		log.FromContext(ctx).V(1).Info("status update after degradation did not apply", "error", uerr.Error())
	}
	return true
}

// holdsReadyThrough reports whether b's Ready state may survive err.
func (r *BucketReconciler) holdsReadyThrough(b *s3v1.Bucket, err error) bool {
	if r.ProviderDegradedGrace <= 0 {
		return false
	}
	// A Bucket being torn down has no Ready state worth defending, and showing
	// one would hide a teardown blocked by the non-empty data-loss guard.
	if !b.DeletionTimestamp.IsZero() {
		return false
	}
	// A structured refusal is the provider's own answer, not a failure to reach
	// it: a 401/403 from the API, or the token endpoint rejecting the
	// service-account key with 400 invalid_grant. The latter is how a revoked or
	// deleted key surfaces — it never reaches the Object Storage API at all — and
	// masking either for the length of the grace window would blind exactly the
	// response that matters most. A gateway page carrying the same status code
	// does not match, see stackit.ProviderRefused.
	if stackit.ProviderRefused(err) {
		return false
	}
	// The operator deleted this workload's live access key in this very pass and
	// could not publish a replacement. That is local certainty that the credential
	// in the Secret is dead, not an unverifiable provider state, so the Bucket must
	// stop advertising Ready at once.
	if errors.Is(err, errCredentialDestroyed) {
		return false
	}
	// Only a Bucket that has converged on its current spec has a verified Ready
	// state to hold. If the user changed the spec and the new state cannot be
	// reached, Ready=False is the honest answer.
	return b.Status.ObservedGeneration == b.Generation &&
		b.Status.Phase == s3v1.PhaseReady &&
		meta.IsStatusConditionTrue(b.Status.Conditions, s3v1.ConditionReady)
}

// clearDegraded removes the degradation markers after a successful reconcile.
// The condition is removed rather than set to True so that a Bucket which never
// degraded and one which recovered look identical, and so an operator upgrade
// writes nothing to Buckets that are simply healthy.
func clearDegraded(b *s3v1.Bucket) {
	b.Status.DegradedSince = nil
	meta.RemoveStatusCondition(&b.Status.Conditions, s3v1.ConditionProviderReachable)
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
//
// The Bucket watch itself only fires on generation or annotation changes (plus
// create/delete; setting deletionTimestamp bumps the generation): the clone
// feature updates status.clone every poll while a clone runs, and without the
// filter every one of those writes would echo into an immediate re-reconcile,
// turning the poll interval into a hot loop. Clone Jobs are watched so their
// completion re-queues the owning Bucket without waiting for the next poll
// (a cross-namespace owner reference is not permitted, hence the annotation
// mapping).
//
// Buckets are additionally watched a second time, as *grantees*: a Bucket named
// in another Bucket's spec.grantReadAccess wakes that grantor when it appears,
// finishes provisioning its credentials group, or starts being deleted, so a
// read grant is applied and revoked without waiting for the drift resync. That
// watch carries its own narrow predicate — see granteeCredentialsPredicate for
// why ordinary status churn must not pass it.
func (r *BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("bucket-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&s3v1.Bucket{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("bucket").
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.bucketsForSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(isManagedSecret)),
		).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(bucketsForCloneJob),
			builder.WithPredicates(predicate.NewPredicateFuncs(isCloneJob)),
		).
		Watches(
			&s3v1.Bucket{},
			handler.EnqueueRequestsFromMapFunc(r.bucketsGrantingTo),
			builder.WithPredicates(granteeCredentialsPredicate),
		).
		WithOptions(controller.Options{RateLimiter: bucketRateLimiter()}).
		Complete(r)
}

// bucketRateLimiter paces the requeue of failed reconciles.
//
// controller-runtime's default starts the per-item backoff at 5ms and adds a
// fleet-wide allowance of 10 requeues per second. Those numbers are meant for a
// controller whose reconcile is a few calls against the local API server. This
// one calls a rate-limited remote API several times per pass, so on a provider
// outage the default turns every Bucket in the cluster into a retry loop against
// an endpoint that is already refusing traffic — verified on mgmt-p 2026-09-02,
// where a provider-side 503 storm became "rate limit on IP level exceeded"
// eleven minutes later.
//
// Starting at a second and capping at a quarter hour keeps a single transient
// failure cheap (one retry a second later, which is where most of them end)
// while making a persistent one back off to a rate a remote API does not notice.
// The fleet-wide allowance is cut to 1/s with a burst of 5 for the same reason.
//
// Only error requeues pass through here. Watch events use Add and RequeueAfter
// uses AddAfter, neither of which is rate limited, so the drift resync and
// event-driven reconciles keep their timing.
func bucketRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Second, 15*time.Minute),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(1), 5)},
	)
}

// granteeCredentialsPredicate scopes the grantee watch to the events that can
// change a read grant's outcome: a Bucket appearing, disappearing, or gaining
// (or losing) the credentials group its grantors reference.
//
// Filtering updates on status.credentialsGroupURN is what keeps this watch from
// looping. The Bucket watch is otherwise generation/annotation-filtered, so
// status writes do not re-trigger a reconcile; this second watch on the same
// kind would reintroduce exactly that unless it ignores ordinary status churn
// (clone progress in particular writes status every 15s). It also forecloses the
// mutual-grant cycle — two Buckets granting to each other would otherwise wake
// one another on every status write forever.
var granteeCredentialsPredicate = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldB, okOld := e.ObjectOld.(*s3v1.Bucket)
		newB, okNew := e.ObjectNew.(*s3v1.Bucket)
		if !okOld || !okNew {
			return false
		}
		// A deletion that is still blocked by the finalizer only shows up as an
		// update; grantors must see it so they revoke early (resolveReadGrants
		// skips a grantee under deletion).
		if oldB.DeletionTimestamp.IsZero() != newB.DeletionTimestamp.IsZero() {
			return true
		}
		return oldB.Status.CredentialsGroupURN != newB.Status.CredentialsGroupURN
	},
}

// bucketsGrantingTo maps an event on a Bucket to the Buckets in the same
// namespace that name it in spec.grantReadAccess, so a grantor re-reconciles
// (and re-writes its policy) when a grantee is created, provisioned or deleted.
//
// The listing is namespace-scoped, matching the resolution rule in
// resolveReadGrants: a Bucket in another namespace with the same name is not a
// grantee and must not be woken. Like bucketsForSecret this filters in Go rather
// than through a field index — the list is served from the controller's cache
// and a namespace holds few Buckets, so an index would add wiring (and a
// fake-client dependency in tests) for no measurable gain.
func (r *BucketReconciler) bucketsGrantingTo(ctx context.Context, obj client.Object) []ctrl.Request {
	var buckets s3v1.BucketList
	if err := r.List(ctx, &buckets, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing buckets for grant-triggered reconcile",
			"grantee", client.ObjectKeyFromObject(obj))
		return nil
	}
	var reqs []ctrl.Request
	for i := range buckets.Items {
		b := &buckets.Items[i]
		if b.Name == obj.GetName() {
			continue // self-reference: nothing to re-evaluate
		}
		for _, ref := range b.Spec.GrantReadAccess {
			if ref.Name == obj.GetName() {
				reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
					Namespace: b.Namespace, Name: b.Name,
				}})
				break
			}
		}
	}
	return reqs
}

// isCloneJob reports whether an object is a clone Job provisioned by this
// operator (managed-by + component labels). Used to scope the Job watch.
func isCloneJob(obj client.Object) bool {
	labels := obj.GetLabels()
	return labels[managedByLabel] == managedByValue && labels[cloneComponentLabel] == cloneComponentValue
}

// bucketsForCloneJob maps a clone Job event back to its owning Bucket via the
// namespace/name annotations stamped on the Job at creation.
func bucketsForCloneJob(_ context.Context, obj client.Object) []ctrl.Request {
	ann := obj.GetAnnotations()
	ns, name := ann[cloneBucketNamespaceAnnotation], ann[cloneBucketNameAnnotation]
	if ns == "" || name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
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
