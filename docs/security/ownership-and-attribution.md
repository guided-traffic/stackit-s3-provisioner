# Ownership and attribution

This page is about one question the operator has to answer before it touches anything in the cloud:
*is this object mine?* It describes the records the operator writes to make that answer possible —
the four bucket tags, the frozen name and the bucket's own isolation policy — the checks that read
them, what an attacker or a misconfiguration cannot do because of them, and where the answer can
still come out wrong. If you are looking for what keeps one tenant's *data* away from another's once
ownership has been settled, that is the bucket policy and belongs to
[tenancy-and-isolation.md](tenancy-and-isolation.md); if you are looking for who holds which
credential and what its loss costs, that is [credentials-and-secrets.md](credentials-and-secrets.md);
if you are looking at what handing somebody `Bucket` write access delegates, that is
[rbac-and-privilege.md](rbac-and-privilege.md).

## The records attribution is made of

STACKIT Object Storage has no owner field on any of its resources — not on a bucket, not on a
credentials group. So every attribution record here is something the operator writes itself. There
are six: the ownership tags `managed-by` and `owner`, the attribution tags `credentials-group-id`
and `credentials-group-urn`, the `workload-objects-only` statement of the bucket's own isolation
policy, and the `stackit-bucket.gtrfc.com/resolved-bucket-name` annotation on the Bucket CR. What
each of them says is tabulated once, in the [naming conventions](../../README.md#naming). What
matters here is not what a record says but what wrote it, because that is what decides whether it
can be forged:

| Record | Written with | Who else can write it |
|---|---|---|
| The four bucket tags | The operator's admin S3 credential, through the data plane ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D2) | Nobody holding Kubernetes permissions alone — see *Who can write these records* below |
| Statement `workload-objects-only` | The same admin credential, on every policy write; it is the migration path for buckets provisioned before the attribution tags existed ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D2) | The same — the workload is denied every bucket-policy action |
| The resolved-name annotation | The operator's **Kubernetes** service account, before any cloud resource is created ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3) | Anybody who may write the Bucket CR, which is the one exception worth reading carefully — [H-19](#h-19-an-empty-untagged-bucket-is-adopted-and-the-applier-can-choose-which-one) |

The four tags are one tag set, and recording an attribution adds to that set rather than replacing
it, so binding a credentials group to a bucket can never silently drop an ownership tag — nor a tag
somebody else put on the bucket. How the read-modify-write is sequenced is in
[../developer/bucket-identity.md](../developer/bucket-identity.md).

## The ownership tags, and what they prevent

`isOwnedByUs` is true only when **both** tags match: `managed-by` equals this installation's
ownership name and `owner` equals the CR's `<namespace>/<name>`. One out of two is not ownership.
The `owner` value deliberately excludes `metadata.uid`, which is reassigned when a CR is re-created,
so a disaster-recovery replay from Git re-adopts its own buckets instead of provisioning a parallel,
empty set ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D6).

Three separate operations refuse to proceed without that proof, and they are checked independently
rather than once at the top of the pass:

- **Adoption.** A pre-existing bucket is taken over only if it passes the check, or if it is
  untagged *and* empty. Anything else is an `ownershipCollisionError`, which parks the Bucket as
  `Ready=Failed` without requeueing and raises a warning event with reason `Failed`
  ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D2).
- **Attribution of the credentials group.** `resolveWorkloadGroup` re-reads the tags and returns
  `errBucketNotOwned` before it will read, write or create anything, so a bucket that is not this
  CR's can neither lend its group nor receive a tag
  ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D6).
- **Destruction.** `deleteBucketIfOwned` and the wipe path each re-read the tags at teardown time
  ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D7).

What that buys, concretely: a Bucket CR in namespace A cannot be made to manage, re-credential or
delete the bucket of a Bucket CR in namespace B, because B's bucket carries `owner=B/...`. It cannot
clobber a bucket that exists in the STACKIT project but was never provisioned by this operator, as
long as that bucket either carries foreign tags or carries none and holds data — an untagged *empty*
bucket is the one case `adoptOrCollide` does adopt, by design and at a cost
([H-19](#h-19-an-empty-untagged-bucket-is-adopted-and-the-applier-can-choose-which-one)). And it cannot
wipe data the operator cannot prove it provisioned.

### The five cases, in full

| The bucket named by this CR | What happens |
|---|---|
| Does not exist | Created, waited for, then stamped with both ownership tags |
| Exists, tags match this CR | Adopted silently |
| Exists, tags name another installation or another CR | `ownershipCollisionError` — parked, no requeue, warning event |
| Exists, no tags at all, and is empty | Adopted and stamped: this is exactly the state a crash between create and tag-write leaves behind |
| Exists, no tags at all, and holds objects | `ownershipCollisionError` — treated as foreign |

The untagged-and-empty case is a deliberate trade: without it, a crash in the one-second window
between `CreateBucket` and the tag write would leave a bucket the operator could never adopt and
never delete. Its cost is [H-19](#h-19-an-empty-untagged-bucket-is-adopted-and-the-applier-can-choose-which-one).

The first row carries a precondition the table cannot show: it is the answer for a CR that has not
completed a provisioning round yet. Once `status.resolvedBucketName` is set — the operator's own
record that a bucket once existed and a workload once held credentials for it — a bucket the
provider reports as absent is reported rather than created: phase `Failed`, `Ready=False` with
reason `BucketMissing`, a parallel `BucketPresent=False` condition and the gauge
`stackit_s3_provisioner_bucket_provisioned_missing`, and nothing is provisioned — no bucket, no
credentials group, no key
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D1, D2). The
security property is the report, not the refusal. Re-creating an empty bucket under the frozen name
made the fleet look healthy again, which is exactly the outcome somebody who deleted a bucket would
want: the operator destroyed the evidence of the deletion. Deletion of a provisioned bucket
is now detected, and `spec.allowRecreate` waives the refusal for one `Bucket` without waiving the
report (D9, and [H-23](#h-23-a-bucket-deleted-out-of-band-orphans-a-keyed-credentials-group) for
what the rebuild leaves behind).

A guard of that kind must not become a denial of service on the operator itself: a false "absent"
declares data loss on a working `Bucket` and pages somebody. So the distinction is structural
rather than a rule a caller has to remember. The question is a per-bucket control-plane read
(`BucketExists` in [stackit/client.go](../../stackit/client.go)), not a scan of the project-wide
listing the provisioning step, the read-grant check and the teardown still use, and only the
provider's own structured JSON 404 is read as absence (`isStructuredNotFound` in
[stackit/errors.go](../../stackit/errors.go)). A transport error, a 5xx, an empty body and a
gateway page carrying a 404 are returned as errors and stay a failure to reach the provider, which
holds `Ready` on an already-provisioned `Bucket` instead of reporting loss
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D3, D4,
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D1, D3). What an operator sees and
does about a vanished bucket is
[../operations/vanished-buckets.md](../operations/vanished-buckets.md).

## A credentials group is attributed through its bucket

A credentials group has no owner field and its display name is not unique in a project, so the group
cannot carry its own attribution. It is attributed through the bucket instead, in a fixed order, and
the group's display name takes no part in it at any point
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1–D3):

1. **The tags.** `credentials-group-id` names the group; its existence is probed by id through the
   keys endpoint, never through the project listing
   ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1). The rule
   comes out of an end-to-end run on 2026-09-03 that left one keyed group behind, which reads as a
   pass not seeing the group it had just created; no operator log survived that run, so the
   provider-side cause is inferred and not measured, and what is verified here is the rule, not a
   claim that a listing lags while the keys endpoint does not. `credentials-group-urn` supplies the
   policy principal without any listing at all. A tag naming a group that no longer exists reads as
   no tag.
2. **The policy.** A bucket without the tags has its group recovered from the single principal of
   its own `workload-objects-only` statement — a document the operator wrote with the admin key —
   and is tagged on the spot, reported as the event `CredentialsGroupAttributed`. A policy that
   cannot be *read* is an error that ends the pass, never "no policy": treating a failed read as
   absence would mint a second group for a bucket that already has one and rotate a working workload
   out of its credential.
3. **Neither.** A fresh group is created and tagged before any key is minted. If the tag write
   fails, the group is deleted again, so retries leave no trail of unreachable empty groups.

Step 3 is additionally guarded against the provider answering a read with state older than the
operator's own write: when the Bucket's status records a group id and that group still exists, a
bucket showing no attribution is read as stale rather than as unattributed, and the pass retries
instead of creating a second group
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D8). The recorded id
decides only *whether to wait*; it never names the group the bucket is bound to, which is why a
forged status can at most delay the forger's own Bucket.

Read grants resolve the same way, through the grantee's own bucket: the grantee's physical bucket
must carry the grantee's ownership tags, and then steps 1 and 2 apply to it
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3). A grantee
that fails either check is skipped with the event `ReadGrantPending` and never blocks the grantor.

### Why it reads this way

Until 2026-09-03 the workload group was located by a derived display name,
`s3op-<namespace>-<name>` truncated to 23 characters plus an 8-hex FNV-1a-32 suffix, with
find-or-create and no ownership check. Distinct namespace/name pairs collide under that scheme —
`("gitlab","gitlab-artifacts")` and `("gitlab-gitlab","artifacts787ngo")` both derive
`s3op-gitlab-gitlab-arti-70dbcfc2`, reproduced on 2026-08-24 — and a namespace admin who controls
the Bucket name can brute-force a 32-bit suffix offline, so a single `kubectl apply` made the
operator adopt a victim's group, delete its live key and mint a new one into the attacker's Secret.
That is why no lookup on this page uses a display name, why the ownership-tag check sits in front of
every tag read and tag write, and why the fix was a re-attribution rather than a better name: the
groups themselves were never renamed and no workload key was rotated during the migration. The
collision is held closed by `TestGroupAttributionSurvivesNameCollision`, and the migration of a
pre-existing bucket was verified against the real STACKIT API on 2026-09-03 by
`TestIntegrationGroupAttributionMigration`, which kept the bucket, the group id, the key id and the
Secret bytes.

## The one identity resolved by display name

`operator-admin`, the shared bootstrap credentials group, is found or created by its display name,
and it is the only group in the system that is
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D6). It belongs to
no Bucket, so there is no bucket to attribute it through and the name is the only handle available.
The exception does not extend to deletion in any form: a Bucket teardown never touches this group,
which is a stricter rule than the one workload groups get. It is also the reason two installations
must not share a project with default configuration — see
[H-20](#h-20-two-installations-sharing-one-project-are-indistinguishable-at-the-defaults).

## The status is never a source

`status.credentialsGroupID` and `status.credentialsGroupURN` are reported, not obeyed. Teardown
deletes only the group the bucket itself attributes; a group named only by the status is left
standing and reported as the warning event `CredentialsGroupNotAttributable`, with the recorded id in
the message so an operator can clean it up by hand
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D4). A teardown
whose second pass finds the bucket and group already gone stays silent, so the event means something
when it appears.

This is deliberate and it was the expensive option. Using the recorded id would have needed no tag
write, no policy read and no migration, and it lost because `buckets/status` is a subresource whose
write permission an RBAC binding can grant: the attribution would then have been safe only for as
long as nobody configured that binding. The shipped user-facing ClusterRoles grant no write on
`buckets/status` — verified in
[deploy/helm/stackit-s3-provisioner/templates/clusterrole-edit.yaml](../../deploy/helm/stackit-s3-provisioner/templates/clusterrole-edit.yaml),
which carries `buckets` only, and
[clusterrole-view.yaml](../../deploy/helm/stackit-s3-provisioner/templates/clusterrole-view.yaml),
which carries `get` on `buckets/status` — but a structural property must not rest on a cluster's RBAC
configuration staying that way.

The same reasoning rules out the CR's annotations, the Secret's contents and `status.accessKeyID` as
sources. All of them are writable or readable by somebody who is not the operator, and
`status.accessKeyID` combined with the rotation annotation would have been a takeover rather than a
nuisance.

The rule is about what may be treated as truth *about a cloud resource*, not about reading the
status at all. `status.resolvedBucketName` is read for three things: it resolves the physical name
([H-19](#h-19-an-empty-untagged-bucket-is-adopted-and-the-applier-can-choose-which-one)), it
addresses the measured bucket
([H-24](#h-24-size-measurement-addresses-a-bucket-without-re-checking-ownership)), and it is what
tells the operator that a provisioning round completed, which is why a bucket that vanished is
reported instead of re-created (see *The ownership tags, and what they prevent* above). None of the
three deletes or attributes anything on the strength of it, and on the provisioning path every tag
read and tag write in front of it still checks ownership, so a forged value parks its own `Bucket`
or squats a name — exactly the reach H-19 and H-24 already describe, and it needs the same
`buckets/status` write that the shipped ClusterRoles grant to nobody.

## Destruction is gated twice over

Teardown makes the emptiness decision before it destroys anything, and only then releases keys,
group, bucket and Secret in that order
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D3). Ownership is re-checked
inside that sequence rather than inherited from the provisioning pass:

- The **bucket** is deleted only if `bucketOwnedByUs` is true at that moment. An untagged bucket
  returns false here — unlike in adoption, there is no empty-bucket exception on the way out — so it
  is left standing with a warning event and the CR is still released.
- The **credentials group** is released only if the bucket attributes it. A bucket that attributes
  nothing, a bucket that is not this CR's, and an absent bucket all release nothing.
- A **wipe** needs three independent authorizations, every time: `spec.wipeOnDelete: true` on the
  CR, the operator-wide gate (`--enable-wipe-on-delete` / `ENABLE_WIPE_ON_DELETE` / Helm
  `wipeOnDelete.enabled`, `false` # default), and ownership tags that prove this operator provisioned
  this bucket for this CR. The ownership check is genuinely independent of the gate: it runs after
  the gate passes and can still refuse. Two out of three degrades to the plain emptiness guard with
  the warning event `WipeOnDeleteSkipped` naming which authorization was missing — never to a partial
  wipe and never to silence ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D4,
  D5, D7).

The practical consequence is that the worst outcome of a wrong or missing tag at teardown is an
orphan — a bucket or a group left behind, named in an event — and never a deletion of somebody
else's data. Operational guidance for a deletion that hangs is in
[../operations/deletion.md](../operations/deletion.md).

## Who can write these records

Every attribution write goes through the admin S3 credential. Workloads cannot forge one, and cannot
even read one: `s3:GetBucketTagging` and `s3:PutBucketTagging` are both absent from the workload's
and the reader's allowed-action lists in
[stackit/s3.go](../../stackit/s3.go), so the isolation policy's `NotAction` statements deny them.
Bucket-policy actions are denied to the workload for the same reason, which closes the other route:
a workload that could rewrite its own bucket policy could rewrite the `workload-objects-only`
principal and thereby forge the migration path of step 2 above
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D4).

There is a second, quieter guarantee in the policy builder. `BuildIsolationPolicy` filters the admin
URN and the bucket's own workload URN out of the reader list unconditionally, wherever that list came
from. An admin URN appearing as a reader would confine the operator's own key to read-only on that
bucket — including `PutBucketPolicy`, a lockout no later reconcile could repair, because there is no
control-plane route to a bucket policy. A workload URN appearing as a reader would add a second,
narrower `Deny` on the bucket's own owner, and `Deny` does not yield to another statement.

## Open gaps

### H-19 An empty untagged bucket is adopted, and the applier can choose which one

**Mechanism.** Two rules meet. `adoptOrCollide` adopts a pre-existing bucket that carries no tags
when it is empty, which is what makes a crash between bucket creation and the tag write recoverable.
And `decideBucketName` resolves the physical name in four branches: `status.resolvedBucketName`
first, then the `stackit-bucket.gtrfc.com/resolved-bucket-name` annotation, then — for a bucket
provisioned before the naming feature, recognised by `status.bucketURL` being set — the raw
`spec.bucketName` ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md)
D4), and only then a freshly composed name. A newly created Bucket has neither status field, so an
annotation set at creation time decides on the second branch and the CR addresses whatever name it
names. The annotation is ordinary metadata: the shipped `-edit` aggregation grants `create`,
`patch`, `update`, `delete` and `deletecollection` on `buckets`, nothing in the CRD constrains the
annotation, and a name taken from it is not even passed through `ValidateBucketName`, which runs only
on a freshly composed name.

**Adversary and reach.** Anybody who may create a Bucket in any namespace. They can aim it at an
empty, untagged bucket anywhere in the STACKIT project, including one another installation has just
created and not yet tagged, and the operator will adopt it, stamp its own tags on it, issue
credentials for it, and delete it when the CR is deleted. They cannot reach a bucket that carries
ownership tags — foreign tags are a collision — and they cannot reach a bucket that holds data, since
an untagged non-empty bucket is refused. The reachable set is therefore empty buckets and name
squatting, not data.

**Live today, and it is a place where the code is narrower than the reasoning.**
[ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) rejects putting the
name prefix in the Bucket spec precisely so that a namespace user cannot compose a name belonging to
another team and adopt an untagged bucket under it; the freeze annotation is metadata rather than
spec and reaches the same input through the same role. Verified by reading `decideBucketName`,
`adoptOrCollide` and the two shipped ClusterRoles on 2026-09-19. Not verified: no test and no live
run exercises a pre-set annotation, so the reach above is derived from the code, not measured.

**What an operator can do meanwhile.** Do not leave empty, untagged buckets in a project an operator
manages. Give each installation a distinct `ownership.name`, so a bucket adopted this way at least
cannot be confused with another installation's afterwards. Watch for `Ready=Failed` with an ownership
collision message, which is what a *failed* attempt looks like, and treat an unexplained
`resolved-bucket-name` annotation on a newly created Bucket as what it is.

### H-20 Two installations sharing one project are indistinguishable at the defaults

**Mechanism.** `managed-by` defaults to the literal `stackit-s3-provisioner` and `owner` is
`<namespace>/<name>`. Two clusters that each hold a Bucket named `registry` in a namespace named
`harbor`, both installed with defaults, compose the same physical name and each read the other's
bucket as their own. Both then manage one bucket and one credentials group, and the first of them to find
its own workload Secret empty clears every key in that shared group before minting its own — which
kills the other installation's live credential, silently, because that installation's Secret still
carries the dead key and its skip condition is satisfied by the replacement key's existence. The shared `operator-admin` group makes it
worse rather than better: it is resolved by display name, is the same name in both installations, and
a re-bootstrap clears every key in it before minting a new one
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D1, D5) — so one
installation re-bootstrapping takes the other's admin key out with it.

**Adversary.** None needed; two well-meaning installations suffice. It is live today, and neither
installation logs anything that identifies the other.

**What an operator can do.** Give every installation sharing a project a distinct
`bucketNaming.prefix`, a distinct `ownership.name`, or both. Not verified: the concurrent-management
behaviour is derived from the ownership comparison and the bootstrap path, not observed in a live
two-cluster setup.

### H-21 Changing the ownership identity after buckets exist makes the operator's own buckets foreign

**Mechanism.** `ownership.name` / `--ownership-name` is half of the ownership key. Change it on a
running installation and every existing bucket fails `isOwnedByUs`: provisioning parks each Bucket
with an ownership collision, credentials-group attribution refuses with `errBucketNotOwned`, and
teardown declines to delete the bucket or release its group. Nothing warns at install or upgrade
time; the first Bucket to reconcile is the alarm.

**Adversary.** None — this is a configuration hazard with no guard, and it is live today. The same
trap is a disaster-recovery requirement in the other direction: a restored installation must set the
same value or it will read its own surviving buckets as foreign
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D6).

**What an operator can do.** Treat `ownership.name` as immutable for the life of an installation, and
record it with the backup alongside the naming policy. Recovering from an accidental change means
re-tagging the affected buckets by hand with the admin credential, or changing the value back.
Operational detail is in [../operations/bucket-naming.md](../operations/bucket-naming.md).

### H-22 Every check on this page rests on the admin credential

**Mechanism.** The tags are written and read with the operator's admin S3 key, and so is the
isolation policy that serves as the migration path. Whoever holds that key can write any tag on any
bucket in the project and can rewrite any bucket policy, which means they can make the operator
attribute any bucket to any Bucket CR, or make it refuse its own.

**Adversary.** Anyone who obtains the admin credential — from the operator's namespace Secret, from
the operator process, or from the STACKIT project directly. It is live today by construction: there
is no second, independent record to cross-check a tag against, and the operator does not detect a tag
it did not write.

**What an operator can do.** Everything protective here is about the credential rather than the
attribution: restrict read access to the operator namespace, keep the service account's roles at
project level only ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D5), and
treat the admin Secret as the highest-value object in the cluster. That credential's own storage,
blast radius and rotation are covered in
[credentials-and-secrets.md](credentials-and-secrets.md).

### H-23 A bucket deleted out of band orphans a keyed credentials group

**Mechanism.** Attribution hangs on the bucket, so a bucket that goes away takes the only proof of
its group's attribution with it. The group survives with a live access key, and before attribution
moved to the bucket the display name would have found it; now nothing does.

**Narrowed, and this is where the remaining cases are.** Until
[ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) the orphan was made
by the operator's own repair: deleting the bucket in the console made the next reconcile create a
fresh one with no tags and no policy, therefore a fresh credentials group, and the Secret in the
cluster was rewritten to the new one — silently, with the CR back in `Ready`. That repair is gone
(D1): the deletion is reported and nothing is provisioned, so the default path leaves no orphan
behind. Three paths still produce one, and each of them is a deliberate act rather than a silent
repair. `spec.allowRecreate` mints a fresh group and key on every unattended rebuild and leaves
the previous group with its live key, so a `Bucket` that keeps losing its bucket accumulates one
orphan per rebuild ([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
D10). Deleting the CR and re-applying it — the supported way to re-create once — does the same,
once (D8). And deleting a `Bucket` whose bucket is already absent completes rather than hanging,
releasing nothing, because nothing attributes the group (*Destruction is gated twice over* above).

**Adversary and reach.** Anyone with console or API access to the project, which is above the
operator's boundary — but also an ordinary operational mistake. The orphaned key is not dangerous
on operator-managed buckets, because every one of them denies all principals but its own
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D3), and it cannot
reach the bucket that replaced its own: that is a new bucket with a new policy naming a new group.
It is a live credential nobody is tracking, and it would have access to any bucket in the project
that is *not* operator-managed. Live today, and the accumulation under `spec.allowRecreate` is the
half that grows unattended.

**What an operator can do.** Do not delete operator-managed buckets out of band. Each orphan is now
announced as it is made — the Warning event `BucketRecreated` names the previous credentials group
id, and the teardown case reports `CredentialsGroupNotAttributable` with the recorded id — so the
cleanup is manual but no longer unnoticed; a deployment using `spec.allowRecreate` should sweep on
that event rather than on a schedule. When it has happened, the orphan is findable by its display
name — which is exactly what the display name is kept for
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D7) — and
[hack/e2ecleanup](../../hack/e2ecleanup) sweeps this shape of leftover for end-to-end runs,
matching workload groups by their `s3op-<prefix>` display name. Verified on 2026-09-19 by reading
`guardBucketPresent`, `reportAuthorizedRecreate` and `teardown` in
[internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go); not
verified against a live provider, where no bucket has been deleted out of band and watched.

### H-24 Size measurement addresses a bucket without re-checking ownership

**Mechanism.** The measuring controller in
[internal/controller/bucket_usage_controller.go](../../internal/controller/bucket_usage_controller.go)
takes the bucket name from `status.resolvedBucketName`, gates on the Bucket being `Ready`, and lists
the bucket with the admin key. It does not re-read the ownership tags the way provisioning and
teardown do. A forged `status.resolvedBucketName` on a Bucket that is already `Ready` would therefore
have the operator list a bucket that is not that CR's and publish its object count and byte size into
`status.usage`.

**Adversary.** Two preconditions, not one. The forged `status.resolvedBucketName` needs write access
to the `buckets/status` subresource, which the shipped ClusterRoles grant to nobody (see *The status
is never a source* above). Measurement also has to be on for that Bucket, and since
`bucketUsage.defaultEnabled` ships `false` # default that means `spec.usage.enabled: true` on the CR —
an ordinary spec write, which the shipped `-edit` aggregation does grant. The measurement is
read-only — it lists, it does not touch objects — so the exposure is object count, total bytes and the
derived cost figure for a bucket in the same project. Live today, conditional on a non-shipped RBAC
binding. Verified by reading the controller on 2026-09-19; not verified by exercising it, and no test
covers the forged-status case.

**What an operator can do.** Do not bind write on `buckets/status` to anyone but the operator's
service account. Only one of the two measurement switches ships off: `bucketUsage.defaultEnabled` is
`false` # default, so a Bucket is measured only if it asks — but the operator-wide hard gate
`bucketUsage.enabled` ships `true` # default, so an installation that wants nothing measured at all
must close that gate explicitly, which stops every measurement whatever any CR asks for
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D10).

## What this does not cover

- **What the tags protect once ownership is settled.** Attribution decides *which* bucket the
  operator acts on; the bucket policy decides who may read and write its contents. Statement shapes,
  the deny-first construction and what it leaves open are in
  [tenancy-and-isolation.md](tenancy-and-isolation.md) and
  [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md).
- **The credentials themselves.** Where the admin key and each workload key live, who can read them,
  what rotation does and what a leak costs are in
  [credentials-and-secrets.md](credentials-and-secrets.md),
  [ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) and
  [ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md).
- **What a cluster role hands over.** Which Kubernetes permissions let somebody create a Bucket at
  all, and what that delegates into the cloud, are in
  [rbac-and-privilege.md](rbac-and-privilege.md) and
  [ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md).
- **Cross-project separation.** Nothing on this page defends a second STACKIT project. That
  separation is a property of the service-account credential and its project-scoped roles, and the
  operator neither enforces it nor compensates for a cascading role
  ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D5).
- **How the mechanisms are implemented.** The resolution order in code, the lag sentinel, the
  listing index and the migration walkthrough are in
  [../developer/bucket-identity.md](../developer/bucket-identity.md); how the name is composed and
  frozen, and what a restore must reproduce, are in
  [../operations/bucket-naming.md](../operations/bucket-naming.md).
- **Anything about a bucket the operator never provisioned.** A bucket in the project with no
  operator-written policy is outside every mechanism described here; the operator does not see it,
  does not protect it, and — apart from the empty untagged case of
  [H-19](#h-19-an-empty-untagged-bucket-is-adopted-and-the-applier-can-choose-which-one) — does not
  touch it.
