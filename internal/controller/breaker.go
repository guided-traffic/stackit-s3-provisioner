package controller

import (
	"errors"
	"sync"
	"time"
)

// errProviderUnavailable is what a Bucket held by the open breaker records in
// its status: the operator did not fail to provision it, it declined to try
// while the provider is known to be down.
var errProviderUnavailable = errors.New("StackIT API unavailable (provider circuit open)")

// Default circuit breaker tuning. Only the threshold and the cooldown cap are
// configurable; the base cooldown is fixed because it only decides how quickly
// the first probe follows the trip, which nothing operational depends on.
const (
	// DefaultCircuitThreshold is how many reconciles must fail back to back,
	// with no successful reconcile in between, before the breaker opens.
	DefaultCircuitThreshold = 3
	// DefaultCircuitMaxCooldown caps how long the breaker stays shut before it
	// probes the provider again, and therefore how long recovery can lag behind
	// the provider coming back.
	DefaultCircuitMaxCooldown = 5 * time.Minute
	// circuitBaseCooldown is the wait after the first trip. Each probe that
	// fails again doubles it, up to the configured cap.
	//
	// A minute rather than something shorter because the open state has to be
	// OBSERVABLE, not just effective: the StackitS3ReconcileErrors alert
	// suppresses itself for windows in which the circuit was open, and the chart
	// scrapes every 30s, so a trip must outlive a scrape gap to be seen. With a
	// scrape interval coarser than this, a single short trip can be missed and
	// the alert falls back to firing — the fail-open direction.
	circuitBaseCooldown = time.Minute
)

// ProviderBreaker is a fleet-wide circuit breaker over Bucket reconciles.
//
// It exists because a provider outage is a property of the provider, not of any
// one Bucket: while the StackIT API answers 503, reconciling the seventeenth
// Bucket cannot succeed, and attempting it costs API calls that make the outage
// worse. On mgmt-p on 2026-09-02 a provider-side 503 storm starting at 14:23
// drove the operator into "rate limit on IP level exceeded" by 14:34 and into 51
// rate-limited requests per minute by 14:42 — the operator's own retries were
// most of the load by the end. 242 reconcile errors were recorded for a single
// outage that resolved on its own.
//
// The discriminator between "the provider is down" and "one Bucket is broken" is
// deliberately not a classification of the error. It is the absence of any
// successful reconcile: a single broken Bucket among healthy ones is interleaved
// with successes on every drift resync, which resets the counter, while a
// fleet-wide outage produces an unbroken run of failures. That keeps the breaker
// out of the business of parsing provider errors, which the operator already
// declines to do elsewhere (see BucketReconciler.degrade).
//
// While open, reconciles return a RequeueAfter and no error. The failures are
// still logged, still raise Events and still land in status.message; they simply
// stop being counted once per retry, so controller_runtime_reconcile_errors_total
// measures outages rather than retry volume.
//
// A zero threshold disables the breaker entirely, which makes it a values-only
// rollback that needs no new image.
type ProviderBreaker struct {
	mu sync.Mutex

	threshold   int
	maxCooldown time.Duration

	// now is a test seam; nil means time.Now.
	now func() time.Time

	consecutive int
	// cooldown is the wait applied by the next trip; it doubles per failed
	// probe and resets on success.
	cooldown time.Duration
	// openUntil is when the next probe may run. Zero means the breaker is closed.
	openUntil time.Time
	// openedAt is when the current open episode began. It survives the doubling
	// of cooldown across probes, so it measures the outage rather than the last
	// probe interval.
	openedAt time.Time
}

// NewProviderBreaker builds a breaker. A threshold below 1 disables it; a
// non-positive maxCooldown falls back to the default cap, and any cap below the
// base cooldown is raised to it so the two cannot be configured inside out.
func NewProviderBreaker(threshold int, maxCooldown time.Duration) *ProviderBreaker {
	if maxCooldown <= 0 {
		maxCooldown = DefaultCircuitMaxCooldown
	}
	if maxCooldown < circuitBaseCooldown {
		maxCooldown = circuitBaseCooldown
	}
	return &ProviderBreaker{threshold: threshold, maxCooldown: maxCooldown}
}

func (b *ProviderBreaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// enabled reports whether the breaker can ever open. A nil receiver counts as
// disabled so callers need no nil check.
func (b *ProviderBreaker) enabled() bool { return b != nil && b.threshold >= 1 }

// Allow reports whether a reconcile may talk to the provider now. When it may
// not, wait is how long until the next probe is due — the caller requeues after
// exactly that, so the breaker and not the workqueue decides the retry cadence.
//
// Once the cooldown has elapsed the breaker admits reconciles again without
// closing: the first one through is the probe, and its outcome (Success or
// Failure) decides whether the breaker closes or re-opens with a longer
// cooldown. The bucket controller reconciles one item at a time, so in practice
// exactly one probe runs per cooldown.
func (b *ProviderBreaker) Allow() (wait time.Duration, allowed bool) {
	if !b.enabled() {
		return 0, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return 0, true
	}
	remaining := b.openUntil.Sub(b.clock())
	if remaining <= 0 {
		return 0, true
	}
	return remaining, false
}

// Failure records a reconcile that failed for a non-definitive reason and
// reports the state afterwards. When open is true the caller must requeue after
// wait instead of returning the error, so the retry cadence stays with the
// breaker.
func (b *ProviderBreaker) Failure() (wait time.Duration, open bool) {
	if !b.enabled() {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutive++
	if b.consecutive < b.threshold {
		return 0, false
	}

	now := b.clock()
	switch {
	case b.openUntil.IsZero():
		// First trip of this episode.
		b.cooldown = circuitBaseCooldown
		b.openedAt = now
	case now.Before(b.openUntil):
		// Already shut and not due for a probe; leave the schedule alone so a
		// failure racing the cooldown cannot extend it.
		return b.openUntil.Sub(now), true
	default:
		// A probe was allowed through and failed: the provider is still down.
		b.cooldown *= 2
	}
	if b.cooldown > b.maxCooldown {
		b.cooldown = b.maxCooldown
	}
	b.openUntil = now.Add(b.cooldown)
	return b.cooldown, true
}

// Success records a completed reconcile, which closes the breaker: the provider
// answered, so whatever failed before was not a fleet-wide outage (or it is over).
func (b *ProviderBreaker) Success() {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.cooldown = 0
	b.openUntil = time.Time{}
	b.openedAt = time.Time{}
}

// OpenedAt reports when the current open episode began. It is the age of the
// outage as the operator sees it, and stays put while the cooldown doubles.
func (b *ProviderBreaker) OpenedAt() (time.Time, bool) {
	if !b.enabled() {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openedAt.IsZero() {
		return time.Time{}, false
	}
	return b.openedAt, true
}
