# Deployment

Installing, upgrading and uninstalling the operator, including the paths the
[README](../../README.md) fast start deliberately leaves out: FluxCD, running without a
service-account key, leader election, and what survives an uninstall in the cloud.

This page explains a handful of settings and never restates the list — the complete Helm value and
CRD surface lives in the [README reference](../../README.md) and nowhere else. For what has to exist
in the StackIT account before any of this works, read [prerequisites.md](prerequisites.md); for the
settings whose consequences are not obvious, [configuration.md](configuration.md); for why a GitOps
sync of an installed release is a no-op, [gitops.md](gitops.md).

---

## What one release is, and what it is bound to

One operator deployment serves exactly **one StackIT project in exactly one region**. The project
comes from the service-account key, the region from `stackit.region`, and both are resolved once at
process start — replacing the key or changing the region takes effect on the next restart, not
before ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D1, D2, D8). A
second project or a second region means a second release with its own key, its own namespace and its
own bootstrap admin credential; two operators must never share a project, because they would contend
for the same project-wide admin identity ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D9).

The chart ([`deploy/helm/stackit-s3-provisioner/`](../../deploy/helm/stackit-s3-provisioner/))
renders these objects:

| Object | Rendered when | Notes |
| --- | --- | --- |
| `Deployment` | always | one container, ports `8080` (metrics) and `8081` (probes), both fixed |
| `ServiceAccount` | `serviceAccount.create` (`true` # default) | otherwise `serviceAccount.name`, or `default` |
| `ClusterRole` + `ClusterRoleBinding` | always | the operator's own permissions; the `coordination.k8s.io/leases` rule only with leader election on |
| `CustomResourceDefinition` (`buckets.stackit-bucket.gtrfc.com`) | `crds.install` (`true` # default) | rendered from `templates/`, so it is applied on install **and** upgrade; carries `helm.sh/resource-policy: keep` |
| `<release>-view` / `<release>-edit` `ClusterRole` | `bucketRoles.create` (`true` # default) | aggregation fragments for the built-in `view`/`edit`/`admin` |
| `NetworkPolicy` `<release>-clone-rc` | `clone.networkPolicy.enabled` (`true` # default) | restricts ingress to clone Job pods to the operator, TCP `5572` |
| metrics `Service` + `ServiceMonitor` | `monitoring.serviceMonitor.enabled` (`false` # default) | needs the `monitoring.coreos.com` CRDs |
| `PrometheusRule` | `monitoring.prometheusRule.enabled` (`false` # default) **and** at least one alert enabled under `monitoring.prometheusRule.alerts` | needs the same CRDs; with every alert switched off the whole resource is skipped, because prometheus-operator rejects a rule group without rules |

Two objects are **not** in the release, are created by the operator at runtime, and persist
independently of it:

- the **admin credentials Secret** in the release namespace (`stackit-s3-provisioner-admin`
  # default), written on the first `Bucket` reconcile, not at startup. It carries no owner
  reference, so nothing garbage-collects it
  ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D3);
- the **workload credentials Secret** in each `Bucket`'s own namespace, owned by its CR
  ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1).

Two more kinds of object are operator-created but transient: while a `Bucket` with `spec.cloneFrom`
is copying, the operator runs a clone `Job` and an rclone staging Secret named after it with an
`-src` suffix, both in the release namespace under operator-derived names that no CR can influence
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D5). Both are described in
[cloning.md](cloning.md).

The operator pod runs as non-root from a distroless image with a read-only root filesystem, all
capabilities dropped, no privilege escalation and `seccompProfile: RuntimeDefault`, so it satisfies
the `restricted` Pod Security Standard without extra values.

---

## Install with Helm

The chart is published as a plain Helm repository from GitHub Pages.

```bash
helm repo add stackit-s3-provisioner https://guided-traffic.github.io/stackit-s3-provisioner/
helm repo update

# 1. The release namespace. It is also where the operator keeps its admin credential.
kubectl create namespace stackit-s3-provisioner-system   # example

# 2. The StackIT service-account key (key flow, RSA private key embedded in the JSON).
kubectl -n stackit-s3-provisioner-system create secret generic stackit-sa-key \
  --from-file=sa-key.json=./account.json

# 3. The release.
helm install stackit-s3-provisioner stackit-s3-provisioner/stackit-s3-provisioner \
  --namespace stackit-s3-provisioner-system \
  --values values.yaml
```

A minimal `values.yaml` that provisions for real:

```yaml
stackit:
  region: eu01                        # default; must match spec.region on every Bucket
  serviceAccountKey:
    secretName: stackit-sa-key        # the Secret created above; empty = skeleton mode
    secretKey: sa-key.json            # default; mounted at /etc/stackit/<secretKey>

# The identity written into every provisioned bucket's managed-by tag. Changing it after
# buckets exist makes the operator treat its own buckets as foreign; a DR restore into a
# new cluster must reproduce this value verbatim.
ownership:
  name: stackit-s3-provisioner        # default

# Keep this on. During a rolling update the incoming pod starts before the outgoing one
# terminates, and without a single leader both reconcile at once.
leaderElection:
  enabled: true                       # default

# Periodic re-reconcile so a policy change shipped in a new operator version reaches
# already-provisioned buckets. Go duration WITH A UNIT - a bare number crash-loops the pod.
driftResyncInterval: "10m"            # default
```

Everything else has a working default. The two identity-bearing values —
`stackit.serviceAccountKey.secretName` and `ownership.name` — are the ones a restore has to
reproduce exactly; see [bucket-naming.md](bucket-naming.md) for the rest of that set.

### Verify the installation

In order, stopping at the first that fails:

```bash
NS=stackit-s3-provisioner-system      # example

# 1. The pod is up and stayed up.
kubectl -n $NS rollout status deploy/stackit-s3-provisioner

# 2. The binding is the one you meant. This line is only logged when a key was loaded.
kubectl -n $NS logs deploy/stackit-s3-provisioner | grep "StackIT client configured"
#   ... "project": "<projectId from the key>", "region": "eu01"

# 3. The CRD is served.
kubectl get crd buckets.stackit-bucket.gtrfc.com

# 4. End to end: a throwaway Bucket in a namespace you own.
kubectl -n default apply -f - <<'EOF'
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: probe
spec:
  bucketName: probe-bucket-0001       # example; the physical name may carry a prefix, see bucket-naming.md
  secretRef:
    name: probe-credentials
EOF
kubectl -n default get bkt probe -w         # PHASE must reach Ready
kubectl -n default get secret probe-credentials

# 5. The admin credential exists now - it is minted on the FIRST reconcile, not at startup.
kubectl -n $NS get secret stackit-s3-provisioner-admin

# 6. Clean up. Deletion only succeeds while the bucket is empty, which a probe bucket is.
kubectl -n default delete bkt probe
```

Step 5 is the reason a fresh install against a healthy project still makes **no** cloud call until
the first `Bucket` appears: the operator does not verify the admin credential at startup
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9), and the
bootstrap is reached only from the reconcile paths in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go), never
from [`cmd/main.go`](../../cmd/main.go). An empty cluster therefore proves the deployment but not
the credential.

How to read the phases, conditions and columns in step 4 is
[bucket-status.md](bucket-status.md); why step 6 can hang is [deletion.md](deletion.md).

---

## Install with FluxCD

A `HelmRepository` plus a `HelmRelease` is all Flux needs. The service-account key is **not** part of
the release and must already exist in the release namespace when the `HelmRelease` reconciles.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: stackit-s3-provisioner
  namespace: flux-system
spec:
  interval: 1h
  url: https://guided-traffic.github.io/stackit-s3-provisioner/
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: stackit-s3-provisioner
  namespace: stackit-s3-provisioner-system   # example
spec:
  interval: 1h
  chart:
    spec:
      chart: stackit-s3-provisioner
      version: "1.x"                 # example; pin exactly if you do not want automatic minors
      sourceRef:
        kind: HelmRepository
        name: stackit-s3-provisioner
        namespace: flux-system
  install:
    createNamespace: true
  values:
    stackit:
      region: eu01                   # default
      serviceAccountKey:
        secretName: stackit-sa-key   # the Secret below, same namespace as the release
```

The key Secret ships next to the `HelmRelease`, encrypted in Git — it contains a usable RSA private
key for the whole StackIT project, so it must never be committed in plain text. With
[SOPS](https://fluxcd.io/flux/guides/mozilla-sops/), SealedSecrets or ExternalSecrets, the decrypted
object has to end up as:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: stackit-sa-key
  namespace: stackit-s3-provisioner-system   # example; the release namespace, not flux-system
stringData:
  sa-key.json: |                             # default key name (stackit.serviceAccountKey.secretKey)
    { … service-account key JSON … }
```

At rest in the cluster it is a plain Secret, so it is protected by whatever the cluster's etcd
encryption and namespace RBAC provide and by nothing else — see
[credentials-and-secrets.md](../security/credentials-and-secrets.md).

Two Flux-specific notes:

- **Ordering.** If the Secret arrives after the `HelmRelease`, the pod does not start at all. The
  chart mounts the key as a **non-optional** Secret volume
  ([`templates/deployment.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml)),
  so the kubelet holds the pod in `ContainerCreating` with a `FailedMount` event
  (`MountVolume.SetUp failed for volume "stackit-sa-key": secret "stackit-sa-key" not found`) —
  there is no container, therefore no log line and no `CrashLoopBackOff`. It does not fall back to
  skeleton mode ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D7).
  Nothing is lost: the pod starts by itself once Flux applies the Secret. A Secret that *exists* but
  carries a different data key, or JSON the operator cannot use, is the other failure — there the
  container does start and then crash-loops; see the failure table below.
- **CRD ownership.** With `crds.install: true` (# default) the CRD is part of the release and is
  re-applied on every upgrade. Set it to `false` only when a separate cluster-admin pipeline applies
  [`config/crd/bases/`](../../config/crd/bases/) itself — and then that pipeline owns keeping the CRD
  in step with the chart version.

A Flux sync of an already-installed release changes nothing, by design; the reasoning is in
[gitops.md](gitops.md).

---

## Skeleton mode: installing without a key

Leaving `stackit.serviceAccountKey.secretName` empty is a supported configuration, not a broken one.
The operator starts, serves its probes, watches `Bucket` resources and reconciles them — and makes no
provider call in either direction, neither provisioning nor teardown
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D6). Every `Bucket` lands
in:

| Where | Value |
| --- | --- |
| `status.phase` | `Pending` |
| `Ready` condition | `False`, reason `NotImplemented` |
| `status.message` | `operator skeleton: no StackIT service-account key configured` |
| Metric | `stackit_s3_provisioner_skeleton_mode` = `1` |
| Alert | `StackitS3SkeletonMode`, severity `critical`, after 15m |

It is useful for a cluster that should serve the CRD before its project exists, and it is what the
Kind smoke suite installs. It is dangerous for exactly one reason: probes stay green while nothing is
provisioned, which is why the alert is `critical` and on by default once
`monitoring.prometheusRule.enabled` is set.

Skeleton mode is entered **only** when no key path is configured at all. A key that is configured but
unusable — Secret missing, wrong `secretKey`, unparseable JSON, no `projectId` — aborts startup
instead ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D7). The operator
never degrades from "should provision" to "provisions nothing".

---

## Leader election, replicas and rolling updates

`leaderElection.enabled` (`true` # default) renders `--leader-elect` into the manager's arguments and
adds the `coordination.k8s.io/leases` rule to the operator's ClusterRole. The flag's own Go default
in [`cmd/main.go`](../../cmd/main.go) is **off**; the chart turns it on, so a hand-rolled deployment
that bypasses the chart does not get it.

What it buys is not high availability — it is the guarantee that **two operator versions never
reconcile at once**. During a rolling update the incoming pod starts before the outgoing one
terminates. Without a single leader both reconcile, and an older version can re-apply a stale bucket
policy over the document the new one just wrote; that is how a shipped policy fix failed to reach an
existing bucket. With leader election on, the incoming pod waits for the lease.

The supporting pieces, all in
[`templates/deployment.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml) and
[`cmd/main.go`](../../cmd/main.go):

- the lease is `stackit-s3-provisioner.stackit-bucket.gtrfc.com` in the release namespace;
- `LeaderElectionReleaseOnCancel` is set, so a graceful shutdown hands the lease over in seconds
  instead of making the successor wait out the full lease duration;
- `terminationGracePeriodSeconds: 40` exceeds controller-runtime's 30s graceful-shutdown timeout, so
  an in-flight reconcile drains and the lease is released before the kubelet sends `SIGKILL`.

**The one rollout that flips this `false` → `true` can still race**, because the outgoing pod predates
leader election and keeps reconciling freely. Nothing in the chart can fix that rollout; what repairs
any stale policy it leaves behind is the periodic `driftResyncInterval` re-reconcile. Every later
rollout is protected.

`replicaCount` above `1` gives warm standbys, not parallelism: the non-leaders run their manager but
reconcile nothing. It costs a second pod and buys a faster failover than a rescheduled pod. The
default is `1`.

---

## Upgrading

```bash
helm repo update
helm upgrade stackit-s3-provisioner stackit-s3-provisioner/stackit-s3-provisioner \
  --namespace stackit-s3-provisioner-system \
  --values values.yaml
kubectl -n stackit-s3-provisioner-system rollout status deploy/stackit-s3-provisioner
```

The release pipeline writes the chart `version`, the `appVersion` and `image.tag` from the release
tag, so a published chart version **is** an operator version and pinning the chart pins the image.
(In the repository's own copy of the chart, `version` and `appVersion` are a stale `0.1.0` that the
release pipeline overwrites from the tag, and `image.tag` is empty in `values.yaml`, which falls back
to `appVersion` — that matters only when installing from a working tree.)

### A policy change reaches existing buckets by itself

Every reconcile recomputes the isolation document the bucket should have, compares it against the
live one ignoring key order and whitespace, and rewrites only on a difference
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8). The `Bucket` watch
only fires on generation and annotation changes, so an untouched CR would never trigger that
comparison after an upgrade; the `driftResyncInterval` requeue (`10m` # default) is what closes the
gap. After an upgrade that changes the policy, every provisioned bucket is corrected within one
interval, with **no CR edit and no CR deletion**.

> **Never delete a `Bucket` CR to force a policy change.** Deletion is a teardown: it removes the
> cloud bucket, its credentials group and its access key, and where a wipe carries all three
> authorizations — `spec.wipeOnDelete: true` on the CR, the operator-wide gate on, **and** ownership
> tags proving this operator provisioned that bucket for that CR — it destroys the objects first
> ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D4). Waiting one drift
> interval is the entire procedure.

Setting `driftResyncInterval: "0"` disables the requeue and leaves the operator event-driven only. A
value without a unit (`10` instead of `"10m"`) is rejected by Go's flag parser and crash-loops the
pod at startup.

### Trap 1: an object of the same name that Helm did not create

Helm does not adopt an existing object that lacks its ownership metadata. The upgrade fails with
*"… exists and cannot be imported into the current release"* and, under Flux, the `HelmRelease` rolls
back. This is a real path, not a hypothetical: a cluster that pre-created a stop-gap ClusterRole
named `stackit-s3-provisioner-view` collides head-on with the chart's `<release>-view` role once
`bucketRoles.create` renders it under the same release name.

Two fixes, in preference order:

1. **Adopt the object** — order-independent, so it works whether the prune or the upgrade lands
   first. Add to the existing object:

   ```yaml
   metadata:
     labels:
       app.kubernetes.io/managed-by: Helm
     annotations:
       meta.helm.sh/release-name: stackit-s3-provisioner        # example
       meta.helm.sh/release-namespace: stackit-s3-provisioner-system   # example
   ```

2. **Delete it first**, before the chart version that starts rendering the same name is applied.
   Under GitOps this means the prune must land in an earlier sync than the release bump, which is
   exactly the ordering assumption fix 1 avoids.

The same applies to any object the chart starts rendering that a cluster already maintains by hand —
a metrics `Service`, a `ServiceMonitor`, a `NetworkPolicy`.

### Trap 2: the CRD ships inside the chart version

The CRD is generated from [`api/v1/`](../../api/v1/) into
[`config/crd/bases/`](../../config/crd/bases/) and synced into the chart's `templates/` by
`make generate-all`; CI fails the release when the checked-in copies drift from the types. Because it
lives in `templates/` and not in Helm's `crds/` directory, it **is** re-applied on upgrade, which is
what makes a new CRD field usable right after `helm upgrade`.

Two consequences:

- With `crds.install: false` nothing updates the CRD. A chart version that adds a field will render
  `Bucket` specs the API server prunes silently, and the operator sees a CR with the field missing.
  Whoever owns the CRD out of band must apply it in the same change as the chart bump.
- Chart and CRD cannot be versioned apart. A CRD change is an operator release.

### Rolling back

`helm rollback` returns to the previous chart version, and with it the previous image. Three
behaviours can also be turned off through values alone, without deploying a different image, when a
new mechanism is the suspect:

| Value | Set to | Effect |
| --- | --- | --- |
| `providerCircuit.threshold` | `0` | no fleet-wide breaker ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D8) |
| `providerDegradedGrace` | `"0"` | a failing reconcile drops `Ready` immediately, as before the hold ([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D5) |
| `bucketUsage.enabled` | `false` | no size measurement traffic at all, whatever a CR asks for |

What those three mechanisms do is [provider-outages.md](provider-outages.md) and
[usage-and-cost.md](usage-and-cost.md).

---

## Failure modes

What goes wrong at install and upgrade time, what it looks like, and what to do. Startup faults are
deliberate `os.Exit` calls in [`cmd/main.go`](../../cmd/main.go): the pod crash-loops with the
message in its log rather than running half-configured. A pod that never reaches
`CrashLoopBackOff` at all failed earlier than the process — check its events before its logs.

| Symptom | What you see | Cause and fix |
| --- | --- | --- |
| `CrashLoopBackOff`, pod log ends immediately | `invalid value "10" for flag -drift-resync-interval` (or `-provider-degraded-grace`, `-provider-circuit-max-cooldown`, `-bucket-usage-interval`, `-bucket-usage-min-interval`) | A duration value without a unit. Quote it and give a unit: `"10m"`. |
| `CrashLoopBackOff` | `invalid bucket naming configuration` | `bucketNaming.prefix` is not a lowercase DNS-1123 label ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D7). |
| `CrashLoopBackOff` | `invalid bucket usage price` | `bucketUsage.pricing.perGBHour` is not a non-negative decimal. Quote it so YAML does not turn it into an exponent. |
| Pod stuck in `ContainerCreating`, **no** container log | Event `FailedMount`: `MountVolume.SetUp failed for volume "stackit-sa-key": secret "…" not found` | The key Secret does not exist in the release namespace. The chart mounts it non-optionally, so no container ever starts. Create the Secret — the kubelet picks it up without a Deployment restart. |
| `CrashLoopBackOff` | `unable to load StackIT service-account key` with `read key file`, `parse key file` or `key file … has no projectId field` | The Secret exists but is wrong: `stackit.serviceAccountKey.secretKey` does not match its data key (`read key file`), the JSON is unparseable (`parse key file`), or it is not a key-flow key (`has no projectId field`) — `LoadAccount` in [`stackit/client.go`](../../stackit/client.go). This is never skeleton mode ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D7). |
| `CrashLoopBackOff` | `operator namespace unknown; set POD_NAMESPACE (or --operator-namespace) when a StackIT key is configured` | The `POD_NAMESPACE` downward-API env var is missing — only possible when the Deployment was not rendered by this chart ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D4). |
| `helm install` fails | `no matches for kind "ServiceMonitor"` / `"PrometheusRule"` | `monitoring.*.enabled` without the `monitoring.coreos.com` CRDs in the cluster. Install prometheus-operator, or leave both `false`. |
| `helm upgrade` fails, Flux rolls back | `… exists and cannot be imported into the current release` | Trap 1 above. |
| Pod runs, every `Bucket` stays `Pending` | `Ready=False`, reason `NotImplemented`; metric `stackit_s3_provisioner_skeleton_mode` = 1 | Skeleton mode: no key configured. Set `stackit.serviceAccountKey.secretName`. |
| A `Bucket` parks in `Failed` right after install | `status.message`: `spec.region "…" does not match this operator's region "…"` | `stackit.region` and `spec.region` disagree. This is definitive and is not retried ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D3, D4). |
| Pod logs `leases.coordination.k8s.io is forbidden` | leader election cannot acquire its lease | The ClusterRole was rendered with `leaderElection.enabled: false` and the flag was added another way. Enable the value so the RBAC rule is rendered with it. |
| Clone Jobs never report progress | clone stuck without `status.clone.progress` | The cluster's CNI ignores `NetworkPolicy`, or a cluster-wide policy blocks TCP `5572` from the operator pod. See [cloning.md](cloning.md). |

Metrics and alerts referenced here are described in [monitoring.md](monitoring.md).

---

## Uninstalling

**Order matters, because the operator is the only thing that can release the cloud resources.** Every
`Bucket` carries a finalizer, and teardown runs in the operator
([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D1). Removing the release first
leaves CRs that nothing can finish deleting until an operator runs again.

```bash
NS=stackit-s3-provisioner-system      # example

# 1. While the operator is still running: delete every Bucket CR, cluster-wide.
kubectl get bkt -A

# 2. Delete them namespace by namespace, and wait. A bucket that still holds objects is
#    NOT deleted - the CR keeps its finalizer and stays in Terminating on purpose.
kubectl -n <namespace> delete bkt <name>

# 3. Confirm nothing is left before touching the release.
kubectl get bkt -A                # must be empty

# 4. Now remove the release.
helm uninstall stackit-s3-provisioner -n $NS
```

If step 2 hangs, read [deletion.md](deletion.md) — the empty-bucket guard is doing its job, and the
way past it is to empty the bucket or to authorize a wipe, never to strip the finalizer.

### What is left behind

| What | Where | Why it stays |
| --- | --- | --- |
| The `Bucket` CRD | cluster-scoped | `helm.sh/resource-policy: keep`, so an uninstall cannot orphan `Bucket` CRs by removing the type from under them. Remove with `kubectl delete crd buckets.stackit-bucket.gtrfc.com` once no CRs exist. |
| The admin credentials Secret | release namespace | It has no owner reference and is not a chart object ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D3). Deleting the namespace removes it. |
| The `operator-admin` credentials group and its access key | the StackIT project | Nothing in the product deletes them; they are project-wide, not release-scoped ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D1). Remove them in StackIT by hand if the project is being retired. |
| Any bucket whose CR was never deleted | the StackIT project | Together with its credentials group, its access key and its policy. |
| The key Secret | release namespace | Created outside the release. |

Deleting the admin Secret while leaving the group in place is safe in one specific way and not in
another: the next operator that starts re-bootstraps by clearing the group's keys and minting a fresh
one ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D5), so a
reinstall recovers — but any other holder of that old key loses it. See
[credentials.md](credentials.md).

**Reinstalling into the same project** works and is the normal disaster-recovery path, provided
`ownership.name` is set to the value the previous release used. It is part of the ownership key of
every provisioned bucket: with a different value the operator treats its own buckets as foreign and
parks their CRs on a collision ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md)
D2). The full set of values a restore must reproduce is in [bucket-naming.md](bucket-naming.md).

---

## User-facing RBAC comes with the install

`bucketRoles.create` (`true` # default) renders `<release>-view` and `<release>-edit`. Both are
**aggregation fragments**: without them a `Bucket` is invisible to every subject that is not a
cluster admin, because the built-in `view`/`edit`/`admin` roles do not cover custom resources.
Aggregation is asynchronous — the controller-manager merges the rules into the built-ins a moment
after the install, so a `kubectl auth can-i` run immediately afterwards can still say no.

`<release>-edit` is aggregated into `edit` and `admin` and is not meant for direct binding: a
`Bucket` uses Secrets of its own namespace on behalf of whoever created it, and the operator cannot
check that creator's rights. What that hands out, and why the chart offers no switch to render it
standalone, is [rbac-and-privilege.md](../security/rbac-and-privilege.md) and the amended user-facing
RBAC consequence of D1–D5 in
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md). Set `bucketRoles.create: false`
when user RBAC is managed out of band; the operator's own ClusterRole is unaffected either way
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D6 — the confinement rules bound
the Bucket, not the operator).

The key is `bucketRoles.create` and deliberately not the `rbac.create` that most operator charts use
for their RBAC block — in those charts, the CloudNativePG operator chart among them,
`rbac.create: false` switches off **all** of the release's RBAC, the operator's own `ClusterRole`
and `ClusterRoleBinding` included (an upstream convention, not re-checked by anything in this
repository). Here those two are rendered unconditionally and the switch removes nothing but the two
aggregation fragments; the distinct name is what keeps muscle memory from another chart from
disabling the operator's own permissions.
[`test/helm/render_test.go`](../../test/helm/render_test.go) pins that in both settings — with the
fragments rendered and with `bucketRoles.create: false`, the operator `ClusterRole` is still there,
unaggregated, with its full `Bucket` verbs.
