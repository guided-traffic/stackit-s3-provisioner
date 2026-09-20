# Bucket policy

Every bucket this operator provisions carries one S3 bucket policy, and that document is the
only thing separating one workload from another inside the STACKIT project. This page covers how
the document is built, which action belongs in which exemption list and why, how it is compared
against the live one, and what must never be added to it. It does not argue the design — that is
[ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) for the isolation
document and [ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md)
for the optional third statement. If you want the threat model and what this isolation does *not*
defend against, you want [../security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md);
if you want to know what a granted reader may do from an operator's seat,
[../operations/read-grants.md](../operations/read-grants.md).

## Where the code is

| File | What it holds |
| --- | --- |
| [`stackit/s3.go`](../../stackit/s3.go) | `BuildIsolationPolicy`, the two action lists and their reasoning, `sanitizeReaderURNs`, `WorkloadPrincipalFromPolicy`, `PoliciesEquivalent`, and the `S3Admin` data-plane client that writes the document |
| [`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go) | `ensureBucketPolicy` (drift check plus write), `resolveReadGrants` (which principals become readers), `groupFromPolicy` (reading the document back), and the clone hold in `provisionCredentialsAndClone` |
| [`stackit/s3_test.go`](../../stackit/s3_test.go) | The offline invariants: the byte-exact pre-grant document, the denied-by-design list, the read-only subset property, reader sanitizing and ordering |

This page names files and functions on purpose. Whoever moves them updates this page in the same
change.

## The document, end to end

`BuildIsolationPolicy(bucket, adminURN, workloadURN, readerURNs)` in
[`stackit/s3.go`](../../stackit/s3.go) is the single source of truth for the shape. It is a pure
function: no provider call, no clock, no map iteration order reaching the output. Everything else
— the reconciler, the layer-2 integration test — goes through it, so there is exactly one place
where the document can be wrong.

It returns two statements, or three when at least one reader survives sanitizing. Both mandatory
statements are `Deny`, because access inside the project is open by default and only `Deny`
removes it ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D2).

```jsonc
// example — the two-statement document, whitespace added for reading
{
  "Statement": [
    {
      "Sid": "deny-all-except-admin-and-workload",
      "Effect": "Deny",
      "NotPrincipal": { "AWS": ["<admin group urn>", "<workload group urn>"] },
      "Action": ["s3:*"],
      "Resource": ["arn:aws:s3:::<bucket>", "arn:aws:s3:::<bucket>/*"]
    },
    {
      "Sid": "workload-objects-only",
      "Effect": "Deny",
      "Principal": { "AWS": "<workload group urn>" },
      "NotAction": [ /* the 17 actions of the exemption list, in list order */ ],
      "Resource": ["arn:aws:s3:::<bucket>", "arn:aws:s3:::<bucket>/*"]
    }
  ]
}
```

Three properties of the emitted bytes are worth knowing before you touch the builder:

* **There is no `Version` element.** The builder marshals `map[string]any{"Statement": …}` and
  nothing else — verified in [`stackit/s3.go`](../../stackit/s3.go). The backend accepts that form:
  the layer-2 integration tests set exactly this document against the real StorageGRID with the
  admin key. Not verified from this repository: which shape the documents on already-provisioned
  buckets of a given installation carry today.
* **Object keys come out sorted, statements do not.** The statements are built as
  `map[string]any` and marshalled with `encoding/json`, which sorts object keys; so a statement
  serialises as `Action, Effect, NotPrincipal, Resource, Sid`. The `Statement` array keeps the
  order the builder appends in: blanket deny, workload, readers.
* **`json.Marshal`'s error is discarded** in `BuildIsolationPolicy` (`b, _ := json.Marshal(doc)`).
  Every value in the document is a string, a `[]string` or a `map[string]any` of those, so there
  is no unmarshalable input reachable — but it is a deliberate discard, not an oversight, and a
  future value type that can fail to marshal would silently produce `""`.

`Principal` in statement 2 is a bare string while `NotPrincipal` in statement 1 and `Principal` in
statement 3 are arrays. That asymmetry is in the builder and pinned by the byte-exact test; do not
"normalise" it, because the pre-grant document is compared literally (see *Drift*).

## Statement 1 — the blanket deny

`deny-all-except-admin-and-workload` denies `s3:*` on the bucket and its objects to every principal
*not* in the `NotPrincipal` list. This is the statement that separates workloads: every other
credentials group of the project falls under it
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D3).

The exemption list is assembled in the builder as `[adminURN, workloadURN]` plus the sanitized
readers, in that order. A principal missing from it is denied regardless of what any later
statement says — which is exactly why a granted reader has to be added *here* as well as getting
its own statement 3 ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D7).

The admin URN is in this list in every document the operator writes, and is never named as a
restricted principal ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D5).
`TestBuildIsolationPolicy_AdminAlwaysExempt` asserts the crude version of that (the admin URN
appears in the document at all); the sanitizing tests below assert the sharp version.

## Statement 2 — the workload exemption list

`workload-objects-only` is a `Deny` with `NotAction` on the workload principal, so
`workloadAllowedActions` is an **inverted whitelist**: every S3 action *not* in the list is denied
to the bucket's own workload credentials. The list fails closed. An action nobody thought of is
denied, and the symptom surfaces as `AccessDenied` inside the client, not in the operator's logs.

### The inclusion criterion

An action qualifies for the list only if it operates on **object data, object metadata or object
listings** ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D4).
Anything that changes *who* may access the bucket, forwards its contents elsewhere, pins an
object's lifetime, destroys history or reconfigures the bucket stays denied. Before 2026-08-05 the
list grew action-by-action as clients broke; since then it is maintained against the backend's
documented action set under that one criterion, which is why it went from eight entries to
seventeen in a single change.

The backend is NetApp StorageGRID. Its action vocabulary is a **superset** of the AWS one, and it
splits operations AWS folds into `s3:PutObject`. A list that merely looks reasonable against the
AWS reference therefore breaks clients silently, at the one call they make rarely. The comment
block above `workloadAllowedActions` in [`stackit/s3.go`](../../stackit/s3.go) carries the
per-group reasoning and links the vendor's action reference; it is the most accurate statement of
the criterion in the repository and should be updated before this page when the list changes.

<details>
<summary>The seventeen actions, grouped as the code groups them</summary>

| Group | Actions | Why it is object work |
| --- | --- | --- |
| Object data | `s3:GetObject`, `s3:PutObject`, `s3:PutOverwriteObject`, `s3:DeleteObject` | The workload's own payload |
| Listing and endpoint discovery | `s3:ListBucket`, `s3:ListBucketVersions`, `s3:GetBucketLocation` | Reading what is there; SigV4 clients resolve the region before their first request |
| Multipart management | `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts`, `s3:AbortMultipartUpload` | Resuming and cleaning up the principal's own chunked upload |
| Object tagging | `s3:GetObjectTagging`, `s3:PutObjectTagging`, `s3:DeleteObjectTagging` | Object metadata, set in passing by rclone `--metadata`, Velero and `aws s3 cp --tagging` |
| Version-aware reads | `s3:GetObjectVersion`, `s3:GetObjectVersionTagging` | Reads only; dormant while versioning is off, and the workload cannot turn it on |
| Read-only configuration probes | `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration` | Issued by several SDKs before a write; they expose nothing, and denying them only produces confusing client-side errors |

Actions the code records as **denied by design**, with the failure each prevents. Most of them are
also asserted by `TestWorkloadAllowedActions_DeniedByDesign`, so adding one to the exemption list
turns the offline suite red — but the two lists are not identical: the test additionally forbids
`s3:*` and `s3:CreateBucket`, and it does **not** cover the two ACL actions the comment names. An
ACL action added to the exemption list would compile and pass today.

| Group | Kept denied | What it would allow |
| --- | --- | --- |
| Policy | `s3:GetBucketPolicy`, `s3:PutBucketPolicy`, `s3:DeleteBucketPolicy` | The workload lifting its own restrictions; the read additionally leaks the admin URN |
| Forwarding | `s3:PutReplicationConfiguration`, `s3:PutBucketNotification`, `s3:PutBucketMetadataNotification` | Exfiltration to a destination the workload chooses |
| Lifetime pinning | `s3:PutObjectRetention`, `s3:PutObjectLegalHold`, `s3:PutBucketObjectLockConfiguration`, `s3:PutBucketCompliance`, `s3:BypassGovernanceRetention` | An undeletable bucket, which breaks finalizer teardown ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)) |
| History | `s3:DeleteObjectVersion`, `s3:DeleteObjectVersionTagging`, `s3:PutObjectVersionTagging` | Destroying or rewriting historic versions |
| Reconfiguration | `s3:PutBucketVersioning`, `s3:PutLifecycleConfiguration`, `s3:PutEncryptionConfiguration`, `s3:PutBucketCORS`, `s3:PutBucketTagging`, `s3:DeleteBucket` | Bucket configuration, including the ownership tags attribution rests on ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)) |
| ACLs | `s3:GetObjectAcl`, `s3:GetBucketAcl` | Nothing needed; ACLs are unused in this product |

</details>

### Actions shared by both lists are constants

The nine actions that appear in both `workloadAllowedActions` and `readerAllowedActions` are
declared once as constants (`actGetObject`, `actListBucket`, …) in
[`stackit/s3.go`](../../stackit/s3.go), so a typo is a build failure instead of a silently
over- or under-permissive policy, and both lists spell the same action the same way.

That is the only thing the compiler does here. It does **not** enforce that the reader list stays
a read-only subset: every one of those constants — including the multipart ones the reader list
deliberately omits — could be pasted into either list and still compile. The subset property is
held by a test, named below.

## Worked example: the three multipart-management actions, 2026-07-22

The GitLab container registry returned `500 Internal Server Error` on every blob push. The registry
pod's log showed the real cause:

```text
"error":"s3aws: AccessDenied: Access Denied\n\tstatus code: 403", "msg":"error resolving upload"
```

The permission matrix was measured directly against the registry's own bucket with the credentials
from its Secret, then reproduced with a second bucket's credentials, which is what proved it was
the policy and not one bucket:

| Operation | Result |
| --- | --- |
| `ListBucket`, `PutObject`, `GetObject`, `DeleteObject` | OK |
| `CreateMultipartUpload`, `UploadPart`, `CompleteMultipartUpload` | OK — they map to `s3:PutObject` |
| `ListMultipartUploads` | **AccessDenied** |
| `ListParts` | **AccessDenied** |
| `AbortMultipartUpload` | **AccessDenied** |

The Docker/GitLab registry S3 driver calls `ListMultipartUploads` on *every* blob commit, which is
the "resolving upload" step; the 403 became a 500 at the registry's edge. The fix added
`s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts` and `s3:AbortMultipartUpload` to
`workloadAllowedActions`, and the rule it left behind is
[ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D7: **multipart
management is object work, not bucket management.** Uploading a part is `s3:PutObject`, but
resuming and cleaning up a chunked upload are distinct actions, and clients issue them on the
ordinary write path — they act on the principal's own in-flight upload in its own bucket, so they
change nothing about who may reach the bucket.

The second half of the incident is the propagation lesson: the fix shipped, and an already
provisioned bucket kept its old document, because nothing re-reconciled it. That is the origin of
the periodic drift resync and of leader election being on by default; the causes and remedies are
in [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) under
*Consequences*, and the mechanism is under *Drift* below.

## Worked example: `s3:PutOverwriteObject`, 2026-08-05

CNPG/barman backups failed at their last step. Writes to new keys worked, multipart worked, and a
plain `PutObject` on an **existing** key did not: barman writes `base/<id>/backup.info` twice per
backup, and the second write is an overwrite.

`s3:PutOverwriteObject` is a StorageGRID-specific action with no AWS equivalent. It gates any write
to a key that already exists — data, user metadata or tags — so on this backend `PutObject` on a
fresh key and `PutObject` on an existing key are two different authorisations. Without it, every
client that rewrites a key breaks: barman, restic, Terraform state, registry drivers.

**It is not a security boundary, and this is the part worth remembering.** `s3:DeleteObject` is
granted anyway, so anyone holding the workload credential reaches the same end state with a delete
followed by a put. NetApp documents WORM semantics as arising from denying `s3:PutOverwriteObject`
*together with* `s3:DeleteObject`, which was never the configuration here. The action was granted
on 2026-08-05 without a measurement of what its absence had been protecting — recorded as
unverified in [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) under
*Residual risks*, and repeated here because a reviewer trimming the list will meet this action
first and it looks like a write escalation.

## The invariant: no tag-based condition keys, ever

There is no `Condition` element anywhere in the builder — verified by reading
[`stackit/s3.go`](../../stackit/s3.go): the only occurrence of the word `Condition` in the package
is the comment stating this invariant, and no statement map carries a `Condition` key.

Keep it that way for as long as the workload may write object tags. `s3:PutObjectTagging` is in the
exemption list, so a condition on `s3:ExistingObjectTag/*` or `s3:RequestObjectTag/*` would let the
workload **rewrite its own permissions** by tagging an object. Granting tagging is only harmless
because no access decision in this document reads a tag. Whoever wants tag-based conditions removes
the tagging actions from the exemption list first, in the same change
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D6).

Not verified: that this backend would honour a tag-based condition key at all. The combination was
never tried, deliberately. The invariant exists so it is not tried by accident.

## `Principal` and `Resource` use two different namespaces

This looks like a bug in review and is not:

| Element | Value shape | Where it comes from |
| --- | --- | --- |
| `Principal` / `NotPrincipal` | `urn:sgws:identity::<account>:group/<id>` # example | The credentials group's `urn`, returned by `CreateCredentialsGroup` in [`stackit/client.go`](../../stackit/client.go) and carried through unmodified |
| `Resource` | `arn:aws:s3:::<bucket>` and `arn:aws:s3:::<bucket>/*` # example | Composed in `BuildIsolationPolicy` from the bucket name |

StorageGRID identifies principals with its own `urn:sgws:` scheme while keeping AWS-shaped `arn:`
resources, and mixing them in one statement is the correct form for this backend. Never "fix" the
principal into an `arn:`, and never derive a principal by string surgery — the URN is opaque and
arrives from the provider.

## Statement 3 — granted readers

The third statement, `granted-readers-read-only`, exists only when at least one reader survives
sanitizing. It is again a `Deny` with `NotAction`, so `readerAllowedActions` is the same kind of
inverted whitelist, applied to the granted principals.

`readerAllowedActions` is a strict read-only subset of `workloadAllowedActions`. Nine actions:
`s3:GetObject`, `s3:GetObjectVersion`, `s3:ListBucket`, `s3:ListBucketVersions`,
`s3:GetBucketLocation`, `s3:GetObjectTagging`, `s3:GetObjectVersionTagging`,
`s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration`.

Two exclusions are deliberate and are the ones a future contributor will want to undo:

* **No multipart listing or abort.** `s3:ListBucketMultipartUploads` and
  `s3:ListMultipartUploadParts` expose the *owner's* not-yet-committed uploads — key names and part
  layout — and `s3:AbortMultipartUpload` destroys them. Reading finished objects never needs them,
  and `s3cmd`/`aws-cli` only issue them for uploads they started themselves.
* **No write of any kind**, including tagging. A reader that could tag could change metadata the
  owner relies on.

`s3:GetBucketLocation` *is* included, because SigV4 clients resolve the bucket region before their
first request and fail confusingly without it. It discloses nothing beyond the region the reader is
already addressing.

### The subset property is held by a test, not by the compiler

[`TestReaderAllowedActions_ReadOnly`](../../stackit/s3_test.go) is the invariant test behind the
whole grant feature. It asserts four things at once:

1. every reader action is also granted to the bucket owner (no privilege inversion);
2. no reader action carries a mutating prefix — the list it checks is `s3:Put`, `s3:Delete`,
   `s3:Create`, `s3:Abort`, `s3:Bypass`, `s3:Restore`, `s3:Replicate`;
3. the three multipart actions are absent, asserted by name so a "just add the missing list
   actions" edit has to face the test rather than slip past a generic prefix rule;
4. `s3:ListBucket`, `s3:GetObject` and `s3:GetBucketLocation` are present, so the feature still
   delivers `ls` and `get`.

Deleting that test deletes the guarantee — that is stated as a residual risk in
[ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md), and it is
literally true: nothing else in the build prevents a write action in the reader list.

## Sanitizing the reader list

`sanitizeReaderURNs` runs as the **first step inside the builder**, not in the caller that
assembles the list. Every caller therefore inherits the protection for free — including any future
one. It trims each candidate, drops empty entries, drops the admin and workload URNs, collapses
duplicates and sorts what remains, returning `nil` for an empty result so callers can test with
`len()` and the document stays at two statements.

The two exclusions implement
[ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D5 /
[ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D7, and they
are not symmetric in consequence:

| Excluded principal | What would happen without the filter |
| --- | --- |
| The admin URN | The admin key is confined to the reader action set on this bucket, which excludes `s3:PutBucketPolicy` — an **unrepairable lockout**, because repairing the policy requires the write the bad policy denies |
| The bucket's own workload URN | A second, *narrower* `Deny` lands on the owner. Denies intersect rather than override, so the owner silently loses write access to its own bucket |

Comparison is **trimmed against trimmed**. `adminFromSecret` in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go) reads
the admin URN straight out of Secret data and does not trim it, so a whitespace-padded admin URN is
reachable in practice; comparing a trimmed candidate against a raw `adminURN` would let the padded
value through the lockout guard. `TestSanitizeReaderURNs_TrimmedComparison` pins both directions —
padded value in the configuration, padded value in the reader list.

A self-grant — a Bucket naming itself in `spec.grantReadAccess` — meets three layers, deliberately,
and they fire in this order:

1. **Admission.** A root-level CEL rule on the CRD in
   [`api/v1/bucket_types.go`](../../api/v1/bucket_types.go)
   (`self.spec.grantReadAccess.all(g, g.name != self.metadata.name)`) refuses the self-reference, so
   such an object is never stored.
2. **Reconcile.** `resolveReadGrants` in
   [`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go)
   skips a reference whose name equals the Bucket's own, with a `ReadGrantPending` event, so
   `status.grantedReadTo` stays truthful for any object that predates the CEL rule.
3. **Builder.** `sanitizeReaderURNs`, described above, drops the bucket's own workload URN, so the
   self-grant collapses to a no-op even if it reaches the document.

Sorting is not cosmetic: it is what keeps the drift comparison from reporting a phantom change when
the reader list arrives in a different order.

### Without a grant, the document is byte-identical

With an empty reader set the builder emits exactly the two-statement document, and that document
is pinned as a literal string constant, `preGrantPolicy`, captured from the commit that preceded
the grant feature. `TestBuildIsolationPolicy_NoReadersUnchanged` compares the builder's output
against that literal, and against seven reader inputs that must all sanitize to nothing: `nil`, an
empty slice, an empty string, whitespace only, the admin URN alone, the workload URN alone, and
both together.

Comparing the new builder against itself would prove nothing about the upgrade path. The literal is
the point: a bucket that does not use grants must keep the policy it already has, or the first
reconcile after an operator upgrade rewrites every bucket in the project
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D8).

## Where the reader principals come from

The builder takes URNs; deciding *which* URNs is `resolveReadGrants` in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go). The
rule that matters for this page: a reader URN is **never** read from the referenced Bucket's
`status.credentialsGroupURN`. Writing a Bucket's status is a weaker permission than reading the
workload Secrets it describes, so a forged value would be pasted verbatim into someone else's
policy — and one forged entry naming the admin group is the unrepairable lockout above.

Instead the reference resolves in the grantor's own namespace to a Bucket CR, that CR's physical
bucket is looked up, the bucket must carry that CR's ownership tags, and the group is the one the
*bucket* attributes — the `credentials-group-id` tag, or the principal of the bucket's own
`workload-objects-only` statement
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3,
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D5/D6). The
attribution chain itself is [bucket-identity.md](bucket-identity.md); the sanitizing above is the
second line of defence behind it, not the first.

An entry that does not resolve is skipped with a `ReadGrantPending` warning event and never blocks
the grantor's readiness. The enumerated skips are
[ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D5; where it
sits in the pass, and the grantee watch that wakes grantors, is
[reconcile-pipeline.md](reconcile-pipeline.md).

## The reader hold during a clone

While `spec.cloneFrom` is still copying, granted readers stay out of the document entirely. The
mechanism is in `provisionCredentialsAndClone`: the policy write is wrapped in a local closure
`applyPolicy(readers, granted)` that both writes the document and records `status.grantedReadTo`
next to the write that makes it true. While `b.ClonePending()` holds, it is called as `applyPolicy(nil, nil)`.

The isolation policy itself is still written first, so a filling bucket is never open to the rest
of the project — only the grants are withheld.

The reason is worth stating because it is not obvious: `holdSecretUntilCloned` covers only the
bucket's **own** workload, by withholding its Secret. A granted reader already holds working
credentials of its own, so the policy is the only thing that can hold it back
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D10,
[ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D8).

When `ensureClone` reports the copy finished, `provisionCredentialsAndClone` calls
`applyPolicy(readerURNs, grantedTo)` a **second time in the very same pass**, so the grants land immediately rather than waiting for the
next reconcile. `TestReadGrantHeldBackDuringClone` covers it offline. The clone machinery itself is
[clone.md](clone.md).

## Drift: how the live document is compared

`ensureBucketPolicy` in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go) is the
whole write path:

1. build an `S3Admin` for this bucket's endpoint with the operator's admin key
   (`newS3Admin` → `stackit.NewS3Admin`, path-style, SigV4);
2. compute the desired document with `BuildIsolationPolicy`;
3. read the live one with `GetBucketPolicy`;
4. if the read succeeded **and** `PoliciesEquivalent(current, desired)`, return without writing;
5. otherwise `SetBucketPolicy`.

The comparison is **structural, not textual**: `PoliciesEquivalent` parses both documents and
re-marshals them, which sorts object keys and drops insignificant whitespace, then compares the
normalised strings. That is what makes the check survive the backend reformatting the document on
read-back — StorageGRID does not necessarily hand back the bytes that were sent. If either side is
not valid JSON, it falls back to a trimmed byte comparison, so a corrupted live document compares
unequal and gets rewritten rather than crashing the pass.

Determinism on our side is what makes the comparison meaningful at all: sorted reader list, fixed
action-list order, fixed statement order. Without the sort, a reordered reader list would look like
drift, and the operator would rewrite the policy on every reconcile of every granting bucket.

Two behaviours to know before changing this function:

* **A failed read is treated as "needs writing".** The condition is `err == nil && equivalent`, so
  a transient `GetBucketPolicy` failure leads to a `SetBucketPolicy` with the correct document.
  That is safe — the write is idempotent and the desired document does not depend on the current
  one — but it means a flaky read costs a write, not a skipped pass. A bucket with no policy at all
  returns `NoSuchBucketPolicy`, which is the same path and the intended one on first provisioning.
* **The operator re-asserts, it does not merge.** A manual change to a bucket's policy is reverted
  on the next pass; a document changed by a new operator version reaches an already-provisioned
  bucket with no change to its custom resource
  ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8). *When* it
  reaches it is a separate question — the `Bucket` watch fires on generation and annotation
  changes, which an operator upgrade does not produce, so convergence is carried by the periodic
  drift resync (`--drift-resync-interval`, `10m` # default, `0` disables; wired in
  [`cmd/main.go`](../../cmd/main.go) and applied as the `RequeueAfter` of a successful pass).

## The `Sid` is an interface, not a label

`sidWorkloadObjectsOnly` (`"workload-objects-only"`) is read back by the operator.
`WorkloadPrincipalFromPolicy` in [`stackit/s3.go`](../../stackit/s3.go) locates that statement by
its `Sid` and returns its single principal — this is the migration path of
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md): a bucket
provisioned before the `credentials-group-id` tag existed still carries, *in its own policy*, the
operator's record of which group it trusts, and that policy is writable only with the admin key.

So renaming a `Sid` is a breaking change to buckets in the field, not a cosmetic edit.

The reader is deliberately lenient in one direction and strict in the other. Lenient: the principal
may arrive as a bare string or as a one-element list, and `{"AWS": …}` wrappers are unwrapped, since
the backend may normalise the form on read-back; unknown statements are ignored. Strict: anything
that does not name **exactly one** workload principal reports `ok=false` — an absent policy, a
foreign document, a statement with several principals — so the caller never attributes a group on a
guess. `groupFromPolicy` in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go) treats
a *failed read* of the policy as an error rather than as "no policy", because treating it as absent
would create a second group for a bucket that already has one and rotate its workload's credential.

## What the tests hold

| Test | File | What breaks if it is deleted |
| --- | --- | --- |
| `TestBuildIsolationPolicy` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | The two-statement shape: effects, principals, the 17 actions, both resource ARNs |
| `TestWorkloadAllowedActions_DeniedByDesign` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | The denied-by-design list; adding `s3:PutBucketPolicy` or `s3:*` to the exemption list would compile silently |
| `TestReaderAllowedActions_ReadOnly` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | The read-only subset property — the only thing enforcing it |
| `TestBuildIsolationPolicy_NoReadersUnchanged` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | Byte-identity with the pre-grant document, i.e. the inert-upgrade guarantee |
| `TestBuildIsolationPolicy_Readers` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | The three-statement shape, including readers being exempted in statement 1 |
| `TestBuildIsolationPolicy_ReaderSanitizing`, `TestSanitizeReaderURNs_TrimmedComparison` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | The lockout guard and the owner-narrowing guard, including the whitespace-padding path |
| `TestBuildIsolationPolicy_ReaderOrderDeterministic` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | Determinism, and with it the drift comparison |
| `TestWorkloadPrincipalFromPolicy` | [`stackit/s3_test.go`](../../stackit/s3_test.go) | The read-back contract the ADR 0002 migration depends on |
| The grant tests (15 functions, 11 of them `TestReadGrant*`) | [`internal/controller/reconciler_grants_test.go`](../../internal/controller/reconciler_grants_test.go) | Grant resolution at reconciler level: namespace scoping, self-reference, admin never granted, revocation, the clone hold, the grantee watch predicate |
| `TestIntegrationWorkloadCredentials` | [`stackit/credentials_integration_test.go`](../../stackit/credentials_integration_test.go) | The two-statement document evaluated by the real backend (build tag `integration`) |
| `TestIntegrationReadGrant` | [`stackit/grants_integration_test.go`](../../stackit/grants_integration_test.go) | The three-statement document evaluated by the real backend — owner keeps write, reader lists and gets, reader denied every write and both policy actions, ungranted workload locked out, admin still able to rewrite, revocation re-locks (build tag `integration`) |
| `TestCloudReadGrant`, `TestCloudReadGrantLifecycle` | [`test/e2e/cloud_test.go`](../../test/e2e/cloud_test.go) | The three-statement document through the operator against the real API (build tag `e2e`, `E2E_STACKIT=1`) |

What each suite costs to run is [testing.md](testing.md).

## What is wrong today, and what could not be verified

* **The exemption list is a hand-maintained approximation.** Two production clients have already
  been broken by an absent action, on 2026-07-22 and 2026-08-05. A third is a matter of time, and
  the product will learn about it from the client's error, not from its own logs or the Bucket's
  status. Extending the list is a code change and a release, not a setting an operator can turn.
* **The read-only subset property has exactly one guard.** A reviewer who deletes
  `TestReaderAllowedActions_ReadOnly` deletes the guarantee. Nothing in the type system helps.
* **`json.Marshal`'s error is discarded** in `BuildIsolationPolicy`. Unreachable with today's value
  types; a future one that can fail to marshal would yield an empty document and the write would be
  attempted with it.
* **Not verified: that a `Deny` on this backend can lock out even the account root.** It comes from
  the vendor's documentation and is the reason the admin exemption is an invariant of the document
  rather than a rule the callers are trusted to follow. Reproducing it means destroying a bucket's
  manageability, so it was deliberately never tried
  ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md), *Residual risks*).
* **Not verified: that a tag-based condition key would be honoured here.** The no-condition-key
  invariant is precautionary.
* **Not verified: that denying `s3:PutOverwriteObject` ever prevented anything.** See the worked
  example above.
* **Not verified: that the 2026-07-22 multipart fix was re-measured against the bucket that
  originally failed** after it shipped. The action list is asserted offline; no closing measurement
  is recorded.
