# ADR 0002: A Credentials Group Is Attributed Through Its Bucket

## Status

Accepted. Date: 2026-09-03.

Implemented in the tree at this commit: `resolveWorkloadGroup`, `releaseWorkloadGroup` and
`resolveReadGrants` in [`bucket_controller.go`](../../internal/controller/bucket_controller.go),
`WorkloadPrincipalFromPolicy` in [`stackit/s3.go`](../../stackit/s3.go), the bucket tag
`credentials-group-id`. This closes security finding 1 in [CLAUDE.md](../../CLAUDE.md) and the
D2 violation recorded in [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md), which is
amended accordingly.

Verified by the offline suite
([`reconciler_attribution_test.go`](../../internal/controller/reconciler_attribution_test.go):
name collision, legacy migration, restore without status, group deleted out of band, tag-write
rollback, policy-read failure, teardown via policy / unattributable / foreign bucket, grants to a
legacy and to a foreign-owned grantee) and by
[`attribution_integration_test.go`](../../internal/controller/attribution_integration_test.go)
against the real STACKIT API: a bucket seeded the pre-ADR way (ownership tags, group by display
name, key, policy, Secret) is reconciled by the production reconciler and keeps bucket, group id,
key id and Secret bytes through the upgrade and through a CR restore without status; teardown
then releases exactly those resources. Run green against project `ebc9d379…` on 2026-09-03
(123s; one transient `connection reset by peer` on the bucket delete, retried as the
finalizer would).

The first full cloud e2e run of this change (`make e2e-stackit`, 2026-09-03) passed every test
and yet the sweep found one keyed group left behind: `s3op-s3e2e-alpha-backup-c5095f96`, the group
of `s3e2e-alpha/backups` in `TestCloudReadGrant`, a Bucket provisioned concurrently with two
others. No operator log survived the run, so the cause is inferred, not read: the pass after
provisioning did not see the group it had just created — a project listing or a tag read lagging
behind the write — and minted a second one, which the teardown later released while the first
kept its key. D8 and the URN tag were added in response, with offline tests for both lag shapes
(`TestGroupAttributionSurvivesListingLag`, `TestGroupAttributionWaitsForStaleBucketRead` and
siblings). The second full run, with the operator log captured this time, passed with an empty
sweep. That log also showed every teardown running twice — a pre-existing conflict on the
finalizer removal requeues it — and the second pass finding neither bucket nor group; the
`CredentialsGroupNotAttributable` event is therefore raised only when the recorded group still
exists (`TestTeardownSecondPassIsSilent`).

Not verified: a rollback to a pre-ADR operator beyond reading its code (it ignores the tags and
finds groups by display name, so it works and re-opens the finding for as long as it runs).

## Context

A Bucket's workload credentials group was located by its display name, `workloadGroupName`:
`s3op-<namespace>-<name>` truncated to 23 characters plus an 8-hex FNV-1a-32 of
`<namespace>/<name>`. `EnsureCredentialsGroup` was find-or-create by that name with no ownership
check, and `resolveWorkloadGroupID` fell back to the same name during teardown. Distinct
namespace/name pairs derive the same name — CLAUDE.md records `("gitlab","gitlab-artifacts")` and
`("gitlab-gitlab","artifacts787ngo")`, reproduced 2026-08-24 — and a namespace admin, who controls
the Bucket name but not the namespace, can brute-force the 32-bit suffix offline for any
prefix-related namespace. One `kubectl apply` then made the operator adopt the victim's group,
delete its live key, mint a new one into the attacker's Secret and, through the victim's own
policy, hand over full object access to a foreign namespace's bucket. This was security finding 1
(open since 2026-08-24) and the reason ADR 0001 recorded D2 as violated. The review of the
user-facing RBAC ticket made it concrete: aggregating an `edit` ClusterRole on `buckets` into the
built-in roles would have handed that capability to every namespace admin.

Two facts fix the shape of the solution. A credentials group has no owner field: the API model is
`credentialsGroupId`, `displayName`, `urn`, and display names are not unique in a project. A
bucket, by contrast, already carries an admin-key-only ownership record — the tags `managed-by` and
`owner=<namespace>/<name>` checked by `isOwnedByUs` — and D2 holds for buckets. The bucket's
isolation policy, also written with the admin key, names the workload group in the statement
`workload-objects-only`; it is the record the operator itself wrote for every bucket provisioned
before this ADR.

So the group is attributed through the bucket: a third tag names the group, and the policy is the
migration path for buckets that predate the tag.

## Decision

**D1 — A bucket names its workload credentials group in the bucket tags `credentials-group-id`
and `credentials-group-urn`.** They are written right after `CreateCredentialsGroup`, before any
access key is minted, with the admin key, next to the ownership tags. Provisioning resolves the
group from these tags first (`resolveWorkloadGroup`); whether the tagged group still exists is
probed by id through its keys endpoint (`groupExists`), not through the project listing, and a
tag naming a group that no longer exists is treated as absent and overwritten. The URN tag makes
the tag path independent of the listing altogether.

**D2 — A bucket without the tag is attributed through its own isolation policy.** The single
principal of the statement `workload-objects-only` (`WorkloadPrincipalFromPolicy`) is resolved to a
group by URN, the tag is written, and the migration is reported as the event
`CredentialsGroupAttributed`. A policy that cannot be read is an error that ends the reconcile, not
"no policy": treating it as absent would create a second group for a bucket that has one and
rotate its workload out of a working credential.

**D3 — Neither tag nor policy: a fresh group is created and tagged.** `FindCredentialsGroupByName`
is never called for a workload group; `EnsureCredentialsGroup` remains in use only for the shared
`operator-admin` group. If the tag write fails, the fresh group is deleted again so retries do not
leave a trail of empty groups.

**D4 — Teardown releases only a group the bucket attributes.** `releaseWorkloadGroup` applies D1
and D2 without D3. A bucket that attributes no group, a bucket that is not the Bucket's, or an
absent bucket releases nothing; when the Bucket's status recorded a group id and that group still
exists, the event `CredentialsGroupNotAttributable` names it so an operator can clean up by hand.
The recorded status is not a source of deletion.

**D5 — A read grant resolves through the grantee's bucket.** `resolveReadGrants` takes the grantee
CR's `EffectiveBucketName()`, requires the bucket to carry the grantee's ownership tags, and applies
D1 and D2 to it. No display-name lookup, hence no ambiguity rule; a grantee whose bucket is not its
own, or attributes no group yet, is skipped with `ReadGrantPending`.

**D6 — Nothing a namespace user controls is a source of attribution.** Not the Bucket spec, not its
annotations, not the Secret's content, not the status. Every source in D1–D5 is written with the
admin key and read back from the bucket that `isOwnedByUs` already proved to be the Bucket's; a
bucket that fails that proof receives no tag (`errBucketNotOwned`).

**D7 — `workloadGroupName` is a label, not an identity.** It is kept exactly as it was so existing
groups keep their names, and it is shown to humans (console, `hack/e2ecleanup`); it is never used
to find, adopt or delete a group.

**D8 — Eventual consistency never creates a second group.** The provider may answer a read with
state older than the operator's own last write: a project listing without the group created a
moment ago, or a bucket without the tags and policy just written to it. Two rules absorb this.
The tag path (D1) does not depend on the listing at all. And before D3 creates a group for a
bucket that shows no attribution, the operator probes the group id recorded in the Bucket's
status: if that group still exists, the bucket is being read stale, the reconcile is retried with
`errAttributionLagging` and nothing is created; only a 404 on that probe — or a bucket created in
this very pass, which cannot have a group yet — lets the create proceed. The recorded id is used
solely to decide whether to wait; it never names the group the bucket is bound to, so a forged
status can at most delay the forger's own Bucket.

## Consequences

* **Upgrade is silent.** The first reconcile of every existing Bucket reads the policy, writes the
  tag and takes the unchanged path in `ensureAccessKeyAndSecret` (Secret has credentials, group has
  a key). No group is renamed, no key rotated, no Secret rewritten; workloads notice nothing. The
  drift resync (`driftResyncInterval`, default 10m) reaches every Bucket without an event.
* **A grant survives the upgrade window.** Migration does not change a grantee's URN, so
  `granteeCredentialsPredicate` does not wake grantors; D5's policy fallback resolves an unmigrated
  grantee anyway, so no reader drops out of a policy for a resync interval.
* **Rollback works and re-opens the finding.** A pre-ADR operator compares only the two ownership
  tag keys, ignores the third tag and finds groups by their unchanged names.
* **A restore without status re-attaches.** Bucket, tag and policy survive in the cloud, so a CR
  re-created with a fresh UID and empty status recovers the same group. Before this ADR that path
  relied on the name and was the reason the name could not be made collision-proof.
* **Display names may repeat.** A colliding pair now yields two groups with the same name; the
  control plane permits it and nothing depends on uniqueness any more. The STACKIT console shows
  the id.
* **A bucket deleted out of band orphans its group.** The next reconcile creates a new bucket with
  no tag and no policy, hence a new group; the old group keeps a live key that no policy trusts
  (every operator bucket denies all but its own principals). Previously the name found the old
  group. The teardown event and the display name make the orphan findable.
* **One more data-plane call per reconcile** (tag read) plus one keys-endpoint probe, one policy
  read for a bucket without the tags, and per grantee one control-plane bucket lookup plus the
  same reads. Teardown needs no group listing.
* **The reason for not aggregating the `edit` ClusterRole is gone.** ADR 0001's consequence was
  conditional on the D2 violation; it is amended with this ADR, and the RBAC ticket
  ([`local_aggregation.md`](../../local_aggregation.md)) follows.
* `resolveWorkloadGroupID` and the name-based teardown fallback are removed;
  `status.credentialsGroupID` stays informational.

## Alternatives Considered

### A collision-proof display name

`s3op-<sha256(namespace/name)>` for new groups, legacy groups migrated by the id recorded in
status. Rejected: legacy groups would stay adoptable by name until every one is migrated, the
migration would rest on trusting status, the names would be unreadable, and a restore without
status would still be name-based.

### `status.credentialsGroupID` as the source

Rejected. Status is writable by whoever holds `buckets/status` update; the attribution would be
safe only for as long as no RBAC grants that to a namespace user. A structural property must not
depend on cluster RBAC configuration.

### An annotation on the CR naming the group

Rejected. Annotations are writable with the `edit` ClusterRole this repository is about to ship;
an attacker would point the annotation at the victim's group.

### The access key id in the Secret as the source

Rejected. `status.accessKeyID` is readable by everyone with `view`, so a Secret naming a victim's
key id is forgeable; with the rotation annotation that becomes a takeover.

### The full namespace/name in the display name

Rejected. The API caps display names at 32 characters.

### An owner marker on the group

Impossible: the credentials-group model has no field for it.

## Residual risks

* **Rollback re-opens the finding** for as long as the pre-ADR operator runs (documented above).
* **Out-of-band bucket deletion orphans a keyed group** (documented above; event on teardown).
* **A crash between `CreateCredentialsGroup` and the tag write** leaves an empty group if the
  rollback delete also fails; it holds no key and carries the display name.
* **A legacy bucket with neither tag nor policy** — a pre-ADR operator that crashed between group
  creation and policy write and never recovered before the upgrade — gets a fresh group and orphans
  the old one. Practically an empty set, since the old operator retried within seconds.
* **D8 can hold a Bucket in retry** when its bucket was re-created after an out-of-band deletion
  and the operator crashed between creating the bucket and creating its group: the next pass
  adopts the (empty) bucket, sees the previous group still recorded and alive, and waits. The
  error names the group; deleting it by hand releases the Bucket. Not reachable without both an
  out-of-band deletion and a crash inside that window.
* **Not verified:** the real API's acceptance of two groups with the same display name (the
  `resolveReadGrants` comment history records that it is permitted; the offline fake models it, the
  integration test does not exercise it); StorageGRID tag limits beyond the three tags used.

## References

* [`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go) —
  `tagCredentialsGroupID`, `resolveWorkloadGroup`, `releaseWorkloadGroup`,
  `reportGroupNotAttributable`, `resolveReadGrants`, `workloadGroupName`, `isOwnedByUs`
* [`stackit/s3.go`](../../stackit/s3.go) — `WorkloadPrincipalFromPolicy`, `BuildIsolationPolicy`,
  `sidWorkloadObjectsOnly`
* [`internal/controller/reconciler_attribution_test.go`](../../internal/controller/reconciler_attribution_test.go),
  [`internal/controller/attribution_integration_test.go`](../../internal/controller/attribution_integration_test.go)
* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — D2, amended by this ADR
* [CLAUDE.md](../../CLAUDE.md) — security finding 1 (closed by this ADR)
