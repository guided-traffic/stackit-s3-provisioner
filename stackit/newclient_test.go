package stackit

import (
	"testing"
)

// writeTestSAKey writes a structurally valid STACKIT service-account key file
// into a temporary directory and returns its path. The key never talks to the
// real API; it only has to satisfy the SDK's parsing and JWT-signer setup.
func writeTestSAKey(t *testing.T) string {
	t.Helper()
	return newKeyFile(t, buildSAKey(t)).path
}

func TestNewClient(t *testing.T) {
	t.Run("valid key file", func(t *testing.T) {
		path := writeTestSAKey(t)
		acc, err := LoadAccount(path)
		if err != nil {
			t.Fatalf("LoadAccount: %v", err)
		}
		c, err := NewClient(acc, RegionEU01)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if c.ProjectID() != fakeProject || c.Region() != RegionEU01 {
			t.Errorf("client identity = (%q, %q)", c.ProjectID(), c.Region())
		}
	})

	t.Run("missing key file", func(t *testing.T) {
		if _, err := NewClient(Account{KeyPath: "/does/not/exist.json", Issuer: "x"}, RegionEU01); err == nil {
			t.Error("NewClient with missing key file succeeded, want error")
		}
	})
}
