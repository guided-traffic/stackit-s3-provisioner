package controller

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// Event reasons emitted by the measurement controller. All of them are warnings:
// a successful measurement is visible in status and in the metrics, and emitting
// an event per bucket per interval would be pure noise.
const (
	// reasonUsageGateDisabled is emitted when a Bucket explicitly asks to be
	// measured while the operator-wide gate is off.
	reasonUsageGateDisabled = "UsageMeasurementDisabled"
	// reasonUsageInvalid is emitted for an unusable spec.usage value.
	reasonUsageInvalid = "UsageConfigInvalid"
	// reasonUsageClamped is emitted when a Bucket's requested interval was raised
	// to the operator's floor.
	reasonUsageClamped = "UsageIntervalClamped"
	// reasonUsageFailed is emitted when a measurement failed.
	reasonUsageFailed = "UsageMeasurementFailed"
	// reasonUsageTruncated is emitted when a measurement hit the object cap and
	// its numbers are therefore a lower bound.
	reasonUsageTruncated = "UsageMeasurementTruncated"
)

// Retry delays for the states a measurement cannot proceed from. They are
// deliberately short compared to the measurement interval: none of them costs a
// listing pass, they only re-check a local precondition.
const (
	// usageNotProvisionedRetry is how long to wait for a Bucket that is not
	// provisioned yet (nothing to measure, and no error either).
	usageNotProvisionedRetry = time.Minute
	// usageFailureRetry bounds how long a failed measurement waits before the
	// next attempt, so a transient S3 error does not cost a full interval.
	usageFailureRetry = 10 * time.Minute
)

// BucketUsageReconciler measures the size of provisioned buckets and writes the
// result — plus the monthly cost estimate derived from it — to the Bucket's
// status.
//
// It is a SEPARATE controller from BucketReconciler on purpose. Measuring means
// listing a bucket end to end, which takes as long as the bucket is large; doing
// it inside the provisioning reconcile would put an unbounded, purely
// informational operation in front of credential and policy management. With its
// own workqueue and its own concurrency limit, a slow measurement delays at most
// other measurements, never a provisioning pass.
//
// It only ever writes status.usage, and it writes it with a merge patch, so it
// cannot clobber the fields the provisioning reconciler owns. The reverse is
// covered by optimistic concurrency: the provisioning reconciler writes status
// with an Update carrying a resourceVersion, so a measurement landing in between
// makes that Update conflict and retry rather than silently dropping the
// measurement.
type BucketUsageReconciler struct {
	client.Client
	Recorder events.EventRecorder

	// Stackit is the StackIT client. Nil in skeleton mode, where nothing is
	// provisioned and there is nothing to measure.
	Stackit *stackit.Client

	// Config is the operator-wide measurement policy.
	Config UsageConfig

	// AdminSecretName / AdminSecretNamespace locate the operator-owned Secret
	// holding the bootstrap S3 admin credentials. Measurement READS that Secret
	// and never creates it: bootstrapping admin credentials is the provisioning
	// reconciler's job, and a measurement must not be able to mint cloud
	// credentials as a side effect.
	AdminSecretName      string
	AdminSecretNamespace string
}

// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets,verbs=get;list;watch
// +kubebuilder:rbac:groups=stackit-bucket.gtrfc.com,resources=buckets/status,verbs=get;update;patch

// Reconcile measures one Bucket if it is due, and otherwise schedules the next
// measurement. It never returns an error: a failed measurement is informational,
// so it must not inflate the reconcile-error signal that provisioning failures
// own, and it must not touch the Bucket's Ready condition. Failures surface in
// status.usage.message, a warning event and the measurement metrics.
func (r *BucketUsageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var b s3v1.Bucket
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// A Bucket being torn down is not measured: the teardown has its own
	// emptiness check, and a listing pass would only race it.
	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	eff := r.Config.effectiveFor(&b)
	if res, stop := r.precheck(ctx, &b, eff); stop {
		return res, nil
	}

	admin, err := r.readAdmin(ctx)
	if err != nil {
		logger.V(1).Info("bucket size measurement waiting for admin credentials", "error", err.Error())
		return ctrl.Result{RequeueAfter: usageNotProvisionedRetry}, nil
	}

	name := b.Status.ResolvedBucketName
	stats, elapsed, err := r.measure(ctx, name, admin, eff)
	if err != nil {
		return r.recordFailure(ctx, &b, name, eff, err), nil
	}

	usageMeasurementDuration.Observe(elapsed.Seconds())
	r.warnAboutMeasurement(&b, name, stats, eff)

	if err := r.patchUsage(ctx, &b, stats, elapsed, eff); err != nil {
		// The measurement is done and its cost is already paid; retry the write
		// soon rather than after a full interval.
		logger.V(1).Info("status patch after measurement did not apply", "error", err.Error())
		return ctrl.Result{RequeueAfter: minDuration(eff.interval, usageFailureRetry)}, nil
	}

	logger.V(1).Info("bucket size measured",
		"bucket", name, "bytes", stats.BillableBytes(), "objects", stats.Objects, "took", elapsed.String())
	return ctrl.Result{RequeueAfter: nextMeasurement(&b, eff.interval)}, nil
}

// precheck decides whether this pass should measure at all. It returns stop=true
// together with the result to return when it should not — because the Bucket is
// not measured, cannot be measured yet, or is simply not due.
//
// Everything it rejects is free: not one of these paths touches the cloud, which
// is what makes the short retry delays affordable.
func (r *BucketUsageReconciler) precheck(
	ctx context.Context, b *s3v1.Bucket, eff effectiveUsage,
) (ctrl.Result, bool) {
	if eff.err != nil {
		// A malformed spec.usage is a configuration fault a retry cannot fix; it
		// parks visibly and re-reconciles on the next spec change.
		r.warn(b, reasonUsageInvalid, eff.err.Error())
		r.logPatchError(ctx, r.patchUsageParked(ctx, b, eff.err.Error()))
		return ctrl.Result{}, true
	}

	if !eff.enabled {
		if eff.requested {
			// Asked for, refused by the operator-wide gate. Silence here would
			// look exactly like a measurement that never runs.
			msg := "bucket size measurement is disabled operator-wide (bucketUsage.enabled=false)"
			r.warn(b, reasonUsageGateDisabled, msg)
			r.logPatchError(ctx, r.patchUsageParked(ctx, b, msg))
			return ctrl.Result{}, true
		}
		// Switched off: drop any previous measurement so a stale size and a stale
		// cost estimate do not linger on the CR.
		r.logPatchError(ctx, r.clearUsage(ctx, b))
		return ctrl.Result{}, true
	}

	// Skeleton mode provisions nothing, so there is nothing to measure.
	if r.Stackit == nil {
		return ctrl.Result{}, true
	}

	if b.Status.ResolvedBucketName == "" || !bucketIsReady(b) {
		// Not provisioned (yet). Re-check cheaply; this costs no cloud call.
		return ctrl.Result{RequeueAfter: usageNotProvisionedRetry}, true
	}

	if wait := untilDue(b, eff.interval); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, true
	}
	return ctrl.Result{}, false
}

// recordFailure reports a measurement that could not complete. The previous
// values stay in place: a stale size is more useful than none, and
// status.usage.message says why it is about to get staler.
func (r *BucketUsageReconciler) recordFailure(
	ctx context.Context, b *s3v1.Bucket, name string, eff effectiveUsage, err error,
) ctrl.Result {
	msg := fmt.Sprintf("measure bucket %q: %v", name, err)
	log.FromContext(ctx).V(1).Info("bucket size measurement failed", "bucket", name, "error", err.Error())
	r.warn(b, reasonUsageFailed, msg)
	usageMeasurementFailures.Inc()
	r.logPatchError(ctx, r.patchUsageMessage(ctx, b, msg))
	return ctrl.Result{RequeueAfter: minDuration(eff.interval, usageFailureRetry)}
}

// warnAboutMeasurement emits the warnings a successful measurement can still
// carry: values that are only a lower bound, and an interval the operator
// refused to honor as asked.
func (r *BucketUsageReconciler) warnAboutMeasurement(
	b *s3v1.Bucket, name string, stats stackit.BucketStats, eff effectiveUsage,
) {
	if stats.Truncated {
		r.warn(b, reasonUsageTruncated, fmt.Sprintf(
			"measurement of bucket %q stopped at the operator's cap of %d objects; "+
				"the reported size and cost are lower bounds",
			name, r.Config.withDefaults().MaxObjects))
	}
	if eff.clamped > 0 {
		r.warn(b, reasonUsageClamped, fmt.Sprintf(
			"spec.usage.interval %s is below the operator's floor; measuring every %s instead",
			eff.clamped, eff.interval))
	}
}

// logPatchError records a status write that did not apply. It is never fatal:
// the next pass recomputes everything from scratch.
func (r *BucketUsageReconciler) logPatchError(ctx context.Context, err error) {
	if err != nil {
		log.FromContext(ctx).V(1).Info("usage status patch did not apply", "error", err.Error())
	}
}

// measure runs one listing pass against the bucket with the admin credentials.
//
// The pass is bounded by a context deadline of one interval: a measurement that
// cannot finish before its own next run is worthless, and leaving it running
// would occupy a measurement slot forever.
func (r *BucketUsageReconciler) measure(
	ctx context.Context, name string, admin *adminCreds, eff effectiveUsage,
) (stackit.BucketStats, time.Duration, error) {
	cfg := r.Config.withDefaults()

	mctx, cancel := context.WithTimeout(ctx, eff.interval)
	defer cancel()

	endpoint, err := r.Stackit.BucketEndpoint(mctx, name)
	if err != nil {
		return stackit.BucketStats{}, 0, fmt.Errorf("bucket endpoint: %w", err)
	}
	s3admin, err := stackit.NewS3Admin(endpoint, admin.accessKeyID, admin.secretAccessKey, r.Stackit.Region())
	if err != nil {
		return stackit.BucketStats{}, 0, fmt.Errorf("build admin S3 client: %w", err)
	}

	started := time.Now()
	stats, err := s3admin.BucketStats(mctx, name, eff.includeVersions, cfg.MaxObjects)
	elapsed := time.Since(started)
	if err != nil {
		return stackit.BucketStats{}, elapsed, err
	}
	return stats, elapsed, nil
}

// readAdmin loads the bootstrap admin credentials from the operator-owned
// Secret. It never creates them: until provisioning has bootstrapped them there
// is nothing provisioned to measure either.
func (r *BucketUsageReconciler) readAdmin(ctx context.Context) (*adminCreds, error) {
	if r.AdminSecretNamespace == "" {
		return nil, fmt.Errorf("operator namespace unknown (set POD_NAMESPACE)")
	}
	key := types.NamespacedName{Name: r.AdminSecretName, Namespace: r.AdminSecretNamespace}
	var sec corev1.Secret
	if err := r.Get(ctx, key, &sec); err != nil {
		return nil, fmt.Errorf("get admin secret %s: %w", key, err)
	}
	ac := adminFromSecret(&sec)
	if ac == nil {
		return nil, fmt.Errorf("admin secret %s is incomplete", key)
	}
	return ac, nil
}

// patchUsage writes a completed measurement and its cost estimate.
func (r *BucketUsageReconciler) patchUsage(
	ctx context.Context, b *s3v1.Bucket, stats stackit.BucketStats, elapsed time.Duration, eff effectiveUsage,
) error {
	cfg := r.Config.withDefaults()
	now := metav1.Now()
	billable := stats.BillableBytes()

	usage := &s3v1.UsageStatus{
		Bytes:               stats.Bytes,
		Objects:             stats.Objects,
		VersionBytes:        stats.VersionBytes,
		VersionObjects:      stats.VersionObjects,
		BillableBytes:       billable,
		HumanReadable:       formatSize(billable, stats.Truncated),
		LastMeasurementTime: &now,
		MeasurementDuration: elapsed.Truncate(time.Millisecond).String(),
		Truncated:           stats.Truncated,
	}
	if cfg.PricePerGBHour > 0 {
		cents := estimateMonthlyCostCents(billable, cfg.PricePerGBHour)
		usage.EstimatedMonthlyCostCents = cents
		usage.EstimatedMonthlyCost = formatCost(cents, cfg.Currency, stats.Truncated)
		usage.Currency = cfg.Currency
	}
	switch {
	case stats.Truncated:
		usage.Message = fmt.Sprintf(
			"measurement stopped at the operator's cap of %d objects; values are lower bounds", cfg.MaxObjects)
	case eff.clamped > 0:
		usage.Message = fmt.Sprintf(
			"requested interval %s raised to the operator's floor of %s", eff.clamped, eff.interval)
	}

	patch := client.MergeFrom(b.DeepCopy())
	b.Status.Usage = usage
	return client.IgnoreNotFound(r.Status().Patch(ctx, b, patch))
}

// patchUsageMessage records why no fresh measurement is available, leaving any
// previous values in place. It is for TRANSIENT trouble: the measurement is
// expected to succeed again, and until it does a stale size is more useful than
// none — the message says how it is aging.
func (r *BucketUsageReconciler) patchUsageMessage(ctx context.Context, b *s3v1.Bucket, msg string) error {
	if b.Status.Usage != nil && b.Status.Usage.Message == msg {
		return nil
	}
	patch := client.MergeFrom(b.DeepCopy())
	if b.Status.Usage == nil {
		b.Status.Usage = &s3v1.UsageStatus{}
	}
	b.Status.Usage.Message = msg
	return client.IgnoreNotFound(r.Status().Patch(ctx, b, patch))
}

// patchUsageParked records that this Bucket will NOT be measured until something
// changes — the operator-wide gate is off, or its spec.usage is unusable — and
// drops any previous values while doing so.
//
// This is the difference to patchUsageMessage: nothing will refresh these
// numbers, so leaving a size and a monthly cost on display would assert a
// currency they no longer have. The message survives to say why they are gone.
func (r *BucketUsageReconciler) patchUsageParked(ctx context.Context, b *s3v1.Bucket, msg string) error {
	parked := &s3v1.UsageStatus{Message: msg}
	if u := b.Status.Usage; u != nil && *u == *parked {
		return nil
	}
	patch := client.MergeFrom(b.DeepCopy())
	b.Status.Usage = parked
	return client.IgnoreNotFound(r.Status().Patch(ctx, b, patch))
}

// clearUsage removes a previous measurement from a Bucket that is no longer
// measured, so nothing stale is displayed as if it were current.
func (r *BucketUsageReconciler) clearUsage(ctx context.Context, b *s3v1.Bucket) error {
	if b.Status.Usage == nil {
		return nil
	}
	patch := client.MergeFrom(b.DeepCopy())
	b.Status.Usage = nil
	return client.IgnoreNotFound(r.Status().Patch(ctx, b, patch))
}

// warn records a Kubernetes Warning event when a recorder is configured. Every
// event this controller emits is a warning: a successful measurement is already
// visible in status and in the metrics, so a Normal event per bucket per
// interval would be pure noise. The note is passed as a %s argument, never as
// the format string.
func (r *BucketUsageReconciler) warn(b *s3v1.Bucket, reason, note string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(b, nil, corev1.EventTypeWarning, reason, "MeasureUsage", "%s", note)
	}
}

// bucketIsReady reports whether a Bucket is provisioned far enough to be
// measured: the bucket exists in the cloud and its policy is in place.
func bucketIsReady(b *s3v1.Bucket) bool {
	return meta.IsStatusConditionTrue(b.Status.Conditions, s3v1.ConditionReady)
}

// untilDue returns how long is left before the next measurement, or 0 when one
// is due now (including a Bucket that was never measured).
func untilDue(b *s3v1.Bucket, interval time.Duration) time.Duration {
	if b.Status.Usage == nil || b.Status.Usage.LastMeasurementTime == nil {
		return 0
	}
	due := b.Status.Usage.LastMeasurementTime.Add(interval)
	if wait := time.Until(due); wait > 0 {
		return wait
	}
	return 0
}

// nextMeasurement is the delay until this Bucket's next measurement: one
// interval plus a small, deterministic per-Bucket skew.
//
// The skew keeps buckets from staying in lock-step forever, which matters
// because they all start together after an operator restart. It is not the
// safety mechanism — that is the controller's concurrency limit, which caps how
// many listings run at once no matter how many come due at the same moment.
func nextMeasurement(b *s3v1.Bucket, interval time.Duration) time.Duration {
	spread := interval / 10
	if spread <= 0 {
		return interval
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(b.Namespace + "/" + b.Name))
	// int64(uint32) is always in range, and the modulus keeps the skew below one
	// tenth of the interval.
	return interval + time.Duration(int64(h.Sum32())%int64(spread))
}

// minDuration returns the smaller of two durations.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// SetupWithManager registers the measurement controller with the manager.
//
// It watches Buckets with the same generation/annotation predicate the
// provisioning controller uses, for the same reason: status writes must not wake
// a controller that writes status itself, or the measurement would re-trigger
// itself in a hot loop. The measurement schedule therefore does not come from
// the watch at all — it comes from the RequeueAfter each pass returns, which
// survives operator restarts because the due time is derived from
// status.usage.lastMeasurementTime rather than from a process-local timer.
func (r *BucketUsageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("bucket-usage-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&s3v1.Bucket{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("bucketusage").
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Config.withDefaults().Concurrency}).
		Complete(r)
}
