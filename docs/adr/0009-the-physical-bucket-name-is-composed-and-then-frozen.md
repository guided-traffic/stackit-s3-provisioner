# ADR 0009: The Physical Bucket Name Is Composed and Then Frozen

## Status

Accepted. Date: 2026-07-01. Record written on 2026-09-19 from the shipped behaviour, so the
alternatives below are reconstructed from the code and the material that preceded it, not from a
discussion minuted at the time.

Implemented and in use: the physical name is composed from the operator-wide naming policy
(`bucket-name-prefix` / `bucket-name-include-namespace`, Helm `bucketNaming.prefix` and
`bucketNaming.includeNamespace`) plus the Bucket's namespace and `spec.bucketName`, frozen in
`status.resolvedBucketName` and in the annotation `stackit-bucket.gtrfc.com/resolved-bucket-name`,
and published to workloads through the credentials Secret. Verified on 2026-09-19 by reading the
composition, the freeze and the validation paths, and covered by the offline suite; the end-to-end
suite that runs against the real STACKIT API installs the operator with a prefix **and** namespace
inclusion enabled, so composed names are exercised against the provider. Still open: the provider
question this scheme was built for — whether a bucket name has to be unique per project or across a
whole region — has never been answered, so the prefix remains a precaution rather than a proven
necessity.

## Context

A Kubernetes name is unique in a namespace. A bucket name is not: it must be unique in whatever scope
the provider uses, and it is chosen by whoever writes the CR. `spec.bucketName` is validated by the
CRD (3–63 characters, DNS-compliant, lowercase) and is immutable after creation, but nothing in the
cluster makes it unique outside that cluster.

That matters because one operator deployment serves exactly one STACKIT project
([ADR 0005](0005-the-operator-serves-one-project-in-one-region.md)) while a project is routinely
shared — several clusters of one fleet, or several teams. Two clusters that each hold a Bucket named
`registry` in a namespace named `harbor` resolve to the same cloud bucket. The ownership tags that
normally catch exactly this ([ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) D2) cannot:
the owner marker is `<namespace>/<name>` and the `managed-by` marker is an installation-wide identity
that defaults to the same string in both installations, so the second cluster does not see a foreign
bucket — it sees its own, adopts it, and both clusters then manage one bucket and one credentials
group, each replacing the other's live key. Not verified in a live two-cluster setup: this is read
off the ownership comparison, not measured. Either way, the identifying part has to be introduced
above the CR, because the CR cannot know which cluster it will be applied to.

The feasibility work of 2026-06-30 had already recorded this as an open question to STACKIT (whether
bucket names are unique per project or shared across a region) and noted that a region-global
namespace would make a collision both a create failure and an information leak — you learn that a
name is taken in a project you cannot see. The question was never answered. The naming policy shipped
on 2026-07-01 as the answer that does not depend on one.

The second force is that the name must never move afterwards. Everything that addresses the bucket
later is keyed by that string: the bucket name and URL that workloads already hold in their
credentials Secret, the ownership tags, the resource named in the isolation policy, and the emptiness
check and delete on teardown. If the name were recomposed on every reconcile, an administrator
changing the prefix would not rename anything — the provider has no rename — but would silently point
every Bucket at a name that does not exist yet: the operator would create fresh empty buckets, mint
fresh credentials into the Secrets, and leave the original buckets behind with the data in them and
nothing referencing them. The same applies to the upgrade that introduced the feature: every bucket
provisioned before it exists under the raw `spec.bucketName`.

The third force is that status is not durable. A CR restored from backup, or a status subresource
lost, must not produce that same re-map. So the frozen name needs a home that survives a status
wipe and is written before the first cloud resource exists.

## Decision

**D1 — The physical bucket name is composed, not taken verbatim.** It is
`<prefix>-<namespace>-<spec.bucketName>`, with any disabled part dropped and the remainder joined by
`-`. With both parts disabled the composition is the identity function, which is what makes the
default an exact continuation of the behaviour that predates this record.

**D2 — The naming policy is operator-wide and set at install time, never per CR.** It is exactly two
settings, and no Bucket field influences the name beyond its own namespace and `spec.bucketName`:

| Setting | Flag / environment variable | Helm value | Value |
| --- | --- | --- | --- |
| Prefix | `--bucket-name-prefix` / `BUCKET_NAME_PREFIX` | `bucketNaming.prefix` | `""` &nbsp;# default (disabled) |
| Namespace part | `--bucket-name-include-namespace` / `BUCKET_NAME_INCLUDE_NAMESPACE` | `bucketNaming.includeNamespace` | `false` &nbsp;# default |
| Example composition | — | — | `my-cluster-monitoring-my-bucket` &nbsp;# example |

A namespace user therefore cannot aim a Bucket at another installation's or another namespace's
bucket by choosing a prefix, which is the same rule as ADR 0001 D4 applied to the name itself.

**D3 — The composed name is frozen the first time the Bucket is provisioned.** It is written to the
annotation `stackit-bucket.gtrfc.com/resolved-bucket-name` **before** any cloud resource is created,
and to `status.resolvedBucketName` when the pass succeeds. Resolution order is fixed:
`status.resolvedBucketName`, then the annotation, then — and only then — a fresh composition. A crash
between the two writes therefore cannot lose the name.

**D4 — A later change to the naming policy affects only buckets provisioned after it.** An existing
Bucket keeps the name it froze. A bucket provisioned before this record — recognisable because status
carries a bucket URL but no frozen name — keeps its raw `spec.bucketName` permanently; the upgrade
that introduces a prefix re-maps nothing.

**D5 — A composed name that is not a valid bucket name is a configuration fault, never a
truncation.** The composed name must be 3–63 characters and DNS-compliant. A freshly composed name is
validated before it is frozen; when the prefix or the namespace push it out of range the Bucket parks
as `Ready=Failed` with the reason and is not requeued. Nothing the applier can change repairs it:
`spec.bucketName` is immutable and a namespace cannot be renamed, so both inputs the applier owns are
fixed for the life of the object and the correction belongs to the operator's naming policy. A Bucket
parked this way carries no timer of its own — after the policy is corrected it resumes when the
operator restarts, or when an annotation on the CR changes. A name that is already frozen is used
exactly as recorded and is not re-validated.

**D6 — The composition inputs are install-time configuration and the CR's own namespace and name —
never cluster-assigned state.** The CR UID in particular is not an input. Re-applying the same
manifests into a fresh cluster, from an installation configured with the same naming policy and the
same ownership identity, therefore reaches the same buckets and re-adopts them instead of duplicating
them.

**D7 — An invalid prefix stops the operator at startup.** The prefix must be a lowercase DNS-1123
label (letters, digits and `-`, no leading or trailing `-`, no dots). The process refuses to start
rather than provisioning under a half-valid policy, so a typo can never reach the provider.

**D8 — The frozen name, not `spec.bucketName`, is what the product publishes.** It is the bucket name
and the bucket URL handed to workloads in the credentials Secret (`S3_BUCKET` and `S3_BUCKET_URL` by
default), the name reported in `status.resolvedBucketName`, and the name every later operation —
policy, grants, measurement, teardown — addresses.

## Consequences

The name a user typed is not the name in the STACKIT console. Anyone correlating a Bucket with cloud
resources, an invoice line or a support ticket has to read `status.resolvedBucketName` first. Nothing
in the data path notices, because workloads take the name from the Secret rather than from the CR.

The naming policy joins the ownership identity as part of the restore procedure, with one
qualification. A different prefix only matters where the restore replays the manifest alone — no
status and no frozen annotation: that Bucket composes a fresh name, so the installation creates a
parallel set of empty buckets and leaves the originals orphaned. Where either the status or the
frozen annotation survives the restore, D3 resolves the frozen name first and the prefix never enters
the calculation. A different ownership identity has no such escape: the installation reads its own
buckets as foreign and parks every Bucket on a collision, frozen name or not. Neither failure is loud
at install time — both surface only when the first Bucket reconciles.

The freeze needs one operator-written annotation, and it is one of the exactly two pieces of a
Bucket's metadata the operator writes at all
([ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md) D3) — the other is the finalizer. A
Bucket applied from Git gains an annotation that is not in Git, once, the first time it is
provisioned.

Switching the namespace part on spends the name budget twice: prefix, namespace and the name the user
chose share 63 characters. A namespace with a long name can make an otherwise perfectly valid Bucket
unprovisionable, and because the CRD cannot see the operator's configuration, the rejection arrives at
reconcile time as a failed Bucket, not at `kubectl apply` as a validation error. That is the price of
D5: an operator-side policy cannot be enforced by admission.

Composition does not make names unique — it makes them attributable. With the defaults, or with a
prefix but no namespace part, two Buckets of the same name in different namespaces still resolve to
one bucket, and the ownership check is what stops the second one.

## Alternatives Considered

**Recompose the name on every reconcile.** Rejected, and it was by far the cheapest option: no
annotation, no status field, no freeze logic, no upgrade case, and the name is always consistent with
the current configuration. It lost on exactly that last property. Changing the prefix would leave
every existing bucket in place and unreferenced, create an empty replacement for each, and write
credentials for the replacement into the Secret the workload is already using — data loss by
configuration change, with no error anywhere. The upgrade that introduced prefixes would have done
this to a whole fleet at once.

**Truncate and hash a name that does not fit.** Rejected. The credentials group's display name is
built that way, and deliberately so, because it is a label rather than an identity
([ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) D7). A bucket name is the
opposite: workloads hold it, invoices show it, and it cannot be changed afterwards. Silently handing
back a different, unreadable name for the one that was asked for buys a successful reconcile at the
cost of a bucket nobody can recognise. A parked Bucket with a message is the louder and cheaper
failure.

**Derive the name from the CR UID.** Rejected. It is the only option that guarantees uniqueness
without configuration, and it destroys the property D6 exists for: the UID is reassigned when the CR
is recreated, so a disaster-recovery replay from Git would never find the buckets it is supposed to
re-adopt and would provision a second, empty set. It also produces names that mean nothing to a human
reading the console.

**No composition at all — the physical name is `spec.bucketName`.** Taken as the default, rejected as
the only option. It is what installations that own their project outright still run, and keeping it
as the default is what makes the feature a no-op on upgrade. It is not sufficient for a shared
project, where the identifying part cannot come from the CR because the CR does not know which
installation will apply it.

**Make the prefix a field in the Bucket spec.** Rejected. It moves the one input that distinguishes
installations into the hands of the namespace user, who could then compose a name belonging to
another cluster or another team and — where that bucket is untagged — adopt it. Nothing a namespace
user controls may be a source of attribution (ADR 0002 D6), and the name is the address attribution
hangs on.

**A mutating admission webhook that writes the resolved name into the spec.** Rejected. It would make
the rejection in D5 an admission error, which is genuinely better feedback, but it writes the spec of
the object the operator reconciles (ADR 0010 D1), it adds a webhook, a certificate and a failure mode
to a controller that has none, and a webhook that is unavailable would block Bucket creation
entirely.

## Residual risks

**The question the scheme answers is still unanswered.** Whether STACKIT's bucket namespace is scoped
per project or shared across a region was raised on 2026-06-30 and never confirmed by the provider.
Not verified: no measurement was taken, and a measurement would require a name collision across two
projects. The prefix is therefore precautionary — correct in either case, but its necessity is
assumed.

**Two installations sharing a project can still collide when the policy is left at its defaults.**
With no prefix, no namespace part and the default ownership identity, two clusters with the same
namespace and Bucket name each read the bucket as their own and manage it concurrently, replacing
each other's credentials. Not verified: derived from the ownership comparison, not observed. The
mitigation is configuration — a distinct prefix, or a distinct ownership identity, per installation —
and nothing in the operator enforces that it was applied.

**The freeze annotation is writable by anybody who may write a Bucket.** Setting it before the first
provisioning selects the physical name directly, bypassing D1 and D2. What contains this is the
ownership check, not the annotation: a bucket carrying another CR's ownership tags parks the Bucket
with a collision, and once `status.resolvedBucketName` is set the annotation no longer decides
anything. The gap that remains is a pre-existing bucket with no ownership tags at all and no objects
in it, which is adopted by design — the same state a crash between create and tag-write leaves.

**An invalid prefix is a startup failure, not a degraded mode.** Verified: the process exits before
the manager starts. Not verified in a cluster: how that presents under a Deployment follows from the
restart policy — a Pod that never becomes ready — and was not observed here.

**The disaster-recovery replay of D6 has no automated test.** Composition, the freeze and the
resolution order are covered offline, and composed names are exercised against the real provider by
the end-to-end suite. That a restore into a fresh cluster re-adopts rather than duplicates is
verified only by reading which inputs the composition uses.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — the namespace as trust boundary; D2
  is the ownership attribution the frozen name is the address for, D4 the reason the policy is not a
  spec field
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — attribution through the
  bucket; D6 and D7 shape two of the alternatives above
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — one deployment, one project,
  one region; the naming policy is install-time for the same reason the region is
* [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) — teardown, which addresses the
  frozen name
* [ADR 0010](0010-the-operator-never-writes-to-a-bucket-spec.md) — D3 names this record's annotation
  as one of the two metadata writes the operator makes
* [docs/operations/bucket-naming.md](../operations/bucket-naming.md) — the operator-facing
  consequence: what the bucket is called, the two failure modes, and the values a restore must
  reproduce
* [docs/operations/gitops.md](../operations/gitops.md) — why a replay from Git reaches the same
  buckets
* [docs/developer/bucket-identity.md](../developer/bucket-identity.md) — how composition, the freeze
  and the ownership tags are implemented
* [docs/security/ownership-and-attribution.md](../security/ownership-and-attribution.md) — what the
  ownership tags defend against and where they do not reach
