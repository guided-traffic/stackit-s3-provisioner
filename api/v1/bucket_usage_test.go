package v1

import (
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func TestUsageEnabled(t *testing.T) {
	tests := []struct {
		name           string
		spec           *UsageSpec
		defaultEnabled bool
		want           bool
	}{
		{"no block inherits the default (off)", nil, false, false},
		{"no block inherits the default (on)", nil, true, true},
		{"empty block inherits the default", &UsageSpec{}, true, true},
		{"explicit true overrides an off default", &UsageSpec{Enabled: boolPtr(true)}, false, true},
		{"explicit false overrides an on default", &UsageSpec{Enabled: boolPtr(false)}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.UsageEnabled(tc.defaultEnabled); got != tc.want {
				t.Fatalf("UsageEnabled(%t) = %t, want %t", tc.defaultEnabled, got, tc.want)
			}
		})
	}
}

func TestUsageIncludeVersions(t *testing.T) {
	tests := []struct {
		name           string
		spec           *UsageSpec
		defaultInclude bool
		want           bool
	}{
		{"nil inherits", nil, true, true},
		{"unset inherits", &UsageSpec{}, false, false},
		{"explicit true", &UsageSpec{IncludeVersions: boolPtr(true)}, false, true},
		{"explicit false", &UsageSpec{IncludeVersions: boolPtr(false)}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.UsageIncludeVersions(tc.defaultInclude); got != tc.want {
				t.Fatalf("UsageIncludeVersions(%t) = %t, want %t", tc.defaultInclude, got, tc.want)
			}
		})
	}
}

func TestUsageInterval(t *testing.T) {
	tests := []struct {
		name    string
		spec    *UsageSpec
		want    time.Duration
		wantErr bool
	}{
		{"nil means no request", nil, 0, false},
		{"empty means no request", &UsageSpec{}, 0, false},
		{"parses a duration", &UsageSpec{Interval: "90m"}, 90 * time.Minute, false},
		{"parses hours", &UsageSpec{Interval: "6h"}, 6 * time.Hour, false},
		// A bare number is the mistake this rejects loudly rather than
		// silently falling back to the operator default.
		{"rejects a bare number", &UsageSpec{Interval: "60"}, 0, true},
		{"rejects garbage", &UsageSpec{Interval: "soon"}, 0, true},
		{"rejects zero", &UsageSpec{Interval: "0s"}, 0, true},
		{"rejects a negative interval", &UsageSpec{Interval: "-5m"}, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.UsageInterval()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UsageInterval() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("UsageInterval(): %v", err)
			}
			if got != tc.want {
				t.Fatalf("UsageInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}
