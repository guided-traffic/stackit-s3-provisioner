package main

import (
	"testing"
	"time"

	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("TEST_ENV_OR_DEFAULT", "set")
	if got := envOrDefault("TEST_ENV_OR_DEFAULT", "def"); got != "set" {
		t.Errorf("envOrDefault(set) = %q, want set", got)
	}
	if got := envOrDefault("TEST_ENV_OR_DEFAULT_UNSET", "def"); got != "def" {
		t.Errorf("envOrDefault(unset) = %q, want def", got)
	}
}

func TestEnvBoolOrDefault(t *testing.T) {
	t.Setenv("TEST_ENV_BOOL", "true")
	if !envBoolOrDefault("TEST_ENV_BOOL", false) {
		t.Error("envBoolOrDefault(true) = false")
	}
	t.Setenv("TEST_ENV_BOOL", "not-a-bool")
	if !envBoolOrDefault("TEST_ENV_BOOL", true) {
		t.Error("envBoolOrDefault(garbage) must fall back to default")
	}
	if envBoolOrDefault("TEST_ENV_BOOL_UNSET", false) {
		t.Error("envBoolOrDefault(unset) must fall back to default")
	}
}

func TestEnvDurationOrDefault(t *testing.T) {
	t.Setenv("TEST_ENV_DURATION", "45s")
	if got := envDurationOrDefault("TEST_ENV_DURATION", time.Minute); got != 45*time.Second {
		t.Errorf("envDurationOrDefault(45s) = %s, want 45s", got)
	}
	// "0" has to survive the parse rather than reading as unset: for the
	// service-account key reload it is the documented off switch, not an
	// absent value.
	t.Setenv("TEST_ENV_DURATION", "0")
	if got := envDurationOrDefault("TEST_ENV_DURATION", 30*time.Second); got != 0 {
		t.Errorf("envDurationOrDefault(0) = %s, want 0 — 0 disables a feature and must not fall back", got)
	}
	// A bare number is not a Go duration. The helper falls back to the default
	// rather than failing, so the chart's warning about crash-looping applies
	// to the flag path only.
	t.Setenv("TEST_ENV_DURATION", "30")
	if got := envDurationOrDefault("TEST_ENV_DURATION", 30*time.Second); got != 30*time.Second {
		t.Errorf("envDurationOrDefault(bare number) = %s, want the default", got)
	}
	if got := envDurationOrDefault("TEST_ENV_DURATION_UNSET", time.Hour); got != time.Hour {
		t.Errorf("envDurationOrDefault(unset) = %s, want the default", got)
	}
}

// TestSetupSAKeyReloadStaysOff pins the two ways the reload is not running.
// The manager is nil on purpose: neither case may reach it, and a nil manager
// is the cheapest possible proof of that.
func TestSetupSAKeyReloadStaysOff(t *testing.T) {
	client, err := stackit.NewClientWithEndpoint(
		"11111111-2222-3333-4444-555555555555", stackit.RegionEU01, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("NewClientWithEndpoint: %v", err)
	}
	t.Run("skeleton mode has no key to reload", func(t *testing.T) {
		if err := setupSAKeyReload(nil, nil, nil, "", 30*time.Second); err != nil {
			t.Errorf("setupSAKeyReload(no client) = %v, want nil", err)
		}
	})
	t.Run("an interval of zero switches it off", func(t *testing.T) {
		if err := setupSAKeyReload(nil, client, nil, "/etc/stackit/sa-key.json", 0); err != nil {
			t.Errorf("setupSAKeyReload(interval 0) = %v, want nil", err)
		}
	})
}
