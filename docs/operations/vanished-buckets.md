# A bucket that vanished

What happens when a bucket the operator provisioned is no longer at the
provider, how to tell that apart from an outage, and the two ways to get a
working `Bucket` back.

The operator does **not** re-create it
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D1). The data is gone either way; re-creating would hand the workload an empty
bucket under the original name and erase the only evidence that anything
happened.

---

## What you see

```
$ kubectl get bkt -A
NAMESPACE   NAME        BUCKET      PHASE    READY   STATUS                                          REGION   AGE
team-a      my-bucket   my-bucket   Failed   False   bucket "my-bucket" was provisioned for this...  eu01     9d
```

```yaml
status:
  phase: Failed
  message: >-
    bucket "my-bucket" was provisioned for this Bucket but the provider reports
    it as gone; refusing to re-create it, because that would hand the workload an
    empty bucket under the same name. Its contents are lost: delete and re-apply
    this Bucket to provision a fresh one, or set spec.allowRecreate if its content
    is regenerable
  conditions:
    - type: Ready
      status: "False"
      reason: BucketMissing
    - type: BucketPresent
      status: "False"
      reason: BucketMissing
      message: "bucket \"my-bucket\" was provisioned for this Bucket but ..."
```

Plus a Warning event with reason `BucketMissing`, and the per-`Bucket` gauge
`stackit_s3_provisioner_bucket_provisioned_missing{namespace,name}` at `1`,
which is what `StackitS3BucketMissing` (`critical`, after `5m`) fires on. The
existing `StackitS3BucketFailed` (`warning`, after `15m`) fires as well, on the
phase - one incident, two alerts of different severity, the same shape the
degraded alerts already have. See [monitoring.md](monitoring.md).

**Dependent Flux `Kustomization`s go not-ready.** That is intended and is not to
be worked around
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D6): a `Bucket` in this state is genuinely unusable.

## This is not a provider outage, and the conditions say which is which

An unreachable API and a vanished bucket look nothing alike on the object, which
is the entire point of having a second condition:

| `Ready` | `ProviderReachable` | `BucketPresent` | What it is |
| --- | --- | --- | --- |
| `True` | absent | absent | Healthy |
| `True` | `False` | absent | The provider is not answering. The `Bucket` keeps its last verified state - [provider-outages.md](provider-outages.md) |
| `False` | absent | `False` | The provider answered, and answered that the bucket is gone. This page |
| `False` | `False` or absent | absent | An ordinary reconcile failure - [bucket-status.md](bucket-status.md) |

`BucketPresent` is only ever `False`; like `ProviderReachable` it is **removed**
on recovery rather than set to `True`, so a `Bucket` that recovered and one that
never lost its bucket look identical
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D5).

Nothing but the provider's own structured answer for that one bucket can put a
`Bucket` into this state
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D3). A gateway or WAF error page, a `5xx`, a dropped connection and an empty
response body all take the degraded path instead, **including an error page that
carries a `404`** - the right status code with the wrong provenance is still not
an answer.

## Upgrading to a version that has this guard

Nothing happens to a healthy `Bucket`. The guard asks the provider about the
bucket, gets it, and the pass continues exactly as before: no bucket is created,
no credentials group is replaced, no Secret is rewritten, and `BucketPresent` is
not written — it is absent on a healthy `Bucket` rather than `True`, so the
upgrade leaves the object byte-identical in that respect.

Two details worth knowing before a rollout:

* **The guard asks about the *frozen* name, never a recomposed one.** It only
  runs for a `Bucket` that carries `status.resolvedBucketName`, and that is the
  name it checks. So changing `bucketNaming.prefix` or
  `bucketNaming.includeNamespace` in the same upgrade cannot make the operator
  ask about a name the bucket was never provisioned under — which would otherwise
  report data loss on a perfectly healthy fleet
  ([bucket-naming.md](bucket-naming.md),
  [ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D2).
* **A `Bucket` old enough to have no `status.resolvedBucketName` is not guarded
  until its next successful pass.** That field first shipped in **v1.3.0**; it
  does not exist in **v1.2.1** or earlier. A `Bucket` whose last successful
  reconcile predates v1.3.0 is adopted by its ownership tags exactly as before,
  and that pass writes the field, which puts it under the guard from then on. The
  gap is one reconcile wide. To find any such `Bucket` before upgrading, read the
  `RESOLVED` column — it is a declared printer column at `priority=1`, so it needs
  `-o wide`, and an empty cell is a `Bucket` with no frozen name:

  ```sh
  kubectl get bkt -A -o wide
  ```

  Scriptable, for a large fleet:

  ```sh
  kubectl get bkt -A -o json |
    jq -r '.items[] | select((.status.resolvedBucketName // "") == "")
           | "\(.metadata.namespace)/\(.metadata.name)"'
  ```

  The field is `omitempty`, so it is **absent** rather than empty when unset —
  which is why the `// ""` is there and why a `jsonpath` filter comparing it to
  `""` would silently match nothing. Empty output means every `Bucket` is guarded
  from the first pass after the upgrade. A `Bucket` that was never provisioned at
  all also appears here and is not a concern: it has no data to protect.

The cost is one extra control-plane read per reconcile of an already-provisioned
`Bucket`. On a rollout that is one additional call per `Bucket`, subject to the
circuit breaker like every other call.

## What actually happened

Three causes, in rough order of how often they occur:

| Cause | Tell |
| --- | --- |
| Somebody deleted the bucket in the STACKIT console, or a cleanup script did | One `Bucket` affected, or a few. The bucket is absent in the console too |
| The operator was restarted with a service-account key for a **different project** | *Every* provisioned `Bucket` reports it at once. Check the project id the operator logs at startup against the one the buckets live in |
| The bucket was never in this project to begin with (a restore into the wrong environment) | Same fleet-wide shape, but the `Bucket` CRs are also new |

The fleet-wide case is bounded on purpose: this fault does **not** trip the
circuit breaker
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D7), so teardowns and every other provider call keep working, and the whole
fleet recovers on its own once the right key is in place. Nothing has to be
restarted or reset by hand.

## If the bucket comes back, so does the Bucket

A restored bucket, or the correct key re-applied, needs **no human action**. The
`Bucket` is retried on the controller's rate limiter (1s, backing off to a 15
minute cap), so it returns to `Ready` on its own within at most that interval.
The `BucketPresent` condition is removed, the credentials group and the workload
Secret are untouched throughout - they were never replaced while the bucket was
missing
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D1).

To not wait out the backoff:

```sh
kubectl annotate bkt my-bucket -n team-a \
  stackit-bucket.gtrfc.com/poke="$(date -u +%FT%TZ)" --overwrite
```

Any annotation change wakes the controller; the key above is not special and the
operator ignores it.

## Re-creating once: delete the CR and re-apply it

This is the supported way, and it needs no new field
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D8). Deleting a `Bucket` whose bucket is absent completes normally - the
teardown skips the empty-check and the bucket delete rather than hanging on the
finalizer.

```sh
kubectl delete bkt my-bucket -n team-a
kubectl apply -f my-bucket.yaml
```

**Do it as one step, not two.** The credentials Secret is deleted with the CR,
so between the two commands the workload has no Secret at all. Pods that
consumed it through `envFrom` keep the values they started with until they
restart; anything reading it live fails.

Four things to expect afterwards:

| | |
| --- | --- |
| **New credentials** | The workload must re-read its Secret - restart it. Honest, because it was writing into a bucket that no longer exists |
| **Possibly a different bucket name** | The frozen name dies with the CR, so the replacement is composed from the operator's *current* naming policy. If `bucketNaming.prefix` or the namespace-inclusion setting changed since the original provisioning, the new bucket gets a different name ([bucket-naming.md](bucket-naming.md)) |
| **The old credentials group stays** | Nothing can attribute it without its bucket ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D4), so the operator must not remove it. It holds a live access key that can no longer reach anything - delete it in the STACKIT console |
| **An empty bucket** | Nothing is restored. If you have a backup, this is when to put it back |

## `spec.allowRecreate`: a standing authorisation for regenerable content

For a bucket whose contents can be rebuilt - a cache, a mirror, derived
artifacts - the guard can be waived permanently:

```yaml
spec:
  bucketName: my-cache
  allowRecreate: true     # example - this bucket's content is regenerable
  secretRef:
    name: my-cache-s3
```

The operator then re-creates a vanished bucket automatically and unattended, and
the `Bucket` returns to `Ready` in the same pass. It is still an incident and is
still reported
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D9): a Warning event with reason `BucketRecreated`, and the counter
`stackit_s3_provisioner_bucket_recreated_total{namespace,name}`.

**Turn the alert on with the field.** `StackitS3BucketRecreated` ships
disabled (`monitoring.prometheusRule.alerts.bucketRecreated.enabled: false`
# default), because it can only fire where this field is used. Enable it in the
same change that first sets `allowRecreate: true`, or the rebuild is announced
only by an event.

Unlike `spec.wipeOnDelete` this needs no operator-wide gate: a wipe destroys
data that is still there, a re-create only rebuilds what is already gone. Setting
it requires write access to the `Bucket` in its own namespace - the same access
that can delete the CR outright - so it grants nobody anything new.

### The workload will fail with 403 until it restarts

The rebuilt bucket gets a **fresh** credentials group and access key, and the
workload Secret is overwritten
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D10). Until the workload re-reads it, it presents a key for the orphaned group,
which the new bucket's policy does not name. That is what the event and the
counter are for: they are what tells somebody to restart the workload. Plan for
it before setting the field on anything that cannot restart cheaply.

The previous credentials group is left standing here too, with the same manual
cleanup.

A completed `spec.cloneFrom` is **not** re-run by a re-create
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D11): the clone is one-shot and its terminal state lives in the `Bucket`'s
status, which survives. The rebuilt bucket is empty.

### After an unattended re-creation there is no durable record on the CR

Deliberately
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D13). The `Bucket` is simply `Ready` again. The only evidence is:

* the Warning event, for as long as the cluster retains events (one hour by
  default in most clusters), and
* `stackit_s3_provisioner_bucket_recreated_total`, for as long as Prometheus
  retains it - and the counter restarts at zero when the operator restarts.

If either matters to you beyond that window, ship the event to your log store.

## What this does not cover

* **It does not detect a bucket that is still there but was emptied.** The guard
  asks whether the bucket exists, not what is in it. A bucket whose objects were
  deleted reports `Ready`, with `stackit_s3_provisioner_bucket_size_bytes`
  dropping to zero if size measurement is on ([usage-and-cost.md](usage-and-cost.md)).
* **It does not protect the teardown.** Deletion still decides whether the
  bucket exists from the project-wide listing, not from the per-bucket read
  ([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md),
  Residual risks). If that listing ever answers incompletely, a teardown can skip
  the empty-check and drop the finalizer on a bucket that is still there.
* **It does not restore anything.** Backups are out of scope for this operator.

## Related

* [provider-outages.md](provider-outages.md) - the other failure this is
  carefully not
* [bucket-status.md](bucket-status.md) - every phase, condition and reason
* [monitoring.md](monitoring.md) - the two series and the two alerts
* [deletion.md](deletion.md) - what deleting a `Bucket` does
* [ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) -
  the decision and what it rejected
