package controller

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// provisionedNotes returns the note of every recorded Provisioned event, in order.
func (f *fakeRecorder) provisionedNotes() []string {
	var out []string
	for _, e := range f.events {
		if e.Reason == s3v1.ReasonProvisioned {
			out = append(out, e.Note)
		}
	}
	return out
}

// TestReportPass pins the reporting policy of a successful pass: changes are
// Info plus the Provisioned event, the first unchanged pass since process start
// is Info without an event, every later unchanged pass is verbosity 1 only.
func TestReportPass(t *testing.T) {
	e := newTestEnv(t)
	var buf bytes.Buffer
	ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(&buf), zap.UseDevMode(true)))
	b := newBucketCR("team-a", "app-data")
	creds := workloadCreds{gid: "g1"}

	e.r.reportPass(ctx, b, "app-data", creds, nil)
	if out := buf.String(); !strings.Contains(out, "INFO") || !strings.Contains(out, "bucket verified after operator start") {
		t.Errorf("first unchanged pass: want an INFO line, got %q", out)
	}
	if got := e.rec.provisionedNotes(); len(got) != 0 {
		t.Errorf("first unchanged pass raised events: %v", got)
	}

	buf.Reset()
	e.r.reportPass(ctx, b, "app-data", creds, nil)
	if out := buf.String(); strings.Contains(out, "INFO") || !strings.Contains(out, "bucket verified") {
		t.Errorf("repeated unchanged pass: want a DEBUG line only, got %q", out)
	}
	if got := e.rec.provisionedNotes(); len(got) != 0 {
		t.Errorf("repeated unchanged pass raised events: %v", got)
	}

	buf.Reset()
	e.r.reportPass(ctx, b, "app-data", creds, passChanges{"isolation policy written"})
	if out := buf.String(); !strings.Contains(out, "INFO") || !strings.Contains(out, "bucket provisioned") ||
		!strings.Contains(out, "isolation policy written") {
		t.Errorf("changed pass: want an INFO line naming the change, got %q", out)
	}
	got := e.rec.provisionedNotes()
	if len(got) != 1 || !strings.Contains(got[0], "isolation policy written") {
		t.Errorf("changed pass: want one Provisioned event naming the change, got %v", got)
	}

	// A deleted Bucket is forgotten, so a re-created one counts as first again.
	e.r.verifiedSinceStart.Delete(types.NamespacedName{Namespace: "team-a", Name: "app-data"})
	buf.Reset()
	e.r.reportPass(ctx, b, "app-data", creds, nil)
	if out := buf.String(); !strings.Contains(out, "INFO") {
		t.Errorf("after forgetting: want an INFO line again, got %q", out)
	}
}

// TestUnchangedPassIsSilent pins that the drift resync of a Bucket nobody
// touched raises no further Provisioned event while lastVerifiedTime still
// advances, so the resync stays provable per object.
func TestUnchangedPassIsSilent(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	if got := e.rec.provisionedNotes(); len(got) != 1 {
		t.Fatalf("Provisioned events after provisioning = %d, want exactly 1: %v", len(got), got)
	}
	first := b.Status.LastVerifiedTime
	if first == nil {
		t.Fatal("lastVerifiedTime not written on the provisioning pass")
	}

	e.reconcileN(t, "team-a", "app-data", 3)
	if got := e.rec.provisionedNotes(); len(got) != 1 {
		t.Errorf("unchanged passes raised Provisioned events: %v", got[1:])
	}
	b = e.getBucket(t, "team-a", "app-data")
	// metav1.Time carries second precision through the API, so within one test
	// the stamp can only be shown not to have gone backwards.
	if b.Status.LastVerifiedTime == nil || b.Status.LastVerifiedTime.Before(first) {
		t.Errorf("lastVerifiedTime = %v after unchanged passes, want >= %v", b.Status.LastVerifiedTime, first)
	}
	if b.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase = %q, want Ready", b.Status.Phase)
	}
}

// TestPolicyDriftIsReported pins that a pass which re-asserts a manually
// changed isolation policy is reported: the Provisioned event names the write.
func TestPolicyDriftIsReported(t *testing.T) {
	e := newTestEnv(t)
	b := e.provision(t, newBucketCR("team-a", "app-data"))
	name := b.Status.ResolvedBucketName
	e.fake.SetPolicy(name, `{"Version":"2012-10-17","Statement":[]}`)

	e.reconcileN(t, "team-a", "app-data", 1)
	got := e.rec.provisionedNotes()
	if len(got) != 2 || !strings.Contains(got[1], "isolation policy written") {
		t.Fatalf("Provisioned events after policy drift = %v, want a second one naming the policy write", got)
	}
	if e.fake.Policy(name) == `{"Version":"2012-10-17","Statement":[]}` {
		t.Error("policy was reported written but not re-asserted")
	}

	// The pass after that has nothing left to do.
	e.reconcileN(t, "team-a", "app-data", 1)
	if got := e.rec.provisionedNotes(); len(got) != 2 {
		t.Errorf("a pass after the repair raised events: %v", got[2:])
	}
}

// TestSecretLossIsReported pins that re-issuing a lost workload Secret is
// reported as a change.
func TestSecretLossIsReported(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.provision(t, newBucketCR("team-a", "app-data"))

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app-data-s3"}}
	if err := e.k8s.Delete(ctx, sec); err != nil {
		t.Fatalf("delete workload secret: %v", err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)
	got := e.rec.provisionedNotes()
	if len(got) != 2 || !strings.Contains(got[1], "workload credentials issued") {
		t.Fatalf("Provisioned events after Secret loss = %v, want a second one naming the credential", got)
	}
}
