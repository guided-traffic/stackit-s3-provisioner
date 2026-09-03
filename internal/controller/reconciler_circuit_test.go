package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// testCircuitThreshold matches the shipped default, so these tests exercise the
// configuration real deployments run.
const testCircuitThreshold = 3

// The breaker is fleet-wide, so which Bucket trips it never matters to what is
// under test; these tests always trip it with the same one.
const (
	circuitNamespace = "team-a"
	circuitBucket    = "app-data"
)

// withCircuit returns an env whose reconciler runs the provider circuit breaker
// and the shipped degradation grace.
func withCircuit(t *testing.T) (*testEnv, *fakeClock) {
	t.Helper()
	e := newTestEnv(t)
	e.r.ProviderDegradedGrace = defaultGrace
	e.r.Breaker = NewProviderBreaker(testCircuitThreshold, time.Minute)
	// Seeded at wall-clock now so cooldowns can be skipped without drifting away
	// from the timestamps the reconciler writes into status.
	clk := newFakeClockAt(time.Now())
	e.r.Breaker.now = clk.Now
	return e, clk
}

// failGetBucket arms one control-plane 503, the shape of the mgmt-p outage on
// 2026-09-02.
func (e *testEnv) failGetBucket() {
	e.fake.FailNext("GetBucket", http.StatusServiceUnavailable)
}

// tripCircuit drives the breaker open with the configured number of consecutive
// failing reconciles on the named Bucket.
func tripCircuit(t *testing.T, e *testEnv) {
	t.Helper()
	for i := 1; i <= testCircuitThreshold; i++ {
		e.failGetBucket()
		res, err := e.reconcile(t, circuitNamespace, circuitBucket)
		if i < testCircuitThreshold {
			if err == nil {
				t.Fatalf("failure %d: reconcile succeeded, want the error reported while the circuit is still closed", i)
			}
			continue
		}
		// The failure that opens the breaker hands the retry schedule over: the
		// error must no longer be returned, or the outage would additionally be
		// requeued on the workqueue's backoff and counted a second time.
		if err != nil {
			t.Fatalf("failure %d returned %v; the trip must take over the retry instead of reporting", i, err)
		}
		if res.RequeueAfter != circuitBaseCooldown {
			t.Fatalf("requeue after the trip = %v, want the base cooldown %v", res.RequeueAfter, circuitBaseCooldown)
		}
	}
}

// TestProviderCircuitStopsHammeringTheProvider is the point of the whole
// mechanism: once the breaker is open, a reconcile must not reach the provider
// at all. On mgmt-p on 2026-09-02 the operator kept calling into a 503 storm
// until the provider answered "rate limit on IP level exceeded", which extended
// the outage it was reacting to.
func TestProviderCircuitStopsHammeringTheProvider(t *testing.T) {
	e, _ := withCircuit(t)
	e.provision(t, newBucketCR("team-a", "app-data"))
	tripCircuit(t, e)

	before := e.fake.Calls("GetBucket")
	for range 5 {
		res, err := e.reconcile(t, "team-a", "app-data")
		if err != nil {
			t.Fatalf("held reconcile returned %v, want no error while the circuit is open", err)
		}
		if res.RequeueAfter <= 0 {
			t.Fatal("a held reconcile must requeue on the breaker's cooldown")
		}
	}
	if got := e.fake.Calls("GetBucket"); got != before {
		t.Errorf("provider saw %d extra calls while the circuit was open, want 0", got-before)
	}
}

// TestProviderCircuitHoldsReadyWithoutChurn pins that a held Bucket keeps its
// Ready state and that the hold is recorded exactly once. Rewriting the same
// degradation on every probe interval for every Bucket is status churn that
// buys nothing.
func TestProviderCircuitHoldsReadyWithoutChurn(t *testing.T) {
	e, _ := withCircuit(t)
	e.provision(t, newBucketCR("team-a", "app-data"))
	tripCircuit(t, e)

	held := e.getBucket(t, "team-a", "app-data")
	assertHeldReady(t, held)

	rv := held.ResourceVersion
	for range 3 {
		if _, err := e.reconcile(t, "team-a", "app-data"); err != nil {
			t.Fatalf("held reconcile: %v", err)
		}
	}
	again := e.getBucket(t, "team-a", "app-data")
	assertHeldReady(t, again)
	if again.ResourceVersion != rv {
		t.Errorf("resourceVersion moved %s -> %s; a repeated hold must write nothing", rv, again.ResourceVersion)
	}
	if !again.Status.DegradedSince.Equal(held.Status.DegradedSince) {
		t.Error("degradedSince was rewritten; it must record when the outage began, not the last probe")
	}
}

// TestProviderCircuitStillGivesUpAfterTheGrace guards the property the hold must
// not break: the breaker delays reconciles, it does not extend the window in
// which a Bucket may advertise a state nobody verified.
func TestProviderCircuitStillGivesUpAfterTheGrace(t *testing.T) {
	e, _ := withCircuit(t)
	e.provision(t, newBucketCR("team-a", "app-data"))
	tripCircuit(t, e)

	// Backdate the degradation past the grace, as wall-clock time would.
	b := e.getBucket(t, "team-a", "app-data")
	expired := metav1.NewTime(time.Now().Add(-defaultGrace - time.Minute))
	b.Status.DegradedSince = &expired
	if err := e.k8s.Status().Update(context.Background(), b); err != nil {
		t.Fatalf("backdate degradedSince: %v", err)
	}

	res, err := e.reconcile(t, "team-a", "app-data")
	if err != nil {
		t.Fatalf("held reconcile returned %v, want no error", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("the Bucket must stay requeued so it recovers when the provider returns")
	}
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseFailed {
		t.Errorf("phase = %q, want Failed once the grace elapsed (message %q)", got.Status.Phase, got.Status.Message)
	}
	// Parked Buckets must not be rewritten on every probe either.
	rv := got.ResourceVersion
	if _, err := e.reconcile(t, "team-a", "app-data"); err != nil {
		t.Fatalf("held reconcile after parking: %v", err)
	}
	if again := e.getBucket(t, "team-a", "app-data"); again.ResourceVersion != rv {
		t.Errorf("resourceVersion moved %s -> %s; a parked Bucket must write nothing", rv, again.ResourceVersion)
	}
}

// TestProviderCircuitRecovers walks the whole outage back out: once the cooldown
// elapses a probe gets through, succeeds, and the Bucket is Ready with the
// degradation cleared.
func TestProviderCircuitRecovers(t *testing.T) {
	e, clk := withCircuit(t)
	e.provision(t, newBucketCR("team-a", "app-data"))
	tripCircuit(t, e)

	// Time travel instead of sleeping through the cooldown.
	clk.Advance(circuitBaseCooldown)

	if _, err := e.reconcile(t, "team-a", "app-data"); err != nil {
		t.Fatalf("probe reconcile: %v", err)
	}
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready after recovery (message %q)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.DegradedSince != nil {
		t.Error("degradedSince must be cleared once the provider answered again")
	}
	if _, open := e.r.Breaker.OpenedAt(); open {
		t.Error("a successful probe must close the breaker")
	}
	// And the fleet is reconciling normally again.
	before := e.fake.Calls("GetBucket")
	if _, err := e.reconcile(t, "team-a", "app-data"); err != nil {
		t.Fatalf("post-recovery reconcile: %v", err)
	}
	if e.fake.Calls("GetBucket") == before {
		t.Error("reconciles must reach the provider again once the circuit closed")
	}
}

// TestProviderCircuitIgnoresAnIsolatedBrokenBucket is the discriminator: one
// Bucket failing forever among healthy ones must never stop the fleet, because
// its failures are interleaved with the successes of every other Bucket.
func TestProviderCircuitIgnoresAnIsolatedBrokenBucket(t *testing.T) {
	e, _ := withCircuit(t)
	e.provision(t, newBucketCR("team-a", "app-data"))
	e.provision(t, newBucketCR("team-b", "other-data"))

	for range 5 {
		e.failGetBucket()
		if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
			t.Fatal("the broken Bucket must keep reporting its error")
		}
		if _, err := e.reconcile(t, "team-b", "other-data"); err != nil {
			t.Fatalf("the healthy Bucket must keep reconciling: %v", err)
		}
	}
	if _, open := e.r.Breaker.OpenedAt(); open {
		t.Error("interleaved successes must keep the breaker closed")
	}
	if got := e.getBucket(t, "team-b", "other-data"); got.Status.Phase != s3v1.PhaseReady {
		t.Errorf("healthy Bucket phase = %q, want Ready", got.Status.Phase)
	}
}

// TestProviderCircuitDefersTeardown covers the delete path: teardown is provider
// work like any other, so it waits for the probe rather than hammering. The
// finalizer stays, which is what keeps the Bucket from being removed while its
// cloud resources are still there.
func TestProviderCircuitDefersTeardown(t *testing.T) {
	e, clk := withCircuit(t)
	e.provision(t, newBucketCR("team-a", "app-data"))
	tripCircuit(t, e)

	b := e.getBucket(t, "team-a", "app-data")
	if err := e.k8s.Delete(context.Background(), b); err != nil {
		t.Fatalf("delete bucket CR: %v", err)
	}

	before := e.fake.Calls("DeleteBucket")
	res, err := e.reconcile(t, "team-a", "app-data")
	if err != nil {
		t.Fatalf("teardown under an open circuit returned %v, want no error", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("a deferred teardown must requeue on the breaker's cooldown")
	}
	if e.fake.Calls("DeleteBucket") != before {
		t.Error("teardown called the provider while the circuit was open")
	}
	still := e.getBucket(t, "team-a", "app-data")
	if len(still.Finalizers) == 0 {
		t.Fatal("the finalizer was dropped without a teardown; the cloud resources would leak")
	}

	// Once the provider is back the delete completes on the next probe.
	clk.Advance(circuitBaseCooldown)
	if _, err := e.reconcile(t, "team-a", "app-data"); err != nil {
		t.Fatalf("teardown after recovery: %v", err)
	}
	if names := e.fake.BucketNames(); len(names) != 0 {
		t.Errorf("buckets left in the provider after teardown: %v", names)
	}
}
