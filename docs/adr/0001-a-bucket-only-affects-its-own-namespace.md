# ADR 0001: A Bucket Only Affects Its Own Namespace

## Status

Accepted. Date: 2026-09-03. Amended 2026-09-03 by
[ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md): D2 now holds for
credentials groups as well, and the conditional consequence on user-facing write RBAC is
withdrawn.

D1 through D6 hold in the tree at this commit. D4's first consequence — the removal of
`spec.secretRef.namespace` — shipped in commit `2fdb722`
(`fix: keep the workload credentials Secret in the Bucket namespace`). D2 held for buckets
only until ADR 0002 attributed credentials groups through the bucket's tags; the former
violation (security finding 1 in [CLAUDE.md](../../CLAUDE.md)) is kept below, marked closed.

Verified by reading the code and by the offline unit suite plus the envtest suite:
`TestBucketsForSecret` and `TestIsAdminSecret` pin D1, the `reconciler_grants_test.go`
cases and the envtest `TestGrantReadAccess_*` tests pin D3's refusal of a foreign
namespace, `TestBucketDeepCopy`/`TestDeepCopyRoundTrips` compile only without the removed
field. Not verified: whether any Bucket on a live cluster still sets the removed field,
and how Flux server-side apply treats one that does (see Residual risks).

## Context

`Bucket` is a namespaced kind, and the tenancy model of every cluster this operator runs on
treats the namespace as the unit of trust: a team may create Buckets in its own namespace
and nowhere else. The isolation policy, the read grants and the deletion guards all
quietly assume that whatever a Bucket does stays inside that namespace. The assumption
was written into doc comments but never stated as a rule, and one field broke it.

`spec.secretRef.namespace` — optional, absent from the README, defaulting to the CR
namespace — let a Bucket direct its credentials Secret at **any** namespace. The only guard
was `isAdminSecret`, a single name+namespace pair protecting the operator's own admin
Secret. `upsertSecret` was a `CreateOrUpdate` merge: an existing Secret in the target
namespace kept its unrelated keys but lost every key with a colliding name, had its type
forced to `Opaque` and received the managed-by label; no owner reference can be set across
namespaces, so nothing tied it to the Bucket. `deleteSecret` removed the whole Secret when
the CR was deleted. Net effect: anyone allowed to create a Bucket anywhere held a
create/merge/delete primitive on Secrets in every namespace but one. Recorded as security
finding 2 in CLAUDE.md on 2026-08-24.

The review of the user-facing RBAC ticket (aggregating an `edit` ClusterRole on `buckets`
into the built-in `edit`/`admin`) made the cost concrete: with that field present, every
namespace admin on the cluster would have become a cluster-wide Secret writer. No Bucket
in the Flux repositories used the field (checked 2026-09-03 in local checkouts dated
2026-08-27 — an indication, not proof).

The decision is recorded as the general rule rather than as the fix, so that the next
field is measured against it before it is added.

## Decision

**D1 — Every cluster object a Bucket creates, changes or deletes lives in the Bucket's
namespace.** Today that is one object: the workload credentials Secret named by
`spec.secretRef.name`. `upsertSecret` writes it into `b.Namespace` and always sets the
Bucket as controller owner, `deleteSecret` removes it there, and the Secret watch
(`bucketsForSecret`) lists only the Buckets in the namespace of the changed Secret.

**D2 — Every cloud object a Bucket creates is attributed to exactly that Bucket by
`namespace/name`, and a Bucket never adopts a cloud object attributed to another Bucket.**
For the bucket itself this holds: `ensureBucket` stamps ownership tags (`ownershipTags`,
managed-by plus owner) on a bucket it creates, and a bucket carrying different tags is an
`ownershipCollisionError` — a definitive failure without requeue. For the credentials
group it holds since ADR 0002: the bucket names its group in the tag `credentials-group-id`
(`resolveWorkloadGroup`), and a group is never found, adopted or deleted by its display
name. *Superseded 2026-09-03:* until then `EnsureCredentialsGroup` was find-or-create by the
derived display name `workloadGroupName`, with no ownership check, and that name is not
collision-proof across namespaces — see Residual risks.

**D3 — A reference in a Bucket spec resolves in the Bucket's namespace only.**
`spec.grantReadAccess` names Bucket CRs and `resolveReadGrants` looks them up with
`{Namespace: b.Namespace}`; `spec.cloneFrom.secretRef` names a Secret in the Bucket's
namespace. Neither reference carries a namespace, and a same-named object elsewhere is a
different object.

**D4 — No spec field may direct any of D1–D3 elsewhere.** No `namespace` on a reference, no
target-namespace selector, no annotation that widens the scope. `spec.secretRef.namespace`
is removed rather than validated to equal the CR namespace: a field that can only ever
hold one legal value documents nothing and invites its own re-enabling.

**D5 — Objects the operator creates for itself live in the operator namespace and are not
the Bucket's effect.** The admin Secret, the clone Job and its staging Secret are created
in `AdminSecretNamespace` — the operator's own namespace — under operator-derived names
(see `clone.go`). A tenant's Bucket triggers them but neither names nor places them. The
one way a Bucket could reach into that namespace, a Bucket in the operator namespace
naming the admin Secret, is refused by `specGuardError` through `isAdminSecret`.

**D6 — D1–D5 confine the Bucket, not the operator.** The operator's ClusterRole keeps
cluster-wide Secret write, because it must write into every tenant namespace. The
namespace is a trust boundary for tenants, not for the operator's own credential.

## Consequences

* Consuming a bucket's credentials from another namespace is not the Bucket's job. A
  consumer creates its own Bucket and is granted read access through
  `spec.grantReadAccess` on the data bucket, declared in the data bucket's namespace — or
  the Secret is copied by tooling outside this operator, under that tooling's rules.
* Removing a CRD field is an API break, shipped deliberately as `fix:` (patch): the field
  was undocumented and its only distinct use was the primitive this ADR forbids. A Bucket
  that still sets it is not rejected but pruned by the API server; `kubectl` with its
  default strict field validation (1.27 and later) rejects it at apply time.
* Migration of a Bucket that used the field: after the CRD upgrade the field is invisible,
  the operator finds no credentials in the (new) Secret in the CR namespace, clears the
  group's keys, mints a new key and writes it there. The workload that read the foreign
  Secret now holds a dead key, and the foreign Secret is orphaned — no owner reference —
  and must be deleted by hand. Looking for such Buckets has to happen **before** the CRD
  upgrade; afterwards the field can no longer be read.
* The Secret watch got cheaper: one namespace-scoped list per Secret event instead of a
  cluster-wide one.
* User-facing write RBAC on `buckets` hands out precisely the power this ADR bounds.
  *Amended 2026-09-03 (ADR 0002):* the condition that kept an `edit` ClusterRole on
  `buckets` out of the built-in `edit`/`admin` aggregation — D2 violated for credentials
  groups — no longer holds. What the role hands out is namespace-scoped (D1–D5); whether to
  aggregate it is a privilege decision of the RBAC ticket, not a security precondition.
  *Superseded rule:* while D2 was violated, the role was shipped for explicit per-namespace
  binding only and not aggregated by default.

## Alternatives Considered

### Keep the field and validate it equals the CR namespace (CEL rule)

Rejected. A field with exactly one legal value documents nothing and invites the
validation to be loosened "for one case". Removing it makes the rule structural: there is
nothing to loosen.

### Keep the field and require an opt-in on the target namespace

An annotation or label on the receiving namespace, as Secret-replication tools do.
Rejected: it adds a second trust mechanism to an operator whose job is provisioning, and
cross-namespace consumption already has a model in D3 (read grants on the data bucket).

### Keep the field and check the creator's permission on the target namespace

A SubjectAccessReview for whoever created the Bucket. Rejected: the creator is not known
at reconcile time — Buckets arrive through GitOps under a shared applier identity, and
reconciles are level-triggered and repeat long after creation.

### Make Bucket cluster-scoped

Rejected. The namespace is the tenancy unit the whole model rests on; cluster scope would
move every boundary into RBAC on object names.

## Residual risks

* **D2 was violated for credentials groups (closed 2026-09-03 by ADR 0002; was security
  finding 1 in CLAUDE.md).** Kept for the record:
  `workloadGroupName` is `s3op-<namespace>-<name>`, truncated, plus an 8-hex FNV-1a-32
  suffix; CLAUDE.md records `("gitlab","gitlab-artifacts")` and
  `("gitlab-gitlab","artifacts787ngo")` deriving the same name (reproduced 2026-08-24). A
  colliding Bucket adopts the victim's group, `ensureAccessKeyAndSecret` deletes the
  victim's live key and mints a new one in that group, and the victim's bucket policy
  trusts that group's URN — a credential takeover of a foreign bucket plus an outage for
  its workload. ADR 0002 closed this by attributing the group through the bucket's tags,
  with the bucket's own policy as the migration path — no group was renamed and no key
  rotated.
* **Not verified:** whether Flux server-side apply rejects or silently prunes a Bucket that
  still sets `spec.secretRef.namespace`; whether any Bucket on the target clusters sets it.
* Read access is bounded by the same rule from the other side: `get`/`list` on `buckets`
  exposes ids, URLs, sizes and conditions, no credential material — `status.accessKeyID`
  is the key id only, the secret is only ever stored in the Secret.

## References

* [`api/v1/bucket_types.go`](../../api/v1/bucket_types.go) — `SecretReference` (name only), `LocalBucketReference`, `CloneSourceSecretRef`
* [`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go) — `upsertSecret`, `deleteSecret`, `bucketsForSecret`, `isAdminSecret`, `specGuardError`, `ensureBucket`, `ownershipTags`, `resolveReadGrants`, `workloadGroupName`
* [`internal/controller/clone.go`](../../internal/controller/clone.go) — clone Job and staging Secret in the operator namespace
* [`stackit/client.go`](../../stackit/client.go) — `EnsureCredentialsGroup`, find-or-create by display name (admin group only since ADR 0002)
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — credentials-group attribution, amends D2 and the RBAC consequence
* [INIT-SETUP.md](../../INIT-SETUP.md) — §0 decision table, §4.1.1 read grants and the group-name collision
* [CLAUDE.md](../../CLAUDE.md) — security findings 1 (open) and 2 (closed by this ADR)
