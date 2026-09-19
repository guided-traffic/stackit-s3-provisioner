# Deleting a Bucket

Deleting a `Bucket` CR is the only way to remove the cloud resources behind it, and
it is deliberately not a fast operation. The operator will not destroy a bucket that
still holds objects: a deletion whose bucket has data in it stops, keeps the CR alive
through its finalizer, and waits for a human. That is the decision recorded in
[ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D2, and the most
common reason a `kubectl delete` appears to hang.

This page covers what a teardown actually does, how to read a delete that will not
finish, and the two supported ways past the guard. For the complete field and value
reference — `spec.wipeOnDelete`, `wipeOnDelete.enabled` and everything else — see
[README.md](../../README.md); this page explains a handful of keys, it does not list
them.

---

## What is removed and in which order

The teardown runs as one pass, in the order below
([`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go),
`teardown`). Every step tolerates the thing it removes being gone already, so a
teardown that is retried after a partial failure converges rather than erroring.

| # | Step | What it touches | Skipped when |
| --- | --- | --- | --- |
| 1 | Stop a running clone | the rclone `Job` and its staging Secret, both in the operator namespace | never — it runs even when the delete then blocks |
| 2 | Look up the physical bucket | control plane, by the resolved bucket name | never |
| 3 | Emptiness decision, or an authorized wipe | S3 data plane, with the operator's admin credential | the bucket does not exist |
| 4 | Delete the access keys of the attributed credentials group, then the group | control plane | the bucket does not exist, its ownership tags are not this CR's, or it attributes no group |
| 5 | Delete the bucket | control plane | the bucket does not exist, or its ownership tags are not this CR's |
| 6 | Delete the workload credentials Secret | the CR's own namespace | the CR names the operator's admin Secret (refused as defence in depth) |
| 7 | Drop the finalizer, releasing the CR | the CR | steps 1–6 did not all complete |

Two properties of this order are load-bearing, both from
[ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D3:

* **The clone is stopped first**, so nothing keeps writing into the bucket while the
  rest of the teardown runs. See [cloning.md](cloning.md).
* **The emptiness decision comes before any credential is removed.** A blocked
  deletion therefore leaves the workload completely functional: its access key is
  still live, its Secret is untouched, and it is still the credential you would use
  to empty the bucket by hand.

The physical bucket acted on is the frozen resolved name, not `spec.bucketName`:
`status.resolvedBucketName` if set, otherwise the annotation backup the reconcile
writes before the status (so a Bucket that crashed between the two is still torn
down by the right name), otherwise the raw `spec.bucketName`
([`api/v1/bucket_types.go`](../../api/v1/bucket_types.go), `EffectiveBucketName`) —
see [bucket-naming.md](bucket-naming.md). If the bucket no longer exists in the
provider, steps 3–5 are all skipped, the Secret is deleted and the CR is released —
and the credentials group is left behind, reported as
`CredentialsGroupNotAttributable`, because without its bucket nothing can prove
which group belongs to this CR (see
[The credentials group left behind](#the-credentials-group-left-behind)). This is
why a `Bucket` reporting `BucketMissing` tears down cleanly instead of hanging on
its finalizer, and therefore why delete-and-re-apply is a way back from a vanished
bucket — [vanished-buckets.md](vanished-buckets.md).

### Deleting a namespace

A namespace deletion deletes the `Bucket` CRs in it, and every rule on this page
applies unchanged. A namespace that holds a Bucket with data in it will sit in
`Terminating` until the Bucket's deletion is unblocked. That is the guard working,
not a stuck namespace controller.

---

## The emptiness guard

A bucket that holds at least one current object is not deleted
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D2). The
teardown returns an error, the finalizer stays, and nothing in steps 4–7 runs.

What you see:

| Signal | Value |
| --- | --- |
| The CR | still present, finalizer intact, `metadata.deletionTimestamp` set |
| `status.phase` | `Failed` after the first attempt (`Deleting` only between the attempt starting and its refusal) |
| `status.message` | `bucket "<resolved name>" is not empty; refusing to delete (data-loss guard)` |
| Event | `Warning` / `Failed` / `refusing to delete non-empty bucket`, plus a second `Warning` / `Failed` carrying the full message; both repeat on every retry |
| Metric | counted in `stackit_s3_provisioner_buckets{phase="Failed"}` |
| Alert | `StackitS3BucketFailed`, after 15 minutes, if the chart's opt-in rule set is installed |

There is no timeout after which the operator gives up and deletes anyway, and no
setting that shortens the wait
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D8). The phase
is written to `Failed` on the first refusal and then deliberately not rewritten, so a
blocked delete does not oscillate between `Deleting` and `Failed` and retrigger itself.

**The alert you would expect is not the one that fires.** `StackitS3BucketStuckDeleting`
keys on `status.phase == "Deleting"` for 30 minutes, and a blocked delete leaves that
phase almost immediately. What actually fires is `StackitS3BucketFailed`, whose text is
about configuration faults. This is recorded as a residual risk in
[ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md); see
[monitoring.md](monitoring.md).

**The guard counts current objects only.** It asks the data plane for a single current
object and treats "none" as empty; it does not list versions. A versioned bucket whose
current objects have all been deleted reads as empty and the deletion proceeds, even
though non-current versions and delete markers still hold data and still cost storage.
*Not verified:* what the provider does with that bucket deletion — whether it refuses,
in which case the delete blocks with a provider error instead of the guard's message,
or accepts and discards the versions.

---

## Runbook: a delete that will not finish

### 1. Read the object

```bash
kubectl -n <namespace> get bkt <name> -o wide
kubectl -n <namespace> describe bkt <name>   # the events carry the reason
```

`bkt` is the CRD's short name. The `Phase` and `Status` columns are `status.phase` and
`status.message`; `Resolved` (a wide column) is the physical bucket name.

### 2. Decide which block you are looking at

| What you see | What it is | Where to go |
| --- | --- | --- |
| `status.message` names a non-empty bucket, `Warning`/`Failed` events repeating | the emptiness guard | step 3 |
| `status.message` is `releasing StackIT resources` and nothing has changed since, no new events | teardown never reached the provider — the fleet-wide circuit is open | step 4 |
| `status.phase` is `Failed` with an older message and no new events arrive | an earlier refusal, now additionally held by an open circuit | step 4, then step 3 |
| Event `Warning`/`WipeOnDeleteSkipped` | a requested wipe was refused and the delete fell back to the guard | [The wipe path](#the-wipe-path) |
| Event `Warning`/`Failed` / `not deleting bucket: it is not owned by this operator` | ownership tags do not match; the bucket is left standing | [What survives a delete](#what-survives-a-delete) |
| Event `Warning`/`CredentialsGroupNotAttributable` | the CR was released but a credentials group was left behind | [The credentials group left behind](#the-credentials-group-left-behind) |

The two blocks look different on the object because a held teardown is **silent**: it
writes no event and no `status.message`
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D4). An absence of
new events on an object whose `deletionTimestamp` is minutes old is itself the signal.

### 3. Get past the emptiness guard

Two supported ways, and one that is not.

**Empty the bucket with the workload's own credential.** It is still live — the guard
runs before any credential is removed — and it is in the Secret named by
`spec.secretRef.name` in the CR's namespace:

```bash
kubectl -n <namespace> get secret <secretRef.name> \
  -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d        # default key names;
kubectl -n <namespace> get secret <secretRef.name> \
  -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d    # overridable per CR via
                                                            # spec.secretRef.keys
```

Then remove the objects with any S3 client against the endpoint in the same Secret
(path-style addressing, SigV4). The next reconcile of the CR — it is already queued on
a backoff — completes the teardown on its own. Note that the workload credential may
delete **current objects** but not object versions: `s3:DeleteObjectVersion` is not in
the workload's allowed actions
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)). Since
the guard counts current objects only, that is enough to unblock the delete.

**Or ask for a wipe**, if the cluster's gate allows it — see
[The wipe path](#the-wipe-path). `spec.wipeOnDelete` is mutable, so it can be set on a
CR that is already being deleted.

**Removing the finalizer by hand is not a fix.** It works, it is not blocked, and it
releases the CR — at the price of leaving the bucket, its credentials group and a live
access key standing in the cloud with nothing left in the cluster naming them. If you
do it, write down the resolved bucket name and `status.credentialsGroupID` first,
because they are about to stop being visible anywhere.

### 4. A delete held by a provider outage

While the fleet-wide provider circuit is open the operator makes **no provider call at
all**, teardown included. The finalizer stays, no phase changes, and the deletion
resumes at the next successful probe
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D4). This is
verified in the offline suite: a teardown attempted under an open circuit makes no
delete call, returns no error, requeues on the breaker's cooldown, keeps the
finalizer, and completes once the provider answers again.

Confirm it with the operator, not with the object:

```bash
# 1 while the operator has stopped calling the provider
stackit_s3_provisioner_provider_circuit_open
# present only while the circuit is open; the moment the outage started
stackit_s3_provisioner_provider_circuit_opened_timestamp_seconds
```

The operator also logs `provider circuit open; deferring teardown` with the bucket
name and the retry delay. It is a debug-verbosity line; the operator's default logging
is development mode, under which debug lines are emitted (`cmd/main.go`). *Not
verified against the pinned controller-runtime release:* the debug-by-default
behaviour was read in an older version of that library present locally.

There is nothing to do here but wait, or fix the provider. Do not remove the finalizer
to "help" — the resources are still there and the teardown will run. See
[provider-outages.md](provider-outages.md).

The same deferral applies when the emptiness decision cannot be made at all because
the operator's admin credential is unavailable
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D9, and
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) for the
credential itself): not being able to check is never treated as "empty". The teardown
fails with the underlying error and retries.

---

## The wipe path

`spec.wipeOnDelete: true` replaces the emptiness decision with a full wipe: every
object, **every non-current version and every delete marker** is deleted first, and the
bucket is removed afterwards
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D6; verified in
[`stackit/s3.go`](../../stackit/s3.go), `WipeBucket`, which lists with versions
included). A wipe on an already-empty bucket does nothing.

### Three authorizations, and all three every time

| # | Authorization | Held by | Where |
| --- | --- | --- | --- |
| 1 | `spec.wipeOnDelete: true` | whoever writes the manifest | the `Bucket` CR |
| 2 | the operator-wide gate | whoever installed the operator | `--enable-wipe-on-delete` / env `ENABLE_WIPE_ON_DELETE` / Helm `wipeOnDelete.enabled`, `false` # default |
| 3 | the bucket's ownership tags name this operator and this CR | nobody — it is proven, not granted | the bucket's tags in the provider |

Two out of three is a refusal, not a wipe
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D4).

### Turning it on

```yaml
# values.yaml for the operator release — authorization 2
wipeOnDelete:
  # Off by default. Turning it on does not wipe anything by itself; it only lets
  # a Bucket CR in any namespace of this cluster ask for a wipe. Leave it off
  # unless the cluster is one where buckets are genuinely disposable.
  enabled: true
```

```yaml
# the Bucket CR — authorization 1
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: scratch
  namespace: team-a
spec:
  bucketName: scratch
  wipeOnDelete: true    # default false: deletion is blocked while data exists.
                        # Mutable, so it can be set immediately before deleting
                        # the CR rather than carried for the bucket's whole life.
  secretRef:
    name: scratch-s3
```

**Verify the gate is actually on before relying on it**, because a wipe that is not
authorized fails at delete time, which is the worst moment to find out:

```bash
# select by label: the Deployment is named <release>-stackit-s3-provisioner, not
# <release>, unless the release name already contains the chart name
kubectl -n <operator-namespace> get deploy -l app.kubernetes.io/name=stackit-s3-provisioner \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' | tr ',' '\n' | grep wipe
# expected: "--enable-wipe-on-delete"   (containers[0] is the `manager` container)
```

or, from the metrics endpoint, `stackit_s3_provisioner_wipe_on_delete_gate_enabled`
is `1`.

### What happens when only some authorizations hold

| Situation | Outcome |
| --- | --- |
| All three | `Normal` / `WipingBucket` event, `status.message` becomes `wiping bucket "<name>" before deletion`, objects and versions are deleted, then the bucket |
| Gate off | `Warning` / `WipeOnDeleteSkipped`: *requested but the wipe feature is disabled by operator config*; falls back to the emptiness guard, so the delete blocks on the data it was asked to destroy |
| Ownership tags do not match | `Warning` / `WipeOnDeleteSkipped`: *refusing to wipe: bucket is not owned by this operator*; falls back to the emptiness guard |
| `spec.wipeOnDelete` false, gate on | no wipe, ever — the gate alone authorizes nothing |

A refused wipe degrades to the guard, never to silence and never to a partial wipe
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D5).

### Living with the gate on

Two alerts exist for exactly this, both enabled inside the chart's rule set, which is
itself opt-in (`monitoring.prometheusRule.enabled`, `false` # default — a default
install ships no `PrometheusRule` and therefore no page):

| Alert | Fires | Means |
| --- | --- | --- |
| `StackitS3BucketsWipeOnDelete` | after 5m, while any CR carries `spec.wipeOnDelete=true` | somebody is one `kubectl delete` away from irreversibly destroying a bucket's contents |
| `StackitS3WipeRequestedButGateDisabled` | after 15m, when a CR asks for a wipe the gate does not permit | the manifest promises something the cluster will not do; the first delete will block instead |

There is no dry run and no confirmation at delete time. Once all three authorizations
hold, deleting the CR destroys the contents. The wipe is also not re-checked for
emptiness afterwards: objects written between the wipe and the bucket deletion are not
seen, and the delete either succeeds having discarded them or fails and the whole
teardown is retried.

---

## The credentials group left behind

Teardown deletes only the credentials group that the **bucket itself** attributes — via
its `credentials-group-id` tag, or failing that via the principal in its own isolation
policy ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)
D4). The id recorded in `status.credentialsGroupID` is never a source of deletion: a
group found by any other means might belong to another Bucket, and deleting its keys
would be an outage in a foreign namespace.

So there are cases where the CR is released and a group remains:

* the physical bucket no longer exists, so nothing can prove the attribution
  ([vanished-buckets.md](vanished-buckets.md));
* the bucket exists but carries no attribution, and no isolation policy naming a
  principal;
* the bucket's ownership tags are not this CR's.

When that happens **and** `status.credentialsGroupID` named a group that still exists,
the operator raises `Warning` / `CredentialsGroupNotAttributable` with the group id in
the message, precisely so an operator can clean it up by hand. When the recorded group
is already gone the teardown stays silent — that is the ordinary second pass after a
requeue, not a leak.

**What to do:** the event message carries the group id. Delete the group's access keys
and then the group in the STACKIT console or via the API; a group cannot be deleted
while it still has keys. Do this only after confirming nothing else uses it — the
event is raised exactly because the operator could not make that determination itself.
See [ownership-and-attribution.md](../security/ownership-and-attribution.md) for how
far the attribution proof reaches.

---

## What survives a delete

| Thing | After a completed teardown | After a blocked one |
| --- | --- | --- |
| The `Bucket` CR | gone | present, `deletionTimestamp` set, finalizer intact |
| The bucket | gone, unless its ownership tags were not this CR's | untouched |
| Its objects | there were none — the bucket had to be empty, or an authorized wipe emptied it | untouched |
| The credentials group and its access keys | gone, if the bucket attributed the group | untouched, and the key still works |
| The workload Secret | deleted | untouched |
| The clone Job and its staging Secret | deleted | deleted — step 1 runs regardless |
| The operator's admin group, key and Secret | never touched by a Bucket teardown | never touched |

Three cases leave cloud state standing on purpose:

**A bucket whose ownership tags are not this CR's** is never deleted and never wiped
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D7). The teardown
reports it (`Warning` / `Failed`, *not deleting bucket: it is not owned by this
operator*) and continues, so an **empty** foreign bucket ends with the CR released and
the bucket standing. A **non-empty** foreign bucket does not get that far: the
emptiness decision runs before ownership is considered, so it blocks like any other
non-empty bucket and the CR is never released. That is stricter than D7's wording,
which describes teardown continuing past a foreign bucket; the code is correct and D7
holds only for the empty case. Ownership is decided by two tags that must both match —
the operator's configured identity and the CR's `namespace/name`; see
[configuration.md](configuration.md) for what changing the operator identity does to
buckets that already exist.

**An operator running without a service-account key** (skeleton mode) drops the
finalizer immediately and touches nothing: there is no cloud client to check emptiness
with. Nothing is destroyed, but nothing is cleaned up either, and the record of what
exists in the cloud goes away with the CR. This matters when a cluster is rebuilt or
re-installed without its key —
[ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) has the
deployment model behind that mode.

**A finalizer removed by hand** defeats every rule on this page. It is a legitimate
operator action and it is not blocked; it leaves live cloud resources unreferenced.

---

## Related

| Page | Why |
| --- | --- |
| [bucket-status.md](bucket-status.md) | reading phases, conditions and the columns used throughout this page |
| [provider-outages.md](provider-outages.md) | the on-call view of the open circuit that defers a teardown |
| [vanished-buckets.md](vanished-buckets.md) | deleting a `Bucket` whose bucket is already gone, and the group that survives it |
| [monitoring.md](monitoring.md) | the metrics and alerts named here, and their tuning constraints |
| [cloning.md](cloning.md) | the clone that step 1 of the teardown stops |
| [credentials.md](credentials.md) | the workload credential you use to empty a bucket by hand |
| [bucket-naming.md](bucket-naming.md) | which physical bucket a teardown acts on |
| [../developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md) | the teardown pass in code: order, guards and status writes |

Decisions behind this page:
[ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) (the guard and the
wipe exception),
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D4
(which group may be released),
[ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D4 (the deferred
teardown),
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) (why a Bucket being
torn down never holds readiness through a failure, so a blocked delete is always
visible).
