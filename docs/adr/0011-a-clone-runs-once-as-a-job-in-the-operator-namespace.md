# ADR 0011: A Clone Runs Once, as a Job in the Operator Namespace

## Status

Accepted. Date: 2026-07-17. Recorded as an ADR on 2026-09-19, with the rules unchanged.

Implemented and live-verified: a `Bucket` that declares `spec.cloneFrom` is seeded once from a
foreign S3 endpoint by a copy job in the operator namespace, the workload Secret is withheld until
the copy completes, and the completed state is terminal. Open: there is no supported way to run a
second clone into the same `Bucket`, and a `spec.cloneFrom` corrected after completion is a silent
no-op — both are accepted costs recorded below, not planned work. The `virtual-hosted` addressing
style has never been exercised against a real virtual-hosted endpoint (see Residual risks).

## Context

A bucket this operator provisions starts empty. The common reason to declare one, though, is to
move an existing workload — out of another project, off AWS, off a self-run MinIO — and a workload
without its data is not a migrated workload. Doing the copy by hand means a machine with network
reach to both endpoints, both sets of credentials in one shell history, and a step that exists
nowhere in the Git state that is supposed to describe the cluster. The declaration and the data
arrive by different routes, and nothing reconciles the two.

Making the operator do it was not free, for three reasons that pull in different directions.

**A copy outlives a reconcile.** A reconcile is a short function call in a process that is expected
to restart — on an upgrade, on eviction, on a node drain. A multi-terabyte transfer is neither
short nor restartable from memory. Anything that copies inside the reconcile loop ties the transfer
to the lifetime of a pod that has no reason to live that long, and loses every byte of progress
when it does not. The copy therefore has to be an object in the cluster with a lifecycle of its
own, observed by the reconciler rather than performed by it.

**The two credentials must not meet in the same namespace.** The destination side of the copy has
to write into a bucket whose isolation policy denies everyone except the operator's own admin
identity and the bucket's workload group ([ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md),
[ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md)) — and under the default
withholding of the workload Secret, the workload credential does not exist yet when the copy
starts. So the destination side authenticates as the project-wide admin identity, which is the one
credential in this system whose blast radius is every bucket the operator manages. The source side,
by contrast, authenticates with credentials that belong to whoever wrote the `Bucket`, which the
operator can only read out of a Secret. Put the copy in the workload namespace and the project-wide
admin credential is mounted where a namespace user can read it; let the source reference name a
foreign namespace and the operator becomes a confused deputy that reads any Secret in the cluster
on a CR author's behalf ([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) D3/D4).

**A half-filled bucket looks finished.** A workload that receives working credentials for a bucket
that is 11% copied does not see an error. It sees an empty or partial bucket and behaves as if that
were the truth — it re-uploads, it reports missing objects, in the worst case it writes a
"migration complete" marker. The credential is the only thing that gates this, so the credential is
what has to wait.

The copier itself is rclone, run from its published image. Its remote-control interface is what
makes a transfer observable from outside the pod, and that interface can also execute commands —
so exposing it for progress reporting opens a control channel inside the cluster, which has to be
closed by something other than obscurity.

**Live verification, 2026-09-01** (full cloud end-to-end run against the real STACKIT API, with the
real `rclone/rclone:1.75.0` image loaded into a Kind node): a second `Bucket` in the same namespace
served as the source, and its workload Secret was used verbatim as `spec.cloneFrom.secretRef` — the
default data-key names match, and the workload policy already allows `s3:ListBucket` and
`s3:GetObject` on the bucket's own contents, which is exactly what reading a source needs.
Confirmed in that run: `status.clone.totalBytes` equalled the seeded size; three objects, including
nested prefixes, arrived byte-identical; `CloneCompleted` became `True`; `status.clone.completedAt`
stayed stable across subsequent reconciles, so the copy really ran once. The run also asserted the
withholding invariant on every poll — the destination's workload Secret never existed while the
clone phase was anything other than `Completed`.

## Decision

**D1 — A clone is a one-shot seeding of a bucket, not a synchronisation.** `spec.cloneFrom` names a
source endpoint and bucket whose contents are copied into the provisioned bucket once. The copy
runs after the bucket exists and after its isolation policy has been written, never before.

**D2 — The copy runs as a Kubernetes Job in the operator namespace.** It is a separate object with
its own lifecycle, so it survives operator restarts, is bounded by pod resource limits, and never
executes in a workload namespace. The reconciler observes the Job; it does not perform the copy.

**D3 — The destination side authenticates as the operator admin identity; the source side only with
credentials from the Bucket's own namespace.** `spec.cloneFrom.secretRef` names a Secret in the
`Bucket`'s namespace and carries no `namespace` field, by the same rule that governs every other
reference in a Bucket spec ([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) D3/D4). The
data-key names default to `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` and are overridable per
`Bucket` via `spec.cloneFrom.secretRef.keys.accessKeyID` and `.secretAccessKey`, so a Secret this
operator wrote for another `Bucket` works as a clone source unchanged. The source credentials are
copied into a staging Secret in the operator namespace, because a Job cannot mount a Secret across
namespaces; that staging Secret is removed when the clone reaches its terminal state.

**D4 — The copy merges and never deletes.** Objects present at the destination and absent at the
source are left alone. A retried attempt resumes and skips what is already there, which is what
makes retrying safe.

**D5 — A completed clone is terminal.** Once `status.clone.phase` is `Completed` the clone never
runs again for that `Bucket`, and later changes to `spec.cloneFrom` have no effect. The terminal
state is written to the status before the Job and the staging Secret are removed, so a crash during
cleanup can never cause a second copy.

**D6 — Cloning a bucket onto itself is a configuration fault.** A source whose endpoint host and
bucket name equal the destination's is refused without a retry, like every other fault a retry
cannot fix ([ADR 0012](0012-ready-describes-the-last-verified-state.md)).

**D7 — By default the workload credential waits for the copy; readiness always does.**
`spec.cloneFrom.holdSecretUntilCloned` defaults to `true`: the workload credentials Secret is
written only after the copy succeeded. Setting it to `false` publishes the credential immediately,
and `Ready` still waits for the clone either way.

**D8 — Granted readers stay out of the policy until the copy completes, in both settings.**
Withholding the Secret protects only the bucket's own workload; a read grant
([ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md)) is held by a
workload that already has credentials of its own, so the policy is the only thing that can hold it
back. The isolation policy itself is still written first, so the bucket is never open to the rest
of the project while it fills. When the copy completes, the policy is rewritten with the readers in
the same pass.

**D9 — Progress is observable from the status, and the channel that provides it is closed to
everyone but the operator.** The copier's remote-control interface is protected by a generated
32-character password held in the staging Secret, and the chart ships a NetworkPolicy
(`clone.networkPolicy.enabled`, default `true`) that admits ingress to port 5572 of clone pods only
from the operator pod. Clone pods carry the labels `app.kubernetes.io/managed-by:
stackit-s3-provisioner` and `app.kubernetes.io/component: clone` so that policy — and any an
operator writes themselves — can select them.

**D10 — The percentage denominator is measured once, before the copy starts.** The operator lists
the source itself and records `status.clone.totalBytes`, rather than adopting the copier's own
running total, which grows while it is still scanning and would make the percentage move backwards.

**D11 — The source is addressed path-style by default; the destination always is.**
`spec.cloneFrom.addressingStyle` accepts `path` (# default) and `virtual-hosted`; the destination
side of the copy is path-style unconditionally, because that is what the provider serves.

**D12 — Deleting a `Bucket` stops a running clone first.** The Job and its staging Secret are
removed before anything else in teardown, so nothing keeps writing into a bucket that is being
emptied and deleted ([ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md)).

### The vocabulary this decision fixes

| Name | What it is |
|---|---|
| `spec.cloneFrom` | The declaration: `endpoint`, `bucket`, `region`, `addressingStyle`, `secretRef`, `holdSecretUntilCloned` |
| `status.clone` | The observation: `phase`, `bytesCopied`, `totalBytes`, `progress`, `rate`, `eta`, `startedAt`, `completedAt`, `message` |
| `status.clone.phase` | `Running`, `Completed` (terminal), `Failed` (retried) |
| `CloneCompleted` | The condition carrying the outcome, with reasons `Cloning`, `Cloned`, `CloneFailed` |
| `CloneStarted` | The event raised when the copy job is created |
| `clone.image`, `clone.resources`, `clone.networkPolicy.enabled` | The Helm values that configure the copier, its pod resources and the ingress restriction |

## Consequences

**A third-party image runs in the operator namespace with the project-wide admin credential in its
environment.** This is the largest cost in this record. The copier is not code this project wrote or
reviewed; its image tag is pinned in the chart and updated through dependency review, and its pod
runs non-root with a read-only root filesystem, no privilege escalation and all capabilities
dropped — but none of that changes the fact that a compromised release of that image is a
compromise of the credential that is exempt in *every* bucket policy in the project. The decision
accepts this in exchange for not writing and maintaining a data mover.

**The operator's privileges grew beyond its own namespace.** Creating and deleting Jobs and reading
Pods are cluster-scoped grants in the shipped ClusterRole, even though the operator only ever uses
them in its own namespace. A reader auditing the operator's privilege footprint sees more than it
exercises.

**The copy is invisible to the team that asked for it.** The Job runs in the operator namespace, so
a namespace user cannot read the copier's logs for their own migration. They see `status.clone` and
the `Clone` printer column and nothing else; a diagnosis beyond "it failed, here is the message"
needs someone with access to the operator namespace.

**A wrong clone cannot be corrected in place.** Terminality (D5) means that fixing a typo in
`spec.cloneFrom` after the copy completed changes nothing, silently: no event, no condition change,
no message. The only way to re-seed is to delete the `Bucket` and create it again — which, under
the empty-only delete rule, first requires emptying the bucket
([ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md)).

**A long clone is a long non-`Ready` period.** The bucket, its policy and its credentials group all
exist while the copy runs; `Ready` does not. Anything that treats `Ready` as "the bucket exists"
will wait hours on a large migration. The status is written on every poll of a running clone
(a fixed 15-second cadence), so anything watching `Bucket` objects has to filter those writes out
or it will re-trigger itself continuously.

**The `Bucket` spec is still never written by the operator.** Everything about a clone is expressed
in the status and in cluster objects the operator owns, so a re-apply of the Git state remains a
no-op ([ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md)).

## Alternatives Considered

**Copy inside the operator process.** Rejected, and it was by far the cheaper option: no Job, no
staging Secret, no cluster-scoped Job and Pod permissions, no pinned third-party image, no
NetworkPolicy, no remote-control password. It lost on a single property — a transfer that outlives
the process doing it. An operator restart mid-copy would have discarded all progress, a large
migration would have tied the operator's memory and CPU to one workload's data, and a copy in flight
would have blocked or starved every other `Bucket`'s reconcile. Every piece of machinery listed
above is the price of the copy having a lifecycle of its own.

**Run the copy job in the `Bucket`'s namespace.** Rejected. It is the more natural place — the data
belongs to that team, the logs would be theirs, and the source credentials would not need to be
staged anywhere. But the destination side authenticates as the project-wide admin identity, and
mounting that credential into a workload namespace hands every bucket in the project to anyone who
can read Secrets there. Losing the log visibility was judged the smaller loss.

**A `namespace` field on `spec.cloneFrom.secretRef`.** Rejected. It would let one team seed from a
Secret another team holds without that Secret being copied, which sounds like a convenience and is a
confused-deputy primitive: the operator holds cluster-wide Secret access, so a reference it follows
on a CR author's behalf is a read the author could not perform themselves
([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) D3/D4).

**Stage the admin credential into the per-clone Secret too, for symmetry with the source side.**
Rejected as machinery with no gain. The Job already runs in the namespace where the admin Secret
lives, so copying that credential into a second Secret in the same namespace changes nothing about
who can read it — it only adds an object to keep in step and to clean up.

**Let the copier's own running total drive the percentage.** Rejected. Its total grows while it is
still scanning the source, so the denominator moves and the percentage falls back as work is
discovered. Measuring the source once before the copy starts costs one listing pass and produces a
number that only ever goes up.

**A repeating sync instead of a one-shot copy.** Rejected. It would make the operator a permanent
data mover, make the source endpoint's availability a standing dependency of a bucket's health, and
— because the copy merges and never deletes (D4) — silently resurrect every object the workload
deleted at the destination. One-shot seeding is a migration primitive; keeping two buckets in step
is a different product.

**Publish the workload credential immediately in all cases.** Taken as an option, not as the
default: `spec.cloneFrom.holdSecretUntilCloned: false` exists for workloads that want the credential
early and can tolerate a partially filled bucket. The default withholds it, because a workload that
starts against a half-copied bucket does not see an error — it sees missing data and acts on it.

## Residual risks

**A compromised copier image is a project-wide credential compromise.** Accepted, mitigated by a
pinned tag and dependency review, not eliminated. An operator who is unwilling to accept it should
not use `spec.cloneFrom`; nothing else in this operator runs foreign code.

**The NetworkPolicy is silently inert on a cluster whose CNI does not enforce NetworkPolicies.**
There the remote-control port is reachable from anywhere in the cluster and only the generated
password stands in front of an interface that can execute commands. The value exists so such
clusters can turn the resource off knowingly rather than believe in a protection they do not have.

**A source that grows during the copy makes the percentage lie.** The denominator is frozen before
the copy starts (D10) and the displayed percentage is clamped at 100, so a clone of a source that
kept receiving writes can sit at 100% while still transferring. The completion signal is the phase
and the condition, never the percentage.

**A permanently unreachable or misconfigured source retries forever.** A failed attempt is deleted
and recreated under reconcile backoff, with no attempt ceiling, and the `Bucket` stays non-`Ready`
for as long as that lasts. A self-clone (D6) is the only source problem refused without a retry; a
`spec.cloneFrom.secretRef` that names a Secret which does not exist, or whose configured data keys
carry no value, is retried under that same backoff like any other clone failure — the operator
waits for the Secret to appear rather than giving up on the clone.

**Not verified: `virtual-hosted` addressing against a real virtual-hosted endpoint.** The offline
suite checks that the setting produces the intended addressing configuration, and the 2026-09-01
cloud run used a path-style source. No run has copied from AWS, or from any endpoint that requires
virtual-hosted addressing, against the real service.

**Not verified: the behaviour of a clone across an operator upgrade mid-transfer.** The design
argument is that the Job survives because it is a separate object and the terminal state is
persisted before cleanup, and a restart is what the crash-safety ordering is for — but no test or
live run has restarted the operator while a copy was in flight.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — why the source Secret reference has
  no namespace, and why the copy's cluster objects live where they do
* [ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the policy the copy has to
  be written through
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the admin identity the
  destination side of the copy authenticates as
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — the destination endpoint and
  region are the operator's, not the CR's
* [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) — why a wrong clone is expensive to
  undo, and what teardown does with a running one
* [ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) — the
  credential whose publication the clone withholds
* [ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) — the readers held
  out of the policy while the copy runs
* [ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md) — why clone state lives in the
  status and in operator-owned objects
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — how a refused self-clone is
  classified against a retryable failure
* [docs/operations/cloning.md](../operations/cloning.md) — declaring a clone, reading its progress,
  and what to do when it fails
* [docs/operations/deletion.md](../operations/deletion.md) — the delete path a re-seed has to go
  through
* [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md) — where the two
  credentials of a copy live and who can read them
* [docs/security/rbac-and-privilege.md](../security/rbac-and-privilege.md) — the Job and Pod
  permissions this decision adds
* [docs/developer/clone.md](../developer/clone.md) — the job and staging-Secret naming, the polling
  and retry mechanics, and the crash-safety ordering
