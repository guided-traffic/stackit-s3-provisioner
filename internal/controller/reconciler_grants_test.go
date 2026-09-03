package controller

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// policyPrincipals decodes a bucket policy and returns, per statement Sid, the
// principals it names (Principal for statements 2/3, NotPrincipal for
// statement 1).
func policyPrincipals(t *testing.T, policy string) map[string][]string {
	t.Helper()
	if policy == "" {
		t.Fatal("bucket has no policy")
	}
	var doc struct {
		Statement []struct {
			Sid          string                  `json:"Sid"`
			Principal    *struct{ AWS any }      `json:"Principal"`
			NotPrincipal *struct{ AWS []string } `json:"NotPrincipal"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, policy)
	}
	out := make(map[string][]string, len(doc.Statement))
	for _, s := range doc.Statement {
		switch {
		case s.NotPrincipal != nil:
			out[s.Sid] = s.NotPrincipal.AWS
		case s.Principal != nil:
			switch p := s.Principal.AWS.(type) {
			case string:
				out[s.Sid] = []string{p}
			case []any:
				for _, e := range p {
					str, _ := e.(string)
					out[s.Sid] = append(out[s.Sid], str)
				}
			}
		}
	}
	return out
}

const sidReaders = "granted-readers-read-only"

// grantorCR returns a Bucket CR that grants read access to the named siblings.
func grantorCR(ns, name string, grantees ...string) *s3v1.Bucket {
	b := newBucketCR(ns, name)
	for _, g := range grantees {
		b.Spec.GrantReadAccess = append(b.Spec.GrantReadAccess, s3v1.LocalBucketReference{Name: g})
	}
	return b
}

// TestReadGrantAppliedToPolicy is the ticket's core acceptance at the offline
// level: a data bucket that grants read access to a sibling ends up with that
// sibling's workload group as a reader principal in its policy, and the sibling
// is recorded in status.
func TestReadGrantAppliedToPolicy(t *testing.T) {
	e := newTestEnv(t)

	// The reader must exist first so its credentials group is provisioned.
	reader := e.provision(t, newBucketCR("gitlab", "backups"))

	data := e.provision(t, grantorCR("gitlab", "artifacts", "backups"))

	principals := policyPrincipals(t, e.fake.Policy("artifacts"))
	readers, ok := principals[sidReaders]
	if !ok {
		t.Fatalf("policy has no reader statement: %s", e.fake.Policy("artifacts"))
	}
	if len(readers) != 1 || readers[0] != reader.Status.CredentialsGroupURN {
		t.Errorf("reader principals = %v, want [%s]", readers, reader.Status.CredentialsGroupURN)
	}

	// The reader must also be exempt from the blanket deny, otherwise
	// statement 1 would deny it everything regardless.
	exempt := principals["deny-all-except-admin-and-workload"]
	if !contains(exempt, reader.Status.CredentialsGroupURN) {
		t.Errorf("reader %s missing from NotPrincipal %v", reader.Status.CredentialsGroupURN, exempt)
	}

	if len(data.Status.GrantedReadTo) != 1 || data.Status.GrantedReadTo[0] != "backups" {
		t.Errorf("status.grantedReadTo = %v, want [backups]", data.Status.GrantedReadTo)
	}

	// The reader's own bucket must not have gained a reader statement.
	if _, has := policyPrincipals(t, e.fake.Policy("backups"))[sidReaders]; has {
		t.Error("grant leaked onto the grantee's own bucket")
	}
}

// TestReadGrantUnresolvedDoesNotBlockReady covers the availability rule: a data
// bucket must reach Ready even when the consumer it grants to does not exist,
// and it must say so via an event instead of failing.
func TestReadGrantUnresolvedDoesNotBlockReady(t *testing.T) {
	e := newTestEnv(t)

	data := e.provision(t, grantorCR("gitlab", "artifacts", "backups"))

	if data.Status.Phase != s3v1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", data.Status.Phase)
	}
	if len(data.Status.GrantedReadTo) != 0 {
		t.Errorf("status.grantedReadTo = %v, want empty", data.Status.GrantedReadTo)
	}
	if _, has := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]; has {
		t.Error("policy names a reader although the grantee does not exist")
	}
	if !e.rec.hasReason(reasonReadGrantPending) {
		t.Errorf("no %s event emitted; events: %+v", reasonReadGrantPending, e.rec.events)
	}
}

// TestReadGrantAppliedWhenGranteeAppears verifies the self-healing half: the
// grant is picked up on a later reconcile once the referenced Bucket has been
// provisioned, without the grantor's spec changing.
func TestReadGrantAppliedWhenGranteeAppears(t *testing.T) {
	e := newTestEnv(t)

	e.provision(t, grantorCR("gitlab", "artifacts", "backups"))
	if _, has := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]; has {
		t.Fatal("reader present before the grantee exists")
	}

	reader := e.provision(t, newBucketCR("gitlab", "backups"))
	e.reconcileN(t, "gitlab", "artifacts", 1)

	readers := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]
	if len(readers) != 1 || readers[0] != reader.Status.CredentialsGroupURN {
		t.Errorf("reader principals = %v, want [%s]", readers, reader.Status.CredentialsGroupURN)
	}
	if got := e.getBucket(t, "gitlab", "artifacts").Status.GrantedReadTo; len(got) != 1 {
		t.Errorf("status.grantedReadTo = %v, want [backups]", got)
	}
}

// TestReadGrantRevokedOnGranteeDeletion pins the revocation path: once the
// referenced Bucket is gone, the next reconcile of the grantor removes it from
// the policy again.
func TestReadGrantRevokedOnGranteeDeletion(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	reader := e.provision(t, newBucketCR("gitlab", "backups"))
	e.provision(t, grantorCR("gitlab", "artifacts", "backups"))
	if len(policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]) != 1 {
		t.Fatal("grant was not applied in the first place")
	}

	// Delete the grantee for real (finalizer teardown, then object removal).
	if err := e.k8s.Delete(ctx, reader); err != nil {
		t.Fatalf("delete grantee: %v", err)
	}
	e.reconcileN(t, "gitlab", "backups", 1)
	var gone s3v1.Bucket
	if err := e.k8s.Get(ctx, types.NamespacedName{Namespace: "gitlab", Name: "backups"}, &gone); err == nil {
		t.Fatalf("grantee still present after teardown: %+v", gone.Finalizers)
	}

	e.reconcileN(t, "gitlab", "artifacts", 1)
	if _, has := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]; has {
		t.Error("reader still in policy after the grantee was deleted")
	}
	if got := e.getBucket(t, "gitlab", "artifacts").Status.GrantedReadTo; len(got) != 0 {
		t.Errorf("status.grantedReadTo = %v, want empty", got)
	}
}

// TestReadGrantIsNamespaceScoped is the isolation guarantee from the ticket: a
// Bucket with the same CR name in another namespace must never be resolved,
// even though StackIT credentials groups live in one flat project namespace.
func TestReadGrantIsNamespaceScoped(t *testing.T) {
	e := newTestEnv(t)

	// Same CR name, different namespace — must not be granted anything.
	foreign := e.provision(t, newBucketCR("other-team", "backups"))
	e.provision(t, grantorCR("gitlab", "artifacts", "backups"))

	principals := policyPrincipals(t, e.fake.Policy("artifacts"))
	if readers, has := principals[sidReaders]; has {
		t.Errorf("cross-namespace grant resolved: %v", readers)
	}
	if contains(principals["deny-all-except-admin-and-workload"], foreign.Status.CredentialsGroupURN) {
		t.Error("foreign-namespace workload was exempted from the blanket deny")
	}
	if !e.rec.hasReason(reasonReadGrantPending) {
		t.Error("expected a pending event for the unresolvable cross-namespace reference")
	}
}

// TestReadGrantSelfReferenceIgnored covers the defense behind the CEL rule: a
// Bucket that somehow carries itself in spec.grantReadAccess (e.g. written
// before the rule existed) must not gain a second, narrower Deny on its own
// workload group, which would strip its write access.
func TestReadGrantSelfReferenceIgnored(t *testing.T) {
	e := newTestEnv(t)

	b := e.provision(t, grantorCR("gitlab", "artifacts", "artifacts"))

	principals := policyPrincipals(t, e.fake.Policy("artifacts"))
	if readers, has := principals[sidReaders]; has {
		t.Errorf("self-grant produced a reader statement: %v", readers)
	}
	if b.Status.Phase != s3v1.PhaseReady {
		t.Errorf("phase = %q, want Ready", b.Status.Phase)
	}
	if len(b.Status.GrantedReadTo) != 0 {
		t.Errorf("status.grantedReadTo = %v, want empty", b.Status.GrantedReadTo)
	}
}

// TestReadGrantAdminNeverGranted is the lockout guard at reconciler level: the
// admin group's URN must never appear as a reader, no matter what. The admin
// group is not backed by a Bucket CR, so the only way it could get in is a
// forged reference; assert the resulting policy keeps the admin unrestricted.
func TestReadGrantAdminNeverGranted(t *testing.T) {
	e := newTestEnv(t)

	e.provision(t, newBucketCR("gitlab", "backups"))
	e.provision(t, grantorCR("gitlab", "artifacts", "backups"))

	admin, err := e.r.ensureAdmin(context.Background())
	if err != nil {
		t.Fatalf("ensureAdmin: %v", err)
	}
	principals := policyPrincipals(t, e.fake.Policy("artifacts"))
	if contains(principals[sidReaders], admin.urn) {
		t.Fatalf("admin URN %q ended up as a reader: %v", admin.urn, principals[sidReaders])
	}
	if !contains(principals["deny-all-except-admin-and-workload"], admin.urn) {
		t.Errorf("admin URN missing from NotPrincipal: %v", principals["deny-all-except-admin-and-workload"])
	}
}

// TestReadGrantMultipleGrantees checks that several grants on one bucket all
// land, which is the GitLab shape (one backup credential reading many data
// buckets, expressed as a grant on each data bucket).
func TestReadGrantMultipleGrantees(t *testing.T) {
	e := newTestEnv(t)

	r1 := e.provision(t, newBucketCR("gitlab", "backups"))
	r2 := e.provision(t, newBucketCR("gitlab", "auditor"))
	e.provision(t, grantorCR("gitlab", "artifacts", "backups", "auditor"))

	readers := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]
	if len(readers) != 2 {
		t.Fatalf("reader principals = %v, want 2 entries", readers)
	}
	for _, want := range []string{r1.Status.CredentialsGroupURN, r2.Status.CredentialsGroupURN} {
		if !contains(readers, want) {
			t.Errorf("reader %s missing from %v", want, readers)
		}
	}
	got := e.getBucket(t, "gitlab", "artifacts").Status.GrantedReadTo
	if len(got) != 2 {
		t.Errorf("status.grantedReadTo = %v, want 2 entries", got)
	}
}

// TestReadGrantStablePolicy guards the drift check: repeated reconciles of an
// unchanged grant must not rewrite the policy, or every resync would issue a
// PutBucketPolicy.
func TestReadGrantStablePolicy(t *testing.T) {
	e := newTestEnv(t)

	e.provision(t, newBucketCR("gitlab", "backups"))
	e.provision(t, grantorCR("gitlab", "artifacts", "backups"))
	first := e.fake.Policy("artifacts")

	e.reconcileN(t, "gitlab", "artifacts", 3)
	if got := e.fake.Policy("artifacts"); got != first {
		t.Errorf("policy changed across reconciles:\n%s\n%s", first, got)
	}
}

// TestBucketsGrantingTo checks the watch mapping that makes a grant self-heal:
// an event on a grantee must enqueue exactly the grantors in its own namespace.
func TestBucketsGrantingTo(t *testing.T) {
	e := newTestEnv(t,
		grantorCR("gitlab", "artifacts", "backups"),
		grantorCR("gitlab", "uploads", "backups"),
		grantorCR("gitlab", "unrelated", "someone-else"),
		grantorCR("other-team", "artifacts", "backups"),
		newBucketCR("gitlab", "backups"),
	)

	grantee := newBucketCR("gitlab", "backups")
	reqs := e.r.bucketsGrantingTo(context.Background(), grantee)

	got := make([]string, 0, len(reqs))
	for _, r := range reqs {
		got = append(got, r.Namespace+"/"+r.Name)
	}
	want := map[string]bool{"gitlab/artifacts": true, "gitlab/uploads": true}
	if len(got) != len(want) {
		t.Fatalf("enqueued %v, want %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected enqueue %q (want only %v)", g, want)
		}
	}
}

// TestBucketsGrantingTo_SkipsSelf makes sure a Bucket that names itself does not
// enqueue itself on every one of its own events, which would be a reconcile
// loop rather than a no-op.
func TestBucketsGrantingTo_SkipsSelf(t *testing.T) {
	e := newTestEnv(t, grantorCR("gitlab", "artifacts", "artifacts"))

	reqs := e.r.bucketsGrantingTo(context.Background(), newBucketCR("gitlab", "artifacts"))
	if len(reqs) != 0 {
		t.Errorf("self-reference enqueued %v, want none", reqs)
	}
}

// TestGranteeCredentialsPredicate pins the filter that keeps the second Bucket
// watch from looping. Ordinary status churn (clone progress, messages) must not
// pass; gaining a credentials group, or entering deletion, must.
func TestGranteeCredentialsPredicate(t *testing.T) {
	withURN := func(urn string) *s3v1.Bucket {
		b := newBucketCR("gitlab", "backups")
		b.Status.CredentialsGroupURN = urn
		return b
	}
	deleting := func() *s3v1.Bucket {
		b := withURN("urn:x")
		now := metav1.Now()
		b.DeletionTimestamp = &now
		return b
	}
	progress := func(p string) *s3v1.Bucket {
		b := withURN("urn:x")
		b.Status.Clone = &s3v1.CloneStatus{Progress: p}
		return b
	}

	tests := []struct {
		name     string
		old, new *s3v1.Bucket
		want     bool
	}{
		{"credentials group appears", withURN(""), withURN("urn:x"), true},
		{"credentials group changes", withURN("urn:x"), withURN("urn:y"), true},
		{"deletion starts", withURN("urn:x"), deleting(), true},
		{"unchanged", withURN("urn:x"), withURN("urn:x"), false},
		{"clone progress churn", progress("1 GiB"), progress("2 GiB"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := granteeCredentialsPredicate.Update(event.UpdateEvent{
				ObjectOld: tc.old, ObjectNew: tc.new,
			})
			if got != tc.want {
				t.Errorf("predicate = %v, want %v", got, tc.want)
			}
		})
	}

	if !granteeCredentialsPredicate.Create(event.CreateEvent{Object: withURN("urn:x")}) {
		t.Error("create events must pass: a new grantee has to reach its grantors")
	}
	if !granteeCredentialsPredicate.Delete(event.DeleteEvent{Object: withURN("urn:x")}) {
		t.Error("delete events must pass: a removed grantee has to be revoked")
	}
	if granteeCredentialsPredicate.Generic(event.GenericEvent{Object: withURN("urn:x")}) {
		t.Error("generic events must not pass")
	}
}

// contains reports whether s contains want.
func contains(s []string, want string) bool {
	for _, e := range s {
		if e == want {
			return true
		}
	}
	return false
}

// TestReadGrantUnaffectedByDuplicateDisplayName pins that a grant binds to the
// group the grantee's bucket attributes, not to a display name: STACKIT does
// not enforce unique names, and a second group carrying the grantee's name must
// neither hijack the grant nor block it.
func TestReadGrantUnaffectedByDuplicateDisplayName(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	reader := e.provision(t, newBucketCR("gitlab", "backups"))

	// A second group with the same display name. The control plane does not
	// enforce uniqueness, so this is reachable state, not a contrived one.
	dupName := workloadGroupName(reader)
	if _, _, err := e.r.Stackit.CreateCredentialsGroup(ctx, dupName); err != nil {
		t.Fatalf("create duplicate group %q: %v", dupName, err)
	}

	data := e.provision(t, grantorCR("gitlab", "artifacts", "backups"))

	readers := policyPrincipals(t, e.fake.Policy("artifacts"))[sidReaders]
	if len(readers) != 1 || readers[0] != reader.Status.CredentialsGroupURN {
		t.Errorf("reader principals = %v, want exactly the grantee's group %s", readers, reader.Status.CredentialsGroupURN)
	}
	if len(data.Status.GrantedReadTo) != 1 || data.Status.GrantedReadTo[0] != "backups" {
		t.Errorf("status.grantedReadTo = %v, want [backups]", data.Status.GrantedReadTo)
	}
}

// TestReadGrantHeldBackDuringClone pins the ordering rule that a granted reader
// must not see a bucket that is still being filled by a clone. The bucket's own
// workload is protected by holdSecretUntilCloned, but a reader already holds
// working credentials, so only the policy can hold it back.
func TestReadGrantHeldBackDuringClone(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloneSource(2)

	// The reader is provisioned first so its credentials group exists and the
	// grant would resolve if nothing held it back.
	reader := e.provision(t, newBucketCR("team-a", "backups"))

	b := e.newCloneBucketCR(t)
	b.Spec.GrantReadAccess = []s3v1.LocalBucketReference{{Name: "backups"}}
	e.startClone(t, b)

	if got := e.fake.Policy("app-data"); got == "" {
		t.Fatal("isolation policy not set before clone")
	}
	if _, has := policyPrincipals(t, e.fake.Policy("app-data"))[sidReaders]; has {
		t.Error("reader was granted access while the bucket was still cloning")
	}
	if got := e.getBucket(t, "team-a", "app-data").Status.GrantedReadTo; len(got) != 0 {
		t.Errorf("status.grantedReadTo = %v while cloning, want empty", got)
	}

	// Finish the clone. The same reconcile that observes completion must add the
	// readers, rather than leaving them for a later trigger.
	e.finishCloneJob(t, b, true, "")
	e.reconcileN(t, "team-a", "app-data", 1)

	readers := policyPrincipals(t, e.fake.Policy("app-data"))[sidReaders]
	if len(readers) != 1 || readers[0] != reader.Status.CredentialsGroupURN {
		t.Errorf("reader principals after clone = %v, want [%s]", readers, reader.Status.CredentialsGroupURN)
	}
	if got := e.getBucket(t, "team-a", "app-data").Status.GrantedReadTo; len(got) != 1 {
		t.Errorf("status.grantedReadTo after clone = %v, want [backups]", got)
	}
}

// TestGranteePublishesGroupURNBeforeReady checks the signal grantors watch for:
// a Bucket must publish status.credentialsGroupURN as soon as its credentials
// group exists, not only once it reaches Ready. A grantee that is itself still
// cloning never reaches Ready, and its grantors would otherwise wait for the
// drift resync instead of being woken by granteeCredentialsPredicate.
func TestGranteePublishesGroupURNBeforeReady(t *testing.T) {
	e := newTestEnv(t)
	e.seedCloneSource(1)

	b := e.newCloneBucketCR(t)
	e.startClone(t, b)

	got := e.getBucket(t, "team-a", "app-data")
	if got.Status.Phase == s3v1.PhaseReady {
		t.Fatalf("test premise broken: bucket reached Ready although its clone is pending")
	}
	if got.Status.CredentialsGroupURN == "" {
		t.Error("status.credentialsGroupURN is empty while cloning; grantors would never be woken")
	}
	if got.Status.CredentialsGroupID == "" {
		t.Error("status.credentialsGroupID is empty while cloning")
	}
}
