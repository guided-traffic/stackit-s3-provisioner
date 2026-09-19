package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// vanish deletes the bucket out of band, the way a console click or a cleanup
// script does: through the provider's own API, behind the operator's back.
func (e *testEnv) vanish(t *testing.T, name string) {
	t.Helper()
	if err := e.r.Stackit.DeleteBucket(context.Background(), name); err != nil {
		t.Fatalf("delete bucket %q out of band: %v", name, err)
	}
	for _, n := range e.fake.BucketNames() {
		if n == name {
			t.Fatalf("premise: bucket %q is still in the cloud", name)
		}
	}
}

// assertReportsMissing checks the whole contract of a reported vanished bucket:
// the Bucket is Failed, Ready is False with the distinct reason, and the
// dedicated condition carries the same reason so the incident is separable from
// a provider outage without parsing a message.
func assertReportsMissing(t *testing.T, b *s3v1.Bucket) {
	t.Helper()
	if b.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed (message %q)", b.Status.Phase, b.Status.Message)
	}
	ready := meta.FindStatusCondition(b.Status.Conditions, s3v1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v, want False", ready)
	}
	if ready.Reason != s3v1.ReasonBucketMissing {
		t.Errorf("Ready reason = %q, want %q", ready.Reason, s3v1.ReasonBucketMissing)
	}
	present := meta.FindStatusCondition(b.Status.Conditions, s3v1.ConditionBucketPresent)
	if present == nil {
		t.Fatalf("no %s condition; conditions = %+v", s3v1.ConditionBucketPresent, b.Status.Conditions)
	}
	if present.Status != metav1.ConditionFalse {
		t.Errorf("%s = %q, want False", s3v1.ConditionBucketPresent, present.Status)
	}
	if present.Reason != s3v1.ReasonBucketMissing {
		t.Errorf("%s reason = %q, want %q", s3v1.ConditionBucketPresent, present.Reason, s3v1.ReasonBucketMissing)
	}
	// A provider outage and a vanished bucket must not look alike: the provider
	// answered here, so nothing may claim it was unreachable.
	if meta.FindStatusCondition(b.Status.Conditions, s3v1.ConditionProviderReachable) != nil {
		t.Errorf("%s condition present; a definitive answer is not an outage", s3v1.ConditionProviderReachable)
	}
	if b.Status.DegradedSince != nil {
		t.Error("status.degradedSince set; the provider answered, so nothing is being held")
	}
}

// TestVanishedBucketIsReportedNotRecreated is the whole point of the guard, and
// it is the exact scenario the operator used to repair silently: an empty bucket
// under the original name, fresh credentials in the workload's Secret, and a
// Ready CR claiming everything is fine while the data is gone.
func TestVanishedBucketIsReportedNotRecreated(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	secretBefore := e.secretData(t, "team-a", "app-data-s3")
	groupBefore := b.Status.CredentialsGroupID

	e.vanish(t, "app-data")
	before := e.snapshot()

	_, err := e.reconcile(t, "team-a", "app-data")
	if err == nil {
		t.Fatal("reconcile succeeded; a vanished bucket must be reported and retried")
	}
	if !strings.Contains(err.Error(), "refusing to re-create") {
		t.Errorf("error = %v, want the refusal to re-create", err)
	}

	// SC1: nothing was created. Not the bucket, not a group, not a key.
	e.assertUntouched(t, before)
	if got := e.fake.BucketNames(); len(got) != 0 {
		t.Errorf("cloud buckets = %v, want none: the bucket must not be re-created", got)
	}

	// SC2: visible without reading logs.
	got := e.getBucket(t, "team-a", "app-data")
	assertReportsMissing(t, got)
	if !strings.Contains(got.Status.Message, "app-data") {
		t.Errorf("status.message = %q, want it to name the bucket", got.Status.Message)
	}
	if !e.rec.hasReason(s3v1.ReasonBucketMissing) {
		t.Errorf("no %s event; events = %+v", s3v1.ReasonBucketMissing, e.rec.events)
	}

	// SC3: the workload keeps the credentials it has. Replacing them would both
	// break the running workload and destroy the evidence.
	if gotSecret := e.secretData(t, "team-a", "app-data-s3"); !equalData(gotSecret, secretBefore) {
		t.Error("workload Secret was rewritten while the bucket was missing")
	}
	if got.Status.CredentialsGroupID != groupBefore {
		t.Errorf("credentialsGroupID = %q, want the unchanged %q", got.Status.CredentialsGroupID, groupBefore)
	}
	if n := e.fake.KeyCountByID(groupBefore); n != 1 {
		t.Errorf("workload group key count = %d, want the original 1", n)
	}
}

// TestVanishedBucketKeepsReportingWithoutRecreating: the guard is not a one-shot.
// Every further pass must reach the same refusal rather than eventually giving up
// and provisioning, which is what a "retry until it works" loop would do.
func TestVanishedBucketKeepsReportingWithoutRecreating(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	e.provision(t, newBucketCR("team-a", "app-data"))
	e.vanish(t, "app-data")

	for i := 1; i <= 5; i++ {
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatalf("pass %d succeeded; the refusal must hold", i)
		}
	}
	if got := e.fake.BucketNames(); len(got) != 0 {
		t.Errorf("cloud buckets = %v, want none after five passes", got)
	}
	assertReportsMissing(t, e.getBucket(t, "team-a", "app-data"))
}

// TestVanishedBucketRecoversWhenItComesBack is SC4: a restored bucket, or the
// operator being pointed back at the right project, heals the Bucket with no
// human action — which is why the guard requeues instead of parking for good.
func TestVanishedBucketRecoversWhenItComesBack(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	tags := e.fake.Tags("app-data")
	group := b.Status.CredentialsGroupID
	secretBefore := e.secretData(t, "team-a", "app-data-s3")

	e.vanish(t, "app-data")
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("premise: the guard did not trip")
	}

	// The bucket comes back exactly as it was — tags included, which is what a
	// restore or a corrected service-account key looks like.
	e.fake.SeedBucket("app-data", tags)

	e.reconcileN(t, "team-a", "app-data", 1)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready after recovery (message %q)", got.Status.Phase, got.Status.Message)
	}
	// Removed, not set to True: a Bucket that recovered and one that never lost
	// its bucket must be indistinguishable.
	if c := meta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionBucketPresent); c != nil {
		t.Errorf("%s condition = %+v, want it removed on recovery", s3v1.ConditionBucketPresent, c)
	}
	// The recovered bucket is the one that was there, so nothing was rotated.
	if got.Status.CredentialsGroupID != group {
		t.Errorf("credentialsGroupID = %q, want the unchanged %q", got.Status.CredentialsGroupID, group)
	}
	if gotSecret := e.secretData(t, "team-a", "app-data-s3"); !equalData(gotSecret, secretBefore) {
		t.Error("workload Secret was rewritten by the recovery")
	}
}

// TestNeverProvisionedBucketStillProvisions is D1/Q2: the guard keys on
// status.resolvedBucketName, which is written only on the success path. The
// resolved-bucket-name annotation is stamped BEFORE any cloud resource exists,
// so keying on it would block first provisioning outright — this pins that it
// does not.
func TestNeverProvisionedBucketStillProvisions(t *testing.T) {
	cr := newBucketCR("team-a", "app-data")
	// The state a crash between the annotation write and the first successful
	// status write leaves behind: intent recorded, nothing provisioned.
	cr.Annotations = map[string]string{s3v1.ResolvedBucketNameAnnotation: "app-data"}

	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	got := e.provision(t, cr)

	if got.Status.ResolvedBucketName != "app-data" {
		t.Errorf("resolvedBucketName = %q, want app-data", got.Status.ResolvedBucketName)
	}
	if names := e.fake.BucketNames(); len(names) != 1 || names[0] != "app-data" {
		t.Errorf("cloud buckets = %v, want [app-data]", names)
	}
}

// TestAllowRecreateRebuildsAndReports is SC5: the standing opt-in re-creates
// without asking, and the re-creation is still reported as the incident it is.
// Without the report, opting in would restore exactly the silent repair the
// guard exists to remove.
func TestAllowRecreateRebuildsAndReports(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	cr := newBucketCR("team-a", "app-data")
	cr.Spec.AllowRecreate = true
	b := e.provision(t, cr)
	oldGroup := b.Status.CredentialsGroupID
	oldSecret := e.secretData(t, "team-a", "app-data-s3")

	e.vanish(t, "app-data")
	before := testutil.ToFloat64(bucketRecreations.WithLabelValues("team-a", "app-data"))

	e.reconcileN(t, "team-a", "app-data", 1)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready (message %q)", got.Status.Phase, got.Status.Message)
	}
	if names := e.fake.BucketNames(); len(names) != 1 || names[0] != "app-data" {
		t.Fatalf("cloud buckets = %v, want the bucket re-created", names)
	}
	// D6: a fresh workload identity, and the old group left standing.
	if got.Status.CredentialsGroupID == oldGroup {
		t.Error("re-created bucket kept the previous credentials group; D6 requires a fresh one")
	}
	if n := e.fake.KeyCountByID(oldGroup); n != 1 {
		t.Errorf("previous group key count = %d, want 1 (left behind for manual cleanup)", n)
	}
	if newSecret := e.secretData(t, "team-a", "app-data-s3"); equalData(newSecret, oldSecret) {
		t.Error("workload Secret was not replaced; the old key cannot reach the new bucket")
	}
	// The report, which is the only thing that tells anybody the data is gone.
	if !e.rec.hasReason(s3v1.ReasonBucketRecreated) {
		t.Errorf("no %s event; events = %+v", s3v1.ReasonBucketRecreated, e.rec.events)
	}
	after := testutil.ToFloat64(bucketRecreations.WithLabelValues("team-a", "app-data"))
	if after != before+1 {
		t.Errorf("stackit_s3_provisioner_bucket_recreated_total = %v, want %v", after, before+1)
	}
	// And the incident does not leave a BucketMissing condition behind on a
	// Bucket that is healthy again.
	if c := meta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionBucketPresent); c != nil {
		t.Errorf("%s condition = %+v, want none after a successful re-creation", s3v1.ConditionBucketPresent, c)
	}
}

// TestAllowRecreateDoesNotRerunACompletedClone is D7: spec.cloneFrom is a
// one-shot, and its terminal state lives in status, which survives an authorised
// re-creation. Re-running it would copy the source into the rebuilt bucket a
// second time, unasked.
func TestAllowRecreateDoesNotRerunACompletedClone(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	cr := e.newCloneBucketCR(t)
	cr.Spec.AllowRecreate = true
	e.seedCloneSource(1)
	e.startClone(t, cr)
	e.finishCloneJob(t, cr, true, "")
	e.reconcileN(t, cr.Namespace, cr.Name, 2)

	done := e.getBucket(t, cr.Namespace, cr.Name)
	if !done.CloneCompleted() {
		t.Fatalf("premise: clone did not complete; status.clone = %+v", done.Status.Clone)
	}

	e.vanish(t, done.Status.ResolvedBucketName)
	e.reconcileN(t, cr.Namespace, cr.Name, 1)

	got := e.getBucket(t, cr.Namespace, cr.Name)
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready (message %q)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.Clone == nil || got.Status.Clone.Phase != s3v1.ClonePhaseCompleted {
		t.Errorf("status.clone = %+v, want the terminal Completed state preserved", got.Status.Clone)
	}
	// A re-run would have created a second clone Job.
	err := e.k8s.Get(context.Background(),
		types.NamespacedName{Namespace: testOpNS, Name: cloneJobName(got)}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("clone Job lookup = %v, want NotFound: a completed clone must not re-run after a re-creation", err)
	}
}

// TestUnreachableProviderNeverTripsTheGuard is SC7, and the decision the whole
// design hangs on: only the provider's own structured JSON 404 means "gone".
// Everything else leaves existence unknown, and unknown must keep taking the
// degraded path — otherwise a gateway hiccup marks every Bucket in the cluster
// as having lost its data and pages somebody.
func TestUnreachableProviderNeverTripsTheGuard(t *testing.T) {
	tests := []struct {
		name   string
		inject func(e *testEnv)
	}{
		{"503 from the control plane", func(e *testEnv) {
			e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
		}},
		{"gateway HTML page carrying 403", func(e *testEnv) {
			e.fake.FailNextRaw("GetBucket", http.StatusForbidden, "text/html", gatewayHTML)
		}},
		{"gateway HTML page carrying 404", func(e *testEnv) {
			// The nastiest one: the right status code with the wrong provenance.
			// An intermediary saying "not found" is not the provider saying it.
			e.fake.FailNextRaw("GetBucket", http.StatusNotFound, "text/html", gatewayHTML)
		}},
		{"404 with an empty body", func(e *testEnv) {
			// An intermediary that drops the body would otherwise look
			// authoritative.
			e.fake.FailNextRaw("GetBucket", http.StatusNotFound, "application/json", "")
		}},
		{"transport failure reaching the provider", func(e *testEnv) {
			// Nothing on the provider side got to decide.
			e.fake.Close()
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.r.ProviderDegradedGrace = defaultGrace

			e.provision(t, newBucketCR("team-a", "app-data"))
			before := e.snapshot()

			tc.inject(e)
			if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
				t.Fatal("reconcile succeeded, want the failure reported for requeue")
			}

			got := e.getBucket(t, "team-a", "app-data")
			if c := meta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionBucketPresent); c != nil {
				t.Fatalf("%s = %+v; a failure to reach the provider is not an absent bucket",
					s3v1.ConditionBucketPresent, c)
			}
			assertHeldReady(t, got)
			// A closed server cannot answer the snapshot calls; the condition
			// check above is the assertion that matters for that case.
			if tc.name != "transport failure reaching the provider" {
				e.assertUntouched(t, before)
			}
		})
	}
}

// TestVanishedBucketDoesNotTripTheBreaker is ADR 0013 D3 applied to this fault.
// After a restart against the wrong project EVERY provisioned Bucket reads as
// absent; routing that through fail() would open the circuit fleet-wide and stop
// every provider call, teardowns included, over an answer the provider gave
// definitively.
func TestVanishedBucketDoesNotTripTheBreaker(t *testing.T) {
	e, _ := withCircuit(t)

	e.provision(t, newBucketCR(circuitNamespace, circuitBucket))
	e.vanish(t, circuitBucket)

	// Comfortably more failures than the breaker's threshold.
	for i := 1; i <= testCircuitThreshold+2; i++ {
		if _, err := e.reconcile(t, circuitNamespace, circuitBucket); err == nil {
			t.Fatalf("pass %d succeeded; the refusal must hold", i)
		}
	}

	if _, open := e.r.Breaker.OpenedAt(); open {
		t.Fatal("the circuit opened on a definitive answer; teardowns across the fleet would now be held")
	}
}

// TestTeardownOfAVanishedBucketCompletes: deleting a CR whose bucket is gone
// must not hang on the finalizer. The credentials group is deliberately left
// behind — without the bucket nothing can attribute it (ADR 0002 D4) — and that
// is reported rather than silently skipped.
func TestTeardownOfAVanishedBucketCompletes(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	group := b.Status.CredentialsGroupID

	e.vanish(t, "app-data")

	if err := e.k8s.Delete(ctx, e.getBucket(t, "team-a", "app-data")); err != nil {
		t.Fatalf("delete bucket CR: %v", err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data"}, &s3v1.Bucket{}); !apierrors.IsNotFound(err) {
		t.Errorf("bucket CR still present (err %v); the finalizer must not hang on an absent bucket", err)
	}
	var sec corev1.Secret
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data-s3"}, &sec); !apierrors.IsNotFound(err) {
		t.Errorf("workload Secret still present (err %v)", err)
	}
	// Left standing, and said so.
	if !e.rec.hasReason(reasonGroupNotAttributable) {
		t.Errorf("no %s event; the orphaned group must be reported, not silently skipped", reasonGroupNotAttributable)
	}
	if n := e.fake.KeyCountByID(group); n != 1 {
		t.Errorf("credentials group key count = %d, want 1 (left behind, cleanup is manual)", n)
	}
}

// equalData compares two Secret data maps.
func equalData(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || string(av) != string(bv) {
			return false
		}
	}
	return true
}

// TestOperatorUpgradeLeavesHealthyBucketsAlone is the rollout case: the guard is
// new, every existing Bucket meets it for the first time on the upgrade, and not
// one of them may be touched by that. It must create nothing, rotate nothing and
// write no condition — a guard that marked healthy Buckets on upgrade would be a
// worse incident than the one it prevents.
func TestOperatorUpgradeLeavesHealthyBucketsAlone(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	group := b.Status.CredentialsGroupID
	secret := e.secretData(t, "team-a", "app-data-s3")
	before := e.snapshot()

	// Several passes, as a rollout plus the drift resync would produce.
	e.reconcileN(t, "team-a", "app-data", 4)

	e.assertUntouched(t, before)
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready (message %q)", got.Status.Phase, got.Status.Message)
	}
	// Absent, not True: a healthy Bucket gets nothing written by the upgrade.
	if c := meta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionBucketPresent); c != nil {
		t.Errorf("%s = %+v on a healthy Bucket, want the condition absent", s3v1.ConditionBucketPresent, c)
	}
	if got.Status.CredentialsGroupID != group {
		t.Errorf("credentialsGroupID = %q, want the unchanged %q", got.Status.CredentialsGroupID, group)
	}
	if now := e.secretData(t, "team-a", "app-data-s3"); !equalData(now, secret) {
		t.Error("workload Secret was rewritten by an ordinary reconcile")
	}
}

// TestUpgradeWithChangedNamingPolicyChecksTheFrozenName is the rollout that could
// have gone badly. The guard reports data loss when a bucket is absent, so it
// must never ask about a name the bucket was not provisioned under. Changing
// bucketNaming between operator versions is exactly how it would: the composed
// name moves, the bucket does not.
//
// The frozen name in status is what forecloses it — the guard only ever runs
// when status.resolvedBucketName is set, and decideBucketName returns that same
// value, so the two cannot disagree.
func TestUpgradeWithChangedNamingPolicyChecksTheFrozenName(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace
	e.r.Naming = s3v1.BucketNaming{Prefix: "old"}

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	if b.Status.ResolvedBucketName != "old-app-data" {
		t.Fatalf("premise: resolvedBucketName = %q, want old-app-data", b.Status.ResolvedBucketName)
	}
	before := e.snapshot()

	// The upgrade ships a different naming policy. A recomposed name would be
	// "new-team-a-app-data", which does not exist in the project.
	e.r.Naming = s3v1.BucketNaming{Prefix: "new", IncludeNamespace: true}

	e.reconcileN(t, "team-a", "app-data", 2)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q (message %q); the guard asked about a recomposed name", got.Status.Phase, got.Status.Message)
	}
	if got.Status.ResolvedBucketName != "old-app-data" {
		t.Errorf("resolvedBucketName = %q, want the frozen old-app-data", got.Status.ResolvedBucketName)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionBucketPresent); c != nil {
		t.Errorf("%s = %+v; the bucket is present under its frozen name", s3v1.ConditionBucketPresent, c)
	}
	e.assertUntouched(t, before)
}

// TestBucketProvisionedBeforeTheNameWasFrozenIsAdopted covers a Bucket
// provisioned by an operator old enough never to have written
// status.resolvedBucketName. The guard skips it — deliberately, because without
// that record there is nothing proving a pass ever completed — so it is adopted
// by its ownership tags exactly as before, and the pass writes the field, which
// puts it under the guard from then on.
func TestBucketProvisionedBeforeTheNameWasFrozenIsAdopted(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	b := e.provision(t, newBucketCR("team-a", "app-data"))

	// Rewind status to what the older operator left behind: a bucket URL, but no
	// frozen name and no annotation.
	stripped := e.getBucket(t, "team-a", "app-data")
	stripped.Status.ResolvedBucketName = ""
	if err := e.k8s.Status().Update(context.Background(), stripped); err != nil {
		t.Fatalf("rewind status: %v", err)
	}
	delete(stripped.Annotations, s3v1.ResolvedBucketNameAnnotation)
	if err := e.k8s.Update(context.Background(), stripped); err != nil {
		t.Fatalf("drop annotation: %v", err)
	}
	before := e.snapshot()

	e.reconcileN(t, "team-a", "app-data", 2)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready (message %q)", got.Status.Phase, got.Status.Message)
	}
	e.assertUntouched(t, before)
	// Adopted, not re-provisioned: the same group as before the rewind.
	if got.Status.CredentialsGroupID != b.Status.CredentialsGroupID {
		t.Errorf("credentialsGroupID = %q, want the adopted %q",
			got.Status.CredentialsGroupID, b.Status.CredentialsGroupID)
	}
	// And the field is back, so the next pass is guarded.
	if got.Status.ResolvedBucketName != "app-data" {
		t.Errorf("resolvedBucketName = %q, want it written back", got.Status.ResolvedBucketName)
	}
}
