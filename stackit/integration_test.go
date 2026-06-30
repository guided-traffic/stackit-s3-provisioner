//go:build integration

// Integration tests hit the real STACKIT Object Storage API and create/delete
// real buckets in both projects. Run explicitly:
//
//	go test -tags integration ./stackit/ -run Integration -v
//
// They are skipped automatically when the SA key files are absent.
package stackit

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func integrationClients(t *testing.T) (*Client, *Client) {
	t.Helper()
	p1, p2 := accountPaths(t)

	a1, err := LoadAccount(p1)
	if err != nil {
		t.Fatalf("load account 1: %v", err)
	}
	a2, err := LoadAccount(p2)
	if err != nil {
		t.Fatalf("load account 2: %v", err)
	}
	c1, err := NewClient(a1, RegionEU01)
	if err != nil {
		t.Fatalf("client 1: %v", err)
	}
	c2, err := NewClient(a2, RegionEU01)
	if err != nil {
		t.Fatalf("client 2: %v", err)
	}
	return c1, c2
}

func bucketName(projectID string) string {
	short := projectID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("s3op-test-%s-%d", short, rand.Intn(1_000_000))
}

// createTempBucket creates a bucket via c, registers cleanup, and waits until it
// is visible in c's own project. Returns the bucket name.
func createTempBucket(t *testing.T, ctx context.Context, c *Client) string {
	t.Helper()
	name := bucketName(c.ProjectID())

	if err := c.EnsureService(ctx); err != nil {
		t.Fatalf("ensure service (project %s): %v", c.ProjectID(), err)
	}
	if err := c.CreateBucket(ctx, name); err != nil {
		t.Fatalf("create bucket %q (project %s): %v", name, c.ProjectID(), err)
	}
	t.Cleanup(func() {
		// Fresh context: the test context may already be done.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.DeleteBucket(cctx, name); err != nil {
			t.Logf("cleanup: delete bucket %q failed (status %d): %v", name, StatusCode(err), err)
		}
	})
	if err := c.WaitBucketVisible(ctx, name, 60*time.Second); err != nil {
		t.Fatalf("wait bucket visible: %v", err)
	}
	return name
}

// TestIntegrationBothAccountsCreateBuckets proves API access works and both
// service accounts can create (and see) a bucket in their own project.
func TestIntegrationBothAccountsCreateBuckets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c1, c2 := integrationClients(t)

	for _, c := range []*Client{c1, c2} {
		name := createTempBucket(t, ctx, c)
		ok, err := c.HasBucket(ctx, c.ProjectID(), name)
		if err != nil {
			t.Fatalf("list own buckets (project %s): %v", c.ProjectID(), err)
		}
		if !ok {
			t.Fatalf("created bucket %q not visible in own project %s", name, c.ProjectID())
		}
		t.Logf("OK: project %s created and sees bucket %q", c.ProjectID(), name)
	}
}

// TestIntegrationCrossProjectIsolation is the security test: account 1 must not
// be able to observe account 2's buckets, and vice versa.
func TestIntegrationCrossProjectIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c1, c2 := integrationClients(t)

	b1 := createTempBucket(t, ctx, c1)
	b2 := createTempBucket(t, ctx, c2)
	t.Logf("project %s bucket=%s | project %s bucket=%s", c1.ProjectID(), b1, c2.ProjectID(), b2)

	// "sehen": account 1 must not observe account 2's bucket, and vice versa.
	assertCannotSee(t, ctx, c1, c2.ProjectID(), b2)
	assertCannotSee(t, ctx, c2, c1.ProjectID(), b1)

	// "verändern": account 1 must not create or delete in account 2's project,
	// and vice versa. c2 is passed as the owner to confirm the victim survives.
	assertCannotModify(t, ctx, c1, c2, b2)
	assertCannotModify(t, ctx, c2, c1, b1)
}

// assertCannotSee requires that `probe` cannot observe `foreignBucket` in
// `foreignProject`. The expected (strong) outcome is an explicit HTTP deny
// (401/403/404). A 200 response is tolerated only if the foreign bucket is
// absent from the listing; the foreign bucket appearing is a hard failure.
func assertCannotSee(t *testing.T, ctx context.Context, probe *Client, foreignProject, foreignBucket string) {
	t.Helper()
	names, err := probe.ListBucketNames(ctx, foreignProject)
	if err != nil {
		if isDenied(err) {
			t.Logf("OK: project %s denied LIST on project %s (HTTP %d)", probe.ProjectID(), foreignProject, StatusCode(err))
			return
		}
		t.Fatalf("cross-project list project %s->%s failed with unexpected error (status %d): %v",
			probe.ProjectID(), foreignProject, StatusCode(err), err)
	}
	for _, n := range names {
		if n == foreignBucket {
			t.Fatalf("ISOLATION BREACH: project %s can see bucket %q belonging to project %s",
				probe.ProjectID(), foreignBucket, foreignProject)
		}
	}
	t.Logf("WARNING: project %s got HTTP 200 listing project %s (%d buckets) without explicit deny; "+
		"isolation holds by absence of %q but a 403 would be stronger", probe.ProjectID(), foreignProject, len(names), foreignBucket)
}

// assertCannotModify requires that `probe` can neither create a bucket in nor
// delete a bucket from `owner`'s project. `victim` is an existing bucket owned by
// `owner`; after the failed delete attempt it must still exist. Uses the
// low-level api client directly to target a foreign project (same package).
func assertCannotModify(t *testing.T, ctx context.Context, probe, owner *Client, victim string) {
	t.Helper()
	foreign := owner.ProjectID()

	// CREATE into the foreign project must be denied.
	attempt := bucketName(foreign)
	if _, err := probe.api.CreateBucket(ctx, foreign, probe.region, attempt).Execute(); err == nil {
		// We wrongly created it — remove it so we don't leak, then fail.
		_, _ = probe.api.DeleteBucket(ctx, foreign, probe.region, attempt).Execute()
		t.Fatalf("ISOLATION BREACH: project %s created bucket %q in foreign project %s", probe.ProjectID(), attempt, foreign)
	} else if !isDenied(err) {
		t.Fatalf("cross-project create %s->%s: unexpected error (status %d): %v", probe.ProjectID(), foreign, StatusCode(err), err)
	} else {
		t.Logf("OK: project %s denied CREATE in project %s (HTTP %d)", probe.ProjectID(), foreign, StatusCode(err))
	}

	// DELETE of the owner's real bucket must be denied.
	if _, err := probe.api.DeleteBucket(ctx, foreign, probe.region, victim).Execute(); err == nil {
		t.Fatalf("ISOLATION BREACH: project %s deleted bucket %q in foreign project %s", probe.ProjectID(), victim, foreign)
	} else if !isDenied(err) {
		t.Fatalf("cross-project delete %s->%s: unexpected error (status %d): %v", probe.ProjectID(), foreign, StatusCode(err), err)
	} else {
		t.Logf("OK: project %s denied DELETE in project %s (HTTP %d)", probe.ProjectID(), foreign, StatusCode(err))
	}

	// The victim bucket must still exist for its owner.
	ok, err := owner.HasBucket(ctx, foreign, victim)
	if err != nil {
		t.Fatalf("owner %s re-check of bucket %q: %v", foreign, victim, err)
	}
	if !ok {
		t.Fatalf("ISOLATION BREACH: bucket %q in project %s disappeared after foreign modify attempts", victim, foreign)
	}
}

// isDenied reports whether err is an authorization-style denial (401/403/404).
func isDenied(err error) bool {
	switch StatusCode(err) {
	case 401, 403, 404:
		return true
	default:
		return false
	}
}
