# ADR 0014: Bucket size is measured by a separate controller that can only write the usage part of the status

## Status

Accepted. Date: 2026-09-01. This record was written on 2026-09-19, after the work landed;
the decisions below are reconstructed from the material written while it was taken.

Implemented: a second controller with its own work queue and concurrency limit measures every
opted-in, provisioned bucket, writes `status.usage` and nothing else, and derives a monthly cost
estimate from a configured price. The operator-wide gate (`bucketUsage.enabled`), the cluster
default (`bucketUsage.defaultEnabled`, off), the interval floor, the object cap, the per-`Bucket`
overrides, the metrics and the two alerts are all in place, and the path was verified against the
real provider on 2026-09-01. Open: incomplete multipart uploads are never counted, and the
measuring path is not held by the fleet-wide provider hold of
[ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md).

## Context

The request was narrow: show the current size of each bucket on its own resource, so it appears
as a column in `kubectl get` and in a cluster UI, at an interval the operator chooses; let a
cluster-wide default decide who is measured; let a single bucket override it in both directions;
and add an estimated monthly storage cost on top. One constraint came with it — finding the size
must never block the operator.

That constraint is not decoration, because of how a size can be obtained at all. The provider's
control-plane API has no usage, statistics or size endpoint; verified against the Object Storage
SDK the operator is built on (v1.9.1), whose entire surface is buckets, credentials groups, access
keys, service enablement, retention and compliance locks. A size therefore exists only as the sum
over a full listing of the bucket — roughly one request per 1000 object keys. A bucket with two
million objects is about 2000 requests, and the pass takes as long as the bucket is large. Put
that in front of credential and policy management and an informational feature decides how long a
workload waits for its credentials.

The second force was money, and it turned out to point the other way than expected. The pricing
and billing basis was researched on 2026-09-01 against the provider's published documents:

| Question | Finding | Source |
| --- | --- | --- |
| Billing metric | per started gigabyte per started hour, and nothing else | Object Storage service description v1.2, valid from 2026-03-26, section on service plan metrics |
| Price, Premium-EU01 | 0.00003697772 EUR per GB/hour (0.03 EUR in the monthly column) | Price list v1.0.43, dated 2026-04-08 |
| Price, Premium-EU02 | 0.00003883000 EUR per GB/hour | same price list |
| Price, Archiving-EU01 | 0.00003697772 EUR per GB/hour, identical to the standard tier | same price list |
| Request, operation or traffic charges | none — zero matches for request, operation, traffic or egress across all 31 pages | same price list |
| Monthly projection | 720 hours, the 30-day month the price list projects its own monthly figures with | footnote on every price list page |

So measuring costs **no money** and costs **time**. Every guard the feature needed had to be a
guard on time — how often a listing runs and how long one listing may last — not a budget. That
also settles what the cost figure can be: an estimate computed from a price the operator is told,
because the provider offers no API to ask what this project is actually charged.

The third force is what a measurement must not be able to break. The measuring path needs S3
credentials with access to every bucket, which is the operator's own admin credential
([ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md)) — the most privileged
thing in the system. A purely informational feature holding that credential must not be able to
create one, must not be able to overwrite the provisioning pass's view of the world, and must not
be able to turn a listing error into an unready bucket or into the alarm that means "provisioning
is broken".

Verified live on 2026-09-01 against project `ebc9d379…` in `eu01`: an empty bucket measured in
64 ms; after writing one 5 MiB object the next pass reported `bytes=5242880`, `objects=1` and
`0.03 EUR` — one started gigabyte, priced as above, so formula and price match a real listing;
a version listing requested on a **non-versioned** bucket was answered by the provider with the
live object marked as the latest version, counted once into the current counters, leaving
`versionBytes` and `versionObjects` at zero; and switching `spec.usage.enabled` to `false`
afterwards removed `status.usage` entirely.

## Decision

**D1 — Size measurement runs in its own controller, with its own work queue and its own
concurrency limit (`bucketUsage.concurrency`).** A slow measurement delays at most other
measurements. No provisioning pass ever waits for a size measurement. A provisioning pass does
list on its own account — a bounded emptiness check before teardown, and, for a `Bucket` with
`spec.cloneFrom`, one full listing of the source bucket so the clone progress has a stable
denominator — but those listings are its own, and no bucket's provisioning waits behind the
measuring queue.

**D2 — The measuring controller writes `status.usage` and nothing else, and it writes it as a
partial update.** It never writes any part of a `Bucket` spec ([ADR
0010](0010-the-operator-never-writes-to-a-bucket-spec.md)), never a condition, and never another
status field, so it cannot clobber what the provisioning pass owns. The opposite direction is
covered by optimistic concurrency: a measurement landing mid-flight makes the provisioning
status write retry rather than silently lose the measurement.

**D3 — A failed measurement is informational and stays informational.** It never touches the
`Ready` condition ([ADR 0012](0012-ready-describes-the-last-verified-state.md)), never returns a
reconcile error, and therefore never reaches the reconcile-error alarm, which belongs to
provisioning. It surfaces in `status.usage.message`, as the warning event
`UsageMeasurementFailed`, and in `stackit_s3_provisioner_usage_measurement_failures_total`.

**D4 — A failed measurement leaves the previous values in place.** A size from the last
successful pass is more useful than no size; `status.usage.message` says why it is aging, and
`status.usage.lastMeasurementTime` says how old it is.

**D5 — The measuring path reads the operator's admin credential and never creates it.** Minting
cloud credentials is the provisioning pass's job alone. With the admin Secret missing or
incomplete, the measurement waits and retries; it does not bootstrap.

**D6 — Only a bucket that is provisioned is measured.** That means a `Ready` bucket with a
`status.resolvedBucketName` ([ADR
0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md)). A bucket being deleted is
not measured — teardown has its own emptiness check and a listing pass would only race it — and
in the credential-less skeleton mode of [ADR
0005](0005-the-operator-serves-one-project-in-one-region.md) nothing is measured at all.

**D7 — When the next measurement is due is derived from `status.usage.lastMeasurementTime`, not
from a process-local timer**, and the next pass is scheduled by requeueing with a small
deterministic per-bucket offset. The schedule therefore survives an operator restart, and buckets
that all came due together do not stay in lock-step. The resource watch is filtered to generation
and annotation changes, because a controller that writes status must not be woken by its own
status write.

**D8 — The guards bound time, not money.** `bucketUsage.minInterval` is a floor: a `Bucket`
asking for a shorter `spec.usage.interval` is clamped up to it and told so in
`status.usage.message` and by the warning event `UsageIntervalClamped`.
`bucketUsage.maxObjects` caps how many listing entries one pass consumes.

**D9 — A capped measurement reports a lower bound and says so.** `status.usage.truncated` becomes
true, the warning event `UsageMeasurementTruncated` is emitted,
`stackit_s3_provisioner_bucket_usage_truncated` goes to 1, and the two rendered strings —
`status.usage.humanReadable` and `status.usage.estimatedMonthlyCost` — carry a `>=` prefix, which
is what a human reading the resource or a column sees. The machine-readable values
(`status.usage.bytes`, `objects`, `versionBytes`, `versionObjects`, `billableBytes`,
`estimatedMonthlyCostCents`, and the exported gauges) are the lower bound itself, written plain;
their marker is `status.usage.truncated` and `stackit_s3_provisioner_bucket_usage_truncated`,
which any consumer of those numbers has to read alongside them. A partial measurement is never
presented as a complete one.

**D10 — Measurement is opt-in through two independent switches.** `bucketUsage.enabled` is the
hard gate: with it off nothing is measured, whatever any `Bucket` asks for.
`bucketUsage.defaultEnabled` is the cluster-wide answer for every `Bucket` that does not set
`spec.usage.enabled` itself, and it ships off. A `Bucket` that explicitly asked while the gate is
closed is told — warning event `UsageMeasurementDisabled` and a note in `status.usage.message` —
rather than silently ignored, because silence is indistinguishable from a measurement that never
runs.

**D11 — A number nobody refreshes is removed, not left on display.** When a `Bucket` stops being
measured, `status.usage` is cleared. A message survives alone, without values, in exactly two
cases: the `Bucket` set `spec.usage.enabled: true` itself while the operator-wide gate is closed,
and `spec.usage.interval` is unusable (warning event `UsageConfigInvalid`). A `Bucket` that was
measured only because of `bucketUsage.defaultEnabled` and never asked for itself loses
`status.usage` with no message when the gate closes — the same silence D10 argues against,
accepted so that a cluster-wide switch-off does not write a message onto every `Bucket` in the
fleet.

**D12 — Non-current versions are counted only when asked for, per `Bucket` or fleet-wide.**
Version counting is on when `spec.usage.includeVersions` asks for it, and, for every `Bucket`
that does not decide for itself, when the operator-wide default `bucketUsage.includeVersions`
(off) is turned on. It switches the pass to a version listing, which walks every version and
delete marker and reports them in `status.usage.versionBytes` and `status.usage.versionObjects`. `status.usage.billableBytes` — the
figure the cost is computed from — is current objects plus whatever versions were counted.
Incomplete multipart uploads are never counted; they occupy billed storage but would need a third
listing.

**D13 — The cost is an estimate computed from a configured price, and the operator never asks the
provider what anything costs.** `status.usage.billableBytes` is rounded **up** to a whole decimal
gigabyte, because the billing metric is per *started* gigabyte, multiplied by
`bucketUsage.pricing.perGBHour` and by 720 hours, and rounded to whole cents.
`status.usage.estimatedMonthlyCostCents` is the canonical value and
`status.usage.estimatedMonthlyCost` is its rendering; `bucketUsage.pricing.currency` is a display
label and converts nothing. An empty or zero price means no estimate is written at all.

## Consequences

A second controller is a second thing to configure, to reason about and to keep RBAC for, and the
`Bucket` resource now has two writers on its status. That is the price of the isolation: the
partial update of D2 and the optimistic concurrency behind it are what make two writers safe, and
they have to stay that way.

Every displayed number is as old as the interval allows. `status.usage.lastMeasurementTime` and
the `Measured` column say how old, and `status.usage.measurementDuration` is the honest price of
the configured interval — the number to look at before lowering it.

The interval floor means a `Bucket` cannot get what it asks for and is told so by an event that
repeats on every pass. That is deliberate noise: a clamped interval is a policy decision the
workload owner should see.

Because a failed measurement is not a reconcile error (D3), nothing in the provisioning alarm
path notices that sizes have stopped updating. That is what
`stackit_s3_provisioner_usage_measurement_failures_total` and its alert exist for; without
monitoring on that counter, a permanently failing measurement is visible only on the resource.

The cost figure is a list-price estimate that can be confidently wrong. It prices a size measured
at one instant as if it were held for a whole 30-day month, it covers storage only, it excludes
taxes, and it uses whatever price the operator was configured with. Setting the price to empty
produces no cost at all, which is the honest configuration for anybody who does not want to
maintain their contract price.

Measuring produces provider traffic that the fleet-wide provider hold does not suppress (see
*Residual risks*), and each due measurement is one control-plane read of the bucket followed by
the listing itself.

## Alternatives Considered

**Ask the provider for the size.** Taken wherever it exists — it does not. The control-plane API
carries no usage, statistics or size endpoint, verified against the SDK version the operator
builds on. This is the cheapest possible implementation and it is simply unavailable; every other
option in this list exists because of that.

**Measure inside the provisioning reconcile.** Rejected, and it was the cheaper option: no second
controller, no second queue, no second status writer, no concurrency knob, and the admin
credential already in hand. It lost on one property — a listing pass is unbounded and takes as
long as the bucket is large, so an informational operation would sit in front of credential and
policy management and decide how long a workload waits. Isolation of the *failure* mattered too:
inside the provisioning pass, a listing error is a reconcile error and an unready bucket, which is
exactly what D3 refuses.

**Let a measurement failure mark the bucket degraded or unready.** Rejected. `Ready` describes
whether the bucket is provisioned, not whether its size is currently known. Folding a listing
failure into it would make an informational feature able to page somebody about a perfectly
healthy bucket.

**Extrapolate a size from a partial listing instead of capping.** Rejected. An estimated size
looks exactly like a measured one and there is no honest way to render the difference. The cap of
D8 with the explicit lower bound of D9 keeps the number true and makes its incompleteness visible
in the value itself, in an event and in a metric.

**Budget the measurement by request cost.** Rejected because there is nothing to budget: the
price list contains no request, operation or traffic position. A money guard would have been
theatre; the real scarce resource is time, and the floor and the cap bound exactly that.

**Schedule from a process-local timer or a cron-style ticker.** Rejected. It does not survive an
operator restart — every bucket would be measured at once on startup — and it would keep the
schedule in a place nobody can read. Deriving the due time from `status.usage.lastMeasurementTime`
puts the schedule where an operator can see it and makes a restart cost nothing.

**Count incomplete multipart uploads.** Rejected for now, deliberately and with a known cost: they
occupy billed storage, so the measurement can understate the invoice, but counting them needs a
third listing pass on top of objects and versions.

**Emit an event per successful measurement.** Rejected. A success is already visible in
`status.usage` and in the metrics; one event per bucket per interval is pure noise in an event
stream that otherwise only carries things worth looking at.

**One switch instead of two.** Rejected. An operator needs to stop all listing traffic at once
without editing every resource, and a platform team needs a default for resources that do not
decide themselves. Collapsing both into one field would have meant that turning the feature off
cluster-wide silently loses each workload's own choice.

## Residual risks

The configured price is a list price and nothing verifies it. A project on a contract price, on a
different tier or in a different region reports a wrong cost with full confidence, and the only
signal is that somebody compares it to an invoice.

A versioned bucket measured without version counting enabled (D12) understates both its size and
its cost, because non-current versions and delete markers never appear in a plain object listing.
Even with versions counted, incomplete multipart uploads are excluded (D12), so the figure remains
a floor rather than a match to the invoice.

A bucket that permanently exceeds `bucketUsage.maxObjects` never reports a true size; its cost
estimate is therefore permanently too low. Raising the cap is the fix, and it makes the pass
proportionally longer — not more expensive.

Measurement is not held during a provider outage. Verified: the measuring controller does not
consult the fleet-wide circuit breaker of [ADR
0013](0013-a-provider-outage-is-held-fleet-wide.md), so while provisioning is held, every due
bucket still issues its control-plane read and its listing. Its queue is also not on the paced
rate limiter that controller uses; what bounds it is the concurrency limit and the interval floor.

Not verified: that the price list and the service description figures reproduced in *Context* are
still current — they were read on 2026-09-01 from price list v1.0.43 (dated 2026-04-08) and service
description v1.2 (valid from 2026-03-26), and nothing in the operator re-checks them. The price
list renders its own date numerically as 08/04/2026; it is read here as 2026-04-08, and that
day-month order is not independently verified.

Not verified against the cloud: the object-cap path and the behaviour of a pass over millions of
keys. The 2026-09-01 live run measured an empty bucket and a single 5 MiB object, and exercised
the version listing on a bucket that was **not** versioned; the cap, the truncation rendering and
a genuinely versioned bucket are covered offline only.

## References

* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the admin credential
  the measuring path reads and may never create (D5)
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — skeleton mode, in which
  nothing is provisioned and therefore nothing is measured (D6)
* [ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — the resolved bucket
  name a measurement lists (D6)
* [ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md) — the write contract this
  controller also obeys (D2)
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — what `Ready` means, and why a
  measurement failure may not touch it (D3)
* [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) — the fleet-wide hold, which does not
  cover measurement
* [docs/operations/usage-and-cost.md](../operations/usage-and-cost.md) — turning measurement on,
  reading the numbers, and what the cost figure is and is not
* [docs/operations/bucket-status.md](../operations/bucket-status.md) — the status fields and
  columns a measurement fills
* [docs/operations/monitoring.md](../operations/monitoring.md) — the measurement metrics and the
  two alerts built on them
* [docs/developer/usage-measurement.md](../developer/usage-measurement.md) — how the schedule,
  the isolation and the cost computation are implemented
