# Ticket: the cold-start sweep stays under the provider rate limit

**Status:** Draft — evidence complete, four options designed and judged, favourite named with its
reasoning. **The decision is not taken and nothing is approved for implementation.**
**Scope:** this repo (`stackit-s3-provisioner`).
**Relates to:**
[ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) — D9 is the rule whose scope this
ticket has to correct, whichever option wins;
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) — D2 forecloses one of the four
options outright;
[ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) — D1 and D7, the second
of which is simply not running today;
[ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md) — D5's proof-of-resync counter
is a casualty of one implementation shape;
[ticket 008](008-decide-bucket-existence-with-a-per-bucket-read.md) — its scope is one of the work
items below, and neither ticket blocks the other.
**Date:** 2026-09-21

---

## Why this exists

On **2026-09-21** the production management cluster hit the provider's IP-level rate limit during
the first reconcile sweep after an operator upgrade. The provider was healthy. The load was the
operator's own cold start. The episode self-healed in 63 seconds and no `Bucket` left `Ready`, so
nothing broke — but it falsified a standing claim in
[ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md), and it did so in a case that record
does not cover.

That claim, from ADR 0013's own residual risks:

> The claim that the doubling cooldown and the workqueue pacing together keep the operator below the
> provider's IP-level rate limit is likewise a reconstruction from that one measurement; the limit's
> actual thresholds are not published and were not measured.

There is now a second measurement, and for the cold start the reconstruction does not hold.

### What happened, in order

Flux upgraded the operator from `1.15.1` to `1.19.2`. Times are UTC, read from the operator log of
the pod that is still running and from the Kubernetes events, which were still inside their
retention window when this was written.

| Time | Event |
| --- | --- |
| 07:48:51 | New pod starts, version `1.19.2`, `driftResyncInterval=10m`, `providerCircuitThreshold=3` |
| 07:49:04 | Leader lease acquired; bucket controller starts with `worker count: 1` |
| 07:49:09–07:49:36 | Ten `Bucket`s complete a pass and log `bucket verified after operator start`, ~2.7 s apart |
| 07:49:37.833–.872 | Three `429 Too Many Requests` — `rate limit on IP level exceeded` — on three different `Bucket`s |
| 07:49:37.882–.960 | Nine further `Bucket`s are held by the now-open circuit and marked degraded **without making a single provider call** |
| 07:50:40–07:51:12 | All twelve reconcile successfully and log `changes: ["recovered to Ready"]` |
| 07:59:13+ | Next drift resync, quiet |

Ten verified plus twelve recovered is the whole fleet of twenty-two. `Ready` never went to `False`
on any of them: the degradation path of
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) held, and the recovery reported
itself because of [ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md) D2, which
shipped one day earlier in `v1.19.0`. The machinery behaved as specified throughout.

**The report is larger than the failure.** Three `Bucket`s failed; twelve reported recovery. A
`Bucket` held by an open circuit is marked degraded by
[`holdForProvider`](../../internal/controller/bucket_controller.go#L248) →
[`degrade`](../../internal/controller/bucket_controller.go#L1791) without reaching the provider, and
`recovering` at
[bucket_controller.go:1861](../../internal/controller/bucket_controller.go#L1861) then reads that
state on the next successful pass. Anyone reading such a burst as a count of failures will
overcount, and this is written down nowhere.

---

## Problem 1 — the cold-start sweep passes through no pacing at all

`bucketRateLimiter` at
[bucket_controller.go:1963](../../internal/controller/bucket_controller.go#L1963) paces error
requeues, and says so in its own doc comment: watch events use `Add` and `RequeueAfter` uses
`AddAfter`, neither of which is rate limited. The informer's initial list arrives as `Add`, so the
first sweep after every process start travels through no limiter and no jitter.

ADR 0013 D9 states that exemption as a decision:

> Watch-driven and timer-driven reconciles do not pass through the failure backoff, so drift resync
> keeps its timing.

**It is not a decision, it is inherited.** `controller.Options.RateLimiter` in
controller-runtime `v0.25.1` has no configuration that would pace a watch-driven `Add`. The record
claims authorship of a framework property. That sentence has to be corrected whichever option below
is chosen.

### What a pass costs today

Steady-state path, a provisioned `Bucket` with no read grants and no clone. Verified by reading the
call sites, not measured against the live API.

| # | Call | Site | Plane | Avoidable |
| --- | --- | --- | --- | --- |
| 1 | `GetBucket` (presence guard) | [`guardBucketPresent`](../../internal/controller/bucket_controller.go#L666) | control | no — it is the guard of [ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) |
| 2 | `ListBuckets`, project-wide | [`ensureBucket`](../../internal/controller/bucket_controller.go#L750) → `HasBucket` | control | **yes** — this is [ticket 008](008-decide-bucket-existence-with-a-per-bucket-read.md) |
| 3 | `GetBucket` | [`BucketConnInfo`](../../internal/controller/bucket_controller.go#L387) | control | **yes, as a duplicate of 1** |
| 4 | `GetBucket` | [`adoptOrCollide`](../../internal/controller/bucket_controller.go#L797) → `newS3Admin` | control | **yes, as a duplicate of 1** |
| 5 | `ListCredentialsGroups`, project-wide | [`listGroups`](../../internal/controller/bucket_controller.go#L530) | control | partly — see work item W3 |
| 6 | `GetBucket` | [`resolveWorkloadGroup`](../../internal/controller/bucket_controller.go#L986) → `newS3Admin` | control | **yes, as a duplicate of 1** |
| 7 | `ListAccessKeys` | [`groupExists`](../../internal/controller/bucket_controller.go#L1140) | control | **yes, as a duplicate of 9** |
| 8 | `GetBucket` | [`ensureBucketPolicy`](../../internal/controller/bucket_controller.go#L1362) → `newS3Admin` | control | **yes, as a duplicate of 1** |
| 9 | `ListAccessKeys` | [`ensureAccessKeyAndSecret`](../../internal/controller/bucket_controller.go#L1192) | control | no |
| — | `GetBucketTagging` ×2, `GetBucketPolicy` ×1 | various | data | no |

**Nine control-plane requests, of which five are the identical `GetBucket` for the same bucket.**
Four of those five exist only to re-derive an endpoint:
[`newS3Admin`](../../internal/controller/bucket_controller.go#L828) calls `BucketEndpoint`, which
calls `BucketConnInfo`, which calls `GetBucket`
([stackit/client.go:572](../../stackit/client.go#L572),
[stackit/client.go:590](../../stackit/client.go#L590)). The value is the bucket's path-style URL,
which cannot change inside one pass. Nothing caches it.

Observed during the incident: ~2.7 s per `Bucket`, one worker, so roughly **3.3 control-plane
requests per second sustained**, and ~91 requests in the 32 seconds before the first `429`.
`MaxConcurrentReconciles` is left at the framework default of 1 for this controller — set only for
the usage controller, at
[bucket_usage_controller.go:444](../../internal/controller/bucket_usage_controller.go#L444).

The usage controller contributed nothing on 2026-09-21: every `status.usage.lastMeasurementTime` in
the fleet predated the restart and none was due at a 6 h interval, so its pass returned before any
cloud call.

---

## Problems that need no decision

These are repairs of documented behaviour or plain staleness. They are listed here because they were
found in the same investigation and because two of them change the numbers any pacing decision is
dimensioned against.

| ID | Problem | Where |
| --- | --- | --- |
| **P1** | **The usage controller's anti-herd skew does not run.** `int64(h.Sum32()) % int64(spread)`: the dividend is at most 4 294 967 295 ns ≈ 4.29 s, the divisor at a 6 h interval is 2 160 000 000 000 ns. The modulus never wraps, so the spread is the raw hash read as nanoseconds — **4.09 s measured across the twenty-two live bucket names, where the intended figure is 36 minutes**. The modulus only bites below a ~43 s interval, and the interval floor is 1 h, so it is dead in every valid configuration. The comment above it names the exact reason it fails while presenting it as the reason it is safe. This is the only anti-thundering-herd mechanism in the operator. | [bucket_usage_controller.go:405](../../internal/controller/bucket_usage_controller.go#L405) |
| **P2** | **`TestNextMeasurementSpreadsBuckets` passes anyway**, because it asserts only that two keys differ and that the result stays under an upper bound. It needs a **lower** bound. | [usage_config_test.go:204](../../internal/controller/usage_config_test.go#L204) |
| **P3** | **The documented per-pass cost is one request short.** `docs/operations/configuration.md` says "eight control-plane requests"; the path costs nine since the presence guard was added in `v1.16.0`. Every pacing number below is dimensioned against this figure. | [configuration.md:358](../operations/configuration.md#L358) |
| **P4** | **The same paragraph says "the traffic is steady rather than spiky"**, which is falsified for the first ~32 s of every process. The startup profile is documented nowhere. | [configuration.md:358](../operations/configuration.md#L358) |
| **P5** | **The operator counts zero provider requests.** There is no request counter, no per-plane split, no status-code label. A `429` the breaker absorbs leaves no trace at all, and every number in this ticket is therefore derived from log timestamps and source reading. The control plane has exactly one chokepoint, `retryTransport.RoundTrip`; the data plane gets one by having `newS3Client` set a shared transport, which also removes the three TLS handshakes a pass currently pays for three data-plane requests. | [stackit/retry.go](../../stackit/retry.go) |
| **P6** | **Two records are overtaken by the incident.** ADR 0013's residual risks say the breaker "has not been exercised against a real provider outage since it was built"; ADR 0017's status says it was "not run against a live cluster". Both were exercised live on 2026-09-21 and behaved as specified — with the nuance that this was not a provider outage but a self-inflicted rate limit, a case neither record claims to have tested. | [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md), [ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md) |

---

## The decision to take

### The conflict, named

Per the ADR obligations in [CLAUDE.md](../../CLAUDE.md), the collision is stated before anything is
built. Three sentences of ADR 0013 D9 are load-bearing, and they collide differently.

**D9, first sentence — scope.**

> The operator paces its own **retries** fleet-wide, so that **a provider failure** cannot be turned
> into a rate limit before the breaker even trips.

Two words carry the scope: *retries* and *a provider failure*. 2026-09-21 was neither. **D9 does not
forbid a cold-start pacer; D9 does not reach it.** What it does claim — fleet-wide pacing — is
broader than what it delivers, and that is the part every option has to correct.

**D9, last sentence — the exemption.**

> Watch-driven and timer-driven reconciles do not pass through the failure backoff, so drift resync
> keeps its timing.

Any structurally clean pacer — a limiter in the transport, a wrapper on the queue — necessarily
paces drift resync and teardown too, and breaks this sentence outright. A pacer that touches only
the cold start breaks it by phase, not by period.

**D9, middle clause — the enumeration.**

> backing off from a rate limit belongs to **the workqueue and to the breaker**, which act on the
> whole fleet rather than on one in-flight request

A limiter inside the provider client is a third organ. It satisfies the stated *reason* — it sees
the whole fleet, which is precisely the test `Retry-After` failed — while falsifying the stated
*enumeration*. A record must either extend that list or place the pacer in one of the two named
organs.

**Already refused, not reopened here.** An error taxonomy (rejected twice: ADR 0012 alternatives and
ADR 0013 D2), honouring `Retry-After` (ADR 0013 alternatives), per-bucket breakers, and a per-`Bucket`
pacing field (ADR 0012 D5's operator-wide precedent). A pacer may be **unconditional** or
breaker-driven. A mechanism that slows down *because it saw a `429`* is an error classifier under a
different name.

**One option needs no amendment beyond the D9 scope correction.** Removing duplicate calls
contradicts no rule at all.

### The options

| | What it does | Effect | ADR cost | New settings | Worst failure mode |
| --- | --- | --- | --- | --- | --- |
| **A** | Ask each provider question once per pass: five `GetBucket` → one, two `ListAccessKeys` → one, project-wide `ListBuckets` → per-bucket read, `ListCredentialsGroups` only when needed | 198 → 66 requests per sweep; rate 3.3 → **~2.7/s**; headroom per 10 m resync ~222 → ~530 `Bucket`s | corrects D9's scope; **contradicts no rule** | **none** | a memo keyed wrongly puts bucket A's endpoint in bucket B's workload Secret — silent, invisible to every metric, security-shaped. Invalidation sits on the teardown path |
| **B** | A startup window in the reconciler: the *first* pass per `Bucket` per process takes an assigned slot, bounded by `driftResyncInterval / n`. Exempt: never provisioned, unseen spec, deletion, breaker probe | worst 60 s window 198 → 54 requests; **the rate inside one pass stays 3.3/s** | amends D9 by phase, not by period | 1 | the bound switches the mechanism off above ~222 `Bucket`s — it disappears exactly where fleets grow into it. A flapping lease never reaches the late `Bucket`s |
| **C** | Two process-wide token buckets, control and data plane, in front of every provider request, blocking inside the pass | the only real ceiling: covers teardown, mass create, the transport's 3× retry and minio-go's 10 retries on `429` | **deletes** D9's last sentence, extends the enumeration, **contracts [ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D1** | 5, two of them undimensionable | a silently throttled fleet: `lastVerifiedTime` ages everywhere, nothing is in error, **nothing alerts**. Pacing is silent by design (D4) |
| **D** | AIMD: halve the rate on a `429`, recover on a timer | a feedback loop instead of a guess | **breaks [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D2** — the only option that breaks a rule rather than extending one | 4 plus 4 fixed in code | the loop cannot converge behind shared NAT: its input is not a function of its output. At the floor, 500 `Bucket`s is 3 h 20 m per sweep with no event, no condition, no alert |

### Favourite: A, and why

**The bind A dissolves:** every pacing number — C's requests per second, B's interval, D's floor and
ceiling — is dimensioned against a quantity that is unpublished, never measured, and shared with
workloads behind the same NAT address that the operator cannot see. **The operator counts zero
provider requests today** (P5). C and D ask for a number to be signed off now that only becomes
measurable after it ships, and C's default would make the operator *more* fleet-limited than it is
today — roughly 200 `Bucket`s per resync interval against ~222 now — in a mode no condition and no
alert reveals.

A needs no number. It removes requests that are provably redundant, which is as correct on a fleet
of five as on a fleet of five hundred, and it turns the refusal of a pacer into a **recorded,
conditional** decision rather than an omission: its record names the observation that overturns it —
a `429` while the operator's own measured traffic sits inside its budget.

**What A gives up, without varnish.** The rate falls by a fifth, not by two thirds. If the limit is
rate-shaped and sits below ~2.7/s, 2026-09-21 repeats — 25 seconds into the sweep instead of 33.
Afterwards **no** mechanism bounds requests per second: not on mass delete, not on the transport's
3× retry amplification, not on minio-go's ten retries against a `429`. And A raises the temptation to
lift `MaxConcurrentReconciles` as a performance fix; at four workers that is ~10.6 requests per
second, three times what drew the `429`. That setting does not exist today and should not be created.

**B is the honest runner-up** and the operationally safest object here: it adds no failure mode on a
large fleet because it switches itself off there. If the answer to Q3 is "the next rollout must be
quiet", **A+B together** is the right combination; they do not exclude each other and B costs one
setting whose default is a guess.

---

## Work items

Ordered. W1–W3 are option A. W4–W6 need no decision. W7 is the record.

| ID | Work | Depends on |
| --- | --- | --- |
| **W1** | Resolve the bucket endpoint once per pass and hand it to every `newS3Admin`, `BucketConnInfo` and presence-guard caller inside that pass. Five `GetBucket` → one. The memo is per-pass, never per-process: a per-process cache is the security-shaped failure mode named above | decision |
| **W2** | Call `ListAccessKeyIDs` once per pass — [`groupExists`](../../internal/controller/bucket_controller.go#L1140) and [`ensureAccessKeyAndSecret`](../../internal/controller/bucket_controller.go#L1192) ask the same question of the same group | decision |
| **W3** | Replace the project-wide `HasBucket` at [bucket_controller.go:750](../../internal/controller/bucket_controller.go#L750) with the per-bucket read, and make `listGroups` lazy. **This is [ticket 008](008-decide-bucket-existence-with-a-per-bucket-read.md)'s scope** — the two tickets are coordinated, not merged, and 008 carries the pagination and definitiveness argument that this one does not repeat | decision, ticket 008 |
| **W4** | Fix the skew arithmetic (P1) and give the test a lower bound (P2). Restores [ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D7 to running | none |
| **W5** | Add `stackit_s3_provisioner_provider_requests_total{plane,operation,code}` and a duration histogram (P5). Every one of the four options ships this and no rule opposes it; it is what turns the next incident from inference into measurement, and it is the precondition for ever revisiting the pacer with data | none |
| **W6** | Correct the per-pass cost and the "steady rather than spiky" claim in `docs/operations/configuration.md` (P3, P4); document the startup profile and the fact that a recovery burst overcounts failures | none |
| **W7** | Write ADR 0018 with the implementation, per [docs/tickets/README.md](README.md): the decision taken, the three alternatives rejected with their reasons, and the correction of ADR 0013 D9's scope. `0018` is free — verified 2026-09-21. ADR status vocabulary is `Accepted` / `Superseded by ADR NNNN` / `Amended`; `Proposed` is not a permitted value | decision |

## Success criteria

| ID | Criterion |
| --- | --- |
| S1 | A cold start of the mgmt-p fleet completes without a `429`, and the new request counter proves the achieved rate rather than the log implying it |
| S2 | The per-pass control-plane count on the steady-state path is asserted by a test against the offline fake, so a future call added back is caught in CI rather than on a cluster |
| S3 | `nextMeasurement` spreads measurements across at least a quarter of the interval, asserted by a lower bound over a realistic set of bucket names |
| S4 | ADR 0013 D9's scope reads correctly: it paces retries, it does not reach first requests, and the exemption of watch-driven reconciles is stated as a framework property rather than a choice |
| S5 | An operator reading a `recovered to Ready` burst can find, in `docs/operations/`, that the burst counts held `Bucket`s and not failures |

## Open questions

| ID | Question | Why it changes the work |
| --- | --- | --- |
| **Q1** | Does mgmt-p egress through shared NAT, and can we reach provider-side or NAT-side counters? | If not, attributing the `429` is unanswerable from inside the pod: option D's control loop is then structurally broken and option C's number is permanently uncovered. If we can reach counters, option A's overturning trigger becomes meaningful |
| **Q2** | What fleet size is one instance heading for in the next twelve months? | Below ~220 `Bucket`s the cold start is a one-off event and A suffices, with headroom to ~530. Above it the sweep is the steady state, B switches itself off there, and C changes from a precaution into a brake |
| **Q3** | May 2026-09-21 repeat once more — 63 s, self-healed, no `Ready` going `False` — while we measure? | Yes means A alone now and the pacer decided in six weeks with data. No means A+B now, accepting that B's interval default is a guess to be corrected later |

---

## Deliberately left out

**Two latent defects found in the same investigation. They are recorded here so they are not lost,
and they are explicitly not part of this ticket — each needs its own record.**

- **A destroyed workload credential feeds the fleet-wide breaker.** `errCredentialDestroyed` arises
  between `DeleteAllAccessKeys` and `CreateAccessKey`
  ([bucket_controller.go:1205](../../internal/controller/bucket_controller.go#L1205)); a `429` on a
  POST is never retried ([stackit/retry.go:155](../../stackit/retry.go#L155)). The error reaches
  `fail()`, which calls `Breaker.Failure()`
  ([bucket_controller.go:1747](../../internal/controller/bucket_controller.go#L1747)), and
  `providerHoldNeedsWrite` returns `false` for `Phase == Failed`, so the repair pass is held
  silently for up to the maximum cooldown. `docs/operations/credentials.md` promises the opposite:
  "let the next pass republish". Not triggered on 2026-09-21 — every `Bucket` took the cheap branch
  and no pass issued a credential. Whether this belongs on the `failNoRequeue` side of the fence or
  needs a gate exemption is an ADR question against 0012 and 0013, not a quiet fix.
- **`observedGeneration` is stamped on a pass that made no provider call.** With the circuit open and
  a spec change pending, `degrade` declines, `markFailedReason` sets
  `ObservedGeneration = Generation`
  ([bucket_controller.go:1890](../../internal/controller/bucket_controller.go#L1890)) on a pass that
  returned before `reconcileNormal`. `docs/operations/bucket-status.md` tells the reader that
  equality means the operator has processed what was applied. The pass that later does apply it then
  reports only `recovered to Ready`, which contradicts ADR 0017 D2's own table row. Latent: every
  `Bucket` on mgmt-p had `generation == observedGeneration` throughout.

**Not in scope either:** raising `MaxConcurrentReconciles`, honouring `Retry-After`, and any
classification of provider errors. The first multiplies the problem, the other two are refused by
existing records.

---

## What was verified, and how

Read-only against the production management cluster on **2026-09-21**, and by reading the source at
commit `f5bb604`.

| Claim | How |
| --- | --- |
| The timeline and the 10 / 3 / 9 / 12 split | Complete operator log of the running pod, plus the twelve `Warning` events on `Bucket` CRs, which were still within retention. Each `Bucket`'s `status.lastVerifiedTime` equals its own log line plus one resync interval, which partitions the fleet with none in both cohorts and none in neither |
| Nine held `Bucket`s were degraded without a provider call | The nine carry the breaker-hold message as their event text, emitted from inside `degrade`, which is the same call that writes `degradedSince` |
| The usage controller contributed nothing | Every `status.usage.lastMeasurementTime` predates the restart and is unchanged after it |
| Nine control-plane calls per steady-state pass, five of them the same `GetBucket` | Call sites read in `internal/controller/bucket_controller.go` and `stackit/client.go`; `BucketEndpoint` → `BucketConnInfo` → `GetBucket` confirmed |
| The second `HasBucket` is not on this path | [bucket_controller.go:1389](../../internal/controller/bucket_controller.go#L1389) sits in `teardown` |
| `MaxConcurrentReconciles` is default for this controller | Set only at [bucket_usage_controller.go:444](../../internal/controller/bucket_usage_controller.go#L444) |
| The skew is dead | Arithmetic, plus the spread computed over the twenty-two live bucket names: 4.09 s against an intended 36 minutes |
| `0018` is free, and the ADR status vocabulary | `git grep` over `docs` and `README.md`; [docs/adr/README.md](../adr/README.md) |

## Not verified

- **That the operator caused the `429`.** This is an inference from the timeline, not a measurement.
  The limit is per source IP, the cluster egresses through shared NAT, and traffic from other
  workloads is invisible from inside the pod.
- **The rate limit's thresholds and its averaging window.** Unpublished, never measured. Option B's
  entire value depends on the window being longer than one pass (~2.7 s); if it is shorter, B buys
  nothing.
- **That the limit is volume-shaped rather than rate-shaped.** Suggested by the 32 seconds of sweep
  before the first `429`, with a live alternative explanation — a NAT neighbour — that cannot be
  excluded.
- **The nine-plus-three call count against the live API.** Established by reading the source and by
  the repository's own offline fake, never counted against the provider.
- **The ~225 ms per-request latency constant**, from which every sweep duration, every rate figure
  and the ~222-`Bucket` threshold in this ticket are derived.
- **The initial-list behaviour** of controller-runtime `v0.25.1` — read from the module source, not
  observed.
- **Whether the provider sends `Retry-After` on a `429`.** Nothing in the repository reads it, the
  logged body does not contain it, and the response headers were not captured.
