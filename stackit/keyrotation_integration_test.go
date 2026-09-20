//go:build integration

// Live rotation of the operator's service-account key against the real STACKIT
// API. Run explicitly, and never alongside another real-API suite:
//
//	go test -tags integration ./stackit/ -run IntegrationKeyRotation -v -timeout 12m
//
// It needs a SECOND service-account key for the SAME project as account-1.json,
// at account-1b.json in the repository root (override with STACKIT_ACCOUNT_1B).
// The suite skips when either file is absent, so a runner without secrets stays
// green.
//
// This is the run ADR 0016 names under its residual risks: every other case in
// that record is reproduced offline against the in-memory fake, and only this
// one proves the swap against the provider that actually mints the tokens.
//
// Resources are prefixed s3rot, deliberately NOT the s3e2e of the Kind suite:
// the e2e sweep runs with -admin and would otherwise delete this test's
// resources — and the shared bootstrap admin group — out from under a run.
// A crashed run is cleaned up with
//
//	go run ./hack/e2ecleanup -key account-1.json -prefix s3rot -delete
package stackit

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rotationPrefix separates this test's cloud resources from real ones and from
// the Kind suite's. It is the only thing that does.
const rotationPrefix = "s3rot"

// rotationAccounts resolves the two keys this test rotates between: the one the
// process starts on and the one it must end on. Both must name the SAME project
// — a reload may not change the project (ADR 0016 D4) — and different service
// accounts, or the test would prove nothing but that re-reading the same key is
// a no-op.
func rotationAccounts(t *testing.T) (before, after string) {
	t.Helper()
	before = envOr("STACKIT_ACCOUNT_1", filepath.Join("..", "account-1.json"))
	after = envOr("STACKIT_ACCOUNT_1B", filepath.Join("..", "account-1b.json"))
	for _, p := range []string{before, after} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("SA key file %s not present: %v", p, err)
		}
	}

	a, err := LoadAccount(before)
	if err != nil {
		t.Fatalf("load %s: %v", before, err)
	}
	b, err := LoadAccount(after)
	if err != nil {
		t.Fatalf("load %s: %v", after, err)
	}
	if a.ProjectID != b.ProjectID {
		t.Fatalf("the two keys name different projects (%s and %s); this test rotates within one project",
			a.ProjectID, b.ProjectID)
	}
	if a.Issuer == b.Issuer {
		t.Fatalf("both keys carry the same issuer %q; a rotation between them would be indistinguishable "+
			"from re-reading one key", a.Issuer)
	}
	t.Logf("rotating project %s from %s to %s", a.ProjectID, a.Issuer, b.Issuer)
	return before, after
}

// stagedKey copies a key file into the test's own directory. The reloader is
// only ever pointed at the copy: rewriting the repository's real key file would
// silently re-point make e2e-stackit, and a partial write would destroy the one
// copy of a private key that is not in git.
type stagedKey struct {
	t    *testing.T
	path string
}

func newStagedKey(t *testing.T, from string) *stagedKey {
	t.Helper()
	s := &stagedKey{t: t, path: filepath.Join(t.TempDir(), "sa-key.json")}
	s.replaceWith(from)
	return s
}

// replaceWith overwrites the staged copy with the contents of another key file,
// the way an external rotation mechanism rewrites the mounted Secret.
func (s *stagedKey) replaceWith(from string) {
	s.t.Helper()
	raw, err := os.ReadFile(from) // #nosec G304 -- a test fixture path
	if err != nil {
		s.t.Fatalf("read %s: %v", from, err)
	}
	s.write(raw)
}

func (s *stagedKey) write(raw []byte) {
	s.t.Helper()
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		s.t.Fatalf("stage key file: %v", err)
	}
}

// mutate rewrites the staged copy with one field of the key document changed.
// The document is never logged: it carries a live private key.
func (s *stagedKey) mutate(from string, edit func(doc map[string]any)) {
	s.t.Helper()
	raw, err := os.ReadFile(from) // #nosec G304 -- a test fixture path
	if err != nil {
		s.t.Fatalf("read %s: %v", from, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		s.t.Fatalf("parse %s: %v", from, err)
	}
	edit(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		s.t.Fatalf("marshal mutated key: %v", err)
	}
	s.write(out)
}

// freshPrivateKeyPEM generates an RSA key the provider has never seen. Swapping
// it into an otherwise intact key document is how this test reaches a
// definitive refusal from the real token endpoint without revoking anything.
func freshPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// tickOnce drives exactly one poll of the production loop. tick is what
// KeyReloader.Start calls, so this exercises the whole shipped path — the read,
// the hash comparison, the parse and the validation — rather than reaching past
// it to (*Client).reload.
func tickOnce(t *testing.T, ctx context.Context, r *KeyReloader) ReloadResult {
	t.Helper()
	res, reported := r.tick(ctx)
	if !reported {
		t.Fatalf("the poll reported nothing; it is backing off when it should not be")
	}
	return res
}

// TestIntegrationKeyRotation is the live half of ADR 0016: the operator is
// started on one service-account key, the file underneath it is replaced with a
// second key for the same project, and the operator has to prove and adopt it
// without a restart — while the bucket, the credentials group and the access
// key the FIRST service account created stay exactly where they are.
func TestIntegrationKeyRotation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	before, after := rotationAccounts(t)
	staged := newStagedKey(t, before)

	acc, err := LoadAccount(staged.path)
	if err != nil {
		t.Fatalf("load staged key: %v", err)
	}
	c, err := NewClient(acc, RegionEU01)
	if err != nil {
		t.Fatalf("client on the first key: %v", err)
	}
	if err := c.EnsureService(ctx); err != nil {
		t.Fatalf("ensure service: %v", err)
	}

	firstIssuer := c.Account().Issuer
	firstHash := c.keyHash()
	firstState := c.state.Load()

	// --- resources created by the FIRST service account -------------------
	sfx := fmt.Sprintf("%06d", mrand.Intn(1_000_000))
	bucket := rotationPrefix + "-" + sfx
	if err := c.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("create bucket %q: %v", bucket, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer ccancel()
		if err := c.DeleteBucket(cctx, bucket); err != nil {
			t.Logf("cleanup: delete bucket %s: %v", bucket, err)
		}
	})

	group := "s3op-" + rotationPrefix + "-" + sfx
	groupID, groupURN, err := c.CreateCredentialsGroup(ctx, group)
	if err != nil {
		t.Fatalf("create credentials group %q: %v", group, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer ccancel()
		if err := c.DeleteAllAccessKeys(cctx, groupID); err != nil {
			t.Logf("cleanup: drain group %s: %v", groupID, err)
		}
		if err := c.DeleteCredentialsGroup(cctx, groupID); err != nil {
			t.Logf("cleanup: delete group %s: %v", groupID, err)
		}
	})
	firstKey, err := c.CreateAccessKey(ctx, groupID)
	if err != nil {
		t.Fatalf("create access key in %s: %v", groupID, err)
	}
	t.Logf("created as %s: bucket %s, group %s (%s), key %s",
		firstIssuer, bucket, group, groupURN, firstKey.KeyID)

	// --- the rotation -----------------------------------------------------
	reloader := NewKeyReloader(c, staged.path, 30*time.Second, nil)

	t.Run("an unchanged file is a no-op", func(t *testing.T) {
		res := tickOnce(t, ctx, reloader)
		if res.Outcome != ReloadUnchanged {
			t.Fatalf("outcome = %q (err %v), want %q", res.Outcome, res.Err, ReloadUnchanged)
		}
		if c.keyHash() != firstHash {
			t.Errorf("the key changed on an unchanged file")
		}
	})

	t.Run("the second key is proven and adopted", func(t *testing.T) {
		staged.replaceWith(after)

		var res ReloadResult
		// One retry: a single provider blip must not read as a failed feature.
		// A non-definitive rejection is exactly the class that says the
		// provider, not the candidate, was the problem.
		for attempt := 0; attempt < 2; attempt++ {
			res = tickOnce(t, ctx, reloader)
			if res.Outcome == ReloadApplied || res.Definitive {
				break
			}
			t.Logf("attempt %d: non-definitive rejection (%v); retrying once", attempt, res.Err)
		}
		if res.Outcome != ReloadApplied {
			t.Fatalf("outcome = %q, definitive %v, err %v — want the rotation to be adopted",
				res.Outcome, res.Definitive, res.Err)
		}

		// Four independent observables, because the project cannot be one:
		// both keys name the same project by construction.
		if got := c.Account().Issuer; got == firstIssuer {
			t.Errorf("issuer still %q; the client is running on the old key", got)
		}
		if got := c.keyHash(); got == firstHash {
			t.Errorf("key hash unchanged after a reported swap")
		} else if got != res.Hash {
			t.Errorf("client hash %q does not match the reported hash %q", got, res.Hash)
		}
		if c.state.Load() == firstState {
			t.Errorf("the client state pointer was not replaced")
		}
		if got := c.ProjectID(); got != acc.ProjectID {
			t.Errorf("ProjectID = %q, want %q — a reload must never move the project", got, acc.ProjectID)
		}
		// ADR 0016 D11: the project-scoped cache survives the swap.
		if !c.serviceReady.Load() {
			t.Errorf("serviceReady was cleared by the swap; it is project-scoped and must be kept")
		}
		t.Logf("adopted %s", c.Account().Issuer)
	})

	if c.Account().Issuer == firstIssuer {
		t.Fatal("the rotation did not happen; the assertions below would prove nothing")
	}

	// --- what the SECOND service account can still see and do -------------
	//
	// This is the additive half of the assumption ADR 0016 D11 rests on: a
	// credentials group and its access keys are project resources, and the
	// service account is only the caller that created them.
	t.Run("resources of the first account survive the rotation", func(t *testing.T) {
		exists, err := c.BucketExists(ctx, bucket)
		if err != nil {
			t.Fatalf("bucket exists under the new key: %v", err)
		}
		if !exists {
			t.Errorf("bucket %s is not visible to the new service account", bucket)
		}

		id, urn, found, err := c.FindCredentialsGroupByName(ctx, group)
		if err != nil {
			t.Fatalf("find group under the new key: %v", err)
		}
		if !found {
			t.Fatalf("credentials group %s is not visible to the new service account", group)
		}
		if id != groupID || urn != groupURN {
			t.Errorf("group identity changed across the rotation: id %s->%s, urn %s->%s",
				groupID, id, groupURN, urn)
		}

		ids, err := c.ListAccessKeyIDs(ctx, groupID)
		if err != nil {
			t.Fatalf("list access keys under the new key: %v", err)
		}
		var kept bool
		for _, k := range ids {
			if k == firstKey.KeyID {
				kept = true
			}
		}
		if !kept {
			t.Errorf("access key %s minted by %s is gone after rotating to %s; keys listed: %v",
				firstKey.KeyID, firstIssuer, c.Account().Issuer, ids)
		}
	})

	t.Run("the new account can manage what the old one created", func(t *testing.T) {
		// Visibility is not authority. Minting a key into the other account's
		// group is what proves the group is a project resource rather than
		// something owned by its creator.
		second, err := c.CreateAccessKey(ctx, groupID)
		if err != nil {
			t.Fatalf("create access key in the first account's group: %v", err)
		}
		if err := c.DeleteAccessKey(ctx, groupID, second.KeyID); err != nil {
			t.Errorf("delete the key just created: %v", err)
		}
	})

	// --- rejections, under the live provider ------------------------------
	t.Run("a foreign project is refused before any call", func(t *testing.T) {
		staged.mutate(after, func(doc map[string]any) {
			doc["projectId"] = "99999999-9999-9999-9999-999999999999"
		})
		assertRefused(t, ctx, c, reloader, true, "foreign project")
	})

	t.Run("unusable key material is refused before any call", func(t *testing.T) {
		staged.mutate(after, func(doc map[string]any) {
			doc["credentials"].(map[string]any)["privateKey"] = "-----BEGIN RSA PRIVATE KEY-----\ntruncated\n"
		})
		assertRefused(t, ctx, c, reloader, true, "truncated key material")
	})

	t.Run("the real token endpoint refuses a key it cannot verify", func(t *testing.T) {
		// Everything the provider matches on stays intact — project, issuer,
		// subject, kid, audience, token endpoint — and only the private key is
		// replaced, so the assertion is signed with a key STACKIT has never
		// seen. This is the one leg that reaches the live token endpoint.
		staged.mutate(after, func(doc map[string]any) {
			doc["credentials"].(map[string]any)["privateKey"] = freshPrivateKeyPEM(t)
		})
		res := assertRefused(t, ctx, c, reloader, false, "unverifiable signature")
		// The shape of this refusal is not recorded anywhere yet; print it so
		// the provider-error page can be trued up from a measurement.
		t.Logf("live refusal shape: definitive=%v ProviderRefused=%v status=%d err=%v",
			res.Definitive, ProviderRefused(res.Err), StatusCode(errors.Unwrap(res.Err)), res.Err)
		if !res.Definitive {
			t.Logf("NOTE: the provider did not answer this with a structured 4xx, so the operator " +
				"treats it as a failure to reach the provider and retries every interval")
		}
	})

	t.Run("the working key is still live after every refusal", func(t *testing.T) {
		if got := c.Account().Issuer; got == firstIssuer {
			t.Fatalf("issuer fell back to %q", got)
		}
		if _, err := c.ListCredentialsGroups(ctx); err != nil {
			t.Fatalf("the client no longer works after the refused candidates: %v", err)
		}
	})
}

// assertRefused replaces the candidate, drives one poll, and requires that the
// running key is untouched. wantDefinitive is checked only when it is true: the
// local guards are definitive by construction, while what the live endpoint
// answers for an unverifiable signature is what this test is measuring.
func assertRefused(t *testing.T, ctx context.Context, c *Client, r *KeyReloader,
	wantDefinitive bool, what string) ReloadResult {
	t.Helper()
	liveIssuer := c.Account().Issuer
	liveHash := c.keyHash()

	res := tickOnce(t, ctx, r)
	if res.Outcome != ReloadRejected {
		t.Fatalf("%s: outcome = %q, want %q", what, res.Outcome, ReloadRejected)
	}
	if res.Err == nil {
		t.Errorf("%s: no reason reported", what)
	}
	if wantDefinitive && !res.Definitive {
		t.Errorf("%s: Definitive = false, want true (err: %v)", what, res.Err)
	}
	if c.Account().Issuer != liveIssuer || c.keyHash() != liveHash {
		t.Fatalf("%s: the refused candidate displaced the running key", what)
	}
	return res
}
