# ADR 0008: A Read Grant Is Declared by the Bucket That Owns the Data

## Status

Accepted. Date: 2026-08-24. Amended 2026-09-03 (D3, after [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md)).

Implemented: `spec.grantReadAccess` on the granting Bucket, the third policy statement
`granted-readers-read-only`, the reader exemption in the blanket deny, `status.grantedReadTo`,
the `ReadGrantPending` event, the grantee watch, and the hold while a clone is still filling the
bucket. Verified offline (grant applied, pending, late-arriving grantee, revocation, namespace
scoping, self-reference, admin never granted, several grantees, stable document, duplicate display
names, hold during clone), against the real provider for the three-statement document under
StorageGRID's own evaluation (owner keeps write access, reader lists and gets, reader cannot write
or delete, ungranted principal locked out entirely), and end to end through the operator against the
real API, including a grantee that appears late and a grant removed from the spec. Not verified: the
dates of those two cloud runs — both suites exist and are recorded as green, but no run date
survives in the sources. Open: nothing in the grant path is rate-limited or batched — a grantor pays its
per-grantee provider reads on every pass (see Consequences), and the list is capped at 32 entries
rather than sized from measurement.

## Context

Until 2026-08-24 a workload credential could touch exactly one bucket: its own. That is the point
of the isolation document ([ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md)),
and it had no exception at all.

The concrete failure, verified on the mgmt-d cluster on 2026-08-23: the GitLab backup job in
namespace `gitlab` runs with the credentials of the `gitlab-backups` bucket and received
`403 AccessDenied` on every sibling data bucket — artifacts, uploads, lfs, packages, mr-diffs,
terraform-state, ci-secure-files, registry and pages, nine buckets of the same namespace and the
same owner. The nightly backup therefore skipped every object-storage component and covered only
the Gitaly repositories. There was no second copy of the object-storage data at all. The isolation
that made the product worth deploying had made its own backup impossible.

So an exception had to exist, and the only open question was who declares it. Two properties of the
system decide that. First, access to a bucket is enforced by exactly one artefact — the bucket
policy of the bucket being read — and that document is written by the reconcile of the Bucket that
owns it; a consumer-declared grant would have to reach across into a document it does not own.
Second, a namespace user who may create a Bucket controls the whole spec of that Bucket. If a
Bucket could name the buckets it wants to *read*, creating one CR would be enough to read a
sibling's data, and no reviewer of the data bucket would ever see it. Declaring on the producer
side inverts both: the grant is written in the spec of the bucket whose data is at stake, so its
full access list is visible where the data is, and the operator only ever widens a document at the
request of that document's owner.

What the reader principal is resolved *from* turned out to be the harder half. The first
implementation resolved a grantee to its credentials group by the group's display name, derived
from namespace and Bucket name. On 2026-08-24 that name was shown to collide across namespaces —
`("gitlab", "gitlab-artifacts")` and `("gitlab-gitlab", "artifacts787ngo")` derive the same name —
and the whole attribution was re-based on the bucket itself on 2026-09-03
([ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md)). The obvious cheap
resolution was cheaper still and equally wrong: reading the grantee's published
`status.credentialsGroupURN`. Writing a Bucket's status is a weaker permission than reading the
workload Secrets it describes, so a forged value would be pasted verbatim into someone else's
bucket policy — naming the operator's own admin principal there confines the admin credential to
read-only on that bucket, including the ability to rewrite the policy, which is an unrepairable
lockout.

## Decision

**D1 — A read grant is declared by the Bucket that owns the data.** `spec.grantReadAccess` lists
the Bucket CRs whose workload credentials additionally receive read-only access to *this* bucket.
No Bucket can widen its own access to another bucket, and a bucket's complete access list is
readable in its own spec. The list holds at most 32 entries.

**D2 — An entry names a Bucket CR and resolves in the grantor's own namespace only.** A Bucket in
another namespace cannot be named, and a same-named Bucket elsewhere in the cluster is a different
Bucket that is neither resolved nor woken ([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) D3).
A Bucket referencing itself is rejected by the CRD schema, and ignored if it ever reaches the
reconcile.

**D3 — The reader principal is resolved through the grantee's own bucket, never from anything a
namespace user can write.** The grantee CR's physical bucket must carry that grantee's ownership
tags, and the credentials group is the one that bucket attributes: the `credentials-group-id` tag,
or, for a bucket provisioned before that tag existed, the principal of its own `workload-objects-only`
statement ([ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) D5/D6). Status,
annotations, Secret contents and group display names take no part in the resolution. Display names
are not unique in the project, so there is no ambiguity rule and no name lookup to be ambiguous
about.

**D4 — Resolving a grant creates nothing.** No credentials group, no bucket, no access key is ever
created on a grantee's behalf while a grantor reconciles. Completing the grantee's own attribution
record is permitted — reading its bucket may write back the `credentials-group-id` and
`credentials-group-urn` tags it was missing — because that record is written with the admin
credential and names a group that already exists.

**D5 — A grant that does not resolve is skipped and never blocks the grantor.** The grantor still
becomes `Ready`; every entry that does not resolve — a self-reference included — raises the warning
event `ReadGrantPending` naming the reference and the reason. The grantor owns the data and must
not lose its own provisioning because a consumer is missing.

| The reference | What happens |
| --- | --- |
| Names no existing Bucket CR in the namespace | Skipped, `ReadGrantPending`; applied automatically once it appears |
| Names a Bucket that has started deleting | Skipped, `ReadGrantPending`; access revoked early rather than left dangling |
| Names a Bucket whose bucket does not exist yet | Skipped, `ReadGrantPending` |
| Names a Bucket that attributes no credentials group yet | Skipped, `ReadGrantPending` |
| Names a Bucket whose physical bucket is not that Bucket's | Skipped, `ReadGrantPending`, naming the bucket |
| Names the grantor itself | Ignored with `ReadGrantPending`; absent from `status.grantedReadTo` |
| Names a Bucket whose `credentials-group-id` tag points at a group the provider has not yet made consistently visible | **Not** skipped: the grantor's reconcile fails and retries |

The table is the closed list of skips. Anything else that goes wrong while resolving fails the
grantor's reconcile and is classified by [ADR 0012](0012-ready-describes-the-last-verified-state.md):
a provider error, a failed read of the grantee CR, and the last row above — a grantee tagged before
the `credentials-group-urn` tag existed, whose group the provider confirms by id but does not yet
return in a listing. That state is momentary and self-clearing, and it is the one case in which a
grantor can lose a pass to a grantee's condition.

**D6 — A reader may read and may do nothing else, and the action set is fixed.** It is a strict
read-only subset of what the bucket's own workload may do; an action qualifies only if it can
neither create nor change nor destroy state — not object data, not metadata, not tags, not
versions, not bucket configuration.

| Granted to a reader | Withheld from a reader |
| --- | --- |
| `s3:GetObject`, `s3:GetObjectVersion` | every `s3:Put*`, `s3:Delete*` and `s3:Create*` action |
| `s3:ListBucket`, `s3:ListBucketVersions` | `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts` |
| `s3:GetObjectTagging`, `s3:GetObjectVersionTagging` | `s3:AbortMultipartUpload` |
| `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration` | policy, replication, notification, lifecycle and object-lock configuration |
| `s3:GetBucketLocation` | anything at all on a bucket that did not grant it |

Multipart listing is withheld on purpose: it exposes the *owner's* not-yet-committed uploads — key
names and part layout — and aborting one destroys them. Reading finished objects never needs it.
`s3:GetBucketLocation` is included because SigV4 clients resolve the region before their first
request and fail confusingly without it; it discloses nothing beyond the region the reader already
addresses.

**D7 — A reader is exempted from the blanket deny and then confined by its own statement.** The
document gains a third statement, `granted-readers-read-only`, and every reader is added to the
`NotPrincipal` exemption of `deny-all-except-admin-and-workload` — without that exemption the
blanket deny locks the reader out no matter what the third statement says. The admin principal and
the bucket's own workload principal are filtered out of the reader list before the document is
built: the admin as reader is the unrepairable lockout of the Context, and the workload as reader
is a second, narrower deny on the owner itself, which — denies intersect — silently strips the
owner of its write access. The reader list is deduplicated and sorted, so the document is
deterministic and drift comparison reports no phantom change.

**D8 — Without a grant the document is byte-identical to a bucket that never used the feature.**
The third statement and the exemption exist only when at least one reader resolves. Upgrading the
operator therefore rewrites no existing policy.

**D9 — Revocation needs no cleanup step and is published.** Removing an entry from
`spec.grantReadAccess`, deleting the grantee, or the grantee entering deletion each drop the reader
from the document on the grantor's next reconcile; the grant lives in exactly one place, so there
is nothing else to undo. What is in effect right now is `status.grantedReadTo`, which lists the
entries that resolved — a pending or revoked grant is visible there without reading the policy out
of S3.

**D10 — A bucket still being filled by a clone shares nothing.** While
[`spec.cloneFrom`](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) is still copying,
readers stay out of the policy and are written in the same pass the copy succeeds. The bucket's own
workload is held back by `holdSecretUntilCloned`, which cannot help here: a granted reader already
holds working credentials of its own, so the policy is the only thing that can hold it back.

**D11 — Grantors are woken by grantee events, on a deliberately narrow condition.** A grantee
appearing, disappearing, starting to delete, or changing its published credentials-group identity
wakes every Bucket of that namespace that names it; nothing else does. A grantee publishes
`status.credentialsGroupURN` as soon as its group exists rather than when it becomes `Ready`,
because a grantee that is itself still cloning would otherwise never wake its grantors.

## Consequences

* **The grantor pays for its grants on every pass.** Each entry costs a bucket-existence check plus
  the grantee's attribution reads against the provider, every reconcile, whether or not anything
  changed. Thirty-two entries is a lot of provider traffic for one Bucket, and the traffic is what
  the fleet-wide circuit breaker
  ([ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md)) has to absorb during an outage.
* **A grant is only as strong as the grantee's attribution.** Everything D3 rests on is the
  attribution chain of ADR 0002; a grantee whose bucket the operator cannot prove is its own grants
  nothing, which is correct but shows up as a persistently pending grant rather than a hard failure.
* **A skipped grant is quiet by design.** The grantor is `Ready` while a consumer sits without
  access. `status.grantedReadTo` and the `ReadGrantPending` events are the only signal, and neither
  is an alert.
* **The reader's credential widens.** A reader's single credential now opens its own bucket plus
  every bucket that granted to it, so leaking it costs more than it did. This is the price of not
  minting a second credential per grant, and it is the reason the granted action set is a strict
  read-only subset rather than "whatever the reader needs".
* **A mutual grant is fine but not free.** Two Buckets granting to each other resolve normally; the
  narrow wake condition of D11 is what stops them waking one another forever over ordinary status
  writes.
* **Nothing about the feature is visible on the grantee.** A Bucket cannot tell from its own CR
  which foreign buckets it may read. The information exists only on the grantors, which is the
  direct cost of D1.
* **Upgrades are inert.** D8 means an operator upgrade touches no policy of a bucket without
  grants, so the feature cannot regress a bucket that does not use it.

## Alternatives Considered

**The consumer declares what it wants to read.** Rejected. It is the shape everyone reaches for
first — the backup job's own Bucket says "I need to read these" — and it hands every namespace user
who may create a Bucket the ability to read any sibling's data by writing their own CR, with no
trace in the spec of the bucket being read. The producer side also happens to be where the policy
is written anyway, so the consumer form would need a second, cross-object write path for no benefit.

**Read the grantee's `status.credentialsGroupURN`.** Rejected, and it was the cheapest option on
the table: one read from the API-server cache, no provider call at all, no dependency on the
attribution chain. It lost because status is writable by a strictly weaker permission than reading
the workload Secrets, so the value is forgeable, and a forged principal is copied verbatim into the
grantor's policy — one forged entry naming the operator's admin group takes the bucket away from
the operator permanently.

**Resolve the grantee by its credentials-group display name.** Taken on 2026-08-24, replaced on
2026-09-03. The name derives from namespace and Bucket name, and colliding namespace/name pairs
derive the same name, so a grant could point at a foreign namespace's group. Display names are not
unique in the project either, which forced an ambiguity rule that no longer exists.

**Mint a dedicated read-only credential per grant.** Rejected. Every grant would add a credentials
group and a Secret to keep in step with the grantor's spec, the reader's workload would need to be
reconfigured with a different credential per bucket it reads, and revocation would become a
deletion path with its own failure modes instead of a policy rewrite. Widening the credential the
reader already has costs one statement.

**Grant across namespaces.** Rejected. A namespace-qualified reference is exactly the spec field
[ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) D3/D4 forbids: it would let whoever may
create a Bucket resolve a reference into a namespace they have no rights in.

**Express the grant as an `Allow` statement.** Rejected. Default access inside the project is open,
so isolation is built from denies ([ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md));
an `Allow` added to that document grants nothing, because the blanket deny of statement 1 still
excludes the reader. The reader has to be exempted there and confined by a deny of its own.

**Give readers multipart listing so their tooling matches the owner's.** Rejected. It exposes the
owner's in-flight uploads and, with abort, destroys them; `ls` and `get` never need it, and the
common clients only issue those calls for uploads they started themselves.

**Hold readers out of the policy until the grantor is fully `Ready`, rather than only during a
clone.** Rejected as too broad: a grantor degraded by a provider problem would silently revoke
working readers. The hold is scoped to the one state where the data itself is incomplete.

## Residual risks

* **A grant is as forgeable as the grantee's bucket ownership.** D3 removes every namespace-writable
  source, but if an attacker could ever make a bucket carry another Bucket's ownership tags, the
  grant would follow. That is the attribution surface of ADR 0002, not of this record.
* **A persistently pending grant looks like a working one from the grantor's phase.** `Ready` is
  green, the data is simply not shared. Operators have to read `status.grantedReadTo`.
* **The reader action set is not enforced by the type system.** Nothing in the build prevents a
  write action from being added to the reader list; an offline test is what holds the read-only
  subset property, and a reviewer removing that test would remove the guarantee.
* **Per-pass provider cost grows linearly with the number of grants** and has no cache. With many
  grantors in one namespace, the same grantee bucket is read once per grantor per pass.
* **Not verified: the 32-entry cap.** It is a schema limit chosen as a bound, not a measured
  ceiling; no run has established what a grantor with 32 entries costs per pass.
* **Not verified: the deliberation behind the producer-side choice.** No contemporaneous record of
  the consumer-side option being weighed survives; the reason stated under Alternatives is derived
  from rules that are in force today, not quoted from a decision made on 2026-08-24.
* **Not verified: revocation latency in the absence of a watch event.** The grantee watch covers
  create, delete, deletion start and a change of published group identity; any other path to
  revocation waits for the periodic drift resync, and no measurement of that worst case exists.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — D3/D4, why a reference resolves in
  one namespace and why no field may point elsewhere
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — D5/D6, the attribution
  chain the reader principal is resolved through
* [ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the two-statement document
  this decision adds a third statement to
* [ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — how a Bucket CR maps to
  the physical bucket a grant is resolved through
* [ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) — the clone whose progress
  holds readers out of the policy
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — how a provider error during grant
  resolution is classified
* [docs/operations/read-grants.md](../operations/read-grants.md) — sharing a bucket read-only, and
  reading a pending grant
* [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md) — what a grant does
  and does not open
* [docs/security/ownership-and-attribution.md](../security/ownership-and-attribution.md) — the
  forgery surface behind D3
* [docs/developer/bucket-policy.md](../developer/bucket-policy.md) — how the document is built,
  compared and kept deterministic
* [docs/developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md) — where grant
  resolution sits in the pass, and the watches that drive it
