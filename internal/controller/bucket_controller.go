// Package controller contains the Bucket reconciler for the StackIT S3 provisioner.
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// BucketReconciler reconciles a Bucket object against StackIT Object Storage.
//
// The skeleton wires the full controller path — finalizer handling, status
// conditions, event recording and the StackIT client seam — but does not yet
// perform provisioning. The reconcile/finalizer flow to implement is documented
// in INIT-SETUP.md §8; the verified StackIT calls live in package stackit.
type BucketReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Stackit is the StackIT Object Storage client bound to this operator's project.
	// It is nil when the operator runs without a service-account key (skeleton mode);
	// in that case the reconciler keeps the CR in a NotImplemented state instead of
	// touching the cloud.
	Stackit *stackit.Client

	// OperatorVersion is stamped into Bucket status for observability.
	OperatorVersion string
}

// +kubebuilder:rbac:groups=s3.gtrfc.com,resources=buckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=s3.gtrfc.com,resources=buckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=s3.gtrfc.com,resources=buckets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a Bucket towards its desired state. In skeleton mode it only
// manages the finalizer and reports a NotImplemented Ready condition.
func (r *BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var bucket s3v1.Bucket
	if err := r.Get(ctx, req.NamespacedName, &bucket); err != nil {
		// Ignore not-found: the object was deleted after the reconcile was queued.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion: release StackIT resources, then drop the finalizer.
	if !bucket.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&bucket, s3v1.BucketFinalizer) {
			// TODO(operator): release access key -> credentials group -> bucket
			// (only when empty), then delete the credentials Secret. See INIT-SETUP.md §8.
			logger.Info("deleting bucket (skeleton: no StackIT teardown performed)", "bucket", bucket.Spec.BucketName)
			controllerutil.RemoveFinalizer(&bucket, s3v1.BucketFinalizer)
			if err := r.Update(ctx, &bucket); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
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

	// TODO(operator): implement the provisioning flow (CreateBucket ->
	// CreateCredentialsGroup -> CreateAccessKey -> write Secret -> PutBucketPolicy).
	// See INIT-SETUP.md §8 and the verified calls in package stackit.
	if r.Stackit != nil {
		logger.V(1).Info("StackIT client configured but provisioning not implemented yet",
			"project", r.Stackit.ProjectID(), "bucket", bucket.Spec.BucketName)
	}

	meta.SetStatusCondition(&bucket.Status.Conditions, metav1.Condition{
		Type:    s3v1.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  s3v1.ReasonNotImplemented,
		Message: "operator skeleton: StackIT provisioning is not yet implemented",
	})
	bucket.Status.ObservedGeneration = bucket.Generation
	bucket.Status.OperatorVersion = r.OperatorVersion

	if err := r.Status().Update(ctx, &bucket); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&s3v1.Bucket{}).
		Named("bucket").
		Complete(r)
}
