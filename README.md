# stackit-s3-provisioner

![Coverage](https://raw.githubusercontent.com/guided-traffic/stackit-s3-provisioner/main/.github/badges/coverage.json)
![Go Version](https://img.shields.io/badge/go-1.26-blue)
![License](https://img.shields.io/badge/license-Apache--2.0-green)

A Kubernetes operator that provisions **StackIT Object Storage** buckets, workload
credentials and isolation policies through Custom Resources. One operator
deployment per cluster, bound to a single StackIT project via a service-account key.

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
