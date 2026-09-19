# ADR 0013: A Provider Outage Is Held Fleet-Wide, and the Trip Condition Is the Absence of Success

## Status

Accepted. Date: 2026-09-02. Amends [ADR 0012](0012-ready-describes-the-last-verified-state.md).

Implemented: the fleet-wide breaker, the doubling probe cooldown, the fleet-wide workqueue pacing,
the removal of transport-level retries for rate-limited responses, the two circuit metrics, and the
retuned alerting. Open: the size-measurement controller runs on its own queue and is **not** gated by
the breaker, so it keeps listing objects on the data plane while the control plane is held; the
breaker is a single in-process state, so two operator replicas would each hold their own (only one
reconciles at a time under leader election, so this is latent, not live).

## Context

[ADR 0012](0012-ready-describes-the-last-verified-state.md) made a provisioned bucket keep its
`Ready` state through a provider failure it could not classify. It stabilised what the cluster
*advertised* and deliberately changed nothing about what the operator *did*: every failure was still
returned as a reconcile error, so the retry, the `Warning` event and
`controller_runtime_reconcile_errors_total` behaved exactly as before. That non-change was recorded
as intentional. It turned out to be the actual driver of the paging.

A read-only investigation of the operator logs and of Prometheus on the production management
cluster on 2026-09-02 produced the following error timeline (UTC, 22 `Bucket` CRs in the fleet):

| Time | Observed |
| --- | --- |
| 14:23 | 11 × `503`, delivered as an HTML error page from the provider edge |
| 14:29 | 17 × `503` |
| 14:34 | 19 × `503` **and the first 14 × `429`** |
| 14:40 | 19 × `503`, 21 × `429` |
| 14:41 | 20 × `503`, 11 × `429` |
| 14:42 | 20 × `503`, **51 × `429`** |
| 14:43 | over; the provider recovered on its own |

The body of the `429` was
`{"status":429,"error":"Too Many Requests","message":"rate limit on IP level exceeded; please try again later"}`.

The causality is readable off the order. The `503` comes first, at 14:23 UTC; the rate limit appears
eleven minutes later, at 14:34 UTC, and then grows. The provider edge failed, and the operator then
hammered its own IP-level rate limit — it extended the outage it was reacting to. The operator can
observe only its own requests, never the edge's total load, so the reading that by 14:42 UTC most of
the load on the failing endpoint was the operator's own retries is an inference from that record and
not a measurement. `sum(increase(controller_runtime_reconcile_errors_total{controller="bucket"}[15m]))`
peaked at 220; the run cost 242 reconcile errors in total. The alert threshold at the time was `> 3` with no
sustain, so one self-healing outage paged twice for something nobody could act on.

Two amplifiers were in the operator's own code, not the provider's:

1. **No workqueue rate limiter on the bucket controller.** It ran on the controller framework's default:
   per-item exponential backoff starting at **5 ms**, with a fleet-wide allowance of 10 requeues per
   second and a burst of 100. Those numbers are chosen for a controller whose reconcile makes a few
   calls against the local API server. This one makes several calls against a rate-limited remote API
   per pass.
2. **Rate-limited responses were retried.** The transport under the SDK retried on `429` with a fixed
   backoff and no regard for `Retry-After`, three attempts per read. Re-asking is the one reply to a
   rate limit that is guaranteed to deepen it.

Underneath both sat a baseline of roughly one error per fifteen minutes from
`read: connection reset by peer` on pooled keep-alive connections the operator kept open longer than
the remote end did. Not verified: the remote end's own idle timeout is not published and was never
measured, so the 30 seconds the operator now holds an idle connection was chosen to sit below any
plausible value rather than to match one. The baseline never tripped the old alert threshold alone,
but it made the threshold cheap to exceed.

The forces that shape the fix: while the provider answers `503`, reconciling the seventeenth bucket
cannot succeed, and attempting it costs calls that make the outage worse — so per-bucket retry is not
merely wasteful, it is harmful. At the same time, [ADR 0012](0012-ready-describes-the-last-verified-state.md)
already refused to maintain a taxonomy of transient error shapes, because such a list is
provider-controlled, open-ended, and silently wrong for anything not on it. A fleet-wide hold
therefore needs a discriminator that is not error parsing.

## Decision

**D1 — A provider outage is a property of the provider, not of a bucket.** After a configured number
of consecutive reconciles that fail for a non-definitive reason, with no successful reconcile in
between, the operator stops calling the provider for **every** bucket and holds the whole fleet until
a probe succeeds.

**D2 — The trip condition is the absence of success, never a classification of the error.** A single
broken bucket among healthy ones has its failures interleaved with the successes of the rest of the
fleet, which resets the run; a fleet-wide outage produces an unbroken chain of failures. This keeps
the breaker out of the error-parsing business that
[ADR 0012](0012-ready-describes-the-last-verified-state.md) already declined to enter, and it
requires no knowledge of what the provider's failure looks like.

**D3 — Only non-definitive failures count toward the trip; any completed reconcile closes the
breaker immediately.** A definitive fault — one the operator established locally about that one
bucket, in the closed list held by [ADR 0012](0012-ready-describes-the-last-verified-state.md) —
neither trips the breaker nor resets it, because it says nothing about the provider. A successful
provisioning pass and a successful teardown both count as success: the provider answered.

**D4 — While the breaker is open the operator makes no provider call at all, including during
teardown.** The finalizer stays and the deletion resumes at the next probe. The hold changes no
phase: the object keeps whichever phase its teardown had already reached — `Deleting`, or `Failed` if
an earlier teardown attempt recorded a reason of its own, such as the emptiness guard. A held
teardown is also silent — no event, no `status.message` — so a delete blocked by an outage is
indistinguishable on the object from a delete blocked by its own cause; the operator log and the
circuit metrics carry the difference. A delete therefore appears stuck for the duration of an
outage. This is the same direction of caution as the emptiness guard in
[ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md): never destroy on the strength of an
unanswered call.

**D5 — A held reconcile returns a delayed retry and no error.** The failure is still logged, and the
reason is recorded on the object — a `Warning` event with reason `Failed`, and `status.message` —
once per bucket per outage, plus once more at the moment the degradation grace elapses. A repeat hold
of a bucket that already carries that record writes nothing. What stops is the counting once per
retry, so `controller_runtime_reconcile_errors_total` measures outages rather than retry volume. One
outage therefore costs one reconcile error fewer than the threshold — two at the default threshold of
`3` — because the failure that reaches the threshold already returns none, and neither does any
failed probe after it.

**D6 — Probing follows a doubling cooldown with a fixed base and a configurable cap.** The wait
starts at 60 seconds and doubles per failed probe up to `--provider-circuit-max-cooldown` (Helm
`providerCircuit.maxCooldown`). The base is fixed on purpose: it only decides how fast the first probe
follows the trip, and nothing operational depends on that, whereas it must stay coarse enough that the
open state outlives a scrape gap and is therefore *observable*, not merely effective. Once the
cooldown elapses the breaker admits reconciles again without closing; the first one through is the
probe, and its outcome decides whether the breaker closes or re-opens with a doubled cooldown.

**D7 — The breaker delays reconciles; it never widens the window in which a bucket may claim an
unverified state.** The degradation grace of
[ADR 0012](0012-ready-describes-the-last-verified-state.md) keeps running while the circuit is open,
and a bucket held past it still drops to `Failed`. `status.degradedSince` is written once per outage,
not once per probe, so an outage lasting hours costs no status churn.

**D8 — The breaker is switched off by setting `--provider-circuit-threshold` (Helm
`providerCircuit.threshold`) to `0`.** That is a values-only rollback to the pre-breaker behaviour and
needs no different image.

**D9 — The operator paces its own retries fleet-wide, so that a provider failure cannot be turned
into a rate limit before the breaker even trips.** Failed reconciles requeue on an exponential backoff
from 1 second to 15 minutes with a fleet-wide allowance of 1 per second and a burst of 5; a response
carrying `429` is never retried at the HTTP layer, because backing off from a rate limit belongs to
the workqueue and to the breaker, which act on the whole fleet rather than on one in-flight request;
and idle pooled connections are closed after 30 seconds, so that the operator drops a stale
connection before the remote end does — 30 seconds being a value chosen to sit below any plausible
remote idle timeout, not one measured against it. Watch-driven and timer-driven reconciles do not
pass through the failure backoff, so drift resync keeps its timing.

**D10 — Two metrics describe the circuit, with deliberately different presence semantics.**
`stackit_s3_provisioner_provider_circuit_open` is **always** exported, so an alert expression never
evaluates against a missing series. `stackit_s3_provisioner_provider_circuit_opened_timestamp_seconds`
exists only while the circuit is open and does **not** move when a probe fails, so it measures the
outage rather than the last probe interval.

**D11 — Alerting reports the need to act, not the burst.** `StackitS3ReconcileErrors` excludes every
window in which the circuit was open at any point, so a provider outage cannot reach it; what remains
is failures that stayed interleaved with fleet successes — a single bucket failing while the rest of
the fleet reconciles, whatever the origin of its failure. A failure that is itself fleet-wide trips
the breaker and is removed by the same exclusion, and that is true regardless of where it came from:
a failure while talking to the Kubernetes API counts toward the trip exactly like a failure while
talking to the provider, because the trip condition is the absence of success (D2), not the origin of
the error. The exclusion is fail-open by construction: if the circuit metric is absent, nothing is
removed and the alert behaves as it did before. A provider outage surfaces through `StackitS3BucketProviderDegraded` instead, which fires on the **age** of the hold
rather than on its existence.

**D12 — There is no alert on the open circuit itself.** The circuit is observability — a dashboard
signal and an alert suppressor. The thing worth waking somebody for is a hold that has lasted long
enough that the drop to `Failed` is near, and that is already one alert.

### Settings

| Setting (flag / Helm value) | Default | What it bounds |
| --- | --- | --- |
| `--provider-circuit-threshold` / `providerCircuit.threshold` | `3` | Consecutive non-definitive failures before the operator stops calling the provider. `0` disables the breaker (D8) |
| `--provider-circuit-max-cooldown` / `providerCircuit.maxCooldown` | `5m` | Upper bound on the wait between probes, and therefore on how far recovery lags behind the provider returning |
| base probe cooldown | `60s`, not configurable | How soon the first probe follows a trip (D6) |
| `--provider-degraded-grace` / `providerDegradedGrace` | `30m` | Owned by [ADR 0012](0012-ready-describes-the-last-verified-state.md); unchanged by the breaker (D7) |
| `monitoring.prometheusRule.alerts.reconcileErrors.suppressWhileCircuitOpen` | `true` | Whether the reconcile-error alert drops windows with an open circuit (D11) |
| `monitoring.prometheusRule.alerts.bucketProviderDegraded.holdForSeconds` | `1200` | Age of the hold that pages. Must stay below `providerDegradedGrace`, or the series disappears before the alert can fire |

### What holds while the circuit is open

| Aspect | Behaviour |
| --- | --- |
| Provider calls | None at all, teardown included (D4) |
| Reconcile result | Delayed retry on the breaker's cooldown, no error (D5) |
| `controller_runtime_reconcile_errors_total` | Counts the outage once, at threshold minus one errors, not the retries (D5) |
| Bucket status | Held per `providerDegradedGrace`; `status.degradedSince` written once, condition `ProviderReachable=False`, reason `ProviderUnreachable` |
| Events and `status.message` | Recorded once per bucket per outage — a `Warning` event with reason `Failed` plus `status.message` — and once more when the grace elapses; every further held reconcile is silent |
| Deletion | Appears stuck: the finalizer stays and no phase changes — `Deleting`, or `Failed` from an earlier teardown attempt — until a probe succeeds, with no event and no `status.message` naming the outage |

## Consequences

A deletion during an outage does not progress. That is the price of D4, and it is the part nobody
likes: a user who deleted a `Bucket` sees a CR that will not go away and a phase that does not move,
and — because a held teardown writes nothing (D4) — nothing on the object says the outage is the
reason. Diagnosing it means reading the operator log or the circuit metrics, which a namespace user
generally cannot. The alternative — letting the teardown path call out while every other path is
held — would mean the operator's only traffic during an outage is the destructive kind, decided on
incomplete answers.

Recovery lags the provider. When the provider returns during a cooldown, nothing happens until that
cooldown elapses — up to `providerCircuit.maxCooldown`, 5 minutes by default, per outage. With a fleet
reconciling one bucket at a time, the fleet then drains at the workqueue's pace rather than at once.

A fleet-wide outage now produces a small, roughly constant number of reconcile errors regardless of
how long it lasts. Any dashboard or alert that treated that counter as a measure of outage severity
reads differently after this change: severity is now a duration on the two circuit metrics and on the
per-bucket degradation timestamp, not a rate on an error counter.

The breaker is one shared state over the whole fleet, so one bucket cannot be diagnosed in isolation
while it is open — every bucket is held, including healthy ones and including a bucket whose own
problem is local. D2 accepts that: the failure of a single bucket is interleaved with fleet successes
and does not trip the breaker in the first place, so the case only arises once the fleet is already
failing.

Cutting the fleet-wide allowance to 1 requeue per second slows recovery after any mass event, not only
a provider outage — a large rollout of new `Bucket` CRs that hits errors converges more slowly than it
did on the controller framework's defaults.

## Alternatives Considered

**A taxonomy of transient provider errors.** Rejected, for the second time. Matching on error text,
on 5xx lists, or on "undefined response type" would let the operator decide per error whether to hold
the fleet, and it would have been cheaper than a breaker — no new state, no probe schedule, no new
metrics. It lost for the reason recorded in
[ADR 0012](0012-ready-describes-the-last-verified-state.md): the list of failure shapes is controlled
by the provider, is open-ended, and every shape not on it falls to the wrong default. The `503`
delivered as an HTML error page and the later `429` were two different shapes from the same single
outage — the evidence for the argument, not against it.

**Per-bucket circuit breakers.** Rejected. A per-bucket breaker sees only its own bucket's failures
and would have to relearn, once per bucket, that the provider is down — which is exactly the cost the
decision removes. It would also reproduce the `429` amplification at reduced volume rather than
eliminate it, because the fleet as a whole would still be probing.

**Rate limiting alone, without a breaker.** Rejected as insufficient, though it was adopted as part of
the fix (D9). Pacing to 1 request per second across the fleet lowers the amplification but keeps the
operator talking to a failing endpoint indefinitely, keeps one error per retry in the error counter,
and leaves the alerting problem exactly where it was. It is a necessary floor, not a solution.

**Honouring `Retry-After` on a rate-limited response instead of not retrying.** Rejected. It is a
correct-sounding refinement that still ends with the operator re-asking a service that just said it is
receiving too much, and it moves the pacing decision into a single in-flight request, where it cannot
see the fleet. D9 puts the decision where the fleet is visible.

**A dedicated alert on the open circuit.** Rejected, deliberately and with the code already in place
to support it. It would fire at the same time as the degradation alert and report the same event
twice. The circuit is a state to look at while investigating; the hold is the thing to act on.

**Suppressing the reconcile-error alert by raising its threshold instead.** Rejected. A threshold high
enough to absorb 242 errors from one outage is high enough to hide a genuine persistent failure. The
`unless`-style exclusion removes exactly the windows the breaker already explains and, when the circuit
metric is missing, removes nothing at all.

## Residual risks

**The size-measurement controller is not gated by the breaker.** It runs on its own queue and lists
objects on the data plane with the admin credential while the control plane is held. Its cadence is
bounded by the measurement interval floor, so the volume is small, but it is not zero, and a data-plane
outage sharing the same IP-level rate limit would not be held. Accepted; see
[ADR 0014](0014-bucket-size-is-measured-by-a-separate-controller.md).

**A probe is a real reconcile, not a cheap health check.** The first reconcile admitted after a
cooldown runs the full provisioning pass for whichever bucket the queue hands over. If that bucket has
a definitive local fault it neither closes nor re-opens the breaker (D3), and the breaker learns
nothing from that pass.

**The threshold counts reconciles, not buckets.** With a single bucket in the cluster, three
consecutive failures of that one bucket open the circuit fleet-wide, which for a fleet of one is the
same thing. In a small fleet the breaker is therefore more sensitive than the wording "fleet-wide"
suggests.

**The breaker lives in the operator process.** It does not survive a restart, so a crash-looping
operator re-learns the outage from scratch each time and can, in the limit, reproduce the traffic the
breaker exists to suppress.

**An alert can miss a short trip.** D11's exclusion depends on the circuit gauge being scraped while
the circuit is open. With a scrape interval coarser than the fixed 60-second base cooldown, a single
short trip may be missed and the reconcile-error alert fires as it did before — the fail-open
direction, and the intended one.

Not verified: the breaker has not been exercised against a real provider outage since it was built —
its behaviour is established by offline tests and by reasoning from the 2026-09-02 log and metric
record, not by a repeat incident. The claim that the doubling cooldown and the workqueue pacing
together keep the operator below the provider's IP-level rate limit is likewise a reconstruction from
that one measurement; the limit's actual thresholds are not published and were not measured. The
teardown behaviour under an open circuit (finalizer retained, deletion resuming at the next probe) is
verified offline only. Two operator replicas each holding a private breaker is inferred from the design
and has not been observed, because leader election means only one replica reconciles.

## References

* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — the decision this one amends: which
  failures are definitive, and the degradation grace that keeps running while the circuit is open
* [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) — the other place where an
  unanswered call blocks a destructive step rather than permitting it
* [ADR 0014](0014-bucket-size-is-measured-by-a-separate-controller.md) — the measurement controller
  that runs outside the breaker's gate
* [docs/operations/provider-outages.md](../operations/provider-outages.md) — what an operator sees
  and what to tune during an outage
* [docs/operations/monitoring.md](../operations/monitoring.md) — the metric catalogue and the two
  retuned alerts
* [docs/operations/configuration.md](../operations/configuration.md) — the settings in context
* [docs/developer/circuit-breaker.md](../developer/circuit-breaker.md) — the mechanism: state,
  counting, probe scheduling, the workqueue limiter and the transport changes
* [docs/developer/provider-errors.md](../developer/provider-errors.md) — how a provider error is
  recognised and classified before it ever reaches the breaker
