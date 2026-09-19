# Tenancy and isolation

This page is about what keeps one tenant's object data away from another's: the boundary
between StackIT projects, the boundary between two workloads inside one project, the document
that creates the second one, and the ways either can be weakened. It is about *separation*.
If what you actually want is where each credential lives, who in the cluster can read it and
what its leak costs, that is [credentials-and-secrets.md](credentials-and-secrets.md); if it
is what a Kubernetes subject gains by being allowed to create a `Bucket`, that is
[rbac-and-privilege.md](rbac-and-privilege.md); if it is how the operator decides a cloud
object is its own, that is [ownership-and-attribution.md](ownership-and-attribution.md).

## The two boundaries

| | Layer 1 — between projects | Layer 2 — inside one project |
| --- | --- | --- |
| What it separates | Every tenant of project A from every tenant of project B | One `Bucket`'s workload from every other workload of the same project |
| Enforced by | StackIT's own authorisation on the service-account token | A per-bucket S3 policy this operator writes |
| Depends on this codebase | No | Entirely |
| Fails if | The service account is given a role above project scope, which is forbidden by [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D5 | The policy is absent, wrong, or shut the operator out |
| Decision | [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D1, D5 | [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D1–D2 |

The two are not alternatives and not redundant. Layer 1 is free and absolute but only as coarse
as a project; Layer 2 is fine-grained and is entirely our work. Everything below Layer 1 in this
product stands on Layer 1 holding.

## Layer 1: separation between StackIT projects

Every control-plane call in the StackIT Object Storage API carries a project id, and a
service-account token carries roles only inside its own project. A call into a foreign project is
refused by the provider before any operator logic runs, so no defect in this codebase can reach a
second project's data. One operator deployment is bound to exactly one project, and the binding
is the mounted service-account key itself — there is no project setting and no `Bucket` field that
selects a project ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D1).

This was measured, not assumed. On 2026-06-30 two service accounts in two projects of the same
organisation were pointed at each other against the real API: cross-project *list*, *create* and
*delete* each answered **HTTP 403** in both directions, and the victim's bucket survived the
attempted delete.

That measurement is kept as an on-demand test, `TestIntegrationCrossProjectIsolation` in
[`stackit/integration_test.go`](../../stackit/integration_test.go), behind the `integration`
build tag; it skips itself when no service-account key files are present, so the offline suite
never runs it. Two things about it are worth stating exactly, because the test is not a
re-run of the measurement:

* For *create* and *delete* it demands an explicit deny (`401`, `403` or `404`) and, for delete,
  that the victim bucket still exists afterwards.
* For *list* it is deliberately weaker: an `HTTP 200` is tolerated as long as the foreign bucket
  does not appear in the listing, and the test only logs a warning in that case. So a green run
  proves the foreign bucket was not visible; it does not by itself re-prove the `403`.

The measurement stands for the two feasibility projects of 2026-06-30, and only for those: their
service accounts were replaced on 2026-08-24 by dedicated end-to-end accounts, and the old projects
now answer `EnsureService` with `403`, so the test can no longer re-prove that measurement against
the subjects it was made on. It has not been repeated against the current accounts
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md), *Residual risks*), and no
run date of `TestIntegrationCrossProjectIsolation` is recorded in this repository.

Layer 1 has exactly one precondition, and it is set outside this product: the service account's
roles must be assigned at *project* scope, and any role that cascades into a second project is
forbidden regardless of convenience
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D5). See
[H-1](#h-1-an-organisation-level-role-dissolves-layer-1-and-nothing-here-can-see-it). A second way
to end up serving the wrong project — mounting a key for a project nobody intended — is
[H-6](#h-6-nothing-declares-which-project-a-deployment-is-supposed-to-serve).

## Layer 2: separation inside one project

Inside one project there is no boundary at all by default. The provider's position is that all
project members can access all data in that project's object storage, and the measurement on
2026-06-30 confirmed it in the harsher form: any credentials group of the project may do anything
in any bucket of the project.

The first policy template drafted for this product was an `Allow` naming the bucket's own
workload group. It separates nothing, and that is a property of S3 policy evaluation rather than
of this backend: `Allow` is additive, so granting one principal access withdraws nothing from
anybody else. Only an explicit `Deny` removes access, and no later `Allow` overrides it. Isolation
inside a project is therefore not the configuration of an existing boundary — it *is* the
boundary, authored per bucket, by this operator
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D2).

**The unit of isolation is the bucket, not the namespace.** Each `Bucket` custom resource gets its
own StackIT credentials group, its own access key and its own policy
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D1). Two `Bucket`s
in the same Kubernetes namespace are as separated from each other in the cloud as two `Bucket`s in
different namespaces; the only way one reaches the other's data is an explicit read grant. The
alternative — one shared group per namespace — was rejected because a leaked Secret would then
widen from one bucket to all of that namespace's buckets, and because it would make read grants
between siblings meaningless (same record, *Alternatives Considered*).

The cluster-side half of tenancy is a separate rule with a separate record: everything a `Bucket`
creates, names or deletes stays in that `Bucket`'s own namespace, and no spec field can direct it
elsewhere ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1–D4). Consuming a
bucket from another namespace has no mechanism in this operator at all.

## The isolation document

One document per bucket, written and re-asserted with the operator's own S3 admin credential
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D2). It is
produced by `BuildIsolationPolicy` in [`stackit/s3.go`](../../stackit/s3.go) and nowhere else —
the integration tests call the same function, so no second spelling of the document exists.

This is the whole document for a bucket with one granted reader. Without a grant the third
statement is absent and the reader URN is absent from the first statement's `NotPrincipal`, which
makes the document byte-identical to the two-statement policy that predates read grants.

```jsonc
// # example — a bucket with one granted reader. Principal values are StorageGRID
// credentials-group URNs; Resource values are S3 ARNs. The mixture is correct.
{
  "Statement": [
    {
      "Sid": "deny-all-except-admin-and-workload",
      "Effect": "Deny",
      "NotPrincipal": { "AWS": [
        "urn:sgws:identity::123:group/operator-admin",
        "urn:sgws:identity::123:group/s3op-team-a-data",
        "urn:sgws:identity::123:group/s3op-team-a-backup"
      ] },
      "Action": ["s3:*"],
      "Resource": ["arn:aws:s3:::example-bucket", "arn:aws:s3:::example-bucket/*"]
    },
    {
      "Sid": "workload-objects-only",
      "Effect": "Deny",
      "Principal": { "AWS": "urn:sgws:identity::123:group/s3op-team-a-data" },
      "NotAction": [ /* the 17 object actions listed below */ ],
      "Resource": ["arn:aws:s3:::example-bucket", "arn:aws:s3:::example-bucket/*"]
    },
    {
      "Sid": "granted-readers-read-only",
      "Effect": "Deny",
      "Principal": { "AWS": ["urn:sgws:identity::123:group/s3op-team-a-backup"] },
      "NotAction": [ /* the 9 read actions listed below */ ],
      "Resource": ["arn:aws:s3:::example-bucket", "arn:aws:s3:::example-bucket/*"]
    }
  ]
}
```

| Statement | Shape | What it denies |
| --- | --- | --- |
| `deny-all-except-admin-and-workload` | `Deny` + `NotPrincipal` | Everything, on the bucket and its objects, to every principal in the project that is not the operator's admin group, this bucket's own workload group, or a granted reader. **This is the statement that separates workloads.** |
| `workload-objects-only` | `Deny` + `NotAction`, principal = this bucket's workload group | Everything the bucket's own workload may not do — i.e. every S3 action outside the 17-action object list. No bucket management, no policy write, no replication or notification, no object lock. |
| `granted-readers-read-only` | `Deny` + `NotAction`, principals = the granted readers | Everything outside the 9-action read list. Present only when at least one grant resolved. |

A principal missing from the first statement's exemption list is denied by that statement
regardless of what any later statement grants it — which is why a granted reader has to appear in
both places, and does.

<details>
<summary>The exact action lists, as the builder holds them today</summary>

Every action that appears in *both* lists is a shared string constant, so it cannot be spelled two
ways in two statements. That is the subset where a divergent spelling would matter; the five
mutating actions the workload alone holds — `s3:PutObject`, `s3:PutOverwriteObject`,
`s3:DeleteObject`, `s3:PutObjectTagging`, `s3:DeleteObjectTagging` — are written as literals,
because they appear in one list only. The reader list being a strict read-only subset of the
workload list is *not* enforced by the compiler — any constant could be written into either list and
still build — it is enforced by `TestReaderAllowedActions_ReadOnly` in
[`stackit/s3_test.go`](../../stackit/s3_test.go).

| Group | The bucket's own workload (17 actions) | A granted reader (9 actions) |
| --- | --- | --- |
| Object data | `s3:GetObject`, `s3:PutObject`, `s3:PutOverwriteObject`, `s3:DeleteObject` | `s3:GetObject` |
| Listing and endpoint discovery | `s3:ListBucket`, `s3:ListBucketVersions`, `s3:GetBucketLocation` | `s3:ListBucket`, `s3:ListBucketVersions`, `s3:GetBucketLocation` |
| Multipart management | `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts`, `s3:AbortMultipartUpload` | — |
| Object tagging | `s3:GetObjectTagging`, `s3:PutObjectTagging`, `s3:DeleteObjectTagging` | `s3:GetObjectTagging` |
| Version-aware reads | `s3:GetObjectVersion`, `s3:GetObjectVersionTagging` | `s3:GetObjectVersion`, `s3:GetObjectVersionTagging` |
| Read-only configuration probes | `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration` | `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration` |

`s3:PutOverwriteObject` is a StorageGRID-specific action that gates any write to a key that
already exists. It is not a security boundary: `s3:DeleteObject` is granted anyway, so the same
effect is reachable with a delete followed by a put.

The inclusion criterion and the per-group reasoning for what stays denied are the subject of
[../developer/bucket-policy.md](../developer/bucket-policy.md) and of
[ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D4; this page states
only the boundary they produce.

</details>

**The policy is written before any workload credential exists.** In
`provisionCredentialsAndClone` ([`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go))
the order is: resolve the workload group → resolve read grants → write the policy → optionally run
the clone → only then mint the access key and write the Secret. A bucket is therefore never both
open to the rest of the project and holding a credential somebody could use. It is still briefly
open between its creation and that first policy write — see
[H-2](#h-2-a-new-bucket-is-open-to-the-project-between-its-creation-and-its-first-policy-write).

## The admin exemption, and why it can never be dropped

The operator's own S3 admin group is in the `NotPrincipal` of the first statement of every policy
the operator writes, and can never appear as a granted reader
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D7,
[ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D5). This is a
lockout guard and not a convenience. There is no control-plane route to a bucket policy — setting
one is a data-plane S3 call — so an admin identity that a policy denies cannot rewrite the policy
denying it. The failure is permanent from inside the product.

The guard is implemented in the builder rather than at the call site. `sanitizeReaderURNs` drops
the admin URN and the bucket's own workload URN from any reader list unconditionally, whitespace-
trimmed on both sides of the comparison, before the document is assembled. The admin case is the
lockout above. The workload case is subtler and equally destructive: a second, narrower `Deny` on
the bucket's own owner would *intersect* with the first, because `Deny` is never overridden, and
the owner would silently lose write access to its own bucket.

Two escape hatches are commonly assumed here and neither is established:

* That the backend can lock out even the account root through a bucket policy. That claim comes
  from the backend vendor's documentation and has never been reproduced in this project — see
  [H-3](#h-3-the-lockout-property-and-its-escape-hatch-are-both-unmeasured).
* That a control-plane bucket delete works regardless of the bucket policy, and is therefore a
  second net under a locked-out bucket. Also unmeasured against a bucket whose policy denies the
  operator, and it would in any case only help a run that gets as far as calling it: the emptiness
  check that guards every deletion is itself a data-plane read with the admin credential
  ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)), so a locked-out bucket
  parks before the control-plane call is ever reached. Same gap, [H-3](#h-3-the-lockout-property-and-its-escape-hatch-are-both-unmeasured).

## What a read grant hands over

A `Bucket` may list sibling `Bucket`s of its **own namespace** in `spec.grantReadAccess`, and
their workload credentials then additionally get read-only access to *this* bucket. The grant is
declared by the bucket whose data is at stake, never by the consumer, so a bucket's full access
list is readable in its own spec and no `Bucket` can widen its own reach
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D1). At most
32 entries, enforced by the CRD schema. How to use it is
[../operations/read-grants.md](../operations/read-grants.md).

| A granted reader may | A granted reader may not |
| --- | --- |
| `s3:GetObject`, `s3:GetObjectVersion` | any `s3:Put*`, `s3:Delete*` or `s3:Create*` action |
| `s3:ListBucket`, `s3:ListBucketVersions` | `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts` — they expose the owner's *uncommitted* uploads, their key names and part layout |
| `s3:GetObjectTagging`, `s3:GetObjectVersionTagging` | `s3:AbortMultipartUpload` — it destroys the owner's in-flight upload |
| `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration` | read or write the bucket policy, replication, notification, lifecycle or object-lock configuration |
| `s3:GetBucketLocation` | anything at all on a bucket that did not grant it |

The security of the whole feature is in **where the reader's principal comes from**, and it is
worth naming the rejected implementation because the current code is shaped by it. The obvious
resolution is to read the grantee's published `status.credentialsGroupURN`. Holding
`buckets/status` update in a namespace is a strictly weaker permission than being able to read the
workload Secrets that status describes, so a forged URN would be pasted verbatim into somebody
else's bucket policy — naming the admin group there produces the unrepairable lockout above, and
naming any other group in the project hands that group read access to data it was never granted.

So the URN is never taken from the referenced object. `resolveReadGrants` resolves the reference
to a `Bucket` CR **in the grantor's own namespace**, looks up that CR's physical bucket in the
cloud, requires that bucket to carry that grantee's ownership tags, and takes the group from the
bucket's `credentials-group-id` tag — or, for a bucket provisioned before that tag existed, from
the principal of that bucket's own `workload-objects-only` statement
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3,
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D5/D6). Both of
those are writable only with the admin credential. Status, annotations, Secret contents and the
group's display name take no part in it.

Group display names take no part *deliberately*, and that rule was bought at a price. Until
2026-09-03 the grantee's group was resolved by a display name derived from namespace and `Bucket`
name; on 2026-08-24 that name was shown to collide across namespaces, with
`("gitlab", "gitlab-artifacts")` and `("gitlab-gitlab", "artifacts787ngo")` deriving the same
name. A colliding `Bucket` could adopt a foreign group, and the victim's bucket policy trusted
that group's URN. Attribution now runs through the bucket, and StackIT's non-unique display names
are consequently harmless — the mechanism and the remaining exposure are
[ownership-and-attribution.md](ownership-and-attribution.md).

Three further properties of the grant matter for isolation:

* **A self-grant cannot widen anything**, and is closed three times over: the CRD rejects it at
  admission with a root-level CEL rule, `resolveReadGrants` skips it with a `ReadGrantPending`
  event if one ever reaches the reconcile, and `sanitizeReaderURNs` would drop the URN anyway.
* **An unresolvable grant is skipped, not fatal.** The grantor still becomes `Ready` and emits a
  `ReadGrantPending` warning event. A data owner must not lose its own provisioning because a
  consumer is missing.
* **A bucket still being filled by a clone grants nothing.** While the copy is pending the policy
  is written with an empty reader set, and the readers are added in the very pass that observes
  the copy finished. Holding the bucket's own Secret back is not enough here, because a granted
  reader already holds working credentials of its own — the policy is the only thing that can hold
  it back ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D10,
  [ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D8).

Layer 2 with a reader present is the one shape an offline fake cannot prove, because it depends on
the backend's evaluation of a `Deny` whose `Principal` is a *list*. It has its own on-demand test
against the real provider, `TestIntegrationReadGrant` in
[`stackit/grants_integration_test.go`](../../stackit/grants_integration_test.go): the owner keeps
full object access, the reader lists and gets but cannot write, delete or touch the policy, an
ungranted fourth principal stays locked out entirely, the admin stays unrestricted, and dropping
the grant locks the reader out again. Not verified: the date of its last green run.

## No tag-based condition keys, for as long as the workload may tag objects

The workload is granted `s3:PutObjectTagging`. That is only harmless because no access decision in
this system reads a tag. A condition on `s3:ExistingObjectTag/*` or `s3:RequestObjectTag/*`
anywhere in this policy would let the workload rewrite its own permissions by tagging an object,
which is a straight privilege escalation from inside a bucket the design otherwise bounds.

The invariant is therefore absolute and stated in both directions
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D6): this document
carries no condition keys at all in any statement, and whoever wants tag-based conditions removes
the tagging actions from the workload list first, in the same change. Verified against the code on
2026-09-19: the string `Condition` appears in [`stackit/s3.go`](../../stackit/s3.go) only inside
the comment that states this invariant, and no statement the builder emits has a `Condition`
element.

Not verified: that this backend would honour a tag-based condition key at all. The combination was
never tried, and the invariant exists so that it is not tried by accident.

## When a `Bucket` names a physical bucket that is not its own

`spec.bucketName` lets a `Bucket` name an existing physical bucket, so tenancy needs an answer for
a `Bucket` in one namespace naming a bucket that belongs to another. The answer is the ownership
tags: a pre-existing bucket is adopted only when its `managed-by` and `owner` tags match this
operator and this CR, and a mismatch is an `ownershipCollisionError` — a definitive fault that
sets `Ready=False` and does not requeue, so it neither manages nor touches the foreign bucket
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D2).

One case is deliberately *not* a collision: a pre-existing bucket with no tags at all is claimed if
it is **empty**, because that is exactly the state an operator crash between bucket creation and
tag write leaves behind. A non-empty untagged bucket is refused. The mechanism, and what an empty
untagged bucket therefore means for a bystander, belong to
[ownership-and-attribution.md](ownership-and-attribution.md).

## How a change to the isolation rules reaches a bucket that already exists

The policy is the operator's document and the operator re-asserts it: every reconcile computes the
document the bucket should have and compares it structurally against the live one — key order and
whitespace ignored — and rewrites only on a difference
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8). A manual change
is therefore reverted rather than preserved, which is the self-healing property Layer 2 rests on.

What that does *not* give you is immediate propagation, and the reason is recorded because it once
failed in production. On 2026-07-22 an upgrade shipped an action the container registry needed and
an already-provisioned bucket silently kept its old policy: the `Bucket` watch fires on generation
and annotation changes, and an operator upgrade produces neither, so nothing re-reconciled the
bucket at all; during the rolling update the outgoing and incoming pods both reconciled, so the old
version could re-apply the stale document after the new one had written the correct one. Both
halves are closed, and both are settings an operator can turn off:

| Setting | Value | What it does for isolation |
| --- | --- | --- |
| `--drift-resync-interval`, env `DRIFT_RESYNC_INTERVAL`, Helm `driftResyncInterval` | `10m` &nbsp;`# default`, `0` disables | Re-reconciles every provisioned `Bucket` on a timer, so a policy change in a new operator version converges across the fleet within one interval instead of never |
| Helm `leaderElection.enabled` | `true` &nbsp;`# default` | One reconciling pod at a time, so a rolling update cannot have the outgoing version re-apply a stale document |

The two do not fully overlap: the single rollout that first turns leader election on still has an
outgoing pod that predates it, and the periodic resync is what heals whatever that rollout leaves
behind. Setting `driftResyncInterval` to `0` makes policy propagation purely event-driven and
therefore leaves an upgraded fleet on its old documents until each CR is touched.

## Open gaps

### H-1 An organisation-level role dissolves Layer 1, and nothing here can see it

**Mechanism:** Layer 1, the project-scoped service-account token.
**Adversary:** none required — this is a misconfiguration that silently removes a boundary, after
which any tenant of one project reaches another project's data through the operator that serves it.
**Live today:** yes, in the sense that nothing detects it. Whether any deployment actually has such
a role is a property of each installation and is not verifiable from this repository.

Layer 1 holds only while the service account's roles are assigned at project scope, which is the
rule and not an inference: a role that cascades into a second project is forbidden, and the operator
neither detects nor compensates for one
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D5). A role granted
at organisation level cascades into every project underneath it, and from inside the operator a
token with too many roles and a token with the right ones are indistinguishable: there is no error,
no log line, and the cross-project calls simply start succeeding. No code in this product can close
this, and adding one would not help — the operator would be asking the same over-privileged token.

**What an operator can do meanwhile:** verify the role assignment at account-setup time, on the
provider side, and re-verify it when roles are changed. Which minimal project-scoped role the
service account should hold is not yet settled, so deployments today are granted something broader
than the decision wants ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md),
*Residual risks*). Running each cluster against its own project keeps the blast radius of a
mistake to that pair.

### H-2 A new bucket is open to the project between its creation and its first policy write

**Mechanism:** Layer 2, the gap between `CreateBucket` and the first `PutBucketPolicy` of the same
reconcile pass.
**Adversary:** a principal that already holds a credentials group in the *same* StackIT project —
so another tenant of this operator, or anyone holding any key of the project.
**Live today:** yes, and unavoidable in this shape: a bucket must exist before a policy can be set
on it, and the provider's default for a policy-less bucket is open.

The window is one reconcile pass wide — `ensureBucket` creates and tags the bucket, then
`provisionCredentialsAndClone` writes the policy — and a bucket in that window holds no data yet,
because the workload credential that could write to it is only minted afterwards. The exposure is
therefore write-and-read access to an empty new bucket by an existing project principal, not access
to anyone's data. A provider error between the two steps widens the window to the retry interval.
A `Bucket` carrying `spec.allowRecreate` is the one way into this window that nobody opens
deliberately: the operator rebuilds a vanished bucket by itself, unattended, rather than somebody
creating a CR ([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D9).
The window has the same width and the rebuilt bucket is just as empty — a completed clone is not
re-run into it (D11).

**What an operator can do meanwhile:** nothing inside this product. The containment is Layer 1 —
keeping the number of principals in the project small and every key in it accounted for. A key
that is unaccounted for in the project is already past Layer 2 for every bucket, not just new ones.

### H-3 The lockout property and its escape hatch are both unmeasured

**Mechanism:** the admin exemption in the first policy statement, and the control-plane delete
offered as a second net under it.
**Adversary:** none — this is a self-inflicted, irreversible failure, reachable by a defect or by a
manual policy edit that drops the admin principal.
**Live today:** the *guard* is live and the code holds it unconditionally. What is not established
is the severity it guards against, and whether any recovery exists.

Two claims stand unverified, and they compound. First: that this backend can lock out even the
account root through a bucket policy. That comes from the backend vendor's documentation and was
deliberately never reproduced, because reproducing it means destroying a bucket's manageability.
Second: that a control-plane bucket delete succeeds regardless of the bucket policy. That is
plausible for an ordinary bucket and is offered as the escape hatch, but it has never been measured
against the one case it is for — a bucket whose policy denies the operator's own identity. And it
would not be reached by the operator anyway: the emptiness guard before any deletion is a
data-plane read with the admin credential, so a locked-out bucket parks before the control-plane
call ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)).

The consequence is that a lockout is documented as unrepairable and the guard is treated as an
invariant of the document itself rather than as a rule the inputs are trusted to respect — which is
why the admin and workload URNs are filtered inside `BuildIsolationPolicy` and not only where the
reader list is assembled.

**What an operator can do meanwhile:** never hand-edit a bucket policy this operator manages — the
drift check would rewrite it on the next pass anyway, and an edit that removes the admin principal
is the one edit no later pass can undo. If a lockout is suspected, recovery is provider-side
intervention, not an operator action.

### H-4 A compromised workload credential destroys its own bucket's data, and the policy is not a network boundary

**Mechanism:** Layer 2 bounds a workload credential to *one* bucket; within that bucket it grants
`s3:GetObject`, `s3:PutObject`, `s3:PutOverwriteObject` and `s3:DeleteObject`.
**Adversary:** anyone who obtains a workload credential — a leaked Secret, a compromised pod in the
`Bucket`'s namespace, a credential copied out of the cluster.
**Live today:** yes, by design.

The policy bounds the blast radius to one bucket; it does not protect the contents of that bucket.
Object lock and retention are deliberately denied to the workload, so they are not available as a
mitigation from inside either
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D4).

There is also no network condition anywhere in the document. Restricting by source IP was
considered when the policy model was drafted and not taken: workload traffic leaves a cluster
through egress addresses shared by every workload of that cluster, so it would separate clusters at
best and never two workloads, and it breaks the moment egress is renumbered. A workload credential
is therefore usable from anywhere on the internet that can reach the S3 endpoint.

**What an operator can do meanwhile:** treat the credentials Secret as the boundary it is — who can
read Secrets in a namespace can read that namespace's buckets
([credentials-and-secrets.md](credentials-and-secrets.md)) — and rotate a suspect credential
through the rotation annotation
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
D8). Rotation deletes every existing key of the group *before* the replacement is created (same
record, D5), and there is no overlap window (D9), so the compromised key is dead from that moment
and any workload still holding it fails until it re-reads the Secret.

For a deployment whose clusters do have stable, distinct egress addresses, a source-IP condition
remains available as a design option and would need its own decision record.

### H-5 The workload action list is a manual approximation, and it fails in the client

**Mechanism:** the inverted whitelist of the second statement — every action not listed is denied.
**Adversary:** none; this is a correctness and availability gap, not an exposure. It is on this page
because it is the reason the exemption list grows, and every growth is a deliberate widening of what
a workload may do.
**Live today:** yes.

The list is maintained by hand against the backend's documented action set. The backend is NetApp
StorageGRID, whose action vocabulary is a superset of the AWS one and which splits operations AWS
folds into `s3:PutObject`, so a list that merely looks reasonable breaks clients silently at the one
call they make rarely. Two production incidents were diagnosed exactly that way — a container
registry failing every blob push on 2026-07-22 for three multipart-management actions, and CNPG
backups failing at their last step on 2026-08-05 for `s3:PutOverwriteObject`, which gates writes to
an already-existing key. The direction of failure is the right one: an unknown action is denied, not
allowed. The cost is that the symptom appears as an `AccessDenied` inside somebody else's client,
never in the operator's logs or in the `Bucket` status, and widening the list is a code change and a
release rather than a setting.

**What an operator can do meanwhile:** when a client fails with `AccessDenied` against its own
bucket, compare the action it attempted against the list in this page's `<details>` block before
suspecting the credential. The inclusion criterion that decides whether such an action may be added
is in [../developer/bucket-policy.md](../developer/bucket-policy.md).

### H-6 Nothing declares which project a deployment is supposed to serve

**Mechanism:** Layer 1's binding. The project id is read from the `projectId` field of the mounted
service-account key and from nowhere else; there is no project setting and no `Bucket` field that
selects one ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D1).
**Adversary:** none required — like [H-1](#h-1-an-organisation-level-role-dissolves-layer-1-and-nothing-here-can-see-it)
this is a misconfiguration, and it is the mirror image of it. In H-1 the boundary dissolves; here the
boundary holds perfectly, around the wrong project.
**Live today:** yes, and undetected at the moment the mistake is made. There is no setting to
declare the expected project id and no check at startup that the mounted key matches it
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md), *Residual risks*). One
check does run later, against candidate keys only; it is described below and it does not close this.

Mounting the wrong key moves an entire cluster's provisioning into a different project, and no
configuration looks wrong while it happens: the chart references a Secret by name, the key inside it
is opaque, and every `Bucket` reconciles to `Ready` in the project the key names. The buckets are
correctly isolated from each other and from that project's other tenants — they are simply in a
project that cluster was never meant to touch, alongside whoever else is a tenant of it. The project
binding is also fixed for the process lifetime, so the mistake persists until someone restarts the
operator with a corrected key
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D8).

**The runtime check, and exactly what it buys.** The operator re-reads its key file while it runs,
and a candidate key whose `projectId` differs from the one the *running process* loaded at start is
refused: nothing is swapped, the running key stays in use, and
`stackit_s3_provisioner_sa_key_reload_failing` goes to `1`
([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) D4). So a
live operator cannot be re-pointed at another project by a write to its Secret — and that is the
whole of it. The reference point is the previous process's own memory, so a restart erases it: a pod
that comes up with a foreign key mounted has nothing to compare against and adopts whatever the file
says. That is the path a botched Secret update leads to rather than an exotic one, because a refused
rotation invites exactly that restart: the operator will not take the new key, and restarting it is
the obvious way to make it. The check is one string comparison with no state and no configuration,
and it is kept on those terms, not because it protects anything.

**What bounds the damage is
[ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md), not that check.** A
`Bucket` whose provisioned bucket the operator can no longer find reports it as missing; nothing is
re-created and nothing is overwritten in the project the wrong key names. That guard never asks
which project the operator is pointed at, only whether the bucket it provisioned is still there, so
unlike the `projectId` comparison it survives every restart. It does not help a `Bucket` created
*while* a foreign key is in use: that one has no history to contradict and is provisioned in
whatever project the key names.

**What an operator can do meanwhile:** read the `projectId` out of the key before installing it and
compare it against the project this cluster is meant to serve, then repeat that comparison on every
key replacement. A replacement is the moment the two can drift apart, and it is no longer also a
restart somebody watches — a proven key is picked up on its own — so the comparison has to be a step
in the rotation procedure ([../operations/credentials.md](../operations/credentials.md)) rather than
a side effect of a pod coming back. Running one project per cluster keeps the damage of a swap to
the two projects involved.

## What this does not cover

* **Where credentials live and what reading one costs.** The service-account key, the operator's
  admin credential, the workload Secret and the clone staging Secret, their custody and their
  rotation: [credentials-and-secrets.md](credentials-and-secrets.md).
* **What creating a `Bucket` hands a Kubernetes subject**, the operator's own privilege footprint,
  and the two user-facing ClusterRoles: [rbac-and-privilege.md](rbac-and-privilege.md).
* **How the operator decides a cloud object is its own** — the ownership tags, the credentials-group
  attribution and the wipe path: [ownership-and-attribution.md](ownership-and-attribution.md).
* **Encryption**, at rest or in transit, beyond the fact that the S3 endpoint is addressed over TLS
  and that `s3:PutEncryptionConfiguration` is denied to the workload. Nothing in this product
  configures bucket encryption, and this page makes no claim about what the provider does by
  default.
* **Availability and quota isolation.** Nothing here stops one tenant's traffic or object count from
  affecting another's; the only per-tenant limits in the product are the measurement guards of
  [ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D8, which bound the
  operator's own listing work and are not a tenancy mechanism.
* **The isolation of the clone path.** A clone reads a foreign bucket with a credential the `Bucket`
  supplies and runs as a Job in the operator's namespace; what that Job may reach is
  [ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) and
  [credentials-and-secrets.md](credentials-and-secrets.md).
* **How to use read grants**, revoke them or check them:
  [../operations/read-grants.md](../operations/read-grants.md). **How the policy document is built,
  compared and extended**: [../developer/bucket-policy.md](../developer/bucket-policy.md).
* **Reporting a vulnerability in this operator.** The channel is GitHub private vulnerability
  reporting on this repository, and [SECURITY.md](../../SECURITY.md) is where it is described: what
  to send, what to leave out, and the fallback if the setting is not switched on. That file is the
  reporting convention and nothing else — the isolation design, and every gap named above, stay
  here.
