# Bucket identity

Given a `Bucket` CR, which object in the STACKIT project is it, and how does the operator know that
object is its own? This page is the mechanism behind those two questions: how the physical bucket
name is composed and frozen, the four bucket tags that carry ownership and credentials-group
attribution, and the order in which a pass resolves them. If you want the operator-facing view —
which values to set, what a restore must reproduce — read
[../operations/bucket-naming.md](../operations/bucket-naming.md); if you want what the tags defend
against and where they do not reach, read
[../security/ownership-and-attribution.md](../security/ownership-and-attribution.md). The decisions
themselves live in [ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md)
(composition and freeze) and
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) (attribution); this
page never restates them, it explains the code that implements them.

## Where identity is decided

Everything on this page lives in three files. This table goes stale when the tree moves, and whoever
moves it updates this page in the same change.

| Symbol | File | Responsibility |
| --- | --- | --- |
| `BucketNaming`, `ComposeBucketName`, `Validate` | [api/v1/bucket_types.go](../../api/v1/bucket_types.go) | The operator-wide naming policy and the composition itself |
| `ValidateBucketName` | [api/v1/bucket_types.go](../../api/v1/bucket_types.go) | Length and DNS check applied to the *composed* name |
| `ResolvedBucketNameAnnotation` | [api/v1/bucket_types.go](../../api/v1/bucket_types.go) | `stackit-bucket.gtrfc.com/resolved-bucket-name` — the durable freeze |
| `Bucket.EffectiveBucketName` | [api/v1/bucket_types.go](../../api/v1/bucket_types.go) | The read-only accessor every consumer outside provisioning uses |
| `decideBucketName`, `persistResolvedName` | [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) | The four-case resolution order and the freeze write |
| `ownershipTags`, `ownerTagValue`, `ownershipName`, `isOwnedByUs` | [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) | The ownership tag set and the ownership predicate |
| `ensureBucket`, `adoptOrCollide`, `failEnsureBucket` | [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) | Create-or-adopt, collision detection, and how a collision is surfaced |
| `resolveWorkloadGroup`, `groupFromTags`, `groupFromPolicy`, `createWorkloadGroup`, `guardGroupCreate` | [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) | Credentials-group attribution through the bucket |
| `bucketOwnedByUs`, `deleteBucketIfOwned`, `releaseWorkloadGroup` | [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) | The teardown-side re-checks |
| `S3Admin.BucketTags`, `S3Admin.SetBucketTags` | [stackit/s3.go](../../stackit/s3.go) | The data-plane tag read/write, admin key only |
| `Client.HasBucket`, `Client.WaitBucketVisible` | [stackit/client.go](../../stackit/client.go) | Existence, decided from the project listing |
| `WorkloadPrincipalFromPolicy` | [stackit/s3.go](../../stackit/s3.go) | The policy fallback used by attribution |

## Name composition

`BucketNaming` is a two-field struct — `Prefix` and `IncludeNamespace` — built once in
[cmd/main.go](../../cmd/main.go) from flags or environment and handed to the reconciler as
`r.Naming`. No `Bucket` field feeds into it
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D2).

| Setting | Flag | Environment variable | Helm value | Value |
| --- | --- | --- | --- | --- |
| Prefix | `--bucket-name-prefix` | `BUCKET_NAME_PREFIX` | `bucketNaming.prefix` | `""` &nbsp;# default |
| Namespace part | `--bucket-name-include-namespace` | `BUCKET_NAME_INCLUDE_NAMESPACE` | `bucketNaming.includeNamespace` | `false` &nbsp;# default |

The chart renders each flag only when its value is truthy, so the Go defaults and the chart defaults
agree by construction; `--bucket-name-include-namespace` is rendered as a bare boolean flag, not as
`=false`. `ComposeBucketName` appends the non-empty parts in order — prefix, namespace,
`spec.bucketName` — and joins them with `-`. With both parts disabled it is the identity function,
which is why the default install is an exact continuation of the pre-feature behaviour. It does no
case folding, and the comment in the code says why: all three inputs are already lowercase by
independent validation (the prefix through `BucketNaming.Validate`, the namespace because it is a
DNS-1123 label, `spec.bucketName` through the CRD pattern).

`Validate` is called in `main` before the manager is constructed and exits the process on failure,
so a malformed prefix never reaches the provider
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D7). It accepts an
empty prefix and otherwise requires a lowercase DNS-1123 label of at most 63 characters —
`bucketNamePrefixRe`, which forbids dots, so the `-` join can never produce an invalid label
boundary.

`spec.bucketName` itself is pinned by four kubebuilder markers on the field: `MinLength=3`,
`MaxLength=63`, the DNS pattern `^[a-z0-9][a-z0-9.-]*[a-z0-9]$`, and
`XValidation:rule="self == oldSelf"` with the message `bucketName is immutable`. That last marker is
the mechanism behind the immutability the freeze relies on — a rejected update, enforced by the API
server, not by the reconciler.

## The freeze, and why the annotation is written first

`decideBucketName` is pure and does no I/O. It returns the name and a `fresh` bool that gates
validation:

| Order | Source | `fresh` | When it applies |
| --- | --- | --- | --- |
| 1 | `status.resolvedBucketName` | `false` | Normal steady state |
| 2 | `ResolvedBucketNameAnnotation` | `false` | Status lost — CR restored from backup, status subresource wiped |
| 3 | `spec.bucketName`, because `status.bucketURL` is set | `false` | Provisioned before this feature existed: a bucket URL but no frozen name |
| 4 | `naming.ComposeBucketName(b)` | `true` | First provisioning of this CR |

Only case 4 is validated, by `ValidateBucketName`; an already-frozen name is used exactly as
recorded and never re-checked
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3/D5). A composed
name out of range is routed to `failNoRequeue`, which writes the failure and returns an empty
`ctrl.Result` with a **nil** error. The drift-resync requeue
(`ctrl.Result{RequeueAfter: r.DriftResyncInterval}`, `--drift-resync-interval`, `10m` # default) is
attached only to the successful return after the Ready status write, so a Bucket parked on an
out-of-range name carries no timer of its own. It resumes on three triggers: the operator restarts,
an annotation on the CR changes, or any spec edit that bumps `metadata.generation` — the `For`
predicate in `SetupWithManager` is `predicate.Or(GenerationChangedPredicate{},
AnnotationChangedPredicate{})`, so a generation bump re-queues a parked object like any other.
Parking is deliberate, not an oversight: none of the three repairs it, because `spec.bucketName` is
immutable and a namespace cannot be renamed, so the correction belongs to the operator's naming
policy. Note the disagreement:
[ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D5 names only the
restart and the annotation change; the third trigger is in the code and is stated here because the
code is what runs.

`persistResolvedName` writes the annotation with a full `r.Update` **before** `ensureAdmin`,
`EnsureService` or `ensureBucket` run — that is, before any cloud object can exist. It is a no-op
when the annotation already carries the name, so a steady-state pass issues no write. `reconcileNormal`
writes `status.resolvedBucketName` only at the end, in the same status update that sets `Ready`.

The ordering is the point. A crash anywhere between "bucket created" and "status written" leaves the
annotation behind, and case 2 picks it up — the operator can never recompose a name for a bucket that
already exists, not even after an administrator changes the prefix. The annotation is one of exactly
two pieces of `Bucket` metadata the operator writes at all
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D3); the other is the
finalizer, and the rotation annotation is read but never written.

`EffectiveBucketName` is the read-only twin used everywhere outside the provisioning path: status,
then annotation, then raw `spec.bucketName`. It lacks case 3, which does not change any outcome,
because case 3 and the final fallback both yield `spec.bucketName`. Its callers are the Secret
contents (`SecretData`), the teardown entry point, the grantee lookup in `resolveReadGrants`, and the
log lines around teardown and the circuit hold. It is the only accessor any new consumer should use;
reading `spec.bucketName` directly is a bug wherever a naming policy could be configured.

## The four bucket tags

STACKIT Object Storage has no native resource tags, so every identity record rides on **S3 bucket
tagging**, written and read with the admin credential through `S3Admin.SetBucketTags` /
`S3Admin.BucketTags` ([credentials.md](credentials.md) covers where that credential comes from).
`BucketTags` maps the provider's `NoSuchTagSet` or a 404 onto an empty map rather than an error, so
"untagged" and "tagged" are one uniform code path. The four keys and the value each one carries are
tabulated in the [naming conventions](../../README.md#naming); what follows is how they are written,
read and compared.

`SetBucketTags` **replaces** the whole tag set — it does not merge — and the two steps that write
tags therefore behave differently on purpose. `ensureBucket` on create and `adoptOrCollide` on
adoption pass `r.ownershipTags(b)`, a freshly built two-key map of `managed-by` (from
`ownershipName()`) and `owner` (from `ownerTagValue`), which is correct because at that moment no
other tag of ours may exist. The `stamp` closure in `resolveWorkloadGroup` starts from the map
`BucketTags` just returned, adds `credentials-group-id` and `credentials-group-urn` and writes the
result, so it preserves the ownership tags and anything else on the bucket. The four keys are one
tag set written by two steps, never two independent sets.

`ownerTagValue` is `b.Namespace + "/" + b.Name` and deliberately excludes `metadata.uid`. A UID is
reassigned when a CR is re-created, so a disaster-recovery replay from Git would otherwise read every
one of its own buckets as foreign
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D6). This is held
closed by `TestOwnerTagValue_StableAcrossUID`.

`ownershipName()` falls back to the constant `defaultOwnershipName` when `OwnershipName` is empty,
which is what lets a test construct a reconciler directly; production always sets it from
`--ownership-name` / `OWNERSHIP_NAME` / `ownership.name`. **The trap:** this value is half the
ownership key. Change it while buckets exist and `isOwnedByUs` returns false for every one of them —
the operator reads its own buckets as foreign and parks each Bucket on a collision until they are
re-tagged by hand. A restore into a fresh cluster must therefore reproduce the same value, and two
installations sharing one STACKIT project must use *distinct* values or they will fight over the same
buckets. The warning lives in
[deploy/helm/stackit-s3-provisioner/values.yaml](../../deploy/helm/stackit-s3-provisioner/values.yaml)
next to the value and in the flag's own usage string.

Not verified, and this is the gap: the operator does **not** validate `ownership.name`. The
values.yaml comment states the allowed character set as that of S3 tag values (letters, digits,
`+ - = . _ : / @` and spaces), but no code enforces it — an out-of-range value would first fail at
tag-write time against the provider, and that failure mode was not exercised here.

## Collision detection

`ensureBucket` decides existence with `Client.HasBucket` and then branches. A fresh create is followed
by `WaitBucketVisible` (`bucketVisibleTimeout`, `60s`) before the tags are stamped, because creation
is eventually consistent. A `409` from `CreateBucket` is treated as a create race, not an error: the
code waits for visibility and then falls through to `adoptOrCollide` rather than blindly stamping its
own tags over whatever appeared.

`adoptOrCollide` reads the tag set and splits on emptiness, which is the distinction worth
understanding:

- **No tags at all.** The bucket is claimed only when `BucketEmpty` says it holds nothing. That case
  exists for exactly one reason: a crash between `CreateBucket` and the tag write leaves this state,
  and without adoption the operator could neither adopt nor delete the bucket it had just made. A
  non-empty untagged bucket is `ownershipCollisionError` with the detail
  `pre-existing non-empty bucket carries no ownership tags`.
- **Tags present.** `isOwnedByUs` requires **both** `managed-by` and `owner` to match. One out of two
  is not ownership. A mismatch is `ownershipCollisionError` carrying the observed `managed-by` and
  `owner` values in its message, so the operator can see whose bucket it is.

`failEnsureBucket` is the router: an `ownershipCollisionError` (matched with `errors.As`) raises a
warning event and goes to `failNoRequeue`; every other error from `ensureBucket` goes to `fail` and is
therefore eligible for the degraded hold described in
[provider-errors.md](provider-errors.md). That split is the whole reason the collision error is a
typed error rather than a formatted string.

The three offline cases are held by `TestReconcileOwnershipCollision` in
[internal/controller/reconciler_fake_test.go](../../internal/controller/reconciler_fake_test.go):
foreign tags park the Bucket as `Failed` with `not owned` in the message, an untagged empty bucket is
adopted and stamped with `owner=team-a/app-data`, an untagged non-empty bucket is refused.

## Credentials-group attribution

A credentials group carries no owner field of its own and its display name is not unique in the
project, so the attribution lives on the bucket instead
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1–D3).
`resolveWorkloadGroup` is the single entry point, used by provisioning (`create=true`), by read-grant
resolution and by teardown (both `create=false`).

It starts by re-reading the bucket tags and re-applying `isOwnedByUs`. That check is **not** skipped
because `ensureBucket` already ran: `resolveWorkloadGroup` is also called on *another* CR's bucket
during grant resolution, and returning `errBucketNotOwned` after that single ownership read, and
before any tag write or group resolution, is what stops a foreign bucket both from lending its group
and from receiving one of our tags
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D6). The ownership
decision is made *from* a tag read, so one read necessarily precedes it; that same map is the one
`stamp` later mutates, which is why the ownership check and the tag write are a single read apart and
no second read can slip between them.

Then, in order:

1. **`groupFromTags`.** Reads `credentials-group-id`; empty means "no tag". Existence is probed by
   `groupExists`, which calls `ListAccessKeyIDs` and reads **only** a 404 as gone — every other
   failure is returned, so a transient error never looks like a deleted group. The keys endpoint is
   used rather than the project listing because it answers for a group the instant it is created. If
   `credentials-group-urn` is present, the pair is returned and no listing is consulted at all.
   Otherwise the URN is recovered from `groupIndex.byID` and stamped back onto the bucket, so the
   next pass needs no listing and the recovery cannot recur for that bucket. A tag naming a group
   that no longer exists reads as *no tag* and falls through.
2. **`groupFromPolicy`.** The migration path for buckets tagged before this scheme existed:
   `WorkloadPrincipalFromPolicy` extracts the single principal of the `workload-objects-only`
   statement from the bucket's own isolation policy, that URN is looked up in `groupIndex.byURN`, the
   tags are written, and the event `CredentialsGroupAttributed` is raised. A failure to *read* the
   policy is returned as an error and ends the pass — it is never treated as "no policy", because
   that would mint a second group for a bucket that has one and rotate a working workload out of its
   credential. `WorkloadPrincipalFromPolicy` accepts a bare string or a one-element list (StorageGRID
   may normalise the form on read-back) and reports `ok=false` for anything that does not name
   exactly one principal, so nothing is ever attributed on a guess. The statement ids in
   [stackit/s3.go](../../stackit/s3.go) are load-bearing for this lookup, not cosmetic — see
   [bucket-policy.md](bucket-policy.md).
3. **`createWorkloadGroup`**, only with `create=true` and only past `guardGroupCreate`. The group is
   created, then `stamp` writes both tags. If the tag write fails, the group is deleted again:
   without the tags it is unreachable for every later pass, and a retry must not leave a trail of
   empty groups. A crash *between* the two calls still leaves one behind — it holds no key and is
   findable by its display name, which is what `workloadGroupName` is for.

`workloadGroupName` — `s3op-<namespace>-<name>` truncated to fit `maxGroupNameLen` (32) minus an
8-hex FNV-1a-32 suffix of `<namespace>/<name>` — produces a **label, not an identity**
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D7). Distinct
namespace/name pairs can collide under it, and the control plane does not enforce unique display
names. It is never used to find, adopt or delete a workload group; the only identity still resolved
by display name is the shared `operator-admin` group
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md), mechanism in
[credentials.md](credentials.md)). Why it is kept exactly as it is, collisions and all: existing
groups keep the names they were created with, so the 2026-09-03 re-attribution renamed nothing.

### Two guards against a provider that answers stale

The provider may answer a read with state older than the operator's own last write. Two named
sentinel errors absorb that
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D8):

- **`errAttributionLagging` in `groupFromTags`**, message `group <id> exists but is not listed yet`.
  It arises on one precise combination: the id tag is present, `groupExists` confirms the group, the
  URN tag is absent, and the group is not in the `groupIndex` this pass built. Because `stamp`
  always writes both tags together, only a bucket tagged before `credentials-group-urn` existed can
  reach it — and once the URN is recovered and stamped back, the condition cannot recur for that
  bucket. The pass retries; nothing is created meanwhile.
- **`errAttributionLagging` from `guardGroupCreate`**, before step 3 creates anything. When
  `status.credentialsGroupID` records a group and `groupExists` says it is still there, a bucket
  showing no attribution is a stale read, not an unattributed bucket. A bucket created in this very
  pass (`freshBucket`) cannot have a group and skips the guard entirely. The recorded status id
  decides **only whether to wait** — it never names the group the bucket is bound to, which is why a
  forged status can at most delay the forger's own Bucket.

`groupIndex` is one `ListCredentialsGroups` call per pass, indexed by id and by URN, so a reconcile
resolving a bucket plus several grantees costs one listing rather than one per group.

## Identity on the other paths

**Read grants.** `resolveReadGrants` resolves each entry in the grantor's own namespace to a Bucket
CR, takes that CR's `EffectiveBucketName()`, confirms the bucket exists, and applies
`resolveWorkloadGroup` to it with `create=false`. The reader URN is therefore never read off the
grantee's `status.credentialsGroupURN` — status is writable by anyone holding `buckets/status update`
in the namespace, a strictly weaker permission than reading the Secret, and a forged URN would be
pasted verbatim into this bucket's policy
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3).
`errGroupNotAttributable` and `errBucketNotOwned` are both downgraded to the `ReadGrantPending`
event and the entry is skipped; every other error, `errAttributionLagging` included, fails the
grantor's pass and is retried. The policy side of this is [bucket-policy.md](bucket-policy.md).

**Teardown.** `teardown` takes `b.EffectiveBucketName()` once and uses it for every step. The
pass runs `deleteCloneArtifacts`, then `Client.HasBucket`, then `prepareBucketForDelete` (the
wipe-or-empty guard), and only then — under `if bucketExists` — `releaseWorkloadGroup`. The
project-wide credentials-group listing lives *inside* `releaseWorkloadGroup`, so it happens only
when the bucket still exists and only after the empty guard has passed; a teardown of a bucket that
is already gone performs no group listing at all and goes straight to `reportGroupNotAttributable`.
When the listing does run it is consumed only as the by-id/by-URN index for the policy fallback and
for buckets carrying an id tag without a URN tag. `releaseWorkloadGroup` calls
`resolveWorkloadGroup` with `create=false`: a bucket that attributes no group, a bucket that is not
this CR's, or an absent bucket releases nothing, and `reportGroupNotAttributable` raises the
`CredentialsGroupNotAttributable` event naming `status.credentialsGroupID` only when that recorded
group still exists. `deleteBucketIfOwned` re-reads the tags through `bucketOwnedByUs` and refuses to
delete an untagged or foreign bucket even after the empty-check passed
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D7). That is two independent
tag reads in a normal teardown pass — one in `resolveWorkloadGroup`, one in `bucketOwnedByUs` — and
three when an authorized wipe runs first, because `prepareBucketForDelete` calls `bucketOwnedByUs`
to re-prove ownership before wiping. The third read is off the default path: the wipe gate
(`--enable-wipe-on-delete`, Helm `wipeOnDelete.enabled`, `false` # default) must be on as well
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D4). The repetition is
intentional: each operation proves ownership for itself.

**Size measurement.** `BucketUsageReconciler` addresses the bucket by `b.Status.ResolvedBucketName`
directly, not through `EffectiveBucketName()`, and skips a Bucket whose status field is empty or
that is not `Ready`. Since `reconcileNormal` writes that field on every successful pass — including
for a pre-feature bucket, where it records the raw `spec.bucketName` — the practical effect is that
measurement starts only after the first successful reconcile under this operator version. It does
not re-check the ownership tags; see
[../security/ownership-and-attribution.md](../security/ownership-and-attribution.md) and
[usage-measurement.md](usage-measurement.md).

## Invariants

| Invariant | Enforced by | What breaks if you change it |
| --- | --- | --- |
| The annotation is written before any cloud object exists | `persistResolvedName` called before `ensureAdmin` in `reconcileNormal` | A crash mid-pass orphans a bucket under a name nothing references |
| A frozen name is never re-validated or recomposed | `decideBucketName` returns `fresh=false` for cases 1–3 | A prefix change re-maps live buckets and mints fresh credentials into workloads' Secrets |
| Both ownership tags must match | `isOwnedByUs` | A partial match would let another CR, or another installation, adopt the bucket |
| The ownership check precedes every tag write and every group resolution | `resolveWorkloadGroup` decides ownership from its one tag read, before `groupFromTags`, `groupFromPolicy` or `stamp` run | A foreign bucket could lend its group or receive our tag |
| A workload group is never located by display name | `groupFromTags` / `groupFromPolicy`; `workloadGroupName` has no lookup caller | Name collisions become credential theft — this was the 2026-09-03 finding |
| A failed policy *read* is an error, not an absence | `groupFromPolicy` returns the error | A second group is minted and a working workload is rotated out of its credential |
| `status` is never a source of identity or deletion | `guardGroupCreate` uses the recorded id only as a wait/no-wait signal | A namespace user with `buckets/status update` could redirect deletion or a policy principal |
| A new consumer of the bucket name calls `EffectiveBucketName()` | Convention only — nothing enforces it | Reading `spec.bucketName` addresses the wrong bucket under any non-default naming policy |

## What holds this closed

<details>
<summary>Offline cases (fake STACKIT API, no network)</summary>

In [internal/controller/reconciler_attribution_test.go](../../internal/controller/reconciler_attribution_test.go)
unless noted otherwise:

| Test | What it pins |
| --- | --- |
| `TestGroupAttributionSurvivesNameCollision` | Two CRs whose derived display names collide keep separate groups |
| `TestGroupAttributionMigratesLegacyBucket` | A bucket with no attribution tags is migrated from its own policy and tagged |
| `TestGroupAttributionSurvivesRestoreWithoutStatus` | A CR restored without status re-attaches to the surviving group |
| `TestGroupAttributionReplacesDeletedGroup` | A tag naming a vanished group reads as no tag |
| `TestGroupAttributionRollsBackUntaggedGroup` | A failed tag write deletes the group it just created |
| `TestGroupAttributionPolicyReadFailureCreatesNothing` | An unreadable policy creates nothing |
| `TestGroupAttributionSurvivesListingLag` | A group omitted from the listing does not disturb a tagged bucket |
| `TestGroupAttributionWaitsForStaleBucketRead` | A stale bucket read waits instead of creating a second group |
| `TestGroupAttributionRecreatesWhenRecordedGroupGone` | A 404 on the recorded group lets the create proceed |
| `TestGroupAttributionFreshBucketSkipsGuard` | A bucket created in this pass skips `guardGroupCreate` |
| `TestTeardownAttributesGroupViaPolicy` | Teardown uses the policy fallback |
| `TestTeardownLeavesUnattributableGroup` | An unattributable group survives teardown, with the event |
| `TestTeardownForeignBucketReleasesNoGroup` | A foreign bucket releases nothing |
| `TestTeardownSurvivesListingLag` | Teardown needs no listing for a fully tagged bucket |
| `TestTeardownSecondPassIsSilent` | The requeued second teardown pass raises no event |
| `TestReadGrantResolvesLegacyGrantee` | A grantee on the policy fallback still resolves |
| `TestReadGrantRefusedForForeignGranteeBucket` | A grantee whose bucket is not its own is skipped |
| `TestReadGrantSurvivesListingLag` | A grant resolves from the grantee's tags without the listing |
| `TestDecideBucketName`, `TestPersistResolvedName`, `TestWorkloadGroupName*` ([bucket_controller_test.go](../../internal/controller/bucket_controller_test.go)) | The four-case order, the idempotent freeze write, the display-name budget |
| `TestOwnerTagValue_StableAcrossUID`, `TestOwnershipName_DefaultsWhenUnset`, `TestOwnershipTags`, `TestIsOwnedByUs`, `TestOwnershipCollisionError` ([ownership_test.go](../../internal/controller/ownership_test.go)) | The tag values and the ownership predicate |
| `TestReconcileNamingPolicy`, `TestReconcileOwnershipCollision` ([reconciler_fake_test.go](../../internal/controller/reconciler_fake_test.go)) | Composition end-to-end, the freeze against a policy change, the three collision cases |

</details>

Against the real STACKIT API, `TestIntegrationGroupAttributionMigration` in
[internal/controller/attribution_integration_test.go](../../internal/controller/attribution_integration_test.go)
(build tag `integration`) seeds a bucket the pre-2026-09-03 way — ownership tags, a group found by
display name, a key, a policy and a Secret — and then runs the production reconciler over it in three
acts: the upgrade, which must change nothing but add the two attribution tags and raise
`CredentialsGroupAttributed`; a restore in which the tags are removed again and the CR re-created
without status, which must re-attach to the surviving group; and a teardown, which must release
exactly the seeded bucket, group and keys. The assertions compare bucket name, group id, key ids,
Secret bytes and the full expected four-key tag map. The 2026-09-03 verification run recorded a
duration of 123 s and one transient `connection reset by peer` on the bucket delete, retried the way
the finalizer retries it; those two observations are from that run's log and were not re-measured for
this page.

## What is wrong today

**Existence is decided by a project-wide listing.** `Client.HasBucket` calls `ListBucketNames` and
scans for the name; `WaitBucketVisible` polls the same call every two seconds. There is no per-bucket
read anywhere in the identity path. Two consequences follow. First, a successful listing that does
not contain the name returns `(false, nil)`, and the next statement creates the bucket — so "the
bucket was deleted behind our back" and "the bucket was never created" are literally the same input
to `ensureBucket`, and a bucket a provisioned Bucket still points at, deleted out of band, is
silently re-created empty rather than reported. The degradation path is reached only when the
*listing call itself* fails. Second, the cost of every existence check scales with the number of
buckets in the project, and a single teardown pass performs one listing for the bucket, plus one
credentials-group listing whenever the bucket still exists.

**A stripped annotation orphans a bucket.** The freeze is durable against a lost status, not against
a lost annotation. If `status.resolvedBucketName` is gone *and* the annotation has been removed — both
are writable by anyone who may write the `Bucket` — `decideBucketName` falls to case 3 or case 4, and
under a non-default naming policy the operator will address a name that does not exist. Verified in
the code, not observed in a cluster. What contains the reverse direction — an annotation set *before*
first provisioning, to point at somebody else's bucket — is the ownership check, not the annotation
itself, and its residual case is an empty untagged bucket, which is adopted by design.

**`ownership.name` is unvalidated**, as noted above.

**Not verified here:** that two installations sharing one STACKIT project actually collide at the
defaults. It follows from `isOwnedByUs` comparing two values that are both installation-wide
defaults, but no two-cluster measurement was taken.

## Related

* [ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — composition, the
  freeze, and the alternatives that were rejected
* [ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) — attribution
  through the bucket
* [ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D2 — the cloud-side attribution
  rule the ownership tags implement
* [reconcile-pipeline.md](reconcile-pipeline.md) — where the steps on this page sit in the pass
* [bucket-policy.md](bucket-policy.md) — the policy document the attribution fallback reads
* [credentials.md](credentials.md) — the admin credential every tag read and write depends on
* [provider-errors.md](provider-errors.md) — `fail` versus `failNoRequeue` and the degraded hold
* [../operations/bucket-naming.md](../operations/bucket-naming.md) — the operator-facing settings and
  the restore checklist
* [../security/ownership-and-attribution.md](../security/ownership-and-attribution.md) — what these
  records defend against, and the open gaps
