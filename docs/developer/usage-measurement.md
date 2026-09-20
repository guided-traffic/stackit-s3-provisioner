# Usage measurement

This page is about the second controller in this operator: the one that measures how large a
bucket is, turns that into a monthly cost estimate, and writes both into `status.usage`. It covers
how a pass is scheduled, how it stays out of the provisioning controller's way, what the listing
actually costs and how the numbers are computed — the things you need before you change
[`internal/controller/bucket_usage_controller.go`](../../internal/controller/bucket_usage_controller.go)
or [`internal/controller/usage_config.go`](../../internal/controller/usage_config.go). If you want
to *use* the feature — switch it on, read the fields, understand what the cost figure is and is
not — you want [docs/operations/usage-and-cost.md](../operations/usage-and-cost.md) instead, and
the decision behind all of it is [ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md).

## Its own controller, queue and concurrency limit

`BucketUsageReconciler` is registered as a second controller on the same manager, next to
`BucketReconciler`, in [`cmd/main.go`](../../cmd/main.go) — unconditionally, including in skeleton
mode. It is named `bucketusage`; the provisioning controller is named `bucket`. That name is not
cosmetic: it is what keeps the two apart in `controller_runtime_reconcile_errors_total`, which the
`StackitS3ReconcileErrors` alert filters to `controller="bucket"`.

`SetupWithManager` sets `MaxConcurrentReconciles` from `UsageConfig.Concurrency` (Helm
`bucketUsage.concurrency`, `2` # default). Because the queue is its own, a bucket whose listing
takes ten minutes delays at most `concurrency - 1` other measurements and never a provisioning
pass ([ADR 0014 D1](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)).

Two things the provisioning controller has that this one deliberately does **not**:

| Mechanism | Provisioning controller | Measuring controller |
|---|---|---|
| Workqueue rate limiter | `bucketRateLimiter()` ([bucket_controller.go:1772](../../internal/controller/bucket_controller.go)) — a `MaxOf` of per-item exponential 1s→15min **and** a fleet-wide 1 qps / burst 5 bucket | controller-runtime's default, which carries no qps component at all |
| Fleet-wide provider circuit breaker | `Breaker` field, consulted every pass | no `Breaker` field at all |

The default limiter is worth being precise about, because the obvious guess is wrong. Verified in
controller-runtime v0.25.0 (`pkg/controller/controller.go`, the version pinned in
[`go.mod`](../../go.mod)): when `Options.RateLimiter` is nil the default depends on
`UsePriorityQueue`, which itself defaults to **true**. On that branch the limiter is a bare
per-item exponential 5ms→1000s; only the non-priority-queue branch uses
`DefaultTypedControllerRateLimiter()`, the `MaxOf` that carries a fleet-wide token bucket. Nothing
in this repository sets `UsePriorityQueue`, so the measuring controller's queue has **no fleet-wide
qps pacing whatsoever**.

In practice the backoff half barely matters: `Reconcile` returns an error only on an API-server read
failure (see [Failure handling](#failure-handling-and-why-it-never-reaches-readiness)), so the
failure backoff is effectively unused, and every delay this controller produces is a `RequeueAfter`,
which is `AddAfter` and is not rate limited. The missing qps bucket and the missing breaker do
matter: see [What is wrong today](#what-is-wrong-today).

## One pass, in order

`Reconcile` ([bucket_usage_controller.go:101](../../internal/controller/bucket_usage_controller.go))
is short on purpose; everything that decides *not* to measure lives in `precheck`, and every one of
those exits is free of cloud calls, which is what makes the short retry delays affordable.

| Step | Condition | What happens |
|---|---|---|
| 1 | `DeletionTimestamp` set | return, no write. Teardown has its own emptiness check and a listing pass would only race it ([ADR 0014 D6](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)) |
| 2 | `eff.err != nil` (unusable `spec.usage.interval`) | warning event `UsageConfigInvalid`, `patchUsageParked`, stop with no requeue |
| 3 | `!eff.enabled` and the CR asked explicitly | warning event `UsageMeasurementDisabled`, `patchUsageParked`, stop with no requeue |
| 4 | `!eff.enabled` and the CR did not ask | `clearUsage`, stop with no requeue |
| 5 | `r.Stackit == nil` (skeleton mode) | stop, no write, no requeue |
| 6 | `status.resolvedBucketName` empty or `Ready` not true | requeue after `usageNotProvisionedRetry` (1 minute) |
| 7 | not due yet | requeue after `untilDue(...)` |
| 8 | admin Secret unreadable | requeue after `usageNotProvisionedRetry` (1 minute) |
| 9 | measurement failed | `recordFailure`: event, counter, message patch, requeue after `min(interval, 10m)` |
| 10 | measurement succeeded | duration histogram, truncation/clamp warnings, `patchUsage`, requeue after `nextMeasurement(...)` |

Steps 2–5 return `ctrl.Result{}` with no requeue at all. A bucket parked in one of those states is
woken only by a generation or annotation change — which is correct, because nothing else can change
the outcome, but it means flipping the operator-wide gate back on does not clear a parked message
until the next restart or the next spec edit.

`measure` performs exactly two provider operations per due bucket: one control-plane `GetBucket`
through `Client.BucketEndpoint` ([`stackit/client.go:411`](../../stackit/client.go)) to learn the S3
endpoint, then the listing itself through a freshly built `S3Admin`. The whole pass runs under
`context.WithTimeout(ctx, eff.interval)`: a measurement that cannot finish before its own next run
is worthless and would otherwise hold a concurrency slot forever.

## The merge patch, and the optimistic-concurrency counterpart

Every write this controller makes is `r.Status().Patch(ctx, b, client.MergeFrom(b.DeepCopy()))`
against `status.usage` and nothing else. There are exactly four of them and no `Update` anywhere in
the file — that is the mechanism behind [ADR 0014 D2](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md),
and it is the thing to preserve if you touch this file.

| Function | Values | Message | Used for |
|---|---|---|---|
| `patchUsage` | fresh | truncation or clamp note, else empty | a completed measurement |
| `patchUsageMessage` | previous values kept | failure reason | a **transient** failure — it will succeed again ([ADR 0014 D4](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)) |
| `patchUsageParked` | dropped | why it is parked | nothing will refresh these numbers ([ADR 0014 D11](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)) |
| `clearUsage` | `status.usage` set to `nil` | — | this bucket is no longer measured |

The difference between `patchUsageMessage` and `patchUsageParked` is the whole of D4 versus D11: a
stale size with a message saying why it is aging is useful; a stale size that nobody will ever
refresh asserts a currency it does not have. `patchUsageMessage` and `patchUsageParked` both
short-circuit when the status already says exactly that, so a permanently failing bucket does not
produce a status write per pass.

Because `UsageStatus` marks every field `omitempty`, a JSON merge patch computed from the deep copy
emits `null` for a field that was set before and is unset now, so switching version counting off,
or a measurement dropping to zero objects, actually removes the old value rather than leaving it
behind.

The reverse direction — the provisioning controller clobbering a measurement — is covered by
optimistic concurrency rather than by a patch: `BucketReconciler` writes status with
`r.Status().Update`, which carries a `resourceVersion`, so a measurement landing mid-flight makes
that `Update` conflict and the pass retry. Changing any of those `Update` calls in
[`bucket_controller.go`](../../internal/controller/bucket_controller.go) or
[`clone.go`](../../internal/controller/clone.go) to a patch would silently remove that protection.

## Scheduling: the due time lives in the status

There is no timer and no ticker. `untilDue` reads `status.usage.lastMeasurementTime`, adds the
effective interval and returns what is left; a bucket that was never measured is due immediately.
The next pass is booked by the `RequeueAfter` the current pass returns
([ADR 0014 D7](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)).

The consequence is the point: after an operator restart the watch delivers a Create event for every
`Bucket`, each one runs `precheck`, and each one either measures (because it was genuinely due while
the operator was down) or requeues for the remainder of its interval. The schedule is reconstructed
from the resource, not from process memory, and an operator restart costs nothing.

`nextMeasurement` adds a deterministic per-bucket skew of up to one tenth of the interval, derived
from an FNV-1a-32 hash over `namespace + "/" + name`
([bucket_usage_controller.go:405](../../internal/controller/bucket_usage_controller.go)). It is
deterministic so the same bucket always lands in the same slot, and it exists because buckets all
come due together after a restart. Read the comment there before you "improve" it: the skew is not
the safety mechanism, the concurrency limit is. The skew only stops the fleet from staying in
lock-step forever.

The watch is filtered with `predicate.Or(GenerationChangedPredicate, AnnotationChangedPredicate)` —
the same pair `BucketReconciler` uses, for the same reason, and here it is load-bearing rather than
merely tidy: this controller writes status on every successful pass, and an unfiltered watch would
turn that write into the trigger for the next pass. That is a hot loop against the provider, not
just against the API server.

## Reading the admin credential, never creating it

`readAdmin` does a plain `Get` on the operator-owned admin Secret named by `AdminSecretName` in
`AdminSecretNamespace`, and runs it through the shared `adminFromSecret` helper. It fails — rather
than bootstrapping — when the operator namespace is unknown (`--operator-namespace`, which defaults
to `POD_NAMESPACE`; `readAdmin`'s own error text names only the env var) or the Secret is
missing or incomplete, and the caller turns
that into a one-minute requeue ([ADR 0014 D5](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md);
the credential itself is [ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md),
and how it is minted is in [credentials.md](credentials.md)).

This is the one privilege-relevant line in the subsystem. The measuring path holds the most
privileged credential in the system, and the only thing stopping an informational feature from
minting cloud credentials as a side effect is that `readAdmin` contains no create path. Keep it
that way.

A coupling worth knowing before you refactor: the kubebuilder RBAC markers on this controller
([bucket_usage_controller.go:93-94](../../internal/controller/bucket_usage_controller.go)) cover
`buckets` and `buckets/status` only. The Secret read and the events it emits ride on the markers
declared on `BucketReconciler`, which are cluster-wide. Narrowing those markers breaks `readAdmin`
with no compile-time signal.

Note also that a fresh `S3Admin` is built per pass from the Secret contents rather than cached, so a
rotated admin key takes effect on the next measurement with no invalidation logic.

## What a listing costs, and what the cap does

`S3Admin.BucketStats` ([`stackit/s3.go:559`](../../stackit/s3.go)) is the whole measurement. There
is no cheaper way: the Object Storage control-plane API has no usage, statistics or size endpoint
(verified against `objectstorage` SDK v1.9.1, whose surface is buckets, credentials groups, access
keys, service enablement, retention and compliance locks), so a size is the sum over one full
listing. `BucketStats` sets no `MaxKeys`, and minio-go v7.3.0 only sends a `max-keys` parameter when
`MaxKeys > 0`, so the request carries none and the **S3 protocol default of 1000 keys per page**
applies — that, not a minio setting, is where "roughly one request per 1000 keys" comes from. Two
million objects is about 2000 requests.

That costs **time**, not money. Verified on 2026-09-01 against the published price list v1.0.43
(dated 2026-04-08) and Object Storage service description v1.2 (valid from 2026-03-26): billing is
per started gigabyte per started hour and the price list carries no request, operation, traffic or
egress position. Every guard in this subsystem is therefore a guard on time
([ADR 0014 D8](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)), and the numbers
themselves live in [docs/operations/usage-and-cost.md](../operations/usage-and-cost.md).

`maxEntries` (Helm `bucketUsage.maxObjects`, `2000000` # default) counts **listing entries
consumed**, not objects. Under version counting a delete marker and a non-current version each
consume one entry, so the cap bites earlier on a versioned bucket than the name suggests. On hitting
it the loop sets `Truncated` and breaks; the deferred `cancel()` on the listing context is what stops
minio's producer goroutine from blocking on the now-unread channel — do not remove it when you touch
the loop. A value `<= 0` means no cap at all; `UsageConfig.withDefaults` normalises a negative value
to `0`, it does not restore the default.

Truncation is rendered in exactly two places. `formatSize` and `formatCost`
([usage_config.go:171-192](../../internal/controller/usage_config.go)) prepend `">= "`, and they are
called only for `UsageStatus.HumanReadable` and `UsageStatus.EstimatedMonthlyCost`. Every
machine-readable field — `bytes`, `objects`, `versionBytes`, `versionObjects`, `billableBytes`,
`estimatedMonthlyCostCents` — and every gauge in
[`metrics.go`](../../internal/controller/metrics.go) carries the lower bound plain, with
`status.usage.truncated` and `stackit_s3_provisioner_bucket_usage_truncated` as its marker
([ADR 0014 D9](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)). If you add a
consumer of those numbers, it has to read the truncation flag alongside them.

## Versions in one listing pass; multipart uploads in none

`includeVersions` switches minio's `ListObjectsOptions.WithVersions`, which returns current objects,
non-current versions and delete markers in the same pass. The classification in `BucketStats` is a
three-way switch and the order matters:

| Entry | Counted as |
|---|---|
| `obj.IsDeleteMarker` | `VersionObjects++`, no bytes — a delete marker has none of its own |
| `includeVersions && !obj.IsLatest` | `VersionObjects++`, `VersionBytes += obj.Size` |
| anything else | `Objects++`, `Bytes += obj.Size` |

The second arm is gated on `includeVersions` because `IsLatest` is only populated by the version
listing; without that gate a plain listing would classify every entry as non-current. Verified live
on 2026-09-01 against StorageGRID: a version listing requested on a bucket that was **not**
versioned is answered normally, the live object arrives with `IsLatest=true`, lands in the current
counters, and `versionBytes`/`versionObjects` stay zero — the path does not run dry and nothing is
double counted.

Where the value comes from is resolved in `UsageConfig.effectiveFor`
([usage_config.go:136](../../internal/controller/usage_config.go)) as
`spec.UsageIncludeVersions(cfg.IncludeVersions)`: `api/v1/bucket_types.go` returns the operator-wide
default whenever the CR leaves the field unset, and that default comes from Helm
`bucketUsage.includeVersions` (`false` # default)
([ADR 0014 D12](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)).

Incomplete multipart uploads are never counted, in either mode. They occupy billed storage, so the
measurement is a floor even with versions on; counting them needs a third listing (`ListMultipartUploads`),
which is not implemented.

## The cost computation

`estimateMonthlyCostCents` ([usage_config.go:163](../../internal/controller/usage_config.go)) is four
lines and every one of them is a decision:

```go
startedGB := (billableBytes + bytesPerGB - 1) / bytesPerGB   // round UP, billing is per STARTED GB
cents := float64(startedGB) * pricePerGBHour * billingHoursPerMonth * 100
return int64(math.Round(cents))
```

* `bytesPerGB = 1_000_000_000` — a **decimal** gigabyte, because that is the billing unit.
* `billingHoursPerMonth = 720` — the 30-day month the provider's own price list projects its monthly
  column with, so the estimate is comparable to the published figure.
* A price `<= 0` **or** a size `<= 0` makes the function return `0`. Those two zeroes are not the
  same thing downstream, and the difference is easy to get wrong — see below.

`EstimatedMonthlyCostCents` is the canonical value and `EstimatedMonthlyCost` is its rendering
([ADR 0014 D13](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)); the metric
converts back by dividing by 100 rather than recomputing. `Currency` is a display label and converts
nothing.

**Zero price and zero size are different states.** `patchUsage`
([bucket_usage_controller.go:304](../../internal/controller/bucket_usage_controller.go)) gates the
whole cost block on `cfg.PricePerGBHour > 0` alone; the measured size is not part of that condition.

| State | `estimatedMonthlyCostCents` | `estimatedMonthlyCost` | `currency` | `..._bucket_estimated_monthly_cost` |
|---|---|---|---|---|
| No price configured (or `<= 0`) | absent | absent | absent | **absent** |
| Price configured, bucket empty | absent (`omitempty` on `0`) | `"0.00 EUR"` # example | `EUR` # default | **emitted, value `0`** |
| Price configured, bucket non-empty | the estimate | its rendering | `EUR` # default | the estimate |

The gauge is emitted whenever `u.Currency != ""` ([metrics.go:322](../../internal/controller/metrics.go)),
and a price-configured empty bucket does set a currency. So
`absent(stackit_s3_provisioner_bucket_estimated_monthly_cost)` is a probe for "no price configured",
**not** for "no cost" — an empty measured bucket will not trip it. `estimatedMonthlyCostCents` is
`omitempty` on an `int64`, so a genuine zero disappears from the status while
`estimatedMonthlyCost` still renders it; read the currency field, not the cents field, to tell the
two states apart on a CR. D13 speaks to the price only; the size row above is what the code does,
verified here against `patchUsage` and `collectUsage` rather than restated from the record. The only
test on this path, `TestUsageWithoutPriceOmitsCost`
([reconciler_usage_test.go](../../internal/controller/reconciler_usage_test.go)), switches off the
**price**, not the size — the middle row is untested.

**A unit trap to know about.** The cost path is decimal (GB), the display path is binary:
`formatSize` delegates to `humanBytes`, which lives in
[`clone.go:579`](../../internal/controller/clone.go) and renders KiB/MiB/GiB. So a bucket holding
5 MiB shows `5.0 MiB` in the `Size` column and is priced as one whole started gigabyte. That is
correct in both halves and confusing in combination; it was verified against a real listing on
2026-09-01 (5 MiB written, `bytes=5242880`, `objects=1`, `0.03 EUR`). Note also that `HumanReadable`
renders `BillableBytes`, not `Bytes`, so the `Size` column includes counted versions while the
`Objects` column does not.

The price itself is parsed once at startup by `parseUsagePrice` in
[`cmd/main.go`](../../cmd/main.go): it is taken as a **string** so YAML cannot turn
`0.00003697772` into an exponent, an unparseable value exits the process with a clear message rather
than silently costing zero, and a negative one is rejected.

## Failure handling, and why it never reaches readiness

`Reconcile` returns `error` in exactly one place: `client.IgnoreNotFound` on the initial `Get`. Every
other path returns `nil`. That is deliberate and it is the implementation of
[ADR 0014 D3](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md): a listing failure
must not inflate `controller_runtime_reconcile_errors_total`, must not enter the workqueue backoff,
and must not touch the `Ready` condition, which describes whether the bucket is provisioned
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)) and not whether its size is
currently known. Nothing in this file writes a condition, a phase or `status.message`.

A failure therefore surfaces in three places and nowhere else: `status.usage.message`, the warning
event `UsageMeasurementFailed`, and the counter
`stackit_s3_provisioner_usage_measurement_failures_total`. That counter is the only aggregation
point — a failed measurement leaves no other trace beyond a message on the CR — which is why the
`StackitS3UsageMeasurementFailing` alert exists ([monitoring](../operations/monitoring.md)).

Every event this controller emits is a **Warning**; there is no event for a successful measurement,
because a success is already in the status and in the metrics and one event per bucket per interval
is pure noise.

<details>
<summary>Events, metrics and retry delays</summary>

| Event reason | Emitted when |
|---|---|
| `UsageConfigInvalid` | `spec.usage.interval` is not a positive Go duration |
| `UsageMeasurementDisabled` | the CR set `spec.usage.enabled: true` while `bucketUsage.enabled` is false |
| `UsageIntervalClamped` | the requested interval was raised to the floor — repeats on every pass, deliberately |
| `UsageMeasurementFailed` | the listing or the endpoint lookup failed |
| `UsageMeasurementTruncated` | the pass stopped at `maxObjects` |

| Metric | Type | Written by |
|---|---|---|
| `stackit_s3_provisioner_bucket_size_bytes` | gauge | collector, from `status.usage.bytes` |
| `stackit_s3_provisioner_bucket_objects` | gauge | collector, from `status.usage.objects` |
| `stackit_s3_provisioner_bucket_version_size_bytes` | gauge | collector |
| `stackit_s3_provisioner_bucket_version_objects` | gauge | collector |
| `stackit_s3_provisioner_bucket_billable_size_bytes` | gauge | collector |
| `stackit_s3_provisioner_bucket_estimated_monthly_cost` | gauge (label `currency`) | collector, only when a currency is set |
| `stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds` | gauge | collector |
| `stackit_s3_provisioner_bucket_usage_truncated` | gauge | collector |
| `stackit_s3_provisioner_buckets_usage_measured` | gauge | collector, count of CRs carrying a measurement |
| `stackit_s3_provisioner_usage_measurement_gate_enabled` | gauge | collector, mirrors `bucketUsage.enabled` |
| `stackit_s3_provisioner_usage_measurement_failures_total` | counter | `recordFailure` |
| `stackit_s3_provisioner_usage_measurement_duration_seconds` | histogram | `Reconcile`, successful passes only |

The per-bucket gauges are recomputed from the manager's cache on every scrape by
`collectUsage` ([metrics.go:312](../../internal/controller/metrics.go)) and are **absent** for a
bucket that was never measured, so `absent()` distinguishes "not measured" from "zero bytes".

| Constant | Value | Meaning |
|---|---|---|
| `usageNotProvisionedRetry` | `time.Minute` | nothing to measure yet, or the admin Secret is not readable |
| `usageFailureRetry` | `10 * time.Minute` | upper bound on the wait after a failure, applied as `min(interval, 10m)` |

</details>

## Configuration surface

All of it is operator-wide; a `Bucket` only chooses within these bounds. **The values themselves —
every key with its default and what it means — live in
[README.md → Reference](../../README.md#reference)**, and
[docs/operations/usage-and-cost.md](../operations/usage-and-cost.md) routes operators there. This
table is only the wiring a developer needs: which Helm key reaches which flag and which struct field.

| Helm value | Flag / env | `UsageConfig` field |
|---|---|---|
| `bucketUsage.enabled` | `--bucket-usage-enabled` / `BUCKET_USAGE_ENABLED` | `Enabled` |
| `bucketUsage.defaultEnabled` | `--bucket-usage-default-enabled` / `BUCKET_USAGE_DEFAULT_ENABLED` | `DefaultEnabled` |
| `bucketUsage.interval` | `--bucket-usage-interval` / `BUCKET_USAGE_INTERVAL` | `Interval` |
| `bucketUsage.minInterval` | `--bucket-usage-min-interval` / `BUCKET_USAGE_MIN_INTERVAL` | `MinInterval` |
| `bucketUsage.maxObjects` | `--bucket-usage-max-objects` / `BUCKET_USAGE_MAX_OBJECTS` | `MaxObjects` |
| `bucketUsage.includeVersions` | `--bucket-usage-include-versions` / `BUCKET_USAGE_INCLUDE_VERSIONS` | `IncludeVersions` |
| `bucketUsage.concurrency` | `--bucket-usage-concurrency` / `BUCKET_USAGE_CONCURRENCY` | `Concurrency` |
| `bucketUsage.pricing.perGBHour` | `--bucket-usage-price-per-gb-hour` / `BUCKET_USAGE_PRICE_PER_GB_HOUR` | `PricePerGBHour` |
| `bucketUsage.pricing.currency` | `--bucket-usage-currency` / `BUCKET_USAGE_CURRENCY` | `Currency` |

Two things about defaults that the values file cannot show you. First, the flag defaults compiled
into [`cmd/main.go`](../../cmd/main.go) are not the numbers an installed operator runs with: the
chart sets every flag explicitly, so in a Helm install the values file wins — a change to a
`DefaultUsage*` constant alone changes nothing for chart users. Second, the price is the one key
whose binary default and chart default genuinely differ: the binary defaults it to **empty**, which
means no cost estimate, and the chart supplies a list price.

`UsageConfig.withDefaults()` only fills `Interval`, `MinInterval`, `Currency` and `Concurrency`, and
normalises a negative `MaxObjects` to `0`. The booleans and the price have **no** default — a
`UsageConfig{}` constructed in a test measures nothing, which is why the offline tests all set
`Enabled: true` explicitly.

`effectiveFor` resolves this per `Bucket` into an `effectiveUsage`, whose four interesting fields
are worth reading as a set: `enabled` (`cfg.Enabled && spec.UsageEnabled(cfg.DefaultEnabled)` — the
gate wins over anything a CR can say), `requested` (`spec != nil && spec.Enabled != nil &&
*spec.Enabled` — what separates "opted in but gated off" from "never opted in", and therefore what
decides `patchUsageParked` versus `clearUsage`), `clamped` (the interval the CR asked for, kept so
the event and message can quote it), and `err` (an unusable interval is surfaced, never silently
defaulted away). The CR-side helpers `UsageEnabled`, `UsageIncludeVersions` and `UsageInterval` are
in [`api/v1/bucket_types.go`](../../api/v1/bucket_types.go) and are nil-receiver safe, which is why
`effectiveFor` can call them on a `nil` `spec`.

## Tests

| File | Covers |
|---|---|
| [`stackit/s3_stats_test.go`](../../stackit/s3_stats_test.go) | `BucketStats` against the in-memory fake: current objects, version listing, the cap stopping early, an empty bucket, error propagation |
| [`internal/controller/usage_config_test.go`](../../internal/controller/usage_config_test.go) | the cost formula, both formatters, `effectiveFor`, `withDefaults`, and that `nextMeasurement` actually spreads buckets |
| [`internal/controller/reconciler_usage_test.go`](../../internal/controller/reconciler_usage_test.go) | the controller end to end against a fake client plus `stackitfake`: gate, clamp, cap, versions, failure path, deletion, skeleton mode, clearing on switch-off |
| [`test/e2e/cloud_test.go`](../../test/e2e/cloud_test.go) | `TestCloudBucketUsage` and `TestCloudBucketUsageWithVersions` against the real provider (`make e2e-stackit`) |

The version listing in the fake is driven by `SeedObjectVersion`
([`internal/stackitfake/fake.go`](../../internal/stackitfake/fake.go)), which is where you model
delete markers and non-current versions. More on the test layers in [testing.md](testing.md).

## What is wrong today

**Measurement is not held during a provider outage.** The fleet-wide circuit breaker
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md),
[circuit-breaker.md](circuit-breaker.md)) is a field on `BucketReconciler` and this controller does
not have one. Verified by reading the file: there is no reference to `ProviderBreaker` in
[`bucket_usage_controller.go`](../../internal/controller/bucket_usage_controller.go). While
provisioning is held, every due bucket still issues its `GetBucket` and its listing.

Its queue is not paced either, and not merely "less paced": as established in
[Its own controller, queue and concurrency limit](#its-own-controller-queue-and-concurrency-limit),
controller-runtime's priority-queue default limiter is per-item exponential only and carries **no
fleet-wide qps component at all**. So during an outage the measuring controller has neither the
breaker nor any rate bucket. The only things bounding its traffic are `bucketUsage.concurrency` and
the interval floor. `bucketUsage.enabled: false` is the switch that actually stops it.

**A parked bucket does not notice the gate reopening.** Steps 2–4 above return with no requeue.
Turning `bucketUsage.enabled` back on restarts the operator, so this resolves in practice; changing
only `bucketUsage.defaultEnabled` on a running operator does not, and a bucket that relied on the
cluster default keeps its cleared `status.usage` until its next generation or annotation change.

**The two unit systems are easy to misread.** `HumanReadable` is binary (GiB) and renders
`BillableBytes`; the cost is decimal (GB) and rounds up. Both are correct; together they surprise
people, and anyone adding a third rendering should pick one and say which.

**`humanBytes` lives in `clone.go`.** `formatSize` in `usage_config.go` depends on it. It is a real
cross-file coupling in the same package; if the clone subsystem is ever split out, this breaks the
build and the fix is to move the helper, not to duplicate it.

**Not verified against the cloud: the cap and a genuinely versioned bucket.** The 2026-09-01 live
run measured an empty bucket and one 5 MiB object, and exercised the version listing on a bucket
that was *not* versioned. The truncation path, the `">= "` rendering and a bucket with real
non-current versions are covered offline only, against `stackitfake`. The behaviour of a pass over
millions of keys — the case `maxObjects` exists for — has never been observed.

**Not verified: the prices.** The figures in the chart are list prices read on 2026-09-01 from price
list v1.0.43 and service description v1.2, and nothing in the operator re-checks them. The price
list renders its own date as `08/04/2026`; it is read here as 2026-04-08, and that day-month order
is not independently verified.

## See also

* [ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) — the decision, its
  alternatives and its residual risks
* [docs/operations/usage-and-cost.md](../operations/usage-and-cost.md) — switching it on and reading
  the numbers
* [docs/operations/bucket-status.md](../operations/bucket-status.md) — the status fields and the
  `Size`, `Cost/Month`, `Objects` and `Measured` columns
* [docs/operations/monitoring.md](../operations/monitoring.md) — the metrics and the two alerts
* [reconcile-pipeline.md](reconcile-pipeline.md) — the other controller, and the status writes this
  one must not collide with
* [credentials.md](credentials.md) — where the admin credential this controller reads comes from
