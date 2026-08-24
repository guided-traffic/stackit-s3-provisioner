// Command e2ecleanup removes the STACKIT Object Storage resources a cloud e2e
// run (make e2e-stackit) created, so a crashed or interrupted run cannot leave
// real buckets — or, worse, live access keys — behind in the project.
//
// It is the last line of defense, not the primary cleanup: a successful run
// tears its resources down through the operator's own finalizer while the Kind
// cluster is still up. This tool exists for the case where that did not happen.
//
//	go run ./hack/e2ecleanup -key account-1.json -prefix s3e2e            # report only
//	go run ./hack/e2ecleanup -key account-1.json -prefix s3e2e -delete    # actually remove
//	go run ./hack/e2ecleanup -key account-1.json -prefix s3e2e -delete -admin
//
// Why the admin group matters: the operator's bootstrap credentials group
// (operator-admin) and its access key live in the cloud, while the only copy of
// the secret lives in a Secret inside the cluster. Deleting the Kind cluster
// therefore orphans a fully privileged, still-valid S3 key. -admin drains and
// removes that group too. Do NOT pass -admin against a project that also hosts a
// real operator deployment: it would invalidate that operator's admin key. (It
// recovers on its own — ensureAdmin re-creates the group and mints a new key —
// but it is a needless disruption.)
//
// A leftover bucket usually still carries an isolation policy that denies every
// principal except the admin group and the bucket's own workload group, so it
// cannot be emptied with a freshly created group. The tool works around that the
// same way the operator does: it looks up the EXISTING operator-admin group and
// mints a new key inside it. The policy names the group's URN, not a key, so the
// new key is exempt and can wipe the bucket.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// adminGroupName mirrors the operator's bootstrap group display name
// (internal/controller: adminGroupName). Kept in sync by hand; a mismatch only
// makes the tool fall back to creating its own group, which cannot wipe buckets
// that already carry a policy.
const adminGroupName = "operator-admin"

func main() {
	var (
		keyPath   = flag.String("key", "account-1.json", "path to the STACKIT service-account key JSON")
		prefix    = flag.String("prefix", "s3e2e", "only touch buckets whose name starts with this")
		region    = flag.String("region", stackit.RegionEU01, "STACKIT region")
		doDelete  = flag.Bool("delete", false, "actually delete; without it the tool only reports")
		withAdmin = flag.Bool("admin", false, "also drain and delete the operator-admin bootstrap group")
		timeout   = flag.Duration("timeout", 10*time.Minute, "overall timeout")
	)
	flag.Parse()

	if err := run(*keyPath, *prefix, *region, *doDelete, *withAdmin, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "e2ecleanup: %v\n", err)
		os.Exit(1)
	}
}

func run(keyPath, prefix, region string, doDelete, withAdmin bool, timeout time.Duration) error {
	if strings.TrimSpace(prefix) == "" {
		return fmt.Errorf("-prefix must not be empty: it is the only thing separating e2e resources from real ones")
	}
	if _, err := os.Stat(keyPath); err != nil {
		fmt.Printf("no service-account key at %s; nothing to do\n", keyPath)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	acc, err := stackit.LoadAccount(keyPath)
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	c, err := stackit.NewClient(acc, region)
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	fmt.Printf("project %s, region %s, prefix %q, delete=%v\n", c.ProjectID(), region, prefix, doDelete)

	buckets, err := leftoverBuckets(ctx, c, prefix)
	if err != nil {
		return err
	}
	groups, adminGroupID, err := leftoverGroups(ctx, c, prefix)
	if err != nil {
		return err
	}

	fmt.Printf("found %d bucket(s) and %d credentials group(s) from e2e runs\n", len(buckets), len(groups))
	for _, b := range buckets {
		fmt.Printf("  bucket %s\n", b)
	}
	for _, g := range groups {
		fmt.Printf("  group  %s (%s)\n", g.DisplayName, g.ID)
	}
	if !doDelete {
		if len(buckets)+len(groups) > 0 {
			fmt.Println("dry run: pass -delete to remove these")
		}
		return nil
	}

	// Buckets first: a credentials group cannot be deleted while it still has
	// keys, and a bucket cannot be deleted while it still has objects.
	if len(buckets) > 0 {
		admin, err := adminS3(ctx, c, adminGroupID, buckets[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: no admin S3 client (%v); buckets that still hold objects cannot be emptied\n", err)
		}
		for _, b := range buckets {
			if admin != nil {
				if err := admin.WipeBucket(ctx, b); err != nil {
					fmt.Fprintf(os.Stderr, "warning: wipe %s: %v\n", b, err)
				}
			}
			if err := c.DeleteBucket(ctx, b); err != nil {
				fmt.Fprintf(os.Stderr, "warning: delete bucket %s: %v\n", b, err)
				continue
			}
			fmt.Printf("deleted bucket %s\n", b)
		}
	}

	for _, g := range groups {
		if err := c.DeleteAllAccessKeys(ctx, g.ID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: drain keys of %s: %v\n", g.DisplayName, err)
		}
		if err := c.DeleteCredentialsGroup(ctx, g.ID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: delete group %s: %v\n", g.DisplayName, err)
			continue
		}
		fmt.Printf("deleted group %s\n", g.DisplayName)
	}

	if withAdmin && adminGroupID != "" {
		if err := c.DeleteAllAccessKeys(ctx, adminGroupID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: drain admin keys: %v\n", err)
		}
		if err := c.DeleteCredentialsGroup(ctx, adminGroupID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: delete admin group: %v\n", err)
		} else {
			fmt.Printf("deleted group %s (bootstrap admin)\n", adminGroupName)
		}
	}
	return nil
}

// leftoverBuckets returns the project's buckets whose name starts with prefix.
func leftoverBuckets(ctx context.Context, c *stackit.Client, prefix string) ([]string, error) {
	names, err := c.ListBucketNames(ctx, c.ProjectID())
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out, nil
}

// leftoverGroups returns the workload credentials groups an e2e run created,
// plus the id of the bootstrap admin group (which is reported separately: it is
// shared and only removed on request).
//
// Workload group display names are workloadGroupName's "s3op-<namespace>-..."
// and the e2e run puts its Buckets in namespaces starting with the same prefix
// as the buckets, so "s3op-<prefix>" identifies them.
func leftoverGroups(ctx context.Context, c *stackit.Client, prefix string) ([]stackit.CredentialsGroupInfo, string, error) {
	groups, err := c.ListCredentialsGroups(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("list credentials groups: %w", err)
	}
	want := "s3op-" + prefix
	var out []stackit.CredentialsGroupInfo
	var adminID string
	for _, g := range groups {
		switch {
		case g.DisplayName == adminGroupName:
			adminID = g.ID
		case strings.HasPrefix(g.DisplayName, want):
			out = append(out, g)
		}
	}
	return out, adminID, nil
}

// adminS3 returns an S3 client authenticated as the operator's bootstrap admin
// group. It mints a FRESH key in the existing group rather than creating a new
// group, because leftover buckets carry a policy that exempts that group's URN
// and denies everything else — a new group would be locked out of the very
// buckets this tool has to empty.
func adminS3(ctx context.Context, c *stackit.Client, adminGroupID, anyBucket string) (*stackit.S3Admin, error) {
	if adminGroupID == "" {
		return nil, fmt.Errorf("no %q group in project; leftover buckets keep denying every other principal", adminGroupName)
	}
	ak, err := c.CreateAccessKey(ctx, adminGroupID)
	if err != nil {
		return nil, fmt.Errorf("create admin key: %w", err)
	}
	endpoint, err := c.BucketEndpoint(ctx, anyBucket)
	if err != nil {
		return nil, fmt.Errorf("bucket endpoint: %w", err)
	}
	return stackit.NewS3Admin(endpoint, ak.AccessKeyID, ak.SecretAccessKey, c.Region())
}
