# Provider outages

What the operator does when it cannot reach the StackIT Object Storage API, what
that looks like on a `Bucket` and in Prometheus, and which two settings bound it.

Two mechanisms are at work, and they are separate:

| Mechanism | What it decides | Record |
| --- | --- | --- |
| The degradation hold | Whether a provisioned `Bucket` keeps saying `Ready` while the operator cannot verify it | [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) |
| The fleet-wide circuit breaker | Whether the operator calls the provider at all | [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) |

The hold is about what a `Bucket` *claims*; the breaker is about what the
operator *does*. An outage normally triggers both, in that order: the first
failures are held per `Bucket`, and once enough of them arrive back to back the
operator stops calling out entirely.

---

## What you see first

`Ready` on a provisioned `Bucket` reports the last state the operator
**verified**, not the outcome of the last attempt to verify it
([ADR 0012 D1](../adr/0012-ready-describes-the-last-verified-state.md)). A
failure while talking to the provider, to the S3 data plane or to the Kubernetes
API is a failure to verify, so the `Bucket` keeps its phase and its `Ready`
condition and records the degradation alongside them:

```
$ kubectl get bucket -A
NAMESPACE   NAME        BUCKET      PHASE   READY   STATUS                          REGION   SIZE       COST/MONTH   AGE
team-a      my-bucket   my-bucket   Ready   True    ensure bucket: unexpected EOF   eu01     18.0 GiB   0.53 EUR     3h
```

(The `SIZE` and `COST/MONTH` cells are filled only where size measurement is on;
six further columns carry `priority=1` and appear with `-o wide` - see
[bucket-status.md](bucket-status.md).)

```yaml
status:
  phase: Ready
  message: "ensure bucket: unexpected EOF"
  degradedSince: "2026-08-25T08:13:04Z"   # example - when the run of failures began
  conditions:
    - type: Ready
      status: "True"                      # held: the last VERIFIED state
      reason: Provisioned
    - type: ProviderReachable
      status: "False"
      reason: ProviderUnreachable
      message: "ensure bucket: unexpected EOF"
```

Four things about that object, all of them deliberate
([ADR 0012 D4](../adr/0012-ready-describes-the-last-verified-state.md)):

| Signal | Behaviour while held |
| --- | --- |
| `status.degradedSince` | Written **once**, when the hold starts. It does not move on every retry or probe, so an outage lasting hours costs no status churn |
| `ProviderReachable` | Set to `False` with reason `ProviderUnreachable` and the error as its message. It is **removed**, not set to `True`, on recovery - a `Bucket` that never degraded and one that recovered look identical |
| `status.message` | Carries the same error text as before the hold existed |
| Events | A `Warning` event with reason `Failed` and the error text, exactly as a hard failure would raise. The reason a `Bucket` is held is always on the object |

Fleet-wide, the two series to look at are
`stackit_s3_provisioner_buckets_provider_degraded` (how many `Bucket`s are being
held) and the per-`Bucket`
`stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds`, which names
them. Both require phase `Ready`: they describe a hold that is *in effect* and
disappear the moment it is given up - see
[monitoring.md](monitoring.md) for the full catalogue and
[bucket-status.md](bucket-status.md) for how to read the rest of the status.

## What is not held

These drop `Ready` immediately, whatever the grace says. The list is closed and
is maintained in
[ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md); anything
not on it is held, so a provider failure mode nobody has seen yet cannot land on
the wrong side of the line
([ADR 0012 D3](../adr/0012-ready-describes-the-last-verified-state.md)).

| Case | What you see | Why it is definitive |
| --- | --- | --- |
| A structured `400`, `401` or `403` from the provider | Phase `Failed` at once, `status.message` carrying the API error | The provider's own refusal, not a failure to reach it. `401`/`403` is the Object Storage API declining an authenticated request; **`400` is how a revoked service-account key surfaces** - the key flow never reaches the Object Storage API and the token endpoint answers `400 {"error":"invalid_grant"}` (measured live 2026-08-25) |
| A gateway or WAF error page carrying `403`, `503`, … | Held, like any other unreachability | It is a failure *in front of* the provider. The discriminator is the body - a structured JSON answer against anything else - never the status code alone ([ADR 0012 D2](../adr/0012-ready-describes-the-last-verified-state.md)). An HTML error page carrying `403` was the 2026-08-25 incident |
| A workload credential the operator destroyed | Phase `Failed`, message naming the failed key replacement | The previous access key was deleted and its replacement could not be published. The operator knows the credential in the Secret is dead: local certainty, not an unverifiable provider state. See [credentials.md](credentials.md) |
| A configuration fault | Phase `Failed`, no requeue hammer; it re-reconciles on the next spec change | A Secret key collision, a `spec.secretRef` aimed at the operator's own admin Secret, a `spec.region` other than the operator's, a composed name outside the permitted range, an ownership collision, an unusable clone source. Each is a statement the operator made about *this* `Bucket` on its own |
| A `Bucket` being deleted | Phase `Deleting` or `Failed`, finalizer retained | Holding `Ready` through a teardown would hide a delete blocked by the emptiness guard of [ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md). See [deletion.md](deletion.md) |
| A `Bucket` whose spec has not been observed (`status.observedGeneration` != `metadata.generation`) | Phase `Failed` | The user asked for something new and it was not achieved; there is no verified state matching the current spec |
| A `Bucket` that has never been `Ready` | Phase `Failed` | There is nothing verified to defend. Failures during initial provisioning surface at once |

The consequence of the first row is worth stating plainly: when the
service-account key is revoked, the fleet does **not** stay green for the grace
window. It fails at once, which is the intended behaviour and the reason `400`
is on the list at all.

## The grace window

The hold is bounded by one operator-wide setting,
`providerDegradedGrace` (`30m` # default). Once it elapses, the `Bucket` drops to
phase `Failed` exactly as it did before the hold existed, keeping
`status.degradedSince` so the status still records when the trouble started
([ADR 0012 D5](../adr/0012-ready-describes-the-last-verified-state.md)).

- **It is not per-`Bucket`.** No `Bucket` can ask for a longer or a shorter hold
  than the operator's - the grace expresses how long *the operator* is willing to
  vouch for a state it cannot verify, and a namespace-writable field would let a
  tenant extend its own blind window.
- **`"0"` disables the hold** and restores the pre-hold behaviour, with no
  different image. Every reconcile failure then drops `Ready` on the spot.
- **The value needs a unit.** `"30m"`, `"1h"` - a bare number is rejected by the
  flag parser and the operator crash-loops at startup.

The price, accepted knowingly: while the operator is blind, a `Bucket` keeps
asserting a state nobody is checking. A policy edited behind the operator's back,
a credential revoked at the provider, a bucket filling up - all of it still reads
as `Ready` until something definitive arrives or the grace elapses. A consumer
that wants "the last attempt succeeded" must read `ProviderReachable` or the
degraded metrics, not `Ready`.

## When the circuit opens

After `providerCircuit.threshold` reconciles fail back to back **with no
successful reconcile in between**, the operator stops calling the provider for
every `Bucket` and probes on a doubling cooldown until one succeeds
([ADR 0013 D1](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).

The trip condition is the *absence of success*, never a classification of the
error ([ADR 0013 D2](../adr/0013-a-provider-outage-is-held-fleet-wide.md)). A
single broken `Bucket` among healthy ones has its failures interleaved with the
successes of the rest of the fleet, which resets the run; it keeps reporting its
own error and never holds anyone else. Only non-definitive failures count toward
the trip - a definitive fault from the table above neither trips the breaker nor
resets it, because it says nothing about the provider
([ADR 0013 D3](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).

| Aspect | Behaviour while the circuit is open |
| --- | --- |
| Provider calls | **None at all**, teardown included ([ADR 0013 D4](../adr/0013-a-provider-outage-is-held-fleet-wide.md)) |
| Reconcile result | A delayed retry on the breaker's cooldown, and **no error** ([ADR 0013 D5](../adr/0013-a-provider-outage-is-held-fleet-wide.md)) |
| `controller_runtime_reconcile_errors_total` | Counts the outage once - `threshold - 1` errors, so **2** at the default threshold of `3` - not the retries. The failure that reaches the threshold already returns no error, and neither does any failed probe after it |
| Probe cadence | `60s`, doubling per failed probe, capped at `providerCircuit.maxCooldown` (`5m` # default) |
| A held `Bucket`'s status | Held per `providerDegradedGrace`, which keeps running: `status.degradedSince` written once, `ProviderReachable=False`, reason `ProviderUnreachable` |
| Events and `status.message` | Recorded once per `Bucket` per outage, and once more when the grace elapses. Every further held reconcile is silent |
| A `Bucket` created during the outage | Has never been `Ready`, so it is not held: phase `Failed`, message `StackIT API unavailable (provider circuit open); next provider probe in 1m0s` (# example - the wait is rendered by Go's duration formatting, so the base cooldown prints as `1m0s`, and a later probe as e.g. `2m0s`). It provisions normally once a probe succeeds |
| A deletion | Appears stuck (see below) |
| Size measurement | **Not gated by the breaker.** The measurement controller runs on its own queue and keeps listing objects on the data plane with the admin credential while the control plane is held ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md), residual risks; see [usage-and-cost.md](usage-and-cost.md)) |

The breaker delays reconciles; it never widens the window in which a `Bucket` may
claim an unverified state. A `Bucket` held past `providerDegradedGrace` still
drops to `Failed` while the circuit is open
([ADR 0013 D7](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).

### A deletion during an outage does not progress

This is the part to know before somebody reports it as a bug. The teardown talks
to the provider on every step - the emptiness check, the keys, the group, the
bucket - so it is gated like any other provider work. The finalizer stays, the
`Bucket` keeps whichever phase its teardown had already reached (`Deleting`, or
`Failed` if an earlier attempt recorded a reason of its own such as the emptiness
guard), and the deletion resumes at the next successful probe. Nothing is
destroyed on the strength of an unanswered call - the same direction of caution as
the emptiness guard itself ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)).

A held teardown writes **nothing that names the outage**: no event, no
`status.message`. (The very first teardown pass still sets phase `Deleting` with
the message `releasing StackIT resources`, because that write happens before the
breaker is consulted - but it says nothing about an outage.) On the object, a
delete blocked by an outage is therefore indistinguishable from a delete blocked
by its own cause. The difference lives in the operator log and in the circuit
metrics, which a namespace user generally cannot read.

Verified in the code: `reconcileDelete` consults the breaker before any teardown
step and returns a requeue without touching the finalizer. **Not verified:** this
path has offline test coverage only - it has never been exercised against a real
provider outage.

One consequence for alerting: an outage lasting over 30 minutes with a deletion
in flight fires `StackitS3BucketStuckDeleting`, whose description blames the
emptiness guard. Check `stackit_s3_provisioner_provider_circuit_open` before
acting on that description.

### The two metrics

| Metric | Presence | Reads as |
| --- | --- | --- |
| `stackit_s3_provisioner_provider_circuit_open` | **Always** exported, `0` while closed - including when the breaker is disabled - so an alert expression never evaluates against a missing series | `1` while the operator is holding the fleet |
| `stackit_s3_provisioner_provider_circuit_opened_timestamp_seconds` | Only while the circuit is open | `time() - <series>` is the age of the outage. It does **not** move when a probe fails, so it measures the outage rather than the last probe interval |

There is deliberately **no alert on the open circuit**
([ADR 0013 D12](../adr/0013-a-provider-outage-is-held-fleet-wide.md)). The
circuit is a state to look at while investigating; the thing worth waking
somebody for is a hold that has lasted long enough that the drop to `Failed` is
near, and that is `StackitS3BucketProviderDegraded`.

## The two settings that matter

Both are Helm values on the operator, not fields on a `Bucket`. This page
explains them; the complete configuration surface lives in the
[README reference](../../README.md) and in
[configuration.md](configuration.md).

```yaml
# values.yaml - the outage-relevant part of a working configuration
providerDegradedGrace: "30m"   # default; needs a unit. "0" disables the hold
                               # entirely: every failure drops Ready at once.
providerCircuit:
  threshold: 3                 # default; consecutive non-definitive failures with
                               # no success in between before the operator stops
                               # calling the provider. 0 disables the breaker.
  maxCooldown: "5m"            # default; upper bound on the wait between probes,
                               # and therefore on how far recovery lags behind the
                               # provider returning. The wait starts at a fixed
                               # 60s and doubles per failed probe.

monitoring:
  prometheusRule:
    alerts:
      bucketProviderDegraded:
        holdForSeconds: 1200   # default (20m); MUST stay below
                               # providerDegradedGrace, see below.
      reconcileErrors:
        suppressWhileCircuitOpen: true   # default
```

How they interact:

| Constraint | Consequence of getting it wrong |
| --- | --- |
| `holdForSeconds` must stay **below** `providerDegradedGrace` | Once the grace elapses the `Bucket` drops to `Failed`, the degraded series disappears (both degraded metrics require phase `Ready`) and the alert can never fire. `StackitS3BucketFailed` takes over, but the warning that could still have been acted on is lost |
| The base probe cooldown is fixed at `60s` and is not configurable | It only decides how fast the first probe follows a trip. It is coarse on purpose: `StackitS3ReconcileErrors` suppresses windows in which the circuit was open, and the chart scrapes every 30s, so a trip must outlive a scrape gap to be *observable* rather than merely effective |
| `maxCooldown` below `60s` | Raised to `60s` by the breaker, so the base and the cap cannot be configured inside out. A value of `0` or less falls back to the `5m` default |
| `reconcileErrors.threshold` (`6` # default) too low against `providerCircuit.threshold` | One tripped circuit costs `providerCircuit.threshold - 1` reconcile errors - **2** at the default threshold of `3` - so a low alert threshold pages for outages the breaker already absorbed. The rule fires on *more than* the threshold in a 15-minute window, so the shipped `6` absorbs three independent trips in one window and pages on the fourth. The constraint only bites when `suppressWhileCircuitOpen` is `false`, or when a trip was too short to be caught by a scrape: with the default `true`, every 15-minute window in which the circuit was open at any point is dropped from the expression, so a tripped circuit does not reach this alert at all |
| `providerCircuit.threshold: 0` | A values-only rollback to the pre-breaker behaviour, with no different image: every retry counts an error again, and the operator keeps calling a failing endpoint |
| `providerDegradedGrace: "0"` with the breaker on | The breaker still holds the fleet, but every held `Bucket` goes to `Failed` on the first failure. The two settings are independent; disabling one does not disable the other |

Rolling either out is an ordinary Helm upgrade; see
[deployment.md](deployment.md).

### What is not tunable

Three pieces of the outage machinery are fixed in the code, and they exist
because the operator once amplified an outage into its own IP-level rate limit
([ADR 0013 D9](../adr/0013-a-provider-outage-is-held-fleet-wide.md)):

<details>
<summary>The fixed pacing values</summary>

| Value | Setting | Why it is fixed |
| --- | --- | --- |
| Workqueue backoff for failed reconciles | Exponential from `1s` to `15m`, fleet-wide `1` requeue per second with a burst of `5` | The controller framework's defaults (from `5ms`, 10 per second, burst 100) are chosen for a reconcile that makes a few calls against the local API server. This one makes several calls against a rate-limited remote API per pass |
| Retries on a `429` response | None on the control-plane transport, at any attempt count ([ADR 0013 D9](../adr/0013-a-provider-outage-is-held-fleet-wide.md)) | Re-asking is the one reply to a rate limit guaranteed to deepen it. Backing off from a rate limit belongs to the workqueue and the breaker, which see the whole fleet, not to a single in-flight request. **This covers the control plane only.** The S3 data plane runs on `minio-go`, which counts `429` among its own retryable statuses and retries it internally - and the size-measurement controller, which is not gated by the breaker, is exactly where that keeps happening during an outage |
| Idle pooled connection lifetime | `30s` | The operator talks in bursts and is then idle for minutes, so a pooled connection the remote end has already dropped surfaces as `read: connection reset by peer`. Closing first turns that into a fresh dial. **Not verified:** the remote end's own idle timeout is not published and was never measured - `30s` was chosen to sit below any plausible value |

Transport-level retries still apply to `GET` and `HEAD` requests only (3
attempts, `200ms` backoff tripling), because repeating a failed write is
ambiguous and a repeated access-key creation would mint a key whose secret is
returned once and then lost. See
[docs/developer/provider-errors.md](../developer/provider-errors.md).

Watch-driven and timer-driven reconciles do not pass through the failure backoff,
so the drift resync keeps its own timing.

</details>

## Confirming the provider, not the operator, is at fault

Work down this list; each step distinguishes a case the one above cannot.

1. **Is the operator holding the fleet?** Port-forward the metrics endpoint and
   read the circuit gauge.

   ```bash
   kubectl -n <operator-namespace> port-forward deploy/stackit-s3-provisioner 8080:8080 &
   curl -s localhost:8080/metrics | grep -E 'provider_circuit|provider_degraded'
   ```

   `stackit_s3_provisioner_provider_circuit_open 1` means the operator has
   stopped calling out and is probing. `..._provider_circuit_opened_timestamp_seconds`
   gives the start of the outage; `..._buckets_provider_degraded` gives how many
   `Bucket`s are held right now.

2. **Which `Bucket`s, and since when?**

   ```bash
   kubectl get bucket -A -o custom-columns=\
   'NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,DEGRADED:.status.degradedSince,MSG:.status.message'
   ```

   Every `Bucket` degraded from the same moment is a provider event. One
   `Bucket` degraded while the rest are clean is that `Bucket`'s problem, and by
   construction it never trips the breaker.

3. **What is the error?** `status.message` and the `ProviderReachable` condition
   carry the last error verbatim. The operator log carries it too, with the
   retry schedule:

   ```bash
   kubectl -n <operator-namespace> logs deploy/stackit-s3-provisioner --since=30m \
     | grep -E 'provider circuit open|ProviderUnreachable'
   ```

   Two lines matter: `reconcile failed; provider circuit open` (error level, with
   `bucket` and `retryAfter`) and `provider circuit open; deferring teardown`
   (debug level, emitted for a held deletion). The operator's logger runs in
   development mode, so debug lines are printed by default.

4. **Read the error, not its status code.** An HTML error page from an
   intermediary and a real refusal by the API arrive in the same shape and can
   carry the same status code. If the logged body is HTML, the request never
   reached the API and the problem is in front of it. If the body is structured
   JSON with `400`, `401` or `403`, the provider itself refused - and a `400` at
   the token endpoint with `{"error":"invalid_grant"}` means the service-account
   key is gone. See [docs/developer/provider-errors.md](../developer/provider-errors.md)
   for the measured semantics and [credentials.md](credentials.md) for replacing
   the key.

5. **Verify recovery.** Once the provider is back, the first reconcile admitted
   after the current cooldown is the probe; on success the breaker closes, the
   degradation markers are removed from every `Bucket` as it reconciles, and
   `stackit_s3_provisioner_provider_circuit_open` returns to `0`. Recovery lags
   the provider by up to `providerCircuit.maxCooldown` per outage, and the fleet
   then reconciles one `Bucket` at a time rather than all at once - the `Bucket`
   controller runs at controller-runtime's default concurrency of one. The
   fleet-wide rate limit of 1 requeue per second applies to *failed* reconciles
   only, so it does not pace the recovery pass.

## When a Bucket falls to Failed after the grace

The grace elapsed, so the operator has stopped vouching for a state it cannot
verify. The object now reads:

```yaml
status:
  phase: Failed
  message: "ensure bucket: unexpected EOF"
  degradedSince: "2026-08-25T08:13:04Z"   # example - kept, so the status still
                                          # says when the trouble started
  conditions:
    - type: Ready
      status: "False"
      reason: Failed
```

`StackitS3BucketProviderDegraded` stops firing at this moment - both degraded
metrics require phase `Ready` - and `StackitS3BucketFailed` takes over. That
handover is the reason `holdForSeconds` must sit below the grace.

What to do:

1. **Establish whether this is still the same outage.** Check the circuit gauge
   (step 1 above). If the circuit is open, the operator is still holding; nothing
   on the `Bucket` needs fixing and the phase will recover on its own once a
   probe succeeds.
2. **If the circuit is closed and the `Bucket` is still `Failed`,** the failure
   is not fleet-wide. Read `status.message`: a configuration fault or a
   structured refusal from the table above is a real fault on this `Bucket`. See
   [bucket-status.md](bucket-status.md).
3. **Do not delete the `Bucket` to clear the phase.** A delete blocked by an
   outage will not progress, and a delete that *does* progress destroys the
   credentials group and the bucket. `Failed` is a status, not a lost resource -
   a successful reconcile clears it and removes `degradedSince` and the
   `ProviderReachable` condition.
4. **If the outage is long and known,** raising `providerDegradedGrace` and
   upgrading holds the fleet green for longer. This is a deliberate trade: it
   widens the window in which every `Bucket` claims a state nobody has verified.

### Two gaps worth knowing about

**A bucket that vanished at the provider is not covered by any of this.** The
provider answers - it answers that the bucket is absent - so the reconcile
succeeds, nothing is held and none of the signals on this page ever appear. What
the operator does instead, and what it costs, is in
[bucket-status.md](bucket-status.md) under *When the status disagrees with the
cloud*.

**The breaker lives in the operator process.** It does not survive a restart, so
a crash-looping operator re-learns the outage from scratch each time and can, in
the limit, reproduce the traffic the breaker exists to suppress. With a single
`Bucket` in the cluster, three consecutive failures of that one `Bucket` open the
circuit "fleet-wide", which for a fleet of one is the same thing - in a small
fleet the breaker is more sensitive than the wording suggests.

**Not verified:** neither the hold nor the breaker has been exercised against a
real provider outage since it was built. Their behaviour is established by
offline tests and by reasoning from the operator log and metric record of
2026-08-25 and 2026-09-02, not by a repeat incident. That `30m` is the right
grace is likewise a choice made to outlast the observed blips (about two minutes
on 2026-08-25, about twenty minutes on 2026-09-02), not a value derived from a
measured distribution of outage lengths.

---

## Related

| Page | What it adds |
| --- | --- |
| [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) | The decision behind the hold, the classification rule, and the closed list of definitive faults |
| [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) | The decision behind the breaker, the 2026-09-02 incident record, and the alerting retune |
| [monitoring.md](monitoring.md) | The full metric catalogue and every alert |
| [bucket-status.md](bucket-status.md) | Phases, conditions and the faults that park a `Bucket` |
| [deletion.md](deletion.md) | What a delete does, and the other reasons one hangs |
| [configuration.md](configuration.md) | The settings in their wider context |
| [docs/developer/circuit-breaker.md](../developer/circuit-breaker.md) | The mechanism: state, counting, probe scheduling, the workqueue limiter |
| [docs/developer/provider-errors.md](../developer/provider-errors.md) | How a provider error is recognised and classified |
