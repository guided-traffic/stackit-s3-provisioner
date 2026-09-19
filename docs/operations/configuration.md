# Configuration

**The complete key list is not here.** Every chart value and every `Bucket` field is listed once, in
the [README reference](../../README.md), with its default. This page explains the settings whose
consequences are not obvious from that list: how a value reaches the running process, what needs a
restart, what the periodic drift resync actually does and costs, and the one region trap that parks
a whole cluster's `Bucket` resources at once.

Settings that have a page of their own are routed at the [bottom](#where-every-other-setting-is-explained)
rather than explained twice.

---

## Contents

- [How a setting reaches the process](#how-a-setting-reaches-the-process)
- [A minimal configuration](#a-minimal-configuration)
- [Durations carry a unit, always](#durations-carry-a-unit-always)
- [Nothing is hot-reloaded](#nothing-is-hot-reloaded)
- [The region, and the trap under it](#the-region-and-the-trap-under-it)
- [The drift resync interval](#the-drift-resync-interval)
- [Leader election](#leader-election)
- [Where every other setting is explained](#where-every-other-setting-is-explained)
- [Failure modes](#failure-modes)

---

## How a setting reaches the process

Three surfaces exist, and they are not alternatives — they are layers:

| Layer | What it is | Who writes it |
| --- | --- | --- |
| Helm value | a key in `values.yaml` | you |
| Command-line flag | an entry under `args:` in the Deployment | the chart, from the value |
| Environment variable | the fallback a flag reads when it is absent, and for two settings the *only* surface | the chart, for the two settings that have no flag: `POD_NAMESPACE` (downward API) and `CLONE_JOB_RESOURCES` (from `clone.resources`); otherwise nobody |

The chart renders the flags in
[`templates/deployment.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml), and
[`cmd/main.go`](../../cmd/main.go) declares each flag with its environment variable as the flag's
*default*. That produces a precedence rule worth knowing before you reach for an environment
variable:

> **A rendered flag always wins over the environment variable of the same setting.** The environment
> variable is only consulted when the flag is absent from the command line.

Most flags are rendered unconditionally — region, drift resync, provider grace, circuit threshold and
cooldown, the clone image and every `bucketUsage` setting. For those, setting `STACKIT_REGION` or
`DRIFT_RESYNC_INTERVAL` in the pod changes nothing: the flag is already there. The environment
variables exist for running the binary outside this chart, and are documented as an equal surface in
the ADRs for that reason.

It also goes the other way. `POD_NAMESPACE` and `CLONE_JOB_RESOURCES` have no flag at all and reach
the process only as environment variables the chart writes itself: `POD_NAMESPACE` carries the
operator namespace in through the downward API, and `CLONE_JOB_RESOURCES` carries `clone.resources`
in as JSON, which the operator unmarshals into the clone Job's pod resources. That makes
`clone.resources` the one Helm value whose malformed content is a startup exit exactly like the
duration faults below — `invalid CLONE_JOB_RESOURCES JSON`, then the process exits and the pod
crash-loops. Both are rendered in
[`templates/deployment.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml);
`CLONE_JOB_RESOURCES` only when `clone.resources` is non-empty.

A handful of flags are rendered only when their value is non-empty or true — `--bucket-name-prefix`,
`--bucket-name-include-namespace`, `--ownership-name`, `--enable-wipe-on-delete`, `--leader-elect`,
`--stackit-sa-key-path`. Their behaviour is otherwise the same.

### Two settings the chart cannot reach

`--admin-credentials-secret-name` (`stackit-s3-provisioner-admin` # default) and
`--operator-namespace` have **no Helm value**, and the chart ships no `extraArgs` or `extraEnv`
escape hatch — verified across
[`deploy/helm/stackit-s3-provisioner/`](../../deploy/helm/stackit-s3-provisioner/) on 2026-09-19.
Under this chart the admin Secret is therefore always called `stackit-s3-provisioner-admin`, and the
operator namespace is always the release namespace, supplied as `POD_NAMESPACE` through the downward
API. The metrics and health-probe addresses (`:8080`, `:8081`) are likewise hard-coded into the
rendered arguments.

That is not a gap to work around: a `Bucket` may not name the admin Secret in the first place
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D8), and the
operator namespace must be the one the pod runs in for the bootstrap credential to be written at all
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D3/D4).

---

## A minimal configuration

The settings this page explains, as a working `values.yaml`. Everything else has a usable default.

```yaml
# values.yaml — the settings explained on this page

stackit:
  # The region this deployment provisions in. Any string is accepted; nothing
  # validates it against a list of real provider regions, so a typo surfaces as
  # failing provider calls rather than as a startup error (ADR 0005 D2).
  # IMPORTANT: the CRD defaults spec.region to eu01. If you set anything other
  # than eu01 here, every Bucket that does not set spec.region itself is parked
  # as a region mismatch. See "The region, and the trap under it" below.
  region: eu01                       # default
  serviceAccountKey:
    # The Secret holding the service-account key JSON. Leaving this empty is
    # skeleton mode: the operator comes up healthy and provisions nothing.
    secretName: stackit-sa-key       # example
    # Must match the data key inside that Secret.
    secretKey: sa-key.json           # default

# Keep this on. It guarantees that two operator VERSIONS never reconcile at
# once during a rolling update; it is not high availability. Enabling it also
# renders the coordination.k8s.io/leases rule into the operator's ClusterRole.
leaderElection:
  enabled: true                      # default

# How often an already-provisioned Bucket is re-reconciled with no event to
# prompt it. This is what carries a policy change from a new operator version
# onto buckets whose CR nobody touched. Go duration WITH A UNIT — a bare number
# is rejected by the flag parser and the pod crash-loops. "0" disables it and
# leaves the operator purely event-driven.
driftResyncInterval: "10m"           # default

# Extra replicas are warm standbys, never parallelism: non-leaders reconcile
# nothing. Raise it only to shorten failover.
replicaCount: 1                      # default
```

Apply it and confirm the values actually landed — the operator logs its own effective configuration
once, at startup:

```bash
NS=stackit-s3-provisioner-system      # example; the release namespace
kubectl -n $NS logs deploy/stackit-s3-provisioner \
  | grep 'starting stackit-s3-provisioner'
# one line carrying: region, bucketNamePrefix, bucketNameIncludeNamespace, ownershipName,
# enableWipeOnDelete, driftResyncInterval, providerDegradedGrace, providerCircuitThreshold,
# providerCircuitMaxCooldown and the bucketUsage values
```

That line does **not** carry the leader-election state, the clone image or the admin Secret name.
Leader election is confirmed by its lease instead — see [below](#leader-election).

---

## Durations carry a unit, always

`driftResyncInterval`, `providerDegradedGrace`, `providerCircuit.maxCooldown`,
`bucketUsage.interval` and `bucketUsage.minInterval` are parsed by Go's `time.ParseDuration`. A bare
number is not a duration:

```text
invalid value "600" for flag -drift-resync-interval: parse error
```

Go's flag parser then prints the usage block and exits, so the container never reaches the manager
and the pod enters `CrashLoopBackOff`. Reproduced against the current binary on 2026-09-19.

Two consequences follow. First, quote these values and give them a unit — `"10m"`, `"1h"`, `"30s"`.
Second, **nothing catches this before the rollout**: the chart carries no `values.schema.json`, so
`helm template` and `helm install` both accept `600` happily and the fault appears as a crash-looping
pod.

`bucketUsage.pricing.perGBHour` is quoted for a different and much milder reason, and it is worth
separating from the duration fault. Rendering the chart with an unquoted `0.00003697772` (the chart's
default price) on 2026-09-19 produced `--bucket-usage-price-per-gb-hour=3.697772e-05`, and the price
parser accepts that: it is `strconv.ParseFloat`, which reads scientific notation and returned the
same number without an error. The cost estimate is correct either way. Quoting is the chart's
readability convention, not a crash you are avoiding — so if a cost estimate looks wrong, this is not
the cause; see [usage-and-cost.md](usage-and-cost.md).

The complete list of startup faults, their log lines and their fixes is in
[deployment.md](deployment.md#failure-modes).

---

## Nothing is hot-reloaded

Every flag is read once, in `main()`, and copied into the reconcilers before the manager starts.
There is no configuration watch and no SIGHUP path. **Changing any operator setting takes effect on
the next process start, and on no earlier event.**

In practice `helm upgrade` handles most of this for you, because a changed flag changes the pod
template and Kubernetes rolls the Deployment. The cases where it does *not* are the ones that bite:

| What you changed | Does the pod restart? | Effect |
| --- | --- | --- |
| Any value rendered into `args:` — region, drift resync, grace, circuit, naming, ownership, wipe gate, clone image, usage settings | Yes, the pod template changed | Applies after the rollout completes |
| `resources`, `clone.resources`, `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations`, `affinity`, `image.*`, `replicaCount` | Yes | Applies after the rollout completes |
| `leaderElection.enabled` | Yes, and the ClusterRole rule changes with it | See [Leader election](#leader-election) |
| **The contents of the service-account key Secret** | **No** — the Deployment is unchanged | The running process keeps using the key it started with |
| `monitoring.*`, `bucketRoles.create`, `crds.install` | No — they render other objects | Applies immediately; nothing in the operator process depends on them |

The service-account key is the sharp edge. The key file is read once at startup, and project,
credential and region are fixed for the lifetime of the process
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D8). Replacing the Secret's
contents — a rotated key, a key moved to another project — changes nothing until you restart the
operator yourself:

```bash
NS=stackit-s3-provisioner-system      # example; the release namespace
kubectl -n $NS rollout restart deploy/stackit-s3-provisioner
kubectl -n $NS logs deploy/stackit-s3-provisioner \
  | grep 'StackIT client configured'
# expected: the project id from the NEW key, and the configured region
```

Until that restart, an operator whose key was revoked out of band keeps authenticating with a dead
credential. The failure arrives as a structured `400` from the token endpoint rather than the
authorisation error one would expect, and it is one of the errors that is *not* held through the
degraded grace — see [provider-outages.md](provider-outages.md#what-is-not-held).

*Not verified, and this is the gap:* whether the provider SDK re-reads the key file from disk on each
token refresh, which would matter for a Secret updated in place rather than by a rollout. The
operator's own binding is fixed either way, because the project id is parsed once; the ADR states the
rule and the SDK's internal behaviour was not tested.

---

## The region, and the trap under it

One deployment provisions in exactly one region, chosen at install time
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D2). `spec.region` on a
`Bucket` **declares** the region it expects; it never selects one
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D3). When the two
disagree, the operator refuses to provision that CR, makes no provider call for it, and parks it:

```text
status.phase:   Failed
Ready=False     reason: Failed
status.message: spec.region "eu02" does not match this operator's region "eu01"; provisioning is limited to "eu01"
```

The refusal is definitive and is never retried
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D4). Nothing already
provisioned is deleted, renamed or reassigned by it.

**The trap: the CRD defaults `spec.region` to `eu01`.** Verified in
[`config/crd/bases/`](../../config/crd/bases/) on 2026-09-19 — the field carries `default: eu01`, so
a `Bucket` manifest that omits it is stored with `eu01` already written in. If you install the
operator with `stackit.region: eu02`, every such `Bucket` is a region mismatch on arrival, and the
symptom is an entire cluster's worth of CRs in `Failed` carrying the same message.

There are exactly two ways to run a non-`eu01` region, and you need both to be true:

1. set `stackit.region` to that region in the release values; **and**
2. set `spec.region` explicitly on every `Bucket`, because the CRD's default will otherwise
   contradict you.

Verify before you create the second `Bucket`:

```bash
NS=stackit-s3-provisioner-system      # example; the release namespace

# what the operator serves
kubectl -n $NS logs deploy/stackit-s3-provisioner \
  | grep 'starting stackit-s3-provisioner'      # look for "region": "..."

# what your Buckets declare — the REGION column is in the plain table output
kubectl get bkt -A
```

The region is also written into every workload's credentials Secret under `S3_REGION`, where it is
consumed as the SigV4 signing region. That is why the field cannot simply be ignored: provisioning in
one region while telling the workload another produces a signature failure inside somebody else's
application ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md), *Alternatives
considered*).

Any string is accepted. There is no startup validation against a list of known regions, deliberately,
so a new provider region is usable without a release — at the price of a typo surfacing as failing
provider calls rather than as a refusal to start.

---

## The drift resync interval

`driftResyncInterval` (`"10m"` # default) makes a successfully reconciled `Bucket` requeue itself
after that duration. It exists because the `Bucket` watch is filtered on generation and annotation
changes, so an untouched CR produces no event, ever — and a bucket whose CR nobody touches would
otherwise never be looked at again after it went `Ready`.

### What a resync pass actually re-checks

It is a full provisioning pass, not a policy-only check. Against the provider it re-establishes:

| Checked every pass | What happens on a difference |
| --- | --- |
| The bucket still exists under its frozen name | It is re-created if missing |
| Its ownership tags still name this operator and this CR | Ownership collision: the CR is parked ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md), [ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)) |
| The credentials group the bucket's tags attribute still exists | A vanished group is replaced and re-tagged |
| Read grants named in `spec.grantReadAccess` still resolve | Newly resolvable grants are added, unresolvable ones skipped ([read-grants.md](read-grants.md)) |
| The isolation policy matches the document this version builds | Rewritten, and only then ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8) |
| The Secret holds a credential and the group holds at least one key | A fresh key is minted and published ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D4/D5) |

Two things it does *not* re-check, because both are cached in the process after their first success:
whether the object-storage service is enabled, and the operator's own admin credential
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9). The two caches
do **not** behave the same across a restart. A restart re-checks whether the service is enabled — the
cached answer lives in the process, so the first pass after a start asks the provider again. It only
*re-reads* the admin credential from its Secret and never verifies it against the provider
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9), so a restart on
its own reloads exactly the credential that was already there. Replacing a broken admin credential is
therefore a pair of acts and neither half works alone: delete the admin Secret **and then** restart —
the procedure is in
[credentials.md](credentials.md#repair-procedure-replacing-the-admin-credential).

A resync on a `Ready` bucket does not flip its phase back to `Provisioning`: the `Provisioning`
marker is skipped once the operator has already driven the current generation to a terminal phase.
The terminal status update is still issued at the end of every successful pass, so a resync is not
free of API-server traffic either — it is the provider calls below that dominate.

### What a pass costs

Roughly a dozen provider requests per `Bucket` per interval on the steady-state path — **eight
control-plane requests**, two of which are project-wide listings (all buckets, all credentials
groups), and **three S3 requests** (two tag reads and one policy read). Counted by reading the
reconcile path on 2026-09-19; *not verified* against the API by measurement.

Two properties bound what that means for a fleet:

- **The provisioning controller runs one reconcile at a time.** `MaxConcurrentReconciles` is left at
  its default of 1 in `SetupWithManager`, so passes are serialised rather than bursting.
- **Drift resyncs are not rate-limited.** The workqueue rate limiter the operator installs paces
  *error requeues* only; a `RequeueAfter` is enqueued with `AddAfter`, which bypasses it entirely
  ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D9 governs the retry pacing, not
  this path).

So the traffic is steady rather than spiky, but it scales linearly with the number of `Bucket`
resources and inversely with the interval. Lowering the interval on a large fleet is the one change
on this page that can push the operator toward the provider's IP-level rate limit — the failure that
turned a self-healing provider blip into a two-page incident on 2026-09-02, described in
[provider-outages.md](provider-outages.md).

### Choosing a value

| Value | When it is right |
| --- | --- |
| `"10m"` # default | Fits a fleet of tens of buckets. A policy change from an operator upgrade reaches every bucket within ten minutes. |
| A longer interval | A large fleet, or a project where provider request volume is a concern. The cost is how long a manual policy edit or a shipped policy change survives. |
| `"0"` | Event-driven only. Nothing self-heals without an event. |

`"0"` is a bigger switch than it looks. It does not merely slow down policy healing; it removes the
*only* trigger a provisioned, untouched `Bucket` has. A policy shipped in a new operator version
never reaches it, a manually edited bucket policy is never reverted, a deleted credentials group is
never noticed. Use it only when something else re-applies the CRs on a schedule.

### Verify it is running

Every successful pass logs one line, whether or not anything changed:

```bash
NS=stackit-s3-provisioner-system      # example; the release namespace
kubectl -n $NS logs -f deploy/stackit-s3-provisioner \
  | grep 'bucket provisioned'
# one line per Bucket per interval, carrying "bucket", "requested" and "credentialsGroup"
```

If that line stops appearing while `Bucket` resources exist, the resync is off (`"0"`), the operator
is in skeleton mode, or the provider circuit is open — in the last case the operator deliberately
makes no provider call at all until a probe succeeds
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D4).

Skeleton mode has no resync either: a `Bucket` reconciled without a service-account key returns
without a `RequeueAfter`, so it waits for an event
([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D6).

---

## Leader election

`leaderElection.enabled` (`true` # default) renders `--leader-elect` and adds the
`coordination.k8s.io/leases` rule to the operator's ClusterRole; the flag's own Go default is off, so
a deployment that bypasses the chart does not get it. What it buys is that two operator *versions*
never reconcile at once during a rolling update — the full rollout reasoning, the graceful-shutdown
settings that support it and the one upgrade that can still race are in
[deployment.md](deployment.md#leader-election-replicas-and-rolling-updates).

The configuration consequence that belongs here: **the lease identity is a constant, not a
release-scoped name.** It is `stackit-s3-provisioner.stackit-bucket.gtrfc.com`, hard-coded in
[`cmd/main.go`](../../cmd/main.go), and the lease is created in the pod's own namespace because
`LeaderElectionNamespace` is left unset. Two releases of this chart installed into the *same*
namespace therefore contend for one lease and elect a single leader between them, and the loser
provisions nothing while looking entirely healthy. Two deployments in two namespaces are unaffected.

Running more than one deployment against one project is separately forbidden, for a harsher reason:
both bootstrap the same project-wide admin identity and the second install destroys the first one's
admin credential ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D9).

Verify:

```bash
NS=stackit-s3-provisioner-system      # example; the release namespace
# the lease NAME below is a constant, not release-scoped
kubectl -n $NS get lease stackit-s3-provisioner.stackit-bucket.gtrfc.com \
  -o jsonpath='{.spec.holderIdentity}{"\n"}'
# expected: the name of the pod that is currently reconciling
```

---

## Where every other setting is explained

Each block below is explained on exactly one page. The complete key list, with every default, is in
the [README reference](../../README.md) and nowhere else.

| Values block | What it controls | Explained in |
| --- | --- | --- |
| `stackit.serviceAccountKey` | The project binding and the account setup behind it | [prerequisites.md](prerequisites.md) |
| `crds`, `image`, `replicaCount`, `resources`, scheduling keys | Install, upgrade, uninstall | [deployment.md](deployment.md) |
| `bucketRoles` | Who may create a `Bucket`, and what that delegates | [deployment.md](deployment.md), [rbac-and-privilege.md](../security/rbac-and-privilege.md) |
| `bucketNaming`, `ownership` | The physical bucket name and the ownership identity a restore must reproduce | [bucket-naming.md](bucket-naming.md) |
| `wipeOnDelete` | The destructive gate, and the two other authorizations it needs | [deletion.md](deletion.md#the-wipe-path) |
| `providerDegradedGrace`, `providerCircuit` | What the operator does while the provider is unreachable | [provider-outages.md](provider-outages.md) |
| `bucketUsage` | Size measurement, its guards and the cost estimate | [usage-and-cost.md](usage-and-cost.md) |
| `clone` | The clone job's image, resources and network policy | [cloning.md](cloning.md) |
| `monitoring` | What each metric and each alert means, and how they are tuned against each other | [monitoring.md](monitoring.md) |
| `spec.secretRef.keys` on a `Bucket` | The Secret data-key contract and the collision fault | [credentials.md](credentials.md#the-workload-secret) |
| `spec.grantReadAccess` on a `Bucket` | Sharing a bucket read-only with a sibling | [read-grants.md](read-grants.md) |

---

## Failure modes

The faults that belong to the settings on this page. Startup faults are deliberate `os.Exit` calls:
the pod crash-loops with the reason in its log rather than running half-configured.

| What you see | What it means | What to do |
| --- | --- | --- |
| `CrashLoopBackOff`; log ends with `invalid value "600" for flag -drift-resync-interval: parse error` and a usage block | A duration value without a unit. Nothing validates it before the rollout. | Quote it and give a unit: `driftResyncInterval: "10m"`. |
| Every `Bucket` in `Failed` with `spec.region "…" does not match this operator's region "…"` | `stackit.region` and the CRD's `eu01` default for `spec.region` disagree. Definitive, never retried ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D3/D4). | Either set `stackit.region: eu01`, or set `spec.region` explicitly on every `Bucket`. |
| A new operator version's policy has not reached existing buckets | The `Bucket` watch does not fire for untouched CRs; only the drift resync does ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8). | Wait one `driftResyncInterval`. Confirm it is not `"0"`. **Never delete a `Bucket` CR to force it** — deletion is a teardown ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)). |
| No `bucket provisioned` log line for minutes, `Bucket` resources exist | `driftResyncInterval: "0"`, skeleton mode, or an open provider circuit. | Check the startup log line for the interval; check `stackit_s3_provisioner_skeleton_mode` and `stackit_s3_provisioner_provider_circuit_open` ([monitoring.md](monitoring.md)). |
| A rotated service-account key still fails after `helm upgrade` | Replacing the Secret's contents does not change the Deployment, so the pod never restarted and still holds the old key ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D8). | `kubectl rollout restart` the Deployment, then confirm the project id in the `StackIT client configured` log line. |
| Provider answers `rate limit on IP level exceeded` during normal operation | Steady drift-resync traffic scaled past the provider's per-IP limit. Resync requeues are not paced by the workqueue rate limiter. | Raise `driftResyncInterval`. Background and the 2026-09-02 incident: [provider-outages.md](provider-outages.md). |
| Two operator pods **from two different releases** in one namespace, only one ever reconciles (for one release, this is the intended `replicaCount: 2` behaviour and not a fault) | The leader-election lease identity is a constant, not release-scoped, so both releases contend for one lease. | Install each release into its own namespace. |
| `leases.coordination.k8s.io is forbidden` in the operator log | `--leader-elect` is on the command line while the `leases` RBAC rule is absent. **The chart cannot produce this**: both are gated on `leaderElection.enabled`, so it means the Deployment was patched out of band or the binary runs outside this chart. | Put the setting back under the Helm value, or add the `coordination.k8s.io/leases` rule to whatever role the process runs with. |
| `CrashLoopBackOff`; log ends with `invalid CLONE_JOB_RESOURCES JSON` | `clone.resources` is the one Helm value that reaches the process as JSON in an environment variable, and it did not unmarshal into pod resource requirements. | Fix the block so it is a valid `resources:` mapping (`requests`/`limits` with quantity strings); see [cloning.md](cloning.md). |
| An environment variable you set in the pod has no effect | The chart renders the matching flag unconditionally, and a flag beats its environment default. | Set the Helm value instead. |

---

## Related

- [README reference](../../README.md) — the complete key list, every value with its default
- [prerequisites.md](prerequisites.md) — the account and key that must exist first
- [deployment.md](deployment.md) — install, upgrade, uninstall, and the full startup-fault table
- [provider-outages.md](provider-outages.md) — the grace window and the circuit breaker
- [bucket-naming.md](bucket-naming.md) — the naming and ownership settings
- [monitoring.md](monitoring.md) — the metrics and alerts named on this page
- [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) — one project, one region,
  and a binding fixed for the life of the process
- [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the policy the drift
  resync re-asserts
