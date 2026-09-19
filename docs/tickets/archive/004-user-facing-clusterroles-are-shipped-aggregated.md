# Ticket: stackit-s3-provisioner — ship user-facing ClusterRoles and aggregate them

**Target project:** this repo (chart `deploy/helm/stackit-s3-provisioner`,
CRD group `stackit-bucket.gtrfc.com/v1`, kind `Bucket`, namespaced).
**Reported from:** k8s-flux-base, 2026-09-03. Chart on mgmt-p: 1.13.1; repo tip: 1.14.0.
**Reviewed against the tree:** 2026-09-03 (see "Review notes" at the end).
**Status:** implemented 2026-09-03 (`feat(helm): ship aggregated user-facing ClusterRoles for Bucket`).

## Problem

The chart ships exactly one ClusterRole — `templates/clusterrole.yaml`, the
operator's own permissions, bound to the operator ServiceAccount. There are no
user-facing roles and no aggregation labels anywhere:

```console
$ grep -rn aggregat deploy/helm/stackit-s3-provisioner/ config/
(no matches)
```

Consequence: `Bucket` CRs are invisible to every non-cluster-admin. Kubernetes
`view` / `edit` / `admin` do not cover custom resources unless a ClusterRole
carries the matching `rbac.authorization.k8s.io/aggregate-to-*` label, so a
cluster whose RBAC is built on those built-ins sees nothing.

Observed on mgmt-p (Sutor platform cluster): a platform lead authenticated via
Pinniped holds `sutor:platform-reader`, a purely aggregated ClusterRole
(`aggregate-to-view` plus an own cluster-scoped helper label). `kubectl get
buckets` returns a forbidden error although the same user reads every other
namespaced resource cluster-wide. Same gap for the namespace-scoped reader
roles and for the built-in `admin` that tenant teams hold on the qs stage.
Only cluster-admin and the operator itself can see a Bucket today — which also
means the CR status (phase, `bucketURL`, `usage`, degraded conditions) is
unreadable for exactly the people who operate the workloads that own the
bucket.

## Desired behavior

Follow the CloudNativePG operator chart, which solves this with two extra
ClusterRoles
([charts/cloudnative-pg/templates/rbac.yaml](https://github.com/cloudnative-pg/charts/blob/main/charts/cloudnative-pg/templates/rbac.yaml),
verified 2026-09-03: `-view` carries all three `aggregate-to-*` labels,
`-edit` carries `aggregate-to-edit` and `aggregate-to-admin`, both behind one
`rbac.aggregateClusterRoles` switch). One deviation: this chart has **no**
aggregation switch. Both roles are always aggregated when rendered — see
"Why `-edit` is aggregation-only" below.

1. **New template `templates/clusterrole-view.yaml`** — name
   `{{ include "stackit-s3-provisioner.fullname" . }}-view`, common labels
   plus all three aggregation labels, unconditionally:

   ```yaml
   labels:
     rbac.authorization.k8s.io/aggregate-to-view: "true"
     rbac.authorization.k8s.io/aggregate-to-edit: "true"
     rbac.authorization.k8s.io/aggregate-to-admin: "true"
   rules:
     - apiGroups: ["stackit-bucket.gtrfc.com"]
       resources: ["buckets"]
       verbs: ["get", "list", "watch"]
     - apiGroups: ["stackit-bucket.gtrfc.com"]
       resources: ["buckets/status"]
       verbs: ["get"]
   ```

   All three labels rather than `aggregate-to-view` alone: on a stock cluster
   the built-in `view` carries `aggregate-to-edit` and `edit` carries
   `aggregate-to-admin`, so one label reaches all three transitively — but a
   cluster that customised its built-ins loses that chain. Three labels cost
   nothing and are what CNPG and kubebuilder do.

   `get` is the only verb the status subresource serves; `list`/`watch` on it
   are meaningless (kubebuilder's viewer role scaffolds it the same way).

2. **New template `templates/clusterrole-edit.yaml`** — name `…-edit`, labels
   `aggregate-to-edit: "true"` and `aggregate-to-admin: "true"`,
   unconditionally. Rules mirror CNPG, write verbs only:

   ```yaml
   rules:
     - apiGroups: ["stackit-bucket.gtrfc.com"]
       resources: ["buckets"]
       verbs: ["create", "delete", "deletecollection", "patch", "update"]
   ```

   No read verbs: the role is an aggregation fragment, and the built-in `edit`
   already contains `view` (which now contains `-view`). No write on
   `buckets/status`: status is the operator's. Leaving the read verbs out is
   deliberate — a role that cannot even `kubectl get` is visibly not meant for
   direct binding (see below).

3. **New values block.** Not under `rbac:` — in CNPG and most charts
   `rbac.create: false` switches off *all* RBAC including the operator's own
   role, and here the operator ClusterRole stays unconditional. A distinct
   key avoids the muscle-memory trap:

   ```yaml
   # bucketRoles ships the user-facing ClusterRoles for Bucket CRs. The
   # operator's own ClusterRole (clusterrole.yaml) is unaffected by this block.
   bucketRoles:
     # create renders the <release>-view and <release>-edit ClusterRoles. Both
     # are aggregation fragments: -view is picked up by the built-in view, edit
     # and admin ClusterRoles, -edit by edit and admin. Read on a Bucket exposes
     # ids, URLs, sizes and conditions, never credential material. Write on a
     # Bucket is the same class of power the built-in edit already holds over
     # PVCs and Secrets in the namespace (ADR 0001, ADR 0002).
     #
     # There is no switch to render -edit without aggregation, on purpose: a
     # Bucket uses Secrets of its namespace on behalf of whoever created it
     # (spec.cloneFrom.secretRef, spec.secretRef adoption), so Bucket write
     # access must only ever be held together with Secret access — which is
     # exactly what edit and admin guarantee. Do not bind -edit directly to a
     # subject that lacks Secret read/write in that namespace. Set create to
     # false when user RBAC is managed out-of-band.
     create: true
   ```

   History: the first draft (2026-09-03, morning) had separate
   `aggregateView` / `aggregateEdit` switches, with `aggregateEdit` defaulting
   to false because security finding 1 was open. ADR 0002 closed it the same
   day and the default flipped to true. The afternoon review then found the
   confused-deputy path below, and the standalone `-edit` use case the switch
   existed for was dropped altogether (option E of that review).
   `aggregateView` went with it: a non-aggregated `-view` has no consumer — a
   cluster wanting read-only Bucket access for a subject binds the built-in
   `view`.

4. **No mirror into `config/rbac/`.** `make manifests` only generates
   `role.yaml` (controller-gen `rbac:roleName=…`, [Makefile](../../../Makefile#L317));
   viewer/editor roles there would be hand-maintained on an untested
   deployment path (e2e installs via Helm). Leave it out.

## Security notes

**Read (`-view`) is safe to aggregate.** Verified against
[api/v1/bucket_types.go](../../../api/v1/bucket_types.go), `BucketStatus`:

- `spec.secretRef` and `spec.cloneFrom.secretRef` hold Secret *names* only
  (`SecretReference`, `CloneSourceSecretRef`); since ADR 0001 there is no
  `namespace` field, the credentials Secret always lives in the Bucket's
  namespace.
- `status.accessKeyID` is the S3 access key id. The secret half is only ever
  written to the referenced Secret, never to status.
- `status.credentialsGroupID` / `credentialsGroupURN` / `bucketURL` /
  `resolvedBucketName` / `usage` / `clone` (phase, bytes, progress) /
  `grantedReadTo` / `degradedSince` / conditions / rotation timestamps are
  metadata.

A `view` holder learns which buckets exist, how big they are and whether they
are healthy. That is the intended blast radius of `view`.

**What `-edit` hands out.** All of it is inside the Bucket's namespace
([ADR 0001](../../../docs/adr/0001-a-bucket-only-affects-its-own-namespace.md) D1–D5)
and comparable to what the built-in `edit` already does with PVCs and
Secrets. It belongs in the values comment and the README so a cluster
operator knows what aggregating `-edit` means:

- Creating a Bucket provisions a real StackIT bucket, a credentials group and
  an access key in the operator's project, and writes the credentials Secret.
- `upsertSecret` ([bucket_controller.go:1489](../../../internal/controller/bucket_controller.go#L1489-L1507))
  is a CreateOrUpdate with `SetControllerReference`: an existing Secret under
  `spec.secretRef.name` that has **no** controller owner is adopted, its
  colliding keys are overwritten, and `deleteSecret`
  ([L1535](../../../internal/controller/bucket_controller.go#L1535-L1543)) removes it
  when the CR is deleted. A Secret already controlled by something else is
  refused (`AlreadyOwnedError`).
- `spec.cloneFrom.secretRef` ([clone.go:137](../../../internal/controller/clone.go#L137-L153))
  makes the operator **use** any Secret in the namespace that holds S3
  credentials — as the source for a full copy into the new bucket, against
  an endpoint the CR chooses. The operator-written Secret of a sibling Bucket
  matches the default key names exactly (documented, e2e-tested in
  `TestCloudClone`).
- `patch` on a Bucket reaches the `stackit-bucket.gtrfc.com/rotate-credentials-at`
  annotation: the live key dies immediately, the workload must re-read the
  Secret.
- `spec.grantReadAccess` grants read on this bucket to sibling Buckets in the
  same namespace.
- `spec.wipeOnDelete: true` plus delete wipes the namespace's own bucket when
  the cluster runs with `wipeOnDelete.enabled` (default off). Same class as
  deleting a PVC.

Cloud cost is not an argument either: every namespace admin can already spend
money with a PVC.

**Why `-edit` is aggregation-only (decided 2026-09-03, afternoon).** The two
Secret items above make a Bucket a confused deputy: the operator acts on the
CR with its own permissions and never checks whether the CR's creator could
read or write the referenced Secret — it cannot, the CR carries no requester
identity and there is no admission webhook. A subject holding Bucket write
**without** Secret read in the same namespace could therefore (a) copy the
full contents of any bucket whose credentials sit in a Secret of that
namespace into a bucket of its own, and (b) overwrite or delete any unowned
Secret there. Kubernetes has the same class of problem with Pods (create a
Pod, mount any Secret) and answers it the same way: the built-in `edit` and
`admin` hand out Secret access together with workload access, so the
combination is never split. Aggregating `-edit` into exactly those roles
preserves that invariant; a standalone `-edit` binding would break it. The
alternatives considered — consent annotation on the source Secret, rejecting
operator-managed Secrets as clone source (breaks the documented sibling-clone
case), an admission webhook with SubjectAccessReview — all cost more than the
use case is worth, since no subject in the fleet needs Bucket write without
Secret access. Should that change, the consent annotation is the path
(own ticket, ADR amendment).

**Closed on 2026-09-03: the reason an earlier draft kept `-edit` unaggregated.**
Until that day ADR 0001 D2 was violated for credentials groups (security
finding 1 in CLAUDE.md): `workloadGroupName` — `s3op-<namespace>-<name>`
truncated to 23 characters plus an 8-hex FNV-1a-32 — was the handle by which
`EnsureCredentialsGroup` found or created a Bucket's group, with no ownership
check, and a namespace admin could brute-force a colliding name for any
prefix-related namespace, adopt the victim's group, kill its key and mint a
new one into their own Secret. Aggregating `-edit` would have handed that to
every namespace admin.
[ADR 0002](../../../docs/adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)
closed it: a bucket names its group in the admin-key-only tag
`credentials-group-id`, legacy buckets are migrated from their own policy, and
a group is never found, adopted or deleted by name. Regression test
`TestGroupAttributionSurvivesNameCollision`; migration verified against the
real API. With that, aggregating `-edit` hands out namespace-scoped power only.

## Acceptance criteria

- `helm template` with defaults renders `…-view` carrying all three
  `aggregate-to-*` labels and `…-edit` carrying `aggregate-to-edit: "true"`
  and `aggregate-to-admin: "true"`; `…-edit` has no read verbs and nothing on
  `buckets/status`.
- `helm template --set bucketRoles.create=false` renders neither role; the
  operator ClusterRole is unaffected in both cases.
- **Chart render test in CI.** Nothing in the pipeline runs `helm lint` or
  asserts on rendered templates today (`grep -rn "helm lint\|helm template"
  .github/` is empty; the only render is `E2E_CLONE_IMAGE` in the Makefile).
  Add a small check that runs before the Kind job in
  [release.yml](../../../.github/workflows/release.yml) — a Go test under `test/helm/`
  shelling out to `helm template`, or a Make target with `yq` assertions —
  covering the two bullets above. Without it the `create=false` path is never
  exercised.
- **e2e test** in [test/e2e/e2e_test.go](../../../test/e2e/e2e_test.go) (runs in
  `make e2e-local` and in CI via `make test-e2e`,
  [release.yml](../../../.github/workflows/release.yml#L262-L285); CI installs the
  chart with `test/e2e/helm-values.yaml`, which does not touch `bucketRoles`,
  so the defaults apply): create a test namespace and a RoleBinding of a
  synthetic user to the built-in `view`, then assert via
  `SubjectAccessReview` (the test already holds a `kubernetes.Interface`,
  [L42](../../../test/e2e/e2e_test.go#L42)) that `list buckets` is allowed and `create
  buckets` is denied; bind the same user to the built-in `edit` and assert
  `create buckets` is allowed and `update buckets/status` is denied.
  **Aggregation is asynchronous**: the controller-manager merges the labelled
  rules into `view`/`edit` some time after the chart install. Poll the
  allow-assertions until they pass (bounded by `testTimeout`), and run every
  deny-assertion only after the corresponding allow has passed — before
  aggregation lands, everything is denied and a deny-check passes vacuously.
  This replaces a manual `kubectl auth can-i`, which without the RoleBinding
  step is always `no`.
- Docs: a new README.md section next to "Install (Helm)" documenting
  `bucketRoles.create`, what aggregated `-edit` hands out (list above), the
  aggregation-only rule and its reason, and the ADR 0001 / 0002 links; the
  values.yaml comment as drafted above. The chart has no README of its own.
- **ADR 0001 amendment**, in the same change, text agreed with Hans before
  implementation (CLAUDE.md, ADR procedure 2): the consequence "User-facing
  write RBAC on `buckets` hands out precisely the power this ADR bounds" gets
  a second amendment — a Bucket uses Secrets of its namespace on behalf of
  its creator (`cloneFrom.secretRef`, `secretRef` adoption) without checking
  the creator's rights, so Bucket write access is shipped aggregated into the
  built-in `edit`/`admin` only and never as a role meant for direct binding.
  Plus a `Status` line that the user-facing roles shipped, with the commit.
- Versioning: **no manual Chart.yaml bump.** [build.yml](../../../.github/workflows/build.yml#L291-L299)
  writes `version`, `appVersion` and `image.tag` from the release tag.
  `.releaserc.json` uses the conventionalcommits preset, so commit as
  `feat(helm): ...` and semantic-release cuts the minor.

## Consumer follow-up (k8s-flux-base)

`components/storage-provisioner/stackit-s3-provisioner/app/clusterrole-view.yml`
there defines an interim ClusterRole `stackit-s3-provisioner-view` with the
`aggregate-to-view` label. With `releaseName: stackit-s3-provisioner` the
chart's fullname is `stackit-s3-provisioner`, so the chart's view role gets
**the same name**. Helm does not adopt an existing object that lacks its
ownership metadata; the upgrade fails with "exists and cannot be imported into
the current release" and Flux rolls back. So, wherever the interim role is
deployed: either remove that file (Flux prunes it) **before** bumping the
chart, or — order-independent and therefore preferred — give the interim
object `app.kubernetes.io/managed-by: Helm` plus the
`meta.helm.sh/release-name` / `release-namespace` annotations so Helm adopts
it. Nothing else to set; `bucketRoles.create` is on by default.

## Review notes (2026-09-03)

Verified in this repo: chart layout and helper names, absence of any user
role or aggregation label, `config/rbac/` contents and what `make manifests`
generates, the status fields and reference types listed above, the Secret
adoption in `upsertSecret` and the `AlreadyOwnedError` refusal, the clone
source Secret read in `cloneSourceCreds`, the sibling-clone e2e case, the
absence of any webhook, the absence of any Helm render check in CI, the
release pipeline's chart-version handling and the semantic-release preset.
Verified against upstream: the CNPG `-view`/`-edit` roles and their switch.
Updated the same day after ADR 0002 closed finding 1 (`aggregateEdit` default
true), then again in the afternoon after the confused-deputy review: the
aggregation switches are gone, `-edit` is aggregation-only (option E),
`-view` carries all three labels, the e2e polling requirement and the render
check were added, line references refreshed. Not verified: whether the
interim role is deployed on any given cluster, and the Flux ordering between
prune and HelmRelease upgrade when both land in one sync.
