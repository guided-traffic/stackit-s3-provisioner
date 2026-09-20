# Cloning a bucket

A `Bucket` can be seeded once from an existing S3 bucket — another STACKIT
project, AWS, a self-run MinIO or Ceph, anything that speaks S3 — by declaring
`spec.cloneFrom`. The copy runs as an rclone Job in the **operator's** namespace
after the bucket and its isolation policy exist, and it runs exactly once: when
`status.clone.phase` reaches `Completed` the clone is finished for the lifetime
of that `Bucket`, and later edits to `spec.cloneFrom` do nothing
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D1, D5).

This page is about declaring, watching and troubleshooting a clone. The complete
field list of `spec.cloneFrom` and of the `clone.*` Helm values lives in
[README.md](../../README.md) and nowhere else; the internals are in
[docs/developer/clone.md](../developer/clone.md).

---

## A working minimal configuration

```yaml
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: my-bucket
  namespace: team-a
spec:
  bucketName: my-bucket           # example: the bucket this operator provisions
  secretRef:
    name: my-bucket-s3            # example: the workload credential this operator writes
  cloneFrom:
    endpoint: object.storage.eu01.onstackit.cloud  # example: bare host (https assumed) or a full URL
    bucket: seed-data                              # example: the SOURCE bucket name
    secretRef:
      name: seed-data-creds       # example: Secret in THIS namespace, read access to the source
    # region: eu01                # optional; set it when the source needs a region for SigV4
    #                             # (eu01 for STACKIT, eu-central-1 for AWS)
    # addressingStyle: path       # default; use virtual-hosted for AWS-style endpoints
    # holdSecretUntilCloned: true # default; false publishes the workload Secret immediately
```

Required: `endpoint`, `bucket`, `secretRef.name`. The rest is optional, and only
two of them carry a default: `addressingStyle` (`path`) and
`holdSecretUntilCloned` (`true`). `region` and `secretRef.keys` are optional
*without* a default — left unset they are simply empty, and the copier is
configured without them (verified against the CRD markers in
[api/v1/bucket_types.go](../../api/v1/bucket_types.go)).

Apply it like any other `Bucket`; nothing else has to be created. Verify:

```bash
kubectl -n team-a get bkt my-bucket -o wide          # CLONE column carries the progress
kubectl -n team-a get bkt my-bucket \
  -o jsonpath='{.status.clone.phase}{"\n"}'          # Running → Completed
```

The bucket is finished when `Ready=True` **and** `CloneCompleted=True`. On a
freshly created `Bucket` both flip together; on a `Bucket` that was already
`Ready` before `cloneFrom` was added, `Ready` never left `True` and only
`CloneCompleted` is meaningful — see
[Adding `cloneFrom` to a `Bucket` that is already provisioned](#adding-clonefrom-to-a-bucket-that-is-already-provisioned).

---

## Where the source credentials must live

`spec.cloneFrom.secretRef` names a Secret in the **`Bucket`'s own namespace**.
There is no `namespace` field, and there will not be one: a reference the
operator follows on a CR author's behalf would be a read that author cannot
perform themselves ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D3/D4,
[ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D3).

| Logical field | Default data key | Override with |
|---|---|---|
| source access key id | `AWS_ACCESS_KEY_ID` | `spec.cloneFrom.secretRef.keys.accessKeyID` |
| source secret access key | `AWS_SECRET_ACCESS_KEY` | `spec.cloneFrom.secretRef.keys.secretAccessKey` |

Those defaults are the same key names this operator writes into the workload
Secret of a `Bucket` it manages, so **a Secret written by this operator for
another `Bucket` works as a clone source unchanged** — no `keys:` block needed.
That is not only a naming coincidence: the workload policy allows
`s3:ListBucket` and `s3:GetObject` on the bucket's own contents
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)), which is
exactly what measuring and reading a source requires. This path was exercised in
the 2026-09-01 cloud end-to-end run, where the source was a second `Bucket` in
the same namespace.

The credentials only need read access to the source. The destination side of the
copy authenticates as the operator's own admin S3 identity
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)) — you never
supply a destination credential.

---

## Addressing style

| `addressingStyle` | The source is addressed as | Use it for |
|---|---|---|
| `path` (# default) | `endpoint.host/bucket` | STACKIT, MinIO, Ceph, most S3-compatible services |
| `virtual-hosted` | `bucket.endpoint.host` | AWS S3 and endpoints that require the AWS style |

The destination side is always path-style, because that is what the provider
serves ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D11).

Getting this wrong fails at the very first step — the operator's own size
measurement of the source, before any Job is created. What you see is a `Failed`
phase with a `measure clone source "<bucket>" at <host>: …` message carrying the
provider's own error; the exact provider error text for a style mismatch is
**not verified, and this is the gap**: no run has copied from an endpoint that
requires virtual-hosted addressing against a real service, so the setting is
verified only against the offline suite
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md), Residual risks).

---

## Secret gating and readiness

`holdSecretUntilCloned` defaults to `true`: the workload credentials Secret named
by `spec.secretRef` is written only after the copy succeeded. A workload that
receives working credentials for a half-filled bucket does not see an error — it
sees missing data and acts on it, so the credential is what waits
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D7).

| Setting | The workload Secret | `Ready` |
|---|---|---|
| `holdSecretUntilCloned: true` (# default) | written after the copy completes | waits for the copy |
| `holdSecretUntilCloned: false` | written before the copy starts | waits for the copy |

Set it to `false` only for a workload that can tolerate a partially filled
bucket. It does not make the bucket `Ready` any earlier — `Ready` waits for the
clone in both settings. The `Ready` column in that table is the first `Ready` a
`Bucket` ever reaches; a `Bucket` that was already `Ready` when `cloneFrom` was
added keeps the condition it had, which is its own case below.

**Granted readers stay out of the bucket policy for the whole copy, in both
settings.** Withholding the Secret protects only this bucket's own workload; a
reader named in `spec.grantReadAccess` already holds credentials of its own, so
the policy is the only thing that can hold it back
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D10,
[ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D8). The
isolation policy itself is written *before* the copy starts, so the bucket is
never open to the rest of the project while it fills. In the very pass in which
the copy completes, the policy is rewritten with the readers — they do not wait
for another reconcile. See [read-grants.md](read-grants.md).

---

## What happens, in order

The order below is the default one, with `holdSecretUntilCloned: true`.

| # | Step | What you see |
|---|---|---|
| 1 | Bucket created and ownership-tagged, credentials group resolved | `Phase=Provisioning` |
| 2 | Isolation policy written **without** granted readers | `status.grantedReadTo` empty |
| 3 | Self-clone check | a fault here stops without retry (see below) |
| 4 | Source size measured once, with the source credentials | `status.clone.totalBytes` set |
| 5 | Staging Secret and the rclone Job created in the operator namespace | event `CloneStarted`, `status.clone.phase=Running` |
| 6 | Job polled every 15s; transfer stats read from rclone's control port | `status.clone.progress`, `rate`, `eta` refresh |
| 7 | Copy succeeds → terminal state persisted, then Job and staging Secret removed | `CloneCompleted=True`, event `Cloned` |
| 8 | Policy rewritten with granted readers, workload Secret written | `status.grantedReadTo` filled, Secret appears |
| 9 | Normal terminal status write | `Ready=True`, `Phase=Ready` |

With `holdSecretUntilCloned: false` only the Secret moves: it is written
immediately after the self-clone check in step 3, before the source is measured
in step 4, and step 8 then only rewrites the policy. Nothing else in the order
changes.

The size in step 4 is measured once, by the operator, so the percentage has a
denominator that only ever goes up
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D10). The
copy **merges and never deletes**: objects present at the destination and absent
at the source are left alone, which is what makes a retry safe
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D4).

---

## Watching progress

```bash
kubectl -n team-a get bkt my-bucket -o wide
# columns abbreviated to the clone-relevant ones; -o wide also prints
# REGION, SIZE, COST/MONTH, DEGRADED, RESOLVED, SECRET, OBJECTS, MEASURED, AGE
# NAME        BUCKET      PHASE          READY   STATUS                                              CLONE
# my-bucket   my-bucket   Provisioning   False   cloning from …/seed-data: 2.0 GiB / 18.0 GiB (11%)   2.0 GiB / 18.0 GiB (11%)
```

The `CLONE` column is a low-priority printer column, so it needs `-o wide`. The
full column set is in [bucket-status.md](bucket-status.md).

<details>
<summary><code>status.clone</code> in full</summary>

| Field | Meaning |
|---|---|
| `phase` | `Running`, `Completed` (terminal), `Failed` (retried) |
| `startedAt` | when the first copy Job was created |
| `completedAt` | when the copy finished successfully; stable across later reconciles |
| `totalBytes` | source size, measured once before the copy starts |
| `bytesCopied` | bytes transferred so far, from the copier's control API |
| `progress` | human-readable summary, e.g. `2.0 GiB / 18.0 GiB (11%)` |
| `rate` | current transfer rate, e.g. `42.0 MiB/s` (empty when unknown) |
| `eta` | estimated remaining time, e.g. `6m30s` (empty when unknown) |
| `message` | the failure message of the last failed attempt |

This page is the home of the `status.clone` field list. The `CloneCompleted`
condition carries the same outcome with reasons `Cloning`, `Cloned` and
`CloneFailed`, and `status.message` mirrors the progress line while the copy
runs; conditions, phases and events as a whole belong to
[bucket-status.md](bucket-status.md).
</details>

The percentage is clamped at 100. A source that keeps receiving writes during
the copy can therefore sit at `100%` while still transferring — **the completion
signal is the phase and the condition, never the percentage**.

Progress is written to the status every 15 seconds while the copy runs. Anything
watching `Bucket` objects has to filter those status writes out or it
re-triggers itself continuously; the operator's own watches do
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md),
[docs/developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md)).

The copy itself runs in the operator namespace, so a namespace user cannot read
its logs. From the operator namespace:

```bash
OPNS=stackit-s3-provisioner   # example: the operator's namespace
ANN='stackit-bucket\.gtrfc\.com'   # quoted: kubectl's JSONPath needs the backslashes,
                                   # an unquoted assignment would eat them and the columns come out empty
kubectl -n $OPNS get jobs -l app.kubernetes.io/component=clone \
  -o "custom-columns=JOB:.metadata.name,NS:.metadata.annotations.$ANN/bucket-namespace,BUCKET:.metadata.annotations.$ANN/bucket-name"
kubectl -n $OPNS logs job/<job-name>
```

The Job carries the owning `Bucket` in annotations rather than an owner
reference, because a cross-namespace owner reference is not allowed.

---

## When it fails

Every failure below is visible three ways: a `Warning` event with reason
`Failed` on the `Bucket`, `status.message`, and — for copy failures —
`status.clone.phase: Failed` with `CloneCompleted=False, reason=CloneFailed`.

| What went wrong | What you see | What happens, what to do |
|---|---|---|
| Source Secret does not exist | `get clone source secret team-a/seed-data-creds: …` | Retried under the workqueue's per-item backoff (1s, doubling, capped at 15m). The operator waits for the Secret; create it. |
| Configured data key carries no value | `clone source secret …/… misses data key "AWS_ACCESS_KEY_ID" or "AWS_SECRET_ACCESS_KEY"` | Same retry. Fix the Secret's keys or set `secretRef.keys`. |
| Source unreadable: wrong endpoint, wrong addressing style, credentials without list/read | `measure clone source "seed-data" at <host>: <provider error>` — no Job exists yet | Same retry. Fix `endpoint`, `addressingStyle`, `region` or the credentials. |
| Source is the destination | `cloneFrom points at the bucket itself (<host>/<bucket>); refusing to clone` | **No retry.** A configuration fault a retry cannot fix ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D6, [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)); it re-reconciles only when the spec changes. |
| The copy Job exhausted its backoff limit (3 retries after the first attempt, so up to 4 pod attempts) | `clone job failed: <job condition message>`, `status.clone.phase=Failed`, metric `stackit_s3_provisioner_buckets_clone{phase="Failed"}` | The Job is deleted and re-created on the next backoff; the copier resumes and skips what is already there. Read the Job logs before it is deleted, or check `status.clone.message`. |
| The copy is running but stats are unreachable | `progress`, `rate` and `eta` stop moving; phase stays `Running`; operator log `clone stats unavailable` (verbosity 1, shown only with `logging.level: debug`) | Nothing to do — stats are best effort (a pod that has not started yet looks exactly like this) and polling continues. The Job's own state, not the stats, decides the outcome. |
| The copy simply takes long | `Phase=Provisioning` for hours (`Ready=False` on a new `Bucket`; on a retrofit `Ready` keeps its stale `True`) | Expected for a large migration. `StackitS3BucketStuckProvisioning` fires after 30m in `Pending`/`Provisioning`; on a large clone that alert is noise, not a fault. |

A failure that keeps repeating is alerted on: `StackitS3CloneFailed` (warning)
fires when any clone stays in phase `Failed` for 30 minutes. The toggle is
`monitoring.prometheusRule.alerts.cloneFailed.enabled` (default `true`) — the
alert toggles are nested under `monitoring.prometheusRule`, not at the top
level. A misconfigured or permanently unreachable source is retried **forever**,
with no attempt ceiling — the `Bucket` stays non-`Ready` for as long as that
lasts. See [monitoring.md](monitoring.md).

The Job's retry budget is `backoffLimit: 3`, which Kubernetes counts as *three
failures tolerated*: the Job is marked failed on the fourth. That reading rests
on upstream Job-controller semantics — **not verified, and this is the gap**: no
run in this repo has been made to burn through the budget deliberately, so the
exact attempt count has not been observed on a live cluster.

A clone failure is a local, definitive statement about this `Bucket` only where
the self-clone check is concerned; the other clone failures are routed through
the retrying path, which means a `Bucket` that was **already** `Ready` can have
its `Ready` state held for the degradation grace instead of dropping at once
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md),
[provider-outages.md](provider-outages.md)). For the normal case — a clone on a
newly created `Bucket` that has never been `Ready` — there is nothing to hold and
the `Bucket` drops to `Failed` immediately.

---

## The one-shot rule

Once `status.clone.phase` is `Completed`, the copy never runs again for that
`Bucket`, and **a corrected `spec.cloneFrom` has no effect at all**: no event, no
condition change, no message. `spec.cloneFrom` is not immutable in the CRD, so
the API server accepts the edit and nothing happens — this is the sharpest edge
of the feature ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D5).

The only way to re-seed is to delete the `Bucket` and create it again, which
under the empty-only delete rule first requires emptying the bucket
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md),
[deletion.md](deletion.md)). Check the source before you apply, not after.

Verified from the code, and worth knowing while a copy is still running:
editing `spec.cloneFrom` mid-copy does not redirect the running Job either. The
operator creates a Job only when none exists and otherwise just observes it. The
edited source is picked up only if that Job later fails and is re-created — so a
mid-flight edit can change the source between attempts without any signal that
it did.

---

## Deleting a `Bucket` while a clone runs

Teardown stops the copy first: the Job and its staging Secret are removed before
anything else, so nothing keeps writing into a bucket that is being emptied and
deleted ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D12,
[ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)).

What follows is the ordinary delete path, and it is where a half-finished clone
bites: the objects already copied make the bucket non-empty, so the data-loss
guard blocks deletion until the bucket is emptied — by hand, or by
`spec.wipeOnDelete` if the operator-wide gate is enabled. The `Bucket` sits in
`Phase=Deleting`, and `StackitS3BucketStuckDeleting` fires after 30 minutes. See
[deletion.md](deletion.md).

---

## Adding `cloneFrom` to a `Bucket` that is already provisioned

Nothing forbids it, and the consequences are worth spelling out, all verified
from the reconcile path:

- The copy runs **into the live bucket**, merging with what is there.
- The **phase** leaves `Ready` — it reads `Provisioning` for the whole copy — and
  `status.observedGeneration` lags behind `metadata.generation`. The **`Ready`
  condition** does not: nothing on the clone path writes it, so the `True` it
  already carried stays until the clone finishes or a failure takes the
  `Bucket` down the failure path. On a retrofit the `READY` column therefore
  reads `True` while the copy runs, and only `CloneCompleted` tells the two
  states apart — so watch `CloneCompleted`, not `Ready`, here.
- Granted readers are removed from the policy for the duration of the copy and
  restored when it completes.
- With the default `holdSecretUntilCloned: true` the existing workload Secret is
  **not** deleted — the hold only defers writing it — so a running workload keeps
  its credential while the bucket fills.

---

## Configuration notes for a real deployment

The copier is a third-party image running in the operator's namespace with the
operator's admin S3 credential in its environment. Three Helm values govern it;
the full value list is in [README.md](../../README.md) and the annotated defaults
are in [values.yaml](../../deploy/helm/stackit-s3-provisioner/values.yaml).

```yaml
clone:
  image:
    repository: rclone/rclone   # default
    tag: "1.75.0"               # default; pinned in the chart, tracked by dependency updates
  # resources: the chart already ships requests and limits. Copying is
  #   network-bound, so raise the limits for a very large transfer; the numbers
  #   are in README.md and values.yaml. Never set an empty map here — that
  #   drops the shipped requests and limits.
  networkPolicy:
    enabled: true         # default; keep it on wherever the CNI enforces NetworkPolicies
```

`clone.image` also reaches the operator as the `--clone-image` flag
(`CLONE_IMAGE`), and `clone.resources` as the `CLONE_JOB_RESOURCES` environment
variable; the chart renders both. See [configuration.md](configuration.md).

**The control port.** The copier's remote-control interface on port 5572 is what
makes the transfer observable — and it can execute commands, not only report. It
is protected by two things: a 32-character password generated per clone into the
staging Secret, and a NetworkPolicy that admits ingress to port 5572 of clone
pods from the operator pod only. Clone pods are selected by the labels
`app.kubernetes.io/managed-by: stackit-s3-provisioner` and
`app.kubernetes.io/component: clone`, so the same selector works for a policy you
write yourself.

`clone.networkPolicy.enabled: false` exists for clusters whose CNI does not
enforce NetworkPolicies, where the resource would be silently inert anyway.
Turning it off — or running on such a cluster with it on — leaves the port
reachable from anywhere in the cluster, with only the generated password in front
of an interface that can execute commands
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D9 and
Residual risks; [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md)).

The clone pod itself runs non-root (uid 65534), with a read-only root
filesystem, no privilege escalation and all capabilities dropped. The Job carries
a 1-hour time-to-live after finishing, as a backstop for a finished Job the
operator failed to delete itself.

**Privileges this feature adds.** Supporting clones is why the operator's
ClusterRole carries Job create/delete and Pod read (the Pod IP is what the stats
poll needs), even though it only ever uses them in its own namespace. See
[docs/security/rbac-and-privilege.md](../security/rbac-and-privilege.md).

---

## The suite that proves this still works

| Suite | Command | What it proves |
|---|---|---|
| Offline reconciler tests | `go test ./internal/controller/ -run Clone` | The hold invariant, early publication with the hold off, progress from stats, Job-failure retry, the guards, teardown cleanup, and that the Job name stays within the label budget |
| Cloud end-to-end | `make e2e-stackit` (`TestCloudClone`) | A real copy with the real `rclone/rclone:1.75.0` image in Kind against the real STACKIT API: a second `Bucket`'s workload Secret used verbatim as the source credential, `totalBytes` equal to the seeded size, three objects including nested prefixes arriving byte-identical, `CloneCompleted=True`, the hold checked on every poll, and `completedAt` stable across later reconciles (clone-once). Last full green run: 2026-09-01. |

The end-to-end target reads the copier image out of the rendered chart and
preloads it into the Kind node, so the test never pulls from a registry mid-run
and there is no third place where the image version is pinned.

---

## Related

| Document | What it adds |
|---|---|
| [ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) | The decision, what was rejected, and the costs accepted |
| [docs/developer/clone.md](../developer/clone.md) | The Job and staging-Secret naming, polling and retry mechanics, crash-safety ordering |
| [README.md](../../README.md) | Every `spec.cloneFrom` field and every `clone.*` Helm value |
| [deletion.md](deletion.md) | The delete path a re-seed has to go through |
| [read-grants.md](read-grants.md) | The readers held out of the policy while a copy runs |
| [credentials.md](credentials.md) | The workload credential whose publication the clone withholds |
| [bucket-status.md](bucket-status.md) | Phases, conditions and events in full |
| [monitoring.md](monitoring.md) | `StackitS3CloneFailed` and the other shipped alerts |
