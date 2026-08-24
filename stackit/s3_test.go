package stackit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildIsolationPolicy(t *testing.T) {
	const (
		bucket   = "s3op-test-bucket"
		adminURN = "urn:sgws:identity::123:group/admin"
		workURN  = "urn:sgws:identity::123:group/workload"
	)
	policy := BuildIsolationPolicy(bucket, adminURN, workURN, nil)

	var doc struct {
		Statement []struct {
			Sid          string                  `json:"Sid"`
			Effect       string                  `json:"Effect"`
			Action       []string                `json:"Action"`
			NotAction    []string                `json:"NotAction"`
			Resource     []string                `json:"Resource"`
			Principal    *struct{ AWS string }   `json:"Principal"`
			NotPrincipal *struct{ AWS []string } `json:"NotPrincipal"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, policy)
	}
	if len(doc.Statement) != 2 {
		t.Fatalf("want 2 statements, got %d: %s", len(doc.Statement), policy)
	}

	// Statement 1: Deny + NotPrincipal listing admin AND workload, all actions.
	s1 := doc.Statement[0]
	if s1.Effect != "Deny" {
		t.Errorf("stmt1 effect = %q, want Deny", s1.Effect)
	}
	if s1.NotPrincipal == nil || len(s1.NotPrincipal.AWS) != 2 {
		t.Fatalf("stmt1 NotPrincipal must list admin+workload, got %+v", s1.NotPrincipal)
	}
	if s1.NotPrincipal.AWS[0] != adminURN || s1.NotPrincipal.AWS[1] != workURN {
		t.Errorf("stmt1 NotPrincipal = %v, want [%s %s]", s1.NotPrincipal.AWS, adminURN, workURN)
	}
	if len(s1.Action) != 1 || s1.Action[0] != actionAll {
		t.Errorf("stmt1 Action = %v, want [s3:*]", s1.Action)
	}

	// Statement 2: Deny + workload Principal, NotAction restricting to object ops.
	s2 := doc.Statement[1]
	if s2.Effect != "Deny" {
		t.Errorf("stmt2 effect = %q, want Deny", s2.Effect)
	}
	if s2.Principal == nil || s2.Principal.AWS != workURN {
		t.Errorf("stmt2 Principal = %+v, want workload %s", s2.Principal, workURN)
	}
	for _, want := range []string{
		"s3:GetObject", "s3:PutObject", "s3:PutOverwriteObject", "s3:DeleteObject",
		"s3:ListBucket", "s3:ListBucketVersions", "s3:GetBucketLocation",
		"s3:ListBucketMultipartUploads", "s3:ListMultipartUploadParts", "s3:AbortMultipartUpload",
		"s3:GetObjectTagging", "s3:PutObjectTagging", "s3:DeleteObjectTagging",
		"s3:GetObjectVersion", "s3:GetObjectVersionTagging",
		"s3:GetBucketVersioning", "s3:GetBucketObjectLockConfiguration",
	} {
		if !contains(s2.NotAction, want) {
			t.Errorf("stmt2 NotAction missing %q: %v", want, s2.NotAction)
		}
	}

	// Both statements scope to the bucket and its objects.
	for i, s := range []struct{ res []string }{{s1.Resource}, {s2.Resource}} {
		if !contains(s.res, "arn:aws:s3:::"+bucket) || !contains(s.res, "arn:aws:s3:::"+bucket+"/*") {
			t.Errorf("stmt%d Resource = %v, want bucket + objects arns", i+1, s.res)
		}
	}
}

// TestWorkloadAllowedActions_DeniedByDesign guards the exemption list against
// well-meant additions: statement 2 is a Deny+NotAction, so anything added here
// is granted to the workload credentials. These actions must never appear —
// they would let a workload lift its own restrictions (policy), route bucket
// contents elsewhere (replication/notification), pin objects so the bucket can
// no longer be torn down (retention/legal hold/compliance), destroy version
// history, or reconfigure the bucket the operator owns.
func TestWorkloadAllowedActions_DeniedByDesign(t *testing.T) {
	for _, forbidden := range []string{
		actionAll,
		"s3:GetBucketPolicy", "s3:PutBucketPolicy", "s3:DeleteBucketPolicy",
		"s3:PutReplicationConfiguration", "s3:PutBucketNotification", "s3:PutBucketMetadataNotification",
		"s3:PutObjectRetention", "s3:PutObjectLegalHold", "s3:PutBucketObjectLockConfiguration",
		"s3:PutBucketCompliance", "s3:BypassGovernanceRetention",
		"s3:DeleteObjectVersion", "s3:DeleteObjectVersionTagging", "s3:PutObjectVersionTagging",
		"s3:PutBucketVersioning", "s3:PutLifecycleConfiguration", "s3:PutEncryptionConfiguration",
		"s3:PutBucketCORS", "s3:PutBucketTagging", "s3:DeleteBucket", "s3:CreateBucket",
	} {
		if contains(workloadAllowedActions, forbidden) {
			t.Errorf("workloadAllowedActions grants %q to the workload principal, "+
				"which breaks bucket isolation or teardown", forbidden)
		}
	}
}

func TestBuildIsolationPolicy_AdminAlwaysExempt(t *testing.T) {
	// Guardrail: the admin URN must always remain in NotPrincipal, else the policy
	// can lock out the account (INIT-SETUP.md §5, guardrail 4).
	policy := BuildIsolationPolicy("b", "urn:admin", "urn:work", nil)
	if !strings.Contains(policy, "urn:admin") {
		t.Fatalf("admin urn absent from policy (lockout risk): %s", policy)
	}
}

func TestPoliciesEquivalent(t *testing.T) {
	a := BuildIsolationPolicy("b", "urn:admin", "urn:work", nil)

	tests := []struct {
		name string
		b    string
		want bool
	}{
		{"identical", a, true},
		{"reordered keys / whitespace", `{  "Statement" : [` + extractStatements(t, a) + `] }`, true},
		{"different workload urn", BuildIsolationPolicy("b", "urn:admin", "urn:other", nil), false},
		{"empty vs policy", "", false},
		{"both empty", "", true},
		{"invalid json falls back to byte compare", "{not json", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			left := a
			if tc.name == "both empty" {
				left = ""
			}
			if got := PoliciesEquivalent(left, tc.b); got != tc.want {
				t.Errorf("PoliciesEquivalent(%q, %q) = %v, want %v", left, tc.b, got, tc.want)
			}
		})
	}
}

// extractStatements returns the inner JSON array elements of a policy's
// Statement, so a re-wrapped document with reordered keys can be compared.
func extractStatements(t *testing.T, policy string) string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(doc["Statement"], &arr); err != nil {
		t.Fatalf("unmarshal statements: %v", err)
	}
	parts := make([]string, len(arr))
	for i, s := range arr {
		parts[i] = string(s)
	}
	return strings.Join(parts, ",")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// --- spec.grantReadAccess: read-only reader principals -----------------------

// policyDoc is the decoded shape used by the reader tests. Principal is decoded
// as `any` because statement 2 carries a single URN string while statement 3
// carries a list.
type policyDoc struct {
	Statement []struct {
		Sid          string                  `json:"Sid"`
		Effect       string                  `json:"Effect"`
		Action       []string                `json:"Action"`
		NotAction    []string                `json:"NotAction"`
		Resource     []string                `json:"Resource"`
		Principal    *struct{ AWS any }      `json:"Principal"`
		NotPrincipal *struct{ AWS []string } `json:"NotPrincipal"`
	} `json:"Statement"`
}

func parsePolicy(t *testing.T, policy string) policyDoc {
	t.Helper()
	var doc policyDoc
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, policy)
	}
	return doc
}

// principalList returns a statement's Principal as a string slice regardless of
// whether it was encoded as a single string or a list.
func principalList(v any) []string {
	switch p := v.(type) {
	case string:
		return []string{p}
	case []any:
		out := make([]string, 0, len(p))
		for _, e := range p {
			s, _ := e.(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

// preGrantPolicy is the exact document BuildIsolationPolicy produced before
// spec.grantReadAccess existed, captured from the commit that preceded the
// feature. Pinning it as a literal is the point: comparing the new builder
// against itself would prove nothing about the upgrade path, and a bucket that
// does not use the feature must keep the policy it already has — otherwise the
// first reconcile after an operator upgrade rewrites every bucket in the
// project.
const preGrantPolicy = `{"Statement":[{"Action":["s3:*"],"Effect":"Deny",` +
	`"NotPrincipal":{"AWS":["urn:admin","urn:work"]},` +
	`"Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"],` +
	`"Sid":"deny-all-except-admin-and-workload"},` +
	`{"Effect":"Deny","NotAction":["s3:GetObject","s3:PutObject",` +
	`"s3:PutOverwriteObject","s3:DeleteObject","s3:ListBucket",` +
	`"s3:ListBucketVersions","s3:GetBucketLocation",` +
	`"s3:ListBucketMultipartUploads","s3:ListMultipartUploadParts",` +
	`"s3:AbortMultipartUpload","s3:GetObjectTagging","s3:PutObjectTagging",` +
	`"s3:DeleteObjectTagging","s3:GetObjectVersion","s3:GetObjectVersionTagging",` +
	`"s3:GetBucketVersioning","s3:GetBucketObjectLockConfiguration"],` +
	`"Principal":{"AWS":"urn:work"},` +
	`"Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"],` +
	`"Sid":"workload-objects-only"}]}`

// TestBuildIsolationPolicy_NoReadersUnchanged is the regression guard for
// buckets that do not use spec.grantReadAccess: adding the feature must not
// change their policy document by a single byte, or every existing bucket would
// be rewritten on the first reconcile after an operator upgrade.
func TestBuildIsolationPolicy_NoReadersUnchanged(t *testing.T) {
	base := BuildIsolationPolicy("b", "urn:admin", "urn:work", nil)
	if base != preGrantPolicy {
		t.Fatalf("policy of a bucket without grants changed:\n got %s\nwant %s", base, preGrantPolicy)
	}
	for name, readers := range map[string][]string{
		"nil":              nil,
		"empty slice":      {},
		"only empty entry": {""},
		"only whitespace":  {"   "},
		"only admin":       {"urn:admin"},
		"only workload":    {"urn:work"},
		"admin+workload":   {"urn:admin", "urn:work"},
	} {
		t.Run(name, func(t *testing.T) {
			got := BuildIsolationPolicy("b", "urn:admin", "urn:work", readers)
			if got != base {
				t.Errorf("policy changed for readers=%v:\n got %s\nwant %s", readers, got, base)
			}
			if n := len(parsePolicy(t, got).Statement); n != 2 {
				t.Errorf("want 2 statements, got %d", n)
			}
		})
	}
}

// TestBuildIsolationPolicy_Readers checks the three-statement shape produced by
// a grant: readers are exempted from the blanket deny (statement 1) and then
// confined to the read-only action set (statement 3), while the owner's own
// statement 2 is untouched.
func TestBuildIsolationPolicy_Readers(t *testing.T) {
	const (
		adminURN = "urn:sgws:identity::123:group/admin"
		workURN  = "urn:sgws:identity::123:group/workload"
		readerA  = "urn:sgws:identity::123:group/reader-a"
		readerB  = "urn:sgws:identity::123:group/reader-b"
	)
	policy := BuildIsolationPolicy("data", adminURN, workURN, []string{readerB, readerA})
	doc := parsePolicy(t, policy)
	if len(doc.Statement) != 3 {
		t.Fatalf("want 3 statements, got %d: %s", len(doc.Statement), policy)
	}

	// Statement 1 must exempt admin, workload and both readers; a reader left
	// out here would be denied everything regardless of statement 3.
	s1 := doc.Statement[0]
	if s1.NotPrincipal == nil {
		t.Fatal("stmt1 has no NotPrincipal")
	}
	wantExempt := []string{adminURN, workURN, readerA, readerB}
	if len(s1.NotPrincipal.AWS) != len(wantExempt) {
		t.Fatalf("stmt1 NotPrincipal = %v, want %v", s1.NotPrincipal.AWS, wantExempt)
	}
	for i, want := range wantExempt {
		if s1.NotPrincipal.AWS[i] != want {
			t.Errorf("stmt1 NotPrincipal[%d] = %q, want %q", i, s1.NotPrincipal.AWS[i], want)
		}
	}

	// Statement 2 (the owner) is unchanged by the grant.
	s2 := doc.Statement[1]
	if got := principalList(s2.Principal.AWS); len(got) != 1 || got[0] != workURN {
		t.Errorf("stmt2 Principal = %v, want [%s]", got, workURN)
	}
	if len(s2.NotAction) != len(workloadAllowedActions) {
		t.Errorf("stmt2 NotAction has %d entries, want %d", len(s2.NotAction), len(workloadAllowedActions))
	}

	// Statement 3 confines the readers.
	s3 := doc.Statement[2]
	if s3.Sid != "granted-readers-read-only" {
		t.Errorf("stmt3 Sid = %q", s3.Sid)
	}
	if s3.Effect != effectDeny {
		t.Errorf("stmt3 Effect = %q, want Deny", s3.Effect)
	}
	if got := principalList(s3.Principal.AWS); len(got) != 2 || got[0] != readerA || got[1] != readerB {
		t.Errorf("stmt3 Principal = %v, want sorted [%s %s]", got, readerA, readerB)
	}
	if len(s3.NotAction) != len(readerAllowedActions) {
		t.Fatalf("stmt3 NotAction = %v, want %v", s3.NotAction, readerAllowedActions)
	}
	for i, want := range readerAllowedActions {
		if s3.NotAction[i] != want {
			t.Errorf("stmt3 NotAction[%d] = %q, want %q", i, s3.NotAction[i], want)
		}
	}
	wantRes := []string{"arn:aws:s3:::data", "arn:aws:s3:::data/*"}
	if len(s3.Resource) != 2 || s3.Resource[0] != wantRes[0] || s3.Resource[1] != wantRes[1] {
		t.Errorf("stmt3 Resource = %v, want %v", s3.Resource, wantRes)
	}
}

// TestBuildIsolationPolicy_ReaderSanitizing pins the security filter: a caller
// that passes the admin URN would otherwise lock the operator out of the bucket
// (the admin key would lose PutBucketPolicy and could never repair the policy),
// and passing the workload URN would silently narrow the bucket owner's own
// access, because Deny statements intersect rather than override.
func TestBuildIsolationPolicy_ReaderSanitizing(t *testing.T) {
	const (
		adminURN = "urn:admin"
		workURN  = "urn:work"
		readerA  = "urn:reader-a"
	)
	tests := []struct {
		name    string
		readers []string
		want    []string
	}{
		{"admin dropped", []string{adminURN, readerA}, []string{readerA}},
		{"workload dropped", []string{workURN, readerA}, []string{readerA}},
		{"self-grant is a no-op", []string{workURN}, nil},
		{"duplicates collapsed", []string{readerA, readerA, readerA}, []string{readerA}},
		{"empty entries dropped", []string{"", readerA, "  "}, []string{readerA}},
		{"whitespace trimmed", []string{" " + readerA + " "}, []string{readerA}},
		{"sorted", []string{"urn:c", "urn:a", "urn:b"}, []string{"urn:a", "urn:b", "urn:c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := parsePolicy(t, BuildIsolationPolicy("b", adminURN, workURN, tc.readers))
			if len(tc.want) == 0 {
				if len(doc.Statement) != 2 {
					t.Fatalf("want 2 statements (no reader survives), got %d", len(doc.Statement))
				}
				return
			}
			if len(doc.Statement) != 3 {
				t.Fatalf("want 3 statements, got %d", len(doc.Statement))
			}
			got := principalList(doc.Statement[2].Principal.AWS)
			if len(got) != len(tc.want) {
				t.Fatalf("stmt3 Principal = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("stmt3 Principal[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
			// The admin must stay exempt from the blanket deny in every case.
			if doc.Statement[0].NotPrincipal.AWS[0] != adminURN {
				t.Errorf("admin no longer first in stmt1 NotPrincipal: %v", doc.Statement[0].NotPrincipal.AWS)
			}
		})
	}
}

// TestBuildIsolationPolicy_ReaderOrderDeterministic guards the drift check: the
// same grant set in a different order must yield the same document, otherwise
// ensureBucketPolicy would rewrite the policy on every reconcile.
func TestBuildIsolationPolicy_ReaderOrderDeterministic(t *testing.T) {
	a := BuildIsolationPolicy("b", "urn:admin", "urn:work", []string{"urn:x", "urn:y", "urn:z"})
	b := BuildIsolationPolicy("b", "urn:admin", "urn:work", []string{"urn:z", "urn:x", "urn:y"})
	if a != b {
		t.Errorf("policy depends on reader order:\n%s\n%s", a, b)
	}
	if !PoliciesEquivalent(a, b) {
		t.Error("PoliciesEquivalent should consider the two documents equal")
	}
}

// TestReaderAllowedActions_ReadOnly is the invariant test behind the whole
// feature: whatever ends up in readerAllowedActions must be a strict, read-only
// subset of what the bucket owner itself may do. A reader that could write,
// delete or reconfigure would break the grant's contract, and an action the
// owner does not even have would be a privilege inversion.
func TestReaderAllowedActions_ReadOnly(t *testing.T) {
	owner := make(map[string]bool, len(workloadAllowedActions))
	for _, a := range workloadAllowedActions {
		owner[a] = true
	}

	// Verbs that mutate state. Anything carrying one of these prefixes is a
	// write, so no future edit can smuggle one into the reader set.
	mutating := []string{"s3:Put", "s3:Delete", "s3:Create", "s3:Abort", "s3:Bypass", "s3:Restore", "s3:Replicate"}

	seen := make(map[string]bool, len(readerAllowedActions))
	for _, a := range readerAllowedActions {
		if !owner[a] {
			t.Errorf("reader action %q is not granted to the bucket owner itself", a)
		}
		for _, verb := range mutating {
			if strings.HasPrefix(a, verb) {
				t.Errorf("reader action %q is mutating (prefix %q); readers must be read-only", a, verb)
			}
		}
		if seen[a] {
			t.Errorf("reader action %q listed twice", a)
		}
		seen[a] = true
	}

	// Multipart listings expose the owner's in-flight uploads and are
	// deliberately excluded; assert that on purpose so a later "just add the
	// missing list actions" edit has to face this test.
	for _, forbidden := range []string{
		"s3:ListBucketMultipartUploads", "s3:ListMultipartUploadParts", "s3:AbortMultipartUpload",
	} {
		if seen[forbidden] {
			t.Errorf("reader action %q exposes or mutates the owner's in-flight uploads", forbidden)
		}
	}

	// The actions s3cmd needs for `ls` and `get` must be present, or the
	// feature does not deliver what it promises.
	for _, required := range []string{"s3:ListBucket", "s3:GetObject", "s3:GetBucketLocation"} {
		if !seen[required] {
			t.Errorf("reader action %q missing; s3cmd ls/get would fail", required)
		}
	}
}

// TestSanitizeReaderURNs_TrimmedComparison pins the fix for an asymmetry in the
// lockout guard: the candidate URN is trimmed before comparison, so comparing it
// against a raw admin/workload URN would let a whitespace-padded value through.
// adminFromSecret reads the admin URN straight out of Secret data without
// trimming, so the padded form is reachable, and the consequence — the admin
// confined to read-only on the bucket, unable to repair its own policy — is the
// worst failure this code has.
func TestSanitizeReaderURNs_TrimmedComparison(t *testing.T) {
	tests := []struct {
		name            string
		admin, workload string
		readers         []string
	}{
		{"padded admin urn in config", " urn:admin ", "urn:work", []string{"urn:admin"}},
		{"padded reader matching admin", "urn:admin", "urn:work", []string{" urn:admin "}},
		{"padded workload urn in config", "urn:admin", "\turn:work\n", []string{"urn:work"}},
		{"padded reader matching workload", "urn:admin", "urn:work", []string{"  urn:work"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeReaderURNs(tc.readers, tc.admin, tc.workload)
			if len(got) != 0 {
				t.Errorf("sanitizeReaderURNs kept %v; admin/workload must never survive as readers", got)
			}
		})
	}
}
