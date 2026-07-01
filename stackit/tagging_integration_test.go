//go:build integration

// Feasibility + regression probe for the ownership marker: does STACKIT's
// StorageGRID backend support S3 bucket-level tagging, and do the production
// S3Admin tag wrappers round-trip against it?
//
// STACKIT's control-plane API has no native bucket tags (verified against the
// SDK), so the operator records bucket ownership as an S3 bucket tag via the
// admin data-plane key. This test exercises exactly the S3Admin.SetBucketTags /
// BucketTags methods the reconciler uses. If it fails, the ownership marker
// cannot live in a bucket tag and we must fall back to a policy-Sid or a
// name-embedded identity instead.
//
//	go test -tags integration ./stackit/ -run IntegrationBucketTagging -v -timeout 5m
//
// Skipped automatically when the SA key files are absent.
package stackit

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func TestIntegrationBucketTaggingSupported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c1, _ := integrationClients(t)
	if err := c1.EnsureService(ctx); err != nil {
		t.Fatalf("ensure service: %v", err)
	}

	// Bucket (control plane) with automatic cleanup.
	bucket := createTempBucket(t, ctx, c1)

	// Admin data-plane credentials: bucket tagging is an S3 call, not a
	// control-plane call, so it needs an access key + secret.
	sfx := fmt.Sprintf("%06d", rand.Intn(1_000_000))
	adminGID, _, err := c1.CreateCredentialsGroup(ctx, "tagging-probe-"+sfx)
	if err != nil {
		t.Fatalf("create admin group: %v", err)
	}
	adminAK, err := c1.CreateAccessKey(ctx, adminGID)
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer ccancel()
		if err := c1.DeleteAccessKey(cctx, adminGID, adminAK.KeyID); err != nil {
			t.Logf("cleanup: delete key %s: %v", adminAK.KeyID, err)
		}
		if err := c1.DeleteCredentialsGroup(cctx, adminGID); err != nil {
			t.Logf("cleanup: delete group %s: %v", adminGID, err)
		}
	})

	endpoint, err := c1.BucketEndpointHost(ctx, bucket)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	t.Logf("S3 endpoint: %s | bucket: %s", endpoint, bucket)

	// Exercise the exact production wrappers the reconciler uses.
	s3admin, err := NewS3Admin(endpoint, adminAK.AccessKeyID, adminAK.SecretAccessKey, RegionEU01)
	if err != nil {
		t.Fatalf("new s3 admin: %v", err)
	}

	// --- 0. an untagged bucket must read back as an empty tag set, not an error ---
	if got, err := s3admin.BucketTags(ctx, bucket); err != nil {
		t.Fatalf("BucketTags on untagged bucket returned an error (want empty map): %v", err)
	} else if len(got) != 0 {
		t.Fatalf("BucketTags on untagged bucket returned %v (want empty map)", got)
	}
	t.Log("OK: untagged bucket reads back as an empty tag set")

	want := map[string]string{
		"managed-by": "stackit-s3-provisioner",
		"owner":      "probe/" + sfx,
	}

	// --- 1. SET (the feasibility crux) ---
	if err := retry(60*time.Second, func() error {
		return s3admin.SetBucketTags(ctx, bucket, want)
	}); err != nil {
		t.Fatalf("FEASIBILITY FAIL: SetBucketTags unsupported by StorageGRID: %v -- "+
			"fall back to policy-Sid or name-embedded identity for ownership", err)
	}
	t.Log("OK: SetBucketTags accepted")

	// --- 2. GET round-trip (eventually consistent after a set) ---
	if err := retry(60*time.Second, func() error {
		got, gerr := s3admin.BucketTags(ctx, bucket)
		if gerr != nil {
			return gerr
		}
		for k, v := range want {
			if got[k] != v {
				return fmt.Errorf("tag %q not yet %q (got %q; full: %v)", k, v, got[k], got)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("FEASIBILITY FAIL: BucketTags round-trip did not match after Set: %v", err)
	}
	t.Log("OK: BucketTags round-trip matches")

	t.Log("FEASIBILITY PASS: STACKIT/StorageGRID supports S3 bucket tagging via the S3Admin " +
		"wrappers -- a bucket tag is viable as the ownership / collision-detection marker")
}
