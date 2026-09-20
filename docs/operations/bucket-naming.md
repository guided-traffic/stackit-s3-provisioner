# Bucket naming and ownership

The name you write in `spec.bucketName` is not necessarily the name the bucket has in STACKIT
Object Storage. The operator composes the physical name from an install-time policy, freezes it the
first time the Bucket is provisioned, and publishes the frozen name — never the requested one — to
the workload. A second install-time value, the ownership identity, decides which cloud buckets this
installation is allowed to touch at all.

Both are operator-wide settings. Neither can be influenced from a `Bucket` CR
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D2,
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D4), and both belong to the
install, the backup procedure and the disaster-recovery runbook rather than to a namespace user.

This page explains those three settings — `bucketNaming.prefix`, `bucketNaming.includeNamespace`
and `ownership.name`. The complete list of Helm values and CRD fields lives in the
[README reference](../../README.md) and only there.

---

## The minimal configuration

```yaml
# values.yaml — the two naming settings and the ownership identity
bucketNaming:
  # Prepended to every bucket name this installation provisions. Empty disables
  # it, which is the default and makes the physical name equal spec.bucketName.
  # Must be a lowercase DNS-1123 label: letters, digits and '-', no dots, no
  # leading or trailing '-'. An invalid value stops the operator at startup.
  prefix: my-cluster        # example
  # Appends the Bucket's namespace after the prefix. Spends the 63-character
  # name budget a second time — see "The name budget" below.
  includeNamespace: true    # example (default: false)

ownership:
  # Written into every provisioned bucket's managed-by tag and required to match
  # before this installation adopts or deletes a pre-existing bucket. Change it
  # after buckets exist and the operator reads its own buckets as foreign.
  # Two installations sharing one STACKIT project MUST use distinct values.
  name: stackit-s3-provisioner   # default
```

Defaults are `prefix: ""`, `includeNamespace: false`, `ownership.name: stackit-s3-provisioner` —
verified in [`deploy/helm/stackit-s3-provisioner/values.yaml`](../../deploy/helm/stackit-s3-provisioner/values.yaml).
With the naming defaults the composition is the identity function and the physical name equals
`spec.bucketName`, which is what installations that own their STACKIT project outright still run.

Each value also exists as a process flag and an environment variable, for a deployment that is not
rendered by this chart:

| Helm value | Flag | Environment variable | Default |
| --- | --- | --- | --- |
| `bucketNaming.prefix` | `--bucket-name-prefix` | `BUCKET_NAME_PREFIX` | `""` &nbsp;# default |
| `bucketNaming.includeNamespace` | `--bucket-name-include-namespace` | `BUCKET_NAME_INCLUDE_NAMESPACE` | `false` &nbsp;# default |
| `ownership.name` | `--ownership-name` | `OWNERSHIP_NAME` | `stackit-s3-provisioner` &nbsp;# default |

The chart renders `--bucket-name-prefix` and `--ownership-name` only when the value is non-empty and
`--bucket-name-include-namespace` only when it is true, so an empty Helm value leaves the flag off
and the process default applies. The process default for the ownership identity is the same string
the chart ships, so an emptied `ownership.name` is not a change of identity.

---

## What the composed name looks like

The name is `<prefix>-<namespace>-<spec.bucketName>`, with every disabled part dropped and the
remainder joined by `-` ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D1).
For a `Bucket` named `my-bucket` in namespace `monitoring`:

| `prefix` | `includeNamespace` | Physical bucket name |
| --- | --- | --- |
| `my-cluster` | `true` | `my-cluster-monitoring-my-bucket` |
| `my-cluster` | `false` | `my-cluster-my-bucket` |
| `""` | `true` | `monitoring-my-bucket` |
| `""` &nbsp;# default | `false` &nbsp;# default | `my-bucket` |

Composition makes names **attributable**, not unique. With the defaults, or with a prefix but no
namespace part, two Buckets of the same name in two namespaces still compose to one physical name;
what stops the second one from managing the first one's bucket is the ownership check described
below, not the name.

### Where the frozen name surfaces

| Surface | What it carries |
| --- | --- |
| Credentials Secret, `S3_BUCKET` and `S3_BUCKET_URL` keys (default names) | The frozen name and the path-style URL built from it — this is what the workload connects to |
| `status.resolvedBucketName` | The frozen name, authoritative |
| `kubectl get bkt -o wide`, column `RESOLVED` | The frozen name — **wide output only** |
| Ownership and attribution tags on the bucket | All four are stamped on the bucket that carries the frozen name: the ownership pair ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D2) and the attribution pair ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1) |
| Isolation policy, read grants, size measurement, teardown | Every one of them addresses the frozen name ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D8) |
| Operator log, lines `bucket provisioned` and `bucket verified` | Both names: `bucket=<frozen>` and `requested=<spec.bucketName>` |

Two places deliberately do **not** carry it. The `BUCKET` column of `kubectl get bkt` shows
`spec.bucketName`, the name that was requested. And the Prometheus metrics are labelled with the
CR's `namespace` and `name` only — verified in
[`internal/controller/metrics.go`](../../internal/controller/metrics.go) — so correlating an alert,
an invoice line or a STACKIT console entry with a `Bucket` means reading
`status.resolvedBucketName` first. See [monitoring.md](monitoring.md) for the metrics themselves.

Nothing in the data path notices the difference, because a workload takes the bucket name from its
Secret rather than from the CR. A workload that hardcodes `spec.bucketName` instead of reading
`S3_BUCKET` breaks the moment a prefix is introduced for new buckets.

### The name budget

Prefix, namespace and the name the user chose share one 63-character ceiling. With
`includeNamespace: true` and a prefix of length *p*, the usable budget is
`63 − p − 2 − len(namespace)` characters for `spec.bucketName`.

The lower bound of the range cannot be reached by composition: `spec.bucketName` is already
CRD-validated to at least 3 characters and composition only ever adds. Verified by reading the
validators in [`api/v1/bucket_types.go`](../../api/v1/bucket_types.go): so is the DNS half of the
check — the prefix is validated as a DNS-1123 label, a Kubernetes namespace is one, and
`spec.bucketName` is CRD-validated to start and end alphanumeric, so a name this operator composes
is always DNS-compliant. **The 63-character ceiling is the constraint that actually bites.**

---

## The freeze

The physical name is decided once, on the first provisioning pass, and then never recomputed. The
operator resolves it in this fixed order, without any call to the provider:

| Order | Source | Meaning |
| --- | --- | --- |
| 1 | `status.resolvedBucketName` | Already frozen; authoritative |
| 2 | Annotation `stackit-bucket.gtrfc.com/resolved-bucket-name` | The durable backup, used when status was lost |
| 3 | `spec.bucketName`, when `status.bucketURL` is set but no frozen name is | A bucket provisioned before this feature existed; it keeps its raw name |
| 4 | A fresh composition from the current policy | A Bucket being provisioned for the first time |

Only case 4 is validated against the 3–63 and DNS constraints; a name already frozen is used exactly
as recorded ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3, D5).

The annotation is written with a full object update **before any cloud resource is created**, and
the status field only when the pass succeeds. A crash between the two therefore cannot lose the
name. The annotation is one of exactly two pieces of a Bucket's metadata the operator ever writes
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D3) — the other is the
finalizer — so a Bucket applied from Git gains one annotation that is not in Git, once. That is by
design and does not cause a sync loop; see [gitops.md](gitops.md).

### Changing the policy later

Changing `prefix` or `includeNamespace` affects **only buckets provisioned after the change**
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D4). Every existing
Bucket keeps the name it froze, stays reachable, and its workload's Secret is not rewritten. The
provider has no rename, so this is the only safe behaviour: recomposing on every reconcile would
leave the original buckets in place with their data and unreferenced, create empty replacements
under the new names, and write the replacements' credentials into the Secrets the workloads are
already using.

There is no supported way to rename an existing bucket. Moving data to a differently-named bucket
means a new `Bucket` CR plus a clone from the old one — see [cloning.md](cloning.md).

### Buckets provisioned before v1.3.0

The naming policy shipped on 2026-07-01 in v1.3.0. A bucket provisioned before that carries no
frozen name and no annotation; case 3 above recognises it by `status.bucketURL` being set and keeps
its raw `spec.bucketName` permanently. On the first reconcile after the upgrade the operator writes
that raw name into the annotation, so from that point the bucket is protected by the same freeze as
every other — verified by reading the reconcile path: the annotation write is unconditional and does
not depend on the name having been freshly composed. Introducing a prefix on an existing
installation re-maps nothing.

---

## Failure mode: an invalid prefix stops the operator at startup

The prefix must be a lowercase DNS-1123 label
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D7). Uppercase, a
dot, a leading or trailing `-`, or more than 63 characters is rejected — validated before the
manager starts, so a typo can never reach the provider.

**What you see.** The process logs and exits with status 1, before any controller is running:

```
ERROR   setup   invalid bucket naming configuration   {"error": "bucket name prefix \"My_Cluster\" is
invalid: must be a lowercase DNS-1123 label (letters, digits and '-'; no leading/trailing '-'; max
63 chars)"}
```

Under a Deployment this presents as a Pod that never becomes ready and restarts — that follows from
the restart policy and was not observed in a cluster here, which is the gap. It is not a degraded
mode: no Bucket is reconciled, and nothing in the cloud changes.

**What to do.** Correct `bucketNaming.prefix`, `helm upgrade`, and confirm the operator came up (see
[Verifying](#verifying) below).

---

## Failure mode: a composed name out of range parks the Bucket

A composed name outside 3–63 characters or not DNS-compliant is a configuration fault, never a
truncation and never a hash ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D5).
The operator refuses to hand back a different name for the one that was asked for.

The CRD cannot catch this: it validates `spec.bucketName` but cannot see the operator's prefix or
the namespace part. The rejection therefore arrives at reconcile time as a failed Bucket, not at
`kubectl apply` as a validation error.

**What you see.** The Bucket parks, with no requeue
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D5):

| Signal | Value |
| --- | --- |
| `status.phase` | `Failed` |
| `Ready` condition | `False`, reason `Failed` |
| `status.message` | `composed bucket name is invalid: bucket name "…" must be 3-63 characters long (got 71)` |
| Event | `Warning` / `Failed`, carrying the same text |
| Cloud state | Nothing was created — the name is validated before the first provider call |

**What to do.** Nothing the applier owns repairs it: `spec.bucketName` is immutable (a CEL rule on
the field rejects a change) and a namespace cannot be renamed, so both inputs the applier controls
are fixed for the life of the object. The correction belongs to the operator's naming policy —
shorten the prefix, or turn `includeNamespace` off for this installation, and remember that doing so
changes the name only for buckets not yet provisioned.

A Bucket parked this way carries **no timer of its own**: the drift-resync requeue is attached only
to a successful pass, and the no-requeue failure path returns without one. It resumes when the
generation changes, when an annotation on the CR changes, or when the operator restarts. After a
`helm upgrade` that corrects the policy, the operator restarts anyway and the Bucket is retried.

---

## The ownership identity, and the trap of changing it

Every bucket the operator provisions is stamped with two **ownership** tags. STACKIT has no native
bucket tags, so this rides on S3 bucket tagging through the admin credential
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)).

| Tag | Value | Configurable |
| --- | --- | --- |
| `managed-by` | The installation identity, `ownership.name` | Yes — install-time |
| `owner` | `<namespace>/<name>` of the Bucket CR | No — derived, and deliberately not the CR UID |

These two are not the whole tag set. A fully provisioned bucket carries **four** tags in one set:
the two above plus `credentials-group-id` and `credentials-group-urn`, which bind the bucket to its
workload credentials group ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1)
and are written into the same tag map, next to the ownership tags. Only the two ownership tags are
the subject of this page; the attribution pair is described in
[ownership-and-attribution.md](../security/ownership-and-attribution.md).

The operator adopts or deletes a pre-existing bucket only when **both** ownership tags match
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D2). Anything else is a
collision. One exception exists and is narrow: a pre-existing bucket with **no tags at all** is
claimed only when it is empty, because that is exactly the state a crash between bucket creation and
the tag write leaves behind. A non-empty untagged bucket is refused.

**Run two installations against one STACKIT project only with distinct `ownership.name` values.**
With the same identity, the same namespace and the same Bucket name, each installation reads the
other's bucket as its own, and the two then manage one bucket and one credentials group, each
replacing the other's live key. Not verified in a live two-cluster setup — this is read off the
ownership comparison, not measured — but the cost if it is right is a workload losing its
credentials without any error being raised. A distinct prefix per installation is the other half of
the same mitigation, and nothing in the operator enforces that either was applied.

### Changing `ownership.name` after buckets exist

The operator then treats its own buckets as foreign. What happens is different for provisioning and
for deletion, and the deletion half is the dangerous one.

**On the next reconcile of an existing Bucket**, the bucket exists, its `managed-by` tag no longer
matches, and the Bucket parks:

| Signal | Value |
| --- | --- |
| `status.phase` | `Failed` |
| `status.message` | `bucket "my-cluster-monitoring-my-bucket" already exists and is not owned by this operator (owned by managed-by="old-identity" owner="monitoring/my-bucket"); refusing to adopt` |
| Events | `Warning` / `Failed`, twice: the collision summary and the message above |
| Cloud state | Untouched. The existing bucket, its credentials group and the live key are left exactly as they are |

The workload keeps working throughout — its Secret still holds a valid key, and the operator has
changed nothing in the cloud.

**On deletion of such a Bucket, the CR is released and the cloud resources are not.** Verified by
reading the teardown path: the emptiness guard runs first, so a non-empty bucket still blocks the
deletion and keeps the finalizer ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D2).
But for an **empty** bucket the teardown skips the credentials-group release (Warning event
`CredentialsGroupNotAttributable`) and skips the bucket delete (Warning event `Failed`, "not
deleting bucket: it is not owned by this operator (no matching ownership tags)"), then deletes the
workload Secret and drops the finalizer. The CR disappears; the bucket, its credentials group and
its access key stay behind in STACKIT with nothing referencing them. See
[deletion.md](deletion.md) for the teardown order in full.

**What to do.** Put the previous value back and `helm upgrade`. The parked Buckets recover on the
restart that follows. There is no operator-side re-tagging path: making the operator adopt buckets
under a new identity means rewriting the `managed-by` tag on each bucket yourself, with an S3 client
and the admin credential, before the operator sees them. That is a manual data-plane operation this
project ships no tooling for.

If you do it anyway, **read the existing tag set first and write it back whole**. S3
`PutBucketTagging` replaces the entire tag set rather than merging into it, so writing only
`managed-by` and `owner` drops `credentials-group-id` and `credentials-group-urn` from the same
bucket. That is recoverable but not free: the operator then re-attributes the group from the
bucket's own isolation policy and re-tags it
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D2), and until that
pass runs the bucket has no tag-path attribution at all.

The security reasoning behind the tags, and what they do not defend against, is in
[ownership-and-attribution.md](../security/ownership-and-attribution.md).

---

## Disaster recovery: the values a restore must reproduce

Restoring the same manifests into a fresh cluster re-adopts the existing buckets instead of
duplicating them — but only if the installation is configured identically. None of the inputs is
cluster-assigned state: the CR UID in particular is deliberately not part of any of them
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D6), which is what
makes a replay from Git work at all.

| Must match the original installation | Why |
| --- | --- |
| `ownership.name` | Part of the ownership key. A different value makes every bucket foreign — and unlike the prefix, there is no escape from this one, frozen name or not |
| `bucketNaming.prefix` | Composes the name, **but only where the restore replays the manifest alone** — see below |
| `bucketNaming.includeNamespace` | Same |
| The STACKIT project — i.e. the service-account key | One deployment, one project ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D1) |
| `stackit.region` | One deployment, one region, fixed for the process lifetime ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D2, D8) |
| The Bucket CRs' namespaces and names | `owner` tag is `<namespace>/<name>`; the namespace is also a name input when `includeNamespace` is on |

The naming policy carries one qualification the ownership identity does not. If the restore brings
back either `status.resolvedBucketName` or the resolved-name annotation, the frozen name wins and
the prefix never enters the calculation — a mismatched prefix is then harmless. It only matters
where the restore replays the manifest alone, with no status and no annotation: that Bucket composes
a fresh name, and a wrong prefix creates a parallel set of empty buckets while the originals are
orphaned with the data still in them.

Neither failure is loud at install time. Both surface only when the first Bucket reconciles: a wrong
ownership identity as a parked Bucket, a wrong prefix as a suspiciously fast, suspiciously empty
successful provisioning. **Check the first reconcile after a restore before declaring it done.**

Not verified, and this is the gap: there is no automated test for the restore-into-a-fresh-cluster
replay. Composition, the freeze and the resolution order are covered by the offline suite, and
composed names are exercised against the real STACKIT API by the end-to-end suite — which installs
the operator with both a prefix and namespace inclusion enabled — but that a restore re-adopts
rather than duplicates is verified only by reading which inputs the composition uses.

---

## Verifying

After changing either setting, in order:

1. **Confirm the operator started.** An invalid prefix exits before the manager runs, so the first
   check is that the process is up at all:

   ```bash
   kubectl -n <operator-namespace> rollout status deploy/<release>-stackit-s3-provisioner
   ```

2. **Confirm the policy it actually loaded.** The startup line logs both naming settings and the
   ownership identity, so there is no need to infer them from the rendered args:

   ```bash
   kubectl -n <operator-namespace> logs deploy/<release>-stackit-s3-provisioner \
     | grep 'starting stackit-s3-provisioner'
   # look for: "bucketNamePrefix":"my-cluster" "bucketNameIncludeNamespace":true
   #           "ownershipName":"stackit-s3-provisioner"
   ```

3. **Provision one Bucket and read the frozen name.** The `RESOLVED` column has printer priority 1,
   so it appears in wide output only — `kubectl get bkt` without `-o wide` will not show it:

   ```bash
   kubectl -n monitoring get bkt my-bucket -o wide
   # NAME        BUCKET      PHASE   READY   ...   RESOLVED                          ...
   # my-bucket   my-bucket   Ready   True    ...   my-cluster-monitoring-my-bucket   ...
   ```

   Equivalently, without the column width problem:

   ```bash
   kubectl -n monitoring get bkt my-bucket -o jsonpath='{.status.resolvedBucketName}{"\n"}'
   ```

4. **Confirm the workload is told the frozen name**, not the requested one:

   ```bash
   # my-bucket-s3 is the spec.secretRef.name of the Bucket  # example
   kubectl -n monitoring get secret my-bucket-s3 -o jsonpath='{.data.S3_BUCKET}' | base64 -d
   # my-cluster-monitoring-my-bucket
   ```

5. **Confirm the freeze is durable.** The annotation must be present before you rely on the status
   surviving a backup:

   ```bash
   kubectl -n monitoring get bkt my-bucket \
     -o jsonpath='{.metadata.annotations.stackit-bucket\.gtrfc\.com/resolved-bucket-name}{"\n"}'
   ```

For existing Buckets after a policy change, step 3 on each of them is the check that nothing was
re-mapped: every pre-existing Bucket must still report the name it had before.

---

## Related

| Page | What it adds |
| --- | --- |
| [ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) | The decision, its alternatives and its residual risks |
| [ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) | The namespace as trust boundary; D2 is the ownership attribution the frozen name is the address for |
| [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) | D3 names the freeze annotation; D5 is the parking behaviour both failure modes use |
| [configuration.md](configuration.md) | The other settings whose consequences are not obvious |
| [deployment.md](deployment.md) | Install and upgrade beyond the README's shortest path |
| [bucket-status.md](bucket-status.md) | How to read a parked Bucket, and the other faults that park one |
| [deletion.md](deletion.md) | The teardown order the ownership guard sits in |
| [gitops.md](gitops.md) | Why the one annotation the operator writes does not cause a sync loop |
| [ownership-and-attribution.md](../security/ownership-and-attribution.md) | What the ownership tags defend against, and where they do not reach |
| [bucket-identity.md](../developer/bucket-identity.md) | How composition, the freeze and the tags are implemented |
| [README](../../README.md) | The complete Helm value and CRD field reference |
