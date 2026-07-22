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
	policy := BuildIsolationPolicy(bucket, adminURN, workURN)

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
	if len(s1.Action) != 1 || s1.Action[0] != "s3:*" {
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
		"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:GetBucketLocation",
		"s3:ListBucketMultipartUploads", "s3:ListMultipartUploadParts", "s3:AbortMultipartUpload",
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

func TestBuildIsolationPolicy_AdminAlwaysExempt(t *testing.T) {
	// Guardrail: the admin URN must always remain in NotPrincipal, else the policy
	// can lock out the account (INIT-SETUP.md §5, guardrail 4).
	policy := BuildIsolationPolicy("b", "urn:admin", "urn:work")
	if !strings.Contains(policy, "urn:admin") {
		t.Fatalf("admin urn absent from policy (lockout risk): %s", policy)
	}
}

func TestPoliciesEquivalent(t *testing.T) {
	a := BuildIsolationPolicy("b", "urn:admin", "urn:work")

	tests := []struct {
		name string
		b    string
		want bool
	}{
		{"identical", a, true},
		{"reordered keys / whitespace", `{  "Statement" : [` + extractStatements(t, a) + `] }`, true},
		{"different workload urn", BuildIsolationPolicy("b", "urn:admin", "urn:other"), false},
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
