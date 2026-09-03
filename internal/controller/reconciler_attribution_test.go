package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// Offline tests for the credentials-group attribution of ADR 0002: a bucket
// is bound to its group by a bucket tag, a legacy bucket is migrated from its
// own policy, and neither a colliding display name nor the recorded status can
// make the operator adopt, delete or re-create anything that is not the
// bucket's own.

const sidWorkload = "workload-objects-only"

// stripGroupTag removes the credentials-group tag from a provisioned bucket,
// leaving the tag set an operator predating ADR 0002 left behind.
func (e *testEnv) stripGroupTag(t *testing.T, bucket string) {
	t.Helper()
	tags := e.fake.Tags(bucket)
	if tags[tagCredentialsGroupID] == "" {
		t.Fatalf("premise: bucket %q carries no %s tag", bucket, tagCredentialsGroupID)
	}
	delete(tags, tagCredentialsGroupID)
	delete(tags, tagCredentialsGroupURN)
	e.fake.SetTags(bucket, tags)
}

// secretData returns the data of the named Secret.
func (e *testEnv) secretData(t *testing.T, ns, name string) map[string][]byte {
	t.Helper()
	var sec corev1.Secret
	if err := e.k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
		t.Fatalf("get secret %s/%s: %v", ns, name, err)
	}
	return sec.Data
}

// cloudSnapshot is the fake's cloud state that a migration must leave alone:
// which buckets and groups exist, and how many create/delete calls were made.
type cloudSnapshot struct {
	buckets      []string
	groupIDs     []string
	createBucket int
	deleteBucket int
	createGroup  int
	deleteGroup  int
	createKey    int
	deleteKey    int
}

func (e *testEnv) snapshot() cloudSnapshot {
	return cloudSnapshot{
		buckets:      e.fake.BucketNames(),
		groupIDs:     e.fake.GroupIDs(),
		createBucket: e.fake.Calls("CreateBucket"),
		deleteBucket: e.fake.Calls("DeleteBucket"),
		createGroup:  e.fake.Calls("CreateGroup"),
		deleteGroup:  e.fake.Calls("DeleteGroup"),
		createKey:    e.fake.Calls("CreateKey"),
		deleteKey:    e.fake.Calls("DeleteKey"),
	}
}

// assertUntouched fails when anything in the cloud was created or deleted
// since the snapshot.
func (e *testEnv) assertUntouched(t *testing.T, before cloudSnapshot) {
	t.Helper()
	if after := e.snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("cloud state changed:\n before %+v\n after  %+v", before, after)
	}
}

// TestGroupAttributionSurvivesNameCollision is the regression test for
// security finding 1 (ADR 0001 residual risk, closed by ADR 0002): two Buckets
// whose derived group display names collide end up with two groups, and neither
// provisioning nor tearing down the second touches the first's key.
func TestGroupAttributionSurvivesNameCollision(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	victim := e.provision(t, newBucketCR("gitlab", "gitlab-artifacts"))
	attacker := newBucketCR("gitlab-gitlab", "artifacts787ngo")
	if workloadGroupName(victim) != workloadGroupName(attacker) {
		t.Fatalf("premise: names do not collide: %q vs %q", workloadGroupName(victim), workloadGroupName(attacker))
	}
	victimSecret := e.secretData(t, "gitlab", "gitlab-artifacts-s3")
	before := e.snapshot()

	got := e.provision(t, attacker)

	if got.Status.CredentialsGroupID == victim.Status.CredentialsGroupID {
		t.Fatalf("second Bucket adopted the first's credentials group %s", victim.Status.CredentialsGroupID)
	}
	if n := e.fake.KeyCountByID(victim.Status.CredentialsGroupID); n != 1 {
		t.Errorf("victim group key count = %d, want 1 (untouched)", n)
	}
	if v := e.getBucket(t, "gitlab", "gitlab-artifacts"); v.Status.AccessKeyID != victim.Status.AccessKeyID {
		t.Errorf("victim access key changed: %q -> %q", victim.Status.AccessKeyID, v.Status.AccessKeyID)
	}
	if data := e.secretData(t, "gitlab", "gitlab-artifacts-s3"); !reflect.DeepEqual(data, victimSecret) {
		t.Error("victim Secret changed")
	}
	if after := e.snapshot(); after.deleteKey != before.deleteKey || after.deleteGroup != before.deleteGroup {
		t.Errorf("provisioning the second Bucket deleted %d keys and %d groups",
			after.deleteKey-before.deleteKey, after.deleteGroup-before.deleteGroup)
	}
	if e.fake.KeyCountByID(got.Status.CredentialsGroupID) != 1 {
		t.Error("second Bucket has no key in its own group")
	}

	// Each policy trusts its own group only.
	p := policyPrincipals(t, e.fake.Policy("artifacts787ngo"))
	if w := p[sidWorkload]; len(w) != 1 || w[0] != got.Status.CredentialsGroupURN {
		t.Errorf("second Bucket's workload principal = %v, want its own %s", w, got.Status.CredentialsGroupURN)
	}
	if contains(p[sidDenyAll], victim.Status.CredentialsGroupURN) {
		t.Error("second Bucket's policy exempts the victim's group")
	}

	// Each bucket names its own group in its tag.
	if tag := e.fake.Tags("gitlab-artifacts")[tagCredentialsGroupID]; tag != victim.Status.CredentialsGroupID {
		t.Errorf("victim bucket tag = %q, want %s", tag, victim.Status.CredentialsGroupID)
	}
	if tag := e.fake.Tags("artifacts787ngo")[tagCredentialsGroupID]; tag != got.Status.CredentialsGroupID {
		t.Errorf("second bucket tag = %q, want %s", tag, got.Status.CredentialsGroupID)
	}

	// Tearing the second Bucket down releases only its own group.
	if err := e.k8s.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "gitlab-gitlab", "artifacts787ngo", 1)
	if n := e.fake.KeyCountByID(victim.Status.CredentialsGroupID); n != 1 {
		t.Errorf("teardown of the second Bucket touched the victim group (keys %d)", n)
	}
	if n := e.fake.KeyCountByID(got.Status.CredentialsGroupID); n != -1 {
		t.Errorf("second Bucket's group not released (keys %d)", n)
	}
}

const sidDenyAll = "deny-all-except-admin-and-workload"

// TestGroupAttributionMigratesLegacyBucket models the first reconcile after the
// upgrade to ADR 0002: a bucket provisioned before the credentials-group tag
// existed keeps its bucket, its group, its key and its Secret. The only change
// is the tag, and the migration is reported.
func TestGroupAttributionMigratesLegacyBucket(t *testing.T) {
	e := newTestEnv(t)

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	e.stripGroupTag(t, "app-data")
	secret := e.secretData(t, "team-a", "app-data-s3")
	before := e.snapshot()

	e.reconcileN(t, "team-a", "app-data", 2)

	e.assertUntouched(t, before)
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.CredentialsGroupID != b.Status.CredentialsGroupID {
		t.Errorf("credentials group changed: %s -> %s", b.Status.CredentialsGroupID, got.Status.CredentialsGroupID)
	}
	if got.Status.AccessKeyID != b.Status.AccessKeyID {
		t.Errorf("access key changed: %s -> %s", b.Status.AccessKeyID, got.Status.AccessKeyID)
	}
	if data := e.secretData(t, "team-a", "app-data-s3"); !reflect.DeepEqual(data, secret) {
		t.Error("Secret changed during migration")
	}
	if tag := e.fake.Tags("app-data")[tagCredentialsGroupID]; tag != b.Status.CredentialsGroupID {
		t.Errorf("bucket tag = %q, want %s", tag, b.Status.CredentialsGroupID)
	}
	if !e.rec.hasReason(reasonGroupAttributed) {
		t.Errorf("no %s event; events: %+v", reasonGroupAttributed, e.rec.events)
	}
}

// TestGroupAttributionSurvivesRestoreWithoutStatus models a disaster-recovery
// restore of a legacy Bucket: the CR comes back with a fresh UID and no status,
// the Secret comes back with it, and the cloud still holds bucket, group, key
// and policy. The operator must re-attach to that group through the bucket, not
// mint a new one.
func TestGroupAttributionSurvivesRestoreWithoutStatus(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	e.stripGroupTag(t, "app-data")
	secret := e.secretData(t, "team-a", "app-data-s3")

	// Lose the CR without a teardown, as a lost cluster would.
	controllerutil.RemoveFinalizer(b, s3v1.BucketFinalizer)
	if err := e.k8s.Update(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := e.k8s.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	before := e.snapshot()

	restored := e.provision(t, newBucketCR("team-a", "app-data"))

	e.assertUntouched(t, before)
	if restored.Status.CredentialsGroupID != b.Status.CredentialsGroupID {
		t.Errorf("restored Bucket got group %s, want the surviving %s", restored.Status.CredentialsGroupID, b.Status.CredentialsGroupID)
	}
	if restored.Status.AccessKeyID != b.Status.AccessKeyID {
		t.Errorf("access key changed on restore: %s -> %s", b.Status.AccessKeyID, restored.Status.AccessKeyID)
	}
	if data := e.secretData(t, "team-a", "app-data-s3"); !reflect.DeepEqual(data, secret) {
		t.Error("Secret changed on restore")
	}
	if tag := e.fake.Tags("app-data")[tagCredentialsGroupID]; tag != b.Status.CredentialsGroupID {
		t.Errorf("bucket tag = %q, want %s", tag, b.Status.CredentialsGroupID)
	}
}

// TestGroupAttributionReplacesDeletedGroup covers a group removed out of band:
// the tag names a group that no longer exists, the policy names the same
// vanished principal, so a fresh group is created, the tag overwritten and the
// workload credential re-issued.
func TestGroupAttributionReplacesDeletedGroup(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	old := b.Status.CredentialsGroupID
	if err := e.r.Stackit.DeleteAllAccessKeys(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Stackit.DeleteCredentialsGroup(ctx, old); err != nil {
		t.Fatal(err)
	}

	e.reconcileN(t, "team-a", "app-data", 2)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.CredentialsGroupID == old || got.Status.CredentialsGroupID == "" {
		t.Fatalf("credentials group = %q, want a fresh one (old %s)", got.Status.CredentialsGroupID, old)
	}
	if tag := e.fake.Tags("app-data")[tagCredentialsGroupID]; tag != got.Status.CredentialsGroupID {
		t.Errorf("bucket tag = %q, want %s", tag, got.Status.CredentialsGroupID)
	}
	if n := e.fake.KeyCountByID(got.Status.CredentialsGroupID); n != 1 {
		t.Errorf("fresh group key count = %d, want 1", n)
	}
	if w := policyPrincipals(t, e.fake.Policy("app-data"))[sidWorkload]; len(w) != 1 || w[0] != got.Status.CredentialsGroupURN {
		t.Errorf("policy workload principal = %v, want %s", w, got.Status.CredentialsGroupURN)
	}
}

// TestGroupAttributionRollsBackUntaggedGroup pins the crash window between
// creating a group and writing its tag: when the tag write fails, the group is
// deleted again so retries do not leave a trail of empty groups, and the next
// pass provisions cleanly.
func TestGroupAttributionRollsBackUntaggedGroup(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// A pre-existing, owned, empty bucket: ensureBucket adopts it without a tag
	// write, so the injected tagging failure hits the group attribution.
	b := newBucketCR("team-a", "app-data")
	e.fake.SeedBucket("app-data", e.r.ownershipTags(b))
	if err := e.k8s.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.fake.FailNext("S3PutTagging", 403)

	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded although the tag write failed")
	}
	// Only the shared admin group may exist; the untagged workload group was
	// rolled back.
	if ids := e.fake.GroupIDs(); len(ids) != 1 {
		t.Errorf("groups after failed tag write = %v, want only the admin group", ids)
	}
	if n := e.fake.Calls("DeleteGroup"); n != 1 {
		t.Errorf("DeleteGroup calls = %d, want 1 (rollback)", n)
	}

	e.reconcileN(t, "team-a", "app-data", 2)
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if tag := e.fake.Tags("app-data")[tagCredentialsGroupID]; tag != got.Status.CredentialsGroupID {
		t.Errorf("bucket tag = %q, want %s", tag, got.Status.CredentialsGroupID)
	}
}

// TestGroupAttributionPolicyReadFailureCreatesNothing pins that a failure to
// read a legacy bucket's policy is an error, not "no policy": otherwise the
// operator would mint a second group for a bucket that has one and rotate its
// workload out of a working credential.
func TestGroupAttributionPolicyReadFailureCreatesNothing(t *testing.T) {
	e := newTestEnv(t)

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	e.stripGroupTag(t, "app-data")
	before := e.snapshot()
	e.fake.FailNext("S3GetPolicy", 403)

	if _, err := e.reconcile(t, "team-a", "app-data"); err == nil {
		t.Fatal("reconcile succeeded although the policy read failed")
	}
	e.assertUntouched(t, before)

	e.reconcileN(t, "team-a", "app-data", 1)
	e.assertUntouched(t, before)
	if got := e.getBucket(t, "team-a", "app-data"); got.Status.CredentialsGroupID != b.Status.CredentialsGroupID {
		t.Errorf("credentials group changed: %s -> %s", b.Status.CredentialsGroupID, got.Status.CredentialsGroupID)
	}
}

// TestTeardownAttributesGroupViaPolicy: a legacy Bucket (no tag, no recorded
// group id) deleted right after the upgrade still releases its group, found
// through its own policy.
func TestTeardownAttributesGroupViaPolicy(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	gid := b.Status.CredentialsGroupID
	e.stripGroupTag(t, "app-data")
	b.Status.CredentialsGroupID = ""
	if err := e.k8s.Status().Update(ctx, b); err != nil {
		t.Fatal(err)
	}

	if err := e.k8s.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if n := e.fake.KeyCountByID(gid); n != -1 {
		t.Errorf("workload group not released via policy attribution (keys %d)", n)
	}
	if got := e.fake.BucketNames(); len(got) != 0 {
		t.Errorf("cloud buckets after teardown = %v, want none", got)
	}
}

// TestTeardownLeavesUnattributableGroup pins ADR 0002 D4 from the other side:
// when the bucket attributes no group, the group recorded in status is NOT
// deleted on the strength of the status alone — status is not a source of
// attribution — and the operator says so.
func TestTeardownLeavesUnattributableGroup(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	gid := b.Status.CredentialsGroupID
	e.stripGroupTag(t, "app-data")
	e.fake.SetPolicy("app-data", "")

	if err := e.k8s.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if n := e.fake.KeyCountByID(gid); n != 1 {
		t.Errorf("group %s deleted on the strength of status alone (keys %d)", gid, n)
	}
	if got := e.fake.BucketNames(); len(got) != 0 {
		t.Errorf("owned empty bucket not deleted: %v", got)
	}
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data"}, &s3v1.Bucket{}); !apierrors.IsNotFound(err) {
		t.Errorf("bucket CR still present (err %v)", err)
	}
	if !e.rec.hasReason(reasonGroupNotAttributable) {
		t.Errorf("no %s event; events: %+v", reasonGroupNotAttributable, e.rec.events)
	}
}

// TestTeardownForeignBucketReleasesNoGroup: when the bucket under the Bucket's
// name is not the Bucket's (ownership tags differ), teardown must not read a
// group off it, whatever the tag says.
func TestTeardownForeignBucketReleasesNoGroup(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	gid := b.Status.CredentialsGroupID
	tags := e.fake.Tags("app-data")
	tags[tagOwnershipOwner] = "other-namespace/app-data"
	e.fake.SetTags("app-data", tags)

	if err := e.k8s.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if n := e.fake.KeyCountByID(gid); n != 1 {
		t.Errorf("group read off a foreign bucket was released (keys %d)", n)
	}
	if got := e.fake.BucketNames(); len(got) != 1 {
		t.Errorf("foreign bucket deleted: %v", got)
	}
	if !e.rec.hasReason(reasonGroupNotAttributable) {
		t.Errorf("no %s event; events: %+v", reasonGroupNotAttributable, e.rec.events)
	}
}

// TestReadGrantResolvesLegacyGrantee: a grantor reconciled before its grantee
// was migrated still resolves the grant through the grantee's policy, so the
// upgrade never drops a reader from a policy; the grantee's tag is written on
// the way.
func TestReadGrantResolvesLegacyGrantee(t *testing.T) {
	e := newTestEnv(t)

	reader := e.provision(t, newBucketCR("gitlab", "backups"))
	e.stripGroupTag(t, "backups")

	data := e.provision(t, grantorCR("gitlab", "artifacts", "backups"))

	readers := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]
	if len(readers) != 1 || readers[0] != reader.Status.CredentialsGroupURN {
		t.Errorf("reader principals = %v, want [%s]", readers, reader.Status.CredentialsGroupURN)
	}
	if len(data.Status.GrantedReadTo) != 1 || data.Status.GrantedReadTo[0] != "backups" {
		t.Errorf("status.grantedReadTo = %v, want [backups]", data.Status.GrantedReadTo)
	}
	if tag := e.fake.Tags("backups")[tagCredentialsGroupID]; tag != reader.Status.CredentialsGroupID {
		t.Errorf("grantee bucket tag = %q, want %s", tag, reader.Status.CredentialsGroupID)
	}
}

// TestReadGrantRefusedForForeignGranteeBucket: the grantee's physical bucket
// must carry the grantee's ownership tags, otherwise no group is read off it
// and nothing is written to it.
func TestReadGrantRefusedForForeignGranteeBucket(t *testing.T) {
	e := newTestEnv(t)

	e.provision(t, newBucketCR("gitlab", "backups"))
	tags := e.fake.Tags("backups")
	tags[tagOwnershipOwner] = "elsewhere/backups"
	e.fake.SetTags("backups", tags)

	data := e.provision(t, grantorCR("gitlab", "artifacts", "backups"))

	if _, has := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]; has {
		t.Error("grant applied although the grantee's bucket is not the grantee's")
	}
	if len(data.Status.GrantedReadTo) != 0 {
		t.Errorf("status.grantedReadTo = %v, want empty", data.Status.GrantedReadTo)
	}
	if data.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase = %q, want Ready: a refused grant must not fail the grantor", data.Status.Phase)
	}
	if !e.rec.hasReason(reasonReadGrantPending) {
		t.Errorf("no %s event; events: %+v", reasonReadGrantPending, e.rec.events)
	}
	if got := e.fake.Tags("backups"); got[tagOwnershipOwner] != "elsewhere/backups" {
		t.Errorf("foreign bucket's tags were rewritten: %v", got)
	}
}

// TestGroupAttributionSurvivesListingLag: the pass after provisioning lists the
// project without the group created a moment ago (the listing lags behind the
// create, as observed on the real API). The bucket tags plus the keys-endpoint
// probe carry the pass; no second group, no rotation.
func TestGroupAttributionSurvivesListingLag(t *testing.T) {
	e := newTestEnv(t)

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	secret := e.secretData(t, "team-a", "app-data-s3")
	before := e.snapshot()
	e.fake.OmitFromNextListing(b.Status.CredentialsGroupID)

	e.reconcileN(t, "team-a", "app-data", 1)

	e.assertUntouched(t, before)
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady || got.Status.CredentialsGroupID != b.Status.CredentialsGroupID {
		t.Errorf("phase %q group %s, want Ready with %s", got.Status.Phase, got.Status.CredentialsGroupID, b.Status.CredentialsGroupID)
	}
	if data := e.secretData(t, "team-a", "app-data-s3"); !reflect.DeepEqual(data, secret) {
		t.Error("Secret changed under a lagging listing")
	}
}

// TestReadGrantSurvivesListingLag: a grantor whose listing does not show the
// grantee's group still resolves the grant through the grantee's tags.
func TestReadGrantSurvivesListingLag(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	reader := e.provision(t, newBucketCR("gitlab", "backups"))
	data := grantorCR("gitlab", "artifacts", "backups")
	if err := e.k8s.Create(ctx, data); err != nil {
		t.Fatal(err)
	}
	e.fake.OmitFromNextListing(reader.Status.CredentialsGroupID)

	e.reconcileN(t, "gitlab", "artifacts", 1)

	readers := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]
	if len(readers) != 1 || readers[0] != reader.Status.CredentialsGroupURN {
		t.Errorf("reader principals = %v, want [%s]", readers, reader.Status.CredentialsGroupURN)
	}
}

// TestTeardownSurvivesListingLag: teardown releases the tagged group without
// needing the listing to show it.
func TestTeardownSurvivesListingLag(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	gid := b.Status.CredentialsGroupID
	e.fake.OmitFromNextListing(gid)

	if err := e.k8s.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if n := e.fake.KeyCountByID(gid); n != -1 {
		t.Errorf("group not released under a lagging listing (keys %d)", n)
	}
}

// TestGroupAttributionWaitsForStaleBucketRead: the bucket shows neither tags
// nor policy (a stale read right after they were written) while the status
// records a group that still exists. The operator must wait, not create; once
// the bucket is readable again it continues with the same group and key.
func TestGroupAttributionWaitsForStaleBucketRead(t *testing.T) {
	e := newTestEnv(t)

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	secret := e.secretData(t, "team-a", "app-data-s3")
	tagged := e.fake.Tags("app-data")
	before := e.snapshot()
	e.fake.SetTags("app-data", e.r.ownershipTags(b))
	e.fake.SetPolicy("app-data", "")

	_, err := e.reconcile(t, "team-a", "app-data")
	if !errors.Is(err, errAttributionLagging) {
		t.Fatalf("reconcile error = %v, want errAttributionLagging", err)
	}
	e.assertUntouched(t, before)

	e.fake.SetTags("app-data", tagged)
	e.reconcileN(t, "team-a", "app-data", 1)
	e.assertUntouched(t, before)
	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady || got.Status.CredentialsGroupID != b.Status.CredentialsGroupID {
		t.Errorf("phase %q group %s, want Ready with %s", got.Status.Phase, got.Status.CredentialsGroupID, b.Status.CredentialsGroupID)
	}
	if data := e.secretData(t, "team-a", "app-data-s3"); !reflect.DeepEqual(data, secret) {
		t.Error("Secret changed across the stale read")
	}
}

// TestGroupAttributionRecreatesWhenRecordedGroupGone: the same unattributed
// bucket, but the recorded group really is gone — creating is right.
func TestGroupAttributionRecreatesWhenRecordedGroupGone(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	old := b.Status.CredentialsGroupID
	e.fake.SetTags("app-data", e.r.ownershipTags(b))
	e.fake.SetPolicy("app-data", "")
	if err := e.r.Stackit.DeleteAllAccessKeys(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Stackit.DeleteCredentialsGroup(ctx, old); err != nil {
		t.Fatal(err)
	}

	e.reconcileN(t, "team-a", "app-data", 2)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady || got.Status.CredentialsGroupID == old || got.Status.CredentialsGroupID == "" {
		t.Fatalf("phase %q group %q, want Ready with a fresh group (old %s)", got.Status.Phase, got.Status.CredentialsGroupID, old)
	}
	if tag := e.fake.Tags("app-data")[tagCredentialsGroupID]; tag != got.Status.CredentialsGroupID {
		t.Errorf("bucket tag = %q, want %s", tag, got.Status.CredentialsGroupID)
	}
}

// TestGroupAttributionFreshBucketSkipsGuard: a bucket deleted out of band is
// re-created by the operator; the recorded group still exists but belongs to
// the previous incarnation, so a fresh group is created and the old one is
// left behind (documented in ADR 0002).
func TestGroupAttributionFreshBucketSkipsGuard(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	old := b.Status.CredentialsGroupID
	if err := e.r.Stackit.DeleteBucket(ctx, "app-data"); err != nil {
		t.Fatal(err)
	}

	e.reconcileN(t, "team-a", "app-data", 2)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase != s3v1.PhaseReady || got.Status.CredentialsGroupID == old || got.Status.CredentialsGroupID == "" {
		t.Fatalf("phase %q group %q, want Ready with a fresh group (old %s)", got.Status.Phase, got.Status.CredentialsGroupID, old)
	}
	if n := e.fake.KeyCountByID(old); n != 1 {
		t.Errorf("previous group keys = %d, want 1 (left behind, never adopted)", n)
	}
	if e.fake.KeyCountByID(got.Status.CredentialsGroupID) != 1 {
		t.Error("fresh group has no key")
	}
}

// TestTeardownSecondPassIsSilent: a teardown that is requeued after bucket and
// group are already gone (as a conflict on the finalizer removal does) must not
// warn about a group that no longer exists.
func TestTeardownSecondPassIsSilent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	b := e.provision(t, newBucketCR("team-a", "app-data"))
	gid := b.Status.CredentialsGroupID
	if err := e.r.Stackit.DeleteAllAccessKeys(ctx, gid); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Stackit.DeleteCredentialsGroup(ctx, gid); err != nil {
		t.Fatal(err)
	}
	if err := e.r.Stackit.DeleteBucket(ctx, "app-data"); err != nil {
		t.Fatal(err)
	}

	if err := e.k8s.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.reconcileN(t, "team-a", "app-data", 1)

	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "app-data"}, &s3v1.Bucket{}); !apierrors.IsNotFound(err) {
		t.Errorf("bucket CR still present (err %v)", err)
	}
	if e.rec.hasReason(reasonGroupNotAttributable) {
		t.Errorf("warned about a group that no longer exists; events: %+v", e.rec.events)
	}
}
