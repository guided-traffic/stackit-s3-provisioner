# ADR 0001: A Bucket Only Affects Its Own Namespace

## Status

Accepted. Date: 2026-09-03.

Amended 2026-09-03 by [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md):
D2 now holds for credentials groups as well, and the conditional consequence on user-facing
write RBAC is withdrawn. Amended 2026-09-03 again when the user-facing ClusterRoles shipped:
the RBAC consequence below records why `<release>-edit` exists only as an aggregation fragment
and has no unaggregated form. Amended 2026-09-19: the record was brought onto the documentation
standard — the code references it carried (file paths, function names, test names and the
References list into the source tree) were removed and the facts moved to the developer pages,
and two misstatements were corrected: read grants were described as a cross-namespace
consumption path (they resolve inside one namespace only), and the two user-facing ClusterRoles
were described as carrying the same aggregation labels (they do not). No rule changed; D1
through D6 keep their numbers and their meaning.

D1 through D6 hold in the tree today, verified against the code on 2026-09-19: D4's first
consequence — the removal of `spec.secretRef.namespace` — shipped on 2026-09-03, and D2 held
for buckets only until ADR 0002 attributed credentials groups through the bucket's tags, so
the former violation is kept below, marked closed. Not verified: whether any Bucket on a live
cluster still sets the removed field, and how Flux server-side apply treats one that does
(see Residual risks).

## Context

`Bucket` is a namespaced kind, and the tenancy model of every cluster this operator runs on
treats the namespace as the unit of trust: a team may create Buckets in its own namespace
and nowhere else. The isolation policy, the read grants and the deletion guards all
quietly assume that whatever a Bucket does stays inside that namespace. The assumption
was written into doc comments but never stated as a rule, and one field broke it.

`spec.secretRef.namespace` — optional, absent from the README, defaulting to the CR
namespace — let a Bucket direct its credentials Secret at **any** namespace. The only guard
was a single name-and-namespace comparison protecting the operator's own admin Secret. The
write was a create-or-update merge: an existing Secret in the target namespace kept its
unrelated keys but lost every key with a colliding name, had its type forced to `Opaque` and
received the `app.kubernetes.io/managed-by` label; no owner reference can be set across
namespaces, so nothing tied it to the Bucket. Deleting the CR deleted that whole Secret.
Net effect: anyone allowed to create a Bucket anywhere held a create/merge/delete primitive
on Secrets in every namespace but one. Recorded as a security finding on 2026-08-24.

The review of the user-facing RBAC work — aggregating a write ClusterRole on `buckets` into
the built-in `edit` and `admin` — made the cost concrete: with that field present, every
namespace admin on the cluster would have become a cluster-wide Secret writer. No Bucket
in the Flux repositories used the field (checked 2026-09-03 in local checkouts dated
2026-08-27 — an indication, not proof).

The decision is recorded as the general rule rather than as the fix, so that the next
field is measured against it before it is added.

## Decision

**D1 — Every cluster object a Bucket creates, changes or deletes lives in the Bucket's
namespace.** Today that is one object: the workload credentials Secret named by
`spec.secretRef.name`. It is created in the Bucket's namespace and always carries the Bucket
as controller owner; it is removed there when the CR is deleted; and a change to a Secret
wakes only the Buckets of that Secret's own namespace.

**D2 — Every cloud object a Bucket creates is attributed to exactly that Bucket by
`namespace/name`, and a Bucket never adopts a cloud object attributed to another Bucket.**
For the bucket itself this holds: the operator stamps the tags `managed-by` (the operator
fleet identity, set at install time) and `owner` (the CR's `namespace/name`) on a bucket it
creates, and a bucket carrying different tags is an ownership collision — a definitive
failure without requeue. For the credentials group it holds since ADR 0002: the bucket names
its group in the tag `credentials-group-id`, and a group is never found, adopted or deleted
by its display name. ~~*Superseded 2026-09-03:* until then the credentials group was
find-or-create by a derived display name, with no ownership check, and that name is not
collision-proof across namespaces~~ — see Residual risks.

**D3 — A reference in a Bucket spec resolves in the Bucket's namespace only.**
`spec.grantReadAccess` names Bucket CRs and they are looked up in the declaring Bucket's
namespace; `spec.cloneFrom.secretRef` names a Secret in the Bucket's namespace. Neither
reference carries a namespace, and a same-named object elsewhere is a different object.

**D4 — No spec field may direct any of D1–D3 elsewhere.** No `namespace` on a reference, no
target-namespace selector, no annotation that widens the scope. `spec.secretRef.namespace`
is removed rather than validated to equal the CR namespace: a field that can only ever
hold one legal value documents nothing and invites its own re-enabling.

**D5 — Objects the operator creates for itself live in the operator namespace and are not
the Bucket's effect.** The admin Secret (ADR 0004), the clone Job and its staging Secret
(ADR 0011) are created in the operator's own namespace under operator-derived names. A
tenant's Bucket triggers them but neither names nor places them. The one way a Bucket could
reach into that namespace — a Bucket in the operator namespace pointing `spec.secretRef.name`
at the admin Secret — is refused as a configuration fault.

**D6 — D1–D5 confine the Bucket, not the operator.** The operator's ClusterRole keeps
cluster-wide Secret write, because it must write into every tenant namespace. The
namespace is a trust boundary for tenants, not for the operator's own credential.

## Consequences

* Consuming a bucket from another namespace has no mechanism in this operator, and D3 is why:
  it forbids the cross-namespace reference rather than leaving a model open. The read grants of
  [ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) are the
  producer-side answer *inside* one namespace and resolve nowhere else (ADR 0008 D2). A consumer
  outside the namespace therefore either gets its own Bucket with its own credentials, or the
  Secret is copied there by tooling outside this operator, under that tooling's rules and not
  under D1.
* Removing a CRD field is an API break, shipped deliberately as a patch release: the field
  was undocumented and its only distinct use was the primitive this ADR forbids. A Bucket
  that still sets it is not rejected but pruned by the API server — the CRD schema has no
  such property and does not preserve unknown fields, so the absence is structural rather
  than test-enforced; `kubectl` with its default strict field validation (1.27 and later)
  rejects it at apply time.
* Migration of a Bucket that used the field: after the CRD upgrade the field is invisible,
  the operator finds no credentials in the (new) Secret in the CR namespace, clears the
  group's keys, mints a new key and writes it there
  ([ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
  D4 and D5). The workload that read the foreign Secret now holds a dead key, and the foreign
  Secret is orphaned — no owner reference — and must be deleted by hand. Looking for such Buckets has to happen **before**
  the CRD upgrade; afterwards the field can no longer be read.
* The Secret watch got cheaper: one namespace-scoped list per Secret event instead of a
  cluster-wide one.
* User-facing write RBAC on `buckets` hands out precisely the power this ADR bounds.
  *Amended 2026-09-03 (ADR 0002):* the condition that kept a write ClusterRole on `buckets`
  out of the built-in `edit`/`admin` aggregation — D2 violated for credentials groups —
  no longer holds. What the role hands out is namespace-scoped (D1–D5); whether to
  aggregate it is a privilege decision, not a security precondition.
  ~~*Superseded rule:* while D2 was violated, the role was shipped for explicit
  per-namespace binding only and not aggregated by default.~~
  *Amended 2026-09-03 (user-facing RBAC shipped):* the chart's `<release>-view` and
  `<release>-edit` ClusterRoles are aggregation fragments, and beyond the chart's standard
  labels they carry only aggregation labels: `<release>-view` carries
  `rbac.authorization.k8s.io/aggregate-to-view`, `-edit` and `-admin`, while `<release>-edit`
  carries `aggregate-to-edit` and `aggregate-to-admin` and deliberately not `aggregate-to-view`
  — the read fragment reaches the built-in `view`, the write fragment never does. There is
  no switch that renders `<release>-edit` unaggregated. A Bucket acts on Secrets of its
  namespace on behalf of whoever created it — `spec.cloneFrom.secretRef` is read and used as
  the clone source (D3), an unowned Secret under `spec.secretRef` is adopted and later
  deleted with the CR (D1) — and the operator cannot check the creator's rights, because the
  CR carries no requester identity and reconciles are level-triggered long after creation.
  Bucket write is therefore only ever handed out together with Secret access in that
  namespace, which is what the built-in `edit` and `admin` guarantee and what a standalone
  `<release>-edit` binding would split. A subject that needs Bucket write without Secret
  access needs a consent mechanism on the referenced Secret first, as its own ADR.

## Alternatives Considered

**Keep the field and validate it equals the CR namespace with a CEL rule.** Rejected. A field
with exactly one legal value documents nothing and invites the validation to be loosened "for
one case". Removing it makes the rule structural: there is nothing to loosen.

**Keep the field and require an opt-in on the target namespace.** An annotation or label on
the receiving namespace, as Secret-replication tools do. Rejected: it adds a second trust
mechanism — a namespace-level opt-in, maintained by whoever owns the receiving namespace,
with its own drift and its own revocation semantics — to an operator whose job is
provisioning. It would also keep the cross-namespace reference alive in the spec, which is
precisely what D3 and D4 remove, and the value it buys is a Secret copy that any replication
tool already performs outside this operator.

**Checking the creator's permission with a SubjectAccessReview.** Rejected twice, for two
different questions, and worth separating. For the removed field (2026-09-03, morning): at
reconcile time there is no creator to check — Buckets arrive through GitOps under a shared
applier identity, and reconciles repeat long after creation, so the review would either be
skipped or run against the operator itself. For the RBAC decision (2026-09-03, afternoon): an
admission webhook *would* have the requester, and could refuse a Bucket whose creator cannot
read the Secret named in `spec.cloneFrom.secretRef`. It lost on cost and on coverage — a
webhook needs a certificate and a failure policy that either blocks all Bucket creation when
it is down or waves everything through, and admission never sees the later changes of a
referenced Secret that the reconcile loop acts on. The confused deputy is closed by never
splitting Bucket write from Secret access instead.

**Make Bucket cluster-scoped.** Rejected. The namespace is the tenancy unit the whole model
rests on; cluster scope would move every boundary into RBAC on object names.

**A consent annotation on the referenced Secret.** Rejected 2026-09-03, and the cheapest of
the three options weighed when the user-facing roles shipped. The operator would use a Secret
as a clone source, or adopt it as the credentials Secret, only if that Secret itself carried
an annotation naming the Bucket allowed to use it. Consent then comes from the party that can
already write the Secret, which needs no requester identity and no webhook. It lost because
no subject in the fleet needs Bucket write without Secret access in the same namespace, so it
would have bought nothing and cost an annotation on every existing clone source and every
adopted Secret at upgrade time. It stays the path back: the day a role must hand out Bucket
write without Secret access, this is the mechanism, and it changes what a Bucket may read —
so it needs its own record before it is built.

**Refusing an operator-managed Secret as a clone source.** Rejected 2026-09-03. Narrower than
the annotation and almost free: the copy job would decline any Secret carrying this operator's
managed-by label, so a Bucket could not be pointed at a sibling Bucket's credentials. It lost
on both ends. It breaks the documented and end-to-end-tested case of seeding one Bucket from a
sibling in the same namespace, which is the feature working as intended, and it closes only
half the hole — an unmanaged Secret holding S3 credentials for some third-party endpoint is
still readable by the copy job, and the Secret adoption under `spec.secretRef` is untouched.

## Residual risks

* **D2 was violated for credentials groups (closed 2026-09-03 by ADR 0002).** Kept for the
  record: the workload group's display name was `s3op-<namespace>-<name>`, truncated to 23
  characters plus an 8-hex FNV-1a-32 suffix, and it was the handle by which a Bucket's group
  was found or created, with no ownership check. The namespace/name pairs
  `("gitlab", "gitlab-artifacts")` and `("gitlab-gitlab", "artifacts787ngo")` derive the same
  name, `s3op-gitlab-gitlab-arti-70dbcfc2` (reproduced 2026-08-24). A colliding Bucket adopted
  the victim's group, deleted the victim's live key and minted a new one in that group, and
  the victim's bucket policy trusted that group's URN — a credential takeover of a foreign
  bucket plus an outage for its workload. ADR 0002 closed this by attributing the group
  through the bucket's tags, with the bucket's own policy as the migration path — no group was
  renamed and no key rotated.
* Read access is bounded by the same rule from the other side: `get`/`list` on `buckets`
  exposes ids, URLs, sizes and conditions, no credential material — `status.accessKeyID` is
  the key id only, and the secret half is only ever written to the Secret.
* **Not verified:** whether Flux server-side apply rejects or silently prunes a Bucket that
  still sets `spec.secretRef.namespace`; whether any Bucket on the target clusters sets it;
  whether the interim, non-Helm-managed ClusterRole of the same name is deployed on any given
  cluster, which would block the chart upgrade that ships the aggregation fragments.

## References

* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — credentials-group attribution; amends D2 and the RBAC consequence
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the operator's own credential, which D5 and D6 carve out
* [ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) — the Secret D1 places, and what happens to it on migration
* [ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) — read grants, which resolve inside one namespace and are the producer-side sharing model D3 leaves room for
* [ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) — the clone Job and staging Secret of D5
* [docs/security/rbac-and-privilege.md](../security/rbac-and-privilege.md) — what Bucket read and Bucket write hand out, in full
* [docs/security/ownership-and-attribution.md](../security/ownership-and-attribution.md) — the tag attribution D2 rests on, and its gaps
* [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md) — the namespace as trust boundary, and the layers beneath it
* [docs/operations/deployment.md](../operations/deployment.md) — installing the user-facing ClusterRoles, and the upgrade trap of a pre-existing role of the same name
* [docs/operations/credentials.md](../operations/credentials.md) — where the credentials Secret lands and how to migrate one that pointed elsewhere
* [docs/developer/credentials.md](../developer/credentials.md) — how the Secret is written, adopted and deleted
* [docs/developer/bucket-identity.md](../developer/bucket-identity.md) — ownership tags, collision detection and group attribution in the code
