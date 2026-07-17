package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

const (
	testSrcBucket = "seed-data"
	testSrcSecret = "seed-creds"
)

// newCloneBucketCR returns a Bucket CR requesting a clone from the fake's S3
// endpoint. The source Secret is created alongside. All clone tests share the
// "team-a" namespace and the CR name "app-data".
func (e *testEnv) newCloneBucketCR(t *testing.T) *s3v1.Bucket {
	t.Helper()
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: testSrcSecret},
		Data: map[string][]byte{
			s3v1.DefaultAccessKeyIDKey:     []byte("src-ak"),
			s3v1.DefaultSecretAccessKeyKey: []byte("src-sk"),
		},
	}
	if err := e.k8s.Create(context.Background(), src); err != nil {
		t.Fatalf("create source secret: %v", err)
	}
	b := newBucketCR("team-a", "app-data")
	b.Spec.CloneFrom = &s3v1.CloneFrom{
		Endpoint:  e.fake.S3.URL,
		Bucket:    testSrcBucket,
		SecretRef: s3v1.CloneSourceSecretRef{Name: testSrcSecret},
	}
	return b
}

// seedCloneSource creates the source bucket with n current objects in the fake
// (each object reports size 1, so the measured total is n bytes).
func (e *testEnv) seedCloneSource(n int) {
	e.fake.SeedBucket(testSrcBucket, nil)
	for i := 0; i < n; i++ {
		e.fake.SeedObject(testSrcBucket, "obj-"+strings.Repeat("x", i+1), "v1", false)
	}
}

// startClone creates the CR and reconciles until the clone job exists,
// returning the fetched job.
func (e *testEnv) startClone(t *testing.T, b *s3v1.Bucket) *batchv1.Job {
	t.Helper()
	if err := e.k8s.Create(context.Background(), b); err != nil {
		t.Fatalf("create bucket CR: %v", err)
	}
	// One reconcile adds the finalizer and provisions up to the running clone.
	res, err := e.reconcile(t, b.Namespace, b.Name)
	if err != nil {
		t.Fatalf("clone-start reconcile: %v", err)
	}
	if res.RequeueAfter != clonePollInterval {
		t.Fatalf("RequeueAfter = %v, want poll interval %v", res.RequeueAfter, clonePollInterval)
	}
	return e.getCloneJob(t, b)
}

func (e *testEnv) getCloneJob(t *testing.T, b *s3v1.Bucket) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	err := e.k8s.Get(context.Background(), types.NamespacedName{Namespace: testOpNS, Name: cloneJobName(b)}, &job)
	if err != nil {
		t.Fatalf("get clone job: %v", err)
	}
	return &job
}

// finishCloneJob marks the clone job as finished (succeeded or failed).
func (e *testEnv) finishCloneJob(t *testing.T, b *s3v1.Bucket, succeeded bool, msg string) {
	t.Helper()
	job := e.getCloneJob(t, b)
	condType := batchv1.JobComplete
	if !succeeded {
		condType = batchv1.JobFailed
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type: condType, Status: corev1.ConditionTrue, Message: msg,
	})
	if err := e.k8s.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("update job status: %v", err)
	}
}

func envValue(t *testing.T, job *batchv1.Job, name string) corev1.EnvVar {
	t.Helper()
	for _, ev := range job.Spec.Template.Spec.Containers[0].Env {
		if ev.Name == name {
			return ev
		}
	}
	t.Fatalf("job env %q missing", name)
	return corev1.EnvVar{}
}

func TestCloneHoldsSecretUntilCompleted(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.seedCloneSource(2)
	b := e.newCloneBucketCR(t)
	job := e.startClone(t, b)

	// Destination bucket exists with its isolation policy before any data flows.
	if got := e.fake.Policy("app-data"); got == "" {
		t.Error("isolation policy not set before clone")
	}

	// The job copies src -> dst with rclone, rc enabled, dest creds from the
	// admin Secret, source creds from the staging Secret.
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{"copy", "src:" + testSrcBucket, "dst:app-data", "--rc"} {
		if !strings.Contains(args, want) {
			t.Errorf("job args %q miss %q", args, want)
		}
	}
	if got := envValue(t, job, "RCLONE_CONFIG_SRC_ENDPOINT").Value; got != e.fake.S3.URL {
		t.Errorf("source endpoint = %q, want %q", got, e.fake.S3.URL)
	}
	if got := envValue(t, job, "RCLONE_CONFIG_DST_ENDPOINT").Value; got != e.fake.S3.URL {
		t.Errorf("dest endpoint = %q, want %q", got, e.fake.S3.URL)
	}
	if got := envValue(t, job, "RCLONE_CONFIG_DST_ACCESS_KEY_ID").ValueFrom.SecretKeyRef.Name; got != testAdminSec {
		t.Errorf("dest access key secret = %q, want admin secret", got)
	}
	if got := envValue(t, job, "RCLONE_RC_PASS").ValueFrom.SecretKeyRef.Name; got != cloneStagingSecretName(b) {
		t.Errorf("rc pass secret = %q, want staging secret", got)
	}

	// Staging Secret: source creds copied, 32-char rc password generated.
	var staging corev1.Secret
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneStagingSecretName(b)}, &staging); err != nil {
		t.Fatalf("get staging secret: %v", err)
	}
	if got := string(staging.Data[cloneSecretKeyAccessKeyID]); got != "src-ak" {
		t.Errorf("staged access key = %q", got)
	}
	if got := len(staging.Data[cloneSecretKeyRcPassword]); got != 32 {
		t.Errorf("rc password length = %d, want 32", got)
	}

	// Workload Secret is held back while the clone runs; status shows progress.
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("workload secret must not exist during clone (err %v)", err)
	}
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseProvisioning || got.Status.Clone == nil {
		t.Fatalf("status during clone = %+v", got.Status)
	}
	if got.Status.Clone.Phase != s3v1.ClonePhaseRunning || got.Status.Clone.TotalBytes != 2 {
		t.Errorf("clone status = %+v, want Running with totalBytes 2", got.Status.Clone)
	}
	if !strings.Contains(got.Status.Message, testSrcBucket) {
		t.Errorf("status message %q misses source bucket", got.Status.Message)
	}
	if !e.rec.hasReason(reasonCloneStarted) {
		t.Errorf("missing %s event; events: %+v", reasonCloneStarted, e.rec.events)
	}

	// Another poll while running changes nothing structurally.
	e.reconcileN(t, "team-a", "app-data", 1)
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("workload secret leaked during clone poll (err %v)", err)
	}

	// Job succeeds -> secret published, Ready, artifacts cleaned up.
	e.finishCloneJob(t, b, true, "")
	e.reconcileN(t, "team-a", "app-data", 1)

	got = e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase after clone = %q (message %q), want Ready", got.Status.Phase, got.Status.Message)
	}
	c := got.Status.Clone
	if c == nil || c.Phase != s3v1.ClonePhaseCompleted || c.CompletedAt == nil {
		t.Fatalf("clone status = %+v, want Completed with timestamp", c)
	}
	if c.Progress != "2 B / 2 B (100%)" {
		t.Errorf("progress = %q, want 100%%", c.Progress)
	}
	cond := findCondition(got.Status.Conditions, s3v1.ConditionCloneCompleted)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != s3v1.ReasonCloned {
		t.Errorf("CloneCompleted condition = %+v", cond)
	}
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &corev1.Secret{}); err != nil {
		t.Errorf("workload secret not published after clone: %v", err)
	}
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneJobName(b)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("clone job not cleaned up (err %v)", err)
	}
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneStagingSecretName(b)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("staging secret not cleaned up (err %v)", err)
	}
	if !e.rec.hasReason(s3v1.ReasonCloned) {
		t.Errorf("missing %s event; events: %+v", s3v1.ReasonCloned, e.rec.events)
	}

	// Terminal: further reconciles never re-run the clone.
	e.reconcileN(t, "team-a", "app-data", 2)
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneJobName(b)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("clone job re-created after completion (err %v)", err)
	}
}

func TestClonePublishesSecretEarlyWhenNotHeld(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.seedCloneSource(1)
	b := e.newCloneBucketCR(t)
	hold := false
	b.Spec.CloneFrom.HoldSecretUntilCloned = &hold
	e.startClone(t, b)

	// Secret is available while the clone still runs, but Ready waits.
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &corev1.Secret{}); err != nil {
		t.Fatalf("workload secret must exist during clone: %v", err)
	}
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseProvisioning {
		t.Errorf("phase during clone = %q, want Provisioning (Ready waits for the clone)", got.Status.Phase)
	}

	e.finishCloneJob(t, b, true, "")
	e.reconcileN(t, "team-a", "app-data", 1)
	got = e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase after clone = %q, want Ready", got.Status.Phase)
	}
}

func TestCloneProgressFromRcStats(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.seedCloneSource(2)
	b := e.newCloneBucketCR(t)

	var gotURL, gotUser, gotPass string
	eta := 390.0
	e.r.cloneStatsFn = func(_ context.Context, baseURL, user, pass string) (*rcloneStats, error) {
		gotURL, gotUser, gotPass = baseURL, user, pass
		return &rcloneStats{Bytes: 1, Speed: 44040192, ETA: &eta}, nil
	}
	e.startClone(t, b)

	// A running clone pod with an IP makes stats polling possible.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testOpNS, Name: cloneJobName(b) + "-abc12",
			Labels: map[string]string{"batch.kubernetes.io/job-name": cloneJobName(b)},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "rclone", Image: "x"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.2.3"},
	}
	if err := e.k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if gotURL != "http://10.1.2.3:5572" || gotUser != cloneRcUser {
		t.Errorf("stats polled at %q as %q", gotURL, gotUser)
	}
	var staging corev1.Secret
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneStagingSecretName(b)}, &staging); err != nil {
		t.Fatal(err)
	}
	if gotPass != string(staging.Data[cloneSecretKeyRcPassword]) {
		t.Error("stats poll used a different rc password than the staging secret")
	}

	got := e.getBucket(t, "team-a", "app-data")
	c := got.Status.Clone
	if c.BytesCopied != 1 || c.Progress != "1 B / 2 B (50%)" {
		t.Errorf("progress = %+v, want 1/2 bytes (50%%)", c)
	}
	if c.Rate != "42.0 MiB/s" {
		t.Errorf("rate = %q, want 42.0 MiB/s", c.Rate)
	}
	if c.ETA != "6m30s" {
		t.Errorf("eta = %q, want 6m30s", c.ETA)
	}
	if !strings.Contains(got.Status.Message, "(50%)") {
		t.Errorf("status message %q misses progress", got.Status.Message)
	}
}

func TestCloneJobFailureIsRetried(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.seedCloneSource(1)
	b := e.newCloneBucketCR(t)
	e.startClone(t, b)

	e.finishCloneJob(t, b, false, "BackoffLimitExceeded")
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile with failed clone job succeeded, want error (backoff retry)")
	}
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed || got.Status.Clone.Phase != s3v1.ClonePhaseFailed {
		t.Errorf("phases = %q/%q, want Failed/Failed", got.Status.Phase, got.Status.Clone.Phase)
	}
	if !e.rec.hasReason(s3v1.ReasonFailed) {
		t.Errorf("missing failure event; events: %+v", e.rec.events)
	}
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneJobName(b)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("failed job not removed (err %v)", err)
	}
	// Workload secret still withheld after a failed attempt.
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("workload secret leaked after clone failure (err %v)", err)
	}

	// Next reconcile starts a fresh attempt; success completes the bucket.
	e.reconcileN(t, "team-a", "app-data", 1)
	e.finishCloneJob(t, b, true, "")
	e.reconcileN(t, "team-a", "app-data", 1)
	if got := e.getBucket(t, "team-a", "app-data"); got.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase after retry = %q, want Ready", got.Status.Phase)
	}
}

func TestCloneGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("missing source secret", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedCloneSource(1)
		b := newBucketCR("team-a", "app-data")
		b.Spec.CloneFrom = &s3v1.CloneFrom{
			Endpoint:  e.fake.S3.URL,
			Bucket:    testSrcBucket,
			SecretRef: s3v1.CloneSourceSecretRef{Name: "does-not-exist"},
		}
		if err := e.k8s.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatal("reconcile without source secret succeeded, want error")
		}
		if got := e.getBucket(t, "team-a", "app-data"); got.Status.Phase != s3v1.PhaseFailed {
			t.Errorf("phase = %q, want Failed", got.Status.Phase)
		}
	})

	t.Run("source secret misses keys", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedCloneSource(1)
		b := e.newCloneBucketCR(t)
		var src corev1.Secret
		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: testSrcSecret}, &src); err != nil {
			t.Fatal(err)
		}
		src.Data = map[string][]byte{"WRONG_KEY": []byte("x")}
		if err := e.k8s.Update(ctx, &src); err != nil {
			t.Fatal(err)
		}
		if err := e.k8s.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatal("reconcile with incomplete source secret succeeded, want error")
		}
		got := e.getBucket(t, "team-a", "app-data")
		if !strings.Contains(got.Status.Message, s3v1.DefaultAccessKeyIDKey) {
			t.Errorf("message %q should name the missing data key", got.Status.Message)
		}
	})

	t.Run("custom source secret keys", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedCloneSource(1)
		src := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "custom-creds"},
			Data:       map[string][]byte{"AK": []byte("custom-ak"), "SK": []byte("custom-sk")},
		}
		if err := e.k8s.Create(ctx, src); err != nil {
			t.Fatal(err)
		}
		b := newBucketCR("team-a", "app-data")
		b.Spec.CloneFrom = &s3v1.CloneFrom{
			Endpoint: e.fake.S3.URL,
			Bucket:   testSrcBucket,
			SecretRef: s3v1.CloneSourceSecretRef{
				Name: "custom-creds",
				Keys: s3v1.CloneSourceSecretKeys{AccessKeyID: "AK", SecretAccessKey: "SK"},
			},
		}
		e.startClone(t, b)
		var staging corev1.Secret
		if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneStagingSecretName(b)}, &staging); err != nil {
			t.Fatal(err)
		}
		if got := string(staging.Data[cloneSecretKeyAccessKeyID]); got != "custom-ak" {
			t.Errorf("staged access key = %q, want custom-ak", got)
		}
	})

	t.Run("virtual-hosted addressing style", func(t *testing.T) {
		e := newTestEnv(t)
		e.seedCloneSource(1)
		b := e.newCloneBucketCR(t)
		b.Spec.CloneFrom.AddressingStyle = s3v1.CloneAddressingVirtualHosted
		if err := e.k8s.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		// The fake's S3 endpoint is an IP, which cannot serve virtual-hosted
		// requests (bucket.<ip> does not resolve) — skip the size measurement by
		// pre-seeding totalBytes and assert the job's addressing config instead.
		fresh := e.getBucket(t, "team-a", "app-data")
		fresh.Status.Clone = &s3v1.CloneStatus{TotalBytes: 1}
		if err := e.k8s.Status().Update(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 1)
		job := e.getCloneJob(t, b)
		if got := envValue(t, job, "RCLONE_CONFIG_SRC_FORCE_PATH_STYLE").Value; got != "false" {
			t.Errorf("SRC_FORCE_PATH_STYLE = %q, want false for virtual-hosted source", got)
		}
		if got := envValue(t, job, "RCLONE_CONFIG_DST_FORCE_PATH_STYLE").Value; got != "true" {
			t.Errorf("DST_FORCE_PATH_STYLE = %q, destination must stay path-style", got)
		}
	})

	t.Run("self-clone is refused", func(t *testing.T) {
		e := newTestEnv(t)
		b := e.newCloneBucketCR(t)
		b.Spec.CloneFrom.Bucket = "app-data" // same endpoint + same bucket
		if err := e.k8s.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		e.reconcileN(t, "team-a", "app-data", 2) // config fault: no requeue error
		got := e.getBucket(t, "team-a", "app-data")
		if got.Status.Phase != s3v1.PhaseFailed || !strings.Contains(got.Status.Message, "itself") {
			t.Errorf("phase/message = %q/%q, want Failed self-clone", got.Status.Phase, got.Status.Message)
		}
	})
}

func TestCloneTeardownRemovesArtifacts(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.seedCloneSource(1)
	b := e.newCloneBucketCR(t)
	e.startClone(t, b)

	// Delete the CR while the clone is still running.
	fresh := e.getBucket(t, "team-a", "app-data")
	if err := e.k8s.Delete(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data"}, &s3v1.Bucket{}); !apierrors.IsNotFound(err) {
		t.Errorf("bucket CR still present (err %v)", err)
	}
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneJobName(b)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("clone job survived teardown (err %v)", err)
	}
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: testOpNS, Name: cloneStagingSecretName(b)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("staging secret survived teardown (err %v)", err)
	}
	for _, name := range e.fake.BucketNames() {
		if name == "app-data" {
			t.Error("destination bucket survived teardown")
		}
	}
}

func TestFetchRcloneCoreStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "operator" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/core/stats" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bytes": 2147483648, "speed": 1048576.0, "eta": 42.0, "transfers": 3,
		})
	}))
	defer srv.Close()

	hc := &http.Client{}
	stats, err := fetchRcloneCoreStats(context.Background(), hc, srv.URL, "operator", "secret")
	if err != nil {
		t.Fatalf("fetchRcloneCoreStats: %v", err)
	}
	if stats.Bytes != 2147483648 || stats.Speed != 1048576.0 || stats.ETA == nil || *stats.ETA != 42.0 {
		t.Errorf("stats = %+v", stats)
	}

	if _, err := fetchRcloneCoreStats(context.Background(), hc, srv.URL, "operator", "wrong"); err == nil {
		t.Error("bad credentials must fail")
	}
}

func TestCloneFormatting(t *testing.T) {
	if got := humanBytes(2147483648); got != "2.0 GiB" {
		t.Errorf("humanBytes(2GiB) = %q", got)
	}
	if got := humanBytes(512); got != "512 B" {
		t.Errorf("humanBytes(512) = %q", got)
	}
	if got := cloneProgress(2147483648, 19327352832); got != "2.0 GiB / 18.0 GiB (11%)" {
		t.Errorf("cloneProgress = %q", got)
	}
	if got := cloneProgress(0, 0); got != "0 B" {
		t.Errorf("cloneProgress without total = %q", got)
	}
	if got := cloneRate(0); got != "" {
		t.Errorf("cloneRate(0) = %q, want empty", got)
	}
	eta := 390.0
	if got := cloneETA(&eta); got != "6m30s" {
		t.Errorf("cloneETA = %q", got)
	}
	if got := cloneETA(nil); got != "" {
		t.Errorf("cloneETA(nil) = %q, want empty", got)
	}
}

func TestCloneJobNameStaysWithinLabelBudget(t *testing.T) {
	b := newBucketCR(strings.Repeat("n", 60), strings.Repeat("m", 60))
	name := cloneJobName(b)
	if len(name) > 52 {
		t.Errorf("cloneJobName length = %d, want <= 52 (job-name pod label budget)", len(name))
	}
	if got := cloneStagingSecretName(b); !strings.HasPrefix(got, name) {
		t.Errorf("staging secret name %q not derived from job name %q", got, name)
	}
	if got := endpointURLFromBucketURL("https://host.example/app-data", "app-data"); got != "https://host.example" {
		t.Errorf("endpointURLFromBucketURL = %q", got)
	}
}

// findCondition returns the condition of the given type, or nil.
func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
