package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// defaultGrace mirrors the operator's shipped --provider-degraded-grace so the
// tests exercise the configuration real deployments run.
const defaultGrace = 30 * time.Minute

// gatewayHTML is the shape of the 2026-08-25 08:13 incident: an intermediary
// answering with an HTML error page, which the SDK surfaces as an ordinary API
// error carrying that page's status code.
const gatewayHTML = "<html>\r\n<head><title>403 Forbidden</title></head>\r\n" +
	"<body>\r\n<center><h1>403 Forbidden</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"

// assertHeldReady checks the whole contract of a held Ready state at once:
// Ready stays True, the phase stays Ready, the degradation is recorded, and
// the reason is visible in the ProviderReachable condition.
func assertHeldReady(t *testing.T, b *s3v1.Bucket) {
	t.Helper()
	if b.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase = %q, want Ready (message %q)", b.Status.Phase, b.Status.Message)
	}
	if !meta.IsStatusConditionTrue(b.Status.Conditions, s3v1.ConditionReady) {
		t.Errorf("Ready condition = %+v, want True", meta.FindStatusCondition(b.Status.Conditions, s3v1.ConditionReady))
	}
	if b.Status.DegradedSince == nil {
		t.Error("status.degradedSince is nil, want the degradation recorded")
	}
	cond := meta.FindStatusCondition(b.Status.Conditions, s3v1.ConditionProviderReachable)
	if cond == nil {
		t.Fatalf("no %s condition; conditions = %+v", s3v1.ConditionProviderReachable, b.Status.Conditions)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("%s = %q, want False", s3v1.ConditionProviderReachable, cond.Status)
	}
	if cond.Reason != s3v1.ReasonProviderUnreachable {
		t.Errorf("%s reason = %q, want %q", s3v1.ConditionProviderReachable, cond.Reason, s3v1.ReasonProviderUnreachable)
	}
}

// TestDegradedHoldsReadyThroughTransientFailures is the core of the change: an
// already-provisioned Bucket must not leave Ready because a provider call
// failed. The three injected failures are the ones actually observed on
// 2026-08-25 — a 5xx, a gateway HTML page, and a control-plane read failing
// mid-flow.
func TestDegradedHoldsReadyThroughTransientFailures(t *testing.T) {
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
		{"500 while listing credentials groups", func(e *testEnv) {
			e.fake.FailNext("ListGroups", http.StatusInternalServerError)
		}},
		{"403 reading ownership tags on the S3 data plane", func(e *testEnv) {
			// A data-plane refusal is not a *oapierror.GenericOpenAPIError, so it
			// never matches the structured-401/403 carve-out: it could as easily
			// be policy propagation lagging as a real refusal, and the grace
			// window bounds the wait either way.
			e.fake.FailNext("S3GetTagging", http.StatusForbidden)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.r.ProviderDegradedGrace = defaultGrace

			e.provision(t, newBucketCR("team-a", "app-data"))

			tc.inject(e)
			if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
				t.Fatal("reconcile succeeded, want the failure to be reported for requeue")
			}

			assertHeldReady(t, e.getBucket(t, "team-a", "app-data"))

			// The degradation must stay as loud in the event stream as a hard
			// failure was before.
			if !e.rec.hasReason(s3v1.ReasonFailed) {
				t.Errorf("no %s event emitted; events: %+v", s3v1.ReasonFailed, e.rec.events)
			}
		})
	}
}

// TestDegradedRecoversOnNextSuccess proves the hold is not sticky beyond the
// failure: once the provider answers again, the Bucket looks untouched.
func TestDegradedRecoversOnNextSuccess(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	e.provision(t, newBucketCR("team-a", "app-data"))

	e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded, want an error")
	}
	assertHeldReady(t, e.getBucket(t, "team-a", "app-data"))

	// Provider is healthy again.
	if _, err := e.reconcile(t, "team-a", "app-data"); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.DegradedSince != nil {
		t.Errorf("status.degradedSince = %v, want nil after recovery", got.Status.DegradedSince)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, s3v1.ConditionProviderReachable); c != nil {
		t.Errorf("%s condition still present after recovery: %+v", s3v1.ConditionProviderReachable, c)
	}
	if got.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}

// TestDegradedGraceExpires proves the hold is bounded. A provider that stays
// unreachable past the grace window must produce a Failed Bucket, otherwise a
// real outage would never be visible in the Bucket's own status.
func TestDegradedGraceExpires(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = time.Hour

	e.provision(t, newBucketCR("team-a", "app-data"))

	e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded, want an error")
	}
	assertHeldReady(t, e.getBucket(t, "team-a", "app-data"))

	// Backdate the degradation past the grace window: the next failure must no
	// longer be held.
	b := e.getBucket(t, "team-a", "app-data")
	old := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	b.Status.DegradedSince = &old
	if err := e.k8s.Status().Update(context.Background(), b); err != nil {
		t.Fatalf("backdate degradedSince: %v", err)
	}

	e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded, want an error")
	}

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed once the grace elapsed", got.Status.Phase)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, s3v1.ConditionReady) {
		t.Error("Ready is still True after the grace elapsed")
	}
	// The record of when it started must survive, so the status still explains
	// how long the Bucket had been degraded before it gave up.
	if got.Status.DegradedSince == nil {
		t.Error("status.degradedSince was cleared; it should record when degradation began")
	}
}

// TestProviderRefusalIsNeverHeld is the security property of the design. A
// structured refusal by the provider must never be masked for the length of the
// grace window, or the fleet looks green while it is dead.
//
// 400 is in the set because that is how a REVOKED service-account key actually
// surfaces: the key flow never reaches the Object Storage API, and the token
// endpoint answers 400 invalid_grant (measured live 2026-08-25). An earlier
// version of this carve-out matched only 401/403 and would have held the entire
// fleet Ready for the full grace after a key revocation.
func TestProviderRefusalIsNeverHeld(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := newTestEnv(t)
			e.r.ProviderDegradedGrace = defaultGrace

			e.provision(t, newBucketCR("team-a", "app-data"))

			// The fake answers injections with a JSON envelope, i.e. a genuine
			// structured API answer — the case that must NOT be held.
			e.fake.FailNext("GetBucket", status)
			if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
				t.Fatal("reconcile succeeded, want an error")
			}

			got := e.getBucket(t, "team-a", "app-data")
			if got.Status.Phase != s3v1.PhaseFailed {
				t.Errorf("phase = %q, want Failed on a structured %d", got.Status.Phase, status)
			}
			if meta.IsStatusConditionTrue(got.Status.Conditions, s3v1.ConditionReady) {
				t.Errorf("Ready still True on a structured %d", status)
			}
			if got.Status.DegradedSince != nil {
				t.Error("a definitive refusal must not be recorded as a degradation")
			}
		})
	}
}

// TestGatewayPageIsHeldEvenWith403 is the counterpart to the test above and the
// reason the classification looks at the body rather than the status code. The
// two cases are indistinguishable by status alone, and treating the gateway page
// as a refusal is what produced the cluster-wide alert storm.
func TestGatewayPageIsHeldEvenWith403(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	e.provision(t, newBucketCR("team-a", "app-data"))

	e.fake.FailNextRaw("GetBucket", http.StatusForbidden, "text/html", gatewayHTML)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded, want an error")
	}
	assertHeldReady(t, e.getBucket(t, "team-a", "app-data"))
}

// TestDegradedDisabledByZeroGrace pins the rollback path: setting the Helm value
// to 0 restores the previous behavior without deploying a different image.
func TestDegradedDisabledByZeroGrace(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = 0

	e.provision(t, newBucketCR("team-a", "app-data"))

	e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded, want an error")
	}

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed with the hold disabled", got.Status.Phase)
	}
	if got.Status.DegradedSince != nil {
		t.Error("status.degradedSince set with the hold disabled")
	}
}

// TestInitialProvisioningFailureIsNotHeld covers the case the ticket explicitly
// carved out: a Bucket that has never been Ready has no verified state to
// defend, so a failure during first provisioning must stay visible immediately.
func TestInitialProvisioningFailureIsNotHeld(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	b := newBucketCR("team-a", "app-data")
	if err := e.k8s.Create(context.Background(), b); err != nil {
		t.Fatalf("create bucket CR: %v", err)
	}
	// Reconcile adds the finalizer and provisions in the SAME pass, so the
	// failure has to be armed before the very first one.
	e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded, want an error")
	}

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase == s3v1.PhaseReady {
		t.Error("phase = Ready for a Bucket that was never provisioned")
	}
	if got.Status.DegradedSince != nil {
		t.Error("a never-Ready Bucket must not enter the degraded hold")
	}
}

// TestSpecChangeFailureIsNotHeld guards the generation check. Once the user asks
// for something new and the operator cannot deliver it, Ready=False is the
// honest answer even though the previous state was verified.
func TestSpecChangeFailureIsNotHeld(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	e.provision(t, newBucketCR("team-a", "app-data"))

	// Change the spec and advance the generation. The controller-runtime fake
	// client does not maintain metadata.generation, so it is set explicitly here
	// to reproduce what a real API server does on a spec write.
	b := e.getBucket(t, "team-a", "app-data")
	b.Spec.GrantReadAccess = []s3v1.LocalBucketReference{{Name: "does-not-matter"}}
	b.Generation = b.Status.ObservedGeneration + 1
	if err := e.k8s.Update(context.Background(), b); err != nil {
		t.Fatalf("update spec: %v", err)
	}

	e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded, want an error")
	}

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.DegradedSince != nil {
		t.Error("a failed reconcile of a CHANGED spec must not be held; Ready was never verified for it")
	}
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}

// TestTeardownFailureIsNotHeld makes sure a Bucket being deleted never reports
// Ready. Holding it would hide a teardown blocked by the non-empty data-loss
// guard, which is exactly the state an operator has to see.
func TestTeardownFailureIsNotHeld(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	b := e.provision(t, newBucketCR("team-a", "app-data"))

	// A non-empty bucket blocks teardown.
	e.fake.SeedObject(b.Status.ResolvedBucketName, "keep-me", "", false)
	if err := e.k8s.Delete(context.Background(), b); err != nil {
		t.Fatalf("delete bucket CR: %v", err)
	}
	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("teardown succeeded, want the data-loss guard to block it")
	}

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase == s3v1.PhaseReady {
		t.Error("phase = Ready while the Bucket is being deleted")
	}
	if got.Status.DegradedSince != nil {
		t.Error("a blocked teardown must not enter the degraded hold")
	}
}

// TestConfigFaultIsNotHeld covers the failNoRequeue family: faults the operator
// established locally about this Bucket are definitive regardless of the grace.
func TestConfigFaultIsNotHeld(t *testing.T) {
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace

	b := e.provision(t, newBucketCR("team-a", "app-data"))

	// Point the CR at a region this single-region operator cannot serve.
	b.Spec.Region = "eu99"
	if err := e.k8s.Update(context.Background(), b); err != nil {
		t.Fatalf("update spec: %v", err)
	}
	if _, err := e.reconcile(t, "team-a", "app-data"); err != nil {
		t.Fatalf("config faults must not requeue-hammer: %v", err)
	}

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed for a config fault", got.Status.Phase)
	}
	if got.Status.DegradedSince != nil {
		t.Error("a config fault must not enter the degraded hold")
	}
}

// TestDestroyedCredentialIsNeverHeld covers the one failure the operator knows
// for certain about the Bucket rather than about the provider: the workload's
// live access key has already been deleted and the replacement could not be
// published. The credential in the Secret is dead, so continuing to advertise
// Ready would be a false green on a Bucket the operator itself just broke.
//
// The path is reachable without any spec change — an annotation-only rotation
// trigger leaves metadata.generation untouched — so the generation guard does
// not catch it and an explicit check is required.
func TestDestroyedCredentialIsNeverHeld(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, e *testEnv, b *s3v1.Bucket)
	}{
		{"rotation requested", func(t *testing.T, e *testEnv, b *s3v1.Bucket) {
			t.Helper()
			// Annotation-only write: generation does not move.
			if b.Annotations == nil {
				b.Annotations = map[string]string{}
			}
			b.Annotations[s3v1.RotateCredentialsAtAnnotation] = "2026-08-25T09:00:00Z"
			if err := e.k8s.Update(context.Background(), b); err != nil {
				t.Fatalf("annotate rotation: %v", err)
			}
		}},
		{"workload secret deleted", func(t *testing.T, e *testEnv, b *s3v1.Bucket) {
			t.Helper()
			sec := &corev1.Secret{}
			key := types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.SecretRef.Name}
			if err := e.k8s.Get(context.Background(), key, sec); err != nil {
				t.Fatalf("get workload secret: %v", err)
			}
			if err := e.k8s.Delete(context.Background(), sec); err != nil {
				t.Fatalf("delete workload secret: %v", err)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.r.ProviderDegradedGrace = defaultGrace

			b := e.provision(t, newBucketCR("team-a", "app-data"))
			tc.prepare(t, e, e.getBucket(t, "team-a", "app-data"))

			// The old key is cleared before the new one is minted, so a failure
			// here leaves the group with zero keys.
			e.fake.FailNext("CreateKey", http.StatusServiceUnavailable)
			if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
				t.Fatal("reconcile succeeded, want an error")
			}

			if n := e.fake.KeyCount(workloadGroupName(b)); n != 0 {
				t.Fatalf("group has %d keys, want 0 — the test did not reach the destroyed-credential path", n)
			}

			got := e.getBucket(t, "team-a", "app-data")
			if meta.IsStatusConditionTrue(got.Status.Conditions, s3v1.ConditionReady) {
				t.Error("Ready is still True although the published credential was destroyed")
			}
			if got.Status.Phase != s3v1.PhaseFailed {
				t.Errorf("phase = %q, want Failed", got.Status.Phase)
			}
			if got.Status.DegradedSince != nil {
				t.Error("a destroyed credential must not be recorded as a provider degradation")
			}
		})
	}
}
