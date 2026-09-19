# RBAC and privilege

This page is about delegation: what the operator itself is allowed to do in the cluster,
what the two user-facing ClusterRoles hand to a tenant, and what a `Bucket` write turns
into once it reaches the cloud. If the question is instead *what keeps one tenant's data
away from another's*, that is
[tenancy-and-isolation.md](tenancy-and-isolation.md); if it is *where a given credential
lives and who can read it*, that is
[credentials-and-secrets.md](credentials-and-secrets.md); if it is *how the operator
decides a cloud object is its own*, that is
[ownership-and-attribution.md](ownership-and-attribution.md). Reporting a vulnerability
is [SECURITY.md](../../SECURITY.md).

## The operator's own privilege footprint

The chart binds one ClusterRole to the operator's ServiceAccount
([`clusterrole.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/clusterrole.yaml),
[`clusterrolebinding.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/clusterrolebinding.yaml)).
It is cluster-scoped in full — there is no namespace allowlist and no per-namespace
RoleBinding variant.

| API group | Resource | Verbs | Why the operator needs it |
| --- | --- | --- | --- |
| `stackit-bucket.gtrfc.com` | `buckets` | get, list, watch, create, update, patch, delete | Reconcile the CRs; `update` also carries the finalizer and the frozen-name annotation |
| `stackit-bucket.gtrfc.com` | `buckets/status` | get, update, patch | Everything the operator observes is reported there and nowhere else ([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D4) |
| `stackit-bucket.gtrfc.com` | `buckets/finalizers` | update | Set and drop the teardown finalizer |
| `""` | `secrets` | get, list, watch, create, update, patch, delete | Write a workload credential into **every** tenant namespace, and hold its own admin Secret |
| `batch` | `jobs` | get, list, watch, create, delete | Run and clean up clone Jobs ([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D2) |
| `""` | `pods` | get, list, watch | Find a clone Job's pod to poll its progress endpoint |
| `""`, `events.k8s.io` | `events` | create, patch | Surface provisioning outcomes on the CR |
| `coordination.k8s.io` | `leases` | get, list, watch, create, update, patch, delete | Leader election, rendered only when `leaderElection.enabled` is `true` (# default) |

The generated `config/rbac/role.yaml` carries the same rules minus the leases, because
the kubebuilder markers on the reconciler
([`bucket_controller.go`](../../internal/controller/bucket_controller.go), the
`+kubebuilder:rbac` block) do not cover leader election. That path is not how this
operator is deployed — installation is by Helm — so the difference is harmless today and
is named here rather than quietly reconciled.

**Cluster-wide Secret access is deliberate and is the largest single item.** The operator
must write a credentials Secret into whichever namespace a `Bucket` appears in, so the
grant cannot be narrowed to a namespace list without breaking the product. The namespace
is a trust boundary for tenants, not for the operator's own credential — that is
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D6, stated as a rule
precisely because D1 through D5 confine the *Bucket* and not the operator.

**`jobs` and `pods` are granted cluster-wide, and the calls — but not the watches — stay
in the operator's own namespace.** Clone Jobs are created with
`Namespace: r.AdminSecretNamespace`
([`clone.go`](../../internal/controller/clone.go), `buildCloneJob`), which is the
operator namespace from `--operator-namespace`, defaulting to `POD_NAMESPACE`
([`cmd/main.go`](../../cmd/main.go))
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D2), and
the pod lookup that polls a clone's progress lists with
`client.InNamespace(r.AdminSecretNamespace)` (`pollCloneStats` in the same file). Nothing
in the code creates a Job or reads a pod in another namespace. The reads nevertheless go
through the manager's cached client, and `client.InNamespace` filters a cached *result*
rather than scoping the informer behind it; the Job watch registered in
`SetupWithManager` is unrestricted as well. The same missing cache restriction that *H-17*
describes for Secrets therefore also puts every Pod and every Job in the cluster into the
operator's informer cache — a far smaller exposure, since a pod or job spec carries no
credential material of its own, but the grant is not as narrow as the call sites suggest.
A cluster that wants the tighter shape cannot simply replace the batch and pods rules
with a namespace-scoped Role: the write calls would not notice, but the cluster-wide
informers would lose their list and watch permission and the clone path with them. The
narrowing has to happen in the manager's cache options first, and those are not
configurable today.

**What the Secret watch means in practice.** The reconciler watches Secrets cluster-wide
(`SetupWithManager` in
[`bucket_controller.go`](../../internal/controller/bucket_controller.go)) and filters
them with `isManagedSecret`, which matches the
`app.kubernetes.io/managed-by: stackit-s3-provisioner` label. That predicate filters
*events*, not the watch: the manager is built with no cache restriction
([`cmd/main.go`](../../cmd/main.go), `ctrl.NewManager` sets no `Cache` options), so every
Secret in the cluster is streamed into the operator's informer cache and held in its
memory. This is not an authorisation gap — the ClusterRole already permits the read — but
it widens what a memory disclosure or a core dump of the operator pod would expose. See
*H-17* below.

**What the operator does not hold.** No verbs on `rbac.authorization.k8s.io`, so it
cannot grant, escalate or bind anything; no `namespaces`, `serviceaccounts`, `nodes`,
`customresourcedefinitions` or webhook configurations; no `update`/`patch` on `jobs`,
only create and delete; no read on `buckets` outside its own API group. The CRD itself is
installed by Helm, under the installer's identity, not the operator's.

**If the operator pod is compromised**, the attacker holds: every Secret in the cluster
(read and write), the StackIT service-account key mounted into the pod, and through it
the project-wide S3 admin credential of
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D1 — which
is exempt from the deny statement of every bucket policy the operator writes
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D7), i.e.
full read and write on every bucket in the project. What that credential is and where it
lives is [credentials-and-secrets.md](credentials-and-secrets.md); what it does *not*
reach is the neighbouring StackIT project, because the service account is granted roles
only at project level
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D5 — a rule
about how the account is provisioned, which the operator neither detects nor enforces).
Without a service-account key the operator runs in skeleton mode and makes no cloud call
in either direction
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D6), which
bounds such a compromise to the cluster.

The pod runs `runAsNonRoot` with `seccompProfile: RuntimeDefault`, and the container with
`allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true` and all capabilities
dropped
([`deployment.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml)).

## The two user-facing ClusterRoles

`Bucket` is a custom resource, so Kubernetes' built-in `view`, `edit` and `admin` do not
cover it on their own. The chart closes that with two ClusterRoles, rendered when
`bucketRoles.create` is `true` (# default) and not rendered at all when it is `false`.
Both are **aggregation fragments**: they carry
`rbac.authorization.k8s.io/aggregate-to-*` labels and are meant to be absorbed by the
built-ins, not bound to a subject.

| ClusterRole | Aggregation labels | Rules |
| --- | --- | --- |
| `<release>-view` | `aggregate-to-view`, `aggregate-to-edit`, `aggregate-to-admin` | `get`, `list`, `watch` on `buckets`; `get` on `buckets/status` |
| `<release>-edit` | `aggregate-to-edit`, `aggregate-to-admin` | `create`, `delete`, `deletecollection`, `patch`, `update` on `buckets` |

The shape is borrowed rather than invented. The CloudNativePG operator chart ships the
same pair — a `-view` ClusterRole carrying all three `aggregate-to-*` labels and a `-edit`
carrying `aggregate-to-edit` and `aggregate-to-admin` — as verified against its
`charts/cloudnative-pg/templates/rbac.yaml` upstream on 2026-09-03; that is an upstream
observation of that date and nothing in this repository re-checks it. The one deviation is
deliberate: CNPG puts both roles behind a single `rbac.aggregateClusterRoles` switch, and
this chart has no such switch, because neither half of it would have a consumer here. A
standalone, unaggregated `-edit` is precisely the confused deputy the rule further down
forbids, and an unaggregated `-view` has nobody to serve — a cluster that wants read-only
`Bucket` access for one subject binds the built-in `view`. `bucketRoles.create` therefore
renders both roles aggregated or renders neither.

The verb list on `buckets/status` is borrowed from the second precedent: kubebuilder's
scaffolded viewer role grants `get` and only `get` on a status subresource, because `get`
is the only verb such a subresource serves and `list`/`watch` on it are meaningless. That
too was read upstream on 2026-09-03 and is not re-checked by anything here.

Three labels on `-view` rather than relying on the chain: on a stock cluster the built-in
`view` carries `aggregate-to-edit` and `edit` carries `aggregate-to-admin`, so one label
would reach all three transitively. That is an upstream Kubernetes property rather than
something this repository proves — no test here inspects the built-in roles, which is the
general assumption *H-18* raises — and a cluster that customised its built-ins loses the
chain. The extra labels cost nothing and do not depend on it.

`-edit` has **no read verbs and nothing on `buckets/status`**, both on purpose. `edit`
already contains `view`, which now contains `-view`, so read is covered by aggregation; a
fragment that cannot even `kubectl get` is visibly not meant for direct binding. The
status subresource stays the operator's: everything it observes is reported there
([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D4) — a tenant who
could write `status.credentialsGroupURN` would be writing into the resolution path of
another bucket's read grant, which is why
[ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3
resolves a reader through the grantee's own bucket and never from anything a grantee can
write itself.

The exact shape of both roles is pinned by
[`test/helm/render_test.go`](../../test/helm/render_test.go), which runs `helm template`
and asserts the labels and the verb sets, including that `-edit` never carries
`aggregate-to-view` and that the operator's own ClusterRole is untouched by the
`bucketRoles` block in both settings. Read that file rather than trusting the table above
if the two ever diverge.

**Aggregation is asynchronous.** The controller-manager merges the labelled rules into
`view`/`edit`/`admin` some time after the chart install, so a permission check run
immediately after installing will fail and then start succeeding on its own.
[`test/e2e/rbac_test.go`](../../test/e2e/rbac_test.go) handles this by polling every
allow-assertion and running each deny-assertion only after the corresponding allow has
passed — before aggregation lands everything is denied and a deny-check would pass
vacuously.

## What Bucket read exposes

`get`/`list`/`watch` on `buckets` and `get` on `buckets/status` expose the spec and the
status of the CR. No credential material is among them: the secret half of a workload
access key never leaves that Secret — not into the status, an event, a log line or a
metric
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
D12). Reading the Secret needs Secret read, which `view` does not carry.

<details>
<summary>Status fields a reader sees, and what each one discloses</summary>

| Field | What a reader learns |
| --- | --- |
| `resolvedBucketName`, `bucketURL` | The physical bucket name and its path-style URL — the address, not access to it |
| `accessKeyID` | The S3 access key **id** only; the secret half is never written to the status ([`api/v1/bucket_types.go`](../../api/v1/bucket_types.go), `BucketStatus.AccessKeyID`) |
| `credentialsGroupID`, `credentialsGroupURN` | The workload credentials group behind the bucket, as identifiers; the URN is the principal the bucket policy names |
| `grantedReadTo` | Which sibling Buckets currently hold a read grant, without reading the policy from S3 |
| `usage` | Size, object count and the derived monthly cost estimate, when measurement is on |
| `clone` | Clone phase, total bytes and a human progress string, including the source that was named |
| `phase`, `message`, `conditions`, `degradedSince` | Health: whether the provider was reachable at the last verified state ([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)), and whether the bucket itself is still there — `BucketPresent=False` says this `Bucket`'s data is gone ([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D5) |
| `lastRotationTrigger`, `lastRotationTime`, `observedGeneration`, `operatorVersion` | Reconcile bookkeeping |

Two spec fields disclose something beyond the bucket's own shape, and both disclose the
same kind of thing: `secretRef.name` and `cloneFrom.secretRef.name` are Secret **names**,
always resolved in the `Bucket`'s own namespace and never carrying one
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D4). A reader learns
which Secrets are in play, not their contents. The remaining spec fields are the
declaration itself and are documented in [README.md](../../README.md).

</details>

A `view` holder therefore learns which buckets exist in the namespaces they can see, how
big they are, what they cost and whether they are healthy. That is the intended blast
radius of `view`, and it is why `-view` is aggregated unconditionally.

## What Bucket write hands out

Everything below stays inside the `Bucket`'s namespace
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1–D5) and is, as a
class, what the built-in `edit` already holds over PVCs and Secrets there. Cloud spend is
not a distinguishing argument either: a namespace admin can already spend money with a
PVC.

| The write | What it causes |
| --- | --- |
| Create a `Bucket` | A real bucket, a credentials group and an access key are provisioned in the operator's StackIT project, and a credentials Secret is written in the CR's namespace |
| `spec.secretRef.name` | An existing Secret of that name with **no** controller owner is adopted and its colliding data keys overwritten; the Secret of that name is deleted when the CR is deleted, adopted or not |
| `spec.cloneFrom.secretRef` | The operator reads that Secret and uses it as S3 credentials against an endpoint the CR chooses |
| `patch` the rotation annotation | `stackit-bucket.gtrfc.com/rotate-credentials-at` with a new value kills the live access key immediately; the workload must re-read the Secret ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D8, D9) |
| `spec.grantReadAccess` | Sibling Buckets of the same namespace get read-only access to this bucket's objects ([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D2) |
| `spec.allowRecreate` | A bucket that vanished is rebuilt unattended under the same frozen name, with a fresh credentials group and access key, and the workload Secret is overwritten — a standing waiver of the refusal to rebuild, not of the report and not a wider reach: it is a spec write on a `Bucket` in its own namespace, the same access that can already delete the CR outright, and every rebuild is still reported ([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D9, D10) |
| `spec.wipeOnDelete` plus a delete | All objects, including versions and delete markers, are destroyed before the bucket is removed — **only** while the operator runs with `wipeOnDelete.enabled` (# default `false`) and the ownership tags prove the bucket is this operator's ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D4–D6) |
| `delete` a `Bucket` | Teardown; a non-empty bucket blocks it rather than losing data ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D2) |

Two of those rows deserve the mechanism spelled out, because the confused-deputy rule
below rests on them.

**Secret adoption.** `upsertSecret`
([`bucket_controller.go`](../../internal/controller/bucket_controller.go)) is a
`CreateOrUpdate` whose mutate function stamps the managed-by label, forces the type to
`Opaque`, merges the credential data keys and calls `SetControllerReference`. An existing
Secret with no controller owner is therefore taken over; one already controlled by
another object is refused with `AlreadyOwnedError` and the reconcile fails.

**Secret deletion is by name, and this is broader than the adoption rule suggests.**
`deleteSecret` deletes `spec.secretRef.name` in the CR's namespace unconditionally — it
checks neither the owner reference nor the managed-by label. The finalizer is added
before any provisioning work runs (`Reconcile` in
[`bucket_controller.go`](../../internal/controller/bucket_controller.go)), so a `Bucket`
whose `secretRef` names a Secret owned by some other controller still reaches teardown,
and teardown still deletes that Secret. The only exception is the operator's own admin
Secret, which `teardown` refuses explicitly as defence in depth
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D8).
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
D11 states this exactly — teardown removes the Secret the `Bucket` currently references —
and the wording that has circulated elsewhere, tying the deletion to adoption, is
narrower than both the rule and the code. The consequence is bounded by
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1 — it is a
namespace-local Secret delete, which the built-in `edit` holds anyway — but it is a
primitive a subject holding only Bucket write would not otherwise have.

## Why `-edit` is never bound directly

**The rule: do not bind `<release>-edit` to a subject that lacks Secret read and write in
the same namespace.** The chart offers no switch to render it unaggregated, and that
absence is the enforcement.

The reason is a confused deputy. The operator acts on a `Bucket` with its own
permissions and never checks whether the CR's creator could read or write the Secrets the
CR names — it cannot: the CR carries no requester identity, and reconciles are
level-triggered long after creation. A subject holding `Bucket` write **without** Secret
access in that namespace could therefore, through the two mechanisms above, copy the
contents of any bucket whose credentials sit in a Secret of that namespace into a bucket
of its own, and overwrite or delete an unowned Secret there. Kubernetes has the same
class of problem with Pods — create a Pod, mount any Secret — and answers it the same
way: the built-in `edit` and `admin` hand out Secret access together with workload
access, so the combination is never split. Aggregating `-edit` into exactly those roles
preserves that invariant; a standalone binding would break it.

The reasoning, the alternatives that lost — a consent annotation on the referenced
Secret, refusing operator-managed Secrets as a clone source, an admission webhook with a
`SubjectAccessReview` — and the condition under which this would be revisited are the
2026-09-03 amendment of
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md), under *Consequences*
and *Alternatives Considered*. The short version: no subject in the fleet needs `Bucket`
write without Secret access, so the cheapest mechanism that would allow it has not been
built; the day one does, the consent annotation is the path and it needs its own record.

There is a second reason the write fragment is shipped the way it is, and it is history
that still explains the shape. Until 2026-09-03 a `Bucket`'s credentials group was found
or created by a derived display name with no ownership check, and two different
namespace/name pairs could derive the same name, so a namespace admin could adopt a
foreign `Bucket`'s group, destroy its live key and mint a replacement into their own
Secret. While that held, aggregating write access into `edit` would have handed a
credential takeover to every namespace admin on the cluster, and an earlier draft of these
roles kept `-edit` unaggregated for exactly that reason.
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) closed it
by attributing the group through the bucket's own tags, which is why the roles ship
aggregated today — see
[ownership-and-attribution.md](ownership-and-attribution.md) for how that attribution
works now.

## The clone Job's own footprint

A clone runs as a Job in the **operator's** namespace, never in a workload namespace
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D2), and
its pod is what actually moves the data. Its privilege shape is therefore part of this
page rather than of the tenancy one.

The pod template (`buildCloneJob` in [`clone.go`](../../internal/controller/clone.go))
sets `runAsNonRoot` with uid/gid 65534, `seccompProfile: RuntimeDefault`,
`allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true` and drops all
capabilities. Both credentials reach it as environment variables from Secrets in the
operator namespace: the source half from the staging Secret the operator writes, the
destination half from the admin Secret
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D3).
The command and image are fixed by the operator; the CR contributes the source endpoint,
the source bucket name, a region and an addressing style, all as separate argument or
environment values rather than as a shell string.

Its progress endpoint is closed twice over: a generated 32-character password in the
staging Secret, and a NetworkPolicy
([`networkpolicy.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/networkpolicy.yaml),
`clone.networkPolicy.enabled`, # default `true`) that admits ingress to port 5572 only
from the operator pod and, by selecting the clone pods with `policyTypes: Ingress`,
denies every other ingress to them
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D9). It
needs a NetworkPolicy-capable CNI to do anything; on a cluster without one the password
is the only remaining control.

There is no egress policy, and the pod runs under the operator namespace's `default`
ServiceAccount — the template sets neither `serviceAccountName` nor
`automountServiceAccountToken`. Both are gaps rather than decisions; see *H-15* and *H-16*.

## Why the roles exist at all

Before 2026-09-03 the chart shipped exactly one ClusterRole, the operator's own, and no
aggregation label anywhere. `Bucket` CRs were consequently invisible to everyone but a
cluster admin and the operator: on the mgmt-p Sutor platform cluster on 2026-09-03 a
platform lead authenticated through Pinniped, holding `sutor:platform-reader` — a purely
aggregated ClusterRole carrying `rbac.authorization.k8s.io/aggregate-to-view` and a
cluster-scoped helper label of its own — could list every other namespaced resource
cluster-wide and still got a forbidden error for `kubectl get buckets`, which meant the
phase, the URL, the size and the degraded conditions were unreadable for exactly the
people operating the workloads that owned the bucket. The same gap held there for the
namespace-scoped reader roles and for the built-in `admin` that tenant teams hold on the
qs stage. The chart deployed on mgmt-p on that date was 1.13.1, against a repository tip
of 1.14.0. That observation comes from operating the cluster before the roles existed;
nothing in this repository records it, so it is the one claim on this page a reader cannot
follow to a file. It is nevertheless why the read fragment carries all three aggregation
labels unconditionally and why `bucketRoles.create` defaults to `true`: the failure being fixed was invisibility, not permissiveness.

## What this does not cover

Where each credential lives, what it can do in the cloud and what its leak costs is
[credentials-and-secrets.md](credentials-and-secrets.md). What separates one tenant's
objects from another's — the bucket policy, the two isolation layers — is
[tenancy-and-isolation.md](tenancy-and-isolation.md). How a cloud object is proven to be
this operator's is [ownership-and-attribution.md](ownership-and-attribution.md).
Installing the roles, and the upgrade trap of a pre-existing ClusterRole of the same name,
is [deployment.md](../operations/deployment.md).

The cloud side of the same question is not answered here either, and not because it was
left out: the exact StackIT role the service account needs is not enumerated anywhere in
this repository. [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md)
D5 states only the constraint — the roles are granted at project level and never at a
level that cascades into a second project — and the operator neither detects nor
compensates for a grant that breaks it. Which named role is the smallest one that still
lets the operator create buckets, credentials groups and access keys is not established,
and this page does not guess at it.

### H-14 — The operator cannot check the creator's rights, and there is no admission webhook

**Live today.** The mechanism is `Bucket` write plus the two Secret paths above. The
operator reconciles with its own permissions and the CR carries no requester identity, so
nothing verifies that whoever created a `Bucket` could read the Secret named in
`spec.cloneFrom.secretRef` or write the one named in `spec.secretRef.name`. There is no
webhook in this repository — no `ValidatingWebhookConfiguration` template, no webhook
server in the manager — so admission cannot close it either, and admission would not see
the later Secret changes the reconcile loop acts on anyway. The adversary is a subject
holding `Bucket` write in a namespace **without** Secret access there; a subject that has
both gains nothing, which is the whole design. What an operator can do: never bind
`<release>-edit` (or any equivalent write role on `buckets`) to such a subject, and keep
`Bucket` write reaching subjects only through the built-in `edit` and `admin`. The two
CRD-level guards that do exist are narrow and unrelated to this: `spec.bucketName` is
immutable and a read grant may not name the `Bucket` itself, both as CEL rules in
[`api/v1/bucket_types.go`](../../api/v1/bucket_types.go).

### H-15 — A clone reaches any endpoint the CR names, with credentials the CR names

**Live today.** `spec.cloneFrom.endpoint` is validated only as a non-empty string
([`api/v1/bucket_types.go`](../../api/v1/bucket_types.go), `CloneFrom`); there is no host
allowlist, and `EndpointURL` accepts an explicit `http://` prefix and otherwise assumes
`https`. Two processes then talk to that host: the operator pod itself, which measures the
source once before the copy starts (`ensureClone` in
[`clone.go`](../../internal/controller/clone.go)), and the clone Job's pod, which performs
the copy. Neither has an egress restriction — the shipped NetworkPolicy governs ingress to
the clone pods only. The adversary is again a subject with `Bucket` write, and the
capability is threefold: outbound S3 requests from inside the operator namespace to any
host reachable from the cluster, whatever those requests return landing in a bucket the
requester can read, and the source credentials being presented to a host the requester
chose. That last one leaks the access key id in cleartext, since SigV4 carries it in the
`Credential` field of the `Authorization` header — that is how SigV4 is defined rather
than something verified in this repository; the secret half is used only as an HMAC key
and does not travel. Combined with H-14 this is the concrete shape of the confused deputy:
two data keys of *any* Secret in the namespace, read by the operator and presented to an
attacker-chosen endpoint. What an operator can do: keep Bucket write bound only together
with Secret access, and add an egress NetworkPolicy on the operator namespace if the
cluster's CNI supports one.

### H-16 — Clone pods carry the operator namespace's default ServiceAccount token

**Live today.** `buildCloneJob` sets no `serviceAccountName` and no
`automountServiceAccountToken`, so a clone pod runs as the `default` ServiceAccount of the
operator namespace with its token projected into the container. Exploitability depends
entirely on what a given cluster binds to that ServiceAccount — on a cluster that binds it
nothing, the token authenticates as a subject with no permissions. It is also not directly
reachable: the pod runs a fixed image with fixed arguments and copies between two S3
remotes, so there is no obvious primitive for reading the token file out of it. It is
named here because the defence should not rest on that. What an operator can do: set
`automountServiceAccountToken: false` on the `default` ServiceAccount of the operator
namespace, which the clone pods do not need and the operator pod does not use.

### H-17 — Every Secret, Pod and Job in the cluster is held in the operator's cache

**Live today.** The manager is created with no cache restriction
([`cmd/main.go`](../../cmd/main.go), `ctrl.NewManager` sets no `Cache` options), so every
type the operator watches or reads through the cached client is listed and watched
cluster-wide and kept in memory: Secrets, through the watch backing `bucketsForSecret`;
Jobs, through the clone-job watch in `SetupWithManager`; and Pods, through the cached
`List` in `pollCloneStats`. The predicates `isManagedSecret` and `isCloneJob` filter the
events that reach the reconciler, not what the informer holds, and the
`client.InNamespace` on the pod list filters the cached result rather than the informer.
The ClusterRole already permits all of these reads, so it is not an authorisation gap —
it changes the consequence of a *different* failure: a memory disclosure, a core dump or
a debugging endpoint on the operator pod exposes the whole cluster's Secret material
rather than only this operator's, and every pod and job spec beside it. The Secrets are
what make this worth naming; the pod and job specs carry no credential material of their
own. What an operator can do: nothing from the outside today, other than keeping the
pod's attack surface small — the fix is a cache selector in the manager options, and it
is not configurable.

### H-18 — The aggregation-only rule assumes the cluster's built-in roles are the stock ones

**Live today.** `<release>-edit` is safe to aggregate only because Kubernetes' built-in
`edit` and `admin` hand out Secret access in the same namespace, so `Bucket` write never
arrives without it. That is a property of the *cluster*, not of this chart: a cluster that
customised `edit` or `admin` to drop Secret access — or that binds `Bucket` write through
some other aggregated role of its own — splits the combination again and re-opens H-14,
silently, with nothing in this repository able to notice. The chart's own render test
([`test/helm/render_test.go`](../../test/helm/render_test.go)) pins the labels it emits,
and the e2e test ([`test/e2e/rbac_test.go`](../../test/e2e/rbac_test.go)) proves
aggregation lands, but neither inspects what the built-in roles actually grant. What an
operator can do: check that the cluster's `edit` and `admin` still carry `secrets` before
relying on the invariant — `kubectl get clusterrole edit -o yaml` — and treat any locally
defined role that aggregates `<release>-edit` without Secret access as a direct binding in
the sense of the rule above.
