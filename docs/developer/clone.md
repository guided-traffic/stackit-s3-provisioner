# Clone

This page is the machinery behind `spec.cloneFrom`: the Job the operator creates, the staging
Secret that feeds it, how progress is read out of a running copy, the ordering that makes a crash
mid-clone safe, and the traps that are not visible from the code. It is written for somebody about
to change [internal/controller/clone.go](../../internal/controller/clone.go). If you want to
*declare* a clone and read its progress, that is
[docs/operations/cloning.md](../operations/cloning.md); the decisions — one-shot, operator
namespace, admin credential on the destination side — are
[ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) and are not
restated here.

## What the operator creates

Two cluster objects, both in the operator namespace (`r.AdminSecretNamespace`, from
`POD_NAMESPACE`), neither of them owned by the `Bucket` CR — a cross-namespace owner reference is
not permitted, so nothing is garbage-collected for us:

| Object | Name | Built by | Removed by |
|---|---|---|---|
| Staging `Secret` | `cloneJobName(b) + "-src"` | [`ensureCloneStagingSecret`](../../internal/controller/clone.go) | `completeClone`, or teardown |
| `Job` | `cloneJobName(b)` | [`buildCloneJob`](../../internal/controller/clone.go) | `completeClone`, a failed-attempt retry, teardown, or the Job TTL |

`cloneJobName` is `s3op-clone-<namespace>-<name>`, truncated and suffixed with an 8-hex FNV-1a-32
hash of `namespace + "/" + name` — the same stable-identity scheme as the workload group name. The
budget is 52 characters, not 63: the Job controller stamps the Job name into every pod's
`batch.kubernetes.io/job-name` label, which must be a valid label value, and the staging Secret
appends `-src` to the same base. `TestCloneJobNameStaysWithinLabelBudget` in
[reconciler_clone_test.go](../../internal/controller/reconciler_clone_test.go) holds that line; if
you widen the prefix, widen the test.

Both objects carry `app.kubernetes.io/managed-by: stackit-s3-provisioner` and
`app.kubernetes.io/component: clone`. The second label is not decoration — it is the selector in the
chart's NetworkPolicy and in `isCloneJob`, which scopes the Job watch. The `managed-by` value here
is the hard-coded constant `managedByValue`, **not** the configurable ownership name that goes into
bucket tags; the two look alike and are not the same knob.

The staging Secret holds three data keys:

| Key | Content |
|---|---|
| `sourceAccessKeyID` | source S3 access key id, copied out of the user's Secret |
| `sourceSecretAccessKey` | source S3 secret |
| `rcPassword` | 32 hex characters from `crypto/rand`, generated once per clone |

It is written with `controllerutil.CreateOrUpdate` and the password branch is guarded by
`len(...) == 0`, so the password is generated once and then stays put. That matters: a retried
attempt reuses the same staging Secret, and the operator's poller and the pod have to agree on the
password across restarts.

The destination credential is **not** staged. The Job's pod reads it straight from the operator's
admin Secret through a `secretKeyRef` on the data keys `accessKeyID` and `secretAccessKey`. The Job
already runs in the namespace where that Secret lives, so a second copy in the same namespace would
buy nothing and add an object to keep in step.

## The two sides of the copy

Everything rclone needs is passed as environment, `RCLONE_CONFIG_<REMOTE>_*`, with two remotes named
`SRC` and `DST`; the container args are
`copy src:<sourceBucket> dst:<destinationBucket> --rc --rc-addr=:5572 --stats=10s --s3-no-check-bucket`.

| Setting | Source (`SRC`) | Destination (`DST`) |
|---|---|---|
| Endpoint | `spec.cloneFrom.endpoint`, `https://` prepended when it is a bare host (`CloneFrom.EndpointURL`) | derived from the bucket's path-style URL by stripping the trailing `/<bucket>` (`endpointURLFromBucketURL`) |
| Credentials | staging Secret | operator admin Secret |
| Region | `spec.cloneFrom.region`, only set when non-empty | the operator's own region, always set |
| `FORCE_PATH_STYLE` | `true` unless `addressingStyle: virtual-hosted` | `true`, unconditionally |

`copy` merges: it never deletes at the destination, which is what makes a retry safe and what makes
this unusable as a sync ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md)
D4). `--s3-no-check-bucket` stops rclone from probing or trying to create the destination bucket,
which the operator has already created. `--stats=10s` only paces rclone's own log lines; the number
the operator reads comes from the remote-control API, not from the log.

The addressing style applies to the **source only**, in two places that must stay in step:
`newCloneSourceClient` picks `stackit.NewS3VirtualHosted` over `stackit.NewS3Admin` for the
measurement client, and the Job gets `RCLONE_CONFIG_SRC_FORCE_PATH_STYLE=false`. Change one and the
measurement and the copy will disagree about how to reach the same bucket. The offline subtest
`TestCloneGuards/virtual-hosted addressing style` asserts both environment values; it pre-seeds
`status.clone.totalBytes` because the in-memory fake serves on an IP address, which cannot answer a
virtual-hosted request at all. That is also why no test anywhere exercises virtual-hosted addressing
end to end — see *What is wrong today*.

The source credentials come from `spec.cloneFrom.secretRef` in the `Bucket`'s own namespace, with
data keys defaulting to `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` — the same defaults the
operator writes into a workload Secret, which is why a Secret this operator produced for another
`Bucket` works as a clone source with no key configuration
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D3). The reference
has no namespace field, and adding one would be the confused-deputy primitive
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D3/D4 exists to forbid.

## The progress denominator is measured once, by the operator

Before the Job is created, `ensureClone` builds its own S3 client from the source credentials and
calls `S3Admin.BucketUsage` on the source bucket — `BucketStats(ctx, bucket, false, 0)` in
[stackit/s3.go](../../stackit/s3.go), an uncapped recursive listing of current objects, the same
listing machinery the size-measurement controller uses
([usage-measurement.md](usage-measurement.md)). The result is `status.clone.totalBytes`.

The measurement is guarded by `if b.Status.Clone.TotalBytes == 0`, so it happens once per `Bucket`
and survives operator restarts through the status. It exists because rclone's own `totalBytes` grows
while it is still scanning the source, which would make the percentage fall backwards
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D10). `cloneProgress`
clamps the percentage at 100 and degrades to the copied amount alone when the total is not positive.

Note that the zero test is on the byte total, not on a "measured" flag — a source that contains no
bytes is re-listed on every poll. See *What is wrong today*.

## Progress polling

`pollCloneStats` lists pods in the operator namespace by `batch.kubernetes.io/job-name`, takes the
first one in phase `Running` with a pod IP, and `POST`s an empty JSON body to
`http://<podIP>:5572/core/stats` with HTTP basic auth (user `operator`, password from the staging
Secret). The decoded fields are `bytes`, `speed` and `eta`; everything else rclone returns is
ignored. The HTTP client is built once per reconciler with a 5-second timeout.

Stats are best effort. An unreachable rc endpoint — pod still starting, pod already gone — is logged
at V(1) and the previous numbers are kept; the poll requeues either way. The cadence is
`clonePollInterval`, a fixed 15 seconds returned as `RequeueAfter`, and there is no flag for it.

`updateCloneProgress` writes `status.clone` plus the coarse `status.phase = Provisioning` and a
`status.message` naming the source and the progress string. Like `markProvisioning` it swallows a
failed write: the next poll rewrites it anyway.

`r.cloneStatsFn` is the test seam that replaces the HTTP call; it is nil in production.

## Why the control port is locked down

rclone's remote-control API is not a read-only stats endpoint — it can execute commands. Exposing it
so the operator can render a percentage opens a control channel inside the cluster, and two things
close it:

* the generated 32-character basic-auth password in the staging Secret, and
* the chart's NetworkPolicy
  [`templates/networkpolicy.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/networkpolicy.yaml),
  which selects pods by the two clone labels, declares `policyTypes: [Ingress]` — thereby denying
  all other ingress to those pods — and admits TCP 5572 only from pods matching the operator's own
  selector labels.

It is on by default (`clone.networkPolicy.enabled: true` # default) and exists as a value so a
cluster whose CNI does not enforce NetworkPolicies can switch it off knowingly rather than believe
in a protection it does not have. On such a cluster the port is reachable cluster-wide and the
password is the only thing in front of it; the auth travels as plain HTTP over the pod network,
because the rc endpoint speaks no TLS here. The port number is duplicated between `cloneRcPort` in
[clone.go](../../internal/controller/clone.go) and the literal `5572` in the NetworkPolicy template
— changing one without the other silently removes the protection rather than breaking anything
loudly. The threat side of this lives in
[docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md).

## Where the clone sits in a provisioning pass

`ensureClone` is called from `provisionCredentialsAndClone` in
[bucket_controller.go](../../internal/controller/bucket_controller.go), and the ordering there is
load-bearing in three ways:

1. **The isolation policy is written before the copy starts**, so the bucket is never open to the
   rest of the project while it fills up ([bucket-policy.md](bucket-policy.md)).
2. **Granted readers are held out of that policy for as long as the clone is pending.** The policy
   closure is called with a nil reader set while `b.ClonePending()` is true, and called a second
   time with the resolved readers in the very pass where the copy completes — not on the next
   reconcile. `holdSecretUntilCloned` cannot do this job: it withholds the bucket's *own* workload
   credential, while a granted reader already holds working credentials of its own
   ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D8).
3. **The self-clone check runs after the policy and before the copy.** `validateCloneSource`
   compares the source bucket name and endpoint host against the resolved destination and is the
   only definitive fault on the clone path — it goes through `failNoRequeue`
   ([provider-errors.md](provider-errors.md)).

With `holdSecretUntilCloned: false` the workload key and Secret are created before `ensureClone`,
and the pending rotation trigger is recorded immediately rather than at the terminal status write,
which a pass with a running clone never reaches — otherwise a requested rotation would re-fire on
every 15-second poll. The mechanics of that are [credentials.md](credentials.md).

A pass that ends inside `ensureClone` returns `done=false`, so `reconcileNormal` stops there:
`Ready` is not set, `observedGeneration` is not advanced, and `status.resolvedBucketName`,
`status.bucketURL` and `status.accessKeyID` stay unwritten. What *is* written early is
`status.credentialsGroupID` / `status.credentialsGroupURN`, deliberately: a grantor watching a
still-cloning grantee would otherwise never be woken.

## Crash safety

`completeClone` sets `phase = Completed`, fills `completedAt`, pins `bytesCopied` to `totalBytes`,
sets `CloneCompleted=True`, and **persists that status before** `deleteCloneArtifacts` touches the
Job or the staging Secret. If the status write fails, the function returns through `fail` and
removes nothing.

The order is the whole of the crash safety
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D5). A crash
between the write and the cleanup leaves a completed Job and a staging Secret behind, and the next
reconcile sees `CloneCompleted()` true and never enters `ensureClone` at all; the leftovers are
collected by the Job's `TTLSecondsAfterFinished` (`cloneJobTTLSeconds`, a fixed constant of 3600
seconds with no override path) and by teardown. A crash *before* the write re-observes a finished
Job on the next reconcile and completes normally. What must never happen is cleanup first: a crash
in that window would delete the evidence that the copy had run and start a second one.

If you add work to `completeClone`, it goes after the status write, not before it.

## Retry

`cloneJobFinished` reads the Job's `Complete` / `Failed` conditions. On failure the operator deletes
the Job — with `DeletePropagationBackground`, so the pods go too — but **keeps** the staging Secret
(`deleteCloneArtifacts(ctx, b, false)`), sets `phase = Failed` with the Job's condition message,
sets `CloneCompleted=False` with reason `CloneFailed`, and returns through `fail`. The next
reconcile creates a fresh Job under the workqueue's exponential backoff, and rclone resumes: objects
already at the destination are skipped.

Inside one Job, `cloneJobBackoffLimit` (3) is the Kubernetes retry budget, not an attempt count:
`.spec.backoffLimit` counts retries, and with `restartPolicy: Never` the Job controller keeps
creating replacement pods and marks the Job `Failed` once the failure count *exceeds* the limit — so
up to four pod attempts are made. (The constant's own comment in
[clone.go](../../internal/controller/clone.go) says "pod attempts" and is imprecise in exactly this
way; it is the reason the number is easy to misread.) Across Jobs there is no ceiling — a
permanently misconfigured source retries until the spec changes or the `Bucket` is deleted.

Three consequences worth knowing before you touch the failure path:

* Every *retryable* clone failure routes through `fail`, which feeds `r.Breaker.Failure()`. (The one
  exception is the self-clone refusal, which takes `failNoRequeue` and never touches the breaker.) A
  clone that keeps failing is indistinguishable, to the breaker, from the provider being down — that
  is the design ([circuit-breaker.md](circuit-breaker.md)), and in a cluster whose only `Bucket` is
  the failing one it will open the fleet-wide circuit after three consecutive passes. With other
  healthy `Bucket`s in the fleet their successes reset the counter.
* **A clone failure always drops the `Bucket` to `Failed`; the degraded path cannot absorb it.**
  `degrade` only holds `Ready` for a `Bucket` whose `observedGeneration` has converged *and* whose
  phase and `Ready` condition are already `Ready` (`holdsReadyThrough` in
  [bucket_controller.go](../../internal/controller/bucket_controller.go)). A clone-pending `Bucket`
  can never be in that state: `ensureClone` is entered only while `b.ClonePending()` is true, which
  requires `status.clone.phase != Completed`, and a cloning `Bucket` only reaches `Ready` through
  `completeClone`, which sets `phase = Completed`. Adding `spec.cloneFrom` to a `Bucket` that is
  already `Ready` bumps the generation and so fails the `observedGeneration` test. The only way into
  the degraded path here is `status.clone` being cleared externally on a converged `Ready` `Bucket` —
  a theoretical state nothing in the operator produces. The general classification is
  [provider-errors.md](provider-errors.md).
* Both source-credential failure modes (Secret absent; configured data key empty) take the retrying
  path, so the operator waits for a Secret to appear rather than giving up.

## Job events, the watch filter, and teardown

A Job cannot own, or be owned by, an object in another namespace, so the link back to the CR is two
annotations stamped on the Job and its pod template:
`stackit-bucket.gtrfc.com/bucket-namespace` and `stackit-bucket.gtrfc.com/bucket-name`. Annotations
rather than labels, because a CR name may exceed the 63-character label limit. `bucketsForCloneJob`
reads them back; `isCloneJob` scopes the watch to the two clone labels. The effect is that a Job
finishing re-queues its `Bucket` at once instead of after up to 15 more seconds.

The main `Bucket` watch is filtered to generation and annotation changes, and this feature is the
reason: `status.clone` is rewritten every 15 seconds while a copy runs, and an unfiltered watch
would turn each of those writes into an immediate reconcile. The rule, the second `Bucket` watch
that had to be given its own narrow predicate for the same reason, and the explicit requeue after
adding the finalizer are all in [reconcile-pipeline.md](reconcile-pipeline.md).

`teardown` calls `deleteCloneArtifacts(ctx, b, true)` **first**, before the empty check and before
anything else — nothing may keep writing into a bucket that is about to be emptied and deleted
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D12). Absence is
tolerated, so it is safe on a `Bucket` that never cloned.

## The surface this feature adds

The `status.clone` fields, the `CloneCompleted` condition and the three `clone.*` Helm values with
their defaults are the user-facing reference and live once, in
[docs/operations/cloning.md](../operations/cloning.md). What follows
is the part of the surface that is only visible from inside the operator.

<details>
<summary>RBAC markers, flag and environment plumbing, event and condition reasons</summary>

RBAC, from the markers in [bucket_controller.go](../../internal/controller/bucket_controller.go),
rendered into the chart's ClusterRole:

| Rule | Why |
|---|---|
| `batch/jobs`: `get;list;watch;create;delete` | the clone Job |
| `pods`: `get;list;watch` | the pod IP the rc endpoint is polled on |

Both are cluster-scoped grants that the operator only ever exercises in its own namespace — an
overreach [docs/security/rbac-and-privilege.md](../security/rbac-and-privilege.md) names.

How the chart's three clone values reach the process:

| Helm value | Reaches the operator as |
|---|---|
| `clone.image.repository` + `clone.image.tag` | the `--clone-image` flag, or the `CLONE_IMAGE` environment variable |
| `clone.resources` | the `CLONE_JOB_RESOURCES` environment variable, a JSON-encoded `ResourceRequirements` |
| `clone.networkPolicy.enabled` | no flag at all — it renders a resource, and the operator never learns whether it exists |

`DefaultCloneImage` in [clone.go](../../internal/controller/clone.go) only applies when no
`--clone-image` is given. The chart always passes one, and the cloud end-to-end target reads the
image back out of the rendered chart, so the version is pinned in two places and not three
([testing.md](testing.md)).

Reasons: the condition `CloneCompleted` uses `Cloning`, `Cloned` and `CloneFailed`, all declared in
[api/v1/bucket_types.go](../../api/v1/bucket_types.go). This file raises two events —
`reasonCloneStarted` (`CloneStarted`), the only reason string declared in
[clone.go](../../internal/controller/clone.go) itself, and `Cloned` on completion, reusing the
condition reason from `api/v1`.

Formatting helpers live at the bottom of [clone.go](../../internal/controller/clone.go):
`humanBytes` (binary units), `cloneProgress`, `cloneRate`, `cloneETA`. They are covered by
`TestCloneFormatting`.

</details>

## What is wrong today

**A source of zero total bytes is re-listed on every poll.** The measurement guard is
`status.clone.totalBytes == 0`, not a separate "measured" marker, so a source bucket that is empty —
or that holds only zero-byte objects — is fully listed again on every 15-second pass for as long as
the clone is pending. It is bounded (the copy of an empty source finishes fast) and it is wasted
work against a foreign endpoint that the operator has no rate budget for.

**A clone poll is a full provisioning pass.** `ensureClone` sits near the end of `reconcileNormal`,
so every 15-second poll first re-runs everything before it: a project-wide bucket listing
(`HasBucket`), a bucket-tag read and possibly an emptiness probe (`adoptOrCollide`), a `GetBucket`
(`BucketConnInfo`), a credentials-group listing, and a policy read. A multi-hour clone therefore
costs several provider calls every 15 seconds for its whole duration. Nothing in the code special-
cases a pass whose only purpose is to read a percentage.

**The Job and the staging Secret have no owner reference.** They are removed by `completeClone` and
by `teardown`, and a finished Job additionally by its TTL — but a `Bucket` whose finalizer is
force-removed while a clone runs leaves a running Job (TTL only applies once a Job has finished) and
a Secret holding the source credentials behind, in the operator namespace, with nothing left to
clean them up. `cloneJobName` is deterministic, so they are findable by name.

**The rc port literal is duplicated** between `cloneRcPort` and the NetworkPolicy template, as noted
above.

**Not verified: virtual-hosted addressing against a real virtual-hosted endpoint.** The offline
suite asserts the two environment values the setting produces and nothing further; the cloud
end-to-end run of 2026-09-01 used a path-style source. No run has copied from AWS or from any
endpoint that requires virtual-hosted addressing.

**Not verified: an operator restart mid-transfer.** The Job is a separate object and the terminal
state is persisted before cleanup, which is the whole argument that a restart is survivable — but no
test and no live run has restarted the operator while a copy was in flight.
