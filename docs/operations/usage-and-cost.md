# Bucket size and monthly cost

The operator can measure how large each bucket is and write the result — plus an
estimated monthly storage cost derived from it — onto the `Bucket` resource, so
both appear as columns in `kubectl get bkt` and in any cluster UI that renders
printer columns:

```
NAME        BUCKET      PHASE   READY   STATUS        REGION   SIZE       COST/MONTH   AGE
my-bucket   my-bucket   Ready   True    provisioned   eu01     18.0 GiB   0.53 EUR     2d
```

Measurement runs in a controller of its own, with its own work queue and its own
concurrency limit, and it writes only `status.usage`
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D1,
D2). Nothing about provisioning waits for it, and a measurement that fails never
makes a bucket unready and never counts as a reconcile error
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D3).
This page covers turning it on, reading the numbers, and knowing what the cost
figure is and is not.

The complete list of every Helm value and every CRD field lives in the
[README reference](../../README.md) — this page explains only the keys it is
about.

---

## The two switches

Measurement is opt-in through two independent switches
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D10),
and a `Bucket` can override one of them:

| Switch | Where | Ships as | What it does |
| --- | --- | --- | --- |
| `bucketUsage.enabled` | Helm value, operator-wide | `true` | The hard gate. With it `false` nothing is measured at all, whatever any `Bucket` asks for. This is what you flip to stop all listing traffic at once without touching a single resource. |
| `bucketUsage.defaultEnabled` | Helm value, operator-wide | `false` | The cluster-wide answer for every `Bucket` that does not decide for itself. It ships **off**, so measurement is something a workload opts into. |
| `spec.usage.enabled` | `Bucket` resource | unset | Overrides `defaultEnabled` in either direction. It cannot open the gate: a `Bucket` asking to be measured while `bucketUsage.enabled` is `false` is refused, loudly (see [Failure modes](#failure-modes)). |

Leaving `spec.usage.enabled` unset is the useful default for a workload: a
cluster-wide policy change then reaches the `Bucket` without anybody editing it.

### A working minimal configuration

Operator side, in your Helm values:

```yaml
# values.yaml — only the keys you have to set or deliberately confirm
bucketUsage:
  enabled: true          # default; the hard gate — false measures nothing anywhere
  defaultEnabled: false  # default; leave it off to keep measurement a per-Bucket opt-in
  pricing:
    perGBHour: "0.00003697772"  # default in this chart; a LIST price, quoted as a STRING
```

The other `bucketUsage.*` keys ship with working defaults and are explained where
their edges are: the interval and its floor, the object cap and the concurrency
limit under [The guards, and what they clamp](#the-guards-and-what-they-clamp),
version counting under
[Versioned buckets understate by default](#versioned-buckets-understate-by-default),
and the currency label under
[How the estimate is derived](#how-the-estimate-is-derived). The complete list of
keys is in the [README reference](../../README.md).

Workload side, on the `Bucket` that wants to be measured:

```yaml
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: my-bucket                  # example
spec:
  bucketName: my-bucket            # example
  secretRef:
    name: my-bucket-credentials    # example
  usage:
    enabled: true                  # the whole opt-in; cannot open the gate
```

That one field is the entire opt-in. Two further fields exist under `spec.usage`
— an interval and a version-counting switch — both optional, both inheriting the
operator-wide default when omitted. Their edges are explained under
[The guards, and what they clamp](#the-guards-and-what-they-clamp) and
[Versioned buckets understate by default](#versioned-buckets-understate-by-default);
their exact spelling is in the [README reference](../../README.md).

Each `bucketUsage.*` value maps to one `--bucket-usage-*` flag and one
`BUCKET_USAGE_*` environment variable on the operator; the complete mapping is in
the [README reference](../../README.md). One difference matters when you run the
binary outside this chart: the price has **no built-in default**. The
`0.00003697772` above comes from the chart's `values.yaml`, and a bare operator
started without `--bucket-usage-price-per-gb-hour` writes no cost estimate at all.

### Verify

1. Confirm the operator came up with the settings you meant. The startup log line
   carries them:

   ```console
   $ kubectl -n <operator-namespace> logs deploy/<release>-stackit-s3-provisioner | grep "starting stackit-s3-provisioner"
   ... "bucketUsageEnabled": true, "bucketUsageDefaultEnabled": false, "bucketUsageInterval": "1h0m0s",
       "bucketUsageMinInterval": "1h0m0s", "bucketUsageMaxObjects": 2000000, "bucketUsagePricePerGBHour": 3.697772e-05
   ```

2. Wait for the first pass on an opted-in bucket. A bucket that was never
   measured is due immediately, and a bucket that is not `Ready` yet is
   re-checked every minute, so the numbers appear within a minute or two of the
   bucket becoming `Ready`:

   ```console
   $ kubectl get bkt my-bucket
   NAME        BUCKET      PHASE   READY   STATUS        REGION   SIZE       COST/MONTH   AGE
   my-bucket   my-bucket   Ready   True    provisioned   eu01     18.0 GiB   0.53 EUR     2d
   ```

3. Read the whole block, including how long the pass took:

   ```console
   $ kubectl get bkt my-bucket -o jsonpath='{.status.usage}' | jq
   ```

4. If the columns stay empty, work through [Failure modes](#failure-modes). Most
   reasons carry a warning event on the resource, but four do not: a missing or
   incomplete admin Secret is visible only as the operator log line `bucket size
   measurement waiting for admin credentials` at `V(1)`, which needs
   `logging.level: debug`, and a bucket that is not
   provisioned yet, skeleton mode and a bucket being deleted produce nothing at
   all. Two more — an unparseable interval or price in the Helm values — never
   reach a resource either: the operator crash-loops or exits at startup, so look
   at the pod first.

---

## What measuring costs

**Money: nothing.** STACKIT bills Object Storage per started gigabyte per started
hour and by nothing else — the price list carries no request, operation, traffic
or egress position. That was researched against the published documents on
2026-09-01 and is reproduced here with its provenance, because the numbers age
and nothing in the operator re-checks them:

| Question | Finding | Source |
| --- | --- | --- |
| Billing metric | per started gigabyte per started hour, and nothing else | Object Storage service description v1.2, valid from 2026-03-26, section on service plan metrics |
| Price, Premium-EU01 | 0.00003697772 EUR per GB/hour (0.03 EUR in the monthly column) | price list v1.0.43, dated 2026-04-08 |
| Price, Premium-EU02 | 0.00003883000 EUR per GB/hour | same price list |
| Price, Archiving-EU01 | 0.00003697772 EUR per GB/hour, identical to the standard tier | same price list |
| Request, operation or traffic charges | none — zero matches for request, operation, traffic or egress across all 31 pages | same price list |
| Monthly projection | 720 hours, the 30-day month the price list projects its own monthly figures with | footnote on every price list page |

Not verified: that these figures are still current on the day you read this. The
price list renders its own date as `08/04/2026`; it is read here as 2026-04-08,
and that day-month order is not independently verified.

**Time: one full listing per measured bucket per interval.** The control-plane
API has no usage, statistics or size endpoint — verified against the Object
Storage SDK the operator builds on (v1.9.1), whose entire surface is buckets,
credentials groups, access keys, service enablement, retention and compliance
locks. A size therefore exists only as the sum over a listing of the bucket,
which is roughly **one request per 1000 object keys**. A bucket with two million
objects is about 2000 requests per pass, and the pass takes as long as the bucket
is large.

That is why every guard on this feature bounds time rather than money
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D8),
and why `status.usage.measurementDuration` is the number to look at before you
lower an interval: it is the honest price of the interval you configured.

---

## The guards, and what they clamp

A `Bucket` that does not ask for an interval of its own is measured at
`bucketUsage.interval`, which ships as `60m`. What that costs in time is bounded
by three operator-wide guards and one deadline the operator derives rather than
exposes:

| Guard | Ships as | What it bounds | What happens at its edge |
| --- | --- | --- | --- |
| `bucketUsage.minInterval` | `60m` | How often one `Bucket` can be measured | A `Bucket` asking for less is measured at the floor instead, told so in `status.usage.message`, and gets the warning event `UsageIntervalClamped` on **every** pass |
| `bucketUsage.maxObjects` | `2000000` | How many listing entries one pass consumes | The pass stops early. `status.usage.truncated` becomes `true`, every value is a lower bound, the two rendered strings get a `>=` prefix, and the warning event `UsageMeasurementTruncated` is emitted |
| `bucketUsage.concurrency` | `2` | How many buckets are measured at once | Further due measurements wait in the measuring controller's own queue. They never delay a provisioning pass |
| measurement deadline | one interval | How long a single pass may run | The listing is cancelled and the pass is recorded as a failed measurement. A pass that cannot finish before its own next run is worthless and would occupy a slot forever |

Two things about the cap are easy to get wrong:

- It counts **listing entries**, not objects. With
  `includeVersions` on, every non-current version and every delete marker
  consumes an entry too, so a versioned bucket reaches the cap sooner than its
  object count suggests.
- A bucket that regularly exceeds the cap **never reports a true size**, and its
  cost estimate is therefore permanently too low. Raising the cap is the fix and
  it is safe in money terms: the pass gets proportionally longer, not more
  expensive. Setting `maxObjects: 0` removes the cap entirely — an unbounded
  listing, bounded then only by the measurement deadline above.

The interval floor produces deliberate, repeating noise: a clamped interval is a
policy decision the workload owner is meant to see, so the event fires on every
pass rather than once.

---

## Reading the numbers

Everything a measurement produces lives under `status.usage`, and every value
there is a measurement taken at `lastMeasurementTime` — not a live figure. It is
as old as the interval allows.

Four of those fields carry the answers people actually come looking for. A status
field never has a default, so every value below is an example:

```yaml
status:
  usage:
    billableBytes: 19327352832                   # current objects plus counted versions — the SIZE column and the cost basis
    truncated: false                             # true = every value in the block is a lower bound
    lastMeasurementTime: "2026-09-01T10:15:00Z"  # how old the figures are
    measurementDuration: 4.213s                  # the honest price of the configured interval
```

The rest of the block — the raw object and version counters, the rendered size
and cost strings, the canonical cost in cents, the currency label and
`message` — is listed field by field in the
[README reference](../../README.md).

Three details that catch people out:

- **The `SIZE` column is `billableBytes`, not `bytes`.** On a bucket measured with
  version counting the column includes the counted versions, which is the point —
  it is the figure the cost is computed from.
- **The display unit is binary, the billing unit is decimal.** `humanReadable`
  renders GiB (1024-based); billing counts started GB (1000-based). 18.0 GiB is
  19327352832 bytes, which is 20 started GB — see the worked example below.
- **`>=` appears only on the two rendered strings**, `humanReadable` and
  `estimatedMonthlyCost`. The machine-readable values and the exported gauges are
  the lower bound itself, written plain; their marker is `status.usage.truncated`
  and the `stackit_s3_provisioner_bucket_usage_truncated` gauge, which any
  consumer of those numbers has to read alongside them
  ([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D9).

The `Objects` and `Measured` columns exist too, at wide priority:

```console
$ kubectl get bkt -o wide
```

The remaining status fields and columns — phase, conditions, the provisioning
message — are described in [bucket-status.md](bucket-status.md).

### Where the schedule comes from

The next measurement is due one interval after
`status.usage.lastMeasurementTime`, plus a small deterministic per-bucket offset
of up to one tenth of the interval
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D7).
Two consequences you can rely on:

- **Restarting the operator costs nothing.** The due time lives on the resource,
  not in a process timer, so a restart does not re-measure the whole fleet — each
  bucket simply picks up its remaining wait.
- **A bucket that was never measured is due immediately**, which is why an opt-in
  shows a number on the next reconcile rather than after a full interval.

---

## How the estimate is derived

`billableBytes` is rounded **up** to a whole decimal gigabyte, because the billing
metric is per *started* gigabyte; multiplied by `bucketUsage.pricing.perGBHour`;
multiplied by 720 hours, the 30-day month the price list projects its own monthly
figures with; and rounded to whole cents
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D13).

Worked through with the values shown above:

```
billableBytes 19327352832           # example — displayed as 18.0 GiB
ceil(19327352832 / 1e9)  = 20       # started decimal gigabytes
20 x 0.00003697772 x 720 = 0.532479 # currency units
round to whole cents     = 53       # estimatedMonthlyCostCents
rendered                 = 0.53 EUR
```

`estimatedMonthlyCostCents` is the canonical value and `estimatedMonthlyCost` is
its rendering. `bucketUsage.pricing.currency` is a **display label only** —
changing it relabels the number and converts nothing.

### The five things this figure is not

1. **Not an invoice.** It prices storage only. Anything else on your bill is not
   in it.
2. **Not a period.** It prices the size measured at *one instant* as if that size
   were held for the whole 30-day month. A bucket that filled up yesterday is
   priced as if it had been full all month.
3. **Not your price.** It uses whatever price the operator was configured with.
   The chart default is the public **list** price for Object Storage Premium-EU01;
   if you are on a contract price, a different tier or a different region, put
   your own number in `bucketUsage.pricing.perGBHour`.
4. **Not tax-inclusive.** The sourced price is net, excluding taxes.
5. **Not checked.** Nothing in the operator asks the provider what anything costs
   — there is no API for it — and nothing re-reads the price list. A wrong price
   produces a confidently wrong cost, and the only signal is somebody comparing
   it to an invoice.

If you do not want to maintain a price, the honest configuration is to switch the
estimate off: set `pricing.perGBHour` to `""` or `"0"`. No cost is then written
to any `Bucket`, `status.usage.currency` stays empty, and the `COST/MONTH` column
is blank while the size keeps working.

Quote the price as a **string**. Unquoted, YAML turns `0.00003697772` into an
exponent and the value that reaches the operator is not the one you typed. A
value that is not a non-negative decimal number makes the operator exit at
startup with `invalid bucket usage price` rather than silently costing zero.

---

## Versioned buckets understate by default

On a bucket with versioning enabled, non-current versions and delete markers
occupy billed storage but never appear in a plain object listing. With
`includeVersions` off — the shipped default, both operator-wide and per
`Bucket` — the measurement therefore reports **less than the invoice**, and the
cost estimate is correspondingly low.

Turning it on switches the pass to a version listing, which walks every version
and every delete marker in one pass and reports them in `versionBytes` and
`versionObjects`. `billableBytes` is then current objects plus counted versions.
The trade is listing cost: more entries per pass, a longer pass, and the object
cap reached sooner.

Even with versions counted the figure remains a floor: **incomplete multipart
uploads are never counted**
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D12).
They occupy billed storage, but finding them needs a third listing pass.

Requesting a version listing on a bucket that is **not** versioned is harmless —
verified live on 2026-09-01 against `eu01`: the provider answers it, the live
object comes back marked as the latest version and is counted once into the
current counters, leaving `versionBytes` and `versionObjects` at zero.

---

## Turning measurement off

A number nobody refreshes is removed rather than left on display
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D11).
There are two distinct outcomes, and the difference is whether the `Bucket` asked
for measurement itself:

| Situation | What happens to `status.usage` | What you see |
| --- | --- | --- |
| `spec.usage.enabled: false`, or `defaultEnabled: false` and the `Bucket` never asked | Removed entirely — size, cost, timestamps, all of it | The `SIZE` and `COST/MONTH` columns go blank, with no message |
| `spec.usage.enabled: true` while `bucketUsage.enabled: false` | Values dropped, **message kept**: `bucket size measurement is disabled operator-wide (bucketUsage.enabled=false)` | Warning event `UsageMeasurementDisabled` |
| `spec.usage.interval` is unusable | Values dropped, message kept with the parse error | Warning event `UsageConfigInvalid` |
| A measurement failed | **Values kept**, message says why they are aging | Warning event `UsageMeasurementFailed` |

The asymmetry is deliberate. A failed measurement is expected to succeed again,
so a stale size is more useful than none and `lastMeasurementTime` says how old
it is ([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)
D4). A bucket that is no longer measured will never get a fresh number, so
leaving one on display would assert a currency it no longer has.

One accepted gap: a `Bucket` that was measured only because of
`defaultEnabled` and never asked for itself loses `status.usage` with **no
message** when the gate closes. That is the same silence the gate refusal above
argues against, accepted so that a cluster-wide switch-off does not write a
message onto every `Bucket` in the fleet.

Clearing happens on the `Bucket`'s next pass through the measuring controller. A
spec edit triggers one immediately. A values-only change restarts the operator,
which reconciles every `Bucket` on startup — so a cluster-wide switch-off takes
effect as the new pod comes up, not gradually.

---

## Failure modes

Everything the measuring controller emits is a **Warning** event: a successful
measurement is already visible in status and in the metrics, so an event per
bucket per interval would be pure noise.

| What went wrong | What you see | What to do |
| --- | --- | --- |
| The listing failed (S3 error, permissions, provider trouble) | Event `UsageMeasurementFailed`, the error in `status.usage.message`, previous values kept, `stackit_s3_provisioner_usage_measurement_failures_total` rises | Read the message. The next attempt is at the smaller of the interval and 10 minutes, so a transient error does not cost a full interval |
| The pass hit the object cap | Event `UsageMeasurementTruncated`, `status.usage.truncated: true`, `>=` on both rendered strings, gauge `stackit_s3_provisioner_bucket_usage_truncated` at 1 | Decide whether a lower bound is good enough. If not, raise `bucketUsage.maxObjects` — the pass gets longer, not more expensive |
| A `Bucket` asked for a shorter interval than the floor | Event `UsageIntervalClamped` on every pass, note in `status.usage.message` | Either accept the floor or lower `bucketUsage.minInterval` operator-wide, having looked at `status.usage.measurementDuration` first |
| `spec.usage.interval` is unusable (e.g. `"0s"`, which passes the CRD pattern but is not positive) | Event `UsageConfigInvalid`, parse error in `status.usage.message`, values dropped, **no retry** until the spec changes | Fix the value. A syntactically wrong string such as `"6 hours"` or a bare `60` is rejected by the CRD at admission instead |
| A `Bucket` asked while the gate is closed | Event `UsageMeasurementDisabled`, message in `status.usage.message` | Either set `bucketUsage.enabled: true` or remove `spec.usage.enabled` from the resource |
| The admin Secret is missing or incomplete, or `POD_NAMESPACE` is unset | No event; a V(1) log line `bucket size measurement waiting for admin credentials`; retry in 1 minute | The measuring path **reads** the operator's admin credential and never creates it ([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D5, [ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)). Fix provisioning first — see [credentials.md](credentials.md) |
| The `Bucket` is not `Ready`, or has no `status.resolvedBucketName` | Nothing measured, no event; retry in 1 minute | Only a provisioned bucket is measured ([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D6). Fix provisioning; see [bucket-status.md](bucket-status.md) |
| The operator runs without a service-account key (skeleton mode) | Nothing measured, no event, no retry | Nothing is provisioned in skeleton mode, so there is nothing to measure ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md)) |
| A `Bucket` is being deleted | Not measured | Teardown runs its own emptiness check and a listing pass would only race it |
| `bucketUsage.interval` or `minInterval` set without a unit (e.g. `60`) | The operator crash-loops at startup on flag parsing | Quote a Go duration with a unit: `"60m"`, `"6h"` |
| The price is not a non-negative decimal | The operator exits at startup with `invalid bucket usage price` | Fix the value, quoted as a string |

### Two things that will not tell you

- **The provisioning alarm path never notices.** Because a measurement failure is
  not a reconcile error, sizes can stop updating fleet-wide without a single
  reconcile-error alert firing. The alert
  `StackitS3UsageMeasurementFailing` exists for exactly this, and without it a
  permanently failing measurement is visible only on the resources. See
  [monitoring.md](monitoring.md).
- **A provider outage does not stop measurement.** The fleet-wide hold of
  [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) covers
  provisioning; the measuring controller does not consult it, and its queue is not
  on the paced rate limiter that controller uses. While provisioning is held,
  every due bucket still issues its control-plane read and its listing. What
  bounds that traffic is `bucketUsage.concurrency` and the interval floor. If you
  need it to stop during an incident, `bucketUsage.enabled: false` is the switch.
  See [provider-outages.md](provider-outages.md).

---

## Related pages

| Page | What it covers |
| --- | --- |
| [ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) | The decision: why measurement is isolated from provisioning in every direction, and why its guards bound time |
| [README reference](../../README.md) | The complete `bucketUsage.*` and `spec.usage.*` surface |
| [bucket-status.md](bucket-status.md) | The rest of the `Bucket` status and the printer columns |
| [monitoring.md](monitoring.md) | The measurement metrics and the two alerts built on them |
| [configuration.md](configuration.md) | The other operator settings whose consequences are not obvious |
| [provider-outages.md](provider-outages.md) | What the operator does when the provider is unreachable |
