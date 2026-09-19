# Sharing a bucket read-only

By default a bucket is reachable by exactly one credential — the one the operator
mints for its own `Bucket` CR. Everything else in the STACKIT project, including
the sibling buckets of the same namespace, is locked out by the bucket's
isolation policy
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)).

A **read grant** is the one exception. It widens the credential a grantee already
holds so that it may additionally *read* the granting bucket — no second Secret,
no second access key. The grant is declared in the spec of the bucket **whose
data is at stake**
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D1),
and takes effect as a third statement in that bucket's policy.

This page covers the two fields involved — `spec.grantReadAccess` on the grantor
and `status.grantedReadTo` on the grantor — and what happens at their edges. The
complete `Bucket` field reference lives in [README.md](../../README.md) and
nowhere else.

Every name on this page — the namespace `gitlab`, the Buckets `gitlab-artifacts`
and `gitlab-backups`, the Secrets `*-s3` — is an example. The only real defaults
shown are marked `# default`.

---

## A working example: the data owner names the grantees

A backup job holds the credentials of `gitlab-backups` and needs to read the data
buckets next to it. The data buckets grant; the backup bucket declares nothing.

```yaml
# The bucket that OWNS the data. Apply one of these per bucket to be backed up.
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: gitlab-artifacts         # example
  namespace: gitlab              # example
spec:
  bucketName: gitlab-artifacts   # example
  secretRef:
    name: gitlab-artifacts-s3    # example
  grantReadAccess:               # example; the field is optional and has no default —
                                 # omitting it means "nobody but my own workload"
    - name: gitlab-backups       # example: the metadata.name of a Bucket CR in THIS
                                 # namespace (gitlab). Not a physical bucket name, not a
                                 # credentials-group name, and no namespace field exists:
                                 # cross-namespace is unexpressible by construction
                                 # (ADR 0001 D3). At most 32 entries; duplicates are
                                 # rejected by the API server, and naming this Bucket
                                 # itself is rejected too.
---
# The bucket that RECEIVES the access. Nothing here mentions the grant — it cannot:
# a consumer must never be able to widen its own access (ADR 0008 D1).
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: gitlab-backups           # example
  namespace: gitlab              # example
spec:
  bucketName: gitlab-backups     # example
  secretRef:
    name: gitlab-backups-s3      # example
```

Apply both, then confirm the grant is in effect:

```console
$ kubectl -n gitlab get bucket gitlab-artifacts -o jsonpath='{.status.grantedReadTo}'
["gitlab-backups"]
```

The credentials in `gitlab-backups-s3` can now list `gitlab-artifacts` and get its
objects with no change to the backup job's configuration beyond the bucket name it
addresses. They still cannot write to it, delete from it, or read its policy.

---

## What the reader gets, in one sentence

A granted reader may read finished objects and list the bucket, and may do nothing
else — the action set is fixed in the operator and is a strict read-only subset of
what the bucket's own workload may do
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D6).

| Granted to a reader | Withheld from a reader |
| --- | --- |
| `s3:GetObject`, `s3:GetObjectVersion` | every `s3:Put*`, `s3:Delete*` and `s3:Create*` action |
| `s3:ListBucket`, `s3:ListBucketVersions` | `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts` |
| `s3:GetObjectTagging`, `s3:GetObjectVersionTagging` | `s3:AbortMultipartUpload` |
| `s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration` | policy, replication, notification, lifecycle and object-lock configuration |
| `s3:GetBucketLocation` | anything at all on a bucket that did not grant it |

Two consequences that surprise people in practice:

- **Multipart listing is withheld on purpose.** It exposes the *owner's*
  not-yet-committed uploads, and aborting one destroys them. Reading finished
  objects never needs it, but a client configured to clean up "stale" multipart
  uploads will get `AccessDenied` on the granting bucket.
- **The reader cannot read the policy.** `s3:GetBucketPolicy` is denied, so a
  reader cannot inspect the document that confines it. Verify a grant by
  behaviour (see below), not by reading the policy with the reader's credential.

The exact boundary this draws between tenants, and what it does *not* defend
against, is in [../security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md).
How the document itself is assembled is in
[../developer/bucket-policy.md](../developer/bucket-policy.md).

---

## Why it is declared on the owner, and what that buys

Two properties of the system force the producer side
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D1,
Alternatives):

1. Access to a bucket is enforced by exactly one artefact — that bucket's own
   policy, written during that bucket's own reconcile. A consumer-declared grant
   would have to reach into a document it does not own.
2. Whoever may create a `Bucket` controls its whole spec. If a Bucket could name
   the buckets it wants to *read*, creating one CR would be enough to read a
   sibling's data, with no trace in the spec of the bucket being read.

What you get in exchange, operationally: **a bucket's complete access list is
readable in one object.** `kubectl -n <ns> get bucket <data-bucket> -o yaml` shows
every principal that may touch it. There is no second place to look, and a review
of the data bucket's manifest is a complete review of who reads it.

The cost is the mirror image: **nothing about the feature is visible on the
grantee.** A Bucket cannot tell from its own CR which foreign buckets it may read.
To answer "what can this credential reach?", search the namespace's grantors:

```console
$ kubectl -n gitlab get buckets \
    -o jsonpath='{range .items[?(@.status.grantedReadTo)]}{.metadata.name}{" -> "}{.status.grantedReadTo}{"\n"}{end}'
gitlab-artifacts -> ["gitlab-backups"]
gitlab-uploads -> ["gitlab-backups"]
```

### Where the reader principal comes from

The principal written into the policy is **never** taken from anything a namespace
user can write — not `status.credentialsGroupURN`, not an annotation, not the
Secret, and not the credentials group's display name
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3).
It is resolved through the grantee's *physical bucket*: that bucket must carry the
grantee's ownership tags, and the group is the one the bucket itself attributes
via its `credentials-group-id` tag, or — for a bucket provisioned before that tag
existed — via the principal of its own isolation policy
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D5/D6).

Three practical consequences:

- **Display names play no part.** They are not unique in a STACKIT project, and
  since [ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)
  no lookup is done by name anywhere. There is no "ambiguous name" outcome to
  handle.
- **A grantee the operator cannot prove is its own grants nothing.** It shows up
  as a permanently pending grant, not as a failure — see the next section.
- **Resolving a grant creates nothing.** No group, no bucket, no key is ever
  created on a grantee's behalf while a grantor reconciles
  ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D4).
  The one write it may do is completing the grantee's own attribution record —
  stamping back the `credentials-group-id`/`-urn` tags a legacy bucket was
  missing — which surfaces as a `CredentialsGroupAttributed` event **on the
  grantee**, emitted during the grantor's pass. That is expected, not a symptom.

---

## A grant never blocks the owner

An entry that cannot be resolved is skipped. The grantor still reaches `Ready`,
because the grantor owns the data and must not lose its own provisioning because a
consumer is missing
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D5).
Every skip raises the warning event `ReadGrantPending` on the grantor. Four of
the six outcomes below additionally write a `read grant pending` log line
carrying `grantee` and `reason`; the **self-reference** and the **grantee under
deletion** emit the event only and log nothing. Grepping the operator log alone
therefore misses those two — read the events.

| The reference | Event message (grantor, `Warning ReadGrantPending`) |
| --- | --- |
| Names no `Bucket` in the namespace | `Bucket "x" referenced in spec.grantReadAccess does not exist (yet); read access not granted` |
| Names a `Bucket` that has started deleting | `Bucket "x" referenced in spec.grantReadAccess is being deleted; read access revoked` |
| Names a `Bucket` whose bucket does not exist yet | `... has no bucket yet; read access not granted` |
| Names a `Bucket` that attributes no credentials group yet | `... has no credentials group yet; read access not granted` |
| Names a `Bucket` whose physical bucket is not that Bucket's | `... names bucket "n", which is not owned by that Bucket; read access not granted` |
| Names the grantor itself | `ignoring self-reference "x" in spec.grantReadAccess` |

Two things to note when grepping for these:

- **Two** of the six messages have a different shape from the rest: the
  self-reference (`ignoring self-reference …`) and the grantee under deletion
  (`… is being deleted; read access revoked`). Neither carries the
  `read access not granted` suffix the other four end with. Match on the event
  *reason* `ReadGrantPending`, not on the message text.
- The event is re-emitted on every grantor pass that still cannot resolve the
  entry, so a long-pending grant keeps producing events for as long as it is
  pending. Not verified, and this is the gap: how a given cluster's event recorder
  aggregates those repeats into a single counted line.

**A skipped grant is quiet by design.** The grantor is green while a consumer sits
without access, and there is no metric for it — the operator exports none for
grants (verified: the metric set in
[`internal/controller/metrics.go`](../../internal/controller/metrics.go) contains
nothing grant-related). `status.grantedReadTo` and the `ReadGrantPending` events
are the only signal.

### The one case that is not a skip

One narrow case fails the grantor's reconcile instead of skipping it. It needs
three things at once: the grantee's physical bucket carries a
`credentials-group-id` tag but **no** `credentials-group-urn` tag — that is, it
was tagged before the URN tag existed — the provider confirms that group exists
by id, and the same group is still missing from the group listing this pass
read. Only then does the grantor **fail and retry** rather than skip
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D5,
last row): the state is momentary and self-clearing, and skipping would silently
revoke a working reader. A grantee carrying **both** tags is answered from the
tags and never consults the listing, so it cannot produce this failure — which
covers every bucket provisioned since the URN tag was introduced
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D5/D6).

The failure is classified like any other non-definitive error
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)), with two
preconditions that decide what you actually see:

- The grantor keeps `Ready` and takes `status.degradedSince` +
  `ProviderReachable=False` **only if it has already observed its current spec**
  (`status.observedGeneration` equals `metadata.generation`), is in phase `Ready`
  with a true `Ready` condition, is not being deleted, and the operator's
  `providerDegradedGrace` is non-zero
  ([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D6). On the
  pass that first applies a spec edit — adding a grant is exactly that — none of
  it holds, so the grantor drops straight to `Ready=False`, phase `Failed`, with
  no `degradedSince` and no `ProviderReachable` condition. The hold only applies
  on a later pass, woken by the grantee watch or the drift resync.
- The reconcile error counts towards the `StackitS3ReconcileErrors` alert
  **unless the fleet-wide provider circuit is open**. While it is, the reconcile
  returns a requeue with no error at all, so nothing is counted, and the alert
  suppresses the whole window anyway
  ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md)). Both halves
  are covered in [provider-outages.md](provider-outages.md).

---

## Revoking a grant

Revocation needs no cleanup step — the grant lives in exactly one place, so there
is nothing else to undo
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D9).
Any of these drops the reader from the policy on the grantor's next reconcile:

```bash
# 1a. Remove ONE grantee from the owner's spec (the normal path). The path is an
#     index into the list, so read the current order first:
#       kubectl -n gitlab get bucket gitlab-artifacts \
#         -o jsonpath='{.spec.grantReadAccess[*].name}'
kubectl -n gitlab patch bucket gitlab-artifacts --type=json \
  -p '[{"op":"remove","path":"/spec/grantReadAccess/0"}]'   # example: the first entry

# 1b. Or remove ALL grants at once by dropping the whole list. With more than one
#     grantee this revokes every reader, not just the one you had in mind.
kubectl -n gitlab patch bucket gitlab-artifacts --type=json \
  -p '[{"op":"remove","path":"/spec/grantReadAccess"}]'

# 2. Or delete the grantee Bucket entirely — access is dropped as soon as
#    deletion starts, before its credentials group is actually gone.
kubectl -n gitlab delete bucket gitlab-backups
```

Verify, in this order — the S3 policy and the CR status converge separately, and
the status write can lose a `resourceVersion` race against the patch that
triggered it and only land on the requeue:

```console
$ # the reader is locked out again (see the verification recipe below)
$ aws --endpoint-url "https://$S3_ENDPOINT" s3api list-objects-v2 --bucket gitlab-artifacts
An error occurred (AccessDenied) ...

$ kubectl -n gitlab get bucket gitlab-artifacts -o jsonpath='{.status.grantedReadTo}'
```

Removing the entry changes `spec`, which bumps the generation and wakes the
grantor immediately. Deleting or provisioning a grantee wakes its grantors through
a dedicated watch, which fires on exactly four conditions: the grantee appearing,
disappearing, starting to delete, or changing its published credentials-group
identity
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D11).
Anything else waits for the periodic drift resync, whose period is the Helm
value

```yaml
driftResyncInterval: "10m"   # default
```

Not verified, and this is the gap: no measurement of that worst-case revocation
latency exists.

---

## A bucket still being cloned shares nothing

While [`spec.cloneFrom`](cloning.md) is still copying, granted readers stay out of
the policy entirely, and are written in the very pass the copy succeeds — not on
some later reconcile
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D10).

`holdSecretUntilCloned` cannot help here: it holds back the bucket's *own*
workload Secret, and a granted reader already holds working credentials of its
own. The policy is the only thing that can hold a reader back.

What you see while the clone runs: the grantor is not `Ready`,
`status.clone.progress` advances, and `status.grantedReadTo` is empty even though
the grantee resolves fine. Both fill in together when the copy completes.

---

## Checking the effective grant list

```bash
# What is in effect right now, per bucket:
kubectl -n gitlab get bucket gitlab-artifacts -o jsonpath='{.status.grantedReadTo}'

# What was requested, for comparison — a requested entry missing from the list
# above is pending or revoked:
kubectl -n gitlab get bucket gitlab-artifacts -o jsonpath='{.spec.grantReadAccess[*].name}'

# Why an entry is missing:
kubectl -n gitlab get events --field-selector reason=ReadGrantPending \
  --sort-by=.lastTimestamp
```

`status.grantedReadTo` lists the entries that **resolved and were written into the
policy**, so a pending or revoked grant is visible without reading the policy out
of S3 — which no credential except the operator's admin key can do anyway. It is
not a print column; `kubectl get bucket` will not show it.

---

## Verification: prove the grant end to end

Run this from a pod in the namespace, or locally with the values pulled out of the
grantee's Secret. `aws` CLI shown; `s3cmd` works the same way.

```bash
NS=gitlab                      # example
# Credentials of the GRANTEE (the reader), straight out of the Secret the
# operator wrote. Key names are the defaults; they are overridable per Bucket
# via spec.secretRef.keys (see README.md).
export AWS_ACCESS_KEY_ID=$(kubectl -n $NS get secret gitlab-backups-s3 \
  -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d)
export AWS_SECRET_ACCESS_KEY=$(kubectl -n $NS get secret gitlab-backups-s3 \
  -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d)
export AWS_DEFAULT_REGION=$(kubectl -n $NS get secret gitlab-backups-s3 \
  -o jsonpath='{.data.S3_REGION}' | base64 -d)
EP=$(kubectl -n $NS get secret gitlab-backups-s3 \
  -o jsonpath='{.data.S3_ENDPOINT}' | base64 -d)

# The PHYSICAL name of the granting bucket — not necessarily spec.bucketName,
# because the operator may compose a prefix (see bucket-naming.md).
# gitlab-artifacts is an example: use the metadata.name of the granting Bucket CR.
GRANTOR=$(kubectl -n $NS get bucket gitlab-artifacts \
  -o jsonpath='{.status.resolvedBucketName}')

# The operator addresses this endpoint PATH-STYLE; the AWS CLI defaults to
# virtual-hosted, so set path addressing explicitly. (Not verified: whether the
# endpoint also serves virtual-hosted requests.)
aws configure set default.s3.addressing_style path

# A function, not an alias: aliases are not expanded in a non-interactive shell,
# so the five steps below also work when this block is pasted into a script.
s3api() { aws --endpoint-url "https://$EP" s3api "$@"; }

# 1. list must SUCCEED
s3api list-objects-v2 --bucket "$GRANTOR" --max-items 1

# 2. get must SUCCEED (some/existing.key is an example — name a key that is there)
s3api get-object --bucket "$GRANTOR" --key some/existing.key /tmp/out.bin

# 3. put must FAIL with AccessDenied
s3api put-object --bucket "$GRANTOR" --key reader-wrote.txt --body /etc/hostname

# 4. delete must FAIL with AccessDenied
s3api delete-object --bucket "$GRANTOR" --key some/existing.key

# 5. reading the policy must FAIL with AccessDenied (the reader cannot inspect its cage)
s3api get-bucket-policy --bucket "$GRANTOR"
```

Steps 1 and 2 succeed, 3 through 5 fail with `AccessDenied`. If step 1 fails while
`status.grantedReadTo` already lists the grantee, the policy write has not
propagated yet — StorageGRID takes a moment. The suites that pin this behaviour
allow it 90 seconds (the provider-level test) and 2 minutes (the end-to-end test)
before treating it as a failure, so retry for a minute or two before concluding
the grant is broken.

**Cross-namespace must stay denied.** Repeat step 1 with the credentials of a
same-named `Bucket` in another namespace: it must fail. A `Bucket` called
`gitlab-backups` in namespace `other` is a different Bucket, is not resolved and is
not woken ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D3).

<details>
<summary>Which suites prove this, and what they assert</summary>

Verified by reading the suites, not by running them for this page; the ADR records
that no run date survives for the two cloud suites.

| Suite | How to run | What it pins |
| --- | --- | --- |
| `stackit/grants_integration_test.go` | `go test -tags integration ./stackit/ -run IntegrationReadGrant -v -timeout 12m` | The three-statement document against real StorageGRID evaluation: owner keeps write access, reader lists and gets, reader denied write/overwrite/delete/policy-read/policy-write, an ungranted workload locked out entirely, the admin still able to rewrite the policy, and revocation locking the reader out again |
| `test/e2e/cloud_test.go` (`TestCloudReadGrant`) | `make e2e-stackit` | The same through the operator on a Kind cluster against the real API, plus the cross-namespace namesake being denied |
| `test/e2e/cloud_test.go` (`TestCloudReadGrantLifecycle`) | `make e2e-stackit` | A grantee that appears *after* the grantor was provisioned (the grantee watch), and revocation by removing the entry from the spec |
| `test/integration/grant_read_access_test.go` | `make test-integration-coverage` | The CRD schema against a real API server: self-reference rejected, a duplicate entry rejected by the list-map key |
| `internal/controller/reconciler_grants_test.go` | `make test-unit-coverage` | Offline: grant applied, pending, late grantee, revocation, namespace scoping, self-reference ignored, admin never granted, several grantees, stable document, duplicate display names irrelevant, readers held back during a clone |
| `stackit/s3_test.go` | `make test-unit-coverage` | The document itself: reader actions are a read-only subset, admin and workload filtered out of the reader list, no-grant document unchanged, reader order deterministic |

</details>

---

## The upgrade guarantee

Without a grant, the policy document is **byte-identical** to that of a bucket that
never used the feature: the third statement and the `NotPrincipal` exemption exist
only when at least one reader resolves
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D8).
Upgrading the operator therefore rewrites no policy of a bucket without grants, and
the drift check reports no change for them.

The reader list is also deduplicated and sorted before the document is built, so
adding a grant twice, or in a different order, produces the same bytes and no
spurious rewrite.

---

## Failure modes

| Symptom | What you see | What to do |
| --- | --- | --- |
| Reader gets `AccessDenied` on a bucket it should read | Grantor `Ready`; entry absent from `status.grantedReadTo`; `Warning ReadGrantPending` on the grantor | Read the event reason in the message. Missing grantee → create it. Not owned → the grantee's physical bucket carries someone else's ownership tags; see [../security/ownership-and-attribution.md](../security/ownership-and-attribution.md) |
| Grant never resolves although the grantee is `Ready` | `ReadGrantPending` with `names bucket "n", which is not owned by that Bucket` | The grantee's physical bucket was not provisioned by this operator, or its ownership tags were changed out of band. Nothing to fix on the grantor |
| Grantee is created but grantor does not pick it up | `status.grantedReadTo` stays empty for minutes | The grantee wakes grantors only on create, delete, deletion start, or a change of `status.credentialsGroupURN`. Anything else waits for `driftResyncInterval` (`10m`, the default). Touching the grantor's spec or annotations wakes it immediately |
| Grantor stuck not `Ready`, grant looks fine | `status.clone.progress` advancing | A clone is still filling the bucket; readers are held out until it completes ([cloning.md](cloning.md)) |
| Grantor goes `Failed` on the very pass that added a grant | `Ready=False`, phase `Failed`, `status.message` carrying the provider error; **no** `degradedSince`, **no** `ProviderReachable` condition | A spec edit has not been observed yet, so nothing may be held ([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D6). The next successful pass clears it. If it repeats, treat `status.message` as the real fault |
| Grantor degraded on a later pass — one woken by the grantee watch or the drift resync | `Ready` still true, `ProviderReachable=False`, `status.degradedSince` set; reconcile errors rising | Either a provider problem, or the momentary attribution-lag case above. Self-clearing; if it outlasts `providerDegradedGrace` the bucket falls to `Failed` — see [provider-outages.md](provider-outages.md) |
| `kubectl apply` rejected: `spec.grantReadAccess must not reference the Bucket itself` | Rejected at admission by the CRD schema | Remove the self-reference. Should one ever reach the reconcile it is ignored with `ReadGrantPending`, and the policy builder filters the resulting principal anyway |
| `kubectl apply` rejected: duplicate entry | API-server list-map-key violation on `name` | Entries are unique by name; remove the duplicate |
| `kubectl apply` rejected: too many entries | API-server `maxItems` violation | The list is capped at 32. Not verified, and this is the gap: the cap is a chosen bound, not a measured ceiling — no run has established what a grantor with 32 entries costs per pass |
| Reader's tooling fails on multipart cleanup | `AccessDenied` on `ListMultipartUploads` / `AbortMultipartUpload` | Expected and deliberate ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D6). Configure the client not to sweep multipart uploads on a bucket it only reads |

### What a grant costs on every pass

Each entry costs the grantor a bucket-existence check plus the grantee's
attribution reads against the provider, **on every reconcile, whether or not
anything changed** — there is no cache and no batching
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md),
Consequences). With many grantors in one namespace, the same grantee bucket is read
once per grantor per pass. Two operational knobs bound the resulting traffic, both
documented in [configuration.md](configuration.md): `driftResyncInterval` sets how
often a quiet bucket reconciles at all, and the fleet-wide circuit breaker
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md)) is what absorbs
this traffic during a provider outage.

### The security tradeoff, stated plainly

A reader's single credential now opens its own bucket **plus every bucket that
granted to it**, so leaking it costs more than it did. That is the price of not
minting a second credential per grant, and it is the reason the granted action set
is a strict read-only subset rather than "whatever the reader needs". Rotating that
credential ([credentials.md](credentials.md)) invalidates it everywhere at once,
grants included.

---

## Related

| Page | What it covers |
| --- | --- |
| [../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) | The decision, its rules D1–D11, the rejected alternatives and the residual risks |
| [../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) | The attribution chain the reader principal is resolved through |
| [../security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md) | What a grant opens and what stays closed |
| [../developer/bucket-policy.md](../developer/bucket-policy.md) | How the three-statement document is built and compared |
| [cloning.md](cloning.md) | The clone that holds readers out while a bucket fills |
| [bucket-status.md](bucket-status.md) | Reading phase, conditions and the rest of `status` |
| [bucket-naming.md](bucket-naming.md) | Why `status.resolvedBucketName` can differ from `spec.bucketName` |
| [../../README.md](../../README.md) | The complete `Bucket` and Helm reference |
