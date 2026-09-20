# ADR 0015: A Provisioned Bucket That Has Vanished Is Reported, Never Re-Created Implicitly

## Status

Accepted. Date: 2026-09-19. Amends [ADR 0012](0012-ready-describes-the-last-verified-state.md).

Implemented: a `Bucket` that has completed a provisioning round and whose bucket the provider
answers for definitively as absent goes to phase `Failed` with `Ready=False`, reason `BucketMissing`,
a parallel `BucketPresent=False` condition, a Warning event and the
`stackit_s3_provisioner_bucket_provisioned_missing` gauge, and nothing is created. The standing
opt-in `spec.allowRecreate` rebuilds such a bucket unattended and reports each rebuild. This record
amends ADR 0012 D6, which enumerated the definitive faults and grows only by being amended: this is
the seventh. Open: the existence question is asked per bucket only *here*; the provisioning step,
the read-grant check and the teardown still decide existence from a project-wide listing, which is
why the teardown risk in D9 below is real rather than theoretical.

## Context

A bucket the operator provisioned can disappear behind its back. Somebody deletes it in the STACKIT
console. A cleanup script runs. The operator is restarted with a service-account key for a different
project, and its own buckets are simply not there.

Until this record the operator read "the bucket is not there" as "the bucket is not there *yet*". It
created an empty bucket under the frozen name, and because a bucket it created in that very pass
cannot yet have a credentials group attributed to it, the lag guard of
[ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) D8 was deliberately skipped:
a fresh group was created, a fresh access key minted, and the workload credentials Secret
overwritten. The pass then *succeeded*. `Ready` stayed `True`, no condition moved, no event was
emitted, no metric changed. The workload kept running and kept writing — into an empty bucket,
believing everything was fine.

[ADR 0012](0012-ready-describes-the-last-verified-state.md) does not reach this case and says so in
its own residual risks: the provider *answers*, so nothing is held and no rule about readiness
applies. That record's asymmetry — hold a state you cannot verify, because dropping it wrongly marks
the whole fleet sick on the first blip — is right, and it is right for the failure it was written
for. It also names the condition under which it has to be revisited: "a bucket that is gone and being
written to". That is exactly this.

The reason the gap survived is that the two failures are genuinely hard to tell apart. "The bucket is
absent" and "I could not find out whether the bucket is there" arrive through the same call, and on
2026-08-25 an intermediary in front of the API produced errors indistinguishable by status code from
the API's own — a `403` carried by an HTML error page. A guard that flips a healthy `Bucket` to
`Failed` on a mistaken reading of that does not merely delay a signal: it declares data loss on
working buckets and pages somebody. So this record is only safe because the distinction it rests on
is a structural one that already exists, and it puts that distinction where it cannot be forgotten.

## Decision

**D1 — A `Bucket` that has been provisioned and whose bucket the provider reports as absent is never
re-created implicitly.** It is reported instead. The data is gone either way; what the operator
controls is whether anybody finds out. Re-creating hands the workload an empty bucket under the
original name and destroys the evidence that anything happened — which is also precisely the outcome
an attacker who deleted a bucket would want.

**D2 — "Provisioned" is `status.resolvedBucketName`, and nothing else.** It is written only when a
pass completed successfully, so it is the operator's own record that a bucket once existed and a
workload once received credentials for it. The `resolved-bucket-name` annotation deliberately does
not count: it is stamped *before* any cloud resource is created, so it proves an intent, not a
completed round, and keying the guard on it would block first provisioning outright. A `Bucket`
interrupted between creating its bucket and its first successful status write therefore provisions
again — correct, because no workload ever held credentials for it and there is no data to protect. A
foreign bucket that happens to share the name is still refused by the ownership check of
[ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md).

**D3 — "Absent" is one structured answer from the provider about that one bucket, and definitive
means structured.** The question is asked as a per-bucket read, not as a scan of a project-wide
listing: a scan answers "not in the list I got", which is a weaker statement — the listing is known
to lag a create, and whether it is complete for a large project is **not verified**, because the
listing call takes no pagination parameters. One definitive answer is enough; no second opinion and
no confirmation pass. But an unreachable provider, a `5xx`, a dropped connection, a gateway or WAF
page and an empty body are **not** answers: nothing on the provider side got to decide. Only a
structured JSON `404` counts as "no", the same discriminator that already separates "Object Storage
is not enabled for this project" from "I could not find out".

**D4 — The classification lives in the provider client, not in the guard.** The client exposes a
per-bucket existence question that returns `false` only for the structured answer and returns
everything else as an error. Conflating the two is then structurally impossible rather than a rule a
caller has to remember, which matters because the cost of forgetting it once is a fleet-wide false
report of data loss. The S3 data plane takes no part in this decision at all: it has its own error
shapes, and the question is asked on the control plane only.

**D5 — The failure is `Ready=False` with reason `BucketMissing`, plus a parallel
`BucketPresent=False` condition.** Three conditions then answer three different questions without
anyone parsing a reason string: `Ready` says usable, `ProviderReachable` says the provider answered,
`BucketPresent` says the bucket is there. A reader separates a provider outage from a vanished bucket
from the conditions alone. Like `ProviderReachable`, `BucketPresent` is **removed** on recovery
rather than set to `True`, so a `Bucket` that never lost its bucket and one that recovered look
identical and an operator upgrade writes nothing to healthy Buckets. No new phase: the phase is
`Failed`, because the phase enum is validated by the CRD and adding a value costs a CRD roll for no
gain.

**D6 — This deliberately breaks kstatus health checks, and that must not be "fixed" later.** A
`Bucket` in this state marks dependent Flux Kustomizations not-ready. That is correct.
[ADR 0012](0012-ready-describes-the-last-verified-state.md) holds `Ready` through failures that carry
*no information* about the bucket; a structured `404` for a bucket the operator provisioned is the
opposite — it is information about the bucket, and it is the worst news the operator can deliver.

**D7 — The fault neither trips nor resets the fleet-wide circuit breaker, and it is retried on the
ordinary rate limiter.** Both halves are load-bearing. Counting it as a provider failure would open
the breaker fleet-wide after a restart against the wrong project — where *every* provisioned `Bucket`
reads as absent — and stop every provider call including teardowns, over an answer the provider gave
definitively; [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) D3 already exempts every
definitive fault for that reason. Refusing to retry at all would be equally wrong: a restored bucket,
or the operator being pointed back at the right project, has to recover with no human action. The
rate-limited retry is what delivers that recovery, and it needs no other machinery.

**D8 — Re-creating once is delete the CR and re-apply it.** No trigger annotation, no extra status
field, no new API surface. The bucket and its contents are gone, so nothing an in-place re-create
preserves is worth preserving, and the deletion path already handles an absent bucket. Two
consequences are documented rather than hidden: the workload gets new credentials and must re-read
its Secret, and the replacement bucket's name is composed from the operator's *current* naming
policy — if the prefix or the namespace-inclusion setting changed since the original provisioning,
the new bucket gets a different name. Harmless, since the old name is free, but it is a real
difference from an in-place re-create and it is why freezing the name
([ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md)) exists at all.

**D9 — `spec.allowRecreate` is a permanent, per-`Bucket` authorisation to rebuild, and every rebuild
is still reported.** It declares that this bucket's content is regenerable. It is a spec field rather
than an annotation because it is permanent, data-affecting policy, which this operator keeps in
`spec` (`spec.wipeOnDelete`); annotations here are triggers and frozen metadata. It needs **no**
operator-wide feature gate, unlike a wipe: a wipe destroys data that is still there, a re-create only
rebuilds what is already gone. Setting it requires write access to the `Bucket` in its own namespace
— the same access that can delete the CR outright — so no new privilege is introduced and the
namespace stays the boundary of [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md). The
report is not optional: a Warning event with reason `BucketRecreated` and the
`stackit_s3_provisioner_bucket_recreated_total` counter fire on every automatic rebuild. Without
them, opting in would restore exactly the silent repair this record removes — the CR would go `Ready`
again with no trace that the data is gone.

**D10 — An authorised re-create mints a fresh workload identity, and the previous credentials group
is left standing.** The rebuilt bucket gets a new credentials group and a new access key, and the
workload Secret is overwritten. Keeping the old identity was considered and rejected. Two
consequences follow and are documented, not discovered: the workload holds a key for a group the new
bucket's policy does not name, so its requests fail with `403` until it re-reads its Secret — which
is what the event and the counter are for — and the previous group keeps a live access key that can
no longer reach anything. Status is not a deletion source
([ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) D4), so the operator must
not remove that group; its cleanup is manual. The same applies to the delete-and-re-apply path of D8.
The teardown of a `Bucket` whose bucket is absent completes rather than hanging, and leaves the group
behind for the same reason.

**D11 — A completed one-shot clone is not re-run by an authorised re-create.** The terminal state of
`spec.cloneFrom` lives in the `Bucket`'s status, which survives a rebuild because the CR is not
deleted. Copying the source again, unasked, into a bucket that was rebuilt unattended is not what
`spec.allowRecreate` authorises.

**D12 — The alarm surface is the gauge, because conditions are not queryable in Prometheus.**
`stackit_s3_provisioner_bucket_provisioned_missing` carries `{namespace, name}` and is absent for
every healthy `Bucket`. The chart ships `StackitS3BucketMissing` on it (`critical`, `for: 5m`,
`monitoring.prometheusRule.alerts.bucketMissing.enabled: true` # default) alongside the existing
`StackitS3BucketFailed` (`warning`, `15m`), which keeps firing on the phase: one incident, two alerts
of different severity, which is how the degraded alerts already behave.
`StackitS3BucketRecreated` (`warning`) exists for the counter but ships
`monitoring.prometheusRule.alerts.bucketRecreated.enabled: false` # default, because it can only fire
on a deployment that has adopted `spec.allowRecreate`; whoever sets that field turns the alert on
with it.

**D13 — No durable trace of an automatic re-creation is written to the CR.** A status field
recording the last rebuild was offered — cheap, since the CRD changes for `spec.allowRecreate`
anyway, and durable where an event and a metric are not — and declined. The consequence is stated
rather than softened: after an unattended rebuild the only evidence is the Warning event for as long
as the cluster keeps it, and the counter for as long as Prometheus does.

## Consequences

The operator now has a failure state that means "your data is gone", and it is loud on purpose. A
`Bucket` in it is not usable, dependent Kustomizations go not-ready, and a critical alert fires after
five minutes. That is the cost of not lying, and it is paid every time somebody deletes a bucket in
the console without deleting the CR — which, under this record, is no longer a silent operation.

The guard is one extra provider call per reconcile of an already-provisioned `Bucket`: a per-bucket
read before the provisioning step. It is a control-plane read, it is subject to the breaker like
every other call, and it replaces nothing — the provisioning step still consults the project listing,
so the two existence notions coexist. That duplication is deliberate for now and is the open item in
the Status above.

A restart against a wrong or rotated service-account key turns every provisioned `Bucket` in the
fleet into a `BucketMissing` report at once. That is the correct reading of what the operator can see
— its buckets really are not in the project it is authenticated against — and D7 is what keeps the
consequence bounded: no breaker trip, so teardowns and every other call keep working, and the whole
fleet recovers on its own when the right key comes back. It also means this guard, not a project
identity check, is what makes running with a foreign key safe.

`spec.allowRecreate` buys unattended recovery at the price of an unattended rotation: the workload
breaks with `403` until somebody restarts it. The field is honest about that, and the two reports of
D9 are the only thing standing between it and the behaviour this record exists to remove.

## Alternatives Considered

**Keep re-creating, and emit a Warning event.** The cheapest option, and it loses because an event is
not a state: it expires, it is not queryable, and the CR goes `Ready` — so every automated consumer,
every dashboard and every GitOps health check still reports success while the data is gone. Reporting
has to change what the object says, not just what the event stream says.

**Confirm the absence with a second read, or across two reconciles, before reporting.** Attractive
against a flaky provider, and rejected because it buys nothing the structural discriminator of D3
does not already buy. A misbehaving intermediary answers the second read exactly like the first, so
two reads confirm each other while being equally wrong; and the delay is paid on every real
incident. The correct hardening is the shape of the answer, not the number of answers.

**Decide absence from the project-wide listing that the provisioning step already uses.** No new
call, no extra latency. It loses on D3: a listing whose completeness is unverified and whose lag is
documented is fine for deciding whether to create something, and not fine for declaring a bucket the
operator provisioned to be gone.

**A one-shot re-create trigger, in the style of the rotation annotation.** Proposed in an early
draft and dropped: a one-shot re-create does not need a trigger at all. Delete the CR and re-apply it
(D8) already does the whole job with machinery that exists and is tested.

**Keep the workload identity across an authorised re-create.** It would spare consumers the `403`
window. It loses because the credentials group is attributed *through the bucket*
([ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md)), and the bucket that
carried the attribution no longer exists; reconstructing the binding from status would make status a
source of truth for a cloud resource, which ADR 0002 D4 forbids for good reason.

**An operator-wide feature gate for `spec.allowRecreate`, mirroring the wipe gate.** Symmetrical, and
rejected in D9: the two are not the same risk. A wipe destroys data that is still there; a re-create
rebuilds what is already gone. Gating it would add an operator-wide switch that can only ever
withhold recovery.

**A `status.lastRecreatedAt` field.** Declined in D13. It is the one alternative that was cheaper
than it looked and still lost — the CRD was changing anyway — on the grounds that a re-creation is an
incident to be noticed now, not a fact to be kept.

## Residual risks

**Every existence question outside this guard still goes through the project-wide listing.** The
provisioning step, the read-grant resolution and the teardown all use it. The teardown case is the
sharp one: if that listing ever answers incompletely, the teardown skips both the empty-check and the
bucket delete and drops the finalizer, orphaning a live bucket. This record does not close that, and
converting the remaining callers is its own decision because one of them — the wait for a freshly
created bucket to become visible — has timing nobody has measured against a per-bucket read.

**A service account that loses its role on the project mid-flight.** The refusal arrives as a
structured `401`/`403`, which is already definitive under
[ADR 0012](0012-ready-describes-the-last-verified-state.md) D6 and never reaches this guard, so the
`Bucket` reports a refusal rather than a missing bucket. **Not verified:** whether the API can ever
answer a per-bucket read with a structured `404` for a bucket that exists but the caller may not see.
If it can, a permission change would be reported as data loss. Nothing observed so far suggests it
does, and the project-scoped answer on a foreign project is a `403`.

**The counter is a process counter.** It restarts at zero when the operator restarts, so
`increase()` over a window that spans a restart under-reports. With D13 declining a durable field,
that is the accepted weakest link in the record of an unattended re-creation.

**Not verified: the live behaviour of the whole guard against the real API.** Every case in this
record — the structured `404`, the gateway page carrying a `404`, the empty body, the transport
failure, the recovery — is reproduced offline against the in-memory fake, and the fake is built to
the shapes captured from the live API on 2026-08-25. Deleting a real bucket in a real project and
watching the guard, the metric and the recovery has not been done.

**Not verified: that `for: 5m` is the right window for the critical alert.** It is a sensible default
chosen to match the existing alert set, not a measurement. A vanished bucket does not come back on
its own, so the window trades nothing but notification latency.

## References

* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — the readiness semantics this record
  amends, and whose residual risks named this gap
* [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) — the breaker, and why a definitive fault
  neither trips nor resets it
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — attribution through the
  bucket, the lag guard an authorised re-create skips, and why status is not a deletion source
* [ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — the frozen name, and
  what delete-and-re-apply gives up
* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — the namespace as the trust boundary
  `spec.allowRecreate` stays inside
* [docs/operations/vanished-buckets.md](../operations/vanished-buckets.md) — what an operator sees
  and does
* [docs/developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md) — where the guard sits
  in the pass
* [docs/developer/provider-errors.md](../developer/provider-errors.md) — the answer-versus-failure
  discriminator D3 and D4 rest on
