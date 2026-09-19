# Prerequisites

Almost everything on this page happens **outside the cluster**, in the StackIT account, before the
chart is installed. The operator cannot create a project, a service account or a role — it can only
use the ones it is handed. Account setup is also where **cross-project** isolation can be broken, by
granting the service account's role above the project, and that break is not detectable from inside
the operator. The second isolation layer — workload against workload *inside* one project — does not
depend on anything on this page; it rests entirely on the deny policy the operator writes per bucket
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)) and is covered in
[../security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md). The one item on this
page that lives *inside* the cluster is the cluster's own Kubernetes version, which this repository
never declares — see [The cluster it runs on](#the-cluster-it-runs-on).

For installing the chart once these exist, see [deployment.md](deployment.md). For what each setting
does at runtime, see [configuration.md](configuration.md). The complete list of Helm values and
`Bucket` fields lives in [the README reference](../../README.md) and nowhere else.

---

## What must exist

| # | Item | Per what | Why it cannot be skipped |
| --- | --- | --- | --- |
| 1 | A StackIT **project** | one per cluster | Object storage is project-bound; the project boundary *is* the tenant boundary ([ADR 0005 D1](../adr/0005-the-operator-serves-one-project-in-one-region.md)) |
| 2 | A **service account** in that project | one per project | The operator authenticates as it for every control-plane call |
| 3 | A role on that service account, **scoped to the project** | one per service account | An organisation-scoped role silently dissolves the tenant boundary ([ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md)) |
| 4 | An exported **service-account key** (key flow, RSA) | one per service account | The only authentication mechanism that still works; see [The key file](#the-key-file) |
| 5 | A chosen **region** | one per deployment | Install-time configuration, not a per-`Bucket` selector ([ADR 0005 D2](../adr/0005-the-operator-serves-one-project-in-one-region.md)) |
| 6 | **Network egress** from the operator pod to three hosts | one per cluster | See [Network egress](#network-egress) |
| 7 | A **Kubernetes cluster at 1.25 or newer** | one per deployment | Below it the CRD's admission-time guards silently do not exist. Derived, not declared — see [The cluster it runs on](#the-cluster-it-runs-on) |

The object-storage *service* inside the project is **not** on this list — the operator enables it
itself, with one caveat recorded under [What the operator does for itself](#what-the-operator-does-for-itself).

---

## The cluster it runs on

**The repository declares no minimum Kubernetes version.** The chart's
[Chart.yaml](../../deploy/helm/stackit-s3-provisioner/Chart.yaml) carries no `kubeVersion`
constraint, no Helm value or flag states one, and no document in the tree names a number, so
`helm install` accepts whatever cluster it is pointed at. The figure below is **derived** from what
the chart and the CRD actually use — it is not a support promise the project has made anywhere.

**Derived floor: Kubernetes 1.25. Exercised by this repository's own tests: 1.33, and nothing else.**

| Evidence | Where | Floor it sets |
| --- | --- | --- |
| Two CEL validation rules (`x-kubernetes-validations`): `bucketName` is immutable, and `spec.grantReadAccess` may not name the `Bucket` itself | [config/crd/bases/stackit-bucket.gtrfc.com_buckets.yaml](../../config/crd/bases/stackit-bucket.gtrfc.com_buckets.yaml), generated from the markers in [api/v1/bucket_types.go](../../api/v1/bucket_types.go) | **1.25** — CRD validation rules are enabled by default from 1.25, alpha and gated before that. The highest floor in this table, and therefore the number |
| CRD served as `apiextensions.k8s.io/v1`, with schema-level `default`s | the same file | 1.16 for the API version, 1.17 for structural-schema defaulting |
| ClusterRole aggregation labels `rbac.authorization.k8s.io/aggregate-to-{view,edit,admin}` | [clusterrole-view.yaml](../../deploy/helm/stackit-s3-provisioner/templates/clusterrole-view.yaml), [clusterrole-edit.yaml](../../deploy/helm/stackit-s3-provisioner/templates/clusterrole-edit.yaml) | 1.17 |
| Pod-level `seccompProfile: RuntimeDefault` | [deployment.yaml](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml) | 1.19 |
| Lease-based leader election — the controller-runtime default behind `--leader-elect` | [cmd/main.go](../../cmd/main.go) | 1.14, for `coordination.k8s.io/v1` |
| `k8s.io/*` v0.37.0 under controller-runtime v0.25.0 | [go.mod](../../go.mod) | No floor, a direction: upstream publishes one controller-runtime minor per client-go minor and tests only that pairing, so these libraries are built against the 1.37 API surface |
| envtest assets 1.33.0, kind node image `v1.33.4` | [Makefile](../../Makefile), [.github/workflows/release.yml](../../.github/workflows/release.yml) | No floor: the only server version any suite in this repository has run against |

What that means for the cluster in hand:

* **Below 1.25, do not.** Such an API server does not know the two CEL rules, and an unknown schema
  field is dropped rather than rejected, so the CRD installs cleanly and the admission-time guards
  are simply gone. **Not verified, and this is the gap:** that pruning behaviour was reasoned from
  the feature's history, not exercised against a real pre-1.25 API server. What is lost is the
  error message rather than the data — a `spec.bucketName` edited after provisioning is ignored,
  because the operator keeps using the physical name it froze in `status.resolvedBucketName`
  ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md)), and a
  self-reference in `spec.grantReadAccess` is skipped by the reconciler and filtered out again when
  the policy document is built.
* **1.25 through 1.32: expected to work, never run.** Not verified, and this is the gap.
* **1.33: the version envtest and the release pipeline use.** Everything this repository proves, it
  proves there.
* **Newer than 1.33:** equally untested here, but it is the direction the client libraries point.

The chart is `apiVersion: v2`, so the install path assumes Helm 3.

---

## One project per cluster

One operator deployment serves exactly one project, and the project is not a setting: it is read
from the `projectId` field of the key file the operator is given
([ADR 0005 D1](../adr/0005-the-operator-serves-one-project-in-one-region.md)). There is no `Bucket`
field that selects a project, and no way to move a `Bucket` from one project to another by editing
it.

That is deliberate. Cross-project separation is enforced by the provider's own authorisation before
any code in this repository runs — two service accounts in two projects of the same organisation
were pointed at each other against the live API on 2026-06-30 and every cross-project list, create
and delete answered HTTP 403 in both directions. A process holding several projects' keys would
replace that guarantee with one enforced by this operator's own lookup logic. Serving three clusters
therefore means three projects, three service accounts, three keys and three operator deployments.

**Two deployments must never share a project**
([ADR 0005 D9](../adr/0005-the-operator-serves-one-project-in-one-region.md)). Both would bootstrap
the same project-wide admin identity, and the bootstrap clears that identity's existing access keys
before minting a fresh one (`ensureAdmin` in
[internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go)), so the
second install destroys the first operator's admin credential — and with it, for that operator,
policy writes, emptiness checks and therefore deletion. Established by reading the bootstrap path,
not by running it.

---

## The service account, its role, and the default credentials group

### The one rule that matters

**Grant the role at project level. Never at organisation level.**
([ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md))

An organisation-scoped role cascades into every project underneath it. The service account would
then be authorised in the *other* clusters' projects too, every cross-project call would simply
start succeeding, and nothing would report it: there is no error, no event, no metric and no
condition. The operator does not inspect its own grants and cannot compensate for them. This is the
single most dangerous mistake in the whole system, and it is made in a console this code cannot see.

### The other rule that matters

**Never hand the project's default credentials group to a workload.** Every workload gets a
credentials group of its own — the one the operator creates for its `Bucket` — together with the
restrictive policy the operator writes on that bucket
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)).

Inside a project the provider's default is open: absent an explicit deny, *any* credentials group of
the project may do anything in any bucket of the project, measured against the live API on
2026-06-30 ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)). The
operator closes that bucket by bucket, naming the principals that may act and denying everyone else.
A key from the default credentials group sits outside that arrangement in both directions: on a
bucket whose policy is already written it is denied, and everywhere else in the project — buckets
this operator did not create, and the window between a fresh bucket existing and its policy being
applied — it still has the project-wide reach the default grants it. It is also not an identity this
operator owns, rotates or could revoke
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)).

Like the role scope above, this is done in a console this code cannot see, and the operator can
neither detect nor repair it: it never inspects credentials it did not mint itself, so a workload
authenticating with a default-group key looks exactly like one using the Secret its `Bucket` wrote.
No event, no metric and no condition tells the two apart.

### Which role

**Not verified, and this is the gap:** the exact minimal project-scoped role name for object-storage
management is not established. Deployments today are therefore granted a broader role than the
decision wants, and the requirement that is enforceable is only the scope — project, never
organisation. This is recorded as an open item under *Residual risks* in
[ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md).

What the role has to cover is knowable, because the operator's control-plane call set is closed.
Every call is made through [stackit/client.go](../../stackit/client.go) and no other path exists:

<details>
<summary>Control-plane operations the operator calls, and why</summary>

| Operation | Called for |
| --- | --- |
| `GetServiceStatus` | Deciding whether object storage is enabled for the project |
| `EnableService` | Enabling it, and only on a structured "not enabled" answer |
| `CreateBucket` | Provisioning a `Bucket`'s cloud bucket |
| `GetBucket` | Reading the bucket's path-style URL, which yields the S3 endpoint written into the workload Secret |
| `ListBuckets` | Deciding whether a bucket already exists |
| `DeleteBucket` | Finalizer teardown, after the emptiness check |
| `CreateCredentialsGroup` | The operator's own admin identity and each `Bucket`'s workload identity |
| `ListCredentialsGroups` | Finding the admin group during bootstrap, and indexing the project's groups once on **every** provisioning reconcile and every teardown — this is the most frequently made call of the set |
| `DeleteCredentialsGroup` | Teardown of a `Bucket`'s own workload group |
| `CreateAccessKey` | Minting the admin key and each workload key |
| `ListAccessKeys` | Checking that a group still holds a live key |
| `DeleteAccessKey` | Key replacement, rotation and teardown |

The **data plane** is not covered by this role at all. Bucket policies, bucket tags, object listing
and object removal are plain S3 calls made with an access key the operator mints for itself — see
[What the operator does for itself](#what-the-operator-does-for-itself) and
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md).

</details>

If the granted role is too narrow, the failure is visible: the API answers a structured `403`, which
is classified as a definitive provider refusal and drops the `Bucket` to `Failed` immediately rather
than holding its readiness ([ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md) —
an enumerated exception list, classified by origin and never by status code alone, per
[ADR 0012 D2](../adr/0012-ready-describes-the-last-verified-state.md); `ProviderRefused` in
[stackit/errors.go](../../stackit/errors.go)). See
[Failure modes](#failure-modes).

---

## The key file

The provider's token flow was switched off on 2025-12-17. The **key flow** is the only mechanism:
the exported key JSON carries an RSA private key, the SDK signs a JWT with it and exchanges that for
a short-lived bearer token which it refreshes on its own.

Export the key as JSON from the service account and keep the whole file intact. Its shape, verified
against a real exported key:

```jsonc
{
  "id": "…",
  "projectId": "00000000-0000-0000-0000-000000000000",   // example - the operator reads this field
  "active": true,
  "keyAlgorithm": "RSA_2048",                            // example
  "keyOrigin": "GENERATED",                              // example
  "keyType": "USER_MANAGED",                             // example
  "createdAt": "…",
  "publicKey": "…",
  "credentials": {
    "kid": "…",
    "iss": "…",
    "sub": "…",
    "aud": "https://accounts.stackit.cloud",                     // example
    "tokenEndpoint": "https://accounts.stackit.cloud/oauth/v2/token", // example
    "privateKey": "-----BEGIN PRIVATE KEY-----…"         // never leaves this file
  }
}
```

What each party uses:

| Field | Consumer |
| --- | --- |
| `projectId` | The operator, to bind the deployment to a project (`LoadAccount` in [stackit/client.go](../../stackit/client.go)) |
| `credentials.iss` | The operator, for log lines only |
| `validUntil`, where the key carries one | The operator, to export when the key in use expires ([ADR 0016 D10](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)). A key file without the field is a normal shape, and then nothing is exported rather than a zero — see [monitoring.md](monitoring.md) |
| everything else | The SDK's key flow; the operator never parses the private key |

Two consequences of how it is read:

* **A key path that is configured but unusable aborts startup.** A file that cannot be read, or JSON
  without a `projectId`, makes the process exit rather than fall back to provisioning nothing
  ([ADR 0005 D7](../adr/0005-the-operator-serves-one-project-in-one-region.md)).
* **The key is re-read while the operator runs, and a replacement is proven before it is used.** The
  process polls the file every `stackit.serviceAccountKey.reloadInterval` (`30s` `# default`) and
  compares its content against the key in use; identical content is a no-op and costs no API call.
  Content that differs is validated first — it must parse, carry a `projectId`, name the same
  project the process started with, be accepted by the SDK, and mint a token in one live call made
  with the candidate — and only then is the authenticated client swapped over. A candidate that
  fails any of those steps is refused and the running key is left exactly where it is, which is what
  keeps a truncated write or an already-revoked key from taking down a healthy operator
  ([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)). Setting
  the interval to `"0"` switches the reload off and restores the restart-only behaviour.

How long a replacement takes to land is the sum of three waits, and the first is the largest and is
not this operator's: the kubelet needs **up to about 90 seconds** to refresh a Secret volume inside
a running container, then up to one poll interval for the operator to notice, and then up to one
`driftResyncInterval` (`10m` `# default`) for the `Bucket` resources that had already failed on the
old key to be reconciled again. Rotating the key is a procedure with steps of its own —
[credentials.md](credentials.md).

The file is a credential for the whole project. Keep it out of Git — `account-*.json` is in
[.gitignore](../../.gitignore) for exactly this reason — and hand it to the cluster as a Secret, as
below.

---

## Network egress

The operator pod needs outbound HTTPS to three hosts. Two are fixed, the third is derived at runtime
from the bucket's own path-style URL rather than hardcoded, so it follows the region.

| Host | Used for | Where it comes from |
| --- | --- | --- |
| `accounts.stackit.cloud` | Token exchange for the key flow | `credentials.tokenEndpoint` in the key file |
| `object-storage.api.stackit.cloud` | Every control-plane call | SDK default (`stackit-sdk-go/services/objectstorage` v1.9.1) |
| `object.storage.eu01.onstackit.cloud` &nbsp;`# example` | Every data-plane call: policies, tags, emptiness checks, size measurement | Parsed from the bucket's `urlPathStyle` (`BucketConnInfo` in [stackit/client.go](../../stackit/client.go)); the shown value is the eu01 one |

A token fetch is a `POST` and is deliberately **not** retried by the operator's retry transport,
which repeats only `GET` and `HEAD` ([stackit/retry.go](../../stackit/retry.go)). A blocked or flaky
path to the token endpoint therefore fails whole reconciles rather than being papered over.

---

## Minimal working configuration

Everything below assumes items 1–4 exist in the StackIT account and the key file is on disk as
`./account.json`.

```bash
# 1. Namespace for the operator. The bootstrap admin Secret is written here, so
#    the operator must know it - the chart sets POD_NAMESPACE for you.
kubectl create namespace stackit-s3-provisioner-system

# 2. The service-account key as a Secret. The data key name is free, but it must
#    match stackit.serviceAccountKey.secretKey below (default: sa-key.json).
kubectl -n stackit-s3-provisioner-system create secret generic stackit-sa-key \
  --from-file=sa-key.json=./account.json

# 3. Install. Without serviceAccountKey.secretName the operator comes up healthy
#    and provisions nothing (skeleton mode) - see Failure modes.
helm repo add stackit-s3-provisioner https://guided-traffic.github.io/stackit-s3-provisioner/
helm install stackit-s3-provisioner stackit-s3-provisioner/stackit-s3-provisioner \
  --namespace stackit-s3-provisioner-system \
  --set stackit.region=eu01 \
  --set stackit.serviceAccountKey.secretName=stackit-sa-key
```

The two values this page is about:

```yaml
# values.yaml
stackit:
  # The region every Bucket of this deployment is provisioned in. A Bucket whose
  # spec.region disagrees is parked as a configuration fault, not routed elsewhere.
  region: eu01                      # default
  serviceAccountKey:
    # Existing Secret holding the key JSON. Empty => skeleton mode, no cloud calls.
    secretName: ""                  # default (empty)
    # Data key inside that Secret; it is mounted at /etc/stackit/<secretKey>.
    secretKey: sa-key.json          # default
```

Both are read once at startup. Changing the region needs a pod restart
([ADR 0005 D8](../adr/0005-the-operator-serves-one-project-in-one-region.md)), and so does pointing
the operator at a different Secret or a different data key, because both are rendered into the
Deployment. Changing the *contents* of the Secret already referenced is the one change that needs no
restart ([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)).
`helm upgrade` rolls the Deployment anyway, so an upgrade that changes any of these restarts on its
own. Every other value in the chart is documented in [the README reference](../../README.md).

---

## What the operator does for itself

Two things that look like account setup are not, and doing them by hand is unnecessary.

### It enables the object-storage service

Before provisioning, the operator reads the service status and enables the service if it is off.
Verified in `EnsureService` ([stackit/client.go](../../stackit/client.go)) and `isServiceNotEnabled`
([stackit/errors.go](../../stackit/errors.go)):

1. `GetServiceStatus`. A `200` means enabled; the answer is cached for the lifetime of the process,
   so this is one call per operator start, not one per `Bucket` per reconcile.
2. `EnableService` runs **only** on the API's definitive "not enabled" answer — a `404` whose body is
   valid JSON. Every other outcome (a `403`, a `5xx`, a transport error, an HTML page from a gateway)
   leaves the status *unknown*, and unknown is returned as an error rather than read as "disabled".
   That distinction is load-bearing: escalating a failed read into an attempted write turned a
   two-minute provider blip on 2026-08-25 into 342 reconcile errors.
3. After enabling, the operator polls the status for up to 30 seconds for the service to report
   ready, then gives up with a timeout error and retries on the next reconcile.

**One caveat, verified by reading the pass order in
[internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go):** the
admin bootstrap runs *before* `EnsureService`, so on a project where object storage has never been
enabled the first provider call the operator makes is a credentials-group call, not
`GetServiceStatus`. Whether the provider answers a credentials-group call on a project whose object
storage is disabled — and therefore whether the auto-enable path is ever reached on a genuinely
fresh project — was **not verified against the live API**. If a fresh project's `Bucket` resources
sit in `Failed` with a message from `bootstrap admin credentials`, enable object storage for the
project once in the StackIT portal; the auto-enable is otherwise a belt-and-braces path for a
service that was turned off after the fact.

### It mints its own S3 admin credential

Bucket policies, bucket tags, emptiness checks and size measurement are S3 data-plane calls, and the
service-account key cannot make a single one of them. So on first need the operator creates a
credentials group named `operator-admin` plus one access key in it
([ADR 0004 D1](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)), and stores the
result in a Secret in its own namespace — name from `--admin-credentials-secret-name`, default
`stackit-s3-provisioner-admin`, namespace from `POD_NAMESPACE` or `--operator-namespace`
([ADR 0004 D3](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)).

Do not create that group, do not place a key in it, and do not hand it to anything. It is exempt from
the deny statement of every bucket policy the operator writes, which is precisely the capability that
must never reach a workload. Its operational care and feeding — including what happens when it is
deleted out of band — is on [credentials.md](credentials.md).

---

## What stays manual

| Step | Where | Notes |
| --- | --- | --- |
| Create the project | StackIT portal | One per cluster |
| Create the service account | StackIT portal, inside that project | Name is free; `s3-bucket-provisioner` &nbsp;`# example` |
| Grant the role | StackIT portal | **Project scope only.** Exact minimal role is an open gap |
| Keep the default credentials group out of workloads | StackIT portal | Never a workload credential; each workload uses the group and Secret its own `Bucket` gets |
| Export the key | StackIT portal | Key flow / RSA, JSON |
| Create the Kubernetes Secret | `kubectl`, see above | The chart consumes an existing Secret; it does not create one |
| Choose the region | Helm value | One per deployment |
| Repeat for every further cluster | — | Never share a project between two deployments |

Nothing pins which project a deployment is *expected* to serve. The project is whatever the mounted
key names, so mounting the wrong key moves an entire cluster's provisioning into a different project
with no configuration looking wrong and nothing checking it at startup. One narrow check exists and
does not close this: a key *replaced while the operator runs* is compared against the project that
process started with and refused if it names another one, but a restart erases that reference point
and the new process adopts whatever the file says
([ADR 0016 D4](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)).
Recorded under *Residual risks* in
[ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) and as a gap in
[../security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md).

---

## Verifying the setup

Run this after installing, before anyone depends on the cluster.

**1. The operator is not in skeleton mode.**

```bash
kubectl -n stackit-s3-provisioner-system logs deploy/stackit-s3-provisioner | grep -i stackit
# want: "StackIT client configured" with the project id and region from the key
# not:  "no StackIT service-account key configured; running in skeleton mode"
```

The same fact is a metric — `stackit_s3_provisioner_skeleton_mode` must be `0`. That metric is
always exported; the alert that watches it is not, because the chart's whole `PrometheusRule` is
gated on `monitoring.prometheusRule.enabled`, which defaults to `false`. See
[monitoring.md](monitoring.md).

**2. The project id is the one you meant.** The log line above prints it, taken from the key file.
This is the only check that the right key was mounted; nothing automates it.

**3. A `Bucket` reaches `Ready`.**

```yaml
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: prereq-check
  namespace: default
spec:
  bucketName: prereq-check        # example
  region: eu01                    # default - must equal the operator's region
  secretRef:
    name: prereq-check-s3
```

```bash
kubectl apply -f prereq-check.yaml            # the manifest pins namespace: default
kubectl wait -n default --for=condition=Ready bucket/prereq-check --timeout=180s
```

A successful wait proves, in one step, that the key authenticates, that the role is broad enough to
create a bucket, a credentials group and an access key, that object storage is enabled, that the
admin bootstrap succeeded and that the data plane is reachable for the policy write.

**4. The credentials Secret was written.**

```bash
kubectl -n default get secret prereq-check-s3 -o jsonpath='{.data.S3_ENDPOINT}' | base64 -d
```

**5. Clean up.** Deleting the `Bucket` also removes its cloud bucket, its credentials group and the
Secret — the bucket is empty, so the emptiness guard passes. See [deletion.md](deletion.md).

```bash
kubectl delete -n default bucket prereq-check
```

The operator's own admin credentials group and Secret stay behind on purpose; they are shared by the
whole project and are never torn down with a `Bucket`.

---

## Failure modes

| What went wrong | What the operator shows | What to do |
| --- | --- | --- |
| No key configured at all | Pod healthy, probes green, log `running in skeleton mode`; every `Bucket` at phase `Pending`, `Ready=False`, reason `NotImplemented`, message `operator skeleton: no StackIT service-account key configured`; metric `stackit_s3_provisioner_skeleton_mode` = 1 (always exported); alert `StackitS3SkeletonMode` after 15m, severity critical, **only when `monitoring.prometheusRule.enabled` is set to true — it defaults to `false`, so a default install shows no alert at all** | Set `stackit.serviceAccountKey.secretName` and check the referenced Secret exists |
| Key path configured but the file is missing, unreadable, empty or has no `projectId` | Startup aborts: log `unable to configure the StackIT client` with the path and the underlying `load StackIT service-account key: …`, then `CrashLoopBackOff`. This is intentional — the operator never degrades from "should provision" to "provisions nothing" ([ADR 0005 D7](../adr/0005-the-operator-serves-one-project-in-one-region.md)) | Check the Secret's data key name against `stackit.serviceAccountKey.secretKey`, and that the exported JSON is complete |
| Key revoked or deleted after startup | `Ready=False`, reason `Failed`, immediately — no grace period. The failure arrives as a structured `400 {"error":"invalid_grant"}` from the token endpoint, not the `401`/`403` one would expect, because the key flow never reaches the object-storage API; a structured `400` is one of the enumerated definitive refusals ([ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md)). Verified live on 2026-08-25 | Issue a new key and write it into the **same** Secret; the operator picks it up without a restart once it has proven it ([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)) — see [credentials.md](credentials.md) for the procedure and [the window above](#the-key-file) for how long it takes. If the operator refuses the replacement it says so in the log and holds `stackit_s3_provisioner_sa_key_reload_failing` at `1` while it keeps using the old key. **Restarting the pod** is the fallback, and the only route when the reload is switched off (`stackit.serviceAccountKey.reloadInterval: "0"`) |
| Role missing, too narrow, or removed from the project | Structured `403` from the API, classified as a definitive refusal ([ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md)): `Ready=False`, reason `Failed`, no readiness hold. `status.message` names the operation that was refused | Re-grant the role at project level and let the next reconcile run |
| Role granted at **organisation** level | **Nothing.** Every `Bucket` goes `Ready`, every metric is green, and the tenant boundary is gone ([ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md)) | Audit the grant in the portal. There is no in-cluster signal to wait for |
| A workload handed a key from the project's **default credentials group** | **Nothing.** Its `Bucket` reaches `Ready`, its own credentials group is created and untouched, and the extra key is invisible to the operator — the reach it carries is over everything in the project the operator has not written a policy for | Audit the project's credentials groups and access keys in the portal, then move the workload onto the Secret its `Bucket` writes ([credentials.md](credentials.md)) |
| Wrong key mounted (right shape, wrong project) | **Nothing** beyond the project id in the startup log. Buckets are provisioned correctly, in the wrong project | Compare the logged project id against the one intended, per cluster |
| `stackit.region` set to a region the `Bucket` resources do not ask for | Every `Bucket` at `Ready=False`, reason `Failed`, message `spec.region "X" does not match this operator's region "Y"; provisioning is limited to "Y"`. No provider call is made and nothing already provisioned is renamed or reassigned ([ADR 0005 D3, D4](../adr/0005-the-operator-serves-one-project-in-one-region.md)). Not requeued on a backoff — a retry cannot change a declared region | Fix `stackit.region` (needs a restart) or fix the CRs. Loud, but only once a `Bucket` is applied |
| `stackit.region` set to a string that is not a real region | **Not verified, and this is the gap:** the operator validates nothing at startup, so this surfaces as failing provider calls rather than a startup error, and the exact failure was not tested | Check the region string against the provider's regions by hand |
| Object storage never enabled in a fresh project | `Ready=False`, reason `Failed`, `status.message` beginning `bootstrap admin credentials` — see the caveat above | Enable object storage for the project once in the portal |
| Two operator deployments on one project | The later bootstrap clears the shared admin identity's keys, so the **earlier** operator loses policy writes, emptiness checks and deletion, while looking configured correctly ([ADR 0005 D9](../adr/0005-the-operator-serves-one-project-in-one-region.md)) | One project, one deployment. Recovering the broken operator's admin credential is on [credentials.md](credentials.md) |

Provider errors that are *not* about account setup — outages, rate limits, the fleet-wide hold — are
on [provider-outages.md](provider-outages.md).

---

## Related

| Page | Read it for |
| --- | --- |
| [deployment.md](deployment.md) | Installing, upgrading and uninstalling the chart |
| [configuration.md](configuration.md) | What the settings do and when a restart is needed |
| [credentials.md](credentials.md) | The operator's admin Secret, rotating the service-account key, and rotating a workload key |
| [bucket-status.md](bucket-status.md) | Reading a parked or held `Bucket` |
| [monitoring.md](monitoring.md) | The skeleton-mode metric and the rest of the alert set |
| [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) | Why one project, one region, one deployment |
| [ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) | Why a replacement key is proven before it is used, and what a refused one does |
| [ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) | Why the operator mints its own S3 admin credential |
| [../security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md) | What the project boundary defends against, and what it does not |
