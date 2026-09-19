# Reconcile pipeline

This page is about the `Bucket` controller's control flow: the order of the provisioning pass, the
order of the teardown pass, which failure lands in which terminal state, and which events are
allowed to wake the controller at all. It is the page to read before changing
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go).
The steps it sequences have pages of their own and are only named here — the physical bucket name
and the ownership tags in [bucket-identity.md](bucket-identity.md), the policy document in
[bucket-policy.md](bucket-policy.md), the access key and the Secret in [credentials.md](credentials.md),
the copy job in [clone.md](clone.md), the size measurement in [usage-measurement.md](usage-measurement.md),
the hold and the fleet-wide breaker in [provider-errors.md](provider-errors.md) and
[circuit-breaker.md](circuit-breaker.md).

This page names files and functions on purpose. It goes stale when the tree moves, and whoever
moves the tree updates it in the same change.

## Entry: what `Reconcile` decides before the pass begins

[`BucketReconciler.Reconcile`](../../internal/controller/bucket_controller.go) is a router, not a
worker. In order:

| # | Check | Outcome |
|---|---|---|
| 1 | `Get` returns not-found | Nothing to do; the object was deleted after the request was queued |
| 2 | `DeletionTimestamp` is set | `reconcileDelete` — the teardown pass |
| 3 | Finalizer absent | `AddFinalizer` + `r.Update`, then **continue in the same pass** |
| 4 | `r.Stackit == nil` (skeleton mode) | `Ready=False`, reason `NotImplemented`, phase `Pending`, no cloud call ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D6) |
| 5 | `r.Breaker.Allow()` says no | `holdForProvider` — requeue on the breaker's cooldown, no provider call, no error ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D5) |
| 6 | otherwise | `reconcileNormal` — the provisioning pass |

Step 3 is where the hot-loop rule first bites. Adding the finalizer is an `Update` on metadata, and
the `Bucket` watch filters on generation and annotations, so that write produces no event the
controller would act on. The pass therefore continues inline rather than returning and waiting for
a wake-up that never comes — [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D7
states the rule, this is the mechanical consequence.

## The provisioning pass

`reconcileNormal` and its helper `provisionCredentialsAndClone` are one sequence; the split exists
only to keep each function readable. Every step is idempotent, and every step that can fail is
routed through `fail` or `failNoRequeue` (see [Guards](#guards-and-their-outcome-class)).

| # | Step | Function | What it enforces |
|---|---|---|---|
| 1 | Validate the Secret data-key names | `specGuardError` → `Bucket.ValidateSecretKeys` | Two logical fields resolving to the same key name would silently lose one ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D10) |
| 2 | Refuse a `secretRef` aimed at the admin Secret | `specGuardError` → `isAdminSecret` | Pollution now, admin-key destruction at teardown ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D8) |
| 3 | Refuse a foreign region | `specGuardError` | `spec.region` declares, it does not select ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D3/D4) |
| 4 | Resolve and freeze the physical name | `decideBucketName`, `s3v1.ValidateBucketName`, `persistResolvedName` | Status → annotation → fresh composition; a freshly composed name is validated, then written to the annotation **before** any cloud resource exists ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3/D5) |
| 5 | Mark the coarse phase | `markProvisioning` | Best-effort progress hint, skipped once the generation has settled on `Ready`/`Failed` |
| 6 | Ensure the admin credential | `ensureAdmin` | One S3 admin credential per project, cached in-process, bootstrapped only when the Secret is absent or incomplete ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D5/D9) |
| 7 | Ensure the Object Storage service | `stackit.Client.EnsureService` | A structured 404 is the only answer that leads to `EnableService`; the result is cached per process |
| 8 | Ensure the bucket, stamp ownership tags | `ensureBucket` → `adoptOrCollide` | Adopt only on matching tags; an untagged bucket is claimed only when empty ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D2) |
| 9 | Derive connection info | `stackit.Client.BucketConnInfo` | Endpoint host and path-style bucket URL, both published in the Secret |
| 10 | Resolve the workload credentials group | `resolveWorkloadGroup` | Bucket tag → own policy (migration) → create; never by display name ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1–D3, D8) |
| 11 | Publish the group identity in status | inline in `provisionCredentialsAndClone` | Written immediately after resolution, not on the terminal write — see [the grantee watch](#the-grantee-watch) |
| 12 | Resolve read grants | `resolveReadGrants` | Reader principals come from the grantee's bucket, never from anything a namespace user writes ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3) |
| 13 | Write the isolation policy | `ensureBucketPolicy` → `stackit.BuildIsolationPolicy` | Rewritten only on drift (`PoliciesEquivalent`); readers are held out while a clone runs ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8, [ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D10) |
| 14 | Refuse a self-clone | `validateCloneSource` | Same endpoint host and same bucket name ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D6) |
| 15 | Run the clone, if requested | `ensureClone` | Returns `done=false` while the copy runs; the pass ends there and polls ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md)) |
| 16 | Re-write the policy with readers | `applyPolicy` closure | Only reached in the pass in which the copy finishes, so grants land without waiting a cycle |
| 17 | Publish the access key and the Secret | `ensureAccessKeyAndSecret` | Secret is the source of truth; clear-before-create ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D1/D5) |
| 18 | Record a handled rotation trigger | `recordPendingRotation` | Turns the annotation back into a level-triggered no-op ([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D6) |
| 19 | Terminal status write, `Ready=True`, `clearDegraded`, `Breaker.Success()` | inline in `reconcileNormal` | The provider answered for every step of this pass |
| 20 | `RequeueAfter: DriftResyncInterval` | inline | See [Drift resync](#drift-resync) |

Two orderings in that list are load-bearing and easy to break by accident.

**The policy is written before any workload credential exists** (step 13 before step 17), and
before a clone starts (step 13 before step 15). A bucket is therefore never open to the rest of the
project while it is being filled. The admin group stays exempt, which is exactly what the clone
Job's destination side authenticates with
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D3).

**Granted readers are held out of the policy while a clone runs**, and the bucket's own workload is
held out of its Secret by `holdSecretUntilCloned` (`true` # default). The two mechanisms are not
interchangeable: `holdSecretUntilCloned` protects only the bucket's own workload, because a granted
reader already holds working credentials of its own and only the policy can hold it back
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D10). With
`holdSecretUntilCloned: false` the key and Secret are published at step 15 instead, and
`recordPendingRotation` runs there too — otherwise a pending rotation trigger would re-rotate on
every 15-second clone poll, since the terminal status write of step 19 is never reached while the
copy runs.

### Bucket existence is a listing, not a read

Step 8 decides existence with `stackit.Client.HasBucket`, which calls `ListBucketNames` and looks
for the name ([`stackit/client.go`](../../stackit/client.go)). A listing that succeeds and does not
contain the name returns "absent" with no error, and the next statement creates the bucket, waits
for it to become visible (`WaitBucketVisible`, bounded by `bucketVisibleTimeout`, 60s) and stamps
the ownership tags. There is no per-bucket existence read, so **"deleted behind our back" and
"never created" are the same input** to the pass. The degradation path of
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) is reached only when the listing
call itself fails.

### Read-grant resolution: three sentinels, and what each does

`resolveReadGrants` calls `resolveWorkloadGroup` on the *grantee's* bucket with `create=false` and
with the pass's shared `groupIndex` — that argument pair is the mechanical expression of
[ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D4, "resolving
a grant creates nothing". Three sentinel errors come back from the attribution path and are treated
differently:

| Sentinel | Meaning | Handling |
|---|---|---|
| `errGroupNotAttributable` | The grantee's bucket names no group | Skipped, `ReadGrantPending` warning event |
| `errBucketNotOwned` | The bucket does not carry the grantee's ownership tags | Skipped, `ReadGrantPending` warning event naming the bucket |
| `errAttributionLagging` | The provider has not made an attribution the operator wrote consistently visible | **Returned** — the grantor's pass fails and retries ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D8) |

Everything else — a failed `Get` of the grantee CR, any provider error — is returned as well and
wrapped by the caller as `resolve read grants: …`.

The reason `ReadGrantPending` carries **three different message shapes**, all from
`resolveReadGrants`. Anything that greps event messages needs all three forms:

| Situation | Message |
|---|---|
| Any ordinary pending grant (`pending()`) | `Bucket "<name>" referenced in spec.grantReadAccess <why>; read access not granted` |
| The grant names the Bucket itself | `ignoring self-reference "<name>" in spec.grantReadAccess` |
| The grantee carries a `DeletionTimestamp` | `Bucket "<name>" referenced in spec.grantReadAccess is being deleted; read access revoked` |

The third shares neither the `; read access not granted` suffix nor the self-reference wording, and
it is the one a revocation alert would actually want: it is raised while the grantee is still alive
behind its finalizer, which is the moment the reader drops out of the policy.

## The teardown pass

`reconcileDelete` → `teardown`. The order below is
[ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D3 as implemented.

| # | Step | Function |
|---|---|---|
| 0 | Skeleton mode: no provider call at all, finalizer dropped immediately | `reconcileDelete` |
| 0 | Breaker open: return `RequeueAfter`, keep the finalizer, no provider call | `reconcileDelete` ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D4) |
| 1 | Stop a running clone (Job + staging Secret), tolerating absence | `deleteCloneArtifacts` |
| 2 | Does the bucket still exist? | `stackit.Client.HasBucket` |
| 3 | Emptiness decision, or the authorized wipe | `prepareBucketForDelete` → `assertBucketEmpty` / `wipeBucket` |
| 4 | Delete the attributed group's access keys | `releaseWorkloadGroup` |
| 5 | Delete the attributed credentials group | `releaseWorkloadGroup` |
| 6 | Delete the bucket, if the tags prove it is ours | `deleteBucketIfOwned` |
| 7 | Delete the workload Secret (never the admin Secret) | `deleteSecret`, guarded by `isAdminSecret` |
| 8 | `Breaker.Success()`, then `dropFinalizer` | `reconcileDelete` |

The breaker row needs one clarification, because the order inside `reconcileDelete` is the opposite
of the intuitive one: the phase is flipped to `Deleting` with `status.message = "releasing StackIT
resources"` **before** the breaker is consulted, and that write is skipped only when the phase is
already `Deleting` or `Failed`. So the first teardown pass under an open breaker does write status;
from the second pass onward the phase is already `Deleting`, the write is skipped, and the hold is
churn-free. The breaker itself then only logs at V(1) and requeues on the cooldown.

**Why the emptiness check comes first.** Steps 4 and 5 destroy the workload's live credential. If
the emptiness check ran after them, a deletion blocked by a non-empty bucket — the normal, expected
outcome for a bucket in use — would have already taken the workload's access key away, and the
workload would be broken by a deletion that was then refused. Putting the decision first means a
blocked deletion leaves the workload **fully functional**: live key, unchanged Secret, unchanged
bucket. That is why the only step allowed to precede it is stopping a clone, which would otherwise
keep writing into the bucket during its own teardown.

The check itself is `S3Admin.BucketEmpty` in [`stackit/s3.go`](../../stackit/s3.go) (around line
612): a recursive `ListObjects` with `MaxKeys: 1` whose context is cancelled as soon as the first
entry arrives. It costs one bounded request, not a full listing pass — that matters because it runs
on every teardown attempt of a blocked deletion.

Two exceptions sit behind the ADR's phrase "every teardown that reaches the provider": the
emptiness decision is skipped entirely when the bucket no longer exists (step 2 returned false), and
the whole teardown is skipped in skeleton mode. Steps 4–6 are likewise all conditional on the bucket
existing — without the bucket there is nothing that can prove which group belongs to this `Bucket`,
so nothing may be deleted ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D4),
and `reportGroupNotAttributable` raises `CredentialsGroupNotAttributable` naming the group recorded
in status — but only when that group still exists, **or when the operator could not determine
whether it does**. A failed existence probe is deliberately reported as if the group existed,
because that is the safe direction for a warning; a probe that answers "gone" stays silent, which is
what keeps a second teardown pass after a finalizer-removal conflict from re-raising the event.

## Guards and their outcome class

Every failure in the pass goes through exactly one of two functions, and the choice is made by
**origin**, not by parsing the error
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D2).

| | `failNoRequeue` | `fail` |
|---|---|---|
| Meaning | Definitive: a statement the operator established locally about *this* Bucket | Non-definitive: something went wrong while talking to another system |
| `Ready` | Drops to `False` immediately | Held while `degrade` applies, then dropped |
| Returns | `ctrl.Result{}, nil` — no requeue | The error — retried on `bucketRateLimiter`'s backoff |
| Breaker | Untouched | `Breaker.Failure()`; while the breaker is open the error is logged and swallowed, and the result carries the breaker's cooldown instead |

There are exactly four `failNoRequeue` call sites in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go), and
they are the closed definitive list:

| Call site | Faults it covers |
|---|---|
| `reconcileNormal`, on `specGuardError` | Secret data-key collision; `secretRef` aimed at the admin Secret; foreign `spec.region` |
| `reconcileNormal`, on `ValidateBucketName` | A freshly composed name outside the DNS/length range |
| `failEnsureBucket`, on `*ownershipCollisionError` | A bucket of that name exists and is not this Bucket's |
| `provisionCredentialsAndClone`, on `validateCloneSource` | A clone source that is the destination itself |

Note the shape of `failEnsureBucket`: **only** an ownership collision is routed to the terminal
state. Every other error out of `ensureBucket` — a failed listing, a failed create, a failed tag
write, a timed-out `WaitBucketVisible` — is returned as a retryable failure and therefore enters the
hold. That is the intended default, because an unrecognised error is non-definitive by construction
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D3).

Two failures are definitive but established mid-pass rather than up front, and `holdsReadyThrough`
excludes them explicitly:

- `stackit.ProviderRefused(err)` — a **structured** 400/401/403. The discriminator is the shape of
  the response body, never the status code alone; see [stackit-api.md](stackit-api.md) and
  [provider-errors.md](provider-errors.md).
- `errCredentialDestroyed` — the workload's live key was deleted in this pass and the replacement
  could not be published. Local certainty that the Secret's credential is dead.

`holdForProvider` writes status only when the record would actually change: the first visit under
an open breaker, and the moment the degradation grace elapses (`providerHoldNeedsWrite`). A Bucket
already held, or already parked in `Failed`, is requeued silently — that is what keeps an outage
lasting hours from producing status churn
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D7).

## Writes to the object, and why there are so few

Exactly three non-status writes to a `Bucket` exist in the tree, which is
[ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D3 made checkable:

| Write | Where |
|---|---|
| `r.Update` after `controllerutil.AddFinalizer` | `Reconcile` |
| `r.Update` after `RemoveFinalizer` | `dropFinalizer` |
| `r.Update` after setting the resolved-name annotation | `persistResolvedName` — a no-op once the annotation already carries the name |

Everything else goes through `r.Status().Update` or `r.Status().Patch`, including the clone progress
writes in [`clone.go`](../../internal/controller/clone.go) and the measurement patch in
[`bucket_usage_controller.go`](../../internal/controller/bucket_usage_controller.go). If you add a
fourth non-status write, you are amending an ADR, not writing code.

## Watches and predicates

The hot-loop rule: **a controller that writes status must filter its own watch, or its writes wake
it again.** The `Bucket` controller writes status constantly — a clone poll every 15 seconds, a
degradation record, a terminal write per pass — so its own watch is predicated.

`SetupWithManager` registers four sources:

| Source | Predicate | Mapping | Purpose |
|---|---|---|---|
| `For(&Bucket{})` | `predicate.Or(GenerationChangedPredicate{}, AnnotationChangedPredicate{})` | — | Spec changes and annotation triggers only |
| `Watches(&corev1.Secret{})` | `predicate.NewPredicateFuncs(isManagedSecret)` | `bucketsForSecret` | A deleted or mutated credentials Secret re-mints the key |
| `Watches(&batchv1.Job{})` | `predicate.NewPredicateFuncs(isCloneJob)` | `bucketsForCloneJob` | A finished clone Job wakes its Bucket without waiting for the poll |
| `Watches(&s3v1.Bucket{})` | `granteeCredentialsPredicate` | `bucketsGrantingTo` | Grantees wake their grantors — see below |

Neither `GenerationChangedPredicate` nor `AnnotationChangedPredicate` overrides `CreateFunc` or
`DeleteFunc`, so create and delete events always pass. That is why **the informer's initial list at
process start reconciles every Bucket once** after a restart or an upgrade — the mechanism that
actually carries a new operator version onto untouched Buckets, as
[ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D8 notes.

It also decides how a Bucket **parked by a definitive fault** gets going again. Such a Bucket carries
no timer of its own — `failNoRequeue` returns `ctrl.Result{}, nil` — so every restart depends on an
event, and `GenerationChangedPredicate` passes every spec edit:

| Parked by | Repaired by | Therefore resumes on |
|---|---|---|
| `specGuardError` (Secret data-key collision, `secretRef` on the admin Secret, foreign `spec.region`) | editing the spec | the next spec edit — `metadata.generation` moves |
| `validateCloneSource` (the clone source is the destination) | editing `spec.cloneFrom` | the next spec edit |
| `ValidateBucketName` (composed name outside the DNS/length range) | the operator's naming policy — nothing the applier owns ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D5) | an operator restart or an annotation change |
| `*ownershipCollisionError` | removing or retagging the foreign bucket, outside Kubernetes | an operator restart or an annotation change |

The bottom two rows are the genuinely stuck ones, and the reason is the same in both: `spec.bucketName`
is immutable (CEL `self == oldSelf`) and a namespace cannot be renamed, so neither fault can be
argued away by changing the object. Independently of all of this, a managed Secret event, a clone-Job
event or a grantee event can enqueue a parked Bucket at any time — those watches carry no generation
filter.

`bucketsForSecret` and `bucketsGrantingTo` both list only the event object's **own namespace**, so a
Secret event never reaches a Bucket elsewhere and a same-named Bucket in another namespace is never
woken ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1/D3). Both filter in Go
rather than through a field index: the list is served from the controller's cache and a namespace
holds few Buckets.

The Secret watch is a self-wake by construction and it is worth knowing why it terminates.
`upsertSecret` stamps `app.kubernetes.io/managed-by: stackit-s3-provisioner` and a controller owner
reference, and the watch is gated on exactly that label — so the first write of a credentials Secret
enqueues its own Bucket. Because `upsertSecret` goes through `controllerutil.CreateOrUpdate`, the
steady-state pass writes nothing, so the wake happens once per Bucket rather than forever.

The measurement controller in
[`bucket_usage_controller.go`](../../internal/controller/bucket_usage_controller.go) uses the same
generation/annotation predicate for the same reason, and therefore takes its schedule from the
`RequeueAfter` each pass returns rather than from the watch — see
[usage-measurement.md](usage-measurement.md).

### The grantee watch

The second `Bucket` watch exists so a read grant is applied and revoked without waiting for the
drift resync. It cannot be an unfiltered watch: the main `Bucket` watch is generation/annotation
filtered precisely so status writes do not re-enter the loop, and a second watch on the same kind
would hand that loop straight back — clone progress alone writes status every 15 seconds. Worse, two
Buckets granting to each other would wake one another on every status write, forever.

`granteeCredentialsPredicate` therefore passes exactly four things:

| Event | Passes | Why |
|---|---|---|
| Create | always | The grantee appeared; the grant may now resolve |
| Delete | always | The grant must drop out of the policy |
| Update where `DeletionTimestamp` went from unset to set (or back) | yes | A deletion blocked by a finalizer is only ever an update, and `resolveReadGrants` revokes early for a grantee under deletion |
| Update where `status.credentialsGroupURN` changed | yes | The reader principal's identity changed |
| Generic, and every other update | no | Ordinary status churn |

The counterpart in the provisioning pass is step 11: the grantee publishes
`status.credentialsGroupURN` as soon as its group exists, not on the terminal `Ready` write. A
grantee that is itself still cloning never reaches that terminal write, so publishing late would
mean its grantors are never woken and wait for the drift resync instead
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D11).

`bucketsGrantingTo` skips the grantee itself when mapping, since a self-reference has nothing to
re-evaluate.

## Queue behaviour

`bucketRateLimiter` replaces controller-runtime's default, which starts the per-item backoff at 5ms
with a fleet-wide allowance of 10 per second — numbers meant for a reconcile that makes a few calls
against the local API server. This one calls a rate-limited remote API several times per pass.

| | Upstream default | This controller |
|---|---|---|
| Per-item backoff | 5ms → 1000s | 1s → 15m |
| Fleet-wide allowance | 10/s, burst 100 — see the caveat below | 1/s, burst 5 |

The upstream numbers above are `workqueue.DefaultTypedControllerRateLimiter`, which is what the
comment on `bucketRateLimiter` describes. Verified against
`sigs.k8s.io/controller-runtime@v0.25.0` (`pkg/controller/controller.go`, around line 271): that
default now applies only when the priority queue is **disabled**; with it enabled, which is the
default, the fallback is the 5ms → 1000s exponential limiter **alone**, with no fleet-wide
allowance. Either way this controller supplies its own limiter, so the behaviour of this operator is
the right-hand column; only the comparison drifted.

Only **error requeues** pass through the rate limiter. Watch events use `Add` and `RequeueAfter`
uses `AddAfter`, neither of which is rate limited — so the drift resync and every event-driven
reconcile keep their timing whatever the backoff is doing
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D9). The controller runs with the
default concurrency of one, so exactly one breaker probe runs per cooldown.

The numbers are not arbitrary. On mgmt-p on 2026-09-02 a provider-side 503 storm starting at
14:23 UTC turned into `429 rate limit on IP level exceeded` by 14:34: the operator's own retries had
become most of the load, and 242 reconcile errors were recorded for a single outage that resolved on
its own. The rate limiter, the breaker, and the HTTP layer no longer retrying `429`
([`stackit/retry.go`](../../stackit/retry.go)) are the three parts of the answer.

## Drift resync

A successful pass returns `ctrl.Result{RequeueAfter: r.DriftResyncInterval}`. `RequeueAfter <= 0`
means no requeue, so setting the interval to `0` restores purely event-driven behaviour.

| Setting | Value |
|---|---|
| Flag | `--drift-resync-interval` |
| Environment variable | `DRIFT_RESYNC_INTERVAL` |
| Helm value | `driftResyncInterval` |
| Default | `10m` (identical in [`cmd/main.go`](../../cmd/main.go) and [`values.yaml`](../../deploy/helm/stackit-s3-provisioner/values.yaml)) |

It must be a Go duration string **with a unit**; a bare number is rejected by `flag.DurationVar` and
crash-loops the operator at startup.

This is a `RequeueAfter` on the controller's own queue, not a manager-level `SyncPeriod`. The
distinction matters, because the manager's periodic re-delivery of unchanged objects is exactly what
the `Bucket` predicate discards.

**The defect this exists for.** Verified live on infra-d on 2026-07-22: a 1.6.0 → 1.9.0 rollout left
`test-bucket` on the old five-action isolation policy although the new version's
`BuildIsolationPolicy` produces eight. Nothing appeared in the container log, because the happy-path
policy write is not logged and the bucket was simply never re-reconciled. Three causes compounded:
the `Ready` path returned `ctrl.Result{}, nil`, so a steady-state bucket only reconciled on an
event; the watch predicate discarded controller-runtime's periodic resync, so an unchanged CR
produced no event; and with `replicaCount: 1` # default and Kubernetes' default `RollingUpdate`
strategy — the chart's [`deployment.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml)
sets no `strategy` block at all, so `maxUnavailable` is 25% of one replica rounded down to 0 and
`maxSurge` rounds up to 1 — the rollout brings the new pod up while the old one still runs, so
without leader election both reconciled and the old version's policy write could land last. The
policy code and `PoliciesEquivalent` were correct throughout — resetting the live policy by hand and
nudging the CR healed it. The gap was purely "no trigger after the upgrade".

The resync is the second half of the fix, and it is not the half that carries a new version onto
untouched Buckets — the informer's initial list already reconciles every Bucket once at process
start. The resync is what looks **again** afterwards: it heals drift introduced between restarts,
and it repairs a stale result left behind by an overlapping rollout
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D8).

## Leader election

The binary defaults to **off** (`--leader-elect`, `false` in
[`cmd/main.go`](../../cmd/main.go)); the chart turns it **on**
(`leaderElection.enabled: true` # default). The chart gates both the flag and the
`coordination.k8s.io/leases` RBAC rule on that value, so switching it off removes the permission
too.

| Setting | Value |
|---|---|
| Lease id | `stackit-s3-provisioner.stackit-bucket.gtrfc.com` |
| `LeaderElectionReleaseOnCancel` | `true` — the lease is released on graceful shutdown so the incoming pod becomes leader in seconds instead of waiting the lease out |
| `terminationGracePeriodSeconds` | `40` in the Deployment template, so SIGTERM-to-SIGKILL outlives the manager's 30s graceful shutdown and the lease is actually released |

It is on because of the rollout race described above: during a rolling update the new pod starts
before the old one terminates, and two versions reconciling concurrently means an older one can
re-apply a stale bucket policy after the newer one wrote the correct document. With both pods
running leader election the incoming pod waits for the lease, so there is no concurrent write.

The nuance worth keeping: leader election only serialises pods that **both** run it. The one rollout
that first flips `false` → `true` still has an outgoing pod that predates it and keeps reconciling
freely, so that single upgrade can still race once. The drift resync heals whatever it leaves
behind. Every rollout after that is fully serialised.

## What is wrong today

- **Bucket existence is a project listing, not a per-bucket read.** A bucket deleted out of band is
  indistinguishable from one that was never created, so the pass re-creates it and stamps fresh
  ownership tags rather than reporting that a provisioned bucket vanished. The consequences and the
  alternative belong to [bucket-identity.md](bucket-identity.md).
- **The admin credential is cached for the life of the process and never probed** (`r.admin`,
  guarded by `adminMu`). An admin S3 key deleted out of band is not noticed until the operator
  restarts; `ensureAdmin` re-bootstraps only when the Secret is absent or incomplete, never when the
  key it holds has stopped working
  ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9).
- **`markProvisioning` and `wipeBucket` swallow their status-write errors**, logging at V(1) and
  continuing. That is deliberate — the terminal write sets the authoritative state — but it means a
  long wipe or a slow provisioning pass can show a stale phase with nothing in the event stream
  saying why.
- **Not verified in this repo:** that `ensureClone`'s `deleteCloneArtifacts` on the failed-Job path
  cannot race a Job TTL controller deleting the same Job. The code logs and continues past the
  error, so the failure mode would be a log line rather than a stuck Bucket, but no test covers the
  race.
