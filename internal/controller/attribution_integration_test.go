//go:build integration

// Integration test for the credentials-group attribution of ADR 0002 against
// the REAL STACKIT API: a bucket provisioned the pre-ADR way (ownership tags,
// group by display name, key, policy, Secret — but no credentials-group tag)
// must survive the upgrade untouched, and a restore of its CR without status
// must re-attach to the same group. Run explicitly:
//
//	go test -tags integration ./internal/controller/ -run IntegrationGroupAttribution -v -timeout 15m
//
// Skipped when the SA key file is absent. Creates and deletes real resources in
// project 1; bootstraps (or re-keys) the shared operator-admin group there.
package controller

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// newIntegrationEnv wires the production reconciler to the real STACKIT client
// and an in-memory Kubernetes client.
func newIntegrationEnv(t *testing.T) *testEnv {
	t.Helper()
	keyPath := os.Getenv("STACKIT_ACCOUNT_1")
	if keyPath == "" {
		keyPath = filepath.Join("..", "..", "account-1.json")
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Skipf("SA key file %s not present: %v", keyPath, err)
	}
	acc, err := stackit.LoadAccount(keyPath)
	if err != nil {
		t.Fatalf("load account: %v", err)
	}
	sc, err := stackit.NewClient(acc, stackit.RegionEU01)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := s3v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&s3v1.Bucket{}).Build()
	rec := &fakeRecorder{}
	return &testEnv{
		r: &BucketReconciler{
			Client:               k8s,
			Scheme:               scheme,
			Recorder:             rec,
			Stackit:              sc,
			OperatorVersion:      "integration-test",
			AdminSecretName:      testAdminSec,
			AdminSecretNamespace: testOpNS,
		},
		k8s: k8s,
		rec: rec,
	}
}

// reconcileTolerant runs `passes` successful reconciles, retrying a reconcile
// that returns an error up to `retries` times in total. The real API answers
// the occasional write with a reset connection; the operator keeps the
// finalizer or the requeue and tries again on the next pass, and so does this.
func (e *testEnv) reconcileTolerant(t *testing.T, ns, name string, passes, retries int) {
	t.Helper()
	for done := 0; done < passes; {
		_, err := e.reconcile(t, ns, name)
		if err == nil {
			done++
			continue
		}
		if retries == 0 {
			t.Fatalf("reconcile %s/%s: %v (no retries left)", ns, name, err)
		}
		retries--
		t.Logf("reconcile %s/%s: %v (retrying, %d left)", ns, name, err, retries)
		time.Sleep(5 * time.Second)
	}
}

// legacyState is what the seeding leaves in the cloud and the cluster.
type legacyState struct {
	bucket   string
	groupID  string
	groupURN string
	keyIDs   []string
	secret   map[string][]byte
}

// seedLegacyBucket provisions a bucket exactly as the operator did before ADR
// 0002 — bucket, ownership tags only, group by display name, one key, policy,
// Secret — bypassing the reconciler, and registers cleanup for all of it.
func seedLegacyBucket(t *testing.T, ctx context.Context, e *testEnv, b *s3v1.Bucket) legacyState {
	t.Helper()
	c := e.r.Stackit
	name := b.Spec.BucketName

	admin, err := e.r.ensureAdmin(ctx)
	if err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	if err := c.EnsureService(ctx); err != nil {
		t.Fatalf("ensure service: %v", err)
	}
	if err := c.CreateBucket(ctx, name); err != nil {
		t.Fatalf("create bucket %q: %v", name, err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := c.DeleteBucket(cctx, name); err != nil && stackit.StatusCode(err) != 404 {
			t.Logf("cleanup: delete bucket %q: %v", name, err)
		}
	})
	if err := c.WaitBucketVisible(ctx, name, 60*time.Second); err != nil {
		t.Fatalf("wait bucket visible: %v", err)
	}
	s3admin, err := e.r.newS3Admin(ctx, name, admin)
	if err != nil {
		t.Fatalf("s3 admin: %v", err)
	}
	// The data plane may lag the control plane by a moment after creation.
	var tagErr error
	for i := 0; i < 10; i++ {
		if tagErr = s3admin.SetBucketTags(ctx, name, e.r.ownershipTags(b)); tagErr == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if tagErr != nil {
		t.Fatalf("set ownership tags: %v", tagErr)
	}

	gid, gurn, err := c.CreateCredentialsGroup(ctx, workloadGroupName(b))
	if err != nil {
		t.Fatalf("create legacy group: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// Also sweep any group the test may have created under the same name
		// (that would be the very failure the test guards against).
		groups, err := c.ListCredentialsGroups(cctx)
		if err != nil {
			t.Logf("cleanup: list groups: %v", err)
			return
		}
		for _, g := range groups {
			if g.ID != gid && g.DisplayName != workloadGroupName(b) {
				continue
			}
			if err := c.DeleteAllAccessKeys(cctx, g.ID); err != nil {
				t.Logf("cleanup: drain group %s: %v", g.ID, err)
			}
			if err := c.DeleteCredentialsGroup(cctx, g.ID); err != nil && stackit.StatusCode(err) != 404 {
				t.Logf("cleanup: delete group %s: %v", g.ID, err)
			}
		}
	})
	ak, err := c.CreateAccessKey(ctx, gid)
	if err != nil {
		t.Fatalf("create legacy key: %v", err)
	}
	host, bucketURL, err := c.BucketConnInfo(ctx, name)
	if err != nil {
		t.Fatalf("bucket conn info: %v", err)
	}
	data := b.SecretData(s3v1.SecretValues{
		AccessKeyID: ak.AccessKeyID, SecretAccessKey: ak.SecretAccessKey, Endpoint: host, BucketURL: bucketURL,
	})
	if err := e.r.upsertSecret(ctx, b, b.Spec.SecretRef.Name, data); err != nil {
		t.Fatalf("write legacy secret: %v", err)
	}
	if err := s3admin.SetBucketPolicy(ctx, name, stackit.BuildIsolationPolicy(name, admin.urn, gurn, nil)); err != nil {
		t.Fatalf("set legacy policy: %v", err)
	}
	keyIDs, err := c.ListAccessKeyIDs(ctx, gid)
	if err != nil {
		t.Fatalf("list legacy keys: %v", err)
	}
	return legacyState{bucket: name, groupID: gid, groupURN: gurn, keyIDs: keyIDs, secret: e.secretData(t, b.Namespace, b.Spec.SecretRef.Name)}
}

// assertLegacyUntouched checks, against the real API, that the bucket still
// exists, the group is the seeded one with the seeded key, the Secret is
// byte-identical, the display name exists exactly once and the tag now binds
// the bucket to the group.
func assertLegacyUntouched(t *testing.T, ctx context.Context, e *testEnv, b *s3v1.Bucket, want legacyState) {
	t.Helper()
	c := e.r.Stackit

	got := e.getBucket(t, b.Namespace, b.Name)
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q (%s), want Ready", got.Status.Phase, got.Status.Message)
	}
	if got.Status.CredentialsGroupID != want.groupID || got.Status.CredentialsGroupURN != want.groupURN {
		t.Errorf("status group = %s / %s, want seeded %s / %s", got.Status.CredentialsGroupID, got.Status.CredentialsGroupURN, want.groupID, want.groupURN)
	}
	exists, err := c.HasBucket(ctx, c.ProjectID(), want.bucket)
	if err != nil || !exists {
		t.Fatalf("bucket %q exists=%v err=%v, want true", want.bucket, exists, err)
	}
	keyIDs, err := c.ListAccessKeyIDs(ctx, want.groupID)
	if err != nil {
		t.Fatalf("list keys of %s: %v", want.groupID, err)
	}
	sort.Strings(keyIDs)
	sort.Strings(want.keyIDs)
	if !reflect.DeepEqual(keyIDs, want.keyIDs) {
		t.Errorf("group keys = %v, want the seeded %v (no rotation)", keyIDs, want.keyIDs)
	}
	if data := e.secretData(t, b.Namespace, b.Spec.SecretRef.Name); !reflect.DeepEqual(data, want.secret) {
		t.Error("Secret changed")
	}
	groups, err := c.ListCredentialsGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range groups {
		if g.DisplayName == workloadGroupName(b) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("groups named %q = %d, want exactly 1 (no duplicate created)", workloadGroupName(b), n)
	}
	admin, err := e.r.ensureAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s3admin, err := e.r.newS3Admin(ctx, want.bucket, admin)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := s3admin.BucketTags(ctx, want.bucket)
	if err != nil {
		t.Fatal(err)
	}
	wantTags := e.r.ownershipTags(b)
	wantTags[tagCredentialsGroupID] = want.groupID
	wantTags[tagCredentialsGroupURN] = want.groupURN
	if !reflect.DeepEqual(tags, wantTags) {
		t.Errorf("bucket tags = %v, want %v", tags, wantTags)
	}
}

func TestIntegrationGroupAttributionMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	e := newIntegrationEnv(t)
	c := e.r.Stackit

	ns := "itest"
	crName := fmt.Sprintf("s3op-itest-%d", rand.Intn(1_000_000))
	b := newBucketCR(ns, crName)
	legacy := seedLegacyBucket(t, ctx, e, b)
	t.Logf("seeded legacy bucket %s, group %s (%s), keys %v", legacy.bucket, legacy.groupID, workloadGroupName(b), legacy.keyIDs)

	// 1. Upgrade: first reconciles of the new operator over the legacy state.
	if err := e.k8s.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.reconcileTolerant(t, ns, crName, 2, 3)
	assertLegacyUntouched(t, ctx, e, b, legacy)
	if !e.rec.hasReason(reasonGroupAttributed) {
		t.Errorf("no %s event; events: %+v", reasonGroupAttributed, e.rec.events)
	}
	t.Log("OK: upgrade over legacy state changed nothing but the tag")

	// 2. Restore: tag gone again (a backup taken before the upgrade), CR lost
	// without teardown, CR re-created without status.
	admin, err := e.r.ensureAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s3admin, err := e.r.newS3Admin(ctx, legacy.bucket, admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s3admin.SetBucketTags(ctx, legacy.bucket, e.r.ownershipTags(b)); err != nil {
		t.Fatal(err)
	}
	cur := e.getBucket(t, ns, crName)
	controllerutil.RemoveFinalizer(cur, s3v1.BucketFinalizer)
	if err := e.k8s.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := e.k8s.Delete(ctx, cur); err != nil {
		t.Fatal(err)
	}
	restored := newBucketCR(ns, crName)
	if err := e.k8s.Create(ctx, restored); err != nil {
		t.Fatal(err)
	}
	e.reconcileTolerant(t, ns, crName, 2, 3)
	assertLegacyUntouched(t, ctx, e, restored, legacy)
	t.Log("OK: restore without status re-attached to the surviving group")

	// 3. Teardown releases exactly the seeded resources.
	cur = e.getBucket(t, ns, crName)
	if err := e.k8s.Delete(ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.reconcileTolerant(t, ns, crName, 1, 3)
	if exists, err := c.HasBucket(ctx, c.ProjectID(), legacy.bucket); err != nil || exists {
		t.Errorf("bucket after teardown exists=%v err=%v, want gone", exists, err)
	}
	groups, err := c.ListCredentialsGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.ID == legacy.groupID {
			t.Errorf("group %s still exists after teardown", g.ID)
		}
	}
	// The keys endpoint of a deleted group must answer 404: that is what
	// groupExists reads as "gone", and the only thing that lets a Bucket
	// create a replacement for a group it once recorded.
	if exists, err := e.r.groupExists(ctx, legacy.groupID); err != nil || exists {
		t.Errorf("groupExists(deleted %s) = %v, %v; want false, nil", legacy.groupID, exists, err)
	}
	var sec corev1.Secret
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: b.Spec.SecretRef.Name}, &sec); err == nil {
		t.Error("Secret still present after teardown")
	}
	t.Log("OK: teardown released bucket, group and Secret")
}
