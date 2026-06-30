package stackit

import (
	"os"
	"path/filepath"
	"testing"
)

// accountPaths returns the two SA key files at the repo root, skipping the test
// if they are absent (e.g. in CI without secrets).
func accountPaths(t *testing.T) (string, string) {
	t.Helper()
	a1 := envOr("STACKIT_ACCOUNT_1", filepath.Join("..", "account-1.json"))
	a2 := envOr("STACKIT_ACCOUNT_2", filepath.Join("..", "account-2.json"))
	for _, p := range []string{a1, a2} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("SA key file %s not present: %v", p, err)
		}
	}
	return a1, a2
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestLoadAccount verifies the SA key files parse and carry the data we depend
// on: a non-empty projectId and issuer, and that the two accounts are distinct
// projects (the basis of the isolation property). Offline — no API calls.
func TestLoadAccount(t *testing.T) {
	p1, p2 := accountPaths(t)

	a1, err := LoadAccount(p1)
	if err != nil {
		t.Fatalf("LoadAccount(%s): %v", p1, err)
	}
	a2, err := LoadAccount(p2)
	if err != nil {
		t.Fatalf("LoadAccount(%s): %v", p2, err)
	}

	if a1.ProjectID == "" || a2.ProjectID == "" {
		t.Fatalf("empty projectId: a1=%q a2=%q", a1.ProjectID, a2.ProjectID)
	}
	if a1.Issuer == "" || a2.Issuer == "" {
		t.Fatalf("empty issuer: a1=%q a2=%q", a1.Issuer, a2.Issuer)
	}
	if a1.ProjectID == a2.ProjectID {
		t.Fatalf("both accounts share project %s — isolation test would be meaningless", a1.ProjectID)
	}
	t.Logf("account-1: project=%s sa=%s", a1.ProjectID, a1.Issuer)
	t.Logf("account-2: project=%s sa=%s", a2.ProjectID, a2.Issuer)
}

func TestLoadAccountErrors(t *testing.T) {
	if _, err := LoadAccount(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"credentials":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAccount(bad); err == nil {
		t.Fatal("expected error for key file without projectId")
	}
}
