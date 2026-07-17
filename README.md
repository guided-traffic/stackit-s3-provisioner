# stackit-s3-provisioner

![Coverage](https://raw.githubusercontent.com/guided-traffic/stackit-s3-provisioner/main/.github/badges/coverage.json)
![Go Version](https://img.shields.io/badge/go-1.26-blue)
![License](https://img.shields.io/badge/license-Apache--2.0-green)

A Kubernetes operator that provisions **StackIT Object Storage** buckets, workload
credentials and isolation policies through Custom Resources. One operator
deployment per cluster, bound to a single StackIT project via a service-account key.

The operator runs on any Kubernetes cluster, but it is designed and tuned for
**GitOps workflows — FluxCD in particular**: see
[GitOps / FluxCD](#gitops--fluxcd).

## What it does

A `Bucket` custom resource maps to one isolated workload: a StackIT bucket, a
dedicated credentials group, an S3 access key, and a deny-based bucket policy that
isolates workloads from each other (Layer 2). Cross-project isolation (Layer 1) is
structurally guaranteed by StackIT itself. See [`CLAUDE.md`](CLAUDE.md) and
[`INIT-SETUP.md`](INIT-SETUP.md) for the architecture and security invariants.

```yaml
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: my-bucket
  namespace: team-a
spec:
  bucketName: my-bucket
  region: eu01
  secretRef:
    name: my-bucket-s3   # operator writes the credentials + connection info here
```

### Credentials Secret

The operator writes the provisioned access key **and** the S3 connection
parameters a workload needs into the referenced Secret. By default the data keys
are env-var style, so the Secret can be consumed directly via `envFrom`:

| Default key             | Value                                              |
| ----------------------- | -------------------------------------------------- |
| `AWS_ACCESS_KEY_ID`     | S3 access key id                                   |
| `AWS_SECRET_ACCESS_KEY` | S3 secret access key                               |
| `S3_BUCKET`             | physical bucket name (see [Bucket naming](#bucket-naming)) |
| `S3_REGION`             | region (e.g. `eu01`)                               |
| `S3_ENDPOINT`           | endpoint host (e.g. `object.storage.eu01.onstackit.cloud`) |
| `S3_BUCKET_URL`         | full path-style bucket URL                         |

Every data-key name is overridable per Bucket via `spec.secretRef.keys` — empty
fields fall back to the defaults above:

```yaml
spec:
  bucketName: my-bucket
  secretRef:
    name: my-bucket-s3
    keys:                          # all optional
      accessKeyID:     ACCESS_KEY  # default AWS_ACCESS_KEY_ID
      secretAccessKey: SECRET_KEY  # default AWS_SECRET_ACCESS_KEY
      bucketName:      BUCKET      # default S3_BUCKET
      region:          REGION      # default S3_REGION
      endpoint:        ENDPOINT    # default S3_ENDPOINT
      bucketURL:       BUCKET_URL  # default S3_BUCKET_URL
```

## GitOps / FluxCD

Nothing in the operator requires FluxCD — it works with plain `kubectl`, Argo CD
or any other tooling. But its behavior is deliberately shaped so that a Git
repository can stay the single source of truth and a continuously syncing
controller like Flux never fights the operator:

- **The operator never mutates `spec`, labels or annotations of a `Bucket`.**
  All operator state goes to the status subresource (plus one operator-owned
  bookkeeping annotation it only ever adds). Server-side apply and Flux drift
  detection stay clean; re-applying the same manifests is always a no-op.
- **Credentials rotation is level-based, not edge-based.** The
  `rotate-credentials-at` annotation value lives in Git; changing it in Git
  rotates exactly once, and every subsequent Flux sync of the same value does
  nothing (see [Credentials rotation](#credentials-rotation)).
- **Bucket cloning is one-shot and terminal.** Once `status.clone.phase` is
  `Completed`, re-applied or even edited `cloneFrom` manifests never re-trigger
  a copy (see [Cloning an existing bucket](#cloning-an-existing-bucket)).
- **Config faults fail without a requeue hammer.** An invalid CR (region
  mismatch, key collision, foreign bucket, self-clone …) parks as
  `Ready=Failed` with a message instead of hot-looping; fixing the manifest in
  Git and letting Flux sync it reconciles the new generation.
- **Secret gating composes with GitOps app rollouts.** With a clone requested,
  the credentials Secret only appears after the data is complete — pods that
  Flux deploys in parallel and that consume the Secret via `envFrom` /
  `secretKeyRef` simply stay pending until the bucket is actually ready. No
  `dependsOn` choreography required.
- **Disaster recovery replays from Git.** Physical bucket names are frozen in a
  durable annotation, ownership tags use `namespace/name` (not the CR UID), and
  cloud resources are found by deterministic names — restoring the same
  manifests into a fresh cluster re-adopts the existing buckets instead of
  duplicating them.

## Status

The operator reports progress on the `Bucket` status subresource, so `kubectl get
bucket` (short name `bkt`) shows the live state:

```
NAME        BUCKET      PHASE   READY   STATUS               REGION   AGE
my-bucket   my-bucket   Ready   True    provisioned          eu01     2m
```

- **`status.phase`** — `Pending` → `Provisioning` → `Ready`, or `Failed` /
  `Deleting`.
- **`Ready` condition** — reasons `Provisioned`, `Provisioning`, `Failed`, or
  `NotImplemented` (skeleton mode). `status.message` carries the current step or
  failure reason.
- Config faults (a `secretRef` pointing at the operator admin Secret, a
  `spec.region` that differs from the operator's region, a bucket-name/secret-key
  collision, or a bucket owned by someone else) set `Ready=Failed` **without**
  requeue-hammering — fix the CR and the next generation reconciles.
- Other status fields: `resolvedBucketName`, `bucketURL`, `credentialsGroupID`,
  `credentialsGroupURN`, `accessKeyID` (never the secret), `observedGeneration`,
  `operatorVersion`.

Each `Bucket` is stamped with S3 ownership tags (`managed-by` + `owner=<ns>/<name>`)
so the operator adopts only buckets it owns and refuses to clobber a pre-existing
foreign or non-empty bucket. On bootstrap the operator creates a shared
`operator-admin` credentials group + S3 key (persisted in its own admin Secret,
default `stackit-s3-provisioner-admin`); that group's URN sits in every bucket
policy's exemption list as a lockout safeguard.

## Cloning an existing bucket

A `Bucket` can be seeded from an existing S3 bucket — any S3-compatible endpoint
(another StackIT project, AWS, MinIO, …) — by declaring `spec.cloneFrom`. The
contents are copied **once**, right after the bucket is provisioned:

```yaml
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: my-bucket
  namespace: team-a
spec:
  bucketName: my-bucket
  secretRef:
    name: my-bucket-s3
  cloneFrom:
    endpoint: object.storage.eu01.onstackit.cloud  # host or URL of the source
    bucket: seed-data                              # source bucket name
    region: eu01                                   # optional (SigV4 signing)
    secretRef:
      name: seed-data-creds     # Secret with read access to the source bucket;
      keys:                     # must live in the Bucket's own namespace
        accessKeyID: AWS_ACCESS_KEY_ID          # optional overrides,
        secretAccessKey: AWS_SECRET_ACCESS_KEY  # defaults shown
    holdSecretUntilCloned: true # default
```

The source credentials Secret is read from the **Bucket's own namespace** only
(no `namespace` field — referencing foreign namespaces through the operator's
privileges is deliberately not possible). Its data-key names are configurable
via `cloneFrom.secretRef.keys`, and the defaults match what this operator writes
into its own credentials Secrets — so a Secret provisioned for another `Bucket`
works as a clone source as-is.

**Secret gating.** By default (`holdSecretUntilCloned: true`) the workload
credentials Secret is only written once the copy finished successfully, so
consuming workloads never start against a half-filled bucket. Set it to `false`
to publish the credentials immediately; the `Ready` condition still waits for
the clone either way.

**How it runs.** The copy is executed by an [rclone](https://rclone.org) Job in
the operator's namespace (image and pod resources via the Helm values
`clone.image` / `clone.resources`). rclone's remote-control API — protected by
a generated 32-character password, and by a NetworkPolicy restricting it to the
operator (`clone.networkPolicy.enabled`, default `true`; disable on clusters
whose CNI does not enforce NetworkPolicies) — is polled while the job runs, and
the transfer progress lands in the CR status:

```
$ kubectl get bkt my-bucket -o wide
NAME        BUCKET      PHASE          READY   STATUS                                                  CLONE
my-bucket   my-bucket   Provisioning   False   cloning from …/seed-data: 2.0 GiB / 18.0 GiB (11%)      2.0 GiB / 18.0 GiB (11%)
```

`status.clone` carries the details (`phase`, `bytesCopied`, `totalBytes`,
`progress`, `rate`, `eta`, `startedAt`, `completedAt`), and the `CloneCompleted`
condition tracks the outcome. The total size is measured once up front, so the
percentage has a stable denominator.

**Semantics.**

- The clone is **one-shot and terminal**: once `status.clone.phase` is
  `Completed` it never runs again for this Bucket, even if `cloneFrom` changes.
- A failed attempt is retried with backoff; rclone resumes and skips objects
  that were already copied. `rclone copy` semantics: the destination is merged
  into, never deleted from.
- Cloning a bucket onto itself (same endpoint + bucket) is rejected as a
  config fault.
- Deleting the CR while a clone is running stops the job and cleans up its
  staging Secret before the normal teardown.

## Credentials rotation

The workload access key can be rotated on demand via an annotation on the
`Bucket` CR — no spec change required:

```yaml
metadata:
  annotations:
    stackit-bucket.gtrfc.com/rotate-credentials-at: "2026-07-16T10:00:00Z"
```

The value is an opaque trigger (by convention an RFC3339 timestamp, mirroring
`kubectl rollout restart`'s `restartedAt`). Whenever it differs from
`status.lastRotationTrigger`, the operator replaces the access key — all keys in
the bucket's credentials group are deleted first, then a single fresh key is
created and written to the credentials Secret — and records the handled value
and time in `status.lastRotationTrigger` / `status.lastRotationTime`, emitting
a `CredentialsRotated` event.

The trigger is level-based and GitOps-safe: the operator never mutates the
annotation, an unchanged value is a no-op, and removing the annotation triggers
nothing. Rotation is **hard**: the old key stops working immediately, so
workloads must re-read the Secret (e.g. restart their pods) to pick up the new
credentials.

Rotate a specific Bucket:

```bash
kubectl annotate bucket my-bucket \
  stackit-bucket.gtrfc.com/rotate-credentials-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --overwrite
```

Rotate all Buckets matching a label selector (e.g. everything labelled
`team=payments`):

```bash
kubectl annotate buckets -l team=payments \
  stackit-bucket.gtrfc.com/rotate-credentials-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --overwrite
```

`--overwrite` is required on re-rotation (the annotation already exists then).
Both commands operate on the current namespace; add `-n <namespace>` or
`--all-namespaces` (with `kubectl annotate buckets --all-namespaces -l …`) as
needed.

## Deletion behavior

Deleting a `Bucket` CR tears down the access key, credentials group, bucket and
credentials Secret — but only when the bucket is **empty**. A non-empty bucket
blocks deletion (data-loss guard) until its objects are removed.

A Bucket can opt into an automatic wipe instead: with `spec.wipeOnDelete: true`
the operator deletes **all objects (including versions and delete markers)**
before removing the bucket. The field is mutable, so it can be set right before
deleting the CR.

```yaml
spec:
  bucketName: my-bucket
  wipeOnDelete: true   # default false: deletion is blocked while data exists
  secretRef:
    name: my-bucket-s3
```

The feature is gated operator-wide by the Helm value `wipeOnDelete.enabled`
(default `false`). While the gate is off, a requested wipe is ignored: deletion
degrades to the safe empty-only guard and a warning event
(`WipeOnDeleteSkipped`) is emitted. A wipe also never runs on a bucket whose
ownership tags do not prove this operator provisioned it.

## Install (FluxCD)

The chart is served from a plain Helm repository, so a `HelmRepository` +
`HelmRelease` pair is all Flux needs:

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
  namespace: stackit-s3-provisioner-system
spec:
  interval: 1h
  chart:
    spec:
      chart: stackit-s3-provisioner
      version: "1.x"   # or pin an exact version
      sourceRef:
        kind: HelmRepository
        name: stackit-s3-provisioner
        namespace: flux-system
  install:
    createNamespace: true
  values:
    stackit:
      region: eu01
      serviceAccountKey:
        secretName: stackit-sa-key   # see below
```

The StackIT service-account key must exist as a Secret (key `sa-key.json`) in
the release namespace. It contains a private key, so never commit it to Git in
plain text — ship it as a [SOPS-encrypted](https://fluxcd.io/flux/guides/mozilla-sops/)
Secret manifest alongside the HelmRelease (or via SealedSecrets / ExternalSecrets):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: stackit-sa-key
  namespace: stackit-s3-provisioner-system
stringData:
  sa-key.json: |
    { … service-account key JSON, SOPS-encrypted in Git … }
```

## Install (Helm)

```bash
helm repo add stackit-s3-provisioner https://guided-traffic.github.io/stackit-s3-provisioner/
helm repo update

# Provide the StackIT service-account key (key flow) as a Secret:
kubectl create namespace stackit-s3-provisioner-system
kubectl -n stackit-s3-provisioner-system create secret generic stackit-sa-key \
  --from-file=sa-key.json=./account.json

helm install stackit-s3-provisioner stackit-s3-provisioner/stackit-s3-provisioner \
  --namespace stackit-s3-provisioner-system \
  --set stackit.region=eu01 \
  --set stackit.serviceAccountKey.secretName=stackit-sa-key
```

Without `stackit.serviceAccountKey.secretName` the operator runs in **skeleton
mode**: it reconciles `Bucket` resources but does not touch the cloud.

## Bucket naming

By default the physical StackIT bucket name equals `spec.bucketName`. The operator
can prepend a fixed **prefix** (e.g. a cluster identifier) and optionally the
Bucket's **namespace**, so bucket names stay unique and traceable across clusters
or teams that share one StackIT project. It is an operator-wide policy configured
at install time:

```yaml
# values.yaml
bucketNaming:
  prefix: my-cluster        # prepended to every bucket name (empty = disabled)
  includeNamespace: true    # append the Bucket's namespace after the prefix
```

With the above, a `Bucket` named `my-bucket` in namespace `monitoring` is
provisioned as the physical bucket **`my-cluster-monitoring-my-bucket`**. The name
is composed as `<prefix>-<namespace>-<spec.bucketName>`, dropping any disabled
part; the defaults (`prefix: ""`, `includeNamespace: false`) reproduce the legacy
behaviour where the physical name equals `spec.bucketName`.

The composed name is what workloads connect to: it is written to the `S3_BUCKET`
and `S3_BUCKET_URL` keys of the credentials Secret and shown as the `RESOLVED`
column in `kubectl get bucket`.

**Stable across policy changes.** The physical name is frozen per Bucket the first
time it is provisioned — recorded in `status.resolvedBucketName` and a durable
annotation (`stackit-bucket.gtrfc.com/resolved-bucket-name`) that survives status
loss (e.g. a CR restored from backup). Changing `prefix` or `includeNamespace`
later therefore only affects **newly created** buckets; existing buckets keep their
original name and stay reachable. Buckets provisioned before this feature existed
keep their raw `spec.bucketName`.

**Constraints.** `prefix` must be a lowercase DNS-1123 label (letters, digits and
`-`, no leading/trailing `-`); an invalid prefix stops the operator at startup. The
composed name must be 3–63 characters and DNS-compliant — if the prefix and
namespace push it out of range the Bucket is rejected (`Ready=Failed`) rather than
silently truncated.

## Development

```bash
make help                      # list all targets
make build                     # build the manager binary
make test-unit-coverage        # unit tests (offline)
make test-integration-coverage # envtest integration tests
make lint gosec vuln cyclo     # linters and security scans
make generate-all              # regenerate CRD + DeepCopy and sync the Helm chart
make e2e-local                 # spin up Kind, install via Helm, run e2e smoke tests
```

Run `make generate-all` after any change to `api/v1/` types and commit the result —
CI fails the release if the checked-in CRD/DeepCopy/Helm chart drift from the types.

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
