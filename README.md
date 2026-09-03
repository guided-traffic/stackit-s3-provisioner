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
structurally guaranteed by StackIT itself. A bucket can optionally
[share itself read-only](#sharing-a-bucket-read-only) with other Buckets in its
namespace. See [`CLAUDE.md`](CLAUDE.md) and [`INIT-SETUP.md`](INIT-SETUP.md) for
the architecture and security invariants.

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
NAME        BUCKET      PHASE   READY   STATUS               REGION   SIZE       COST/MONTH   AGE
my-bucket   my-bucket   Ready   True    provisioned          eu01     18.0 GiB   0.53 EUR     2m
```

`SIZE` and `COST/MONTH` stay empty unless
[size measurement](#bucket-size-and-monthly-cost) is switched on for the Bucket.

- **`status.phase`** — `Pending` → `Provisioning` → `Ready`, or `Failed` /
  `Deleting`.
- **`Ready` condition** — reasons `Provisioned`, `Provisioning`, `Failed`, or
  `NotImplemented` (skeleton mode). `status.message` carries the current step or
  failure reason.
- Config faults (a `secretRef` pointing at the operator admin Secret, a
  `spec.region` that differs from the operator's region, a bucket-name/secret-key
  collision, or a bucket owned by someone else) set `Ready=Failed` **without**
  requeue-hammering — fix the CR and the next generation reconciles.
- **`ProviderReachable` condition** — `False` while a provisioned Bucket's
  `Ready` state is being *held* through repeated provider failures, see
  [Ready during provider outages](#ready-during-provider-outages). Absent on a
  healthy Bucket.
- Other status fields: `resolvedBucketName`, `bucketURL`, `credentialsGroupID`,
  `credentialsGroupURN`, `accessKeyID` (never the secret), `observedGeneration`,
  `operatorVersion`, `degradedSince` (see
  [Ready during provider outages](#ready-during-provider-outages)),
  `grantedReadTo` (see
  [Sharing a bucket read-only](#sharing-a-bucket-read-only)), `clone` (see
  [Cloning an existing bucket](#cloning-an-existing-bucket)),
  `lastRotationTrigger` / `lastRotationTime` (see
  [Credentials rotation](#credentials-rotation)), `usage` (see
  [Bucket size and monthly cost](#bucket-size-and-monthly-cost)).

Each `Bucket` is stamped with S3 ownership tags (`managed-by` + `owner=<ns>/<name>`)
so the operator adopts only buckets it owns and refuses to clobber a pre-existing
foreign or non-empty bucket. On bootstrap the operator creates a shared
`operator-admin` credentials group + S3 key (persisted in its own admin Secret,
default `stackit-s3-provisioner-admin`); that group's URN sits in every bucket
policy's exemption list as a lockout safeguard.

## Ready during provider outages

`Ready` on a provisioned `Bucket` describes the **last verified state of the
bucket**, not the outcome of the last attempt to verify it. When a reconcile of
an already-`Ready` Bucket fails for a reason that says nothing about the bucket —
the StackIT API unreachable, a gateway or WAF answering with an error page, a
Kubernetes API blip — the operator keeps `Ready=True` and records the
degradation instead:

```
NAME        PHASE   READY   STATUS                             AGE
my-bucket   Ready   True    ensure bucket: unexpected EOF       3h
```

```yaml
status:
  phase: Ready
  degradedSince: "2026-08-25T08:13:04Z"   # when the failures started
  conditions:
    - type: Ready
      status: "True"                      # held: last VERIFIED state
      reason: Provisioned
    - type: ProviderReachable
      status: "False"
      reason: ProviderUnreachable
      message: 'ensure bucket: unexpected EOF'
```

Why: without this, one failed control-plane call flips a healthy Bucket to
`Failed` immediately. A short provider blip therefore marked every Bucket on the
cluster non-ready at once, which cascaded into everything health-checking them
(Flux `Kustomization` health checks in particular) and produced a cluster-wide
alert storm out of a two-minute outage.

The hold is **bounded** by `providerDegradedGrace` (default `30m`). Once it
elapses the Bucket drops to `Failed` exactly as before, so a real outage still
becomes visible in the Bucket's own status — the window only decides how fast.

What is **not** held, and drops `Ready` immediately regardless of the grace:

| Case | Why |
| ---- | --- |
| A structured `400`/`401`/`403` **from the provider** | The provider refusing the request, not failing to answer. `401`/`403` is the Object Storage API; **`400` is how a revoked service-account key surfaces** — the key flow never reaches the API, and the token endpoint answers `400 invalid_grant`. A gateway error page carrying any of those codes has a non-JSON body and *is* held. |
| A workload credential the operator destroyed | The old access key was deleted and the replacement could not be published (a rotation or a re-created Secret hitting a provider failure). The operator knows the published credential is dead — that is local certainty, not an unverifiable provider state. |
| Config faults (the `failNoRequeue` family above) | Statements about *this* Bucket that the operator established locally. |
| A Bucket that has never been `Ready` | There is no verified state to defend; initial provisioning failures surface at once. |
| A Bucket whose spec changed (`observedGeneration != generation`) | The user asked for something new and it was not achieved. |
| A Bucket being deleted | Holding `Ready` would hide a teardown blocked by the non-empty data-loss guard. |

The `Warning` events and `status.message` are unchanged, so the reason a Bucket
is held is always visible on the object. What is **not** emitted once per retry
any more is the reconcile error — see
[the provider circuit breaker](#provider-circuit-breaker) below.

```yaml
# values.yaml
providerDegradedGrace: "30m"   # default; "0" disables the hold entirely
```

Setting it to `"0"` restores the previous behavior without deploying a different
image.

> Trade-off: while `Ready` is held, a bucket that really did break stays green
> for up to the grace window. That is deliberate — a delayed signal is bounded
> and recoverable, whereas marking the whole fleet unhealthy on the first blip is
> neither.

### Provider circuit breaker

A provider outage is a property of the provider, not of any one Bucket. While
the StackIT API answers `503`, reconciling the seventeenth Bucket cannot succeed
— and attempting it costs API calls that make the outage worse.

Measured on a production cluster on 2026-09-02: a provider-side `503` storm
starting at 14:23 drove the operator into
`429 rate limit on IP level exceeded` by 14:34 and into 51 rate-limited requests
per minute by 14:42. By the end, most of the load on the failing endpoint was the
operator retrying. One outage that resolved on its own produced **242 reconcile
errors** and paged twice for something nobody could act on.

The breaker stops that. After `providerCircuit.threshold` reconciles fail back to
back, the operator stops calling the provider entirely, holds every Bucket, and
probes on a doubling cooldown (60s, 2m, 4m … capped at
`providerCircuit.maxCooldown`) until one succeeds.

```yaml
# values.yaml
providerCircuit:
  threshold: 3      # consecutive failures before the operator stops calling out
  maxCooldown: "5m" # upper bound on the wait between probes
```

What distinguishes an outage from one broken Bucket is the **absence of any
successful reconcile** in between, not a classification of the error. A single
failing Bucket among healthy ones has its failures interleaved with the successes
of the rest of the fleet, which resets the run — so it keeps reporting its own
error and never holds anyone else.

While the circuit is open:

| | |
| --- | --- |
| Provider calls | none at all, including teardown (the finalizer stays, the delete resumes on the next probe) |
| Reconcile result | `RequeueAfter` on the breaker's cooldown, **no error** |
| `controller_runtime_reconcile_errors_total` | counts the outage (`threshold` errors), not the retries |
| Bucket status | held per `providerDegradedGrace`; `status.degradedSince` is written once, not per probe |
| Metrics | `stackit_s3_provisioner_provider_circuit_open`, `..._provider_circuit_opened_timestamp_seconds` |
| Alerting | `StackitS3ReconcileErrors` excludes windows in which the circuit was open; the outage surfaces via `StackitS3BucketProviderDegraded` once the hold has lasted long enough that the drop to `Failed` is close |

The grace is **not** extended by the breaker: a Bucket held past
`providerDegradedGrace` still drops to `Failed`. The breaker delays reconciles,
it does not widen the window in which a Bucket may advertise a state nobody
verified.

Setting `providerCircuit.threshold: 0` disables the breaker without deploying a
different image.

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
    addressingStyle: path                          # optional: path (default) or
                                                   # virtual-hosted (AWS style)
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

**Addressing style.** The source is addressed path-style by default
(`endpoint/bucket` — the norm for S3-compatible services like StackIT, MinIO or
Ceph). For sources that prefer or require virtual-hosted addressing
(`bucket.endpoint` — AWS's recommended style), set
`cloneFrom.addressingStyle: virtual-hosted`. The destination (StackIT) always
stays path-style.

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

## Sharing a bucket read-only

By default a bucket is reachable by exactly one credential: its own. A bucket
can additionally grant **read-only** access to other `Bucket` CRs in the **same
namespace** via `spec.grantReadAccess`. The grant is declared on the bucket that
owns the data, so a bucket's full access list is visible in its own spec:

```yaml
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: gitlab-artifacts
  namespace: gitlab
spec:
  bucketName: gitlab-artifacts
  secretRef:
    name: gitlab-artifacts-s3
  grantReadAccess:            # optional, default: no additional access
    - name: gitlab-backups    # metadata.name of a Bucket CR in namespace gitlab
```

The credentials in `gitlab-backups-s3` can now list `gitlab-artifacts` and get
its objects. They still cannot write to it, delete from it, or touch its
configuration.

| Granted to a reader | Denied to a reader |
| ------------------- | ------------------ |
| `ListBucket`, `ListBucketVersions` | every `Put*`, `Delete*` and `Create*` action |
| `GetObject`, `GetObjectVersion` | multipart listing/abort (would expose or destroy the owner's in-flight uploads) |
| `GetObjectTagging`, `GetObjectVersionTagging` | bucket policy, replication, notifications, lifecycle, object lock |
| `GetBucketLocation`, `GetBucketVersioning`, `GetBucketObjectLockConfiguration` | anything at all on buckets that did not grant it |

Rules worth knowing:

- **Namespace-scoped.** Entries name a `Bucket` CR and are resolved in the
  granting Bucket's own namespace, so a Bucket in another namespace cannot be
  named here and a same-named Bucket elsewhere resolves to a different
  credentials group. Note that the principal written into the policy is located
  by the operator's derived credentials-group name, which is what already decides
  which group a Bucket owns — a namespace allowed to create `Bucket` resources is
  inside the trust boundary either way.
- **Never blocking.** A referenced Bucket that does not exist yet (or is not
  finished provisioning) is skipped with a `ReadGrantPending` warning event; the
  granting bucket still becomes `Ready`. The grant is applied automatically as
  soon as the reference resolves.
- **Revocation is automatic.** Deleting a referenced Bucket removes it from the
  policy on the granting bucket's next reconcile. Removing the entry from
  `spec.grantReadAccess` does the same, immediately.
- **Self-references are rejected** by the CRD schema.
- **A bucket being filled by a clone shares nothing yet.** While
  [`spec.cloneFrom`](#cloning-an-existing-bucket) is still copying, granted
  readers stay out of the policy and are added the moment the copy succeeds — the
  same reason the bucket's own credentials Secret is held back by default.
- **An ambiguous reference grants nothing.** If the credentials-group name a
  reference resolves to exists more than once in the StackIT project, the grant
  is refused with a `ReadGrantPending` event rather than pointed at a guess.
- Whatever is currently in effect is listed in `status.grantedReadTo`:

  ```console
  $ kubectl get bucket gitlab-artifacts -o jsonpath='{.status.grantedReadTo}'
  ["gitlab-backups"]
  ```

Leaving `grantReadAccess` unset keeps the previous behavior exactly — the
bucket policy is then identical to that of a bucket that never used the feature.

## Bucket size and monthly cost

The operator can measure each bucket's size at a configurable interval and write
it — plus an estimated monthly storage cost — onto the `Bucket` CR, so both show
up as columns in `kubectl get bkt` and in Lens:

```
NAME        BUCKET      PHASE   READY   STATUS        REGION   SIZE       COST/MONTH   AGE
my-bucket   my-bucket   Ready   True    provisioned   eu01     18.0 GiB   0.53 EUR     2d
```

**Measurement never blocks provisioning.** It runs in its own controller with its
own work queue and concurrency limit, so a slow measurement delays at most other
measurements. A failed measurement keeps the previous values, does **not** touch
the `Ready` condition and does **not** count as a reconcile error.

### What it costs

STACKIT bills Object Storage **per started gigabyte per started hour** and its
price list contains **no** request, operation or traffic position, so measuring
costs no money (verified against the STACKIT price list v1.0.43 and the Object
Storage Leistungsschein v1.2, see [`INIT-SETUP.md` §8.3](INIT-SETUP.md)).

It costs **time**: the control-plane API exposes no usage endpoint, so the only
way to obtain a size is to list the bucket — roughly one request per 1000 object
keys. A bucket with 2 million objects is ~2000 requests per pass, every pass.
Two operator-wide guards bound that, and `status.usage.measurementDuration`
tells you what a pass actually costs on your data before you lower the interval.

### Operator-wide configuration

```yaml
# values.yaml
bucketUsage:
  enabled: true            # default; HARD gate — false disables measurement everywhere
  defaultEnabled: false    # default; applies to Buckets that do not decide themselves
  interval: "60m"          # default; for Buckets that do not request their own
  minInterval: "60m"       # default; floor — a Bucket asking for less is clamped up
  maxObjects: 2000000      # default; abort cap, reports a LOWER BOUND beyond it
  includeVersions: false   # default; count non-current versions and delete markers
  concurrency: 2           # default; buckets measured in parallel
  pricing:
    perGBHour: "0.00003697772"   # default: STACKIT list price Object Storage Premium-EU01
    currency: "EUR"              # default; display label only, no conversion
```

Two switches, not one:

- **`enabled`** is the kill switch. With `false` nothing is measured, whatever a
  Bucket asks for; a Bucket that asked explicitly gets the warning event
  `UsageMeasurementDisabled` and a note in `status.usage.message`.
- **`defaultEnabled`** is the cluster-wide policy for Buckets that do not set
  `spec.usage.enabled`. It ships **off**, so measurement is opt-in.

⚠️ **`minInterval` and `maxObjects` are cost-of-time guards, and both bite.** A
Bucket asking for a shorter interval is clamped up (warning event
`UsageIntervalClamped`); a bucket with more objects than `maxObjects` is measured
only partially and every value it reports — size *and* cost — becomes a lower
bound, marked with a `>=` prefix and the warning event
`UsageMeasurementTruncated`. Raise the cap deliberately: the pass gets
proportionally longer, not more expensive.

⚠️ **`pricing.perGBHour` is a LIST price, not your contract.** The default is the
public STACKIT list price for Object Storage Premium-EU01 (price list v1.0.43,
08/04/2026, net, excluding taxes; EU02 was `0.00003883000`). Nothing verifies it.
Put your own price there, or set it to `""` to switch the estimate off entirely.
Quote the value as a **string** so YAML does not turn it into an exponent.

### Per-Bucket overrides

Every field is optional; an omitted field follows the operator-wide default, so a
cluster-wide policy change reaches the Bucket without editing it.

```yaml
spec:
  bucketName: my-bucket
  usage:
    enabled: true          # override defaultEnabled in either direction
    interval: "6h"         # Go duration WITH a unit; clamped up to minInterval
    includeVersions: true  # count non-current versions and delete markers too
```

- `enabled: false` switches measurement off for this Bucket **and clears
  `status.usage`**, so a size and a cost nobody refreshes any more are not left
  on display.
- `interval` must be a Go duration with a unit (`"30m"`, `"6h"`). A bare number
  is rejected by the CRD at admission time.
- `includeVersions` matters on a **versioned** bucket: non-current versions and
  delete markers occupy billed storage but never appear in a plain object
  listing, so the default understates both size and cost there.

### Status fields

```yaml
status:
  usage:
    bytes: 19327352832              # current objects
    objects: 4213
    versionBytes: 0                 # non-current versions (only with includeVersions)
    versionObjects: 0
    billableBytes: 19327352832      # bytes + versionBytes — what the cost is based on
    humanReadable: 18.0 GiB         # ">= 18.0 GiB" when the measurement was capped
    estimatedMonthlyCost: 0.53 EUR  # ">= ..." when the measurement was capped
    estimatedMonthlyCostCents: 53   # canonical value; the estimate is cent-rounded
    currency: EUR
    lastMeasurementTime: "2026-09-01T10:15:00Z"
    measurementDuration: 4.213s     # the honest price of the configured interval
    truncated: false                # true = every value above is a lower bound
    message: ""                     # why no fresh measurement, or a config note
```

How the estimate is computed, because it is an **estimate and not an invoice**:
`billableBytes` is rounded **up** to a whole decimal gigabyte (the billing metric
is per *started* gigabyte), priced for **720 hours** (the 30-day month STACKIT's
own price list projects with) and rounded to whole cents. It prices the size
measured at *one* moment as if it were held all month, uses the price the
operator is configured with rather than your contract, covers storage only, and
excludes taxes.

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

## Monitoring

The operator serves Prometheus metrics on `:8080` (`--metrics-bind-address`) —
the standard controller-runtime and Go collectors plus its own metrics:

| Metric | Type | Meaning |
| ------ | ---- | ------- |
| `stackit_s3_provisioner_buckets{phase}` | gauge | Number of `Bucket` resources per `status.phase` (`Pending`, `Provisioning`, `Ready`, `Failed`, `Deleting`; `Unknown` for CRs without a status yet). All phases are always exported. |
| `stackit_s3_provisioner_buckets_clone{phase}` | gauge | Number of `Bucket` resources per clone phase (`Running`, `Completed`, `Failed`); only Buckets with a clone are counted |
| `stackit_s3_provisioner_buckets_wipe_on_delete` | gauge | Number of `Bucket` resources with `spec.wipeOnDelete: true` |
| `stackit_s3_provisioner_buckets_provider_degraded` | gauge | Number of `Bucket` resources whose `Ready` state is being [held through provider failures](#ready-during-provider-outages) |
| `stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds{namespace,name}` | gauge | Unix time at which this Bucket started degrading; **absent** for Buckets that are not degraded — so `time() - <series>` is the age of the degradation wherever the series exists |
| `stackit_s3_provisioner_provider_circuit_open` | gauge | `1` while the [provider circuit breaker](#provider-circuit-breaker) is open and the operator is not calling StackIT at all. Always present, so an alert expression never races an absent series. |
| `stackit_s3_provisioner_provider_circuit_opened_timestamp_seconds` | gauge | Unix time at which the circuit last opened; **absent** while closed — so `time() - <series>` is the age of the outage. It does not move when a probe fails, so it measures the outage rather than the last probe interval. |
| `stackit_s3_provisioner_skeleton_mode` | gauge | `1` while the operator runs without a StackIT service-account key (provisions nothing) |
| `stackit_s3_provisioner_wipe_on_delete_gate_enabled` | gauge | `1` while the operator-wide `--enable-wipe-on-delete` feature gate is on |
| `stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds{namespace,name}` | gauge | Unix time of the Bucket's last [credentials rotation](#credentials-rotation); absent for never-rotated Buckets |
| `stackit_s3_provisioner_bucket_size_bytes{namespace,name}` | gauge | Size of the Bucket's current objects at the last [measurement](#bucket-size-and-monthly-cost); **absent** for never-measured Buckets, so `absent()` distinguishes "not measured" from "empty" |
| `stackit_s3_provisioner_bucket_objects{namespace,name}` | gauge | Number of current objects at the last measurement |
| `stackit_s3_provisioner_bucket_version_size_bytes{namespace,name}` | gauge | Size of non-current object versions; `0` unless `includeVersions` is on |
| `stackit_s3_provisioner_bucket_version_objects{namespace,name}` | gauge | Number of non-current versions and delete markers; `0` unless `includeVersions` is on |
| `stackit_s3_provisioner_bucket_billable_size_bytes{namespace,name}` | gauge | The size the cost estimate is computed from (current objects + counted versions) |
| `stackit_s3_provisioner_bucket_estimated_monthly_cost{namespace,name,currency}` | gauge | Estimated monthly storage cost in whole currency units; absent when no price is configured |
| `stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds{namespace,name}` | gauge | Unix time of the last successful measurement — `time() - <series>` is the age of the reported size |
| `stackit_s3_provisioner_bucket_usage_truncated{namespace,name}` | gauge | `1` while the Bucket's last measurement hit `maxObjects`, i.e. its size and cost are lower bounds |
| `stackit_s3_provisioner_buckets_usage_measured` | gauge | Number of `Bucket` resources carrying a size measurement |
| `stackit_s3_provisioner_usage_measurement_gate_enabled` | gauge | `1` while the operator-wide `bucketUsage.enabled` gate is on |
| `stackit_s3_provisioner_usage_measurement_failures_total` | counter | Measurements that failed. Measurement failures deliberately do not surface as reconcile errors, so this is the only place they aggregate. |
| `stackit_s3_provisioner_usage_measurement_duration_seconds` | histogram | Duration of a successful measurement (one full listing pass) — the number to check before lowering the interval |

All gauges are computed live from the cluster state on every scrape, so they
never drift. The two `usage_measurement_*` process metrics are the exception:
a failed measurement leaves no trace on the CR beyond a message, and the duration
of a pass is gone once it finished, so both are tracked in the process. For clusters running the
[prometheus-operator](https://github.com/prometheus-operator/prometheus-operator)
stack (e.g. kube-prometheus-stack), the chart can ship the scrape config and
alerting rules — both **disabled by default** because they require the
`monitoring.coreos.com` CRDs, and installation would fail on clusters without
them:

```yaml
# values.yaml
monitoring:
  serviceMonitor:
    enabled: true          # renders a metrics Service + ServiceMonitor
    interval: 30s
    scrapeTimeout: ""      # empty = Prometheus default
    labels: {}             # extra ServiceMonitor labels, e.g. release: kube-prometheus-stack
  prometheusRule:
    enabled: true
    labels: {}             # extra PrometheusRule labels
    alerts:                # every alert has its own toggle, all default to enabled
      bucketsWipeOnDelete:          { enabled: true }
      bucketFailed:                 { enabled: true }
      bucketStuckProvisioning:      { enabled: true }
      bucketStuckDeleting:          { enabled: true }
      cloneFailed:                  { enabled: true }
      skeletonMode:                 { enabled: true }
      wipeRequestedButGateDisabled: { enabled: true }
      reconcileErrors:              { enabled: true }
      bucketProviderDegraded:       { enabled: true }
      usageMeasurementFailing:      { enabled: true }
      usageMeasurementTruncated:    { enabled: true }
```

Some kube-prometheus-stack installs only discover `ServiceMonitor`/
`PrometheusRule` objects carrying a specific label (typically
`release: <helm-release-name>`); set it via the `labels` values above.

Shipped alerts — every toggle lives under `monitoring.prometheusRule.alerts.<name>.enabled`:

| Alert | Toggle | Severity | Fires when |
| ----- | ------ | -------- | ---------- |
| `StackitS3BucketFailed` | `bucketFailed` | `warning` | a `Bucket` sits in phase `Failed` for 15m. Config faults deliberately park without requeueing — without this alert nobody notices them. |
| `StackitS3BucketStuckProvisioning` | `bucketStuckProvisioning` | `warning` | Buckets sit in `Pending`/`Provisioning` for 30m (StackIT API problems, quota, a long-running clone) |
| `StackitS3BucketStuckDeleting` | `bucketStuckDeleting` | `warning` | a finalizer teardown hangs for 30m — usually the non-empty data-loss guard blocking [deletion](#deletion-behavior) |
| `StackitS3CloneFailed` | `cloneFailed` | `warning` | a [bucket clone](#cloning-an-existing-bucket) stays `Failed` for 30m despite backoff retries |
| `StackitS3SkeletonMode` | `skeletonMode` | `critical` | the operator runs without a service-account key for 15m: probes stay green, nothing is provisioned |
| `StackitS3BucketsWipeOnDelete` | `bucketsWipeOnDelete` | `warning` | at least one `Bucket` carries `spec.wipeOnDelete: true` for 5m — deleting such a CR irreversibly wipes all objects in its bucket |
| `StackitS3WipeRequestedButGateDisabled` | `wipeRequestedButGateDisabled` | `warning` | Buckets request `spec.wipeOnDelete` while the operator-wide gate (`wipeOnDelete.enabled`) is off — deletion would silently degrade to the empty-only guard |
| `StackitS3ReconcileErrors` | `reconcileErrors` | `warning` | more than `threshold` (default `6`) reconcile errors within 15m, sustained for `sustainedFor` (default `15m`), **excluding** windows in which the [provider circuit breaker](#provider-circuit-breaker) was open. A StackIT outage therefore cannot reach this alert; what is left is errors the breaker never recognised — Kubernetes API failures, or single Buckets failing while the fleet reconciles fine. Set `suppressWhileCircuitOpen: false` to alert on provider outages here too. |
| `StackitS3BucketProviderDegraded` | `bucketProviderDegraded` | `warning` | a Bucket's `Ready` state has been [held through provider failures](#ready-during-provider-outages) for longer than `holdForSeconds` (default `1200` = 20m, i.e. 10m before the default 30m grace runs out). It alerts on the **age** of the hold, not its existence: a provider blip that resolves inside the window is exactly what the hold is for. `holdForSeconds` must stay below `providerDegradedGrace`, or the Bucket drops to `Failed` — and the series disappears — before the alert can fire. |
| `StackitS3UsageMeasurementFailing` | `usageMeasurementFailing` | `warning` | more than 3 [size measurements](#bucket-size-and-monthly-cost) failed within 30m. A failed measurement keeps the previous values and never marks a Bucket unhealthy, so without this alert the sizes and costs on the CRs go stale silently. |
| `StackitS3UsageMeasurementTruncated` | `usageMeasurementTruncated` | `warning` | a Bucket keeps hitting `bucketUsage.maxObjects` for 30m: its reported size and cost are lower bounds, which is exactly the state in which a size-based alert under-reports |

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
make e2e-stackit               # same, but with a REAL StackIT key: creates and deletes real
                               # buckets, credentials groups, access keys and clone Jobs
make e2e-stackit-sweep-dry     # report cloud leftovers from an aborted e2e run
```

`make e2e-stackit` needs a service-account key (`SA_KEY`, default `account-1.json`) and
provisions against the real API — including a bucket clone with the real rclone image and
periodic size measurement. It tears every Bucket down through the finalizer, then sweeps
the project for leftovers; `make e2e-stackit-sweep` is the manual backstop after a crash.

Run `make generate-all` after any change to `api/v1/` types and commit the result —
CI fails the release if the checked-in CRD/DeepCopy/Helm chart drift from the types.

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
