package controller

import (
	"testing"
	"time"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

func usageBool(v bool) *bool { return &v }

// eu01ListPrice is the STACKIT list price for Object Storage Premium-EU01 per
// GB per hour (price list v1.0.43, 08/04/2026). Keeping the real number in the
// tests is what makes the estimate comparable to the published monthly figure.
const eu01ListPrice = 0.00003697772

func TestEstimateMonthlyCostCents(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		price float64
		want  int64
	}{
		{"no price configured yields no estimate", 5 << 30, 0, 0},
		{"empty bucket costs nothing", 0, eu01ListPrice, 0},
		// STACKIT bills per STARTED gigabyte, so a single byte is a whole GB.
		// 1 GB * 0.00003697772 * 720 h = 0.02662... -> 3 cents.
		{"one byte is billed as one started GB", 1, eu01ListPrice, 3},
		{"exactly one GB stays one GB", bytesPerGB, eu01ListPrice, 3},
		{"one byte over rounds up to two GB", bytesPerGB + 1, eu01ListPrice, 5},
		// 1000 GB * 0.00003697772 * 720 = 26.6239... -> 2662 cents.
		{"a terabyte", 1000 * bytesPerGB, eu01ListPrice, 2662},
		// The published price list shows 0.03 EUR per GB per month for this
		// rate, which is what a single GB must round to.
		{"matches the published per-GB monthly price", bytesPerGB, eu01ListPrice, 3},
		{"a round price stays exact", 100 * bytesPerGB, 0.001, 7200},
		{"a negative price is treated as unconfigured", 1 << 40, -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateMonthlyCostCents(tc.bytes, tc.price); got != tc.want {
				t.Fatalf("estimateMonthlyCostCents(%d, %v) = %d cents, want %d",
					tc.bytes, tc.price, got, tc.want)
			}
		})
	}
}

func TestFormatCost(t *testing.T) {
	tests := []struct {
		name      string
		cents     int64
		currency  string
		truncated bool
		want      string
	}{
		{"whole euros", 500, "EUR", false, "5.00 EUR"},
		{"cents are padded", 3, "EUR", false, "0.03 EUR"},
		{"mixed", 12345, "EUR", false, "123.45 EUR"},
		{"zero", 0, "EUR", false, "0.00 EUR"},
		{"another currency label", 199, "CHF", false, "1.99 CHF"},
		{"an empty currency falls back", 199, "", false, "1.99 EUR"},
		// A capped measurement priced the part it saw, so the cost is a floor.
		{"a truncated measurement is a lower bound", 2662, "EUR", true, ">= 26.62 EUR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatCost(tc.cents, tc.currency, tc.truncated); got != tc.want {
				t.Fatalf("formatCost(%d, %q, %t) = %q, want %q",
					tc.cents, tc.currency, tc.truncated, got, tc.want)
			}
		})
	}
}

func TestFormatSize(t *testing.T) {
	if got := formatSize(3<<30, false); got != "3.0 GiB" {
		t.Fatalf("formatSize = %q, want %q", got, "3.0 GiB")
	}
	if got := formatSize(3<<30, true); got != ">= 3.0 GiB" {
		t.Fatalf("truncated formatSize = %q, want %q", got, ">= 3.0 GiB")
	}
}

func TestUsageConfigEffectiveFor(t *testing.T) {
	base := UsageConfig{
		Enabled:     true,
		Interval:    time.Hour,
		MinInterval: time.Hour,
	}

	t.Run("the operator default decides for a Bucket that says nothing", func(t *testing.T) {
		cfg := base
		cfg.DefaultEnabled = true
		eff := cfg.effectiveFor(&s3v1.Bucket{})
		if !eff.enabled || eff.requested {
			t.Fatalf("enabled=%t requested=%t, want enabled without an explicit request", eff.enabled, eff.requested)
		}
		if eff.interval != time.Hour {
			t.Fatalf("interval = %v, want the operator default", eff.interval)
		}
	})

	t.Run("a Bucket opts in against an off default", func(t *testing.T) {
		b := &s3v1.Bucket{}
		b.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true)}
		eff := base.effectiveFor(b)
		if !eff.enabled || !eff.requested {
			t.Fatalf("enabled=%t requested=%t, want both true", eff.enabled, eff.requested)
		}
	})

	t.Run("a Bucket opts out of an on default", func(t *testing.T) {
		cfg := base
		cfg.DefaultEnabled = true
		b := &s3v1.Bucket{}
		b.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(false)}
		if eff := cfg.effectiveFor(b); eff.enabled {
			t.Fatal("an explicit opt-out must win over the operator default")
		}
	})

	t.Run("the gate overrules an opted-in Bucket", func(t *testing.T) {
		cfg := base
		cfg.Enabled = false
		cfg.DefaultEnabled = true
		b := &s3v1.Bucket{}
		b.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true)}
		eff := cfg.effectiveFor(b)
		if eff.enabled {
			t.Fatal("the operator-wide gate must win over a Bucket asking for measurement")
		}
		// The request has to survive the refusal, or the refusal cannot be
		// distinguished from a Bucket that simply never opted in.
		if !eff.requested {
			t.Fatal("a refused request must still be reported as requested")
		}
	})

	t.Run("a longer interval is honored", func(t *testing.T) {
		b := &s3v1.Bucket{}
		b.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true), Interval: "6h"}
		eff := base.effectiveFor(b)
		if eff.interval != 6*time.Hour || eff.clamped != 0 {
			t.Fatalf("interval = %v clamped = %v, want 6h unclamped", eff.interval, eff.clamped)
		}
	})

	t.Run("a shorter interval is clamped to the floor", func(t *testing.T) {
		b := &s3v1.Bucket{}
		b.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true), Interval: "1m"}
		eff := base.effectiveFor(b)
		if eff.interval != time.Hour {
			t.Fatalf("interval = %v, want the floor of 1h", eff.interval)
		}
		if eff.clamped != time.Minute {
			t.Fatalf("clamped = %v, want the originally requested 1m", eff.clamped)
		}
	})

	t.Run("an unparseable interval disables the measurement", func(t *testing.T) {
		b := &s3v1.Bucket{}
		b.Spec.Usage = &s3v1.UsageSpec{Enabled: usageBool(true), Interval: "60"}
		eff := base.effectiveFor(b)
		if eff.err == nil {
			t.Fatal("want an error for a bare number")
		}
		// Falling back to the default would hide the typo; the Bucket has to be
		// visibly not measured instead.
		if eff.enabled {
			t.Fatal("a Bucket with an unusable interval must not be measured")
		}
	})

	t.Run("version counting follows the operator default unless overridden", func(t *testing.T) {
		cfg := base
		cfg.IncludeVersions = true
		if eff := cfg.effectiveFor(&s3v1.Bucket{}); !eff.includeVersions {
			t.Fatal("want the operator default to apply")
		}
		b := &s3v1.Bucket{}
		b.Spec.Usage = &s3v1.UsageSpec{IncludeVersions: usageBool(false)}
		if eff := cfg.effectiveFor(b); eff.includeVersions {
			t.Fatal("want the Bucket override to apply")
		}
	})
}

func TestUsageConfigWithDefaults(t *testing.T) {
	got := UsageConfig{}.withDefaults()
	if got.Interval != DefaultUsageInterval || got.MinInterval != DefaultUsageMinInterval {
		t.Fatalf("intervals = %v/%v, want the documented defaults", got.Interval, got.MinInterval)
	}
	if got.Currency != DefaultUsageCurrency || got.Concurrency != DefaultUsageConcurrency {
		t.Fatalf("currency/concurrency = %q/%d, want the documented defaults", got.Currency, got.Concurrency)
	}
	// A zero cap means "no cap" and must survive defaulting, or the operator
	// could never be configured for unbounded measurement.
	if got.MaxObjects != 0 {
		t.Fatalf("MaxObjects = %d, want 0 (uncapped) to be preserved", got.MaxObjects)
	}
}

func TestNextMeasurementSpreadsBuckets(t *testing.T) {
	interval := time.Hour
	a := &s3v1.Bucket{}
	a.Namespace, a.Name = "ns", "a"
	b := &s3v1.Bucket{}
	b.Namespace, b.Name = "ns", "b"

	da, db := nextMeasurement(a, interval), nextMeasurement(b, interval)
	if da == db {
		t.Fatalf("both Buckets scheduled at %v; the skew must separate them", da)
	}
	for _, d := range []time.Duration{da, db} {
		if d < interval || d >= interval+interval/10 {
			t.Fatalf("delay %v outside [%v, %v)", d, interval, interval+interval/10)
		}
	}
	// The skew is derived from the Bucket identity, so it must be stable across
	// calls — otherwise the schedule would wander on every reconcile.
	if again := nextMeasurement(a, interval); again != da {
		t.Fatalf("delay changed between calls: %v then %v", da, again)
	}
}
