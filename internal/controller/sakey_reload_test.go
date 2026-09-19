package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

const (
	saKeyCountSeries      = "stackit_s3_provisioner_sa_key_reload_total"
	saKeyFailingSeries    = "stackit_s3_provisioner_sa_key_reload_failing"
	saKeyValidUntilSeries = "stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds"
	saKeyLoadedSeries     = "stackit_s3_provisioner_sa_key_loaded_timestamp_seconds"
)

func timePtr(t time.Time) *time.Time { return &t }

func newTestObserver(breaker *ProviderBreaker, acc stackit.Account) *SAKeyReloadObserver {
	return NewSAKeyReloadObserver(logr.Discard(), breaker, acc)
}

// TestSAKeyReloadCounters pins that all three result series exist from the
// first scrape, so an alert expression never races an absent one.
func TestSAKeyReloadCounters(t *testing.T) {
	o := newTestObserver(nil, stackit.Account{ProjectID: "p"})

	fresh := `
# HELP stackit_s3_provisioner_sa_key_reload_total Total number of service-account key polls, by what the poll did.
# TYPE stackit_s3_provisioner_sa_key_reload_total counter
stackit_s3_provisioner_sa_key_reload_total{result="applied"} 0
stackit_s3_provisioner_sa_key_reload_total{result="rejected"} 0
stackit_s3_provisioner_sa_key_reload_total{result="unchanged"} 0
`
	if err := testutil.CollectAndCompare(o, strings.NewReader(fresh), saKeyCountSeries); err != nil {
		t.Fatalf("before any poll: %v", err)
	}

	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadUnchanged})
	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadUnchanged})
	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadRejected, Err: errors.New("nope")})
	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadApplied, Account: stackit.Account{ProjectID: "p"}})

	counted := `
# HELP stackit_s3_provisioner_sa_key_reload_total Total number of service-account key polls, by what the poll did.
# TYPE stackit_s3_provisioner_sa_key_reload_total counter
stackit_s3_provisioner_sa_key_reload_total{result="applied"} 1
stackit_s3_provisioner_sa_key_reload_total{result="rejected"} 1
stackit_s3_provisioner_sa_key_reload_total{result="unchanged"} 2
`
	if err := testutil.CollectAndCompare(o, strings.NewReader(counted), saKeyCountSeries); err != nil {
		t.Fatalf("after four polls: %v", err)
	}
}

// TestSAKeyReloadFailingGauge pins the series a rejection alarms on. It is the
// only thing that distinguishes an operator whose rotation is being refused
// from a healthy one — both keep working, right up to the old key's expiry.
func TestSAKeyReloadFailingGauge(t *testing.T) {
	o := newTestObserver(nil, stackit.Account{ProjectID: "p"})

	gauge := func(want string) string {
		return `
# HELP stackit_s3_provisioner_sa_key_reload_failing 1 while the operator keeps rejecting the service-account key on disk and carries on with the key it already holds.
# TYPE stackit_s3_provisioner_sa_key_reload_failing gauge
stackit_s3_provisioner_sa_key_reload_failing ` + want + "\n"
	}

	if err := testutil.CollectAndCompare(o, strings.NewReader(gauge("0")), saKeyFailingSeries); err != nil {
		t.Fatalf("before any poll: %v", err)
	}

	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadRejected, Err: errors.New("nope")})
	if err := testutil.CollectAndCompare(o, strings.NewReader(gauge("1")), saKeyFailingSeries); err != nil {
		t.Fatalf("after a rejection: %v", err)
	}

	// Reverting the file to the key already in use ends the rejection just as
	// a successful swap does.
	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadUnchanged})
	if err := testutil.CollectAndCompare(o, strings.NewReader(gauge("0")), saKeyFailingSeries); err != nil {
		t.Fatalf("after the file went back to the loaded key: %v", err)
	}

	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadRejected, Err: errors.New("nope")})
	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadApplied, Account: stackit.Account{ProjectID: "p"}})
	if err := testutil.CollectAndCompare(o, strings.NewReader(gauge("0")), saKeyFailingSeries); err != nil {
		t.Fatalf("after a successful swap: %v", err)
	}
}

// TestSAKeyValidUntilGaugeIsAbsentWithoutAnExpiry pins the absent case as a
// supported path rather than a fallback: neither e2e key file carries
// validUntil, so this is the shape the suites actually run on, and an expiry
// alert must not fire on that silence.
func TestSAKeyValidUntilGaugeIsAbsentWithoutAnExpiry(t *testing.T) {
	o := newTestObserver(nil, stackit.Account{ProjectID: "p"})

	if err := testutil.CollectAndCompare(o, strings.NewReader(""), saKeyValidUntilSeries); err != nil {
		t.Fatalf("a key without validUntil must export no expiry series: %v", err)
	}

	expiry := time.Unix(1800000000, 0).UTC()
	o.Observe(stackit.ReloadResult{
		Outcome: stackit.ReloadApplied,
		Account: stackit.Account{ProjectID: "p", ValidUntil: timePtr(expiry)},
	})
	present := `
# HELP stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds Unix time at which the service-account key currently in use expires; absent when the key file carries no validUntil.
# TYPE stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds gauge
stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds 1.8e+09
`
	if err := testutil.CollectAndCompare(o, strings.NewReader(present), saKeyValidUntilSeries); err != nil {
		t.Fatalf("after loading a key with validUntil: %v", err)
	}

	// Rotating to a key without an expiry must take the series away again,
	// rather than leaving the previous key's expiry standing.
	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadApplied, Account: stackit.Account{ProjectID: "p"}})
	if err := testutil.CollectAndCompare(o, strings.NewReader(""), saKeyValidUntilSeries); err != nil {
		t.Fatalf("after rotating to a key without validUntil: %v", err)
	}
}

// TestSAKeyLoadedGaugeStartsAtStartup pins that the series answers "is this pod
// running a key from before the rotation" from the first scrape, not only after
// the first reload.
func TestSAKeyLoadedGaugeStartsAtStartup(t *testing.T) {
	before := time.Now().Unix()
	o := newTestObserver(nil, stackit.Account{ProjectID: "p"})
	after := time.Now().Unix()

	if got := o.loadedAt.Load(); got < before || got > after {
		t.Errorf("loaded-at = %d, want a timestamp between %d and %d", got, before, after)
	}

	// And it is exported as an absolute Unix time, so the remaining lifetime
	// is computed in the alerting rule rather than by the operator.
	o.adopt(stackit.Account{ProjectID: "p"}, time.Unix(1700000000, 0))
	exported := `
# HELP stackit_s3_provisioner_sa_key_loaded_timestamp_seconds Unix time at which the service-account key currently in use was loaded.
# TYPE stackit_s3_provisioner_sa_key_loaded_timestamp_seconds gauge
stackit_s3_provisioner_sa_key_loaded_timestamp_seconds 1.7e+09
`
	if err := testutil.CollectAndCompare(o, strings.NewReader(exported), saKeyLoadedSeries); err != nil {
		t.Fatalf("exported loaded-at: %v", err)
	}
}

// TestSAKeyReloadResetsTheBreaker pins the half of ADR 0016 D6 that costs
// nothing to get wrong and minutes to get right: a validated swap has already
// made a successful authenticated call, so the fleet must not sit out the
// circuit cooldown after the fix has landed.
func TestSAKeyReloadResetsTheBreaker(t *testing.T) {
	breaker := NewProviderBreaker(1, time.Minute)
	o := newTestObserver(breaker, stackit.Account{ProjectID: "p"})

	breaker.Failure()
	if _, open := breaker.OpenedAt(); !open {
		t.Fatalf("breaker did not open, cannot test the reset")
	}

	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadApplied, Account: stackit.Account{ProjectID: "p"}})

	if _, open := breaker.OpenedAt(); open {
		t.Errorf("breaker still open after a validated swap, want closed")
	}
	if _, allowed := breaker.Allow(); !allowed {
		t.Errorf("breaker still holding reconciles after a validated swap")
	}
}

// TestSAKeyRejectionNeverTouchesTheBreaker pins the other half: the candidate
// is on trial, the provider is not.
func TestSAKeyRejectionNeverTouchesTheBreaker(t *testing.T) {
	breaker := NewProviderBreaker(1, time.Minute)
	o := newTestObserver(breaker, stackit.Account{ProjectID: "p"})

	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadRejected, Err: errors.New("nope"), Definitive: true})

	if _, open := breaker.OpenedAt(); open {
		t.Errorf("a rejected candidate opened the breaker, want it untouched")
	}
}

// TestSAKeyObserverIsSafeWithoutABreaker covers skeleton-adjacent wiring: the
// breaker is nil when the circuit is disabled, and a nil receiver must stay a
// no-op rather than panicking the poll goroutine.
func TestSAKeyObserverIsSafeWithoutABreaker(t *testing.T) {
	o := newTestObserver(nil, stackit.Account{ProjectID: "p"})
	o.Observe(stackit.ReloadResult{Outcome: stackit.ReloadApplied, Account: stackit.Account{ProjectID: "p"}})
}

func TestValidUntilForLog(t *testing.T) {
	if got := validUntilForLog(stackit.Account{}); got != "none" {
		t.Errorf("validUntilForLog(no expiry) = %q, want %q", got, "none")
	}
	expiry := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	if got := validUntilForLog(stackit.Account{ValidUntil: &expiry}); got != "2027-03-01T12:00:00Z" {
		t.Errorf("validUntilForLog = %q, want the RFC 3339 expiry", got)
	}
}
