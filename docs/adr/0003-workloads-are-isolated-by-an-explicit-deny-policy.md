# ADR 0003: Workloads Are Isolated From Each Other By An Explicit Deny Policy

## Status

Accepted. Date: 2026-06-30.
Amended 2026-07-22 (multipart management is object work, D7).
Amended 2026-08-05 (the exemption list follows one inclusion criterion instead of growing
action by action, D4; the no-condition-key invariant, D6).
Amended 2026-08-24 (a granted reader is a third exempted principal, D3; the grant itself is
[ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md)).

Implemented. Every provisioned bucket carries the document described here, written with the
operator's own admin credential ([ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md));
the two mandatory statements and the optional third are produced from one builder, so the
grantor and grantee action lists cannot drift apart in spelling. The two-statement shape and the
three-statement shape have both been exercised against the real backend by the layer-2
integration suite. Still open: the exemption list is maintained against the provider's
documented action set by hand, so a client that needs an action nobody has hit yet still
surfaces as an `AccessDenied` in that client rather than as a failure the operator can report.

## Context

The product's hard requirement is that a workload can neither see nor change another workload's
data. Two boundaries carry that, and only one of them is free.

Across StackIT projects the boundary is structural. Every control-plane call is project-scoped
and the operator's service-account token holds roles only in its own project, so no operator
defect can reach a second project. Measured on 2026-06-30 with two service accounts in two
projects of the same organisation: cross-project list, create and delete each answered **HTTP
403** in both directions, and the victim bucket survived the attempted delete. This boundary
costs the product nothing and depends on no code of ours — see
[ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) for why one deployment stays
bound to one project.

Inside one project there is no boundary at all by default. The provider's own position is that
all project members can access all data in that project's object storage, and the measurement on
2026-06-30 confirmed it in the harsher form: *any* credentials group of the project may do
*anything* in *any* bucket of the project. The first policy template drafted for this product was
an `Allow` for the bucket's own workload group. It separates nothing. S3 policy evaluation is
additive for `Allow`: granting one principal access does not withdraw the access everybody else
already has. The only construct that takes access away is an explicit `Deny`, which no later
`Allow` overrides. So isolation inside a project is not configuration of an existing boundary —
it is the boundary, authored per bucket, by us.

That has a second consequence which shaped everything after it: a bucket policy is set on the S3
data plane, not through the control-plane SDK, so the operator must hold a data-plane credential
of its own before it can isolate anything ([ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md)).
And because the restricting construct is `Deny`, the operator can lock *itself* out: on the
StorageGRID backend a `Deny` that catches the administrative identity is not repairable from
outside, since repairing it would itself require a bucket-policy write.

The second statement — the one that keeps the workload to object work — is an inverted
whitelist, and the cost of that shape was paid twice in production:

| Date | What broke | What was missing |
| --- | --- | --- |
| 2026-07-22 | GitLab container registry returned `500` on every blob push; the registry log showed `AccessDenied` while "resolving upload". Measured directly with the bucket's own credentials: `CreateMultipartUpload`, `UploadPart` and `CompleteMultipartUpload` succeeded, `ListMultipartUploads`, `ListParts` and `AbortMultipartUpload` were denied. Reproduced with a second bucket's credentials, so it was the policy and not one bucket. | `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts`, `s3:AbortMultipartUpload` |
| 2026-08-05 | CNPG/barman backups failed at their last step: barman writes `base/<id>/backup.info` twice per backup, and the second write is an overwrite of an existing key. Writes to new keys and multipart uploads worked, a plain `PutObject` on an existing key did not. | `s3:PutOverwriteObject`, a StorageGRID-specific action that gates any write to a key that already exists |

The exemption list had started at five actions on 2026-07-01, went to eight on 2026-07-22 and
reached seventeen on 2026-08-05, when the approach changed from adding the action that just
broke to stating one inclusion criterion and aligning the list with the backend's documented
action set under it. The backend is NetApp StorageGRID, whose action names are a superset of the
AWS ones and which splits operations AWS folds into `s3:PutObject`; a list that merely looks
reasonable therefore breaks clients silently, at the one call they make rarely.

## Decision

**D1 — Workloads are isolated from each other inside the project, not only across projects.**
Cross-project separation is a property of the provider and is not sufficient. Every bucket this
operator provisions gets its own credentials group and its own policy, and the two together are
what separates one workload from its neighbour in the same project and the same cluster.

**D2 — Isolation is expressed as `Deny`. An `Allow`-only document is never the isolation
mechanism.** The provider's default inside a project is open, `Allow` is additive, and only an
explicit `Deny` removes access. A policy that grants without denying has been measured to
separate nothing.

**D3 — The first statement denies everything to every principal the bucket does not name.**
Statement `deny-all-except-admin-and-workload` denies `s3:*` on the bucket and on everything
inside it, with a `NotPrincipal` list holding the operator's admin identity, the bucket's own
workload identity, and — once the bucket's contents are complete — any identity the bucket has
granted read access to. While an initial clone is still filling the bucket, no granted reader is
in that list at all; the readers are added in the pass that observes the copy finished
([ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D10,
[ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D8), so a grant does not
take effect during a clone even though the grantee already holds its own credentials. This is the
statement that separates workloads: every other credentials group of the project falls under it.
A principal that is missing from the exemption list is denied by this statement regardless of what
any later statement grants it.

**D4 — The second statement denies the owning workload everything that is not object work.**
Statement `workload-objects-only` is a `Deny` with `NotAction` on the workload principal, i.e. an
inverted whitelist: an action that is not listed is denied. An action qualifies for the list only
if it operates on object data, object metadata or object listings. Denied by design stays
anything that changes *who* may access the bucket, routes its contents elsewhere, pins an
object's lifetime, destroys history, or reconfigures the bucket.

| Group | Actions in the exemption list | Why it is object work |
| --- | --- | --- |
| Object data | `s3:GetObject`, `s3:PutObject`, `s3:PutOverwriteObject`, `s3:DeleteObject` | The workload's own payload. `s3:PutOverwriteObject` is StorageGRID's separate action for writing a key that already exists |
| Listing and endpoint discovery | `s3:ListBucket`, `s3:ListBucketVersions`, `s3:GetBucketLocation` | Reading what is there; SigV4 clients resolve the region before their first request |
| Multipart management | `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts`, `s3:AbortMultipartUpload` | See D7 |
| Object tagging | `s3:GetObjectTagging`, `s3:PutObjectTagging`, `s3:DeleteObjectTagging` | Object metadata; set in passing by common clients |
| Version-aware reads | `s3:GetObjectVersion`, `s3:GetObjectVersionTagging` | Reads only; the workload cannot enable versioning |
| Read-only configuration probes | `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration` | Issued by several SDKs before a write; they expose nothing, and denying them only produces confusing client-side errors |

| Group | Kept denied | The failure it prevents |
| --- | --- | --- |
| Policy | `s3:GetBucketPolicy`, `s3:PutBucketPolicy`, `s3:DeleteBucketPolicy` | The workload lifting its own restrictions; the read additionally leaks the admin identity |
| Forwarding | `s3:PutReplicationConfiguration`, `s3:PutBucketNotification`, `s3:PutBucketMetadataNotification` | Exfiltration: the bucket's contents mirrored to a destination the workload chooses |
| Lifetime pinning | `s3:PutObjectRetention`, `s3:PutObjectLegalHold`, `s3:PutBucketObjectLockConfiguration`, `s3:PutBucketCompliance`, `s3:BypassGovernanceRetention` | A workload could make its bucket permanently undeletable and break finalizer teardown ([ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md)) |
| History | `s3:DeleteObjectVersion`, `s3:DeleteObjectVersionTagging`, `s3:PutObjectVersionTagging` | Destroying or rewriting historic versions, which is what makes versioning worth having |
| Reconfiguration | `s3:PutBucketVersioning`, `s3:PutLifecycleConfiguration`, `s3:PutEncryptionConfiguration`, `s3:PutBucketCORS`, `s3:PutBucketTagging`, `s3:DeleteBucket` | Bucket configuration is the operator's, including the ownership tags attribution rests on ([ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md)) |

**D5 — The operator's admin identity is in the exemption list of every policy the operator writes,
and is never named as a restricted principal.** This is a lockout guard, not a convenience: on
this backend a `Deny` can shut out even the account root, and repairing a policy requires the
very write the bad policy denies. The rule holds for identities supplied from elsewhere too — an
identity that would end up restricting the admin or narrowing the bucket's own workload is
removed before the document is built, which also makes a bucket granting itself read access a
harmless no-op.

**D6 — This policy carries no tag-based condition keys, in any statement, for as long as the
workload may write object tags.** With `s3:PutObjectTagging` granted, a condition on
`s3:ExistingObjectTag/*` or `s3:RequestObjectTag/*` would let the workload rewrite its own
permissions by tagging an object. Granting tagging is only safe because no access decision reads
a tag. Whoever wants tag-based conditions removes the tagging actions from D4 first, in the same
change.

**D7 — Multipart management is object work and belongs in D4's exemption list; it is not bucket
management.** Uploading a part maps to `s3:PutObject`, but resuming and cleaning up a chunked
upload are distinct actions, and clients call them on the ordinary write path rather than
exceptionally. They act on the principal's own in-flight upload of its own bucket, so they change
nothing about who may reach the bucket.

**D8 — The bucket policy is the operator's document, and the operator re-asserts it.** Every
reconcile computes the document the bucket should have and compares it against the live one,
ignoring key order and whitespace; it is rewritten only when the two differ. A manual change is
therefore reverted rather than preserved, and a document changed by a new operator version
reaches an already-provisioned bucket without anyone touching its custom resource.

## Consequences

Provisioning a bucket is never complete without a data-plane write, so the operator must hold a
project-wide S3 admin credential and keep it working. That credential is a single point of
failure for the whole fleet, which is why it has its own record
([ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md)).

A policy that loses the admin exemption is not repairable from inside the product: fixing it needs
the very bucket-policy write the bad policy denies, so the remedy is provider-side intervention.
Not verified: this rests on the same undemonstrated backend behaviour as the account-root claim in
Residual risks, and was deliberately never reproduced, because reproducing it means destroying a
bucket's manageability. D5 is therefore an invariant of the document itself rather than a rule the
identities that feed into it are trusted to respect.

Teardown inherits the same dependency. The emptiness check before a delete
([ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md)) is a data-plane read with the
admin credential, so a bucket whose policy shut the admin out cannot even be checked, and its
deletion parks. Not verified: that the control plane can still remove such a bucket irrespective
of its policy. A control-plane delete does not evaluate the bucket policy for an ordinary bucket,
which is why it is offered as a second net, but nothing here has measured it against a bucket
whose policy denies the operator's own identity — the one case the net is for — and it helps only
an operator run that gets as far as calling it.

A changed policy reaches existing buckets only through D8's comparison, and only when something
re-reconciles the bucket with the new version. On 2026-07-22 an upgrade shipped the multipart fix
and an already-provisioned bucket silently kept its old five-action policy. Two causes, each
sufficient on its own, and the remedy has two halves:

| Cause | Remedy |
| --- | --- |
| Nothing re-reconciled the bucket after the upgrade: the `Bucket` watch fires on generation and annotation changes, which an operator upgrade does not produce | A provisioned bucket is re-reconciled periodically (`--drift-resync-interval`, environment `DRIFT_RESYNC_INTERVAL`, Helm `driftResyncInterval`, `10m` # default, `0` disables) |
| During a rolling update the outgoing and the incoming pod both reconcile, so the old version could re-apply the stale document after the new one had written the correct one | Leader election is on by default (Helm `leaderElection.enabled`, `true` # default), so the incoming pod waits for the lease and there is no concurrent write |

The fleet therefore converges within the resync interval after an upgrade rather than immediately.
The two halves do not fully overlap: the single rollout that first turns leader election on still
has an outgoing pod that predates it and keeps reconciling freely, and the periodic resync is what
heals whatever that rollout leaves behind.

The inverted whitelist of D4 fails closed, which is the right direction and an operational cost:
an action nobody thought of is denied, and the symptom lands in the client as `AccessDenied`,
often on a rarely-taken path, not in the operator's logs or the bucket's status. Two production
incidents were diagnosed that way. Extending the list is a code change and a release, not a
setting an operator can turn.

D4 also decides against per-application policies: every workload gets the same object-level
capability set. A workload that needs less than the full set cannot be narrowed, and one that
needs a denied action cannot be widened for itself alone.

## Alternatives Considered

**An `Allow`-only policy naming the bucket's workload group.** Rejected, and it was the original
design. It is the cheaper document — one statement, no exemption lists, no lockout risk — and it
lost on measurement rather than on reasoning: on 2026-06-30 the provider's project-wide default
access was found to be open, and an additive `Allow` withdraws nothing from anybody. The template
was replaced the same day. The cheapness was real and it bought nothing, because the thing being
bought was the removal of access.

**No intra-project isolation at all: one project per workload.** Rejected. It would have made
every boundary structural and free, since cross-project separation is the provider's own. It
fails on the shape of the product: one operator deployment is bound to exactly one project
([ADR 0005](0005-the-operator-serves-one-project-in-one-region.md)), so a project per workload
means an operator per workload, plus a service account, a key and a role assignment for each —
provisioned by hand, outside Kubernetes, which is exactly the work this operator exists to remove.

**One shared credentials group per namespace instead of one per bucket.** Rejected. It reduces
the number of cloud identities and makes the policy shorter, and the namespace is already the
trust boundary for cluster objects ([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md)).
But it makes every bucket of a namespace reachable with every other bucket's credentials, so a
leaked secret widens from one bucket to all of them, and read grants between two buckets of the
same namespace — the case that motivated them
([ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md)) — become
meaningless because the principal is already the same. It also removes the per-bucket credential
rotation that [ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
rests on.

**Bucket and object ACLs instead of a policy.** Rejected. ACLs cannot express a denial: they
grant, in a fixed and coarse vocabulary, with no equivalent of `NotPrincipal` or `NotAction`, so
the open default would remain untouched underneath them. They also attach to objects, which would
put a correctness-critical setting on the write path of every workload client. Nothing in this
product uses ACLs, and the policy is deliberately the only access-control surface.

**A single policy covering all buckets, or a group policy on the credentials group.** Rejected.
The document would have to enumerate every bucket and every group in the project, so it would be
rewritten on every provisioning and every teardown — one document whose every write touches every
tenant, with no per-bucket blast radius and no way to leave a bucket's access unchanged while
another bucket changes. A per-bucket document is written once per bucket, compared per bucket,
and wrong for at most one tenant at a time.

**Restricting access by source IP in addition to principal.** Not taken. It was considered when
the policy model was first drafted and offers little in the clusters this operator targets: workload
traffic leaves through egress addresses shared by every workload of the cluster, so it separates
clusters at best, never two workloads, and it breaks the moment egress is renumbered. It remains
available for a deployment whose clusters have stable, distinct egress.

## Residual risks

The admin identity's exemption is the single point of failure of this design. D5 removes the
known path to losing it, and the loss is unrepairable by the product if it ever happens.

The exemption list is a manual approximation of "object work" against a provider whose action
vocabulary is larger than the AWS one. Two clients have already been broken by an absent action.
A third is a matter of time, and the product will learn about it from the client's error, not
from its own.

`s3:DeleteObject` is granted, so a compromised workload credential destroys the bucket's data
within its own bucket. The policy bounds the blast radius to that bucket; it does not protect the
contents of it. Object lock and retention are deliberately denied to the workload (D4), so they
are not available as a mitigation from inside either.

Layer 1 is what this design stands on, and it is a property of how the service account's roles
were assigned. An organisation-level role that cascades into a second project defeats it, and no
code here can detect that: from inside the operator, a token with too many roles and a token with
the right ones are indistinguishable. This is a deployment-time check.

Not verified in this repository: that the backend can lock out even the account root through a
bucket policy. That claim comes from the backend vendor's documentation and is the reason D5
exists; deliberately never reproduced, because reproducing it means destroying a bucket's
manageability. D5 is therefore a precaution against a documented behaviour, not against a
measured one.

Not verified: that denying `s3:PutOverwriteObject` ever prevented anything. The vendor documents
WORM semantics as arising from denying it *together with* `s3:DeleteObject`, which was never the
case here, so the action was granted on 2026-08-05 without a measurement of what its absence had
been protecting.

Not verified: that a tag-based condition key would actually be honoured by this backend. D6 is
precautionary; the combination was never tried, and the invariant is written to keep it from
being tried by accident.

Not verified: that the multipart fix was re-measured against the bucket that originally failed
after it shipped. The action list is asserted by the offline suite and the fix shipped on
2026-07-22; the closing measurement is not recorded here.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — the namespace as the trust
  boundary for everything a Bucket touches; this record is the cloud-side counterpart
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — which credentials
  group a bucket's policy names, and why the policy itself is an attribution source
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the data-plane
  credential without which no policy can be written
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — the project binding that
  makes cross-project isolation structural
* [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) — the teardown guard that also
  depends on the admin credential reaching the bucket
* [ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) — the
  per-bucket credential whose identity this policy names
* [ADR 0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) — the optional
  third statement and the rules for the identities it exempts
* [ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) — why a granted reader
  stays out of the document while a bucket is still being filled
* [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md) — the threat
  model, the policy document as written, and what this isolation does not defend against
* [docs/developer/bucket-policy.md](../developer/bucket-policy.md) — how the document is built,
  compared and repaired, and where the action lists live
* [docs/operations/read-grants.md](../operations/read-grants.md) — what a granted reader may do
* [docs/operations/configuration.md](../operations/configuration.md) — the drift resync interval,
  leader election and the other operator-wide settings
* [docs/operations/deployment.md](../operations/deployment.md) — what an operator upgrade does to
  the policy of an already-provisioned bucket
