# Ticket: a provisioned bucket that vanished is reported, never silently re-created

**Status:** Draft — behavior decided in review on 2026-09-19, design proposal, open questions listed.
Not approved for implementation.
**Scope:** this repo (`stackit-s3-provisioner`).
**Blocks:** [hot-reload the StackIT service-account key](007-hot-reload-the-stackit-service-account-key.md) —
that ticket's Q12 concluded that this guard, not a project-identity check, is what makes a foreign key
safe. This one stands on its own merit and should land first.
**ADR:** written as part of implementing this ticket, as its own ADR — decided in review on
2026-09-19: every ticket carries its own ADR, produced with the implementation, not signed off in
advance ([CLAUDE.md](../../CLAUDE.md) §2).
**Date:** 2026-09-19

## Problem

A bucket that the operator provisioned can disappear behind its back: somebody deletes it in the
StackIT console, a cleanup script runs, or the operator is restarted with a service-account key for a
different project so its own buckets are simply not there any more.

Today the operator treats "the bucket is not there" as "the bucket is not there *yet*" and provisions
it again. Reading the flow — **derived from the code, not yet reproduced**; an offline reproduction
against `stackitfake` is the first acceptance test of this ticket:

1. `ensureBucket` asks `HasBucket`, gets `false`, and calls `CreateBucket`
   ([bucket_controller.go:569-585](../../internal/controller/bucket_controller.go#L569-L585)). The new,
   empty bucket is stamped with this operator's ownership tags. `freshBucket` is now `true`.
2. `resolveWorkloadGroup` finds neither a `credentials-group-id` tag nor a policy on the fresh bucket,
   so it falls through to creating a new credentials group. The ADR 0002 D8 guard that would normally
   stop that — "do not create a second group while the group recorded in status still exists" — does
   not apply, because `guardGroupCreate` returns `nil` immediately when `freshBucket` is set
   ([bucket_controller.go:914-918](../../internal/controller/bucket_controller.go#L914-L918)).
3. The new group has no access keys, so `ensureAccessKeyAndSecret` mints one and **overwrites the
   workload Secret**.
4. The reconcile completes: `Ready=True`, an empty bucket under the original name, fresh credentials
   in the workload's Secret, the previous credentials group orphaned, and no event, condition or
   metric anywhere saying that the data is gone.

The workload keeps running and keeps writing — into an empty bucket, believing everything is fine.
This is the one place the operator is expected to be conservative, and it is the one place it silently
repairs instead of reporting.

### Second consequence: the credentials group is left behind

`teardown` already gates every provider step behind `bucketExists`
([bucket_controller.go:1201-1231](../../internal/controller/bucket_controller.go#L1201-L1231)), so
deleting a CR whose bucket is gone completes rather than hanging: the empty-check and the bucket delete
are skipped. What it does **not** do is release the workload credentials group — without the bucket
nothing can attribute it, so the operator reports `CredentialsGroupNotAttributable` and leaves it
standing. That is [ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D4
working as designed, and it is the right call, but it means every third-party deletion leaves a
credentials group holding a live access key behind. The key cannot reach the replacement bucket (new
bucket, new policy, new group), so this is orphaned garbage rather than live exposure — but it
accumulates and its cleanup is manual.

## Decided behavior (review 2026-09-19)

> A Bucket that was already provisioned and whose bucket the provider reports as absent must **not** be
> re-created. The CR goes into a failure state that makes the incident visible.
>
> Re-creating once needs no new mechanism: the data is gone either way, so **delete the CR and re-apply
> it**. That path already exists and already does the right thing.
>
> `spec.allowRecreate: true` is a **permanent** opt-in for Buckets whose content is regenerable: for
> those, a vanished bucket is re-created automatically without asking.

## Success criteria

| ID | Criterion |
|---|---|
| SC1 | A provisioned Bucket whose bucket is absent is never re-created implicitly |
| SC2 | The incident is visible without reading logs: `Ready=False` with a distinct reason, a Warning event, and a dedicated metric to alarm on |
| SC3 | The workload's Secret is not rewritten and its credentials group is not replaced while the bucket is missing |
| SC4 | A Bucket that comes back (bucket restored, correct key re-applied) recovers without human action |
| SC5 | `spec.allowRecreate: true` re-creates automatically, and each automatic re-creation is still reported as the incident it is |
| SC6 | Deleting and re-applying the CR provisions a fresh bucket — the documented way to re-create once |
| SC7 | A provider blip or a listing lag never trips the guard on a healthy Bucket |

## Design

### D1 — What counts as "provisioned"

`status.resolvedBucketName != ""`. It is written only on the success path, together with
`credentialsGroupID` and `accessKeyID` ([bucket_controller.go:375](../../internal/controller/bucket_controller.go#L375)).

The `stackit-bucket.gtrfc.com/resolved-bucket-name` annotation must **not** be used for this: it is
stamped *before* any cloud resource exists ([bucket_controller.go:1951-1960](../../internal/controller/bucket_controller.go#L1951-L1960)),
so it proves an intent, not a completed provisioning round.

### D2 — What counts as "absent", and why the current check is the wrong one

This is where SC7 is won or lost. `HasBucket` today is a **project-wide listing plus a linear scan**
([client.go:207-218](../../stackit/client.go#L207-L218) over `ListBucketNames`). Three problems for a
guard that flips a healthy Bucket to `Failed`:

* The listing is known to lag. `WaitBucketVisible` exists precisely because a freshly created bucket
  is not immediately visible in it ([client.go:222-239](../../stackit/client.go#L222-L239)).
* `ListBuckets(projectId, region)` takes no pagination parameters at all in the SDK
  (`objectstorage@v1.9.1`), so whether a large project returns every bucket is **unverified**. A capped
  page would make a present bucket read as absent — and, with this guard in place, alarm on it.
* A scan answers "not in the list I got", which is not the same statement as "does not exist".

**Decided in review on 2026-09-19: one definitive answer is enough — but "definitive" is a structured
answer from the API, never a status code on its own.** A provider that is unreachable, a gateway or WAF
serving an HTML error page, a 5xx, a dropped connection: none of those may reach the guard. They are
the failure mode the transient-error work already covers
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)) and they must keep taking the degraded path — sticky `Ready`,
`status.degradedSince`, `ProviderReachable=False`, grace window — not the vanished-bucket path.

The discriminator already exists and is exactly the right one: `apiAnswer` in
[stackit/errors.go](../../stackit/errors.go) requires a `*oapierror.GenericOpenAPIError` **whose body is
valid JSON** before it will report a status code, precisely because an intermediary produces the same
error type with the same status and an HTML body. `isServiceNotEnabled` is the structured-404 case of
it already.

So the guard asks `GetBucket` — the per-bucket control-plane read already used by `BucketConnInfo`
([client.go:394-406](../../stackit/client.go#L394-L406)) — and the classification lives **inside the
client**, not in the controller:

```go
// BucketExists reports whether the bucket exists in this client's project.
// Only the API's own structured JSON 404 counts as "no"; every other failure —
// transport error, 5xx, gateway page, empty body — is returned as an error and
// therefore stays a failure to reach the provider, never a statement about the
// bucket.
func (c *Client) BucketExists(ctx context.Context, name string) (bool, error)
```

Putting it there makes the conflation the review warned about **structurally impossible** rather than a
rule the caller has to remember — the same reason `EnsureService` keeps `isServiceNotEnabled` private
and only ever acts on its answer.

The S3 data plane is not a source for this decision at all. `BucketEmpty` and the policy reads go
through minio and have their own error shapes; the guard reads the control plane only.

### D3 — The failure state

`Phase=Failed`, `Ready=False` with a new `ReasonBucketMissing = "BucketMissing"`, and `status.message`
naming the bucket. No new phase: the phase enum is CRD-validated
([bucket_types.go](../../api/v1/bucket_types.go)) and adding one costs a CRD roll for no gain.

**Plus a dedicated condition, decided in review on 2026-09-19:**
`ConditionBucketPresent = "BucketPresent"`, set to `False` with the same reason, alongside the existing
`ConditionReady` and `ConditionProviderReachable` ([bucket_types.go:16-33](../../api/v1/bucket_types.go#L16-L33)).
The three then answer three different questions without anyone parsing a reason string: `Ready` says
usable, `ProviderReachable` says the provider answered, `BucketPresent` says the bucket is there. A
reader can tell a provider outage from a vanished bucket directly from the conditions.

Like `ProviderReachable`, the condition is **removed** rather than set to `True` on recovery, so a
Bucket that never lost its bucket and one that recovered look identical and an operator upgrade writes
nothing to healthy Buckets (`clearDegraded` sets the precedent,
[bucket_controller.go:1665-1670](../../internal/controller/bucket_controller.go#L1665-L1670)).

Alarm surface stays the gauge: `stackit_s3_provisioner_bucket_provisioned_missing{namespace,name}`,
following the existing `..._buckets_provider_degraded` / `..._bucket_degraded_since_timestamp_seconds`
pattern in [metrics.go](../../internal/controller/metrics.go), plus a `PrometheusRule` entry. Conditions
are not queryable in Prometheus; the gauge is what an alert fires on.

**This deliberately breaks kstatus health checks and will mark dependent Flux Kustomizations
not-ready.** That is correct here and must not be "fixed" later: the transient-error work
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)) holds `Ready` for failures that carry no information about the bucket. A structured
404 for a bucket we provisioned is the opposite — it is information about the bucket, and it is the
worst news the operator can deliver.

Requeue with `fail`, not `failNoRequeue`: rate-limited retry (1s → 15min cap) means a restored bucket
or a corrected key recovers on its own, satisfying SC4 with no extra machinery.

### D4 — Re-creating once: delete the CR and re-apply it

No new API surface, no trigger annotation, no extra status field. The bucket is gone and its data with
it, so there is nothing an in-place re-create preserves that a fresh CR does not. `reconcileDelete`
already handles an absent bucket (see the Problem section), and re-applying the manifest provisions a
new bucket, group, key and Secret.

Two consequences to document rather than hide:

* The workload gets **new credentials** and has to re-read its Secret. Honest: it was writing into a
  bucket that no longer exists.
* `status.resolvedBucketName` and the `resolved-bucket-name` annotation die with the CR, so the new
  bucket's name is composed from the operator's *current* naming policy
  ([`decideBucketName`](../../internal/controller/bucket_controller.go#L1932)). If the prefix or the
  namespace-inclusion setting changed since the original provisioning, the replacement bucket gets a
  different name. Harmless here — the old name is free — but it is a real difference from an in-place
  re-create, and it is why freezing the name in status exists in the first place.
* The old credentials group stays behind, as described in the Problem section.
* The **credentials Secret is deleted with the CR** — verified: `teardown` calls `deleteSecret`
  unconditionally as its last step ([bucket_controller.go:1232-1239](../../internal/controller/bucket_controller.go#L1232-L1239),
  the only exception being the operator's own admin Secret), and `upsertSecret` additionally sets a
  controller owner reference ([bucket_controller.go:1505](../../internal/controller/bucket_controller.go#L1505))
  so Kubernetes garbage collection would remove it even if teardown never ran. Operationally this means
  a gap: between deleting the CR and re-applying it the workload has no Secret. Pods that consumed it
  via `envFrom` keep their environment until they restart; anything reading it live fails. Plan the
  re-apply as one step, not two.

### D5 — Permanent opt-in: `spec.allowRecreate`

`spec.allowRecreate: true` declares that this bucket's content is regenerable and that a vanished
bucket may be re-created automatically, unattended. Decided in review on 2026-09-19: a spec field, not
an annotation, matching `spec.wipeOnDelete` — this operator keeps permanent data-affecting policy in
`spec` and uses annotations for triggers and frozen metadata. It needs a kubebuilder marker, a
regenerated CRD (`make generate-all`) and the Helm CRD sync.

A boolean is right here precisely because the policy *is* permanent: it lives in the manifest, it is
visible in review, and it says what it means. (An earlier draft proposed a timestamp-valued one-shot
trigger in the style of `rotate-credentials-at`; that was the wrong shape — a one-shot re-create does
not need a trigger at all, it needs D4.)

Unlike `wipeOnDelete` it needs **no** operator-wide feature gate: a wipe actively destroys data that is
still there, a re-create only rebuilds what is already gone.

Even when authorized, the re-creation is still an incident and must be reported: a Warning event and a
counter (`stackit_s3_provisioner_bucket_recreated_total{namespace,name}`) on every automatic
re-creation. Without that, opting in would restore exactly the silent-repair behavior this ticket
exists to remove — the CR would go `Ready` again with no trace that the data is gone.

Privilege: setting it needs write access to the Bucket CR in its own namespace — the same access that
can delete the CR outright, so no new privilege is introduced. Per
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) the namespace is the trust boundary
and this stays inside it.

### D6 — An authorized re-create mints a fresh workload identity

**Decided in review on 2026-09-19: fresh identity.** The re-created bucket gets a new credentials
group and a new access key, and the workload Secret is overwritten — the current code path, so
`guardGroupCreate`'s `freshBucket` bypass stays as it is and ADR 0002 D8 is not touched.

Two consequences that must be documented, not discovered:

* **The workload has to pick up the new Secret.** Until it does, it holds a key for the orphaned group,
  which the re-created bucket's policy does not name — so its requests fail with 403. A pod consuming
  the Secret via `envFrom` keeps the old values until it restarts. Under `spec.allowRecreate` this
  happens unattended, which is precisely why the Warning event and the
  `..._bucket_recreated_total` counter of D5 are load-bearing: they are what tells somebody to restart
  the workload.
* **The previous credentials group is left standing**, holding a live access key that can no longer
  reach anything. `status.credentialsGroupID` names it, but per
  [ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D4 the status is not a
  deletion source, so the operator must not remove it. Cleanup is manual, same as for the
  delete-and-re-apply path.

Teardown needs no change (Problem section), but the same caveat as D2 applies to it: it decides via
`HasBucket`, the project-wide listing. If that listing ever lies, teardown skips both the empty-check
and the bucket delete and drops the finalizer, orphaning a live bucket. Another argument for moving the
existence question to `GetBucket` ([Q6](#q6--answered-converted-everywhere-but-in-its-own-ticket)).

### D7 — Interactions

* **Usage measurement** gates on `ResolvedBucketName != "" && bucketIsReady(b)`
  ([bucket_usage_controller.go:183](../../internal/controller/bucket_usage_controller.go#L183)), so a
  missing bucket stops being measured automatically. Nothing to do.
* **Read grants:** a grantor whose own bucket is missing never reaches the policy step. A *grantee*
  that is missing already degrades to `ReadGrantPending`, which is unchanged.
* **Clone:** `spec.cloneFrom` runs against a freshly created bucket only. An authorized re-create of a
  Bucket whose clone is `Completed` must **not** re-run the clone — the clone-once terminal state is
  in `status.clone` and stays. Worth an explicit test.

## Security considerations

* This turns a silent, automatic repair into a visible fault. That is a net security gain: the current
  behavior destroys the evidence of a deletion by making the fleet look healthy again, which is exactly
  what an attacker who deleted a bucket would want.
* `spec.allowRecreate` is a standing authorization to skip a data-loss report, so the re-creation it
  permits must still be reported (D5). It never widens who may act — only Bucket writers in the
  namespace, who could already delete the CR.
* The guard must not become a denial-of-service on the operator itself: a false "absent" answer marks
  working Buckets as failed and pages somebody. Hence D2 (definitive per-bucket read, not a listing
  scan) and [Q3](#q3--answered-one-definitive-answer-and-definitive-means-structured).
* No new data leaves the cluster and no new credential is created by any of this.

## Test plan

* **Offline reproduction first** (`stackitfake`): provision a Bucket, delete the bucket out of band in
  the fake, reconcile, and assert today's behavior — re-created bucket, new credentials group,
  rewritten Secret. That test then flips to asserting the guard.
* Offline: guard trips only with `status.resolvedBucketName` set; a never-provisioned Bucket still
  provisions normally; the Secret and the credentials group are untouched while missing; recovery when
  the bucket reappears.
* Offline: `spec.allowRecreate: true` re-creates automatically and still emits the event and the
  counter; unset or false keeps the guard.
* Offline: teardown of a CR whose bucket is absent completes and still removes group and Secret.
* Offline, the SC7 test and the one this decision hangs on: a transport error, a 5xx, an empty body and
  a **404 carrying an HTML body** each take the degraded path; only a structured JSON 404 trips the
  guard. `stackitfake` can serve all of those.
* envtest: conditions, event and phase as specified.
* Live (`-tags integration`, real API): delete a provisioned bucket in the project, observe the guard
  and the metric, then authorize the re-create and observe recovery. This also settles the unverified
  pagination question in D2.

## Open questions

### Q1 — ANSWERED: an authorized re-create mints a fresh workload identity
**Decision (2026-09-19): fresh group, fresh key, Secret overwritten.** Keeping the identity was
proposed and rejected. The current code path therefore stands unchanged, `guardGroupCreate` keeps its
`freshBucket` bypass, and ADR 0002 D8 is untouched. Consequences for the workload and for the orphaned
group are in [D6](#d6--an-authorized-re-create-mints-a-fresh-workload-identity).

### Q2 — ANSWERED: no, the guard starts at the first successful provisioning
**Decision (2026-09-19): `status.resolvedBucketName` alone, nothing else.** A Bucket that created a
cloud bucket but was interrupted before the first successful status write has no
`resolvedBucketName`, so it provisions again — correct, because no workload ever received credentials
for it and there is no data to protect. A foreign bucket that happens to share the name is still
refused by `adoptOrCollide`'s ownership check, not adopted.

The `resolved-bucket-name` annotation must stay out of the predicate for the reason in
[D1](#d1--what-counts-as-provisioned): it is stamped before any cloud resource exists, so using it
would block first provisioning outright.

### Q3 — ANSWERED: one definitive answer, and definitive means structured
**Decision (2026-09-19): a single structured-JSON 404 from `GetBucket` trips the guard** — no second
opinion, no confirmation pass. With the explicit condition that an unreachable or misbehaving API must
stay distinguishable from a real absence and must keep taking the degraded path of [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md).
[D2](#d2--what-counts-as-absent-and-why-the-current-check-is-the-wrong-one) implements that by putting
the `apiAnswer` discriminator inside `Client.BucketExists`, so a gateway page or a 5xx can never arrive
at the guard as "absent".

### Q4 — ANSWERED: both — reason on Ready plus a `BucketPresent` condition
**Decision (2026-09-19):** `Ready=False` with `Reason=BucketMissing` **and** a parallel
`ConditionBucketPresent=False`, so a reader can separate "provider unreachable" from "bucket gone"
without matching reason strings. Details and the removal-on-recovery rule in
[D3](#d3--the-failure-state).

### Q5 — ANSWERED: spec field, not annotation
**Decision (2026-09-19): `spec.allowRecreate`.** It is a permanent, data-affecting policy, and this
repo already puts one of those in `spec` (`spec.wipeOnDelete`); annotations here are triggers and
frozen metadata (`rotate-credentials-at`, `resolved-bucket-name`). Costs a CRD change plus
`make generate-all` and the Helm CRD sync; buys `kubectl explain` and CRD validation. No operator-wide
feature gate (see D5).

### Q6 — ANSWERED: converted everywhere, but in its own ticket
**Decision (2026-09-19): this ticket adds `Client.BucketExists` and uses it for the guard only.**
Converting `ensureBucket`, the read-grant check and `teardown` off the project listing is
[decide bucket existence with `GetBucket`, not a project-wide listing](008-decide-bucket-existence-with-a-per-bucket-read.md),
which also carries the unverified `ListBuckets` pagination question and the one risky conversion
(`WaitBucketVisible`, whose timing against `GetBucket` nobody has measured).

Consequence to accept meanwhile: two existence notions coexist in the reconciler — the guard's
definitive per-bucket read and the listing scan everywhere else — and the teardown risk described in
[D6](#d6--an-authorized-re-create-mints-a-fresh-workload-identity) stays open until that ticket lands.
The two tickets are independent; neither blocks the other.

### Q7 — ANSWERED: its own ADR, written with the implementation
**Decision (2026-09-19): each ticket gets a separate ADR, written when that ticket is implemented.**
For this one: "a provisioned bucket is never re-created implicitly", stating the guard, the standing
`spec.allowRecreate` opt-in, delete-and-re-apply as the way to re-create once, the structured-404 rule
from [D2](#d2--what-counts-as-absent-and-why-the-current-check-is-the-wrong-one), and explicitly that
this failure is **not** covered by the degraded-Ready hold of [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md).

## References

* [hot-reload the StackIT service-account key](007-hot-reload-the-stackit-service-account-key.md) — the ticket
  whose Q12 produced this one
* [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) — `ensureBucket`, `resolveWorkloadGroup`, `guardGroupCreate`, `assertBucketEmpty`, `deleteBucketIfOwned`
* [stackit/client.go](../../stackit/client.go) — `HasBucket`, `WaitBucketVisible`, `BucketConnInfo`
* [stackit/s3.go](../../stackit/s3.go) — `BucketEmpty`
* [api/v1/bucket_types.go](../../api/v1/bucket_types.go) — status fields and the rotation-annotation precedent
* [docs/adr/0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md), [docs/adr/0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)
* [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) — the transient-error classification this failure is deliberately outside of
