package controller

import (
	"fmt"
	"math"
	"time"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
)

// Defaults for the bucket-size measurement, applied when the operator is
// configured with zero values (Helm always sets them explicitly).
const (
	// DefaultUsageInterval is how often a Bucket that does not ask for something
	// else is measured.
	DefaultUsageInterval = time.Hour
	// DefaultUsageMinInterval is the floor a Bucket cannot undercut. Measuring is
	// a full listing pass, so the floor is what stops a single CR from turning
	// the operator into a listing loop.
	DefaultUsageMinInterval = time.Hour
	// DefaultUsageMaxObjects caps how many listing entries one measurement
	// consumes before it gives up and reports a lower bound.
	DefaultUsageMaxObjects int64 = 2_000_000
	// DefaultUsageCurrency is the currency the cost estimate is rendered in.
	DefaultUsageCurrency = "EUR"
	// DefaultUsageConcurrency is how many buckets are measured at once.
	DefaultUsageConcurrency = 2
)

// Billing constants for the cost estimate.
const (
	// bytesPerGB is a billing gigabyte: STACKIT bills per started GIGABYTE, the
	// decimal unit, not a gibibyte.
	bytesPerGB int64 = 1_000_000_000
	// billingHoursPerMonth is the month length STACKIT's own price list projects
	// its per-month figures with ("a hypothetical subscription period of 720
	// hours (30-day month)"), so an estimate built on it is comparable to the
	// published monthly price.
	billingHoursPerMonth = 720
)

// UsageConfig is the operator-wide policy for measuring bucket sizes. It is set
// once per deployment (Helm bucketUsage.*), never per CR; a Bucket only chooses
// within the bounds it defines.
type UsageConfig struct {
	// Enabled is the hard feature gate. When false the operator measures
	// nothing, whatever a Bucket asks for, and a Bucket that explicitly asked is
	// told so with a warning event. This is the switch an operator flips to stop
	// all listing traffic at once.
	Enabled bool

	// DefaultEnabled is the answer for every Bucket that does not set
	// spec.usage.enabled itself. It is the cluster-wide policy; off by default,
	// so measurement is something a workload opts into.
	DefaultEnabled bool

	// Interval is the measurement interval for Buckets that do not ask for one.
	Interval time.Duration

	// MinInterval is the floor: a Bucket asking to be measured more often is
	// clamped up to it and told so in status.
	MinInterval time.Duration

	// MaxObjects caps the listing entries one measurement consumes. Beyond it the
	// measurement stops and reports a lower bound rather than walking an
	// unbounded bucket.
	MaxObjects int64

	// IncludeVersions is the default for counting non-current versions.
	IncludeVersions bool

	// PricePerGBHour is the price of one gigabyte for one hour, in whole currency
	// units, used for the monthly cost estimate. Zero disables the estimate
	// entirely (no cost is written to any Bucket).
	PricePerGBHour float64

	// Currency labels the estimate (display and metric label only; no conversion
	// happens anywhere).
	Currency string

	// Concurrency is how many buckets are measured in parallel. Measurement runs
	// in its own controller, so this bounds listing traffic without ever
	// competing with provisioning.
	Concurrency int
}

// withDefaults fills unset fields, so a UsageConfig built in a test behaves like
// a Helm-configured one.
func (c UsageConfig) withDefaults() UsageConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultUsageInterval
	}
	if c.MinInterval <= 0 {
		c.MinInterval = DefaultUsageMinInterval
	}
	if c.MaxObjects < 0 {
		c.MaxObjects = 0
	}
	if c.Currency == "" {
		c.Currency = DefaultUsageCurrency
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultUsageConcurrency
	}
	return c
}

// effectiveUsage is the measurement policy resolved for one Bucket: the operator
// defaults with the Bucket's own overrides applied and bounded.
type effectiveUsage struct {
	// enabled reports whether this Bucket is measured at all.
	enabled bool
	// requested reports that the Bucket asked for measurement explicitly, which
	// is what distinguishes "not opted in" from "opted in but gated off".
	requested bool
	// interval is the bounded measurement interval.
	interval time.Duration
	// includeVersions reports whether non-current versions are counted.
	includeVersions bool
	// clamped reports that the Bucket's requested interval was raised to the
	// operator's floor.
	clamped time.Duration
	// err carries an unusable spec.usage value; the Bucket is not measured and
	// the reason is surfaced in status instead of being silently defaulted away.
	err error
}

// effectiveFor resolves the measurement policy for one Bucket.
func (c UsageConfig) effectiveFor(b *s3v1.Bucket) effectiveUsage {
	cfg := c.withDefaults()
	spec := b.Spec.Usage

	eff := effectiveUsage{
		requested:       spec != nil && spec.Enabled != nil && *spec.Enabled,
		interval:        cfg.Interval,
		includeVersions: spec.UsageIncludeVersions(cfg.IncludeVersions),
	}
	// The gate wins over everything a CR can say.
	eff.enabled = cfg.Enabled && spec.UsageEnabled(cfg.DefaultEnabled)

	requested, err := spec.UsageInterval()
	if err != nil {
		eff.err = err
		eff.enabled = false
		return eff
	}
	if requested > 0 {
		eff.interval = requested
		if requested < cfg.MinInterval {
			eff.interval = cfg.MinInterval
			eff.clamped = requested
		}
	}
	return eff
}

// estimateMonthlyCostCents prices a measured size for a month, in whole cents.
//
// It follows STACKIT's stated billing metric — per STARTED gigabyte per started
// hour — so the size is rounded UP to a whole (decimal) gigabyte before it is
// priced, and the month is the 720 hours the published price list projects with.
// A zero price means "no price configured" and yields no estimate.
func estimateMonthlyCostCents(billableBytes int64, pricePerGBHour float64) int64 {
	if pricePerGBHour <= 0 || billableBytes <= 0 {
		return 0
	}
	startedGB := (billableBytes + bytesPerGB - 1) / bytesPerGB
	cents := float64(startedGB) * pricePerGBHour * billingHoursPerMonth * 100
	return int64(math.Round(cents))
}

// formatCost renders a cent amount for display, e.g. "1.23 EUR". A measurement
// that hit the object cap is marked as the lower bound it is.
func formatCost(cents int64, currency string, truncated bool) string {
	if currency == "" {
		currency = DefaultUsageCurrency
	}
	s := fmt.Sprintf("%d.%02d %s", cents/100, cents%100, currency)
	if truncated {
		return ">= " + s
	}
	return s
}

// formatSize renders a measured size for display, marking a capped measurement
// as a lower bound.
func formatSize(bytes int64, truncated bool) string {
	if truncated {
		return ">= " + humanBytes(bytes)
	}
	return humanBytes(bytes)
}
