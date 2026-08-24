//go:build integration

// Layer-2 read-grant test: a bucket that grants read-only access to a second
// workload (spec.grantReadAccess) must let that workload list and get objects
// while every write, delete and management action stays denied — and a third,
// ungranted workload must stay locked out entirely.
//
// This is the only test that can prove the three-statement policy is enforced as
// designed: it exercises the real StorageGRID evaluation of a Deny statement
// whose Principal is a *list*, which no offline fake models.
//
//	go test -tags integration ./stackit/ -run IntegrationReadGrant -v -timeout 12m
package stackit

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func TestIntegrationReadGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	c1, _ := integrationClients(t)
	if err := c1.EnsureService(ctx); err != nil {
		t.Fatalf("ensure service: %v", err)
	}

	type keyRef struct{ group, key string }
	var (
		buckets []string
		keys    []keyRef
		groups  []string
		adminMC *minio.Client
	)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		for _, b := range buckets {
			if adminMC != nil {
				emptyBucket(cctx, adminMC, b)
			}
			if err := c1.DeleteBucket(cctx, b); err != nil {
				t.Logf("cleanup: delete bucket %s (status %d): %v", b, StatusCode(err), err)
			}
		}
		for _, k := range keys {
			if err := c1.DeleteAccessKey(cctx, k.group, k.key); err != nil {
				t.Logf("cleanup: delete key %s: %v", k.key, err)
			}
		}
		for _, g := range groups {
			if err := c1.DeleteCredentialsGroup(cctx, g); err != nil {
				t.Logf("cleanup: delete group %s: %v", g, err)
			}
		}
	})

	sfx := fmt.Sprintf("%06d", rand.Intn(1_000_000))

	// One bucket: the data bucket that hands out the grant.
	dataBucket := bucketName(c1.ProjectID())
	if err := c1.CreateBucket(ctx, dataBucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	buckets = append(buckets, dataBucket)
	if err := c1.WaitBucketVisible(ctx, dataBucket, 60*time.Second); err != nil {
		t.Fatalf("wait bucket: %v", err)
	}
	t.Logf("data bucket: %s", dataBucket)

	// Four principals: admin, the bucket owner, the granted reader, an outsider.
	mkGroup := func(name string) (string, string, AccessKey) {
		gid, urn, err := c1.CreateCredentialsGroup(ctx, name+"-"+sfx)
		if err != nil {
			t.Fatalf("create group %s: %v", name, err)
		}
		groups = append(groups, gid)
		ak, err := c1.CreateAccessKey(ctx, gid)
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		keys = append(keys, keyRef{gid, ak.KeyID})
		return gid, urn, ak
	}
	_, adminURN, adminAK := mkGroup("operator-admin")
	_, ownerURN, ownerAK := mkGroup("owner")
	_, readerURN, readerAK := mkGroup("reader")
	_, _, outsiderAK := mkGroup("outsider")

	endpoint, err := c1.BucketEndpoint(ctx, dataBucket)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	adminMC = newMinio(t, endpoint, adminAK)
	ownerMC := newMinio(t, endpoint, ownerAK)
	readerMC := newMinio(t, endpoint, readerAK)
	outsiderMC := newMinio(t, endpoint, outsiderAK)

	// The exact document the operator writes for a bucket with one grant.
	policy := BuildIsolationPolicy(dataBucket, adminURN, ownerURN, []string{readerURN})
	t.Logf("policy: %s", policy)
	if err := adminMC.SetBucketPolicy(ctx, dataBucket, policy); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	const obj = "shared.txt"
	payload := []byte("owner-payload-" + sfx)

	// The owner keeps full object access; retry until the policy is active.
	if err := retry(90*time.Second, func() error { return putObject(ownerMC, dataBucket, obj, payload) }); err != nil {
		t.Fatalf("owner write to its own bucket failed after granting a reader: %v", err)
	}
	t.Log("OK: owner still writes to its own bucket")

	// --- the grant itself: list + get must succeed ---
	if err := retry(90*time.Second, func() error { return listErr(readerMC, dataBucket) }); err != nil {
		t.Fatalf("granted reader cannot list the bucket (the feature does not work): %v", err)
	}
	t.Log("OK: granted reader lists the bucket")

	got, err := getObject(readerMC, dataBucket, obj)
	if err != nil {
		t.Fatalf("granted reader cannot get an object (the feature does not work): %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("granted reader read mismatch: got %q want %q", got, payload)
	}
	t.Log("OK: granted reader reads an object")

	// --- the grant's limits: every mutation must be denied ---
	mustDeny := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("ISOLATION BREACH: granted reader could %s", what)
		}
		if !isS3Denied(err) {
			t.Fatalf("granted reader %s: unexpected error (want AccessDenied): %v", what, err)
		}
		t.Logf("OK: granted reader denied %s", what)
	}

	mustDeny("write a new object", putObject(readerMC, dataBucket, "reader-wrote.txt", []byte("nope")))
	mustDeny("overwrite the owner's object", putObject(readerMC, dataBucket, obj, []byte("nope")))

	rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
	defer rmCancel()
	mustDeny("delete the owner's object",
		readerMC.RemoveObject(rmCtx, dataBucket, obj, minio.RemoveObjectOptions{}))

	// The policy is the reader's own cage; being able to read or replace it
	// would make the grant self-escalating.
	_, polErr := readerMC.GetBucketPolicy(rmCtx, dataBucket)
	mustDeny("read the bucket policy", polErr)
	mustDeny("replace the bucket policy",
		readerMC.SetBucketPolicy(rmCtx, dataBucket, policy))

	// The owner's object must be intact after all the denied attempts.
	got, err = getObject(ownerMC, dataBucket, obj)
	if err != nil {
		t.Fatalf("owner read after reader's denied writes: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("owner object was modified by the reader: got %q want %q", got, payload)
	}
	t.Log("OK: owner object unchanged")

	// --- an ungranted workload stays fully locked out ---
	if err := retry(90*time.Second, func() error {
		_, gerr := getObject(outsiderMC, dataBucket, obj)
		if isS3Denied(gerr) {
			return nil
		}
		if gerr == nil {
			return fmt.Errorf("ISOLATION BREACH: ungranted workload read the object")
		}
		return fmt.Errorf("ungranted workload read: unexpected error (want AccessDenied): %v", gerr)
	}); err != nil {
		t.Fatal(err)
	}
	if err := listErr(outsiderMC, dataBucket); !isS3Denied(err) {
		t.Fatalf("ISOLATION BREACH: ungranted workload could list the bucket (err=%v)", err)
	}
	t.Log("OK: ungranted workload denied read and list")

	// --- the admin must remain unrestricted, or the bucket is unrepairable ---
	if _, err := adminMC.GetBucketPolicy(ctx, dataBucket); err != nil {
		t.Fatalf("LOCKOUT: admin cannot read the policy of a bucket with a grant: %v", err)
	}
	if err := adminMC.SetBucketPolicy(ctx, dataBucket, policy); err != nil {
		t.Fatalf("LOCKOUT: admin cannot rewrite the policy of a bucket with a grant: %v", err)
	}
	if err := putObject(adminMC, dataBucket, "admin-probe.txt", []byte("admin")); err != nil {
		t.Fatalf("LOCKOUT: admin cannot write to a bucket with a grant: %v", err)
	}
	t.Log("OK: admin retains full control")

	// --- revocation: dropping the grant must lock the reader out again ---
	if err := adminMC.SetBucketPolicy(ctx, dataBucket,
		BuildIsolationPolicy(dataBucket, adminURN, ownerURN, nil)); err != nil {
		t.Fatalf("revoke policy: %v", err)
	}
	if err := retry(90*time.Second, func() error {
		_, gerr := getObject(readerMC, dataBucket, obj)
		if isS3Denied(gerr) {
			return nil
		}
		if gerr == nil {
			return fmt.Errorf("REVOCATION FAILED: reader still reads after the grant was removed")
		}
		return fmt.Errorf("reader read after revocation: unexpected error (want AccessDenied): %v", gerr)
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("OK: revoking the grant locks the reader out again")
}
