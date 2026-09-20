# ADR 0010: The Operator Never Writes to a Bucket Spec

## Status

Accepted. Date: 2026-09-19.

D1 through D8 hold in the tree today. Verified by reading the controller: a `Bucket` object is
written outside the status subresource in exactly three places — the finalizer is added before
any provisioning work, the finalizer is removed after the last cloud resource is gone, and the
frozen physical name is written into an annotation before any cloud resource is created. No code
path assigns to a field of `spec` or to a label of a `Bucket`. Open: the composite property this
record asserts — that a re-apply of an unchanged manifest is a no-op — is verified per rule and
not as a whole; no test drives a syncing controller against a live operator (see Residual risks).

## Context

A `Bucket` is applied from a Git repository and kept there by a controller that re-applies it
continuously. That controller also health-checks the object: on 2026-08-25 a two-minute provider
blip on the mgmt-p cluster flipped all 19 Buckets out of `Ready`, and roughly eleven tenant
Kustomizations went unhealthy behind them because their readiness gate is the `Bucket` itself
(the readiness half of that incident is [ADR 0012](0012-ready-describes-the-last-verified-state.md)).
The relevant fact here is the coupling it proved: these objects are not operator-private state.
They are continuously reconciled by a second controller that has its own opinion about what they
should contain.

Two controllers writing one object fight in a way that is hard to see. If the operator writes
anything the applier also declares, each sync reverses the other's write; if the operator writes
a field the applier does not declare, a strict apply configuration removes it on the next sync
and the operator restores it on the next reconcile. Neither shows up as an error — it shows up
as an object that changes forever, as provider calls nobody asked for, and, where the write is
a trigger, as work that repeats on every sync. An edge-triggered rotation is the sharp case: if
the operator cleared the trigger it had handled, Git would put it back, and the workload
credential would be replaced on every sync interval.

The same reasoning forces a second rule that is easy to miss. The operator writes status
continuously — clone progress, size measurements, degradation bookkeeping — and a controller
that reacts to its own status writes never stops. So the `Bucket` trigger is narrowed to changes
of `metadata.generation` and of annotations. That narrowing has a cost, and the cost was
measured: on 2026-07-22, on the infra-d cluster, an upgrade from operator version 1.6.0 to 1.9.0
did not reach the buckets it was meant to fix. `test-bucket` kept its old five-action isolation
policy although the new code produced eight. Nothing appeared in the log. Three causes compounded:
a successfully reconciled Bucket returned no requeue, the narrowed trigger discarded the periodic
re-delivery that would otherwise have re-reconciled an unchanged object, and a rolling update ran
two operator versions concurrently so the older one could write last. The policy code was correct
throughout — resetting the live policy by hand and touching the CR healed it immediately. A
restart does reconcile every object once, so the new version did reach that bucket; what was
missing was anything that looked again after the losing write landed. Closing that gap is what
D8 exists for.

The third force is the failing object. An invalid manifest cannot be fixed by retrying it, and
a fleet of objects retrying a fault they cannot fix is not free: on 2026-09-02 the operator drove
its own source-IP rate limit at the provider into `429 rate limit on IP level exceeded` during an
outage, producing 242 reconcile errors for a single self-healing incident (see
[ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md)). A fault whose only cure is an edit in
Git should wait for that edit, not consume the shared budget while it waits.

## Decision

**D1 — The operator never writes a Bucket's `spec`.** Every field of `spec` belongs to whoever
applied the manifest. A value the operator computes — a physical bucket name, a credentials group
identifier, an access key id — is published in `status` or in the credentials Secret, never
written back into the object being reconciled.

**D2 — The operator never writes a Bucket's labels.** Labels on a `Bucket` are the applier's
alone; the operator selects nothing by them and adds none. Labels on objects the operator itself
creates and owns — the credentials Secret, a clone Job and its staging Secret — are not covered by
this rule; those are its own objects.

**D3 — The operator writes exactly two pieces of a Bucket's metadata, and nothing else.**

| Metadata written | When | Lifecycle |
|---|---|---|
| Finalizer `stackit-bucket.gtrfc.com/finalizer` | Added before any provisioning work begins | Removed once the last cloud resource of the Bucket is released ([ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md)) |
| Annotation `stackit-bucket.gtrfc.com/resolved-bucket-name` | Written before any cloud resource is created, carrying the physical bucket name ([ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3) | Added once; thereafter only ever written back as the value already frozen in `status.resolvedBucketName`. Never removed, never recomputed from the current naming policy |

Any future need to record something on a `Bucket` outside `status` amends this table or does not
happen.

**D4 — Everything the operator observes goes to the status subresource.** A status write does not
change `metadata.generation`, so it is invisible to a manifest diff and to a syncing controller's
drift detection.

| What the operator observes | Where it is reported |
|---|---|
| Coarse phase and the authoritative machine-readable state | `status.phase`, `status.conditions`, `status.message`, `status.observedGeneration` |
| The provisioned identity of the bucket and its credential | `status.resolvedBucketName`, `status.bucketURL`, `status.credentialsGroupID`, `status.credentialsGroupURN`, `status.accessKeyID` |
| Which read grants resolved | `status.grantedReadTo` |
| Clone progress and outcome | `status.clone` |
| Size measurement and cost estimate | `status.usage` |
| How long the provider has been failing | `status.degradedSince` |
| The rotation trigger already handled, and when | `status.lastRotationTrigger`, `status.lastRotationTime` |
| Which operator version last wrote | `status.operatorVersion` |

**D5 — A configuration fault parks the object instead of retrying it.** The Bucket is set to
phase `Failed` with the `Ready` condition `False`, reason `Failed`, and the failure text in
`status.message`; `status.observedGeneration` is advanced; a Warning event is recorded; and the
reconcile ends without a requeue. These faults are the ones an edit in Git fixes and a retry
cannot:

| Fault | What causes it |
|---|---|
| Colliding Secret data keys | Two logical fields of `spec.secretRef.keys` resolve to the same key name, which would silently lose one of them |
| Admin Secret targeted | `spec.secretRef.name` points at the operator's own admin credentials Secret ([ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md)) |
| Foreign region | `spec.region` differs from the single region this operator serves ([ADR 0005](0005-the-operator-serves-one-project-in-one-region.md)) |
| Invalid composed name | The physical name composed from the naming policy is not DNS-compliant or not 3–63 characters ([ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D5) |
| Ownership collision | A bucket of that name exists and its ownership tags say it is not this Bucket's ([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) D2), or it carries no tags and is not empty |
| Self-clone | `spec.cloneFrom` names this very bucket at this very endpoint ([ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D6) |

**D6 — Every trigger the operator offers is level-based, and the operator never writes the trigger
it reads.** A trigger is an opaque value the applier owns; the operator compares it against what
it recorded in `status` and acts only on a difference. The rotation trigger
`stackit-bucket.gtrfc.com/rotate-credentials-at` is compared against `status.lastRotationTrigger`
([ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D8); a
clone is terminal once `status.clone.phase` is `Completed`
([ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D5). Re-applying the same
value is a no-op, and removing the annotation triggers nothing.

**D7 — No trigger the operator registers may be satisfied by its own routine writes.** A Bucket's
own update trigger ignores status-only changes: it fires on a change of `metadata.generation` or
of annotations, so the continuous status writes of D4 do not re-enter the loop. Creation and
deletion of a `Bucket` always pass, which is why an operator restart reconciles every Bucket once.
The rule is enforced trigger by trigger rather than by a blanket exemption for operator writes:
the second Bucket trigger that wakes a grantor of a read grant reacts only to a change of the
grantee's `status.credentialsGroupURN` ([ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D11),
and the Secret trigger reacts only to Secrets the operator itself labels
`app.kubernetes.io/managed-by: stackit-s3-provisioner`. Where the operator must write metadata
under D3 it continues the same pass rather than waiting for the event its write produces.

**D8 — Because D7 also discards the periodic re-delivery of unchanged objects, a successfully
provisioned Bucket is re-reconciled on a timer.** The interval is an install-time setting
(`--drift-resync-interval`, environment `DRIFT_RESYNC_INTERVAL`, Helm key `driftResyncInterval`,
`10m` # default; `0` disables it). The timer is not what carries a new operator version onto
untouched Buckets — a restart already reconciles every Bucket once. It is what looks again
afterwards: it heals drift introduced between restarts, and it repairs a stale result left behind
when two operator versions overlap during a rollout and the outgoing one writes last, which is the
2026-07-22 failure.

## Consequences

Re-applying the Git state is a no-op, and that composes: an application rollout needs no
dependency choreography, because the credentials Secret appears when the bucket is usable and
pods consuming it stay pending until then. A restore of the same manifests into a fresh cluster
re-adopts the existing buckets rather than duplicating them, because identity is derived from
install-time values and the CR's namespace and name rather than from cluster state
([ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D6).

A parked Bucket needs a human. D5 buys quiet at the price of blindness to a fault that clears
itself elsewhere: an ownership collision resolved by whoever owned the colliding bucket is not
noticed, because a parked object is not requeued and D8's timer only applies after a successful
reconcile. Such a Bucket resumes when its generation or an annotation changes, or when the
operator restarts and reconciles everything once.

D8 is a standing cost, not a free repair. Every provisioned Bucket calls the provider on a fixed
interval whether or not anything changed, and the fleet multiplies it — which is part of why an
outage is held fleet-wide rather than per Bucket
([ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md)). Without it, a rollout in which the
outgoing version writes last leaves a stale result standing until somebody edits the CR — the
2026-07-22 failure.

Two of the operator's own writes do wake it, and both are one-off rather than a loop: the
name-freeze annotation of D3, because an annotation change is a trigger under D7, and the first
write of the workload credentials Secret, because that Secret carries the operator's own
`app.kubernetes.io/managed-by` label and the Secret trigger selects on exactly that label. Each
costs one extra reconcile pass over a Bucket's lifetime; a steady-state pass rewrites neither and
stays quiet.

Level-based triggers mean there is no "run now": an operator who wants an immediate rotation
changes a value in the manifest, and cannot ask the operator to clear it afterwards. The trigger
also does not move `metadata.generation`, so a guard keyed to the observed generation does not
cover a rotation pass — [ADR 0012](0012-ready-describes-the-last-verified-state.md) accounts for
that separately.

## Alternatives Considered

**Write the resolved physical name back into `spec.bucketName`.** Rejected. It is the cheapest
possible storage for the frozen name — no second field, no annotation, no status dependency — and
it loses on the central property: the field is declared in Git, so each sync would reverse the
operator's write and each reconcile would reverse the sync's. The field is additionally immutable
at admission, so the write would simply be refused.

**Keep the frozen name in `status` only, and write no metadata at all.** Rejected, and this is the
option that was cheaper and lost anyway: it would make the finalizer the sole metadata exception
and remove the annotation's extra reconcile noted above. It loses because `status` is not part of a backup
restore or of a re-applied manifest. A CR that comes back without its status would recompose the
name from the operator's *current* naming policy and provision a second, empty bucket beside the
live one, leaving the data stranded under the old name.

**Recompose the physical name on every reconcile instead of freezing it.** Rejected. It needs no
persistence of any kind, and it makes the naming policy a live setting rather than an install-time
one — which is exactly the failure: changing a prefix would silently re-point every existing
Bucket at a bucket that does not exist yet.

**An edge-triggered rotation: a flag the operator clears when it has acted.** Rejected. It is the
smaller implementation — one field, no status bookkeeping, no comparison — and it is unsafe in
precisely this environment. The clear is a write into an object a second controller re-applies
from Git, so the flag returns on the next sync and the workload credential is destroyed and
replaced on every sync interval. Unbounded credential churn is a worse failure than a trigger
value that has to be edited to be used.

**Retry a configuration fault with backoff, like any other error.** Rejected. Uniform handling is
simpler and needs no classification of errors by origin. It loses on evidence: a permanently
invalid object retried forever spends the operator's shared provider budget, and the 2026-09-02
incident showed that budget is real and reachable — the operator drove its own source IP into the
provider's rate limit and produced 242 reconcile errors for one self-healing outage. A fault only
an edit can fix waits for the edit.

**Trigger on every update of a Bucket, with no filtering.** Rejected. It is the default behaviour
and needs no code, and it cannot survive D4: the operator writes status during a clone every few
seconds, and each write would queue another reconcile, which would write status again. The
resulting loop is self-sustaining and hits the provider on every turn.

## Residual risks

A parked Bucket under D5 stays parked while a fault it did not cause is cleared elsewhere. This
is accepted: the alternative is a retry loop with no terminating condition. An operator restart
or any edit to the CR resumes it.

The freeze annotation of D3 is metadata that no manifest declares and therefore no syncing
controller owns. Not verified: how a server-side-apply configuration that prunes fields it does
not own treats it. If it were removed while `status.resolvedBucketName` survived, the name would
still be correct; only the loss of both would recompose it.

Not verified: no incident is on record of a syncing controller actually fighting this operator
over a `Bucket`. D1 through D3 are preventive; what is measured is the cost side — the 2026-07-22
propagation gap — not the failure they avert.

Not verified beyond Flux: the claim that re-application is a no-op has been exercised in practice
only against Flux on the mgmt-p and infra-d clusters. Whether another tool's diff engine reports
the operator-added annotation or the finalizer as drift has not been checked.

D8's timer is an install-time value and can be set to `0`. An installation that disables it keeps
every rule of this record, and an operator restart still reconciles every Bucket once, so an
upgrade still reaches them. What is lost is everything that looks again afterwards: drift
introduced between restarts, and the stale result of a rollout in which two operator versions
overlapped and the outgoing one wrote last, are then never repaired.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — the Secret this operator writes instead of writing to the CR lives in the Bucket's own namespace, and the ownership tags whose mismatch is the collision fault of D5
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — the credentials group is attributed through the bucket's own tags, never through anything in the spec, so D1's published group identifier is a report and not a source
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the admin Secret a Bucket may not target
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — the region guard in D5
* [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) — what the finalizer of D3 guards
* [ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) — the level-based rotation trigger of D6
* [ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) — D11, the grantor trigger that D7 narrows to a single status field
* [ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — the frozen name recorded by the annotation of D3
* [ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) — the terminal clone of D6
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — readiness under provider failure, and the generation guard a rotation bypasses
* [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) — the shared provider budget D5 and D8 spend
* [docs/operations/gitops.md](../operations/gitops.md) — the integrator-facing form of these rules
* [docs/operations/bucket-status.md](../operations/bucket-status.md) — how a parked Bucket looks and what to change
* [docs/developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md) — where the writes and the guards of D3, D5 and D7 sit in the pass
* [docs/developer/bucket-identity.md](../developer/bucket-identity.md) — how the frozen name is chosen and read back
