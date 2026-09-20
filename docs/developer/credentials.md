# Credentials

This page is about the two S3 credentials this operator handles in code: the **workload
credential** a `Bucket` publishes into a Kubernetes Secret, and the **operator admin
credential** that every data-plane call on a bucket this operator provisions is made with
(reading a clone source is the one data-plane operation that is not — see *Who uses which
credential* below and
[ADR 0004 D2](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)). It
documents the data-key resolver,
the Secret write, the key-replacement path, the rotation bookkeeping and the admin
bootstrap — the mechanisms, not the decisions. The decisions are
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
(workload credential) and
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) (admin
credential). If you wanted *who may read a Secret and what its loss costs*, that is
[docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md); if you
wanted *how to rotate one in a running cluster*, that is
[docs/operations/credentials.md](../operations/credentials.md); the position of the
credential step inside a reconcile pass is in
[reconcile-pipeline.md](reconcile-pipeline.md).

This page names files and functions on purpose. It goes stale when the tree moves, and
whoever moves the tree updates it in the same change.

## The Secret data contract

Six logical fields are written into the workload Secret. Each has a default data-key name
and each is individually overridable under `spec.secretRef.keys`. The whole contract lives
in [api/v1/bucket_types.go](../../api/v1/bucket_types.go) — types, constants, resolver and
validator — and is unit-tested in
[api/v1/bucket_types_test.go](../../api/v1/bucket_types_test.go).

The **published** default names are documented once, for the people who consume the Secret,
in [README.md → Naming conventions](../../README.md#naming); do not copy that
table here or anywhere else. What a developer needs is which method resolves which field:

| Logical field | Override path | Resolver |
|---|---|---|
| `accessKeyID` | `spec.secretRef.keys.accessKeyID` | `SecretKeys.AccessKeyIDKey()` |
| `secretAccessKey` | `spec.secretRef.keys.secretAccessKey` | `SecretKeys.SecretAccessKeyKey()` |
| `bucketName` | `spec.secretRef.keys.bucketName` | `SecretKeys.BucketNameKey()` |
| `region` | `spec.secretRef.keys.region` | `SecretKeys.RegionKey()` |
| `endpoint` | `spec.secretRef.keys.endpoint` | `SecretKeys.EndpointKey()` |
| `bucketURL` | `spec.secretRef.keys.bucketURL` | `SecretKeys.BucketURLKey()` |

Three pieces of code implement the contract, and nothing else in the tree may resolve a
data key by hand:

**The resolver.** Every `SecretKeys.<field>Key()` method is one line over `orDefault`: a
non-empty override wins, an empty field falls back to the `Default*Key` constant. There is
no defaulting in the CRD schema, so an unset field is genuinely empty at reconcile time and
the resolution happens in Go, every pass.

**The data-map builder.** `Bucket.SecretData(SecretValues)` builds the
`map[string][]byte`. Credentials, bucket name and region are always written; `endpoint` and
`bucketURL` are written **only when the supplied value is non-empty**, which is why a
consumer must treat those two keys as optional. The bucket name written is
`EffectiveBucketName()` — the frozen physical name, not `spec.bucketName` (see
[bucket-identity.md](bucket-identity.md) and
[ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md)) — and the
region is `GetRegion()`, which defaults to the operator's region. The values that only the
operator knows arrive in the `SecretValues` struct; bucket name and region are deliberately
not part of it, because they come from the CR.

**The collision validator.** `Bucket.ValidateSecretKeys()` walks all six logical fields —
including `endpoint` and `bucketURL`, whether or not they will be populated this pass — and
returns an error naming both fields and the shared key if two resolve to the same data key.
Two fields writing the same map entry would silently discard one value, which is the reason
[ADR 0007 D10](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
makes it a refusal rather than a warning.

**Verified: the validator really runs before any Secret write.** It is the first check in
`specGuardError` ([internal/controller/bucket_controller.go:413](../../internal/controller/bucket_controller.go#L413)),
which is the first thing `reconcileNormal` does after taking its logger
([:328](../../internal/controller/bucket_controller.go#L328)) and returns through
`failNoRequeue`, so a colliding `Bucket` is parked as `Failed` before the admin bootstrap,
before any provider call and long before `upsertSecret`. There is **no** CEL rule for this
on the CRD — the only `XValidation` markers in
[api/v1/bucket_types.go](../../api/v1/bucket_types.go) are the `bucketName` immutability
rule and the self-grant rejection — so the reconciler guard is the whole enforcement. A
colliding `Bucket` is accepted by the API server and fails on reconcile.

## Where the default key names live

The `Default*Key` constants in
[api/v1/bucket_types.go:188-202](../../api/v1/bucket_types.go#L188-L202) are the single
source, and the table in
[README.md → Naming conventions](../../README.md#naming) is derived from them.
If you change a constant you change the published contract of every `Bucket` that did not
override that field, so change it in the same commit as the README table and as
[docs/operations/credentials.md](../operations/credentials.md).

The same constants are reused for the clone-source Secret: `CloneSourceSecretKeys` has only
`accessKeyID` and `secretAccessKey` and falls back to the same two `AWS_*` names, which is
what makes a Secret this operator wrote for one `Bucket` usable as another `Bucket`'s clone
source without any key configuration. See [clone.md](clone.md).

## Where the Secret lives, and why it cannot be aimed elsewhere

`SecretReference` ([api/v1/bucket_types.go:465](../../api/v1/bucket_types.go#L465)) carries
a `name` and the key overrides — and no namespace. Every call site derives the location from
the CR: `ensureAccessKeyAndSecret` builds
`types.NamespacedName{Name: b.Spec.SecretRef.Name, Namespace: b.Namespace}`, `upsertSecret`
sets `sec.Namespace = b.Namespace`, `deleteSecret` does the same, and `bucketsForSecret`
lists `Bucket`s only in the Secret's own namespace. That is
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1/D4 as implemented.

What actually stops a leftover `spec.secretRef.namespace` in an old manifest from having an
effect is the CRD schema: the generated
[config/crd/bases/stackit-bucket.gtrfc.com_buckets.yaml](../../config/crd/bases/stackit-bucket.gtrfc.com_buckets.yaml)
declares `secretRef` with exactly `name` and `keys` and the file contains no
`x-kubernetes-preserve-unknown-fields` anywhere, so the API server prunes the field on
write. It is **not** the generated deep-copy code: `SecretReference.DeepCopyInto` in
[api/v1/zz_generated.deepcopy.go](../../api/v1/zz_generated.deepcopy.go) is a plain struct
copy and would compile unchanged with an extra scalar field.

The migration path for a `Bucket` that carried the old field is the ordinary replacement
path below: the field is pruned, the Secret in the CR's own namespace carries no
credentials, so the operator clears the attributed group's keys, mints a new one and upserts
the Secret in `b.Namespace`
([ADR 0007 D4/D5](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)).
The Secret in the foreign namespace is left behind holding a dead credential.

## The write path

`upsertSecret` ([:1489](../../internal/controller/bucket_controller.go#L1489)) is a
`controllerutil.CreateOrUpdate` whose mutate function does four things: stamp the
`app.kubernetes.io/managed-by: stackit-s3-provisioner` label, force
`type: Opaque`, **merge** the provisioned data keys into `sec.Data` (unrelated entries are
left alone), and call `controllerutil.SetControllerReference(b, sec, r.Scheme)`.

The owner reference is what decides the three cases, and the behaviour comes from
controller-runtime, not from this repo:

| State of the named Secret | Outcome |
|---|---|
| does not exist | created, labelled, controller-owned by the `Bucket` |
| exists, no controller owner reference | **adopted** — the owner reference is added and the colliding data keys are overwritten |
| exists, controlled by something else | `SetControllerReference` returns `*controllerutil.AlreadyOwnedError`, `CreateOrUpdate` aborts before any write, and the reconcile fails |

The adoption case is the concrete mechanism behind the confused-deputy argument on
[docs/security/rbac-and-privilege.md](../security/rbac-and-privilege.md): creating a
`Bucket` is enough to have the operator write into, and later delete, an unowned Secret of
that namespace on the creator's behalf, with no check that the creator may read or write it
themselves. That is why `Bucket` write access is only ever shipped aggregated into the
built-in `edit`/`admin` roles, which already carry Secret access in the same namespace.

**Deletion.** `deleteSecret` ([:1535](../../internal/controller/bucket_controller.go#L1535))
issues a plain `Delete` on `b.Spec.SecretRef.Name` in `b.Namespace`, tolerating `NotFound`.
It does not re-check ownership or the label, so an adopted Secret is deleted with the CR
exactly like one the operator created. It is called last in `teardown`
([:1192](../../internal/controller/bucket_controller.go#L1192)), and only after
`isAdminSecret` ([:1246](../../internal/controller/bucket_controller.go#L1246)) has had the
chance to return early — defence in depth against a `Bucket` in the operator's own namespace
naming the admin Secret, which `specGuardError` already refuses to provision
([ADR 0004 D8](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)). Because
the controller owner reference is also set, an ordinary CR deletion would garbage-collect
the Secret anyway; the explicit delete is what makes the removal happen at teardown time
rather than whenever the garbage collector gets there.

**Watching the Secret back to its Bucket.** The controller watches `Secret`s filtered by
`isManagedSecret` ([:1865](../../internal/controller/bucket_controller.go#L1865)) — the
managed-by label — and maps them with `bucketsForSecret`
([:1873](../../internal/controller/bucket_controller.go#L1873)), which lists `Bucket`s in
the Secret's namespace and enqueues those whose `spec.secretRef.name` matches. A deleted or
edited credentials Secret therefore triggers a reconcile within a watch event rather than
waiting for the drift resync. Note the consequence of the label filter: a Secret the
operator has never written carries no label and is not watched, so adopting a pre-existing
Secret only becomes observable after the first successful write.

## The key replacement path

`ensureAccessKeyAndSecret` ([:998](../../internal/controller/bucket_controller.go#L998))
implements [ADR 0007 D4-D6](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
in one function. The order of statements is the whole safety argument, so read it as a
sequence:

1. `Get` the Secret in the `Bucket`'s namespace. A `NotFound` is carried forward as a flag;
   any other error aborts before a single provider call.
2. `ListAccessKeyIDs(groupID)` — one control-plane call, on every pass that reaches this
   function. This is the "does the group still back the credential" half of D4 and it
   cannot be answered from cluster state.
3. **The skip condition.** All four must hold: the `Get` succeeded, `secretHasCreds`
   ([:1995](../../internal/controller/bucket_controller.go#L1995)) finds both credential
   values non-empty under *this* `Bucket`'s resolved key names, the listing returned at
   least one key, and `b.PendingRotationTrigger()` is empty. Then the function returns
   `secretAccessKeyID(&sec, b)` and changes nothing.
4. **Clear.** `DeleteAllAccessKeys(groupID)`
   ([stackit/client.go:341](../../stackit/client.go#L341)) deletes every key in the group.
   It lists first and tolerates a 404 on the group and on each individual key, so a group
   already emptied out of band is treated as drained.
5. **Create.** `CreateAccessKey(groupID)`
   ([stackit/client.go:273](../../stackit/client.go#L273)) with an explicitly constructed
   empty payload (`*objectstorage.NewCreateAccessKeyPayload()`) — the request fails if the
   payload is omitted entirely, and the builder takes a value type, so there is no nil to
   pass in the first place. An empty payload is also what leaves the key's expiry unset
   ([ADR 0007 D3](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)).
6. **Publish.** `SecretData` + `upsertSecret`.
7. **Compensate.** If the Secret write fails, `DeleteAccessKey(groupID, ak.KeyID)` removes
   the key just minted. A failure of that rollback is logged and swallowed — the reconcile
   is failing anyway and the original error is the one worth surfacing.

Everything from step 5 onward wraps its error in `errCredentialDestroyed`
([:978](../../internal/controller/bucket_controller.go#L978)). That sentinel is read by
`holdsReadyThrough` ([:1630](../../internal/controller/bucket_controller.go#L1630)), which
refuses the degraded hold for it: the operator knows locally that the published credential
is dead, so this is not an unverified provider state and `Ready` must drop at once
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)). Note the asymmetry —
a failure in step 4 (the clear itself) is **not** wrapped. The ADR draws the line *after*
the clear
([ADR 0007 D7](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md))
and the code follows it literally, so a failure inside the clear loop that has already
deleted the live key is not reported as a destroyed credential: `DeleteAllAccessKeys`
([stackit/client.go:341](../../stackit/client.go#L341)) lists and then deletes key by key,
so a mid-loop error can well leave the published credential dead. The next pass repairs
that whenever the clear got far enough to leave the group without any key: the skip
condition fails and a fresh key is minted. If some other key in the group survived, the
count-based skip condition can still accept the dead credential — that is the first entry
under *What is wrong today*. Either way the failing pass takes the ordinary degraded hold
rather than the immediate `Ready` drop.

### Three ids, and why only one is persisted

`CreateAccessKey` returns three distinct values and the difference matters whenever you
touch this code:

| Field on `stackit.AccessKey` | Provider field | What it is | Where it goes |
|---|---|---|---|
| `AccessKeyID` | `accessKey` | the S3 access key id used to sign requests | the Secret, and `status.accessKeyID` |
| `SecretAccessKey` | `secretAccessKey` | the secret half, returned exactly once | the Secret only — never status, event, log or metric |
| `KeyID` | `keyId` | the handle `DeleteAccessKey` needs | held for the length of the call, never persisted |

`ListAccessKeyIDs` ([stackit/client.go:375](../../stackit/client.go#L375)) returns
`k.GetKeyId()`, and the SDK's listing model
(`objectstorage.AccessKey`) carries only `displayName`, `expires` and `keyId` — **the S3
access key id is not in the listing at all**. This sharpens the residual risk recorded in
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md):
the record calls comparing the stored key id against the listing "the obvious next step",
and against the code that comparison is not merely unimplemented, it is not expressible
with the values the operator keeps. The id in the Secret and the ids in the listing are
different identifiers. Making the check identity-based would first require persisting
`KeyID` — the deletion handle — alongside the credential.

## Rotation

The trigger is the annotation `stackit-bucket.gtrfc.com/rotate-credentials-at`
(`RotateCredentialsAtAnnotation`, [api/v1/bucket_types.go:97](../../api/v1/bucket_types.go#L97)),
and the entire comparison is `Bucket.PendingRotationTrigger()`
([api/v1/bucket_types.go:912](../../api/v1/bucket_types.go#L912)): the annotation value,
unless it is empty or **equal to `status.lastRotationTrigger`**, in which case the empty
string means "nothing pending".

It is a single-value equality check, not a set membership test. The operator keeps no
history of handled triggers, which has a consequence worth knowing before you touch it: an
older value that was already handled and has since been superseded rotates **again** if it
is re-applied. Only the one value currently recorded in status is inert
([ADR 0007 D8](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)).

A pending trigger acts by *disabling the skip path* in step 3 above — there is no separate
rotation routine. The replacement path is the same clear-then-create sequence, which is why
a rotation is hard and has no overlap window.

`recordPendingRotation` ([:1051](../../internal/controller/bucket_controller.go#L1051))
does the bookkeeping: it writes `status.lastRotationTrigger` and `status.lastRotationTime`
in memory, logs, and emits the Normal event `CredentialsRotated` (`reasonRotated`,
[:1288](../../internal/controller/bucket_controller.go#L1288)). It writes nothing to the
API server itself; the caller's status update persists it. The metric
`stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds` is derived from
`status.lastRotationTime` by the collector in
[internal/controller/metrics.go:286](../../internal/controller/metrics.go#L286) — the
`if t := b.Status.LastRotationTime; t != nil` branch, whose `Desc` is declared at
[:78](../../internal/controller/metrics.go#L78) — so the series is absent for a `Bucket`
that never rotated.

**It is called from two places, and the second one is not obvious.** The normal call site is
in `reconcileNormal` just before the terminal status write. The second is inside
`provisionCredentialsAndClone` ([:455](../../internal/controller/bucket_controller.go#L455)),
on the branch where a clone is still running and `holdSecretUntilCloned` is off so the
Secret is published early. That pass never reaches the terminal status write — it returns
while the clone runs — so without recording the trigger there, the still-pending annotation
would re-rotate the workload on every 15-second clone progress poll. The clone progress
status updates are what persist the recorded value.

**The crash window is real and deliberate.** The handled value is recorded *after* the
credential is published, so a crash in between leaves the annotation pending and the next
pass rotates again. Both rotations are hard, so the outcome is correct and the cost is a
second disruption.

### Pass ordering, and when the listing is paid

The credential step sits at the end of `provisionCredentialsAndClone`, after the group is
resolved and the isolation policy is written. A pass reaches it in exactly three situations:
no clone is pending; a clone finished in this very pass; or a clone is pending and
`holdSecretUntilCloned` is false. A pass that is still holding the Secret back for an
unfinished clone returns before the credential step and therefore issues **no** access-key
listing at all. The full ordering, including why the policy is written before any workload
credential exists, is in [clone.md](clone.md) and
[reconcile-pipeline.md](reconcile-pipeline.md).

## The admin credential

`ensureAdmin` ([:1435](../../internal/controller/bucket_controller.go#L1435)) is the whole
bootstrap. It holds `r.adminMu` for its entire body, so two concurrent reconciles cannot
both bootstrap.

```
cached (r.admin != nil)?            -> return it, no API server call, no provider call
AdminSecretNamespace == ""?         -> hard error ("set POD_NAMESPACE")
Get admin Secret
  ok    + adminFromSecret != nil    -> cache and return
  ok    + adminFromSecret == nil    -> fall through (incomplete)
  NotFound                          -> fall through
  other error                       -> return it
EnsureCredentialsGroup("operator-admin")   # find-by-display-name or create
DeleteAllAccessKeys(gid)                   # any pre-existing key's secret is unrecoverable
CreateAccessKey(gid)
writeAdminSecret(...)  -- on failure: DeleteAccessKey(gid, ak.KeyID), return error
cache and return
```

`adminFromSecret` ([:2009](../../internal/controller/bucket_controller.go#L2009)) defines
"complete": `accessKeyID`, `secretAccessKey` and `urn` must all be non-empty.
`credentialsGroupID` is read but **not** required, so a Secret missing only that field is
accepted and the cached `adminCreds.groupID` is empty — the field is needed to delete a key
in the group, and today nothing in the provisioning path deletes an admin key outside
bootstrap itself.

| Data key | Content | Required for "complete" |
|---|---|---|
| `accessKeyID` | S3 access key id | yes |
| `secretAccessKey` | S3 secret | yes |
| `urn` | URN of the `operator-admin` group, exempt in every bucket policy | yes |
| `credentialsGroupID` | group id, the handle for deleting a key in it | no |

The Secret's name and namespace come from `--admin-credentials-secret-name` /
`ADMIN_CREDENTIALS_SECRET_NAME` (default `stackit-s3-provisioner-admin`) and
`--operator-namespace` / `POD_NAMESPACE`, wired in
[cmd/main.go:90-95](../../cmd/main.go#L90-L95). A configured service-account key with no
known namespace exits at startup, in `newStackitClient`
([cmd/main.go:363-365](../../cmd/main.go#L363-L365)), which is
[ADR 0004 D4](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md). The
Helm chart does not template either setting and does not create the Secret — verified by
grep over [deploy/helm/stackit-s3-provisioner/](../../deploy/helm/stackit-s3-provisioner/):
the only thing it supplies is `POD_NAMESPACE` from the downward API in
[templates/deployment.yaml](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml).

`writeAdminSecret` ([:1512](../../internal/controller/bucket_controller.go#L1512)) is a
`CreateOrUpdate` like `upsertSecret`, with one deliberate difference: it sets **no owner
reference**. Nothing garbage-collects this Secret, which is what lets a reinstall pick the
same credential back up — and what leaves a live privileged credential behind on uninstall.

`operator-admin` is the one credentials group resolved by display name, through
`EnsureCredentialsGroup` ([stackit/client.go:325](../../stackit/client.go#L325)) which lists
and matches `DisplayName`. That is the single carve-out from
[ADR 0002 D3](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md), and its
reason is recorded in
[ADR 0004 D6](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md): a
workload group belongs to a bucket and is attributed through it, this group belongs to no
bucket, so the name is the only handle there is. Do not copy the pattern.

### The cache has no invalidation point

`r.admin` is a plain field on `BucketReconciler` guarded by `r.adminMu`. It is assigned in
exactly two places — the successful load from the admin Secret
([:1452](../../internal/controller/bucket_controller.go#L1452)) and the end of a bootstrap
([:1481](../../internal/controller/bucket_controller.go#L1481)) — and it is never set back
to `nil` outside tests
([reconciler_fake_test.go:719](../../internal/controller/reconciler_fake_test.go#L719)).
There is no invalidation point. `ensureAdmin` returns the cached value *before* it
issues the `Get` of the admin Secret, and no controller watches that Secret — the Secret
watch is filtered to `isManagedSecret`, which the admin Secret does satisfy (it carries the
label), but `bucketsForSecret` maps it only to `Bucket`s in the operator namespace whose
`secretRef` names it, and such a `Bucket` is refused by `specGuardError` anyway.

The practical consequences, both of them from
[ADR 0004 D9](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md):

- Deleting the admin Secret under a running process is a **no-op** until the process
  restarts. Every `Bucket` keeps being served from the cached copy.
- Restarting without deleting the Secret reloads the same credential, because it is
  complete and D5's bootstrap does not run.

Any future auto-repair has to introduce an explicit invalidation point; there is none to
hook today.

The measuring controller is the one other reader.
`BucketUsageReconciler.readAdmin`
([internal/controller/bucket_usage_controller.go:269](../../internal/controller/bucket_usage_controller.go#L269))
re-reads the Secret for every measurement, has its own `AdminSecretName`/`AdminSecretNamespace`
fields and **never bootstraps**: a missing or incomplete Secret makes it log at V(1) and
wait ([ADR 0004 D10](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md),
[usage-measurement.md](usage-measurement.md)). It is therefore not a way around the cache
either — it reads, it never writes.

## Who uses which credential

Worth keeping straight while editing, because using the wrong one silently works until a
policy is in place:

| Operation | Credential | Code |
|---|---|---|
| bucket ownership and attribution tags | admin | `newS3Admin` ([:648](../../internal/controller/bucket_controller.go#L648)) |
| isolation policy write and drift heal | admin | `ensureBucketPolicy` ([:1174](../../internal/controller/bucket_controller.go#L1174)) |
| emptiness check before deletion | admin | `assertBucketEmpty` ([:1313](../../internal/controller/bucket_controller.go#L1313)) |
| opt-in wipe | admin | `wipeBucket` ([:1292](../../internal/controller/bucket_controller.go#L1292)) |
| size listing | admin | `BucketUsageReconciler.measure` |
| clone destination | admin | [clone.md](clone.md) |
| clone **source** | the Secret named by `spec.cloneFrom.secretRef`, read from the CR's namespace | [clone.md](clone.md) |
| everything the workload does | the published workload credential | — |

The clone source is the single exception
([ADR 0004 D2](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)): a
foreign bucket at a foreign endpoint, where the admin credential has no standing.

## Tests that pin this

<details>
<summary>The offline tests to run before you change anything on this page</summary>

| Test | File | What it pins |
|---|---|---|
| `TestSecretKeysDefaults`, `TestSecretKeysOverrides` | [api/v1/bucket_types_test.go](../../api/v1/bucket_types_test.go) | the resolver and its fallbacks |
| `TestSecretDataDefaults`, `TestSecretDataCustomKeys`, `TestSecretDataRegionDefault` | same | the data-map builder |
| `TestSecretDataOmitsEmptyOptional` | same | `endpoint`/`bucketURL` are written only when non-empty |
| `TestSecretDataUsesEffectiveName` | same | the frozen physical name, not `spec.bucketName` |
| `TestValidateSecretKeysOK`, `TestValidateSecretKeysCollision` | same | the collision validator |
| `TestPendingRotationTrigger` | same | the single-value trigger comparison |
| `TestSecretHasCredsAndAccessKeyID`, `TestSecretHasCreds_HonorsKeyOverrides` | [internal/controller/bucket_controller_test.go](../../internal/controller/bucket_controller_test.go) | the skip condition reads the *resolved* key names |
| `TestIsAdminSecret`, `TestAdminFromSecret` | same | the admin-Secret guard and what "complete" means |
| `TestBucketsForSecret`, `TestIsManagedSecret` | same | the Secret watch mapping |
| `TestReconcileHealsLostSecret` | [internal/controller/reconciler_fake_test.go](../../internal/controller/reconciler_fake_test.go) | a deleted Secret is healed by replacement, group holds exactly one key afterwards |
| `TestReconcileRotateCredentials` | same | new value rotates, unchanged value is a no-op, changed value rotates again, one key, `CredentialsRotated` event |
| `TestEnsureAdminRebootstrap` | same | incomplete Secret rebootstraps, complete Secret is reused, missing namespace is a hard error |
| `TestSecretWriteFailureRollsBackAccessKey` | [internal/controller/reconciler_errors_test.go](../../internal/controller/reconciler_errors_test.go) | the compensating delete: zero keys left after a failed Secret write |
| `TestAdminSecretWriteFailureRollsBackAdminKey` | same | the same compensation on the admin path |
| `TestDestroyedCredentialIsNeverHeld` | [internal/controller/reconciler_degraded_test.go](../../internal/controller/reconciler_degraded_test.go) | `errCredentialDestroyed` bypasses the degraded hold |
| `TestCloneHoldsSecretUntilCompleted`, `TestClonePublishesSecretEarlyWhenNotHeld` | [internal/controller/reconciler_clone_test.go](../../internal/controller/reconciler_clone_test.go) | the two clone orderings of the credential step |

All of these are netless and run under `make test-unit-coverage`.

</details>

## What is wrong today

**The backing check is count-based and cannot currently be made identity-based.** The skip
condition accepts "the group holds at least one key". A workload key deleted out of band
while another key exists in the same group leaves the `Bucket` `Ready` with a dead
credential in its Secret and nothing in status explaining it. As shown above, the fix is
larger than
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
suggests: the listing does not return S3 access key ids, so the `keyId` would have to be
persisted at create time first.

**A re-created bucket orphans its previous credentials group.** `ensureBucket`
([:569](../../internal/controller/bucket_controller.go#L569)) reports `created=true` for a
bucket it had to create because `HasBucket` said it was absent — including a bucket that
was deleted out of band. That flag is passed to `guardGroupCreate`
([:914](../../internal/controller/bucket_controller.go#L914)) as `freshBucket` and skips the
[ADR 0002 D8](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) refusal
to create a second group while the one in status still exists. The re-created bucket carries
no `credentials-group-id` tag, so a new group is created, tagged, given a new key and
written into the workload Secret. The previously attributed group survives holding the
now-unused old key, and nothing deletes it — teardown only releases the group the bucket
itself attributes ([ADR 0002 D4](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)).

**A Secret controlled by another controller costs a credential on every pass.** The
`AlreadyOwnedError` refusal happens *inside* `upsertSecret`, which runs after the clear and
the create. So a `Bucket` pointed at, say, a Secret owned by another operator does not fail
cheaply: each pass clears the group, mints a key, fails the write, deletes the key again and
reports `errCredentialDestroyed`. The refusal is correct; its position in the sequence means
the cost is two provider writes per attempt rather than zero. *Not verified:* no test covers
a foreign-owned Secret — this is reasoned from `SetControllerReference` returning before
`CreateOrUpdate` writes, and from the ordering in `ensureAccessKeyAndSecret`.

**An adopted Secret is deleted like an owned one.** `deleteSecret` re-checks nothing. A
Secret that existed before the `Bucket` did, and that the operator adopted because it had no
controller owner, is removed when the `Bucket` is deleted, together with whatever unrelated
data keys it carried. Named as a gap on
[docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md).

**Nothing marks an admin bootstrap.** No event, no metric — a re-mint is visible only in the
operator log. Combined with the cache having no invalidation point, an admin credential
destroyed in the cloud surfaces as an opaque data-plane failure repeating forever
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md), Residual
risks).

**Not verified: renaming `spec.secretRef.name` on a live `Bucket`.** By the rules above the
new name looks unprovisioned, the skip condition fails and the credential is replaced, with
the old Secret left holding dead values until the CR is deleted. No test covers a rename;
this is reasoned from the write path, not observed.

**Not verified: behaviour during a leader handover.** A rolling update releases leadership
without an atomic handover, and a bootstrap that clears the `operator-admin` group's keys
while the outgoing process still holds one has not been analysed. The chart enables leader
election by default; the operator's own `--leader-elect` flag defaults to off, so a
deployment that bypasses the chart may run several reconciling replicas at once.
