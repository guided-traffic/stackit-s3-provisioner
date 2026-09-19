# ADR 0007: A workload credential lives in its Secret and rotates only on request

## Status

Accepted. Date: 2026-09-19.

Fully implemented. The no-expiry rule was taken on 2026-06-30, the clear-before-create
ordering shipped with the reconciler on 2026-07-01, and the request-driven rotation on
2026-07-16; this record collects the three into one decision family and states what each
one rejected. One weakness is known and open: the operator decides that a published
credential is still backed by asking whether the credentials group holds **any** access
key, not whether it holds **that** key, so a key deleted out of band while another key
exists in the same group is not noticed (see *Residual risks*).

## Context

A `Bucket` hands its workload one thing: an S3 access key pair, written into a Kubernetes
Secret in the workload's own namespace. Everything in this record follows from one
property of the provider, recorded as a guardrail on 2026-06-30 while the feasibility was
being established: **the secret half of an access key is returned exactly once, in the
response that creates it.** There is no endpoint that returns it again. The provider will
happily list access keys afterwards, but a listing yields key ids only.

That single fact removes the repair path that every other resource in this operator
relies on. A bucket, a credentials group, an isolation policy and a set of ownership tags
can all be read back from the provider and compared against the desired state, so drift is
detectable and self-healing is cheap. A credential cannot. Once the creating response is
gone, nobody — not the operator, not the provider's console — can recover the secret half.
The copy in the Secret is the only copy in existence, which is why the Secret, and not the
provider, is the source of truth for the live credential.

Two consequences follow immediately and neither is free:

**A lost Secret can only be healed by replacing the credential.** If the Secret is deleted,
emptied, or written before the operator ever wrote it, there is nothing to restore. The
only path back to a working workload is a new access key — which invalidates whatever the
workload was using and is therefore a disruptive act performed in response to an
accidental one.

**Ordering during replacement decides whether keys leak.** Creating the replacement first
and deleting the old one afterwards is the intuitive order, and it is the wrong one: a
crash between the two steps leaves a live access key in the project whose secret half
nobody holds and which no later reconcile can recognise as its own, because nothing
distinguishes it from the key currently in the Secret except material the operator no
longer has. Such a key cannot be reasoned about, only listed and deleted wholesale. The
operator therefore clears first and creates second, which trades an unrecoverable leak for
a bounded outage: a crash after the clear leaves the workload without a credential until
the next pass, and that state is visible, self-correcting and survivable.

The same ordering is also what made the credentials-group name collision of 2026-08-24
destructive rather than merely confusing. Two `Bucket` CRs in different namespaces could
compute the same group display name, and the group was then found by that name; the second
CR did not just share a group, it cleared every key in it and wrote a fresh one into its
own Secret — deleting the victim's live credential in the process. The clear-before-create
rule is only safe because a group is attributed through the bucket that owns it and never
through a name, which is the decision recorded in [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md).

Against that background, automatic rotation was deliberately not built. A key that expires
on a schedule turns every consumer into something that must re-read a Secret at a moment
nothing in the cluster forces: a Secret consumed as environment variables is read once, at
pod start, and no controller restarts a pod because a Secret changed. An operator-chosen
expiry is therefore an operator-scheduled outage for every workload that does not restart
on its own. The decision of 2026-06-30 was explicit: version 1 issues keys with no expiry
and performs no rotation of its own, and rotation stays retrofittable.

What remained was how an operator *asks* for a rotation. The obvious levers were both
closed. The operator never writes a `Bucket`'s spec or labels and writes only two pieces of
its metadata — the finalizer and the annotation that freezes the resolved bucket name
([ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md) D1/D2/D3). A rotation
trigger is not among them, so the operator cannot consume an edge by clearing a flag it was
given; and a continuously syncing GitOps controller
re-applies the same manifest indefinitely, so anything that reads as "a rotation was
requested" in the manifest would request one again on every sync. The answer is the shape
`kubectl rollout restart` already established: an opaque value carried in an annotation,
compared against the value the operator last acted upon and recorded in status. Changing
the value in Git rotates exactly once; every later sync of the same value is a no-op.

## Decision

**D1 — The Secret referenced by `spec.secretRef.name` is the source of truth for the live
workload credential.** The operator never attempts to read the secret half back from the
provider, because the provider does not return it. The credential in the Secret is
authoritative until the operator itself replaces it.

**D2 — A workload credentials group holds exactly one access key at a time, and it belongs
to exactly one `Bucket`.** The credential is per-`Bucket`, never shared between CRs, and
the group it lives in is the one that `Bucket` attributes to itself
([ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md)).

**D3 — Access keys are issued without an expiry and the operator never rotates on its own.**
The provider's optional expiry timestamp is left unset, no time-based rotation runs, and no
setting enables one. A key stays valid until this operator deletes it or somebody deletes
it out of band.

**D4 — A pass that finds a usable credential changes nothing.** "Usable" means the Secret
carries both credential values under this `Bucket`'s resolved data keys, the credentials
group still holds at least one access key, and no rotation is pending. Otherwise the pass
(re)provisions under D5 — which makes a deleted or emptied Secret self-healing, at the
price of a new credential.

**D5 — Every existing key in the group is deleted before the replacement is created.**
Clear first, create second, never the reverse. This keeps the key count at one across any
crash and makes it impossible to leave behind a key whose secret half nobody holds.

**D6 — A key whose Secret write fails is deleted immediately.** Its secret half exists only
in the failed write, so the key is worthless the moment the write fails and must not be
left in the project.

**D7 — A failure after the clear is treated as local certainty that the published
credential is dead.** It is not a provider state the operator merely failed to verify, so
the `Bucket` drops `Ready` at once instead of holding it through the degraded grace window
— one of the closed exceptions in [ADR 0012](0012-ready-describes-the-last-verified-state.md).

**D8 — A rotation is requested by the annotation `stackit-bucket.gtrfc.com/rotate-credentials-at`,
and the trigger is level-based.** Its value is opaque (an RFC3339 timestamp by convention);
whenever it differs from `status.lastRotationTrigger`, the operator replaces the key and
records the handled value in `status.lastRotationTrigger` and the time in
`status.lastRotationTime`. An unchanged value is a no-op and removing the annotation
triggers nothing; the comparison is against the single value currently recorded in
`status.lastRotationTrigger`, not against a history, so re-adding that recorded value does
not rotate again while re-adding an older value that has since been superseded does. The
operator never mutates the annotation.

**D9 — Rotation is hard, with no overlap window.** The previous key stops working the
moment it is deleted. There is no second valid credential and no grace period; consumers
must re-read the Secret, which in practice means restarting the pods that hold it.

**D10 — The Secret's data-key names are a per-`Bucket` contract, and a collision is refused
before anything is written.** Six logical fields — `accessKeyID`, `secretAccessKey`,
`bucketName`, `region`, `endpoint`, `bucketURL` — are each overridable under
`spec.secretRef.keys`, defaulting to env-var-style names so the Secret can be consumed
directly via `envFrom`; the credential defaults are `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` (# default). If two logical fields resolve to the same data key the
`Bucket` is parked as a configuration fault without a requeue hammer, because the
alternative is one value silently overwriting another.

**D11 — The operator merges into the Secret and owns it.** Unrelated data keys in the same
Secret are left untouched, the `Bucket` is set as controller owner so the Secret is
garbage-collected with the CR, and teardown removes the Secret the `Bucket` currently
references. The Secret always lives in the `Bucket`'s namespace
([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md)); publishing it may be delayed
until a requested clone has finished ([ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md)).

**D12 — The secret half never leaves the Secret.** It is never written to status, an event,
a log line or a metric. What a rotation leaves behind is exactly this:

| Surface | Value |
|---|---|
| `status.accessKeyID` | the access key id of the live credential — the public half only |
| `status.lastRotationTrigger` | the annotation value the operator last acted upon |
| `status.lastRotationTime` | when that rotation completed |
| Event `CredentialsRotated` | one Normal event per performed rotation |
| Metric `stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds` | Unix time of the last rotation; absent for a `Bucket` that never rotated |

## Consequences

**A rotation is a workload outage until the workload restarts.** Nothing in Kubernetes
propagates a changed Secret into a running process that read it at start. The operator can
publish a new credential; it cannot make anybody use it. Whoever rotates must plan the
restart, and rotating a whole label selector at once means restarting all of those
workloads at once.

**Deleting the Secret costs a credential, not just a Secret.** Under D4 a missing Secret is
indistinguishable from a never-provisioned one, so the next pass mints a replacement and
the previous credential dies. Restoring a Secret backup does not undo this: the restored
values are already invalid if a pass ran in between. A backup of this Secret is a backup of
a live credential and has to be protected like one.

**A failed rotation leaves the workload without a credential.** The clear happens first, so
the window between "the old key is gone" and "the new key is published" is real. It is
short, it is visible (`Ready` drops immediately under D7), and the next successful pass
closes it — but during it the workload's S3 access is down. This is the price paid for
never leaking an unrecoverable key, and it was paid knowingly.

**Anyone who can patch a `Bucket` can kill its live credential instantly.** The rotation
annotation is part of `metadata`, so a patch permission on the CR is enough; no spec
change and no Secret access is required. Inside a namespace this is a denial-of-service
primitive against that namespace's own workload, which is why `Bucket` write is only ever
handed out together with the permissions that already imply it.

**Rotation moves no `metadata.generation`.** The generation guard that elsewhere
distinguishes "converged on the current spec" from "never verified this spec" does not fire
for a rotation, which is precisely why the destroyed-credential case has to be an explicit
exception in [ADR 0012](0012-ready-describes-the-last-verified-state.md) rather than falling
out of the generation comparison.

**Every pass that evaluates D4 costs one control-plane listing of the group's access keys.**
D4 cannot be evaluated from cluster state alone; the "does the group still back this
credential" half is a provider read, paid on every reconcile of every `Bucket` that reaches
the credential step. A pass that is still holding the Secret back for an unfinished clone
([ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md)) returns before
that step and issues no listing.

**There is no audit trail of who rotated.** The recorded trigger value and the
`CredentialsRotated` event say that a rotation happened and which value caused it; the
identity that set the annotation is only in the API server's own audit log, if one is
enabled.

## Alternatives Considered

**Create the replacement first, then delete the old key.** Rejected, although it is the
cheaper and friendlier order: it shortens the outage to nothing, since the new credential
is published before the old one dies. It loses on crash behaviour. A crash between create
and delete leaves a live access key in the project whose secret half was never persisted
and which no later pass can tell apart from the legitimate one — an unrecoverable leak that
grows by one key per crash and can only be cleaned up by deleting every key in the group,
which is the disruptive act the ordering was supposed to avoid. A bounded, visible outage
beat an unbounded, invisible leak.

**Keep the old key alive for a grace period so consumers can migrate.** Rejected. The
isolation policy names the credentials *group* as its principal, not an individual key, so
a retained key is exactly as powerful as the new one — it is not a weaker fallback, it is a
second full credential nobody is tracking. Retiring it later requires a scheduled second
act that must survive operator restarts, and until it runs the group holds two keys, which
breaks the one-key invariant that makes the clear step safe to perform blind. The overlap
would also have to surface in the Secret under a second set of data keys for a consumer to
use it, which the fixed data-key contract does not offer.

**Set an expiry on the access key and rotate automatically.** Rejected for version 1 and
explicitly left retrofittable. An expiry the operator chooses is an outage the operator
schedules for every consumer that does not re-read the Secret on its own, and the operator
has no way to know which consumers those are or to make them restart. Automatic rotation
only becomes safe once something in the cluster is responsible for propagating a changed
Secret into running workloads; until then a human asking for a rotation at a moment of
their choosing is strictly better than a timer choosing for them.

**Rotate by changing a field in `spec`.** Rejected. It would make rotation a spec edit and
therefore a generation bump, which is tidier in one respect, but the field would have to be
consumed — cleared or acknowledged — to avoid re-firing, and the operator does not write to
a `Bucket` spec ([ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md)). Leaving
it set instead makes the spec carry a permanent record of a one-time act, and comparing it
against status is the level-triggered design again, just occupying a spec field.

**Rotate on the presence of an annotation and then remove it.** Rejected. This is
edge-triggered, and it requires the operator to mutate the object it reconciles. Under a
continuously syncing GitOps controller the removal is reverted on the next sync and the
rotation fires again, indefinitely — a rotation loop instead of a rotation. The
level-triggered comparison against `status.lastRotationTrigger` was chosen precisely so
that the desired state in Git and the acted-upon state in the cluster can be compared
without either side writing to the other's half.

**Verify the published credential by reading it back from the provider.** Rejected because
it is impossible, not because it is expensive. A listing returns key ids only, so the
strongest available check is whether the group still holds a key — which is what D4 does,
and which is weaker than it looks (see *Residual risks*).

## Residual risks

**The backing check is count-based, not identity-based.** D4 is satisfied when the group
holds at least one access key; it does not verify that the key id stored in the Secret is
among them. If the workload's key is deleted out of band while some other key exists in the
same group, the operator sees a usable credential and changes nothing, while the Secret in
fact carries a dead credential. The `Bucket` stays `Ready` and the workload fails with
provider-side authentication errors that no status field explains. Comparing the stored key
id against the listing would close this and is the obvious next step; it is not implemented
today.

**A crash between publishing the key and writing status re-rotates.** The handled trigger
value is recorded in status after the credential is published, so a crash in between leaves
the annotation pending and the next pass rotates again. Both rotations are hard, so the
result is correct but costs a second disruption — and a workload that had just picked up
the first new credential is invalidated again.

**Renaming `spec.secretRef.name` on a live `Bucket` rotates the credential.** The new name
looks like an unprovisioned Secret, so D4 fails, D5 clears the group and a fresh credential
is published under the new name. The old Secret is left holding a dead credential; it stays
controller-owned and is therefore removed when the CR is deleted, but until then it is a
plausible-looking Secret full of values that do not work. *Not verified:* no test covers a
rename, and this behaviour is reasoned from the write and delete rules rather than observed.

**A key deleted out of band as the only key is healed by replacement, not by recovery.**
The group holds no key, D4 fails, and the workload receives a new credential it must be
restarted to pick up. There is no path that restores the previous one.

*Not verified:* whether the provider enforces a maximum number of access keys per
credentials group. The one-key invariant of D2 and D5 means the operator has never
approached such a limit, so no limit has been observed either way.

*Not verified:* the behaviour of a consumer that reads the Secret through a projected
volume rather than `envFrom`. The kubelet does refresh mounted Secret volumes, so such a
consumer may pick up a rotated credential without a restart, but this operator has not
tested it and D9 does not promise it.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — why the Secret is always in the `Bucket`'s own namespace and can never be aimed elsewhere
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — the attribution that makes clearing every key in a group safe to do blind
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the operator's own admin credential, which is a different credential with a different lifecycle
* [ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md) — the write contract that forces the trigger to be level-based
* [ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) — why publishing the Secret can be deliberately delayed
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — why a destroyed credential drops readiness at once instead of being held
* [docs/operations/credentials.md](../operations/credentials.md) — how to rotate a workload key and what to restart afterwards
* [docs/operations/gitops.md](../operations/gitops.md) — why a repeated sync of the same trigger value does nothing
* [docs/operations/bucket-status.md](../operations/bucket-status.md) — reading the rotation fields on a `Bucket`
* [docs/operations/monitoring.md](../operations/monitoring.md) — the rotation metric and what it is good for
* [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md) — who can read the Secret and what holding it is worth
* [docs/developer/credentials.md](../developer/credentials.md) — the mechanics: the key-replacement path, the Secret write and the rotation bookkeeping
