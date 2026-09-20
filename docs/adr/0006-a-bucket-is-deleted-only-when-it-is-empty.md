# ADR 0006: A Bucket Is Deleted Only When It Is Empty

## Status

Accepted. Date: 2026-06-30 — the empty-only guard. Extended 2026-07-15 by the `spec.wipeOnDelete`
exception and its operator-wide gate; written down as a record on 2026-09-19, no rule changed.

Implemented and in use: the emptiness decision runs at the head of every teardown that reaches the
provider — there is nothing to decide when the physical bucket is already gone, and an operator
running without a service-account key makes no provider call at all (Residual risks) — the wipe
exception is gated off by default and additionally requires the bucket's ownership tags to match,
and a refused wipe degrades to the guard with a warning event. Verified on 2026-09-19 by reading
the teardown path and its offline suite (non-empty bucket blocks and the CR survives, wipe
requested with the gate on deletes objects, versions and delete markers and then the bucket, wipe
requested with the gate off or on a foreign-tagged bucket leaves all three objects in place). The
cloud end-to-end suite installs the operator with the wipe gate on and asks for a wipe on the
buckets it writes objects into, so a cloud run exercises the wipe path against the real provider;
*not verified:* that suite was read, not run, for this record. Two things are open and carried under
Residual risks: the emptiness test counts current objects only, and the alert keyed on the
`Deleting` phase does not observe a blocked deletion.

## Context

Every cloud resource this operator manages is reachable from one namespaced object. Deleting that
object is routine and often not a decision anybody made about the data: a manifest is dropped from
Git and a GitOps controller prunes it, a namespace is deleted, a chart is uninstalled, a `kubectl
delete` hits the wrong context. On the other side of that object sits a bucket whose contents are
somebody's backups, artifacts or registry. Object storage has no undo and no recycle bin, the
operator keeps no copy anywhere, and the deletion is a control-plane call that does not come back.
So the question decided on 2026-06-30, and recorded that day as the delete semantics of the
project alongside the tenancy model, the region and the key-expiry policy, was not *how* to delete
but *whether a CR deletion is allowed to be the whole distance to irreversible data loss*. The
answer was no: delete only when the bucket is empty, otherwise fail the reconcile — data loss is
not an outcome a manifest change may produce by itself. The guard was implemented with the
reconciler on 2026-07-01.

A second force decided where in the teardown the check sits. Tearing down a Bucket means removing
several things in sequence: a running clone, the access keys, the credentials group, the bucket
itself, and the workload's credentials Secret. If the emptiness decision came anywhere but first,
a blocked deletion would still have destroyed the workload's credential before stopping — the data
would survive and nothing would be able to read it, which is the same outage as losing the data
plus an unusable bucket nobody can empty. Putting the decision at the head makes a blocked
deletion a full no-op against everything that matters: the key still works, the Secret is
untouched, the workload keeps running, and a human decides what happens to the objects.

The guard's cost showed up quickly. A refusal is permanent until somebody empties the bucket by
hand, which is exactly right for production data and exactly wrong for a bucket that was created to
be thrown away. The project's own end-to-end suite against the real provider is the sharpest
example: it writes objects into the buckets it provisions, so without a way past the guard every run
would leave real buckets, credentials groups and live access keys behind in the cloud. The wipe
exception was added on 2026-07-15 for that shape of bucket. It had to be built so that it could
never become the ordinary path — an opt-in field on a namespaced manifest is, by itself, one line
of YAML between a CR deletion and destroying every object and every version in a bucket. The
answer was to require three independent yes-es from three different authorities: the manifest
author asks for it, the person who installed the operator allowed it at install time, and the
bucket's own ownership tags have to prove this operator provisioned it.

## Decision

**D1 — A Bucket CR is not released until the cloud state behind it is gone.** A finalizer keeps the
object alive through teardown; a teardown that cannot complete keeps the finalizer, and with it the
only record of what still has to be cleaned up. A deletion that is refused stays visible as an
undeleted CR rather than disappearing and leaving orphans.

**D2 — A bucket that holds objects is not deleted.** The teardown ends with an error, the finalizer
stays, the CR stays, and the bucket and its contents stay. This is the default and it is not
configurable per cluster in any direction other than D4.

**D3 — The emptiness decision is made before anything is destroyed.** The only step allowed to
precede it is stopping a clone that is still writing into the bucket. The destructive steps run in
the order below, and none of them starts before the decision has been made.

| Order | Step | Runs before the emptiness decision? |
|---|---|---|
| 1 | Stop a running clone (its Job and its staging Secret) | Yes — it would otherwise keep writing during teardown |
| 2 | Emptiness decision, or the authorized wipe of D4 | This is the decision |
| 3 | Delete the access keys of the attributed credentials group | No |
| 4 | Delete the attributed credentials group | No |
| 5 | Delete the bucket | No |
| 6 | Delete the workload credentials Secret | No |

A blocked deletion therefore leaves the workload fully functional: its access key is live and its
Secret is unchanged.

**D4 — A wipe needs three independent authorizations, and all three every time.** The CR asks for
it with `spec.wipeOnDelete: true`; the operator-wide gate must be on
(`--enable-wipe-on-delete`, also settable as the environment variable `ENABLE_WIPE_ON_DELETE`,
Helm value `wipeOnDelete.enabled`, `false` # default); and the bucket's ownership tags must prove
this operator provisioned it for this CR. Two out of three is a refusal, not a wipe.

**D5 — A refused wipe degrades to D2, never to silence and never to a partial wipe.** The operator
emits the warning event `WipeOnDeleteSkipped` naming which authorization was missing and then
applies the emptiness decision unchanged, so the outcome of a misconfiguration is a blocked
deletion, never a deleted object.

**D6 — An authorized wipe removes every object version and every delete marker**, not only the
current objects, because a bucket that still carries non-current versions still carries the data
and still costs storage. A wipe is idempotent: on an already-empty bucket it does nothing.

**D7 — Nothing is deleted or wiped that the ownership tags do not claim.** A bucket carrying
foreign tags, or no tags at all, is neither wiped nor deleted; it is left standing and reported as
a warning event. Teardown continues past it, so the CR is eventually released while the bucket
remains — the operator refuses to destroy what it cannot prove is its own, and refuses equally to
hold a CR hostage to a bucket it has no claim on.

**D8 — A blocked deletion is reported, never silently forced.** The CR keeps its finalizer, the
status phase and `status.message` name the refusal, and a warning event with reason `Failed`
carries the same text. There is no timeout after which the operator gives up and deletes anyway,
and no field that shortens the wait.

**D9 — A deletion the operator cannot even attempt is deferred, not approximated.** While the
fleet-wide provider circuit is open no teardown makes a provider call at all; the finalizer stays
and the deletion resumes on the next probe. The same holds when the emptiness decision cannot be
made because the admin credential that makes it is unavailable. Not being able to check is never
treated as "empty".

## Consequences

* **Deleting a Bucket CR can hang indefinitely, and that is the feature.** A namespace owner who
  deletes a manifest with data behind it gets a CR that will not go away until the data is dealt
  with. In GitOps this surfaces as a prune that never finishes, which is the visible form of "the
  cluster is telling you something".
* **A blocked deletion holds a name.** The physical bucket, its credentials group and its Secret
  all still exist, so a new Bucket CR aimed at the same bucket name collides instead of
  provisioning.
* **Manual intervention has a deliberate cost.** Getting past the guard means emptying the bucket
  with an S3 client, or setting `spec.wipeOnDelete` on a cluster where the gate is on. Removing the
  finalizer by hand is possible and releases the CR, at the price of leaving the bucket, its group
  and a live access key behind with nothing left in the cluster that names them.
* **Throwaway environments must be configured for it.** Any automated test or ephemeral environment
  that writes objects has to run with the gate on, or it leaks real buckets and live credentials.
  The project's own cloud end-to-end suite does exactly this, and that is the honest measure of how
  often the exception is genuinely needed.
* **Two authorities must agree in advance.** A platform team that leaves the gate off has taken the
  wipe away from every namespace in the cluster, including the ones that would use it correctly;
  they find out only when a delete blocks. The alert `StackitS3WipeRequestedButGateDisabled` exists
  to shorten that discovery: it fires after the condition — a CR asking for a wipe the gate does not
  permit — has held for 15 minutes.
* **A CR that carries `spec.wipeOnDelete` is dangerous in a way nothing else in the API is**, so it
  is exported as its own metric (`stackit_s3_provisioner_buckets_wipe_on_delete`) and alerted on
  merely for existing (`StackitS3BucketsWipeOnDelete`, after 5 minutes). Both this alert and the
  gate-disabled one are enabled by default *within the chart's rule set*, but the rule set itself is
  opt-in (`monitoring.prometheusRule.enabled`, `false` # default): a default install ships no
  PrometheusRule and therefore no page.
* **The field stays mutable on purpose**, so a bucket can be marked for wipe immediately before its
  CR is deleted rather than carrying the marker for its whole life. The cost is that the
  authorization can be added after the fact by anyone who can edit the CR; the operator-wide gate
  is what keeps that from being sufficient on its own.
* **The visible signals of a blocked deletion are these**, and nothing else:

| Signal | Value while a deletion is blocked |
|---|---|
| The CR | Still present, finalizer intact, `deletionTimestamp` set |
| `status.phase` | `Failed` after the first attempt (`Deleting` only between the attempt starting and its refusal) |
| `status.message` | The refusal, naming the bucket |
| Event | Warning, reason `Failed`, same text; `WipeOnDeleteSkipped` in addition when a wipe was asked for and refused |
| Metric | Counted in `stackit_s3_provisioner_buckets{phase="Failed"}` |
| Alert | `StackitS3BucketFailed` after 15m, if the chart's opt-in rule set is installed |

## Alternatives Considered

**Delete whatever the CR points at.** The obvious behaviour, and the one every reader assumes
before being told otherwise: a CR is a declaration, removing it removes what it declared. Rejected
because the blast radius is not symmetric with the effort. Creating a bucket costs one manifest and
is fully reversible; destroying a bucket costs the same manifest and is not reversible at all. An
interface where those two cost the same is mis-priced.

**Attempt the delete and let the provider refuse a non-empty bucket.** This was the cheaper option
— no listing, no admin credential needed at teardown, no guard to maintain — and it lost on
ordering. The provider can only answer about the bucket, and the bucket is destroyed fourth: by the
time the refusal arrives, the access keys and the credentials group are already gone and the
workload that could have emptied the bucket has no credential left. A guard that fires after the
credential is destroyed converts a recoverable situation into an unrecoverable one. It also
inherits whatever the provider happens to do, and *not verified*: whether the provider refuses to
delete a non-empty bucket at all.

**A `deletionPolicy` field with `Delete` / `Retain` / `Orphan`.** The Kubernetes-idiomatic shape,
and rejected for what `Retain` leaves behind: a bucket, a keyed credentials group and a live access
key standing in the cloud with no object in the cluster naming them any more. The empty-only guard
already provides retention — the CR *is* the record, kept alive by its finalizer, and it points at
everything that still exists. Retention without a record is not retention, it is a leak.

**The wipe as a CR field only, with no operator-wide gate.** Rejected: it would put irreversible,
fleet-wide destructive capability behind one boolean in a namespaced manifest, writable by anyone
who can write a Bucket. The gate splits the authorization between the person who installs the
operator and the person who writes the manifest, and defaults to the safe half being absent.

**The wipe gated only by the operator-wide switch, without the ownership check.** Rejected. The
gate says "wipes are allowed in this cluster", which is not the same statement as "this bucket is
this operator's to destroy". A bucket that merely shares a name — a pre-existing one, or one
belonging to a different operator in the same project — must not be emptied because a CR in some
namespace asked for it.

**Wiping by default for any bucket the operator itself created.** Rejected. Provenance is not
consent: the operator creates precisely the buckets that hold the data it was asked to manage, so
"we made it" would authorize destroying every bucket in the fleet.

## Residual risks

* **The emptiness test counts current objects only.** Verified in the code: it asks for a single
  current object and treats "none" as empty, without listing versions. A versioned bucket whose
  current objects have all been deleted therefore reads as empty and the operator proceeds to
  delete it, even though non-current versions and delete markers still hold data and still cost
  storage. *Not verified:* what the provider does with that delete — whether it refuses, in which
  case the deletion blocks with a provider error instead of the guard's message, or accepts and
  discards the versions.
* **The alert keyed on the `Deleting` phase does not observe a blocked deletion.** Verified in the
  code: the first failed teardown attempt parks the CR in `Failed`, and the phase is deliberately
  not rewritten afterwards so a blocked delete does not oscillate and retrigger itself. The alert
  `StackitS3BucketStuckDeleting` waits 30 minutes on the `Deleting` phase and will practically
  never see it; what actually fires is `StackitS3BucketFailed`, whose text is about configuration
  faults rather than blocked deletions.
* **An operator running without provider credentials releases CRs without touching anything.**
  There is no cloud client to check emptiness with, so the finalizer is dropped immediately.
  Nothing is destroyed, but nothing is cleaned up either, and the record of what exists in the
  cloud goes away with the CR. Relevant when a cluster is rebuilt or re-installed without its
  service-account key.
* **A wipe is not re-checked for emptiness before the delete.** Objects written between the wipe
  and the bucket deletion are not seen; the delete either succeeds having discarded them or fails
  and the whole teardown, wipe included, is retried.
* **The wipe has no dry run and no confirmation at delete time.** Once all three authorizations of
  D4 are in place, deleting the CR destroys the contents without a further prompt. The metric and
  the alert on `spec.wipeOnDelete` are the only advance warning, and the alert only where the
  chart's opt-in rule set is installed.
* **Removing the finalizer by hand defeats every rule here.** It is a legitimate operator action,
  it is not blocked, and it leaves live cloud resources unreferenced.
* **Not verified:** whether the options under Alternatives Considered were weighed at the time.
  They are reconstructed from the material and the code as it stands on 2026-09-19; the reasons
  given are the ones that hold today, not minutes of a meeting.

## References

* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — its D4 decides which
  credentials group step 3 of D3 is allowed to release
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the credential that
  makes the emptiness decision and performs the wipe; its unavailability is the D9 case
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — the deployment model behind
  the credential-less mode named in Residual risks
* [ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — which cloud bucket a
  teardown acts on
* [ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) — the clone that step 1
  of D3 stops before anything else
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — why a Bucket being torn down never
  holds its readiness through a failure, so a blocked deletion is always visible
* [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) — the open circuit that defers a
  teardown under D9
* [docs/operations/deletion.md](../operations/deletion.md) — what deleting a Bucket does, why a
  delete hangs, and how to get past it safely
* [docs/operations/monitoring.md](../operations/monitoring.md) — the metrics and alerts named here
* [docs/security/ownership-and-attribution.md](../security/ownership-and-attribution.md) — the
  ownership tags D4 and D7 rest on, and where that proof does not reach
* [docs/developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md) — the teardown pass:
  order, the emptiness and wipe implementations, and the status writes behind a blocked delete
