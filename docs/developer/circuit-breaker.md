# Circuit breaker

This page describes the four mechanisms that stop the operator from amplifying a provider outage:
the fleet-wide breaker in [`internal/controller/breaker.go`](../../internal/controller/breaker.go),
the workqueue rate limiter in [`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go),
the two transport changes in [`stackit/retry.go`](../../stackit/retry.go), and the two circuit
metrics in [`internal/controller/metrics.go`](../../internal/controller/metrics.go). The decision
behind them, with the 2026-09-02 incident record that forced it, is
[ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) — this page never restates a rule,
it explains the code that implements one. If you want how a single provider error is recognised and
classified *before* it ever reaches the breaker, that is
[provider-errors.md](provider-errors.md). If you want what an on-call engineer sees and tunes during
an outage, that is [../operations/provider-outages.md](../operations/provider-outages.md).

## The breaker object

`ProviderBreaker` in [`internal/controller/breaker.go`](../../internal/controller/breaker.go) is a
mutex-guarded struct with four exported methods and no goroutine of its own. It has no knowledge of
buckets, errors or HTTP — it counts reconcile outcomes and hands back a wait.

| Method | Called by | What it does |
| --- | --- | --- |
| `Allow() (wait, allowed)` | `Reconcile`, `reconcileDelete` | Reports whether a reconcile may touch the provider now, and if not, how long until the next probe is due |
| `Failure() (wait, open)` | `fail` | Records one non-definitive failure and reports the state afterwards |
| `Success()` | `reconcileNormal` (terminal Ready write), `reconcileDelete` (completed teardown), `SAKeyReloadObserver.Observe` (a validated key swap) | Resets everything — run counter, cooldown, open window, opened-at |
| `OpenedAt() (time, bool)` | the metrics collector | When the current open episode began |

Its state is four fields:

| Field | Meaning |
| --- | --- |
| `consecutive` | Failures since the last `Success()` |
| `cooldown` | The wait the *next* trip applies; doubles per failed probe, reset by `Success()` |
| `openUntil` | When the next probe may run. Zero means closed — this, not a boolean, is the open flag |
| `openedAt` | Start of the open episode. Deliberately *not* touched when a probe fails, so it measures the outage and not the last probe interval |

The `now func() time.Time` field is a test seam; `clock()` falls back to `time.Now` when it is nil.
All the offline tests drive a fake clock through it rather than sleeping.

### Counting, and the off-by-one that follows from it

`Failure()` increments `consecutive` and returns `open=false` only while the counter is **strictly
below** the threshold. The reconcile that *reaches* the threshold already returns `open=true`. That
single `<` is the whole reason an outage costs one reconcile error fewer than the threshold — two at
the default of `3` ([ADR 0013 D5](../adr/0013-a-provider-outage-is-held-fleet-wide.md)):

```go
	b.consecutive++
	if b.consecutive < b.threshold {
		return 0, false
	}
```

`fail()` returns the error to the workqueue only while `open` is false. From the threshold reconcile
onward — including every failed probe — it returns `ctrl.Result{RequeueAfter: wait}, nil`, so
`controller_runtime_reconcile_errors_total` stops advancing.

Two tests pin the two halves of that, and it is worth knowing which is which.
`TestBreakerOpensOnlyAtThreshold` in
[`internal/controller/breaker_test.go`](../../internal/controller/breaker_test.go) drives `Failure()`
and `Allow()` directly and pins only the breaker's own `open` flag — it knows nothing about
reconciles or returned errors. The reconciler-visible consequence is pinned by `tripCircuit` in
[`internal/controller/reconciler_circuit_test.go`](../../internal/controller/reconciler_circuit_test.go):
the first `threshold-1` reconciles must return an error, and the one that trips must return none and
a `RequeueAfter` of exactly `circuitBaseCooldown`.

### The probe schedule

The `switch` inside `Failure()` has exactly three cases, and each exists for a reason:

| Case | Condition | Effect |
| --- | --- | --- |
| First trip | `openUntil.IsZero()` | `cooldown = circuitBaseCooldown`, `openedAt = now` |
| Failure racing an open window | `now.Before(openUntil)` | Returns the remaining wait and changes nothing — a late failure arriving while the breaker is already shut cannot push the probe further out |
| Failed probe | otherwise | `cooldown *= 2`, then clamped to `maxCooldown` |

`Allow()` does **not** close the breaker when the cooldown elapses; it simply starts returning
`allowed=true` again while `openUntil` stays set. The first reconcile through is the probe, and its
own `Success()` or `Failure()` decides what happens next. The bucket controller sets no
`MaxConcurrentReconciles`, so it runs one item at a time and in practice exactly one probe runs per
cooldown — verified by reading [`SetupWithManager`](../../internal/controller/bucket_controller.go),
which passes only `RateLimiter` in `controller.Options`.

### Tuning and what disables it

| Constant / field | Value | Where |
| --- | --- | --- |
| `DefaultCircuitThreshold` | `3` `# default` | [`breaker.go`](../../internal/controller/breaker.go) |
| `DefaultCircuitMaxCooldown` | `5 * time.Minute` `# default` | [`breaker.go`](../../internal/controller/breaker.go) |
| `circuitBaseCooldown` | `time.Minute`, unexported, not configurable | [`breaker.go`](../../internal/controller/breaker.go) |
| `--provider-circuit-threshold` / `PROVIDER_CIRCUIT_THRESHOLD` | defaults to `DefaultCircuitThreshold` | [`cmd/main.go`](../../cmd/main.go) |
| `--provider-circuit-max-cooldown` / `PROVIDER_CIRCUIT_MAX_COOLDOWN` | defaults to `DefaultCircuitMaxCooldown` | [`cmd/main.go`](../../cmd/main.go) |

`NewProviderBreaker` normalises its two arguments so they cannot be configured inside out: a
non-positive `maxCooldown` falls back to `DefaultCircuitMaxCooldown`, and any cap *below*
`circuitBaseCooldown` is raised to the base. `TestNewProviderBreakerClampsCooldown` covers both.

The base is a minute rather than something shorter because the open state has to be **observable**,
not merely effective: the chart's `monitoring.serviceMonitor.interval` is 30s and the
`StackitS3ReconcileErrors` rule suppresses windows in which the circuit gauge was non-zero, so a trip
must outlive a scrape gap to be seen at all. The source comment on the constant says so; lowering it
silently weakens the alert suppression rather than the hold.

`enabled()` returns false for a nil receiver and for `threshold < 1`. A disabled breaker is inert in
the **safe** direction, not the zero-value one: `Allow()` returns `(0, true)` and therefore admits
every reconcile, while `Failure()` and `OpenedAt()` report a closed breaker and `Success()` is a
no-op. Two consequences worth knowing before you touch this: a reconciler
built with `Breaker: nil` (as most unit tests are) needs no nil checks, and setting the threshold to
`0` is a values-only rollback that needs no different image
([ADR 0013 D8](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).

## Where the reconciler calls it

The gate sits at the top of the pass, not around individual provider calls. `Reconcile` in
[`bucket_controller.go`](../../internal/controller/bucket_controller.go) runs, in this order:

1. `Get` the object (Kubernetes only).
2. Deletion branch → `reconcileDelete`, which carries **its own** `Allow()` gate.
3. Add the finalizer if missing (Kubernetes only).
4. Skeleton-mode branch (`r.Stackit == nil`) — no provider work exists to hold.
5. `r.Breaker.Allow()`; if not allowed → `holdForProvider`.
6. `reconcileNormal`.

Everything a *reconcile* does against the provider lives behind step 5 or inside `reconcileDelete`,
so one gate per pass is sufficient and nothing downstream re-checks. There is exactly one provider
call in the process that does not pass a gate at all — the service-account key validation, on the
reload goroutine — and it is
[the deliberate exception below](#the-one-provider-call-that-ignores-allow). Two consequences of
the ordering above are easy to miss:

- **A `Bucket` created during an outage still gets its finalizer**, because step 3 precedes the gate
  and only touches the Kubernetes API.
- **A never-provisioned `Bucket` drops to `Failed` immediately.** `holdForProvider` calls `degrade`,
  `holdsReadyThrough` requires `phase == Ready` with `ObservedGeneration == Generation`, a fresh CR
  has neither, so it falls through to `markFailed`. The degradation grace protects a *verified*
  state; a CR that never had one has nothing to hold.

### `holdForProvider` and the silence rule

`holdForProvider` returns `ctrl.Result{RequeueAfter: wait}, nil` — never an error. It writes status
only when `providerHoldNeedsWrite` says the record would actually change:

| Object state | `providerHoldNeedsWrite` | What happens |
| --- | --- | --- |
| `phase == Failed` | false | Silent requeue; the hold has nothing to add |
| `status.degradedSince == nil` | true | First hold of this outage → `degrade()` records it, or `markFailed` if the CR was never Ready |
| Degraded, grace not elapsed | false | Silent requeue |
| Degraded, `ProviderDegradedGrace > 0` and elapsed | true | `degrade()` declines, `markFailed` drops the bucket to `Failed` |

The `Warning` event with reason `Failed` comes from `degrade()` and `markFailed()`, which are reached
only on those two visits. Every other held reconcile therefore emits **no event, no status write and
no log line at the default verbosity**. That is what turns an hours-long outage into a handful of
writes; `TestProviderCircuitHoldsReadyWithoutChurn` pins it.

The error text recorded on the object is `errProviderUnavailable`, wrapped with the remaining wait:
`StackIT API unavailable (provider circuit open); next provider probe in 1m0s` `# example`.

### Teardown

`reconcileDelete` gates separately because `Reconcile` routes deletions before the main gate. Two
details matter when changing it:

- The phase write to `Deleting` happens **before** the `Allow()` check, and only when the phase is
  neither `Deleting` nor `Failed`. The condition exists so a blocked delete does not flip-flop
  `Deleting` ↔ `Failed` and self-trigger reconciles through the status watch. It also means the hold
  is not quite as phase-neutral as
  [ADR 0013 D4](../adr/0013-a-provider-outage-is-held-fleet-wide.md) states: on the *first* reconcile
  after a deletion starts during an open circuit, a `Ready` bucket is written to `Deleting` and only
  then does the code consult the breaker. Every hold after that writes nothing, which is the property
  D4 is really about. Verified by reading `reconcileDelete` in
  [`bucket_controller.go`](../../internal/controller/bucket_controller.go); see
  [What is wrong today](#what-is-wrong-today).
- The held path emits `logger.V(1).Info("provider circuit open; deferring teardown", ...)` and
  nothing else — no event, no `status.message`. A delete blocked by an outage is therefore
  indistinguishable *on the object* from a delete blocked by its own cause
  ([ADR 0013 D4](../adr/0013-a-provider-outage-is-held-fleet-wide.md)); the log and the circuit
  metrics carry the difference.

`Breaker.Success()` is called on a completed teardown, before `dropFinalizer()`. A teardown that
answers is proof the provider is up, exactly like a provisioning pass.
`TestProviderCircuitDefersTeardown` asserts zero `DeleteBucket` calls while open, a retained
finalizer, and completion on the first probe after recovery.

### The three `Success()` call sites

Two of them sit on a path where the provider answered every call of a reconcile pass: the terminal
status write in `reconcileNormal` (right after `clearDegraded` and the `Ready` condition) and the
completed teardown in `reconcileDelete`. One early return bypasses both: a clone still running makes
`provisionCredentialsAndClone` return `done=false`, and `reconcileNormal` hands that result straight
back without reaching the terminal write. It calls neither `Success()` nor `Failure()`, so it is
neutral — the run is neither advanced nor reset.

The third sits **outside the reconcile loop entirely**, on the key-reload goroutine:
`SAKeyReloadObserver.Observe` in
[`sakey_reload.go`](../../internal/controller/sakey_reload.go) calls `Breaker.Success()` on a
`ReloadApplied` outcome. That is sound for the same reason the other two are, and for nothing to do
with keys: a candidate key only ever reaches `ReloadApplied` after `(*Client).reload` has made one
live authenticated `GetServiceStatus` call with the candidate client and got an answer
([`stackit/keyreload.go`](../../stackit/keyreload.go)). A successful authenticated call is the exact
evidence `Success()` waits for — the breaker's discriminator is the presence of a success, not who
produced it or on whose behalf ([ADR 0013 D2](../adr/0013-a-provider-outage-is-held-fleet-wide.md),
[ADR 0016 D6](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)). So the
reset assumes nothing it has not just measured, and it is what makes the fleet recover on the
reload rather than on the next cooldown probe.

The asymmetry is deliberate: a **failed** validation never calls `Failure()`. The candidate is on
trial, the provider is not, and counting a rejected key toward a provider outage would trip the
breaker on a truncated file. `TestSAKeyReloadResetsTheBreaker` and
`TestSAKeyRejectionNeverTouchesTheBreaker` in
[`sakey_reload_test.go`](../../internal/controller/sakey_reload_test.go) pin both halves; the
observer is nil-safe, so a reconciler built with `Breaker: nil` needs no guard here either.

An unresolvable read grant is **not** such an early return, and the difference matters if you are
reasoning about what closes the breaker. `resolveReadGrants` `continue`s past a grantee it cannot
resolve — absent, being deleted, without a bucket or a group yet — emits `ReadGrantPending` and lets
the pass run to the terminal write, which does call `Success()`. That is the right outcome: the
provider answered every call of that pass, so it is up, and the grant is simply not yet grantable.

### The one provider call that ignores `Allow()`

The key validation does not consult the breaker at all. It is not routed through `Reconcile`, it
holds no reference to the breaker on the way in — `(*Client).reload` is in package `stackit`, which
imports nothing above it — and `KeyReloader.tick` runs on its own ticker whatever the circuit is
doing. That is an exception to
[ADR 0013 D4](../adr/0013-a-provider-outage-is-held-fleet-wide.md) ("while the breaker is open the
operator makes no provider call at all"), written into the record by
[ADR 0016 D6](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md).

Obeying `Allow()` here would be wrong, not merely inconvenient. The scenario the whole reload exists
for is a revoked key: the breaker is then open **because** the dead key is producing
`400 invalid_grant` on every reconcile, so a validation that waits for the breaker waits for a
recovery that cannot happen until the validation succeeds. The cost is up to
`--provider-circuit-max-cooldown` (`5m` `# default`) of extra outage, spent while reconciles keep
probing with the key that is known dead and doubling the cooldown further.

The exception is bounded by the shape of the caller rather than by a gate:

| Bound | Where it comes from |
| --- | --- |
| At most one validation call per **distinct file content** | `tick` compares the file's SHA-256 against the hash of the loaded key and short-circuits on a match, so an unchanged file costs a `read(2)` and nothing else |
| At most one per poll interval | the single `time.Ticker` in `KeyReloader.Start`, `--stackit-sa-key-reload-interval` (`30s` `# default`) |
| A definitively rejected candidate is retried on a doubling schedule, capped at `10m` | `reloadRejectBackoffMax` and the `waitIntervals`/`ticksLeft` bookkeeping in `rejected` ([ADR 0016 D7](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)) |
| The call cannot wedge the loop, and cannot hold shutdown | `reloadProbeTimeout` (`90s`) wraps the probe context, and `reload` selects on the caller's context so cancellation returns at once. The budget is above the SDK's own one-minute auth timeout on purpose: the key flow's token POST carries no context at all, so a shorter one would expire inside the token fetch and fail a good key ([service-account-key-reload.md](service-account-key-reload.md)) |

The worst case is therefore one token request every ten minutes for a key the provider will never
accept, which is nothing like the traffic the breaker exists to suppress — and it happens in a state
that is already alarming through `StackitS3SaKeyReloadFailing`. The mechanism itself is
[service-account-key-reload.md](service-account-key-reload.md).

## The fleet-wide workqueue rate limiter

`bucketRateLimiter()` in [`bucket_controller.go`](../../internal/controller/bucket_controller.go) is
passed to `controller.Options{RateLimiter: ...}` in `SetupWithManager`. It is a
`NewTypedMaxOfRateLimiter` over two limiters, so the *slower* of the two wins for every item:

| Limiter | Values | Replaces the controller-runtime default of |
| --- | --- | --- |
| `TypedItemExponentialFailureRateLimiter` | base `time.Second`, cap `15 * time.Minute` | base 5ms, cap 1000s |
| `TypedBucketRateLimiter` (`rate.NewLimiter`) | 1 qps, burst 5 | 10 qps, burst 100 |

The defaults are sized for a controller whose reconcile makes a few calls against the local API
server. A `Bucket` reconcile makes several calls against a rate-limited remote API, which is how a
provider-side 503 storm on 2026-09-02 became `rate limit on IP level exceeded` eleven minutes later
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) carries the timeline).

**Only error requeues pass through this limiter.** Watch events enter the queue via `Add` and a
`RequeueAfter` via `AddAfter`, neither of which is rate limited — so the drift resync
(`--drift-resync-interval`, `10m` `# default`), the breaker's own probe requeue and every
event-driven reconcile keep their timing. This is why cutting the fleet allowance to 1 qps does not
slow normal operation; it only slows a fleet that is failing.

The measurement controller (`BucketUsageReconciler`) sets no `RateLimiter` and runs on the
controller-runtime default — see [What is wrong today](#what-is-wrong-today).

## Transport changes

Both live in [`stackit/retry.go`](../../stackit/retry.go), installed by `newKeyFlowAPIClient` in
[`stackit/client.go`](../../stackit/client.go) — the constructor `NewClient` and every key reload
share — via `config.WithHTTPClient(&http.Client{Transport: newRetryTransport(…)})`. A **fresh**
transport per client is deliberate: `objectstorage.NewAPIClient` mutates the `http.Client` it is
handed, so sharing one would rewire a live client out from under the reconciler. The SDK takes that
client as its *inner* transport, under its own auth round-tripper, so the changes cover both API
requests and the key-flow token fetch.

**A rate-limited response is never retried.** `retryableResult` returns true for any transport error
and for `>= 500` only. Every 4xx is an answer, and `429` is the one answer where repeating the
request is guaranteed to make things worse. Before this, the transport multiplied every `GET` by
three throughout the outage. Backing off from a rate limit belongs to the workqueue and the breaker,
which see the whole fleet; a single in-flight request cannot.

**Idle pooled connections are closed after 30s.** `newBaseTransport()` clones
`http.DefaultTransport` (rather than mutating the shared instance, which would reach every other HTTP
user in the process) and sets `IdleConnTimeout = idleConnTimeout`, `30 * time.Second`, far below the
standard library's 90s. The operator talks to the API in bursts — one per drift resync — and is then
idle for minutes, so every burst after the first reused a connection the provider edge had already
dropped, surfacing as `read: connection reset by peer`. Closing first turns those into a fresh dial.
`TestNewRetryTransportDefaults` asserts both the clone and the constant. The source comment is
explicit that the edge's own idle timeout was never measured: 30s was chosen to sit *below* any
plausible value, not to match a known one.

The same pool is drained on demand by `retryTransport.CloseIdleConnections`, which a key reload
calls on the client it retires and on a candidate whose probe failed. It exists because the obvious
call does nothing: once `objectstorage.NewAPIClient` has replaced the `http.Client`'s `Transport`
with the key flow's round tripper, `http.Client.CloseIdleConnections` forwards to a transport that
does not implement the method, and the retired pool would sit idle for the full `idleConnTimeout`
while the code pretended otherwise.

The retry behaviour itself — 3 attempts, 200ms backoff tripling, GET/HEAD and bodyless requests only —
is [provider-errors.md](provider-errors.md)'s subject, not this page's.

## Interaction with the readiness grace

The breaker delays reconciles; it does not widen the window in which a bucket may claim an unverified
state ([ADR 0013 D7](../adr/0013-a-provider-outage-is-held-fleet-wide.md)). The mechanism is that
`degrade()` compares wall-clock time against `status.degradedSince`, which is written once and then
left alone:

```go
	now := metav1.Now()
	if b.Status.DegradedSince == nil {
		b.Status.DegradedSince = &now
	} else if now.Sub(b.Status.DegradedSince.Time) >= r.ProviderDegradedGrace {
		// The provider has been unreachable for longer than the operator is
		// willing to vouch for a state it can no longer verify. Hand back to
		// markFailed, keeping degradedSince so the status records when it began.
		return false
	}
```

Nothing in the breaker path resets that timestamp, and `providerHoldNeedsWrite` re-checks the same
elapsed comparison on every held pass, so a bucket held past `--provider-degraded-grace`
(`30m` `# default`) drops to `Failed` while the circuit is still open. The grace is owned by
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) and unchanged by the breaker.

`clearDegraded()` removes `status.degradedSince` and the `ProviderReachable` condition on the same
terminal path that calls `Breaker.Success()`, so recovery of the fleet and recovery of a single
bucket's advertised health happen in one write.

## The two metrics

Both are computed in `bucketMetricsCollector.Collect` in
[`internal/controller/metrics.go`](../../internal/controller/metrics.go) from
`breaker.OpenedAt()` on every scrape — there is no per-reconcile bookkeeping to go stale.

| Metric | Presence | Why |
| --- | --- | --- |
| `stackit_s3_provisioner_provider_circuit_open` | **Always** emitted, `0` while closed — including when the breaker is disabled, because `OpenedAt()` on a disabled breaker returns `(zero, false)` | An alert expression must never evaluate against a missing series |
| `stackit_s3_provisioner_provider_circuit_opened_timestamp_seconds` | Only while open | `time() - <series>` is then the age of the outage wherever the series exists |

The second one is a Unix timestamp taken from `openedAt`, which the probe path never touches, so it
measures the **outage** and not the last probe interval. If you ever make `Failure()` refresh
`openedAt`, this metric silently changes meaning.

The rules that consume them live in
[`deploy/helm/stackit-s3-provisioner/templates/prometheusrule.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/prometheusrule.yaml):
`StackitS3ReconcileErrors` appends `unless on() (max_over_time(stackit_s3_provisioner_provider_circuit_open[15m]) > 0)`
when `monitoring.prometheusRule.alerts.reconcileErrors.suppressWhileCircuitOpen` is true
(`true` `# default`), and `StackitS3BucketProviderDegraded` fires on
`max(time() - stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds) > holdForSeconds`.
The `unless` form is fail-open by construction: with the circuit series absent, it removes nothing.
There is deliberately no alert on the open circuit itself
([ADR 0013 D12](../adr/0013-a-provider-outage-is-held-fleet-wide.md)); the catalogue and the tuning
are [../operations/monitoring.md](../operations/monitoring.md)'s subject.

One coupling to keep in mind when touching either the collector or the rules: the per-bucket
`..._bucket_degraded_since_timestamp_seconds` series is emitted **only while `phase == Ready`**
(see the guard in `Collect`). Once the grace elapses the bucket goes to `Failed`, the series
disappears and `StackitS3BucketFailed` takes over — which is why `holdForSeconds` (`1200`
`# default`) must stay below `providerDegradedGrace`.

## Tests that pin this

<details>
<summary>The offline suite covering the breaker (no network, no cluster)</summary>

| Test | File | What it pins |
| --- | --- | --- |
| `TestBreakerOpensOnlyAtThreshold` | [`breaker_test.go`](../../internal/controller/breaker_test.go) | The strict `<`: `Failure()` reports closed below the threshold and `Allow()` keeps admitting |
| `TestBreakerSuccessResetsTheRun` | [`breaker_test.go`](../../internal/controller/breaker_test.go) | The discriminator — interleaved successes never let the run reach the threshold |
| `TestBreakerProbeAndBackoff` | [`breaker_test.go`](../../internal/controller/breaker_test.go) | The doubling cooldown and its clamp |
| `TestBreakerClosesOnSuccessfulProbe` | [`breaker_test.go`](../../internal/controller/breaker_test.go) | A successful probe clears all four state fields |
| `TestBreakerFailureDoesNotExtendAnOpenWindow` | [`breaker_test.go`](../../internal/controller/breaker_test.go) | A failure racing an open window leaves the schedule alone |
| `TestBreakerDisabled` | [`breaker_test.go`](../../internal/controller/breaker_test.go) | `threshold < 1` and a nil receiver are inert |
| `TestNewProviderBreakerClampsCooldown` | [`breaker_test.go`](../../internal/controller/breaker_test.go) | The cap cannot be configured below the base |
| `TestProviderCircuitStopsHammeringTheProvider` | [`reconciler_circuit_test.go`](../../internal/controller/reconciler_circuit_test.go) | Zero provider calls while open |
| `TestProviderCircuitHoldsReadyWithoutChurn` | [`reconciler_circuit_test.go`](../../internal/controller/reconciler_circuit_test.go) | The hold is recorded exactly once |
| `TestProviderCircuitStillGivesUpAfterTheGrace` | [`reconciler_circuit_test.go`](../../internal/controller/reconciler_circuit_test.go) | The grace keeps running while the circuit is open |
| `TestProviderCircuitRecovers` | [`reconciler_circuit_test.go`](../../internal/controller/reconciler_circuit_test.go) | Probe → success → closed, degradation cleared |
| `TestProviderCircuitIgnoresAnIsolatedBrokenBucket` | [`reconciler_circuit_test.go`](../../internal/controller/reconciler_circuit_test.go) | One broken bucket among healthy ones never holds the fleet |
| `TestProviderCircuitDefersTeardown` | [`reconciler_circuit_test.go`](../../internal/controller/reconciler_circuit_test.go) | Finalizer retained, no provider call, delete completes on the next probe |
| `TestRetryTransportDoesNotRetryDefiniteAnswers` | [`stackit/retry_test.go`](../../stackit/retry_test.go) | `429` and every other 4xx are answers, not retries |
| `TestNewRetryTransportDefaults` | [`stackit/retry_test.go`](../../stackit/retry_test.go) | The 30s idle timeout reaches a *clone* of `http.DefaultTransport` |
| `TestSAKeyReloadResetsTheBreaker` | [`sakey_reload_test.go`](../../internal/controller/sakey_reload_test.go) | The third `Success()` call site: a validated key swap closes the breaker |
| `TestSAKeyRejectionNeverTouchesTheBreaker` | [`sakey_reload_test.go`](../../internal/controller/sakey_reload_test.go) | A rejected candidate is neither a success nor a failure — the run is left exactly as it was |
| `TestSAKeyObserverIsSafeWithoutABreaker` | [`sakey_reload_test.go`](../../internal/controller/sakey_reload_test.go) | The observer's nil-breaker path, so a test reconciler needs no breaker |

`withCircuit` builds the test reconciler at the shipped threshold (`3`) and drives
`ProviderBreaker.now` from a fake clock, so cooldowns are skipped rather than slept through. Keep
that property when adding a test here — a sleeping breaker test is an unreliable one.

</details>

## What is wrong today

**A teardown fault counts toward the trip, and no record says whether it should.** The emptiness
guard ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)) refuses a delete by
returning an error from `teardown()`, and `reconcileDelete` routes that through `fail()` — which
calls `Breaker.Failure()`. In a fleet with little other traffic, a `Bucket` being deleted while it
still holds objects can therefore open the circuit fleet-wide on its own. Verified by reading
[`bucket_controller.go`](../../internal/controller/bucket_controller.go) (`teardown` →
`prepareBucketForDelete` → `reconcileDelete` → `fail`); **not** verified by execution, and no test
covers it.

This is an unrecorded consequence rather than a contradiction, and the distinction is worth getting
right before anyone "fixes" it. [ADR 0013 D3](../adr/0013-a-provider-outage-is-held-fleet-wide.md)
exempts a definitive fault *as enumerated by*
[ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md) — structured `400`/`401`/`403`,
a self-destroyed credential, the six configuration faults (the code's `failNoRequeue` set), plus the
subject-state exclusions. A non-empty bucket refusing its own deletion is on none of those lists,
and [ADR 0013 D4](../adr/0013-a-provider-outage-is-held-fleet-wide.md) itself contemplates a teardown
that already recorded `Failed` through this same path. So the ADRs simply do not say which side of
the trip condition a teardown fault falls on; D3 as written covers the provisioning path, and the
teardown path counts.

**The first hold of a deletion still writes a phase.** `reconcileDelete` sets `Deleting` before it
consults `Allow()`, so a `Bucket` deleted while the circuit is already open moves `Ready` → `Deleting`
on that first pass — one write more than
[ADR 0013 D4](../adr/0013-a-provider-outage-is-held-fleet-wide.md)'s "the hold changes no phase"
allows. Every subsequent held pass is silent, so the churn D4 exists to prevent does not occur; the
divergence is one status update per deletion, not per reconcile.

**The measurement controller is outside the gate.** `BucketUsageReconciler.SetupWithManager` passes
only `MaxConcurrentReconciles` and the reconciler holds no `Breaker`, so size measurements keep
listing objects on the data plane with the admin credential while the control plane is held, on the
controller-runtime default rate limiter. The measurement interval floor bounds the volume, but it is
not zero. Accepted in [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) and
[ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md); the mechanism is
[usage-measurement.md](usage-measurement.md).

**The breaker is process-local and does not survive a restart.** A crash-looping operator re-learns
the outage from scratch each time and can, in the limit, reproduce the traffic the breaker exists to
suppress. Two replicas would each hold a private breaker; leader election means only one
*reconciles*, so that is latent rather than live — inferred from the design, not observed. The key
poller is the one runnable that is not covered by that argument: it reports
`NeedLeaderElection() false` and therefore polls on every replica
([ADR 0016 D13](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)), so a
standby makes a provider call of its own whenever the key file's content changes, gated by nothing.
Bounded by the same limits as on the leader, multiplied by the replica count.

**A probe is a full reconcile, not a health check.** The first pass admitted after a cooldown runs
the whole provisioning pipeline for whichever bucket the queue hands over. If that bucket has a
definitive local fault it takes `failNoRequeue`, which touches the breaker not at all, and the probe
teaches the breaker nothing.

**Not verified: the breaker has never run against a real provider outage.** Its behaviour is
established by the offline tests listed above and by reasoning from the 2026-09-02 log and metric
record. The `e2e-stackit` suite does not exercise it. Likewise unmeasured: the provider's actual
IP-level rate-limit thresholds, and the edge's idle timeout that `idleConnTimeout` is chosen to
undercut.
