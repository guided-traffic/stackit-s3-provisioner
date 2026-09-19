# GitOps

This page is for somebody who keeps `Bucket` manifests in Git and lets a controller
re-apply them continuously — Flux, Argo CD, or a `kubectl apply` in a pipeline. It explains
why a sync is always a no-op, what the operator does put on a `Bucket` that no manifest
declares, how to health-check a `Bucket` without turning a provider blip into a cluster-wide
alert storm, and what a disaster-recovery replay from Git does and does not restore. For the
complete `Bucket` field list and the complete chart values, read the reference in
[README.md](../../README.md) — this page names only the handful of settings whose GitOps
consequence is not obvious. For installing the operator itself see
[deployment.md](deployment.md); for reading a single `Bucket` see
[bucket-status.md](bucket-status.md).

## A sync is a no-op

Re-applying an unchanged `Bucket` manifest changes nothing, does not wake the operator, and
produces no provider call. That is a composite of several rules, each recorded separately,
and it is worth stating as one property because it is the property an integrator plans
around:

* nothing the operator learns is written where a manifest could declare it, so a diff engine
  sees no drift to correct;
* nothing the operator writes routinely wakes the operator, so its own bookkeeping cannot
  become a loop;
* every trigger it offers compares a value it was given against a value it recorded, so
  re-delivering the same trigger does nothing.

Verified against the code: a `Bucket` object is written outside the status subresource in
exactly three places — the finalizer is added before any provisioning work
([bucket_controller.go](../../internal/controller/bucket_controller.go), `Reconcile`), the
finalizer is removed after the last cloud resource is gone (`dropFinalizer`), and the frozen
physical name is written to an annotation before the first cloud resource is created
(`persistResolvedName`). No path assigns to a field of `spec` or to a label of a `Bucket`.

Not verified, and this is the gap: the composite property is verified rule by rule, not as a
whole. No suite in this repository drives a syncing controller against a live operator, and
the claim has been exercised in practice only against Flux
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md), Residual risks).

### The rules it rests on

| Rule | What it means for a sync | Record |
|---|---|---|
| The operator never writes a `Bucket`'s `spec` | A value the operator computes is published in `status` or in the credentials Secret; the fields in Git are never rewritten under you | [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D1 |
| The operator never writes a `Bucket`'s labels | Your labels and label selectors are yours alone; the operator selects nothing by them | [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D2 |
| Exactly two pieces of metadata are written, both by the operator alone | A finalizer and one annotation; see the table below | [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D3 |
| Everything observed goes to the status subresource | A status write does not change `metadata.generation`, so it is invisible to a manifest diff | [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D4 |
| A configuration fault parks the object instead of retrying it | A broken manifest waits for the edit in Git rather than spending the shared provider budget | [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D5 |
| Credential rotation is level-based | The rotation annotation value lives in Git; changing it rotates once, re-syncing it does nothing, and the operator never clears it | [ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D8, [ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D6 |
| A completed clone is terminal | Once `status.clone.phase` is `Completed`, a re-applied or even edited `spec.cloneFrom` never copies again | [ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D5 |
| A replay reaches the same buckets | Bucket identity is derived from install-time configuration plus the CR's own namespace and name, never from cluster-assigned state such as the CR UID | [ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D6 |

### What the operator puts on a Bucket that your manifest does not declare

| Written | Value | When | What a diff engine should do with it |
|---|---|---|---|
| Finalizer `stackit-bucket.gtrfc.com/finalizer` | — | Added before any provisioning work begins | Leave it. Removing it strands the cloud bucket, its credentials group and its access key ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)) |
| Annotation `stackit-bucket.gtrfc.com/resolved-bucket-name` | The physical bucket name | Written before the first cloud resource is created | Leave it. It is the durable backup of the frozen name when `status` is lost ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3) |

Neither is declared by any manifest, so neither is owned by a syncing controller's field
manager and neither is pruned. Not verified: how a server-side-apply configuration that
prunes fields it does not own treats the annotation. If it were removed while
`status.resolvedBucketName` survived, the name would still resolve from status; only losing
both would recompose the name from the operator's *current* naming policy
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3).

### Verify it

```bash
# Nothing to apply: a server-side dry run against the live object prints no diff for the
# finalizer or the annotation, because your manifest declares neither.
kubectl diff -f bucket.yaml

# The operator did not re-enter its loop: generation is unchanged and matches what it observed.
kubectl get bucket my-bucket -n team-a \
  -o jsonpath='{.metadata.generation} {.status.observedGeneration}{"\n"}'
```

A `Bucket` whose `metadata.generation` equals `status.observedGeneration` and whose phase is
`Ready` has converged; a sync that leaves both numbers where they were did nothing.

## A minimal Flux setup

```yaml
# example - the Kustomization that applies the Bucket and gates on it
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: team-a-storage
  namespace: flux-system
spec:
  interval: 10m
  path: ./clusters/mgmt-p/team-a
  prune: true                     # removing the Bucket from Git DELETES the cloud bucket - see "Pruning a Bucket from Git"
  sourceRef:
    kind: GitRepository
    name: platform
  timeout: 15m                    # must exceed FIRST-time provisioning, not the steady state - see "Health-checking a Bucket"
  dependsOn:
    # kustomize-controller resolves this against another Kustomization, never against a
    # HelmRelease: name the Kustomization that installs the operator (and with it the Bucket CRD).
    - name: storage-provisioner   # example
  healthChecks:                   # optional; read "Health-checking a Bucket" before enabling it
    - apiVersion: stackit-bucket.gtrfc.com/v1
      kind: Bucket
      name: my-bucket
      namespace: team-a
---
# example - the Bucket itself
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: my-bucket
  namespace: team-a
  annotations:
    # Level-based: changing this value rotates the workload key exactly once. Every later
    # sync of the SAME value is a no-op, and the operator never edits or clears it.
    stackit-bucket.gtrfc.com/rotate-credentials-at: "2026-09-19T08:00:00Z"   # example
spec:
  bucketName: my-bucket   # immutable after creation; the PHYSICAL name may differ - see bucket-naming.md
  region: eu01            # default - must equal the operator's region, or the Bucket parks as Failed
  secretRef:
    name: my-bucket-s3    # the operator CREATES and OWNS this Secret - never declare it in Git
```

Two things in that block are easy to get wrong:

* **Do not declare the credentials Secret in Git.** The operator creates it in the `Bucket`'s
  namespace, sets the `Bucket` as its controller owner, labels it
  `app.kubernetes.io/managed-by: stackit-s3-provisioner` and merges the credential keys into
  it ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D11,
  [ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1). A Secret of that name
  that already carries a *different* controller owner cannot be adopted and the credential
  write fails. Not verified: what a syncing controller that also declares the same Secret does
  to the operator's data keys on each sync — no such setup has been exercised.
* **`dependsOn` here is about the operator, not about the application** — and it must name the
  `Kustomization` that installs the operator. Flux resolves `dependsOn` against other
  `Kustomization` objects, so pointing it at a `HelmRelease` name does not order anything.
  (Flux behaviour; not verifiable in this repository.) The CRD ships inside
  the chart's `templates/` (with `helm.sh/resource-policy: keep`), so it is applied on install
  *and* on every upgrade; set `crds.install=false` only when a separate cluster-admin pipeline
  owns it. Until the CRD exists, a `Bucket` manifest is an unknown kind and the Kustomization
  reports an error and retries.

## Why the application needs no dependsOn

The credentials Secret appears when the bucket is usable, never before. The provisioning pass
creates the bucket, writes the isolation policy, and only then mints the workload access key
and writes the Secret. Verified in `provisionCredentialsAndClone`
([bucket_controller.go](../../internal/controller/bucket_controller.go)): the policy call
precedes every path that can mint a key, so the bucket is never open to the rest of the
project while it is still being populated. With
`spec.cloneFrom` set and the default `holdSecretUntilCloned: true`, the Secret is additionally
withheld until the copy has succeeded, so no workload ever starts against a half-copied bucket
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D7).

So an application and its `Bucket` can be deployed in the same sync, in any order. A pod that
consumes the Secret through `envFrom` or `secretKeyRef` does not start until the Secret exists;
that is the kubelet's behaviour (the container stays in `CreateContainerConfigError` and is
retried), not something this repository enforces or tests. No dependency choreography between
the two is required, and adding one buys nothing.

The one ordering that *is* required is the operator and its CRD before the first `Bucket`, as
above.

## Health-checking a Bucket

A `Bucket` reports `status.conditions[type=Ready]`, `status.phase` and
`status.observedGeneration`, which is what a kstatus-based evaluator such as Flux reads.
`status.observedGeneration` is advanced only once the operator is finished with the current
spec. It has exactly three writers in
[bucket_controller.go](../../internal/controller/bucket_controller.go): `Reconcile` for
skeleton mode, `reconcileNormal` on success and `markFailed` on a failure. The in-flight
write is the counter-example — `markProvisioning` deliberately sets only the phase and the
message and never touches `observedGeneration`, which is why a pass that is still running
never advances it. A `Bucket` that is
still being provisioned therefore reads as in-progress rather than as healthy, and it carries
no `Ready` condition at all until the first pass reaches an outcome.

Two properties decide whether gating a rollout on a `Bucket` is a good idea:

**A provisioned `Bucket` holds `Ready` through a provider failure.** `Ready` describes the
last *verified* state, not the outcome of the last attempt to verify it
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D1). While the hold is in
effect the object carries `status.degradedSince` and the condition `ProviderReachable=False`
with reason `ProviderUnreachable`, a `Warning` event with reason `Failed` is still recorded,
and the error is still in `status.message` — but the health check passes. The hold is bounded
by `providerDegradedGrace` (`30m` # default); once it elapses the `Bucket` drops to `Failed`
and the health check fails as it did before
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D5). An enumerated set of cases
skips the hold entirely and drops `Ready` at once — a structured `400`, `401` or `403` from the
provider (a revoked service-account key is the `400`), a workload credential the operator
itself destroyed, a configuration fault, a `Bucket` being deleted, a spec that has not been
observed, and a `Bucket` that was never `Ready`
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D6).

This behaviour exists because of a measured amplification. On 2026-08-25 on the mgmt-p cluster,
the provider's edge answered `403` as an HTML error page for roughly two minutes; all 19
`Bucket` objects on the cluster went non-ready at once and roughly eleven tenant Kustomizations
that health-check them went `Ready=False` behind them, firing the cluster's Flux readiness
alert for seven unrelated applications and their configuration Kustomizations. The provisioner was the amplifier, and holding `Ready` is what
removes it. A health check therefore tells you about the *bucket*; it deliberately does not tell
you whether the operator can currently reach the provider. For that, watch
`ProviderReachable`, `stackit_s3_provisioner_buckets_provider_degraded` and
`stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds` — see
[provider-outages.md](provider-outages.md) and [monitoring.md](monitoring.md).

**The timeout must cover first-time provisioning, not the steady state.** Also observed on
mgmt-p, on 2026-08-22: the initial provisioning of one `Bucket` took longer than the
Kustomization's 5-minute timeout, and the Kustomization went unhealthy for that reason alone.
A `Bucket` with `spec.cloneFrom` is the extreme case — `Ready` waits for the whole copy
whatever `holdSecretUntilCloned` says
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D7) — so either
give that Kustomization a timeout above the expected copy time or leave the `Bucket` out of its
health checks. Progress is readable meanwhile in `status.clone.progress`
([cloning.md](cloning.md)).

## When a Bucket parks

A configuration fault sets phase `Failed`, `Ready=False` with reason `Failed`, puts the reason
in `status.message`, records a `Warning` event and **ends the reconcile without a requeue**
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D5). Nothing retries it.
The faults and their exact messages are in [bucket-status.md](bucket-status.md); what matters
here is how a parked object comes back:

| Where the fix lives | How the `Bucket` resumes |
|---|---|
| The manifest in Git (a wrong `spec.region`, colliding `spec.secretRef.keys`, a `secretRef` aimed at the operator's admin Secret, a self-referencing `spec.cloneFrom`) | Commit the fix. The changed spec bumps `metadata.generation`, which is a trigger, so the next sync reconciles it |
| An annotation on the CR | Any annotation change is a trigger, so adding or changing one resumes a parked `Bucket` — the escape hatch when the fault is not in the spec |
| The operator's install-time configuration (a `bucketNaming` prefix that pushes the composed name past 63 characters, an `ownership.name` that no longer matches the buckets' `managed-by` tag) | Correct the chart values. Every one of these values is rendered into the Deployment's container arguments, so changing it rolls the pod, and a restart reconciles every `Bucket` once |
| Somewhere else entirely (an ownership collision cleared by whoever owned the colliding bucket) | Nothing notices. A parked object is not requeued and the drift timer only applies after a *successful* reconcile. Touch the CR or restart the operator |

The periodic re-reconcile that heals ordinary drift — `driftResyncInterval`
(`--drift-resync-interval`, `DRIFT_RESYNC_INTERVAL`, `10m` # default, `0` disables it) — is a
`RequeueAfter` on a successful pass and therefore does **not** apply to a parked object
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D8). It exists because the
`Bucket` trigger is narrowed to generation and annotation changes, which discards the periodic
re-delivery a controller would otherwise get for free; see [configuration.md](configuration.md).

## Pruning a Bucket from Git

With `prune: true`, removing a `Bucket` from Git deletes the CR, and deleting the CR deletes the
cloud bucket, its credentials group, its access key and the credentials Secret. The finalizer
holds the object until that teardown has actually completed, and the teardown refuses a bucket
that still contains objects
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)).

What that looks like when it blocks:

* the CR stays, with a deletion timestamp, phase `Failed` and
  `status.message: bucket "<name>" is not empty; refusing to delete (data-loss guard)`;
* a `Warning` event with reason `Failed` carrying the same text;
* the delete is retried on the controller's own backoff, which starts at one second and caps at
  fifteen minutes (`bucketRateLimiter` in
  [bucket_controller.go](../../internal/controller/bucket_controller.go)), so emptying the bucket
  by hand completes the deletion without further action;
* the syncing controller sees an object it asked to remove that is still there, and reports the
  Kustomization as not finished until the finalizer clears.

Emptying the bucket is what unblocks it; the opt-in wipe and the exact procedure are in
[deletion.md](deletion.md). While the fleet-wide circuit breaker is open the teardown makes no
provider call at all and the finalizer simply stays
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D4).

## Disaster recovery: replaying from Git

Re-applying the same manifests into a fresh cluster **re-adopts the existing buckets instead of
duplicating them**, because a bucket's identity is composed from install-time configuration and
the CR's own namespace and name — never from cluster-assigned state such as the CR UID
([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D6). Verified in
the code: `ownerTagValue` is `namespace + "/" + name` and excludes `metadata.uid`, and
`isOwnedByUs` requires both the `managed-by` and the `owner` tag to match
([bucket_controller.go](../../internal/controller/bucket_controller.go)).

That only holds if the new installation reproduces the identity values of the old one. These
are the install-time values that participate:

| Value | Flag / environment | Effect if it differs from the original installation |
|---|---|---|
| `ownership.name` (`stackit-s3-provisioner` # default) | `--ownership-name` / `OWNERSHIP_NAME` | The operator reads its own buckets' `managed-by` tag as foreign: every `Bucket` parks with an ownership collision until the buckets are re-tagged. This is also why two operators against one STACKIT project need *distinct* values |
| `bucketNaming.prefix` (`""` # default, disabled) | `--bucket-name-prefix` / `BUCKET_NAME_PREFIX` | The composed physical name differs, so a restored `Bucket` that lost its status *and* its frozen-name annotation provisions a new, empty bucket beside the live one |
| `bucketNaming.includeNamespace` (`false` # default) | `--bucket-name-include-namespace` / `BUCKET_NAME_INCLUDE_NAMESPACE` | Same as the prefix |
| The STACKIT project and the region (`stackit.region`, `eu01` # default) | the service-account key binds the project; `--stackit-region` / `STACKIT_REGION` sets the region | A different project or region is a different bucket namespace entirely; a `spec.region` that does not match the operator's parks the `Bucket` ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D3) |

Keep these in the same Git repository as the manifests. The naming details are in
[bucket-naming.md](bucket-naming.md).

**What replays and what does not.** A manifest carries `spec`, labels and annotations. It does
not carry `status`, and three behaviours read their history from `status`:

| Restored from Git | Not restored, and the consequence |
|---|---|
| `spec`, labels, the rotation annotation, the frozen-name annotation *if* the CR was backed up rather than re-applied from the manifest | `status.resolvedBucketName` — the name still resolves from the annotation when the CR was backed up; re-applying a bare manifest recomposes it, which is correct only while the naming policy is unchanged ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D3) |
| — | `status.lastRotationTrigger` — a `rotate-credentials-at` annotation in Git is treated as unhandled and rotates once after the restore. Harmless in practice: the key is being replaced anyway, see the next row |
| — | The credentials Secret, which lives in the cluster and not in Git. Without it the pass mints a fresh access key after deleting every key in the group, so **workload credentials change on a restore into a fresh cluster** and the pods that hold them must re-read the Secret ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D4, D5, D9) |
| — | `status.clone.phase` — and this one bites. A completed clone is terminal only through that status field, so a `Bucket` restored with `spec.cloneFrom` still in its manifest **runs the copy again**. The copy merges and never deletes ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D4), so nothing is lost, but the source is copied over the destination a second time and `Ready` waits for it |

The clone case has a cheap remedy, and it is worth doing the day the clone finishes: once
`status.clone.phase` is `Completed`, remove `spec.cloneFrom` from the manifest in Git. The field
is not immutable (only `spec.bucketName` is), removing it changes nothing about the live object,
and it takes the re-seed out of every future replay.

The operator's own admin credential is not part of this: it lives in a Secret in the operator
namespace, and a fresh installation re-bootstraps it by clearing the admin group's keys and
minting a new one ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)) —
see [credentials.md](credentials.md).

## Argo CD and plain kubectl

Nothing in the operator is Flux-specific. What differs between tools is only how they report
health and how aggressively they prune.

**Argo CD.** A custom resource with no health customisation is assessed as healthy as soon as it
exists, which is not what you want for a `Bucket`. A resource customisation reading the same two
fields a kstatus evaluator reads gives the same behaviour as the Flux setup above:

<details>
<summary>An Argo CD health check for <code>Bucket</code> (not exercised by any suite in this repository)</summary>

```yaml
# example - argocd-cm ConfigMap
data:
  resource.customizations.health.stackit-bucket.gtrfc.com_Bucket: |
    hs = {}
    if obj.status ~= nil then
      if obj.status.observedGeneration ~= nil and obj.status.observedGeneration ~= obj.metadata.generation then
        hs.status = "Progressing"
        hs.message = "waiting for the operator to observe the current spec"
        return hs
      end
      if obj.status.conditions ~= nil then
        for i, c in ipairs(obj.status.conditions) do
          if c.type == "Ready" then
            if c.status == "True" then
              hs.status = "Healthy"          -- includes a Bucket whose Ready is being HELD
            elseif obj.status.phase == "Failed" then
              hs.status = "Degraded"
            else
              hs.status = "Progressing"
            end
            hs.message = c.message
            return hs
          end
        end
      end
    end
    hs.status = "Progressing"
    hs.message = "no Ready condition yet"    -- the state during first-time provisioning
    return hs
```

Not verified: no suite in this repository runs Argo CD. The snippet reads only fields verified
to exist on the CRD — `status.observedGeneration`, `status.phase` and
`status.conditions[].type/status/message`. There is deliberately no branch on the condition's
`reason`: the only reasons the controller ever writes on `Ready` are `Provisioned`, `Failed`
and `NotImplemented` (skeleton mode), and during first-time provisioning there is no `Ready`
condition to read at all. The Lua contract itself is Argo CD's.

</details>

Also for Argo CD: do not enable pruning of the credentials Secret or of the operator-written
metadata. The Secret is not declared in Git and is owned by the `Bucket`; the finalizer and the
frozen-name annotation are covered in *What the operator puts on a Bucket* above.

**Plain `kubectl`.** `kubectl apply` performs a three-way merge against its own
`last-applied-configuration`, so it removes only keys that it previously applied. The
operator's finalizer and annotation were never in a `kubectl apply` manifest and are left
alone; `kubectl diff -f` therefore prints nothing for them. Not verified against a live
cluster in this repository — that is kubectl's documented merge behaviour, not a property this
repository tests. Note that `kubectl delete bucket` blocks until the teardown completes, for
the same reason a Flux prune does; see *Pruning a Bucket from Git*.

## What this page could not verify

| Claim | Why it is not verified |
|---|---|
| The composite "a sync is a no-op" property, end to end | No suite drives a syncing controller against a live operator; it is verified rule by rule and exercised in practice only against Flux |
| That another tool's diff engine leaves the finalizer and the frozen-name annotation alone | Only Flux has been observed doing so; a server-side-apply configuration that prunes unowned fields has not been tried |
| What happens if a syncing controller also declares the credentials Secret | No such setup has been exercised. The operator merges its data keys into the Secret and requires controller ownership of it |
| The Argo CD health snippet | No suite in this repository runs Argo CD |
| That a Flux `dependsOn` resolves only against other `Kustomization` objects | Flux's own contract, not a property this repository exercises; no Flux controller runs in any suite here |
