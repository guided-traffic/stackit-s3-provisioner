package stackit

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// writeTestSAKey generates a throwaway RSA key and writes a structurally valid
// STACKIT service-account key file (key flow). The key never talks to the real
// API; it only has to satisfy the SDK's parsing and JWT-signer setup.
func writeTestSAKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	doc := map[string]any{
		"id":           "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"projectId":    fakeProject,
		"active":       true,
		"createdAt":    "2026-01-01T00:00:00Z",
		"keyAlgorithm": "RSA_2048",
		"keyOrigin":    "GENERATED",
		"keyType":      "USER_MANAGED",
		"publicKey":    "",
		"credentials": map[string]any{
			"aud":        "https://stackit-service-account-prod.apps.01.cf.eu01.stackit.cloud",
			"iss":        "test-sa@sa.stackit.cloud",
			"kid":        "11111111-2222-3333-4444-555555555555",
			"sub":        "99999999-8888-7777-6666-555555555555",
			"privateKey": string(keyPEM),
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal key doc: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sa-key.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
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
