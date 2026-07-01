// Package stackit provides a thin, testable wrapper around the STACKIT Object
// Storage API, scoped to a single service account / project. It exists to prove
// feasibility of the S3 operator: create buckets and verify that one service
// account cannot observe another project's buckets.
package stackit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/stackitcloud/stackit-sdk-go/core/config"
	"github.com/stackitcloud/stackit-sdk-go/core/oapierror"
	"github.com/stackitcloud/stackit-sdk-go/services/objectstorage"
)

// RegionEU01 is the only region used in v1 (see INIT-SETUP.md §0).
const RegionEU01 = "eu01"

// Account is the minimal view of a STACKIT service-account key file we need:
// the embedded projectId plus the SA identity (for logging). The same file is
// also handed to the SDK for key-flow authentication.
type Account struct {
	ProjectID string
	Issuer    string
	KeyPath   string
}

// LoadAccount reads a STACKIT SA key JSON file and extracts the projectId.
// The RSA private key stays in the file and is consumed only by the SDK.
func LoadAccount(keyPath string) (Account, error) {
	// keyPath is operator-supplied configuration (CLI flag / env var / mounted
	// Secret path), not attacker-controlled input — directory traversal is not a
	// concern here.
	raw, err := os.ReadFile(keyPath) // #nosec G304
	if err != nil {
		return Account{}, fmt.Errorf("read key file: %w", err)
	}
	var doc struct {
		ProjectID   string `json:"projectId"`
		Credentials struct {
			Iss string `json:"iss"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Account{}, fmt.Errorf("parse key file %s: %w", keyPath, err)
	}
	if doc.ProjectID == "" {
		return Account{}, fmt.Errorf("key file %s has no projectId field", keyPath)
	}
	return Account{
		ProjectID: doc.ProjectID,
		Issuer:    doc.Credentials.Iss,
		KeyPath:   keyPath,
	}, nil
}

// Client wraps the Object Storage API client bound to one service account.
type Client struct {
	api     *objectstorage.APIClient
	account Account
	region  string
}

// NewClient builds an Object Storage client authenticated with the account's SA
// key file. Auth uses the key flow; the RSA private key is embedded in the file,
// so no separate key path is required.
func NewClient(acc Account, region string) (*Client, error) {
	api, err := objectstorage.NewAPIClient(
		config.WithServiceAccountKeyPath(acc.KeyPath),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("init object storage client for %s: %w", acc.Issuer, err)
	}
	return &Client{api: api, account: acc, region: region}, nil
}

// ProjectID returns the project this client is bound to.
func (c *Client) ProjectID() string { return c.account.ProjectID }

// Region returns the region this client operates in.
func (c *Client) Region() string { return c.region }

// EnsureService makes sure Object Storage is enabled for the project (a
// prerequisite for creating buckets). It is idempotent: if the service is
// already enabled it returns nil without re-enabling.
func (c *Client) EnsureService(ctx context.Context) error {
	if _, err := c.api.GetServiceStatus(ctx, c.account.ProjectID, c.region).Execute(); err == nil {
		return nil // already enabled
	}
	if _, err := c.api.EnableService(ctx, c.account.ProjectID, c.region).Execute(); err != nil {
		return fmt.Errorf("enable object storage in project %s: %w", c.account.ProjectID, err)
	}
	// Wait until the service reports ready.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := c.api.GetServiceStatus(ctx, c.account.ProjectID, c.region).Execute(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("object storage in project %s not ready within timeout", c.account.ProjectID)
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return ctx.Err()
		}
	}
}

// CreateBucket creates a bucket in the client's own project.
func (c *Client) CreateBucket(ctx context.Context, name string) error {
	if _, err := c.api.CreateBucket(ctx, c.account.ProjectID, c.region, name).Execute(); err != nil {
		return fmt.Errorf("create bucket %q in project %s: %w", name, c.account.ProjectID, err)
	}
	return nil
}

// DeleteBucket deletes a bucket from the client's own project. The bucket must
// be empty (STACKIT lösch-semantics, INIT-SETUP.md §0).
func (c *Client) DeleteBucket(ctx context.Context, name string) error {
	if _, err := c.api.DeleteBucket(ctx, c.account.ProjectID, c.region, name).Execute(); err != nil {
		return fmt.Errorf("delete bucket %q in project %s: %w", name, c.account.ProjectID, err)
	}
	return nil
}

// ListBucketNames lists bucket names in projectID. The projectID is explicit on
// purpose: passing a foreign project's ID is exactly how cross-project isolation
// is probed — a correctly isolated SA token yields an HTTP error, not data.
func (c *Client) ListBucketNames(ctx context.Context, projectID string) ([]string, error) {
	resp, err := c.api.ListBuckets(ctx, projectID, c.region).Execute()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.GetBuckets()))
	for _, b := range resp.GetBuckets() {
		names = append(names, b.GetName())
	}
	return names, nil
}

// HasBucket reports whether a bucket with the given name is visible to this
// client in projectID.
func (c *Client) HasBucket(ctx context.Context, projectID, name string) (bool, error) {
	names, err := c.ListBucketNames(ctx, projectID)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// WaitBucketVisible polls until name appears in the client's own project listing
// or the timeout elapses (bucket creation may be eventually consistent).
func (c *Client) WaitBucketVisible(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := c.HasBucket(ctx, c.account.ProjectID, name)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bucket %q not visible in project %s within %s", name, c.account.ProjectID, timeout)
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return ctx.Err()
		}
	}
}

// AccessKey is the S3 credential material returned when creating an access key.
// SecretAccessKey is only ever returned at creation time.
type AccessKey struct {
	AccessKeyID     string // S3 access key id (AWS_ACCESS_KEY_ID)
	SecretAccessKey string // S3 secret (AWS_SECRET_ACCESS_KEY)
	KeyID           string // STACKIT-internal id, used to delete the key
}

// CreateCredentialsGroup creates a credentials group ("workload account") and
// returns its id (for management) and urn (used as the bucket-policy principal).
func (c *Client) CreateCredentialsGroup(ctx context.Context, displayName string) (id, urn string, err error) {
	payload := objectstorage.NewCreateCredentialsGroupPayload(displayName)
	resp, err := c.api.CreateCredentialsGroup(ctx, c.account.ProjectID, c.region).
		CreateCredentialsGroupPayload(*payload).Execute()
	if err != nil {
		return "", "", fmt.Errorf("create credentials group %q: %w", displayName, err)
	}
	g := resp.GetCredentialsGroup()
	return g.GetCredentialsGroupId(), g.GetUrn(), nil
}

// DeleteCredentialsGroup removes a credentials group (its access keys must be
// deleted first).
func (c *Client) DeleteCredentialsGroup(ctx context.Context, groupID string) error {
	if _, err := c.api.DeleteCredentialsGroup(ctx, c.account.ProjectID, c.region, groupID).Execute(); err != nil {
		return fmt.Errorf("delete credentials group %s: %w", groupID, err)
	}
	return nil
}

// CreateAccessKey creates an S3 access key inside the given credentials group.
// No expiry is set (INIT-SETUP.md §0).
func (c *Client) CreateAccessKey(ctx context.Context, groupID string) (AccessKey, error) {
	resp, err := c.api.CreateAccessKey(ctx, c.account.ProjectID, c.region).
		CredentialsGroup(groupID).
		CreateAccessKeyPayload(*objectstorage.NewCreateAccessKeyPayload()).
		Execute()
	if err != nil {
		return AccessKey{}, fmt.Errorf("create access key in group %s: %w", groupID, err)
	}
	return AccessKey{
		AccessKeyID:     resp.GetAccessKey(),
		SecretAccessKey: resp.GetSecretAccessKey(),
		KeyID:           resp.GetKeyId(),
	}, nil
}

// DeleteAccessKey removes an access key. The owning group id is required by the
// API to locate the key.
func (c *Client) DeleteAccessKey(ctx context.Context, groupID, keyID string) error {
	if _, err := c.api.DeleteAccessKey(ctx, c.account.ProjectID, c.region, keyID).
		CredentialsGroup(groupID).Execute(); err != nil {
		return fmt.Errorf("delete access key %s (group %s): %w", keyID, groupID, err)
	}
	return nil
}

// CredentialsGroupInfo is a listed credentials group.
type CredentialsGroupInfo struct {
	ID          string
	URN         string
	DisplayName string
}

// FindCredentialsGroupByName looks up a credentials group by its display name.
// Display names are the operator's idempotency handle: a deterministic name per
// Bucket CR lets a reconcile find a group it (or a crashed predecessor) already
// created, instead of creating a duplicate.
func (c *Client) FindCredentialsGroupByName(ctx context.Context, displayName string) (id, urn string, found bool, err error) {
	groups, err := c.ListCredentialsGroups(ctx)
	if err != nil {
		return "", "", false, err
	}
	for _, g := range groups {
		if g.DisplayName == displayName {
			return g.ID, g.URN, true, nil
		}
	}
	return "", "", false, nil
}

// EnsureCredentialsGroup returns the id/urn of the group with the given display
// name, creating it if absent. It is idempotent across reconciles and crashes as
// long as displayName is deterministic for the desired resource.
func (c *Client) EnsureCredentialsGroup(ctx context.Context, displayName string) (id, urn string, err error) {
	id, urn, found, err := c.FindCredentialsGroupByName(ctx, displayName)
	if err != nil {
		return "", "", err
	}
	if found {
		return id, urn, nil
	}
	return c.CreateCredentialsGroup(ctx, displayName)
}

// DeleteAllAccessKeys removes every access key in a credentials group. It is
// used both to drain a group before deletion and to clear stale keys before
// (re)provisioning a fresh key — an access key's secret is only available at
// create time, so a key whose secret was lost is worthless and must be replaced.
// A missing group is treated as already drained.
func (c *Client) DeleteAllAccessKeys(ctx context.Context, groupID string) error {
	ids, err := c.ListAccessKeyIDs(ctx, groupID)
	if err != nil {
		if StatusCode(err) == 404 {
			return nil
		}
		return err
	}
	for _, id := range ids {
		if err := c.DeleteAccessKey(ctx, groupID, id); err != nil {
			if StatusCode(err) == 404 {
				continue
			}
			return err
		}
	}
	return nil
}

// ListCredentialsGroups lists the credentials groups in the client's project.
func (c *Client) ListCredentialsGroups(ctx context.Context) ([]CredentialsGroupInfo, error) {
	resp, err := c.api.ListCredentialsGroups(ctx, c.account.ProjectID, c.region).Execute()
	if err != nil {
		return nil, fmt.Errorf("list credentials groups: %w", err)
	}
	groups := resp.GetCredentialsGroups()
	out := make([]CredentialsGroupInfo, 0, len(groups))
	for _, g := range groups {
		out = append(out, CredentialsGroupInfo{ID: g.GetCredentialsGroupId(), URN: g.GetUrn(), DisplayName: g.GetDisplayName()})
	}
	return out, nil
}

// ListAccessKeyIDs lists the key ids of a credentials group.
func (c *Client) ListAccessKeyIDs(ctx context.Context, groupID string) ([]string, error) {
	resp, err := c.api.ListAccessKeys(ctx, c.account.ProjectID, c.region).
		CredentialsGroup(groupID).Execute()
	if err != nil {
		return nil, fmt.Errorf("list access keys (group %s): %w", groupID, err)
	}
	keys := resp.GetAccessKeys()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.GetKeyId())
	}
	return out, nil
}

// BucketConnInfo returns the S3 connection details for a bucket: the endpoint
// host (no scheme, no bucket) used to configure an S3 client, and the full
// path-style bucket URL (scheme + host + bucket) written into the workload
// credentials Secret. Both are derived from the bucket's path-style URL rather
// than hardcoded.
func (c *Client) BucketConnInfo(ctx context.Context, name string) (host, pathStyleURL string, err error) {
	resp, err := c.api.GetBucket(ctx, c.account.ProjectID, c.region, name).Execute()
	if err != nil {
		return "", "", fmt.Errorf("get bucket %q: %w", name, err)
	}
	b := resp.GetBucket()
	raw := b.GetUrlPathStyle()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("parse bucket path-style url %q: %w", raw, err)
	}
	return u.Host, raw, nil
}

// BucketEndpointHost returns the S3 endpoint host (no scheme, no bucket) for a
// bucket, derived from its path-style URL — used to configure an S3 client.
func (c *Client) BucketEndpointHost(ctx context.Context, name string) (string, error) {
	host, _, err := c.BucketConnInfo(ctx, name)
	return host, err
}

// StatusCode extracts the HTTP status code from a STACKIT API error, or 0 if the
// error is not a STACKIT API error.
func StatusCode(err error) int {
	var apiErr *oapierror.GenericOpenAPIError
	if errors.As(err, &apiErr) {
		return apiErr.GetStatusCode()
	}
	return 0
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
