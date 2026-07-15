package main

import "testing"

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
