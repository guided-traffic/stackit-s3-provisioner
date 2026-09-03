package controller

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock so the breaker's cooldowns are tested
// exactly rather than slept through.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return newFakeClockAt(time.Date(2026, 9, 2, 14, 23, 0, 0, time.UTC))
}

// newFakeClockAt seeds the clock at a chosen instant. Tests that mix the
// breaker's clock with wall-clock state (a Bucket's status.degradedSince) seed
// it at time.Now() so the two stay comparable.
func newFakeClockAt(t time.Time) *fakeClock {
	return &fakeClock{t: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestBreaker builds a breaker on a fake clock.
func newTestBreaker(t *testing.T, threshold int, maxCooldown time.Duration) (*ProviderBreaker, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	b := NewProviderBreaker(threshold, maxCooldown)
	b.now = clk.Now
	return b, clk
}

// TestBreakerOpensOnlyAtThreshold pins that failures below the threshold leave
// the breaker closed and the error on the caller's normal path.
func TestBreakerOpensOnlyAtThreshold(t *testing.T) {
	b, _ := newTestBreaker(t, 3, time.Minute)

	for i := 1; i < 3; i++ {
		if _, open := b.Failure(); open {
			t.Fatalf("failure %d opened the breaker; want closed until the threshold", i)
		}
		if _, allowed := b.Allow(); !allowed {
			t.Fatalf("after failure %d reconciles must still be allowed", i)
		}
	}

	wait, open := b.Failure()
	if !open {
		t.Fatal("third consecutive failure must open the breaker")
	}
	if wait != circuitBaseCooldown {
		t.Errorf("wait = %v, want the base cooldown %v", wait, circuitBaseCooldown)
	}
	if _, allowed := b.Allow(); allowed {
		t.Error("an open breaker must not allow reconciles")
	}
}

// TestBreakerSuccessResetsTheRun is the discriminator between "the provider is
// down" and "one Bucket is broken": a single failing Bucket among healthy ones
// has its failures interleaved with successes, so the run never reaches the
// threshold and the fleet keeps reconciling.
func TestBreakerSuccessResetsTheRun(t *testing.T) {
	b, _ := newTestBreaker(t, 3, time.Minute)

	for range 10 {
		b.Failure() // the one broken Bucket
		b.Success() // some other Bucket reconciled fine
		b.Failure()
		b.Success()
	}
	if _, allowed := b.Allow(); !allowed {
		t.Fatal("interleaved failures must never open the breaker")
	}
	if _, open := b.OpenedAt(); open {
		t.Error("breaker reports an open episode that never happened")
	}
}

// TestBreakerProbeAndBackoff walks a whole outage: trip, wait, probe, fail,
// longer wait — and pins that the cooldown doubles up to the cap while the
// episode's start time stays put.
func TestBreakerProbeAndBackoff(t *testing.T) {
	b, clk := newTestBreaker(t, 1, 2*time.Minute)

	wait, open := b.Failure()
	if !open || wait != circuitBaseCooldown {
		t.Fatalf("first trip: wait = %v, open = %v; want %v, true", wait, open, circuitBaseCooldown)
	}
	openedAt, ok := b.OpenedAt()
	if !ok {
		t.Fatal("OpenedAt must report the episode once open")
	}

	for _, want := range []time.Duration{2 * time.Minute, 2 * time.Minute, 2 * time.Minute} {
		// Still shut just before the cooldown elapses.
		clk.Advance(wait - time.Second)
		if remaining, allowed := b.Allow(); allowed {
			t.Fatalf("breaker opened early (remaining %v)", remaining)
		}
		// The probe is due; exactly one reconcile gets through.
		clk.Advance(time.Second)
		if _, allowed := b.Allow(); !allowed {
			t.Fatal("breaker must admit a probe once the cooldown elapsed")
		}
		// The probe fails: the provider is still down.
		wait, open = b.Failure()
		if !open {
			t.Fatal("a failed probe must re-open the breaker")
		}
		if wait != want {
			t.Errorf("cooldown = %v, want %v", wait, want)
		}
	}

	if got, _ := b.OpenedAt(); !got.Equal(openedAt) {
		t.Errorf("OpenedAt moved to %v; it must measure the outage, not the last probe", got)
	}
}

// TestBreakerClosesOnSuccessfulProbe pins recovery: the first reconcile that
// completes ends the episode and restores the base cooldown for the next one.
func TestBreakerClosesOnSuccessfulProbe(t *testing.T) {
	b, clk := newTestBreaker(t, 1, time.Hour)

	b.Failure()
	clk.Advance(circuitBaseCooldown)
	b.Failure() // failed probe, cooldown doubled
	clk.Advance(2 * time.Minute)

	b.Success()
	if _, allowed := b.Allow(); !allowed {
		t.Fatal("a successful probe must close the breaker")
	}
	if _, open := b.OpenedAt(); open {
		t.Error("a closed breaker must report no open episode")
	}
	if wait, _ := b.Failure(); wait != circuitBaseCooldown {
		t.Errorf("cooldown after recovery = %v, want the base %v", wait, circuitBaseCooldown)
	}
}

// TestBreakerFailureDoesNotExtendAnOpenWindow guards the case where several
// Buckets fail into an already-open breaker: the probe schedule must stay where
// it is, or a busy fleet could push the next probe out indefinitely.
func TestBreakerFailureDoesNotExtendAnOpenWindow(t *testing.T) {
	b, clk := newTestBreaker(t, 1, time.Hour)

	b.Failure()
	clk.Advance(20 * time.Second)
	for range 20 {
		if wait, open := b.Failure(); !open || wait != 40*time.Second {
			t.Fatalf("wait = %v, open = %v; want the remaining 40s of the original window", wait, open)
		}
	}
	clk.Advance(40 * time.Second)
	if _, allowed := b.Allow(); !allowed {
		t.Error("the probe must still be due at the original time")
	}
}

// TestBreakerDisabled covers the values-only rollback: threshold 0 (and a nil
// breaker) must behave exactly like no breaker at all.
func TestBreakerDisabled(t *testing.T) {
	for name, b := range map[string]*ProviderBreaker{
		"threshold zero": NewProviderBreaker(0, time.Minute),
		"nil":            nil,
	} {
		t.Run(name, func(t *testing.T) {
			for range 100 {
				if wait, open := b.Failure(); open || wait != 0 {
					t.Fatal("a disabled breaker must never open")
				}
			}
			if wait, allowed := b.Allow(); !allowed || wait != 0 {
				t.Error("a disabled breaker must always allow")
			}
			if _, open := b.OpenedAt(); open {
				t.Error("a disabled breaker must report no episode")
			}
			b.Success() // must not panic on a nil receiver
		})
	}
}

// TestNewProviderBreakerClampsCooldown pins that the cap cannot be configured
// below the base cooldown, which would make the doubling meaningless.
func TestNewProviderBreakerClampsCooldown(t *testing.T) {
	if got := NewProviderBreaker(1, 0).maxCooldown; got != DefaultCircuitMaxCooldown {
		t.Errorf("maxCooldown = %v, want the default %v", got, DefaultCircuitMaxCooldown)
	}
	if got := NewProviderBreaker(1, time.Second).maxCooldown; got != circuitBaseCooldown {
		t.Errorf("maxCooldown = %v, want it raised to the base %v", got, circuitBaseCooldown)
	}
}
