# Reading a Bucket

Everything the operator knows about a `Bucket` is on the object: a coarse phase,
a set of conditions, a one-line message, and a block of observed values. This
page is how to read them — which column comes from which field, what each phase
and each reason means, which faults park a CR until somebody edits it, and what
it means when the status and the cloud disagree. For the complete field list of
the CRD, see the reference in [README.md](../../README.md); this page explains
the fields it does not restate.

The operator writes nothing but `status` and two pieces of metadata
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D1, D3),
so everything below is an observation — never a change to what you applied.

## A Bucket and the commands that read it

```yaml
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: artifacts                 # example
  namespace: team-a               # example
spec:
  bucketName: artifacts           # example - the REQUESTED name; the physical
                                  # name is composed from it and then frozen,
                                  # see bucket-naming.md
  region: eu01                    # default - must equal the operator's region
  secretRef:
    name: artifacts-s3            # example - Secret written in THIS namespace
```

```bash
kubectl -n team-a get bkt                       # bkt is the short name
kubectl -n team-a get bkt -o wide               # adds the six priority columns
kubectl -n team-a describe bkt artifacts        # conditions + the event stream
kubectl -n team-a get bkt artifacts -o jsonpath='{.status}' | jq   # everything

# Has the operator seen the generation you just applied?
kubectl -n team-a get bkt artifacts \
  -o jsonpath='{.metadata.generation}/{.status.observedGeneration}{"\n"}'
```

The verify step after any edit is the last command: `status.observedGeneration`
equal to `metadata.generation` means the operator has processed what you applied.

Only the *terminal verdict* is stamped with the generation. `observedGeneration`
is advanced by the three writes that end a pass — the success write, the failure
write and the skeleton-mode write — so while the two numbers differ, the phase
`Ready` or `Failed` and the conditions still belong to the previous generation.
`status.message` and an intermediate `Provisioning` phase are not stamped: they
are written by steps *inside* the current attempt. A Bucket that has been cloning
for hours therefore shows a current-generation message (`cloning from …`) while
`observedGeneration` still lags, and that is correct, not stale.

## What `kubectl get` shows

The columns come from the printer markers on the CRD
([api/v1/bucket_types.go](../../api/v1/bucket_types.go), regenerated into
[config/crd/bases](../../config/crd/bases/)). Fourteen are declared; six carry
`priority=1` and appear only with `-o wide`. `kubectl` prepends `NAME`, the CR's
own name.

Plain — a measured bucket and, below it, one without measurement:

```
NAME        BUCKET      PHASE   READY   STATUS                                                       REGION   SIZE       COST/MONTH   AGE
artifacts   artifacts   Ready   True    bucket "prod-team-a-artifacts" provisioned with isolated...  eu01     18.0 GiB   0.53 EUR     4d
logs        logs        Ready   True    bucket "prod-team-a-logs" provisioned with isolated...       eu01                             2h
```

(The `STATUS` cell is shortened here to keep the block readable. The API server
sends `status.message` verbatim — the CRD table conversion applies no length cap
— but what the `kubectl` client printer does with a long cell is not verified
from this repository.)

Wide — the same first row:

```
NAME        BUCKET      PHASE   READY   STATUS  REGION   SIZE       COST/MONTH   DEGRADED   CLONE   RESOLVED                SECRET          OBJECTS   MEASURED   AGE
artifacts   artifacts   Ready   True    ...     eu01     18.0 GiB   0.53 EUR                        prod-team-a-artifacts   artifacts-s3    12043     14m        4d
```

| Column | Source | Wide only | Notes |
|---|---|---|---|
| `BUCKET` | `spec.bucketName` | no | What you asked for, not what exists in the cloud |
| `PHASE` | `status.phase` | no | See [Phases](#phases) |
| `READY` | the `Ready` condition's `status` | no | `True`/`False`; empty until the first *terminal* write — a Bucket in phase `Provisioning` has no `Ready` condition yet, see [Conditions](#conditions) |
| `STATUS` | `status.message` | no | Current step, or the failure text |
| `REGION` | `spec.region` | no | Always populated — the CRD defaults it to `eu01` |
| `SIZE` | `status.usage.humanReadable` | no | Empty unless measurement is on; a `>=` prefix means the object cap was hit |
| `COST/MONTH` | `status.usage.estimatedMonthlyCost` | no | Empty unless measurement is on and a price is configured |
| `DEGRADED` | `status.degradedSince` | yes | Rendered as a timestamp, not an age (the marker types it as a string) |
| `CLONE` | `status.clone.progress` | yes | Only while a clone runs or after one finished |
| `RESOLVED` | `status.resolvedBucketName` | yes | The physical bucket name, written at the end of the first successful pass |
| `SECRET` | `spec.secretRef.name` | yes | The Secret in the Bucket's own namespace |
| `OBJECTS` | `status.usage.objects` | yes | Current objects at the last measurement |
| `VERIFIED` | `status.lastVerifiedTime` | yes | Age of the last successful pass, changed or not — advances every drift resync ([ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md) D4) |
| `MEASURED` | `status.usage.lastMeasurementTime` | yes | Rendered as an age (`14m`) — this marker is typed as a date |
| `AGE` | `metadata.creationTimestamp` | no | Age of the CR, not of the cloud bucket |

`RESOLVED` being empty while the bucket is being provisioned for the first time
is normal: the name is frozen into the annotation
`stackit-bucket.gtrfc.com/resolved-bucket-name` *before* anything is created in
the cloud, and copied into `status.resolvedBucketName` only by the terminal
success write ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3).
The annotation is the durable one — read it when status has been lost. See
[bucket-naming.md](bucket-naming.md).

## Phases

`status.phase` is the coarse, human-readable summary; the conditions carry the
machine-readable truth. The CRD restricts it to five values.

| Phase | What it means | What happens next |
|---|---|---|
| `Pending` | Skeleton mode only: the operator runs without a service-account key and provisions nothing ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D6) | Nothing, until a key is configured and the operator restarts |
| `Provisioning` | A pass is working through the cloud steps, or a clone is running | `Ready` or `Failed` at the end of the pass; a clone re-writes progress every 15s |
| `Ready` | Bucket, credentials group, access key, isolation policy and Secret were all verified in the last completed pass | Re-verified on every event and on the drift-resync timer ([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D8) |
| `Failed` | The last pass failed, or a teardown is blocked. `status.message` says why | Retried with backoff, unless it is a configuration fault — then nothing until the spec changes |
| `Deleting` | The finalizer teardown is running | `Deleting` until the cloud state is gone; `Failed` if the teardown is refused ([deletion.md](deletion.md)) |

Two things this list does *not* do. It does not start at `Pending` outside
skeleton mode — a freshly created `Bucket` has no phase at all until the first
status write, which is `Provisioning` unless a configuration fault is caught
first, in which case it is `Failed`. Four of the six faults below are checked
*before* the `Provisioning` marker is written — a Secret key collision, a
`secretRef` pointing at the admin Secret, a foreign region and an invalid
composed name — so for those CRs `Failed` is the first phase the object ever
shows. And it is not re-entered once settled: a
`Bucket` that is `Ready` or `Failed` for the generation it has already observed
is not flipped back to `Provisioning` by a re-reconcile, because that write would
wake the controller again through its own watch.

A blocked delete stays visible for the same reason: once teardown has recorded
`Failed`, the phase is not rewritten to `Deleting` on every retry, so a bucket
that refuses to delete does not flip-flop between the two.

## Conditions

| Type | When present | `status` values | Reasons |
|---|---|---|---|
| `Ready` | From the first *terminal* write — the success write, the failure write, or the skeleton-mode write. Absent for the whole first `Provisioning` phase | `True`, `False` | `Provisioned`, `Provisioning`, `Failed`, `NotImplemented`, `BucketMissing` |
| `CloneCompleted` | Only on a `Bucket` with `spec.cloneFrom`; written as soon as the copy Job reports, so on a first provisioning it can appear before `Ready` does | `True`, `False` | `Cloning`, `Cloned`, `CloneFailed` |
| `ProviderReachable` | From the first hold until the next successful reconcile — including after the hold has been given up | `False` | `ProviderUnreachable` |
| `BucketPresent` | From the first report that a provisioned bucket is gone until the next successful reconcile | `False` | `BucketMissing` |

An empty `READY` cell therefore does not mean "the operator has not written
anything yet". The write that flips the phase to `Provisioning` sets only
`status.phase` and `status.message` and no condition at all; it is a progress
hint, deliberately cheap, and the terminal write at the end of the pass is what
carries the authoritative verdict. A Bucket showing `PHASE=Provisioning` with an
empty `READY` is a Bucket mid-pass, not a Bucket the operator has ignored — use
`status.message` to see which step it is on.

`ProviderReachable` outlives the hold it records. It is removed only by a
successful reconcile, so when the grace elapses and the Bucket drops to `Failed`,
the condition stays `False` on it until the provider answers again. The degraded
*metric* deliberately diverges at exactly that moment: it counts a Bucket only
while the phase is still `Ready`, because after the drop the hold is over and the
`Failed` phase gauge covers it. See [monitoring.md](monitoring.md).

`ProviderReachable` is deliberately absent rather than `True` on a healthy
Bucket: a Bucket that never degraded and one that recovered look identical, and
an operator upgrade writes nothing to Buckets that are simply healthy
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D4).

The same rule holds for `BucketPresent`, for the same reason: it is only ever
`False`, and a successful reconcile *removes* it instead of setting it to `True`,
so a Bucket whose bucket came back and one that never lost it are
indistinguishable
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D5).
Reading the two together is what separates the two failures: `ProviderReachable`
missing with `BucketPresent=False` means the provider answered, and answered that
the bucket is gone — see [vanished-buckets.md](vanished-buckets.md).

| Reason | On | Meaning |
|---|---|---|
| `Provisioned` | `Ready=True` | Everything verified in the last completed pass |
| `Provisioning` | `Ready=False` | Declared in the API but **never written** by the operator today (verified: no code path sets it). Work in progress is carried by `status.phase`, and the condition keeps its previous value until the pass ends |
| `Failed` | `Ready=False` | The last pass failed, and either the failure was definitive or the grace window has elapsed |
| `NotImplemented` | `Ready=False` | Skeleton mode — no service-account key, no cloud call |
| `Cloning` | `CloneCompleted=False` | The rclone Job is copying; `status.clone.progress` is refreshed every 15s |
| `Cloned` | `CloneCompleted=True` | Terminal — the clone never runs again for this Bucket ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D5) |
| `CloneFailed` | `CloneCompleted=False` | The Job failed; it is deleted and retried with backoff, and rclone resumes |
| `ProviderUnreachable` | `ProviderReachable=False` | `Ready` is being held through failures that say nothing about this Bucket |
| `BucketMissing` | `Ready=False`, `BucketPresent=False` | The bucket this Bucket was provisioned with is gone at the provider, and the operator refuses to re-create it over the workload ([vanished-buckets.md](vanished-buckets.md)) |

### `Ready=True` does not mean the last attempt succeeded

On a provisioned `Bucket`, `Ready` reports the last state the operator
*verified*, not the outcome of the last attempt to verify it
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D1). While
the provider is unreachable, the Bucket keeps `Ready=True` and gains
`status.degradedSince` plus `ProviderReachable=False`; only after
`--provider-degraded-grace` (`30m` # default) does it drop to `Failed`, keeping
`degradedSince` so the status still says when the trouble began
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D5).

A consumer that needs "the last attempt succeeded" — a health gate, a deployment
check — must read `ProviderReachable` or the degraded metrics, never `Ready`.
The whole mechanism, what to tune and what it looks like during an outage, is in
[provider-outages.md](provider-outages.md).

Seven cases skip the hold and drop `Ready` immediately, whatever the grace says:
the configuration faults below, a structured `400`/`401`/`403` from the
provider, a workload credential the operator itself destroyed and could not
replace, a Bucket being deleted, a Bucket whose spec has not been observed yet,
a Bucket that has never been `Ready`, and a provisioned Bucket whose bucket the
provider answers for as gone
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D6, whose
seventh case was added by
[ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)).

## Configuration faults park the CR

Six faults are statements the operator established *locally* about this Bucket.
Retrying cannot fix any of them, so the reconcile ends without requeueing: phase
`Failed`, `Ready=False` with reason `Failed`, the failure text in
`status.message`, `status.observedGeneration` advanced, and a `Warning` event
with reason `Failed` ([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D5).
Nothing happens until the spec changes — which is the point: the CR sits there
with the reason on it instead of hammering the API.

| Fault | `status.message` starts with | What to change |
|---|---|---|
| Colliding Secret data keys | `secretRef.keys: "x" and "y" both map to data key "K"` | Two logical fields of `spec.secretRef.keys` resolve to the same key name; give them distinct names ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D10) |
| Admin Secret targeted | `secretRef <ns>/<name> targets the operator's admin credentials Secret` | `spec.secretRef.name` names the operator's own admin Secret and the Bucket lives in the operator namespace; point it elsewhere ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D8) |
| Foreign region | `spec.region "x" does not match this operator's region "eu01"` | One deployment serves one region; either set the operator's region or deploy a second operator ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D3, D4) |
| Invalid composed name | `composed bucket name is invalid` | Prefix plus namespace plus `spec.bucketName` left the 3–63 character DNS range; shorten the name or the naming policy ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D5) |
| Ownership collision | `bucket "x" already exists and is not owned by this operator` | A bucket of that name exists whose ownership tags are not this Bucket's, or it is untagged and not empty. Change the name so a different one is composed — there is no operator-side adoption switch ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D2) |
| Self-clone | `cloneFrom points at the bucket itself` | `spec.cloneFrom` names this very bucket at this very endpoint ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D6) |

The ownership collision additionally emits its own `Warning` event
(`bucket ownership collision: a bucket with this name exists but was not
provisioned by this operator`) before the CR is parked.

The operator adopts an existing bucket in exactly two cases, neither of which you
can ask it for: both ownership tags match — `managed-by` equal to the operator's
configured ownership name, `owner` equal to `<namespace>/<name>` of this CR — or
the bucket carries no tags at all *and* is empty, which is the operator's own
crash between create and tag-write. Setting those two tags on somebody else's
bucket out of band, with the admin key, would make the next pass adopt it; that
is a deliberate hand-over of the data, not a repair, and the safe fix is a
different name. The tags are described in
[bucket-naming.md](bucket-naming.md).

Verify a fix: apply the corrected spec and watch `status.observedGeneration`
catch up to `metadata.generation`. The edit bumps the generation, which is the
trigger the operator registers for. An operator restart, an annotation change or
one of the operator's other watches also re-runs the pass — but with the same
result, because the fault is in the spec: the CR parks again until the spec
changes. What a parked CR is *not* is retried on the drift-resync timer, which
only applies after a successful reconcile
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D5, D7, D8).

## The status fields

| Field | What it holds | Explained in |
|---|---|---|
| `phase` | The coarse lifecycle state | [Phases](#phases) |
| `message` | The current step, or the failure text | this page |
| `observedGeneration` | The `metadata.generation` last reconciled | this page |
| `resolvedBucketName` | The frozen physical bucket name | [bucket-naming.md](bucket-naming.md) |
| `bucketURL` | The path-style S3 URL of the bucket | [README.md](../../README.md) — the same value is published into the Secret |
| `credentialsGroupID` | The workload credentials group backing the access key | [credentials.md](credentials.md) |
| `credentialsGroupURN` | That group's URN, published as soon as the group exists so a grantor can resolve it | [read-grants.md](read-grants.md) |
| `accessKeyID` | The S3 access key id in use. The secret half is **never** in status, in an event or in a log ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D12) | [credentials.md](credentials.md) |
| `grantedReadTo` | The `spec.grantReadAccess` entries actually in the policy right now | [read-grants.md](read-grants.md) |
| `clone` | Phase, timestamps, bytes, progress, rate, ETA of the one-shot copy | [cloning.md](cloning.md) |
| `lastRotationTrigger` / `lastRotationTime` | The rotation annotation value already acted upon, and when | [credentials.md](credentials.md) |
| `lastVerifiedTime` | When the last successful pass over this Bucket completed, whether or not it changed anything. The per-object proof that the drift resync is running, now that an unchanged pass raises no event ([ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md) D4) | [configuration.md](configuration.md#verify-it-is-running) |
| `degradedSince` | When the current run of non-definitive failures began | [provider-outages.md](provider-outages.md) |
| `operatorVersion` | The operator version that last wrote this status | this page |
| `usage` | Measured size, object counts, cost estimate, and how old the measurement is | [usage-and-cost.md](usage-and-cost.md) |
| `conditions` | `Ready`, `CloneCompleted`, `ProviderReachable`, `BucketPresent` | [Conditions](#conditions) |

Three of these need a warning about what they are *not*:

- **`grantedReadTo` is the current truth, not a history.** An entry that cannot
  be resolved is absent from it, which is how a pending or revoked grant becomes
  visible without reading the policy out of S3
  ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D9).
  While a clone is still filling the bucket, readers are deliberately kept out of
  the policy altogether
  ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D8).
- **`usage` is a measurement, not a live figure**, and it is as old as
  `usage.lastMeasurementTime` says. `usage.truncated` means the object cap was
  hit and every size and cost value is a lower bound
  ([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D9).
  A failed measurement leaves the previous numbers in place and puts the reason
  in `usage.message`; it never touches `Ready`
  ([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D3, D4).
- **`credentialsGroupID` is not a deletion source.** Teardown deletes only the
  group the *bucket* attributes through its tags or its own policy; a group
  recorded in status but not attributed by the bucket is left standing, with a
  `CredentialsGroupNotAttributable` warning
  ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D4).

### Clone sub-status

`status.clone` exists only on a Bucket that requested a clone. Its `phase` is
`Running`, `Completed` or `Failed`; `Completed` is terminal.

| Field | Meaning |
|---|---|
| `phase` | `Running` / `Completed` / `Failed` |
| `startedAt`, `completedAt` | When the first Job was created, and when the copy finished |
| `totalBytes` | Source size, measured once before the copy starts so the percentage has a stable denominator ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D10) |
| `bytesCopied`, `progress`, `rate`, `eta` | Refreshed every 15s from the Job's rclone remote control; best effort — an unreachable endpoint keeps the previous numbers |
| `message` | The short failure reason while `phase` is `Failed` |

## Events

`kubectl describe bkt <name>` is where a fault that does not stop the reconcile
shows up — a skipped read grant, a refused wipe, a clamped measurement interval.

<details>
<summary>Every event reason the operator emits</summary>

| Reason | Type | Emitted when |
|---|---|---|
| `Provisioned` | Normal | A pass **changed** something — created the bucket, wrote the policy, issued credentials, finished a clone — and the message names what. A pass that only verified raises no event ([ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md) D1/D2); its trace is `status.lastVerifiedTime` |
| `Failed` | Warning | Any failed pass — including one whose `Ready` is being held, so a hold is as visible in the event stream as a hard failure. The one failure that carries a different reason is the vanished bucket below |
| `BucketMissing` | Warning | A bucket that was provisioned is gone at the provider and the operator refuses to re-create it. The event carries the same text as `status.message`, and the same reason as the `Ready` and `BucketPresent` conditions ([vanished-buckets.md](vanished-buckets.md)) |
| `BucketRecreated` | Warning | A vanished bucket was rebuilt automatically because `spec.allowRecreate` is set: the contents are still lost, and the workload Secret is replaced later in the same pass. This reason exists **only** in the event stream — no condition ever carries it, and the CR is simply `Ready` again ([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D9, D13) |
| `CredentialsRotated` | Normal | An annotation-triggered rotation completed |
| `CredentialsGroupAttributed` | Normal | A pre-tag bucket's group was recovered from its own isolation policy and written into the bucket tags |
| `CredentialsGroupNotAttributable` | Warning | Teardown left a credentials group standing because the bucket does not attribute it |
| `ReadGrantPending` | Warning | A `spec.grantReadAccess` entry could not be resolved: the Bucket does not exist yet, is being deleted, has no bucket yet, has no credentials group yet, names a bucket that is not its own, or is a self-reference. The grantor still provisions ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D5) |
| `CloneStarted` | Normal | The rclone Job was created |
| `Cloned` | Normal | The copy finished |
| `WipingBucket` | Normal | An authorized wipe is deleting objects before the bucket is removed |
| `WipeOnDeleteSkipped` | Warning | `spec.wipeOnDelete` was requested but the operator's feature gate is off, or the ownership tags do not authorize it; the delete falls back to the empty-only guard ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D5) |
| `UsageMeasurementDisabled` | Warning | A Bucket asked to be measured and the operator-wide gate refuses |
| `UsageConfigInvalid` | Warning | `spec.usage` carries a value the operator cannot use; measurement parks until the spec changes |
| `UsageIntervalClamped` | Warning | The requested interval was below the operator's floor and was raised |
| `UsageMeasurementFailed` | Warning | A measurement did not complete; the previous numbers stay |
| `UsageMeasurementTruncated` | Warning | The measurement stopped at the object cap; size and cost are lower bounds |

</details>

Fleet-level counterparts exist as metrics — `stackit_s3_provisioner_buckets`
counts Buckets per phase, `stackit_s3_provisioner_buckets_clone` per clone
phase, `stackit_s3_provisioner_buckets_provider_degraded` counts the Buckets
currently held. See [monitoring.md](monitoring.md).

## When the status disagrees with the cloud

`status` describes the last thing the operator verified. Four ways that can be
out of date, and what to do about each:

**The operator has not seen your edit.** `status.observedGeneration` is behind
`metadata.generation`. The terminal verdict — phase `Ready` or `Failed`, and the
conditions — still describes the previous generation; `status.message` and a
`Provisioning` phase describe the attempt running right now. Nothing is wrong;
wait for the pass, or look for a parked configuration fault above.

**The provider is unreachable.** `status.degradedSince` is set and
`ProviderReachable=False`, and `Ready` is whatever was last verified. During a
fleet-wide outage the operator stops calling the provider entirely and holds
every Bucket, writing status only on the first hold and at the moment the grace
runs out ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D4,
D5) — so a Bucket held for hours shows one timestamp and no churn. See
[provider-outages.md](provider-outages.md).

**Somebody changed the bucket policy by hand.** Nothing on the CR says so. The
operator re-asserts its own document whenever it looks and rewrites it on drift
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8),
which the drift-resync timer makes happen without an event
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D8). The
correction is silent — expect the change to disappear, not a status field to
report it.

**The bucket was deleted behind the operator's back.** The status says so, and
nothing is repaired over it. A `Bucket` that completed a provisioning round and
whose bucket the provider answers for as absent goes to phase `Failed` with
`Ready=False` reason `BucketMissing` and a `BucketPresent=False` condition beside
it; the operator refuses to re-create the bucket, so the credentials group and
the workload Secret stay exactly as they were
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D1). Only the provider's own structured answer about that one bucket counts — a
gateway page, a `5xx` or a dropped connection takes the degraded path above
instead. What to check, how to get a working `Bucket` back, and the standing
`spec.allowRecreate` opt-in that waives the refusal are in
[vanished-buckets.md](vanished-buckets.md).

## Where to go next

| You want | Page |
|---|---|
| The complete CRD and Helm key reference | [README.md](../../README.md) |
| Why the physical name differs from `spec.bucketName` | [bucket-naming.md](bucket-naming.md) |
| A delete that will not finish | [deletion.md](deletion.md) |
| Rotating a workload key, or the admin Secret | [credentials.md](credentials.md) |
| What a held `Ready` means on call | [provider-outages.md](provider-outages.md) |
| A bucket that is gone at the provider | [vanished-buckets.md](vanished-buckets.md) |
| Metrics and alerts behind these fields | [monitoring.md](monitoring.md) |
| The clone status block in full | [cloning.md](cloning.md) |
| The measurement behind `status.usage` | [usage-and-cost.md](usage-and-cost.md) |
