# ADR 0002: A Credentials Group Is Attributed Through Its Bucket

## Status

Accepted. Date: 2026-09-03. Amended 2026-09-19 (editorial: no rule changed).

Implemented and in use: the bucket tags `credentials-group-id` and `credentials-group-urn` carry the
attribution, the bucket's own isolation policy is the migration path for buckets provisioned before
this record, and teardown releases only what the bucket itself attributes. This closes security
finding 1 — the workload group could be adopted across namespaces through its display name — and the
D2 violation recorded in [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md), which is
amended accordingly. Verified on 2026-09-03 offline and against the real STACKIT API, where a bucket
seeded the pre-ADR way kept its bucket, group, key and Secret through the upgrade, through a restore
without status, and through teardown; nothing in this record is outstanding, and what remains is
listed under Residual risks.

## Context

A Bucket's workload credentials group was located by a generated display name,
`s3op-<namespace>-<name>` truncated to 23 characters plus an 8-hex FNV-1a-32 suffix over
`<namespace>/<name>`. The lookup was find-or-create by that name with no ownership check, and
teardown fell back to the same name. Distinct namespace/name pairs derive the same name —
`("gitlab","gitlab-artifacts")` and `("gitlab-gitlab","artifacts787ngo")` both derive
`s3op-gitlab-gitlab-arti-70dbcfc2`, reproduced on 2026-08-24 — and a namespace admin, who controls
the Bucket name but not the namespace, can brute-force the 32-bit suffix offline for any
prefix-related namespace. One `kubectl apply` then made the operator adopt the victim's group, delete
its live key, mint a new one into the attacker's Secret and, through the victim's own bucket policy,
hand full object access to a foreign namespace's bucket. This was security finding 1, open since
2026-08-24, and the reason ADR 0001 recorded D2 as violated. Reviewing the user-facing RBAC work made
it concrete: aggregating a write role on `buckets` into the built-in `edit`/`admin` roles would have
handed that capability to every namespace admin.

Two facts fix the shape of the solution. A credentials group has no owner field: the provider's model
is `credentialsGroupId`, `displayName` and `urn`, and display names are not unique within a project.
A bucket, by contrast, already carries an admin-key-only ownership record — the tags `managed-by` and
`owner=<namespace>/<name>` — and D2 of ADR 0001 holds for buckets. The bucket's isolation policy, also
written with the admin key, names the workload group as the principal of the statement
`workload-objects-only`; it is a record the operator itself wrote for every bucket provisioned before
this record. So the group is attributed through the bucket: a tag names the group, and the policy is
the migration path for buckets that predate the tag.

One more force shaped D8. The first full end-to-end run against the real API on 2026-09-03 passed
every test and still left one keyed group behind: `s3op-s3e2e-alpha-backup-c5095f96`, belonging to a
Bucket provisioned concurrently with two others. No operator log survived that run, so the cause is
inferred and not read: the pass after provisioning did not see the group it had just created — a
project listing or a tag read answering with state older than the operator's own write — and minted a
second one, which teardown later released while the first kept its key. The rerun, with the log
captured, was clean and additionally showed every teardown running twice, because a conflict on the
finalizer removal requeues it; the second pass finds neither bucket nor group.

## Decision

**D1 — A bucket names its workload credentials group in the bucket tags `credentials-group-id`
and `credentials-group-urn`.** They are written with the admin key immediately after the group is
created in the control plane, before any access key is minted, next to the ownership tags.
Provisioning resolves the group from these tags first; whether the tagged group still exists is
probed by id through its keys endpoint, never through the project listing, and a tag naming a group
that no longer exists is treated as absent and overwritten. The URN tag makes the tag path
independent of the listing: a bucket carrying both tags resolves its group without one, and a bucket
that predates `credentials-group-urn` reads the URN from the listing once and is then tagged with
it.

**D2 — A bucket without the tag is attributed through its own isolation policy.** The single
principal of the statement `workload-objects-only` is resolved to a group by URN, the tag is written,
and the migration is reported as the event `CredentialsGroupAttributed`. A policy that cannot be read
is an error that ends the reconcile, not "no policy": treating it as absent would create a second
group for a bucket that has one and rotate its workload out of a working credential.

**D3 — Neither tag nor policy: a fresh group is created and tagged.** A workload group is never
looked up by display name; find-or-create by display name remains in use only for the shared
`operator-admin` group. If the tag write fails, the fresh group is deleted again so retries do not
leave a trail of empty groups.

**D4 — Teardown releases only a group the bucket attributes.** It applies D1 and D2 without D3. A
bucket that attributes no group, a bucket that is not the Bucket's, or an absent bucket releases
nothing; when `status.credentialsGroupID` recorded a group and that group still exists, the event
`CredentialsGroupNotAttributable` names it so an operator can clean up by hand. The recorded status is
not a source of deletion.

**D5 — A read grant resolves through the grantee's bucket.** Resolution takes the grantee CR's
resolved physical bucket name, requires that bucket to carry the grantee's ownership tags, and applies
D1 and D2 to it. There is no display-name lookup and therefore no ambiguity rule; a grantee whose
bucket is not its own, or that attributes no group yet, is skipped with the event `ReadGrantPending`.

**D6 — Nothing a namespace user controls is a source of attribution.** Not the Bucket spec, not its
annotations, not the Secret's content, not the status. Every source in D1–D5 is written with the admin
key and read back from the bucket that the ownership-tag check already proved to be the Bucket's; a
bucket that fails that proof receives no tag.

**D7 — The generated group display name is a label, not an identity.** It is kept exactly as it was so
existing groups keep their names, and it is shown to humans in the provider console and in the
end-to-end cleanup tooling; it is never used to find, adopt or delete a group.

**D8 — Eventual consistency never creates a second group.** The provider may answer a read with state
older than the operator's own last write: a project listing without the group created a moment ago, or
a bucket without the tags and policy just written to it. Two rules absorb this. Once both tags are
present — and every attribution is written with both — the tag path (D1) needs no listing; a bucket
carrying only `credentials-group-id`, from before the URN tag existed, still resolves its URN from the
project listing and, while that listing lags, waits and retries rather than creating a second group.
And before D3 creates a group for a bucket that shows no attribution, the operator probes the group id recorded in `status.credentialsGroupID`: if that group
still exists, the bucket is being read stale, the reconcile is retried and nothing is created; only a
404 on that probe — or a bucket created in this very pass, which cannot have a group yet — lets the
create proceed. The recorded id is used solely to decide whether to wait; it never names the group the
bucket is bound to, so a forged status can at most delay the forger's own Bucket.

## Consequences

* **Upgrade is silent.** The first reconcile of every existing Bucket reads the policy, writes the tag
  and takes the unchanged credential path, because the Secret holds credentials and the group holds a
  key. No group is renamed, no key rotated, no Secret rewritten; workloads notice nothing. The drift
  resync (`driftResyncInterval`, `10m` # default) reaches every Bucket without an event.
* **A grant survives the upgrade window.** Migration does not change a grantee's URN, so the watch
  that wakes grantors does not fire; D5's policy fallback resolves an unmigrated grantee anyway, so no
  reader drops out of a policy for a resync interval.
* **Rollback works and re-opens the finding.** An operator build from before this record compares only
  the two ownership tags, ignores the attribution tags and finds groups by their unchanged names.
* **A restore without status re-attaches.** Bucket, tag and policy survive in the cloud, so a CR
  re-created with a fresh UID and an empty status recovers the same group. Before this record that
  path relied on the display name, and that was the reason the name could not be made collision-proof.
* **Display names may repeat.** A colliding pair now yields two groups with the same name; the control
  plane permits it and nothing depends on uniqueness any more. The provider console shows the id.
* **A bucket deleted out of band orphans its group.** The next reconcile creates a new bucket with no
  tag and no policy, hence a new group; the old group keeps a live key that no policy trusts, since
  every operator-managed bucket denies all but its own principals. Previously the display name found
  the old group. The teardown event and the display name make the orphan findable.
* **More reads per reconcile:** one tag read and one keys-endpoint probe, plus one policy read for a
  bucket without the tags, and per grantee one bucket lookup plus the same reads. Teardown no longer
  finds a group by listing it — the group comes from the bucket; the one project-wide group listing
  it still performs per pass serves only the URN lookups, of the D2 policy principal and of a bucket
  tagged before `credentials-group-urn` existed.
* **The reason for not aggregating a write role on `buckets` is gone.** The consequence recorded in
  [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) was conditional on the D2 violation; it
  is amended by this record, and that ADR now carries the shipped RBAC decision and its own reason for
  aggregation-only roles.
* The name-based teardown fallback is gone; `status.credentialsGroupID` stays informational.

## Alternatives Considered

**A collision-proof display name.** `s3op-<sha256(namespace/name)>` for new groups, legacy groups
migrated through the id recorded in status. Rejected: legacy groups would stay adoptable by name until
every one of them is migrated, the migration would rest on trusting the status, the names would be
unreadable, and a restore without status would still be name-based.

**`status.credentialsGroupID` as the source of attribution.** This was the cheap option — the id is
already recorded, no tag write, no policy read, no migration — and it lost anyway. Status is writable
by whoever holds update on the `buckets/status` subresource; the attribution would be safe only for as
long as no RBAC binding grants that to a namespace user. A structural property must not depend on
cluster RBAC configuration.

**An annotation on the CR naming the group.** Rejected. Annotations are writable with the write role
this repository ships; an attacker would point the annotation at the victim's group.

**The access key id in the Secret as the source.** Rejected. `status.accessKeyID` is readable by
anyone with read access to `buckets`, so a Secret naming a victim's key id is forgeable; together with
the rotation annotation that becomes a takeover.

**The full namespace and name in the display name.** Rejected. The provider caps display names at 32
characters.

**An owner marker on the group itself.** Impossible: the credentials-group model has no field for it.

## Residual risks

* **Rollback re-opens the finding** for as long as an operator build from before this record runs.
* **Out-of-band bucket deletion orphans a keyed group**; teardown raises an event naming it.
* **A crash between creating the group and writing the tag** leaves an empty group if the compensating
  delete also fails. It holds no key and carries the display name.
* **A legacy bucket with neither tag nor policy** — one whose operator crashed between group creation
  and policy write and never recovered before the upgrade — gets a fresh group and orphans the old
  one. Practically an empty set, since the previous behaviour retried within seconds.
* **D8 can hold a Bucket in retry** when its bucket was re-created after an out-of-band deletion and
  the operator crashed between creating the bucket and creating its group: the next pass adopts the
  empty bucket, sees the previously recorded group still alive, and waits. The error names the group;
  deleting it by hand releases the Bucket. Not reachable without both an out-of-band deletion and a
  crash inside that window.
* **Not verified:** that the real API accepts two groups with the same display name — the code history
  records it as permitted and the offline fake models it, but no test against the real API exercises
  it; the provider's tag limits beyond the four tag keys used here; a rollback to an operator build
  from before this record, beyond reading that build's source, which ignores the attribution tags and
  finds groups by display name, so it works and re-opens the finding for as long as it runs.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — namespace as the trust boundary; its
  D2 and its RBAC consequence are amended by this record
* [ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the isolation policy whose
  `workload-objects-only` statement D2 migrates from
* [ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) — read grants, whose
  grantee resolution is D5
* [docs/security/ownership-and-attribution.md](../security/ownership-and-attribution.md) — the tag
  checks, what they defend against and where they do not reach
* [docs/developer/bucket-identity.md](../developer/bucket-identity.md) — how attribution, migration
  and the lag guard are implemented
