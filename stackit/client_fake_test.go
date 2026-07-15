package stackit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/guided-traffic/stackit-s3-provisioner/internal/stackitfake"
)

const (
	fakeProject = "11111111-2222-3333-4444-555555555555"
	fakeRegion  = "eu01"
)

// newFakeClient starts an in-memory StackIT fake and returns a Client bound to
// it. The fake is shut down via t.Cleanup.
func newFakeClient(t *testing.T) (*Client, *stackitfake.Server) {
	t.Helper()
	fake := stackitfake.New(fakeProject, fakeRegion)
	t.Cleanup(fake.Close)
	c, err := NewClientWithEndpoint(fakeProject, fakeRegion, fake.CP.URL)
	if err != nil {
		t.Fatalf("NewClientWithEndpoint: %v", err)
	}
	return c, fake
}

func TestClientAccessors(t *testing.T) {
	c, _ := newFakeClient(t)
	if got := c.ProjectID(); got != fakeProject {
		t.Errorf("ProjectID() = %q, want %q", got, fakeProject)
	}
	if got := c.Region(); got != fakeRegion {
		t.Errorf("Region() = %q, want %q", got, fakeRegion)
	}
}

func TestEnsureService(t *testing.T) {
	ctx := context.Background()

	t.Run("already enabled", func(t *testing.T) {
		c, _ := newFakeClient(t)
		if err := c.EnsureService(ctx); err != nil {
			t.Fatalf("EnsureService: %v", err)
		}
	})

	t.Run("enables when disabled", func(t *testing.T) {
		c, fake := newFakeClient(t)
		fake.DisableService()
		if err := c.EnsureService(ctx); err != nil {
			t.Fatalf("EnsureService: %v", err)
		}
		// Second call sees the enabled service.
		if err := c.EnsureService(ctx); err != nil {
			t.Fatalf("EnsureService (second): %v", err)
		}
	})

	t.Run("enable failure", func(t *testing.T) {
		c, fake := newFakeClient(t)
		fake.DisableService()
		fake.FailNext("EnableService", 500)
		if err := c.EnsureService(ctx); err == nil {
			t.Fatal("EnsureService succeeded, want error")
		}
	})
}

func TestBucketLifecycle(t *testing.T) {
	ctx := context.Background()
	c, fake := newFakeClient(t)

	if err := c.CreateBucket(ctx, "bkt1"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Creating the same bucket again is a 409.
	err := c.CreateBucket(ctx, "bkt1")
	if err == nil {
		t.Fatal("CreateBucket duplicate succeeded, want 409")
	}
	if got := StatusCode(err); got != 409 {
		t.Errorf("StatusCode(duplicate create) = %d, want 409", got)
	}

	names, err := c.ListBucketNames(ctx, fakeProject)
	if err != nil {
		t.Fatalf("ListBucketNames: %v", err)
	}
	if len(names) != 1 || names[0] != "bkt1" {
		t.Errorf("ListBucketNames = %v, want [b1]", names)
	}

	ok, err := c.HasBucket(ctx, fakeProject, "bkt1")
	if err != nil || !ok {
		t.Errorf("HasBucket(b1) = %v, %v; want true, nil", ok, err)
	}
	ok, err = c.HasBucket(ctx, fakeProject, "nope")
	if err != nil || ok {
		t.Errorf("HasBucket(nope) = %v, %v; want false, nil", ok, err)
	}

	// Cross-project access must be denied (Layer-1 fidelity).
	if _, err := c.ListBucketNames(ctx, "99999999-8888-7777-6666-555555555555"); StatusCode(err) != 403 {
		t.Errorf("foreign-project list: StatusCode = %d (err %v), want 403", StatusCode(err), err)
	}

	if err := c.WaitBucketVisible(ctx, "bkt1", time.Second); err != nil {
		t.Errorf("WaitBucketVisible(b1): %v", err)
	}
	if err := c.WaitBucketVisible(ctx, "ghost", 0); err == nil {
		t.Error("WaitBucketVisible(ghost) succeeded, want timeout error")
	}

	host, bucketURL, err := c.BucketConnInfo(ctx, "bkt1")
	if err != nil {
		t.Fatalf("BucketConnInfo: %v", err)
	}
	if host == "" || bucketURL != fake.S3.URL+"/bkt1" {
		t.Errorf("BucketConnInfo = (%q, %q), want host + %q", host, bucketURL, fake.S3.URL+"/bkt1")
	}
	endpoint, err := c.BucketEndpoint(ctx, "bkt1")
	if err != nil {
		t.Fatalf("BucketEndpoint: %v", err)
	}
	if endpoint != fake.S3.URL {
		t.Errorf("BucketEndpoint = %q, want %q", endpoint, fake.S3.URL)
	}
	if _, _, err := c.BucketConnInfo(ctx, "ghost"); err == nil {
		t.Error("BucketConnInfo(ghost) succeeded, want error")
	}
	if _, err := c.BucketEndpoint(ctx, "ghost"); err == nil {
		t.Error("BucketEndpoint(ghost) succeeded, want error")
	}

	if err := c.DeleteBucket(ctx, "bkt1"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if err := c.DeleteBucket(ctx, "bkt1"); StatusCode(err) != 404 {
		t.Errorf("DeleteBucket(gone): StatusCode = %d, want 404", StatusCode(err))
	}
}

func TestCredentialsGroupsAndKeys(t *testing.T) {
	ctx := context.Background()
	c, fake := newFakeClient(t)

	id, urn, err := c.CreateCredentialsGroup(ctx, "group-a")
	if err != nil || id == "" || urn == "" {
		t.Fatalf("CreateCredentialsGroup = (%q, %q, %v)", id, urn, err)
	}

	// Find by name: hit and miss.
	fid, furn, found, err := c.FindCredentialsGroupByName(ctx, "group-a")
	if err != nil || !found || fid != id || furn != urn {
		t.Errorf("FindCredentialsGroupByName(group-a) = (%q, %q, %v, %v), want (%q, %q, true, nil)", fid, furn, found, err, id, urn)
	}
	if _, _, found, err := c.FindCredentialsGroupByName(ctx, "missing"); err != nil || found {
		t.Errorf("FindCredentialsGroupByName(missing) found=%v err=%v, want false, nil", found, err)
	}

	// Ensure: reuses the existing group, creates a fresh one otherwise.
	eid, _, err := c.EnsureCredentialsGroup(ctx, "group-a")
	if err != nil || eid != id {
		t.Errorf("EnsureCredentialsGroup(group-a) = (%q, %v), want existing id %q", eid, err, id)
	}
	bid, _, err := c.EnsureCredentialsGroup(ctx, "group-b")
	if err != nil || bid == "" || bid == id {
		t.Errorf("EnsureCredentialsGroup(group-b) = (%q, %v), want fresh id", bid, err)
	}

	groups, err := c.ListCredentialsGroups(ctx)
	if err != nil || len(groups) != 2 {
		t.Errorf("ListCredentialsGroups = %v, %v; want 2 groups", groups, err)
	}
	if names := fake.GroupNames(); len(names) != 2 || names[0] != "group-a" || names[1] != "group-b" {
		t.Errorf("fake.GroupNames = %v, want [group-a group-b]", names)
	}

	// Access keys.
	ak, err := c.CreateAccessKey(ctx, id)
	if err != nil || ak.AccessKeyID == "" || ak.SecretAccessKey == "" || ak.KeyID == "" {
		t.Fatalf("CreateAccessKey = %+v, %v", ak, err)
	}
	ids, err := c.ListAccessKeyIDs(ctx, id)
	if err != nil || len(ids) != 1 || ids[0] != ak.KeyID {
		t.Errorf("ListAccessKeyIDs = %v, %v; want [%s]", ids, err, ak.KeyID)
	}

	// Group with keys cannot be deleted (422 fidelity).
	if err := c.DeleteCredentialsGroup(ctx, id); StatusCode(err) != 422 {
		t.Errorf("DeleteCredentialsGroup(with keys): StatusCode = %d, want 422", StatusCode(err))
	}

	// Real-API fidelity: deleting a key without its group id is a 500, an
	// unknown key id in a valid group a 404.
	if err := c.DeleteAccessKey(ctx, "", ak.KeyID); StatusCode(err) != 500 {
		t.Errorf("DeleteAccessKey(no group): StatusCode = %d, want 500", StatusCode(err))
	}
	if err := c.DeleteAccessKey(ctx, id, "key-ghost"); StatusCode(err) != 404 {
		t.Errorf("DeleteAccessKey(ghost key): StatusCode = %d, want 404", StatusCode(err))
	}
	if err := c.DeleteAccessKey(ctx, id, ak.KeyID); err != nil {
		t.Fatalf("DeleteAccessKey: %v", err)
	}
	if err := c.DeleteCredentialsGroup(ctx, id); err != nil {
		t.Fatalf("DeleteCredentialsGroup: %v", err)
	}
	if err := c.DeleteCredentialsGroup(ctx, id); StatusCode(err) != 404 {
		t.Errorf("DeleteCredentialsGroup(gone): StatusCode = %d, want 404", StatusCode(err))
	}
}

func TestDeleteAllAccessKeys(t *testing.T) {
	ctx := context.Background()
	c, fake := newFakeClient(t)

	id, _, err := c.CreateCredentialsGroup(ctx, "drain-me")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.CreateAccessKey(ctx, id); err != nil {
			t.Fatalf("create key %d: %v", i, err)
		}
	}
	if err := c.DeleteAllAccessKeys(ctx, id); err != nil {
		t.Fatalf("DeleteAllAccessKeys: %v", err)
	}
	if got := fake.KeyCount("drain-me"); got != 0 {
		t.Errorf("KeyCount after drain = %d, want 0", got)
	}

	// A missing group counts as already drained.
	if err := c.DeleteAllAccessKeys(ctx, "cg-does-not-exist"); err != nil {
		t.Errorf("DeleteAllAccessKeys(missing group) = %v, want nil", err)
	}

	// A non-404 listing error is surfaced.
	fake.FailNext("ListKeys", 500)
	if err := c.DeleteAllAccessKeys(ctx, id); err == nil {
		t.Error("DeleteAllAccessKeys with listing failure succeeded, want error")
	}

	// Per-key behavior: a 404 on delete is tolerated (already gone), any other
	// delete error is surfaced.
	if _, err := c.CreateAccessKey(ctx, id); err != nil {
		t.Fatalf("re-create key: %v", err)
	}
	fake.FailNext("DeleteKey", 404)
	if err := c.DeleteAllAccessKeys(ctx, id); err != nil {
		t.Errorf("DeleteAllAccessKeys with 404 on delete = %v, want nil", err)
	}
	if _, err := c.CreateAccessKey(ctx, id); err != nil {
		t.Fatalf("re-create key: %v", err)
	}
	fake.FailNext("DeleteKey", 500)
	if err := c.DeleteAllAccessKeys(ctx, id); err == nil {
		t.Error("DeleteAllAccessKeys with delete failure succeeded, want error")
	}
}

func TestCreateAccessKeyUnknownGroup(t *testing.T) {
	c, _ := newFakeClient(t)
	if _, err := c.CreateAccessKey(context.Background(), "cg-ghost"); StatusCode(err) != 404 {
		t.Errorf("CreateAccessKey(ghost group): StatusCode = %d, want 404", StatusCode(err))
	}
}

func TestStatusCodeNonAPIError(t *testing.T) {
	if got := StatusCode(errors.New("plain")); got != 0 {
		t.Errorf("StatusCode(plain error) = %d, want 0", got)
	}
	if got := StatusCode(nil); got != 0 {
		t.Errorf("StatusCode(nil) = %d, want 0", got)
	}
}

func TestSleepCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Minute) {
		t.Error("sleepCtx returned true on cancelled context")
	}
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("sleepCtx returned false after elapsed duration")
	}
}
