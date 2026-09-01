package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// measuringConfig is a UsageConfig that measures every Bucket by default, with
// the EU01 list price configured so the cost estimate is exercised end to end.
func measuringConfig() UsageConfig {
	return UsageConfig{
		Enabled:        true,
		DefaultEnabled: true,
		Interval:       time.Hour,
		MinInterval:    time.Hour,
		MaxObjects:     DefaultUsageMaxObjects,
		PricePerGBHour: eu01ListPrice,
		Currency:       "EUR",
	}
}

// usageReconciler builds a measurement controller sharing this env's clients.
func (e *testEnv) usageReconciler(cfg UsageConfig) *BucketUsageReconciler {
	return &BucketUsageReconciler{
		Client:               e.k8s,
		Recorder:             e.rec,
		Stackit:              e.r.Stackit,
		Config:               cfg,
		AdminSecretName:      testAdminSec,
		AdminSecretNamespace: testOpNS,
	}
}

// measure runs one measurement reconcile for the named Bucket.
func (e *testEnv) measure(t *testing.T, cfg UsageConfig, b *s3v1.Bucket) ctrl.Result {
	t.Helper()
	res, err := e.usageReconciler(cfg).Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(b),
	})
	// Measurement is informational: it must never surface as a reconcile error,
	// because that signal belongs to provisioning failures.
	if err != nil {
		t.Fatalf("usage reconcile returned an error: %v", err)
	}
	return res
}

// usageOf returns the Bucket's measurement, failing when there is none.
func (e *testEnv) usageOf(t *testing.T, b *s3v1.Bucket) *s3v1.UsageStatus {
	t.Helper()
	got := e.getBucket(t, b.Namespace, b.Name)
	if got.Status.Usage == nil {
		t.Fatal("status.usage is unset, want a measurement")
	}
	return got.Status.Usage
}

func TestUsageMeasuresProvisionedBucket(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	name := b.Status.ResolvedBucketName

	// 3 GB of current data in two objects.
	e.fake.SeedObjectVersion(name, "big.bin", "v1", false, true, 2_000_000_000)
	e.fake.SeedObjectVersion(name, "small.bin", "v1", false, true, 1_000_000_000)

	res := e.measure(t, measuringConfig(), b)

	u := e.usageOf(t, b)
	if u.Bytes != 3_000_000_000 || u.Objects != 2 {
		t.Fatalf("measured %d bytes / %d objects, want 3000000000/2", u.Bytes, u.Objects)
	}
	if u.BillableBytes != 3_000_000_000 {
		t.Fatalf("BillableBytes = %d, want the current-object total", u.BillableBytes)
	}
	if u.HumanReadable != "2.8 GiB" {
		t.Fatalf("HumanReadable = %q, want the GiB rendering of 3 GB", u.HumanReadable)
	}
	// 3 started GB * 0.00003697772 * 720 h = 0.0798... -> 8 cents.
	if u.EstimatedMonthlyCostCents != 8 || u.EstimatedMonthlyCost != "0.08 EUR" {
		t.Fatalf("cost = %d cents / %q, want 8 / \"0.08 EUR\"",
			u.EstimatedMonthlyCostCents, u.EstimatedMonthlyCost)
	}
	if u.Currency != "EUR" {
		t.Fatalf("Currency = %q, want EUR", u.Currency)
	}
	if u.LastMeasurementTime == nil {
		t.Fatal("LastMeasurementTime unset after a successful measurement")
	}
	if u.MeasurementDuration == "" {
		t.Fatal("MeasurementDuration unset; it is the honest price of the interval")
	}
	if u.Truncated || u.Message != "" {
		t.Fatalf("Truncated=%t Message=%q, want a clean measurement", u.Truncated, u.Message)
	}
	// The schedule comes from the requeue, not from a watch on status.
	if res.RequeueAfter < time.Hour {
		t.Fatalf("RequeueAfter = %v, want at least one interval", res.RequeueAfter)
	}

	// The measurement must not disturb what provisioning owns.
	got := e.getBucket(t, b.Namespace, b.Name)
	if got.Status.Phase != s3v1.PhaseReady || got.Status.ResolvedBucketName != name {
		t.Fatalf("provisioning status damaged by a measurement: phase=%q resolved=%q",
			got.Status.Phase, got.Status.ResolvedBucketName)
	}
	if got.Status.AccessKeyID == "" || got.Status.CredentialsGroupURN == "" {
		t.Fatal("measurement clobbered credential fields in status")
	}
}

func TestUsageDisabledByDefaultDoesNotMeasure(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 10)

	cfg := measuringConfig()
	cfg.DefaultEnabled = false // the shipped Helm default

	before := e.fake.Calls("S3ListObjects")
	e.measure(t, cfg, b)

	if got := e.getBucket(t, b.Namespace, b.Name); got.Status.Usage != nil {
		t.Fatalf("status.usage = %+v, want nothing measured", got.Status.Usage)
	}
	if after := e.fake.Calls("S3ListObjects"); after != before {
		t.Fatalf("listed the bucket %d time(s) while measurement is off", after-before)
	}
}

func TestUsageOptInAgainstOffDefault(t *testing.T) {
	e := newTestEnv(t)
	cr := newBucketCR("team-a", "app-data")
	cr.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true)}
	b := e.provision(t, cr)
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 42)

	cfg := measuringConfig()
	cfg.DefaultEnabled = false

	e.measure(t, cfg, b)
	if u := e.usageOf(t, b); u.Bytes != 42 {
		t.Fatalf("measured %d bytes, want 42", u.Bytes)
	}
}

func TestUsageClearsStaleMeasurementWhenSwitchedOff(t *testing.T) {
	e := newTestEnv(t)
	cr := newBucketCR("team-a", "app-data")
	b := e.provision(t, cr)
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 4096)

	cfg := measuringConfig()
	e.measure(t, cfg, b)
	if u := e.usageOf(t, b); u.Bytes != 4096 {
		t.Fatalf("measured %d bytes, want 4096", u.Bytes)
	}

	// Switch measurement off for this Bucket. A size and a cost that nobody
	// refreshes any more must not keep being displayed as if they were current.
	live := e.getBucket(t, b.Namespace, b.Name)
	live.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(false)}
	if err := e.k8s.Update(context.Background(), live); err != nil {
		t.Fatalf("update spec: %v", err)
	}
	e.measure(t, cfg, b)

	if got := e.getBucket(t, b.Namespace, b.Name); got.Status.Usage != nil {
		t.Fatalf("status.usage = %+v, want it cleared", got.Status.Usage)
	}
}

func TestUsageGateRefusesExplicitRequest(t *testing.T) {
	e := newTestEnv(t)
	cr := newBucketCR("team-a", "app-data")
	cr.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true)}
	b := e.provision(t, cr)
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 10)

	// Measure once while the gate is open, so there is something to become stale.
	cfg := measuringConfig()
	e.measure(t, cfg, b)
	if u := e.usageOf(t, b); u.Bytes != 10 {
		t.Fatalf("measured %d bytes, want 10", u.Bytes)
	}

	cfg.Enabled = false // the hard kill switch

	before := e.fake.Calls("S3ListObjects")
	e.measure(t, cfg, b)

	if after := e.fake.Calls("S3ListObjects"); after != before {
		t.Fatal("the operator-wide gate did not stop the listing")
	}
	// Silence would be indistinguishable from a measurement that simply has not
	// run yet, so the refusal has to be visible on the CR and as an event.
	u := e.usageOf(t, b)
	if !strings.Contains(u.Message, "disabled operator-wide") {
		t.Fatalf("status.usage.message = %q, want the gate mentioned", u.Message)
	}
	// Nothing will ever refresh these numbers again, so displaying them would
	// assert a currency they no longer have.
	if u.Bytes != 0 || u.LastMeasurementTime != nil || u.EstimatedMonthlyCost != "" {
		t.Fatalf("stale values survived the refusal: %d bytes, measured %v, cost %q",
			u.Bytes, u.LastMeasurementTime, u.EstimatedMonthlyCost)
	}
	if !e.rec.hasReason(reasonUsageGateDisabled) {
		t.Fatalf("no %s event; events are %+v", reasonUsageGateDisabled, e.rec.events)
	}
}

func TestUsageInvalidIntervalParksVisibly(t *testing.T) {
	e := newTestEnv(t)
	cr := newBucketCR("team-a", "app-data")
	// A bare number is the classic mistake; defaulting it away would hide it.
	cr.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true), Interval: "60"}
	b := e.provision(t, cr)

	before := e.fake.Calls("S3ListObjects")
	res := e.measure(t, measuringConfig(), b)

	if after := e.fake.Calls("S3ListObjects"); after != before {
		t.Fatal("measured despite an unusable interval")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want no retry hammer for a config fault", res.RequeueAfter)
	}
	if u := e.usageOf(t, b); !strings.Contains(u.Message, "spec.usage.interval") {
		t.Fatalf("status.usage.message = %q, want the offending field named", u.Message)
	}
	if !e.rec.hasReason(reasonUsageInvalid) {
		t.Fatalf("no %s event; events are %+v", reasonUsageInvalid, e.rec.events)
	}
}

func TestUsageIntervalClampedToFloor(t *testing.T) {
	e := newTestEnv(t)
	cr := newBucketCR("team-a", "app-data")
	cr.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true), Interval: "1m"}
	b := e.provision(t, cr)
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 10)

	res := e.measure(t, measuringConfig(), b)

	// Clamped, not refused: the bucket is measured, just not that often.
	u := e.usageOf(t, b)
	if u.Bytes != 10 {
		t.Fatalf("measured %d bytes, want 10", u.Bytes)
	}
	if !strings.Contains(u.Message, "raised to the operator's floor") {
		t.Fatalf("status.usage.message = %q, want the clamp explained", u.Message)
	}
	if !e.rec.hasReason(reasonUsageClamped) {
		t.Fatalf("no %s event; events are %+v", reasonUsageClamped, e.rec.events)
	}
	if res.RequeueAfter < time.Hour {
		t.Fatalf("RequeueAfter = %v, want the clamped interval, not the requested 1m", res.RequeueAfter)
	}
}

func TestUsageNotDueSchedulesWithoutListing(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 10)

	cfg := measuringConfig()
	e.measure(t, cfg, b)
	first := e.usageOf(t, b).LastMeasurementTime.Time

	before := e.fake.Calls("S3ListObjects")
	res := e.measure(t, cfg, b)

	if after := e.fake.Calls("S3ListObjects"); after != before {
		t.Fatal("re-listed the bucket before the interval elapsed")
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Hour {
		t.Fatalf("RequeueAfter = %v, want the remainder of the interval", res.RequeueAfter)
	}
	if got := e.usageOf(t, b).LastMeasurementTime.Time; !got.Equal(first) {
		t.Fatalf("measurement timestamp moved from %v to %v without a listing", first, got)
	}
}

func TestUsageMeasuresAgainOnceDue(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	name := b.Status.ResolvedBucketName
	e.fake.SeedObjectVersion(name, "a", "v1", false, true, 10)

	cfg := measuringConfig()
	e.measure(t, cfg, b)

	// Backdate the measurement past its interval and let the bucket grow. The
	// due time comes from status, not from a process-local timer, which is what
	// makes the schedule survive an operator restart.
	live := e.getBucket(t, b.Namespace, b.Name)
	live.Status.Usage.LastMeasurementTime = &metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	if err := e.k8s.Status().Update(context.Background(), live); err != nil {
		t.Fatalf("backdate measurement: %v", err)
	}
	e.fake.SeedObjectVersion(name, "b", "v1", false, true, 90)

	e.measure(t, cfg, b)
	if u := e.usageOf(t, b); u.Bytes != 100 || u.Objects != 2 {
		t.Fatalf("re-measured %d bytes / %d objects, want 100/2", u.Bytes, u.Objects)
	}
}

func TestUsageTruncatedAtObjectCap(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	name := b.Status.ResolvedBucketName
	for _, k := range []string{"a", "b", "c"} {
		e.fake.SeedObjectVersion(name, k, "v1", false, true, 2_000_000_000)
	}

	cfg := measuringConfig()
	cfg.MaxObjects = 2

	e.measure(t, cfg, b)

	u := e.usageOf(t, b)
	if !u.Truncated {
		t.Fatal("want status.usage.truncated after hitting the cap")
	}
	// Everything derived from a capped listing is a lower bound and has to say so,
	// or a size-based alert silently under-reports.
	if !strings.HasPrefix(u.HumanReadable, ">= ") {
		t.Fatalf("HumanReadable = %q, want a lower-bound marker", u.HumanReadable)
	}
	if !strings.HasPrefix(u.EstimatedMonthlyCost, ">= ") {
		t.Fatalf("EstimatedMonthlyCost = %q, want a lower-bound marker", u.EstimatedMonthlyCost)
	}
	if !strings.Contains(u.Message, "cap") {
		t.Fatalf("status.usage.message = %q, want the cap mentioned", u.Message)
	}
	if !e.rec.hasReason(reasonUsageTruncated) {
		t.Fatalf("no %s event; events are %+v", reasonUsageTruncated, e.rec.events)
	}
}

func TestUsageCountsVersionsWhenEnabled(t *testing.T) {
	e := newTestEnv(t)
	cr := newBucketCR("team-a", "app-data")
	cr.Spec.Usage = &s3v1.UsageSpec{IncludeVersions: usageBool(true)}
	b := e.provision(t, cr)
	name := b.Status.ResolvedBucketName

	e.fake.SeedObjectVersion(name, "a", "v1", false, true, 1_000_000_000)
	e.fake.SeedObjectVersion(name, "a", "v0", false, false, 2_000_000_000)
	e.fake.SeedObjectVersion(name, "gone", "v9", true, true, 0)

	e.measure(t, measuringConfig(), b)

	u := e.usageOf(t, b)
	if u.Bytes != 1_000_000_000 || u.Objects != 1 {
		t.Fatalf("current = %d bytes / %d objects, want 1000000000/1", u.Bytes, u.Objects)
	}
	if u.VersionBytes != 2_000_000_000 || u.VersionObjects != 2 {
		t.Fatalf("versions = %d bytes / %d objects, want 2000000000/2", u.VersionBytes, u.VersionObjects)
	}
	// The billed figure is what the cost is computed from: without version
	// counting this bucket would be priced at a third of its real footprint.
	if u.BillableBytes != 3_000_000_000 {
		t.Fatalf("BillableBytes = %d, want 3000000000", u.BillableBytes)
	}
	if u.EstimatedMonthlyCostCents != 8 {
		t.Fatalf("cost = %d cents, want 8 (3 started GB)", u.EstimatedMonthlyCostCents)
	}
}

func TestUsageFailureKeepsPreviousMeasurement(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	name := b.Status.ResolvedBucketName
	e.fake.SeedObjectVersion(name, "a", "v1", false, true, 4096)

	cfg := measuringConfig()
	e.measure(t, cfg, b)
	first := e.usageOf(t, b)

	// Make the next listing fail, and let the bucket become due again.
	live := e.getBucket(t, b.Namespace, b.Name)
	live.Status.Usage.LastMeasurementTime = &metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	if err := e.k8s.Status().Update(context.Background(), live); err != nil {
		t.Fatalf("backdate measurement: %v", err)
	}
	// 403, not a 5xx: minio-go retries 5xx internally, so a 5xx injection would
	// be swallowed and the measurement would quietly succeed.
	e.fake.FailNext("S3ListObjects", 403)

	res := e.measure(t, cfg, b)

	u := e.usageOf(t, b)
	// A stale size beats no size, and the message says what happened.
	if u.Bytes != first.Bytes || u.Objects != first.Objects {
		t.Fatalf("previous measurement lost: %d/%d, want %d/%d",
			u.Bytes, u.Objects, first.Bytes, first.Objects)
	}
	if !strings.Contains(u.Message, "measure bucket") {
		t.Fatalf("status.usage.message = %q, want the failure recorded", u.Message)
	}
	if !e.rec.hasReason(reasonUsageFailed) {
		t.Fatalf("no %s event; events are %+v", reasonUsageFailed, e.rec.events)
	}
	// Retried sooner than a whole interval, since the failure cost no listing.
	if res.RequeueAfter <= 0 || res.RequeueAfter > usageFailureRetry {
		t.Fatalf("RequeueAfter = %v, want a bounded retry", res.RequeueAfter)
	}

	// A measurement failure says nothing about the bucket itself.
	got := e.getBucket(t, b.Namespace, b.Name)
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready to be untouched by a measurement failure", got.Status.Phase)
	}
	if got.Status.DegradedSince != nil {
		t.Fatal("a measurement failure must not degrade the Bucket")
	}
}

func TestUsageSkipsUnprovisionedBucket(t *testing.T) {
	e := newTestEnv(t)
	cr := newBucketCR("team-a", "app-data")
	if err := e.k8s.Create(context.Background(), cr); err != nil {
		t.Fatalf("create bucket CR: %v", err)
	}

	res := e.measure(t, measuringConfig(), cr)

	if got := e.getBucket(t, cr.Namespace, cr.Name); got.Status.Usage != nil {
		t.Fatalf("status.usage = %+v, want nothing measured before provisioning", got.Status.Usage)
	}
	// Cheap re-check: there is nothing to measure yet, but there will be.
	if res.RequeueAfter != usageNotProvisionedRetry {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, usageNotProvisionedRetry)
	}
}

func TestUsageSkipsBucketUnderDeletion(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 10)

	if err := e.k8s.Delete(context.Background(), e.getBucket(t, b.Namespace, b.Name)); err != nil {
		t.Fatalf("delete bucket CR: %v", err)
	}

	before := e.fake.Calls("S3ListObjects")
	res := e.measure(t, measuringConfig(), b)

	// Teardown runs its own emptiness check; a measurement would only race it.
	if after := e.fake.Calls("S3ListObjects"); after != before {
		t.Fatal("measured a Bucket that is being torn down")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want none for a Bucket under deletion", res.RequeueAfter)
	}
}

func TestUsageSkeletonModeMeasuresNothing(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))

	r := e.usageReconciler(measuringConfig())
	r.Stackit = nil // skeleton mode: nothing was provisioned to measure

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	if err != nil {
		t.Fatalf("usage reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want no polling in skeleton mode", res.RequeueAfter)
	}
	if got := e.getBucket(t, b.Namespace, b.Name); got.Status.Usage != nil {
		t.Fatalf("status.usage = %+v, want nothing in skeleton mode", got.Status.Usage)
	}
}

func TestUsageWithoutPriceOmitsCost(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	e.fake.SeedObjectVersion(b.Status.ResolvedBucketName, "a", "v1", false, true, 4_000_000_000)

	cfg := measuringConfig()
	cfg.PricePerGBHour = 0 // no price configured

	e.measure(t, cfg, b)

	u := e.usageOf(t, b)
	if u.Bytes != 4_000_000_000 {
		t.Fatalf("measured %d bytes, want the size regardless of pricing", u.Bytes)
	}
	// Inventing a cost without a configured price would be worse than none.
	if u.EstimatedMonthlyCost != "" || u.EstimatedMonthlyCostCents != 0 || u.Currency != "" {
		t.Fatalf("cost fields set without a price: %q / %d / %q",
			u.EstimatedMonthlyCost, u.EstimatedMonthlyCostCents, u.Currency)
	}
}
