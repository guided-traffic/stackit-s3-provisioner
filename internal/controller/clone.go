package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// DefaultCloneImage is the rclone image used for clone jobs when no
// --clone-image is configured (Helm always sets one explicitly).
const DefaultCloneImage = "rclone/rclone:1.74.4"

const (
	// cloneComponentValue marks clone job pods (app.kubernetes.io/component).
	// The Helm chart's NetworkPolicy selects clone pods by this label.
	cloneComponentLabel = "app.kubernetes.io/component"
	cloneComponentValue = "clone"

	// Annotations on a clone Job linking it back to its owning Bucket CR
	// (annotations, not labels: CR names may exceed the 63-char label limit).
	cloneBucketNamespaceAnnotation = "stackit-bucket.gtrfc.com/bucket-namespace"
	cloneBucketNameAnnotation      = "stackit-bucket.gtrfc.com/bucket-name"

	// cloneRcPort is the rclone remote-control port inside the clone job pod,
	// polled by the operator for transfer stats.
	cloneRcPort = 5572
	// cloneRcUser is the basic-auth user protecting the rc endpoint. The
	// password is generated per clone into the staging Secret.
	cloneRcUser = "operator"

	// clonePollInterval is how often a running clone is re-checked (job state +
	// rc transfer stats into status.clone).
	clonePollInterval = 15 * time.Second

	// cloneJobTTLSeconds cleans up a finished clone job the operator failed to
	// delete itself (e.g. crash between status write and cleanup).
	cloneJobTTLSeconds = 3600

	// cloneJobBackoffLimit is how many pod attempts one clone Job makes before
	// the operator observes it as failed, deletes it and re-creates it with
	// reconcile backoff (rclone resumes: already-copied objects are skipped).
	cloneJobBackoffLimit = 3

	// Data keys inside the clone staging Secret (operator namespace).
	cloneSecretKeyAccessKeyID     = "sourceAccessKeyID"
	cloneSecretKeySecretAccessKey = "sourceSecretAccessKey" // #nosec G101 -- data-key name, not a secret
	cloneSecretKeyRcPassword      = "rcPassword"

	// Event reasons for the clone lifecycle.
	reasonCloneStarted = "CloneStarted"
)

// rcloneStats is the subset of rclone's core/stats response the operator
// surfaces in Bucket status.
type rcloneStats struct {
	// Bytes is the number of bytes transferred so far.
	Bytes int64 `json:"bytes"`
	// Speed is the current transfer rate in bytes/second.
	Speed float64 `json:"speed"`
	// ETA is rclone's estimated seconds to completion (null when unknown).
	ETA *float64 `json:"eta"`
}

// endpointURLFromBucketURL derives the scheme-qualified S3 endpoint URL from a
// bucket's path-style URL by stripping the trailing bucket path segment.
func endpointURLFromBucketURL(bucketURL, bucket string) string {
	return strings.TrimSuffix(bucketURL, "/"+bucket)
}

// cloneJobName derives the deterministic name of a Bucket's clone Job in the
// operator namespace. Same stable-identity scheme as workloadGroupName; the
// budget keeps the name a valid label value (the Job controller stamps it into
// the pods' job-name label) with room for the staging Secret's "-src" suffix.
func cloneJobName(b *s3v1.Bucket) string {
	const maxLen = 52
	suffix := shortHash(b.Namespace + "/" + b.Name)
	base := fmt.Sprintf("s3op-clone-%s-%s", b.Namespace, b.Name)
	if keep := maxLen - len(suffix) - 1; len(base) > keep {
		base = base[:keep]
	}
	return base + "-" + suffix
}

// cloneStagingSecretName is the operator-namespace Secret holding the clone
// source credentials and the rc basic-auth password for a Bucket's clone Job.
func cloneStagingSecretName(b *s3v1.Bucket) string {
	return cloneJobName(b) + "-src"
}

// validateCloneSource rejects a clone spec that copies the bucket onto itself
// (same endpoint host and bucket name) — a configuration fault a retry cannot
// fix. name is the resolved physical destination bucket name, destHost the
// operator's S3 endpoint host.
func validateCloneSource(b *s3v1.Bucket, name, destHost string) error {
	src := b.Spec.CloneFrom
	if src == nil {
		return nil
	}
	if src.Bucket == name && src.EndpointHost() == destHost {
		return fmt.Errorf("cloneFrom points at the bucket itself (%s/%s); refusing to clone", destHost, name)
	}
	return nil
}

// newCloneSourceClient builds the S3 client used to measure the clone source,
// honoring the requested addressing style (path by default, virtual-hosted for
// AWS-style endpoints).
func newCloneSourceClient(src *s3v1.CloneFrom, accessKeyID, secretAccessKey string) (*stackit.S3Admin, error) {
	if src.VirtualHosted() {
		return stackit.NewS3VirtualHosted(src.EndpointURL(), accessKeyID, secretAccessKey, src.Region)
	}
	return stackit.NewS3Admin(src.EndpointURL(), accessKeyID, secretAccessKey, src.Region)
}

// cloneSourceCreds reads the clone-source S3 credentials from the Secret
// referenced by spec.cloneFrom.secretRef, which must live in the Bucket's own
// namespace (see CloneSourceSecretRef).
func (r *BucketReconciler) cloneSourceCreds(ctx context.Context, b *s3v1.Bucket) (accessKeyID, secretAccessKey string, err error) {
	ref := b.Spec.CloneFrom.SecretRef
	key := types.NamespacedName{Namespace: b.Namespace, Name: ref.Name}
	var sec corev1.Secret
	if err := r.Get(ctx, key, &sec); err != nil {
		return "", "", fmt.Errorf("get clone source secret %s: %w", key, err)
	}
	accessKeyID = string(sec.Data[ref.Keys.AccessKeyIDKey()])
	secretAccessKey = string(sec.Data[ref.Keys.SecretAccessKeyKey()])
	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", fmt.Errorf("clone source secret %s misses data key %q or %q",
			key, ref.Keys.AccessKeyIDKey(), ref.Keys.SecretAccessKeyKey())
	}
	return accessKeyID, secretAccessKey, nil
}

// ensureClone drives the one-shot clone of the source bucket into the freshly
// provisioned destination bucket via an rclone Job in the operator namespace.
// It returns done=true when no clone work is left (completed now or earlier)
// so reconcileNormal continues with credential provisioning; otherwise the
// returned result/error terminate the current reconcile (polling requeue while
// the job runs, backoff error when it failed).
//
// Crash-safety: the terminal Completed state is persisted to status BEFORE the
// job and staging Secret are removed, so the clone never re-runs after a crash
// mid-cleanup (leftovers are removed by the job TTL and teardown). A crash
// before that write simply re-observes the finished job on the next reconcile.
func (r *BucketReconciler) ensureClone(ctx context.Context, b *s3v1.Bucket, name, destEndpointURL string) (bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	src := b.Spec.CloneFrom

	srcAK, srcSK, err := r.cloneSourceCreds(ctx, b)
	if err != nil {
		res, rerr := r.fail(ctx, b, err)
		return false, res, rerr
	}

	if b.Status.Clone == nil {
		b.Status.Clone = &s3v1.CloneStatus{}
	}

	// Measure the source size once, before the copy starts, so the progress
	// percentage keeps a stable denominator (rclone's own totalBytes grows
	// while it scans).
	if b.Status.Clone.TotalBytes == 0 {
		srcS3, err := newCloneSourceClient(src, srcAK, srcSK)
		if err != nil {
			res, rerr := r.fail(ctx, b, fmt.Errorf("build clone source client: %w", err))
			return false, res, rerr
		}
		total, err := srcS3.BucketUsage(ctx, src.Bucket)
		if err != nil {
			res, rerr := r.fail(ctx, b, fmt.Errorf("measure clone source %q at %s: %w", src.Bucket, src.EndpointHost(), err))
			return false, res, rerr
		}
		b.Status.Clone.TotalBytes = total
	}

	rcPass, err := r.ensureCloneStagingSecret(ctx, b, srcAK, srcSK)
	if err != nil {
		res, rerr := r.fail(ctx, b, fmt.Errorf("ensure clone staging secret: %w", err))
		return false, res, rerr
	}

	jobKey := types.NamespacedName{Namespace: r.AdminSecretNamespace, Name: cloneJobName(b)}
	var job batchv1.Job
	getErr := r.Get(ctx, jobKey, &job)
	switch {
	case apierrors.IsNotFound(getErr):
		newJob := r.buildCloneJob(b, name, destEndpointURL, jobKey.Name, cloneStagingSecretName(b))
		if err := r.Create(ctx, newJob); err != nil {
			res, rerr := r.fail(ctx, b, fmt.Errorf("create clone job %s: %w", jobKey, err))
			return false, res, rerr
		}
		logger.Info("clone job created", "job", jobKey.String(), "source", src.EndpointHost()+"/"+src.Bucket, "bucket", name)
		r.event(b, corev1.EventTypeNormal, reasonCloneStarted,
			fmt.Sprintf("cloning %s from %s (%s)", src.Bucket, src.EndpointHost(), humanBytes(b.Status.Clone.TotalBytes)))
		r.updateCloneProgress(ctx, b, nil)
		return false, ctrl.Result{RequeueAfter: clonePollInterval}, nil
	case getErr != nil:
		res, rerr := r.fail(ctx, b, fmt.Errorf("get clone job %s: %w", jobKey, getErr))
		return false, res, rerr
	}

	succeeded, failed, failMsg := cloneJobFinished(&job)
	switch {
	case succeeded:
		return r.completeClone(ctx, b, &job)
	case failed:
		// Remove the failed Job so the next (backoff-delayed) reconcile creates
		// a fresh attempt; rclone resumes, already-copied objects are skipped.
		if err := r.deleteCloneArtifacts(ctx, b, false); err != nil {
			logger.Error(err, "failed to delete failed clone job", "job", jobKey.String())
		}
		b.Status.Clone.Phase = s3v1.ClonePhaseFailed
		b.Status.Clone.Message = failMsg
		meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
			Type:    s3v1.ConditionCloneCompleted,
			Status:  metav1.ConditionFalse,
			Reason:  s3v1.ReasonCloneFailed,
			Message: failMsg,
		})
		res, rerr := r.fail(ctx, b, fmt.Errorf("clone job failed: %s", failMsg))
		return false, res, rerr
	}

	// Still running: refresh transfer stats from the pod's rclone rc endpoint.
	// Stats are best-effort — an unreachable rc (pod still starting) keeps the
	// previous numbers and polls again.
	stats, err := r.pollCloneStats(ctx, jobKey.Name, rcPass)
	if err != nil {
		logger.V(1).Info("clone stats unavailable", "job", jobKey.String(), "error", err.Error())
	}
	r.updateCloneProgress(ctx, b, stats)
	return false, ctrl.Result{RequeueAfter: clonePollInterval}, nil
}

// completeClone persists the terminal clone state and cleans up the job and
// staging Secret (best effort; the job TTL and teardown catch leftovers).
func (r *BucketReconciler) completeClone(ctx context.Context, b *s3v1.Bucket, job *batchv1.Job) (bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	c := b.Status.Clone
	now := metav1.Now()
	c.Phase = s3v1.ClonePhaseCompleted
	if c.StartedAt == nil {
		c.StartedAt = job.Status.StartTime
	}
	c.CompletedAt = &now
	c.BytesCopied = c.TotalBytes
	c.Progress = cloneProgress(c.TotalBytes, c.TotalBytes)
	c.Rate = ""
	c.ETA = ""
	c.Message = ""
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:    s3v1.ConditionCloneCompleted,
		Status:  metav1.ConditionTrue,
		Reason:  s3v1.ReasonCloned,
		Message: fmt.Sprintf("cloned %s from %s", b.Spec.CloneFrom.Bucket, b.Spec.CloneFrom.EndpointHost()),
	})
	if err := r.Status().Update(ctx, b); err != nil {
		// The Completed state must be durable before the job is removed,
		// otherwise a crash here would re-run the clone from scratch.
		res, rerr := r.fail(ctx, b, fmt.Errorf("persist clone completion: %w", err))
		return false, res, rerr
	}
	if err := r.deleteCloneArtifacts(ctx, b, true); err != nil {
		logger.Error(err, "failed to clean up clone artifacts", "bucket", b.Name)
	}
	logger.Info("clone completed", "bucket", b.Name, "source", b.Spec.CloneFrom.Bucket, "bytes", c.TotalBytes)
	r.event(b, corev1.EventTypeNormal, s3v1.ReasonCloned,
		fmt.Sprintf("cloned %s (%s) from %s", b.Spec.CloneFrom.Bucket, humanBytes(c.TotalBytes), b.Spec.CloneFrom.EndpointHost()))
	return true, ctrl.Result{}, nil
}

// deleteCloneArtifacts removes the clone Job (with its pods) and, when
// includeStaging is set, the staging Secret. Absence is tolerated, so it is
// safe to call from teardown for Buckets that never cloned.
func (r *BucketReconciler) deleteCloneArtifacts(ctx context.Context, b *s3v1.Bucket, includeStaging bool) error {
	propagation := metav1.DeletePropagationBackground
	job := &batchv1.Job{}
	job.Name = cloneJobName(b)
	job.Namespace = r.AdminSecretNamespace
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete clone job %s/%s: %w", job.Namespace, job.Name, err)
	}
	if !includeStaging {
		return nil
	}
	sec := &corev1.Secret{}
	sec.Name = cloneStagingSecretName(b)
	sec.Namespace = r.AdminSecretNamespace
	if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete clone staging secret %s/%s: %w", sec.Namespace, sec.Name, err)
	}
	return nil
}

// ensureCloneStagingSecret creates or updates the operator-namespace Secret
// backing the clone Job: the source credentials (copied from the user's source
// Secret, which the Job cannot mount across namespaces) plus a generated
// basic-auth password for rclone's rc endpoint. The password is created once
// and kept stable so a restarting job pod and the polling operator agree on it.
func (r *BucketReconciler) ensureCloneStagingSecret(ctx context.Context, b *s3v1.Bucket, srcAK, srcSK string) (string, error) {
	sec := &corev1.Secret{}
	sec.Name = cloneStagingSecretName(b)
	sec.Namespace = r.AdminSecretNamespace
	var rcPass string
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.Labels == nil {
			sec.Labels = map[string]string{}
		}
		sec.Labels[managedByLabel] = managedByValue
		sec.Labels[cloneComponentLabel] = cloneComponentValue
		sec.Type = corev1.SecretTypeOpaque
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[cloneSecretKeyAccessKeyID] = []byte(srcAK)
		sec.Data[cloneSecretKeySecretAccessKey] = []byte(srcSK)
		if len(sec.Data[cloneSecretKeyRcPassword]) == 0 {
			pass, err := randomHex32()
			if err != nil {
				return fmt.Errorf("generate rc password: %w", err)
			}
			sec.Data[cloneSecretKeyRcPassword] = []byte(pass)
		}
		rcPass = string(sec.Data[cloneSecretKeyRcPassword])
		return nil
	})
	if err != nil {
		return "", err
	}
	return rcPass, nil
}

// buildCloneJob assembles the rclone Job copying the clone source into the
// destination bucket. It runs in the operator namespace so neither the source
// copy nor the admin credential ever materialize in a workload namespace:
// the destination side authenticates with the operator's admin S3 key
// (mounted straight from the admin Secret), the source side with the staged
// copy of the user-provided credentials. rclone is configured entirely via
// environment (RCLONE_CONFIG_<REMOTE>_*), and --rc exposes transfer stats on
// cloneRcPort, guarded by basic auth from the staging Secret.
func (r *BucketReconciler) buildCloneJob(b *s3v1.Bucket, destBucket, destEndpointURL, jobName, stagingName string) *batchv1.Job {
	src := b.Spec.CloneFrom
	secretEnv := func(name, secretName, key string) corev1.EnvVar {
		return corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  key,
				},
			},
		}
	}
	env := []corev1.EnvVar{
		{Name: "RCLONE_CONFIG_SRC_TYPE", Value: "s3"},
		{Name: "RCLONE_CONFIG_SRC_PROVIDER", Value: "Other"},
		{Name: "RCLONE_CONFIG_SRC_ENDPOINT", Value: src.EndpointURL()},
		{Name: "RCLONE_CONFIG_SRC_FORCE_PATH_STYLE", Value: strconv.FormatBool(!src.VirtualHosted())},
		secretEnv("RCLONE_CONFIG_SRC_ACCESS_KEY_ID", stagingName, cloneSecretKeyAccessKeyID),
		secretEnv("RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY", stagingName, cloneSecretKeySecretAccessKey),
		{Name: "RCLONE_CONFIG_DST_TYPE", Value: "s3"},
		{Name: "RCLONE_CONFIG_DST_PROVIDER", Value: "Other"},
		{Name: "RCLONE_CONFIG_DST_ENDPOINT", Value: destEndpointURL},
		{Name: "RCLONE_CONFIG_DST_REGION", Value: r.Stackit.Region()},
		{Name: "RCLONE_CONFIG_DST_FORCE_PATH_STYLE", Value: "true"},
		secretEnv("RCLONE_CONFIG_DST_ACCESS_KEY_ID", r.AdminSecretName, adminSecretKeyAccessKeyID),
		secretEnv("RCLONE_CONFIG_DST_SECRET_ACCESS_KEY", r.AdminSecretName, adminSecretKeySecretAccessKey),
		{Name: "RCLONE_RC_USER", Value: cloneRcUser},
		secretEnv("RCLONE_RC_PASS", stagingName, cloneSecretKeyRcPassword),
	}
	if src.Region != "" {
		env = append(env, corev1.EnvVar{Name: "RCLONE_CONFIG_SRC_REGION", Value: src.Region})
	}

	labels := map[string]string{
		managedByLabel:      managedByValue,
		cloneComponentLabel: cloneComponentValue,
	}
	annotations := map[string]string{
		cloneBucketNamespaceAnnotation: b.Namespace,
		cloneBucketNameAnnotation:      b.Name,
	}

	falseVal, trueVal := false, true
	uid := int64(65534)
	backoff := int32(cloneJobBackoffLimit)
	ttl := int32(cloneJobTTLSeconds)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   r.AdminSecretNamespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &trueVal,
						RunAsUser:      &uid,
						RunAsGroup:     &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "rclone",
						Image: r.cloneImage(),
						Args: []string{
							"copy", "src:" + src.Bucket, "dst:" + destBucket,
							"--rc", fmt.Sprintf("--rc-addr=:%d", cloneRcPort),
							"--stats=10s", "--s3-no-check-bucket",
						},
						Env:       env,
						Ports:     []corev1.ContainerPort{{Name: "rc", ContainerPort: cloneRcPort, Protocol: corev1.ProtocolTCP}},
						Resources: r.CloneJobResources,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							ReadOnlyRootFilesystem:   &trueVal,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// cloneImage returns the configured clone job image, falling back to the
// pinned default (production sets it via --clone-image / Helm).
func (r *BucketReconciler) cloneImage() string {
	if r.CloneImage != "" {
		return r.CloneImage
	}
	return DefaultCloneImage
}

// cloneJobFinished inspects a Job's terminal conditions.
func cloneJobFinished(job *batchv1.Job) (succeeded, failed bool, failMsg string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false, ""
		case batchv1.JobFailed:
			msg := c.Message
			if msg == "" {
				msg = c.Reason
			}
			return false, true, msg
		}
	}
	return false, false, ""
}

// pollCloneStats locates the clone job's running pod and queries rclone's
// remote-control API for transfer statistics.
func (r *BucketReconciler) pollCloneStats(ctx context.Context, jobName, rcPass string) (*rcloneStats, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(r.AdminSecretNamespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": jobName},
	); err != nil {
		return nil, fmt.Errorf("list clone job pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		baseURL := "http://" + net.JoinHostPort(p.Status.PodIP, strconv.Itoa(cloneRcPort))
		if r.cloneStatsFn != nil {
			return r.cloneStatsFn(ctx, baseURL, cloneRcUser, rcPass)
		}
		return fetchRcloneCoreStats(ctx, r.cloneHTTPClient(), baseURL, cloneRcUser, rcPass)
	}
	return nil, fmt.Errorf("no running clone pod for job %q", jobName)
}

// cloneHTTPClient returns the short-timeout HTTP client for rc stats polls.
func (r *BucketReconciler) cloneHTTPClient() *http.Client {
	r.cloneHTTPOnce.Do(func() {
		r.cloneHTTP = &http.Client{Timeout: 5 * time.Second}
	})
	return r.cloneHTTP
}

// fetchRcloneCoreStats POSTs to rclone's core/stats rc endpoint and decodes
// the transfer statistics.
func fetchRcloneCoreStats(ctx context.Context, hc *http.Client, baseURL, user, pass string) (*rcloneStats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/core/stats", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rc core/stats: unexpected status %d", resp.StatusCode)
	}
	var stats rcloneStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("decode rc core/stats: %w", err)
	}
	return &stats, nil
}

// updateCloneProgress refreshes status.clone (and the coarse phase/message)
// while the clone runs. Best effort like markProvisioning: a failed write is
// logged and retried implicitly on the next poll.
func (r *BucketReconciler) updateCloneProgress(ctx context.Context, b *s3v1.Bucket, stats *rcloneStats) {
	c := b.Status.Clone
	c.Phase = s3v1.ClonePhaseRunning
	if c.StartedAt == nil {
		now := metav1.Now()
		c.StartedAt = &now
	}
	c.Message = ""
	if stats != nil {
		c.BytesCopied = stats.Bytes
		c.Rate = cloneRate(stats.Speed)
		c.ETA = cloneETA(stats.ETA)
	}
	c.Progress = cloneProgress(c.BytesCopied, c.TotalBytes)
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type:    s3v1.ConditionCloneCompleted,
		Status:  metav1.ConditionFalse,
		Reason:  s3v1.ReasonCloning,
		Message: fmt.Sprintf("cloning %s from %s", b.Spec.CloneFrom.Bucket, b.Spec.CloneFrom.EndpointHost()),
	})
	b.Status.Phase = s3v1.PhaseProvisioning
	b.Status.Message = fmt.Sprintf("cloning from %s/%s: %s",
		b.Spec.CloneFrom.EndpointHost(), b.Spec.CloneFrom.Bucket, c.Progress)
	if err := r.Status().Update(ctx, b); err != nil {
		log.FromContext(ctx).V(1).Info("clone progress status update did not apply", "error", err.Error())
	}
}

// randomHex32 returns a 32-character hex string from 16 bytes of
// crypto/rand entropy (the rc basic-auth password).
func randomHex32() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// humanBytes renders a byte count in binary units ("18.0 GiB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// cloneProgress renders "copied / total (pct%)"; without a known total it
// degrades to the copied amount alone.
func cloneProgress(copied, total int64) string {
	if total <= 0 {
		return humanBytes(copied)
	}
	pct := copied * 100 / total
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("%s / %s (%d%%)", humanBytes(copied), humanBytes(total), pct)
}

// cloneRate renders a transfer rate in bytes/second ("42.0 MiB/s").
func cloneRate(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return ""
	}
	return humanBytes(int64(bytesPerSecond)) + "/s"
}

// cloneETA renders rclone's ETA seconds as a duration ("6m30s").
func cloneETA(eta *float64) string {
	if eta == nil || *eta < 0 {
		return ""
	}
	return (time.Duration(*eta) * time.Second).String()
}
