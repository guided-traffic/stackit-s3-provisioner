# Monitoring

What the operator exports, what each series means, which alerts the chart ships,
and the tuning constraints that decide whether those alerts are useful or noise.

The operator serves Prometheus metrics on `:8080` and health probes on `:8081`.
Neither endpoint is configurable through the chart: `deployment.yaml` pins
`--metrics-bind-address=:8080` and `--health-probe-bind-address=:8081`. The
metrics `Service` the chart renders publishes `8080` and the `ServiceMonitor`
selects it by the port **name** `metrics` — the `Service`'s `targetPort` resolves
that name against the container port of the same name in `deployment.yaml`. Both
ends of that name have to match if either is hand-edited. The flags exist on the
binary, so a hand-rolled deployment can move the ports; there is no Helm value
for either.

This page explains the handful of `monitoring.*` values it is about. The
complete configuration surface — every `Bucket` field and every Helm value —
lives in the [README reference](../../README.md).

**The metrics endpoint is plain HTTP with no authentication and no TLS.** The
manager is started with `metricsserver.Options{BindAddress: metricsAddr}` and no
filter provider, so anything that can reach the pod on `8080` can read the
fleet's bucket names, namespaces, sizes and cost estimates. There is no secret
material in any series. The chart ships no `NetworkPolicy` restricting ingress
to `8080` — the only policy it renders protects the clone job's remote-control
port. Verified in `cmd/main.go` and
`deploy/helm/stackit-s3-provisioner/templates/networkpolicy.yaml`.

**The health probes say nothing about the provider.** Both are registered as
`healthz.Ping`: they report that the process is alive, not that the StackIT API
is reachable or that a service-account key was ever loaded. That is precisely
why `StackitS3SkeletonMode` exists and why it is one of only three `critical`
alerts in the set — an operator with no key comes up, passes both probes forever
and provisions nothing. The other two report the other things a green probe says
nothing about: `StackitS3BucketMissing`, a provisioned bucket the provider answers
for as gone ([vanished-buckets.md](vanished-buckets.md)), and
`StackitS3SaKeyExpiringCritical`, the service-account key the process runs on
being days from expiry — when it does expire the provider's refusal is definitive,
so it is not held through the degraded grace and every `Bucket` drops to `Failed`
at once ([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)).

---

## Scraping the operator

### With the prometheus-operator stack

```yaml
# values.yaml — the monitoring block of a working configuration
monitoring:
  serviceMonitor:
    # Renders BOTH the metrics Service and the ServiceMonitor that selects it.
    # Off by default because it needs the monitoring.coreos.com CRDs, and a
    # release into a cluster without them fails at apply time.
    enabled: true                      # false # default
    interval: 30s                      # default
    scrapeTimeout: ""                  # default; empty = the Prometheus default
    # Many kube-prometheus-stack installs only discover ServiceMonitor and
    # PrometheusRule objects carrying the release label of the monitoring stack.
    # Without it the object is created, is valid, and is silently never read.
    labels:
      release: kube-prometheus-stack   # example — match YOUR stack's selector
  prometheusRule:
    enabled: true                      # false # default
    labels:
      release: kube-prometheus-stack   # example — same selector as above
    # alerts.<name>.enabled toggles each rule individually; fifteen of the
    # sixteen default to true and bucketRecreated is the one that ships off.
    # The whole PrometheusRule is skipped when every toggle is false, because
    # prometheus-operator rejects a rule group with no rules.
```

Both switches default to `false` for the same reason: the chart would otherwise
fail to install on any cluster without the `monitoring.coreos.com` CRDs.

The discovery-label trap is worth stating plainly, because it fails silently in
both directions: `kubectl get servicemonitor,prometheusrule -n <release-ns>`
shows the objects, the operator looks correctly configured, and Prometheus never
loads either one. Confirm the stack's own selector before assuming the label:

```console
$ kubectl get prometheus -A -o jsonpath='{.items[*].spec.ruleSelector}'
$ kubectl get prometheus -A -o jsonpath='{.items[*].spec.serviceMonitorSelector}'
```

### Without the prometheus-operator stack

The `ServiceMonitor` switch is the only thing the chart offers for scrape
configuration, and the metrics `Service` is rendered together with it — enabling
`serviceMonitor` purely to get the `Service` is legitimate. For an annotation-
based Prometheus, annotate the pod instead:

```yaml
# values.yaml
podAnnotations:
  prometheus.io/scrape: "true"   # example — the convention your Prometheus reads
  prometheus.io/port: "8080"     # example
```

The alerting rules are plain Prometheus rules; rendering them for a non-operator
Prometheus is `helm template` plus copying the `spec.groups` block:

```console
$ helm template <release> deploy/helm/stackit-s3-provisioner \
    --set monitoring.prometheusRule.enabled=true \
    --show-only templates/prometheusrule.yaml
```

### Verify

```console
$ kubectl -n <release-namespace> port-forward deploy/<release> 8080:8080
$ curl -s localhost:8080/metrics | grep -c '^stackit_s3_provisioner_'
```

A non-zero count proves the endpoint serves. To prove Prometheus actually scrapes
it, query for a series that is exported unconditionally — `skeleton_mode` and
`provider_circuit_open` are always present, so `absent()` on either one
distinguishes "operator healthy and scraped" from "not scraped at all":

```promql
stackit_s3_provisioner_skeleton_mode
```

---

## The metric catalogue

28 series in total, on top of the standard controller-runtime and Go collectors.
The controller-runtime series matter too: the reconcile-error alert is built on
`controller_runtime_reconcile_errors_total`, and the two controllers are labelled
`controller="bucket"` (provisioning) and `controller="bucketusage"` (size
measurement). `controller_runtime_reconcile_total{controller="bucket",result=~"success|requeue_after"}`
is the fleet-wide proof that the drift resync is running — an unchanged pass
raises no event and logs nothing at the default level, so this counter and the
per-object `status.lastVerifiedTime` are what remain. A pass that schedules
the next resync is counted as `requeue_after`; `success` is what it counts as
only with the resync off
([ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md) D4/D5).

### How they are produced

Every gauge below is computed from the manager's cache at scrape time — the
collector lists the `Bucket` resources and derives the numbers on the spot. Three
consequences follow, and alert expressions depend on all three:

| Consequence | Why it matters |
| --- | --- |
| The gauges cannot drift | There is no per-reconcile bookkeeping to get out of step with reality; a wrong value self-heals on the next scrape |
| A failed cache list omits **every** bucket-derived series for that scrape | An absent series, not a zero. The collector bounds the wait at 10s so a scrape during startup cannot hang the handler |
| Every replica exports the full fleet, leader or not | The metrics server is not gated on leader election. During a rolling update two pods export the same numbers, which is why the fleet-gauge expressions wrap their selector in `max(…)` rather than `sum(…)`. This does not apply to the counter-based expressions: reconciling and measuring *are* gated on leader election (`leaderElection.enabled: true` # default), so a non-leader replica never increments `controller_runtime_reconcile_errors_total`, `..._usage_measurement_failures_total` or `..._bucket_recreated_total` and `sum(increase(…))` over them is safe. Verified in `cmd/main.go` and `deploy/helm/stackit-s3-provisioner/values.yaml` |

The [process series](#process-series) are the deliberate exception, and one rule
decides which side a series falls on: it is derived from the cache when the
`Bucket` resources carry the answer, and process-scoped when they cannot. Three of
them describe work that leaves nothing durable behind — a failed measurement
leaves nothing on the `Bucket` beyond a message, the duration of a listing pass is
gone once it finished, and an authorised re-creation leaves no durable trace on
the CR at all
([ADR 0015 D13](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)).
The other four describe the operator's own service-account key, which no `Bucket`
mentions at all. Every replica polls that file and swaps on its own
([ADR 0016 D13](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)),
so the four are per-pod state: two replicas can report different values while a
rotation is landing.

### Fleet-wide series

| Metric | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `stackit_s3_provisioner_buckets{phase}` | gauge | Always, all six labels | `Bucket` count per `status.phase`: `Pending`, `Provisioning`, `Ready`, `Failed`, `Deleting`, and `Unknown` for a CR with no status written yet. Every phase is exported with `0` when empty, so an expression never races an absent label |
| `stackit_s3_provisioner_buckets_clone{phase}` | gauge | Always, three labels | `Bucket` count per clone phase: `Running`, `Completed`, `Failed`. Buckets without a clone are not counted at all |
| `stackit_s3_provisioner_buckets_wipe_on_delete` | gauge | Always | `Bucket` resources carrying `spec.wipeOnDelete: true` |
| `stackit_s3_provisioner_buckets_provider_degraded` | gauge | Always | `Bucket` resources whose `Ready` is currently being **held** through failing reconciles. Counts a hold that is in effect: a bucket that already fell to `Failed` keeps `status.degradedSince` but is no longer counted here ([ADR 0012 D4](../adr/0012-ready-describes-the-last-verified-state.md)) |
| `stackit_s3_provisioner_buckets_usage_measured` | gauge | Always | `Bucket` resources that carry a size measurement |
| `stackit_s3_provisioner_skeleton_mode` | gauge | Always | `1` while the operator runs without a service-account key and provisions nothing |
| `stackit_s3_provisioner_wipe_on_delete_gate_enabled` | gauge | Always | `1` while the operator-wide `--enable-wipe-on-delete` gate is on |
| `stackit_s3_provisioner_usage_measurement_gate_enabled` | gauge | Always | `1` while the operator-wide size-measurement gate is on |
| `stackit_s3_provisioner_provider_circuit_open` | gauge | Always, `0` while closed — including when the breaker is disabled | `1` while the fleet-wide breaker is open and the operator is making no provider calls at all ([ADR 0013 D10](../adr/0013-a-provider-outage-is-held-fleet-wide.md)) |
| `stackit_s3_provisioner_provider_circuit_opened_timestamp_seconds` | gauge | **Only while open** | When the current outage episode began. It does not move when a probe fails, so `time() - <series>` is the age of the outage, not of the last probe interval ([ADR 0013 D10](../adr/0013-a-provider-outage-is-held-fleet-wide.md)) |

### Per-`Bucket` series

All of these carry `{namespace, name}`. Absence is meaningful on every one of
them — that is the design, so `absent()` separates "no measurement" from "zero".

| Metric | Type | Present when | Meaning |
| --- | --- | --- | --- |
| `stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds` | gauge | The bucket is being held (`status.degradedSince` set **and** phase still `Ready`) | When the run of failures began. `time() - <series>` is the age of the hold; the series disappears when the hold is given up |
| `stackit_s3_provisioner_bucket_provisioned_missing` | gauge | The provider answered that this provisioned bucket is gone (the `BucketPresent` condition is `False`) | `1` means the data is lost, not that a call failed — only the provider's own structured answer gets a `Bucket` into this state ([ADR 0015 D3](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)). Derived from the condition, not from the phase: a `Bucket` that is `Failed` for any other reason carries no series at all |
| `stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds` | gauge | The bucket was rotated at least once | When its workload credential was last rotated ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)) |
| `stackit_s3_provisioner_bucket_size_bytes` | gauge | Measured at least once | Size in bytes of the bucket's current objects at the last measurement |
| `stackit_s3_provisioner_bucket_objects` | gauge | Measured at least once | Number of current objects |
| `stackit_s3_provisioner_bucket_version_size_bytes` | gauge | Measured at least once | Non-current versions; `0` unless version counting is on ([ADR 0014 D12](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)) |
| `stackit_s3_provisioner_bucket_version_objects` | gauge | Measured at least once | Non-current versions **and** delete markers; `0` unless version counting is on |
| `stackit_s3_provisioner_bucket_billable_size_bytes` | gauge | Measured at least once | Current objects plus counted versions — the figure the estimate is computed from |
| `stackit_s3_provisioner_bucket_estimated_monthly_cost{currency}` | gauge | Measured **and** a price is configured | Estimated monthly storage cost in whole currency units (the canonical value on the resource is cents; this divides by 100) |
| `stackit_s3_provisioner_bucket_usage_last_measurement_timestamp_seconds` | gauge | Measured at least once | `time() - <series>` is the age of the reported size |
| `stackit_s3_provisioner_bucket_usage_truncated` | gauge | Measured at least once | `1` when the last measurement stopped at the object cap, so size and cost are **lower bounds** ([ADR 0014 D9](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)) |

### Process series

| Metric | Type | Meaning |
| --- | --- | --- |
| `stackit_s3_provisioner_usage_measurement_failures_total` | counter | Measurements that could not complete. A failed measurement deliberately returns **no** reconcile error ([ADR 0014 D3](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)), so this counter is the only place they aggregate — `controller_runtime_reconcile_errors_total{controller="bucketusage"}` stays flat through them |
| `stackit_s3_provisioner_bucket_recreated_total` | counter | Vanished buckets rebuilt automatically because `spec.allowRecreate` authorised it ([ADR 0015 D9](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)). The one series in this table that carries `{namespace, name}`, because it has to name the `Bucket` whose workload is now holding a replaced Secret — and it belongs here rather than with the per-`Bucket` gauges because it cannot be derived from the cache: an authorised re-creation ends with the CR back in `Ready` and leaves nothing behind on it. A process counter, so it restarts at zero with the operator |
| `stackit_s3_provisioner_usage_measurement_duration_seconds` | histogram | Duration of one successful listing pass. Buckets: `0.1, 0.5, 1, 5, 15, 60, 300, 900, 1800` seconds. This is the number to look at before lowering `bucketUsage.interval`, and the one that shows a bucket outgrowing its cap |
| `stackit_s3_provisioner_sa_key_reload_total{result}` | counter | Polls of the service-account key file, by what the poll decided: `applied`, `rejected`, `unchanged`. All three label values are exported from the first scrape, starting at `0`, so an expression never races an absent label. `unchanged` is the normal case and costs no provider call — only a changed file is validated ([ADR 0016 D2](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)). Polls skipped while a definitively rejected candidate is backing off are not counted at all: the counter measures decisions, not ticks |
| `stackit_s3_provisioner_sa_key_loaded_timestamp_seconds` | gauge | When **this process** started using the key it is running on — process start for the key it booted with, the moment of the swap after a reload. `time() - <series>` is therefore the age of this pod's key in this pod, not the age of the key itself |
| `stackit_s3_provisioner_sa_key_reload_failing` | gauge | `1` while a candidate on disk keeps being rejected and the operator carries on with the key it already holds. Back to `0` on the first poll that either applies a candidate or finds the file unchanged ([ADR 0016 D8](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)) |
| `stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds` | gauge | When the key in use expires, taken from the `validUntil` the provider stamps into the key file. **Absent** when the file carries none — see [Absent is not zero](#absent-is-not-zero) ([ADR 0016 D10](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)) |

The four `sa_key_*` series exist only while the reload is actually running: an
operator in skeleton mode, or one with
`stackit.serviceAccountKey.reloadInterval` set to `"0"`, exports none of them
rather than a reassuring zero for a mechanism that is not there
([ADR 0016 D12](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)).
Their absence therefore does not distinguish a stopped operator from a deliberate
configuration; the scrape check above tests `skeleton_mode`, which a running
operator exports either way.

### Absent is not zero

The five cases an expression has to keep apart:

| You see | It means |
| --- | --- |
| A fleet gauge is `0` | The operator is up, scraped, and the answer really is none |
| Every `stackit_s3_provisioner_*` series is gone | The operator is down, not scraped, or the scrape config was never discovered. **No shipped alert fires on this** — see [What is deliberately not alerted on](#what-is-deliberately-not-alerted-on) |
| Only the bucket-derived series are gone while `skeleton_mode` and `provider_circuit_open` remain | The collector's list against the cache failed on that scrape |
| A per-bucket usage series is absent | That bucket was never measured — not that it is empty |
| `..._sa_key_valid_until_timestamp_seconds` is absent while the other three `sa_key_*` series are there | The key file carries no `validUntil`. A real shape rather than a fault, and the consequence is that the two expiry alerts simply never fire for such a key instead of firing falsely — the rotation deadline then has to be tracked outside this chart ([ADR 0016 D10](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)) |

---

## The shipped alerts

Sixteen rules, each with its own `enabled` toggle under
`monitoring.prometheusRule.alerts.<name>`. Fifteen default to `true`;
`StackitS3BucketRecreated` is the single exception and ships disabled, because it
can only ever fire on a deployment that has adopted `spec.allowRecreate`
([ADR 0015 D12](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)).
They are only rendered when `monitoring.prometheusRule.enabled` is `true` **and**
at least one toggle is on.

The alert names, their toggle keys and which three of them are `critical` are in
the [README reference](../../README.md); the other thirteen are `warning`. What
that list does not carry is the `for`
window each rule waits out before it fires, and the rendered expression behind
it — and the `for` window is what separates an alert reporting a state from one
reporting a transient:

| Alert | `for` |
| --- | --- |
| `StackitS3BucketsWipeOnDelete` | 5m |
| `StackitS3BucketFailed` | 15m |
| `StackitS3BucketStuckProvisioning` | 30m |
| `StackitS3BucketStuckDeleting` | 30m |
| `StackitS3CloneFailed` | 30m |
| `StackitS3SkeletonMode` | 15m |
| `StackitS3SaKeyReloadFailing` | 30m |
| `StackitS3SaKeyExpiring` | 1h |
| `StackitS3SaKeyExpiringCritical` | 1h |
| `StackitS3WipeRequestedButGateDisabled` | 15m |
| `StackitS3BucketProviderDegraded` | 1m |
| `StackitS3BucketMissing` | 5m |
| `StackitS3BucketRecreated` | 1m (ships disabled) |
| `StackitS3UsageMeasurementFailing` | none |
| `StackitS3UsageMeasurementTruncated` | 30m |
| `StackitS3ReconcileErrors` | `sustainedFor` (15m # default) |

Only `StackitS3ReconcileErrors` has a configurable `for` window, and it is the
one to read carefully: `sustainedFor` stacks **on top of** the rule's own
15-minute `increase()` window, so the error rate has to hold across a second
window before the alert pages. The short and absent windows are not oversights —
for `StackitS3BucketProviderDegraded`, `StackitS3BucketRecreated` and
`StackitS3UsageMeasurementFailing` the window that matters is inside the
expression itself (`holdForSeconds`, the `increase()` lookback `window` and a
30-minute `increase()` respectively), so a `for` clause would only add a second,
redundant delay.

<details>
<summary>The sixteen expressions as rendered</summary>

```promql
# StackitS3BucketsWipeOnDelete            for: 5m
max(stackit_s3_provisioner_buckets_wipe_on_delete) > 0

# StackitS3BucketFailed                   for: 15m
max(stackit_s3_provisioner_buckets{phase="Failed"}) > 0

# StackitS3BucketStuckProvisioning        for: 30m
sum(max by (phase) (stackit_s3_provisioner_buckets{phase=~"Pending|Provisioning"})) > 0

# StackitS3BucketStuckDeleting            for: 30m
max(stackit_s3_provisioner_buckets{phase="Deleting"}) > 0

# StackitS3CloneFailed                    for: 30m
max(stackit_s3_provisioner_buckets_clone{phase="Failed"}) > 0

# StackitS3SkeletonMode                   for: 15m   severity: critical
max(stackit_s3_provisioner_skeleton_mode) == 1

# StackitS3SaKeyReloadFailing             for: 30m
max(stackit_s3_provisioner_sa_key_reload_failing) > 0

# StackitS3SaKeyExpiring                  for: 1h
min(stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds - time()) < 1209600   # leadTimeDays: 14 # default

# StackitS3SaKeyExpiringCritical          for: 1h    severity: critical
min(stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds - time()) < 259200    # leadTimeDays: 3 # default

# StackitS3WipeRequestedButGateDisabled   for: 15m
max(stackit_s3_provisioner_buckets_wipe_on_delete) > 0
  and max(stackit_s3_provisioner_wipe_on_delete_gate_enabled) == 0

# StackitS3BucketProviderDegraded         for: 1m
max(time() - stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds) > 1200   # holdForSeconds # default

# StackitS3BucketMissing                  for: 5m    severity: critical
max(stackit_s3_provisioner_bucket_provisioned_missing) > 0

# StackitS3BucketRecreated                for: 1m    (disabled # default)
sum(increase(stackit_s3_provisioner_bucket_recreated_total[6h])) > 0   # window # default

# StackitS3UsageMeasurementFailing        (no for:)
sum(increase(stackit_s3_provisioner_usage_measurement_failures_total[30m])) > 3

# StackitS3UsageMeasurementTruncated      for: 30m
max(stackit_s3_provisioner_bucket_usage_truncated) > 0

# StackitS3ReconcileErrors                for: 15m   (sustainedFor)
(
  sum(increase(controller_runtime_reconcile_errors_total{controller="bucket"}[15m])) > 6   # threshold # default
)
unless on()
(max_over_time(stackit_s3_provisioner_provider_circuit_open[15m]) > 0)
```

The `unless on()` clause is only rendered while
`alerts.reconcileErrors.suppressWhileCircuitOpen` is `true` (# default).

</details>

Note that the fleet-count alerts fire on the **existence** of a bucket in a state
for the whole `for` window, not on one bucket staying there. A fleet that always
has some bucket in `Provisioning` — a busy cluster, or one long clone — keeps
`StackitS3BucketStuckProvisioning` firing continuously. The per-bucket detail is
not in the alert; it is in `kubectl get bkt -A`.

---

## The alerts that carry a number

Ten of the sixteen are threshold-free statements about a state that should not
persist. Six carry numbers. Five of those numbers are Helm values — two
constrained by something else in the configuration, one by a property of the
counter it reads, and the two key-expiry lead times by each other; the sixth is
hardcoded in the rule template. All six are here because their implications are
not obvious.

### `StackitS3ReconcileErrors` — and why it excludes provider outages

`threshold: 6` errors in a 15-minute window, `sustainedFor: 15m`, and every window
in which the circuit breaker was open at any point is removed from consideration
entirely.

The exclusion exists because a fleet-wide provider outage is reported by a
different alert ([ADR 0013 D11](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).
Once the breaker trips the operator stops calling the provider, so it stops
producing errors at all — what still reaches this alert is failures that stayed
**interleaved with fleet successes**, whatever their origin: typically a single
bucket failing while the rest of the fleet reconciles fine.

That qualification is load-bearing. The trip condition is the absence of a
successful reconcile, not the origin of the error, so a failure while talking to
the **Kubernetes API** counts toward the trip exactly like a provider failure
([ADR 0013 D11](../adr/0013-a-provider-outage-is-held-fleet-wide.md)). A
fleet-wide Kubernetes API problem therefore trips the breaker too, and the same
exclusion removes it from this alert. Only the interleaved remainder arrives
here. The README and the chart's own alert annotation phrase this more loosely —
where they disagree with the ADR, the ADR is authoritative and matches the code.

Three properties of this expression, each deliberate:

| Property | Consequence |
| --- | --- |
| `unless on()` with no label matcher | Removes the *whole* window when the circuit was open anywhere, not a matching subset |
| Fail-open by construction | If `..._provider_circuit_open` is absent — an operator image older than the chart — the `unless` removes nothing and the alert behaves as it did before the breaker existed |
| `max_over_time(…[15m])` | A trip is caught if it survived at least one scrape. A scrape interval coarser than the breaker's 60s base cooldown can miss a single short trip; the result is the alert firing, the fail-open direction again |

**The constraint on `threshold`:** a tripped circuit costs one reconcile error
**fewer** than `providerCircuit.threshold` — two at the default threshold of `3`.
The failure that reaches the threshold already returns a delayed retry and no
error, and so does every probe failure while the circuit is open
([ADR 0013 D5](../adr/0013-a-provider-outage-is-held-fleet-wide.md), and its
Context: the breaker exists to make
`controller_runtime_reconcile_errors_total` measure outages rather than retry
volume). Keeping the alert threshold above that cost is what prevents unrelated
short outages in one window from paging: at the defaults a single outage
contributes two errors, so three unrelated outages inside one 15-minute window
still do not cross `> 6`, and four do. `threshold: 6` is also exactly twice the
breaker threshold, but that is arithmetic rather than the safety margin — the
margin is measured against `providerCircuit.threshold - 1`.

To alert on provider outages here too, set
`monitoring.prometheusRule.alerts.reconcileErrors.suppressWhileCircuitOpen: false`
— the clause then disappears from the rendered rule, and a fleet-wide outage
produces the burst it produced before.

### `StackitS3BucketProviderDegraded` — the age of the hold, not its existence

```promql
max(time() - stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds) > 1200
```

A provisioned `Bucket` keeps saying `Ready` while the operator cannot verify it
([ADR 0012 D1](../adr/0012-ready-describes-the-last-verified-state.md)). That hold
is the feature — a provider blip that resolves inside the window is exactly what
it is for, and alerting on the mere existence of a hold means paging for every
blip the design already absorbed. So the alert fires on how **long** the hold has
lasted: reaching the window means the drop to `Failed` is close and there is still
time to act.

**The ordering constraint, and it is hard:** `holdForSeconds` must stay below
`providerDegradedGrace`. The hold is bounded by that grace
([ADR 0012 D5](../adr/0012-ready-describes-the-last-verified-state.md)); once it
elapses the `Bucket` drops to `Failed`, the collector stops emitting
`bucket_degraded_since_timestamp_seconds` for it — the series is only emitted
while the phase is still `Ready` — and the expression has nothing left to
evaluate. Set `holdForSeconds` at or above the grace and the alert can never fire
at all; `StackitS3BucketFailed` takes over instead, 15 minutes later.

| Value | Default | Constraint |
| --- | --- | --- |
| `monitoring.prometheusRule.alerts.bucketProviderDegraded.holdForSeconds` | `1200` (20m) | Must be `< providerDegradedGrace` |
| `providerDegradedGrace` | `30m` | `0` disables the hold; the degraded series then never appears and this alert is dead by construction |

The defaults leave a 10-minute window between the alert firing and the bucket
falling to `Failed`. Lowering the grace without lowering `holdForSeconds` silently
disables the alert — change both or neither. The mechanism itself is documented in
[provider-outages.md](provider-outages.md).

### `StackitS3BucketRecreated` — a lookback, not a state

```promql
sum(increase(stackit_s3_provisioner_bucket_recreated_total[6h])) > 0   # window # default
```

Every other alert in the set fires on something that has not stopped — a state
that is still there, or, for the two other `increase()` rules, a run of failures
that is still arriving. This one reports an event that is already over: a
`Bucket` carrying `spec.allowRecreate` lost its bucket, the operator rebuilt it
empty and unattended, and the CR went back to `Ready` with no durable record of
any of it
([ADR 0015 D13](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)).
There is nothing left to test afterwards, so `window` is the whole visibility of
the incident — it decides how long after the rebuild the alert can still fire,
and it has to outlast the time somebody needs to notice.

| Value | Default | Constraint |
| --- | --- | --- |
| `monitoring.prometheusRule.alerts.bucketRecreated.window` | `"6h"` | The lookback of the `increase()`, and the only thing keeping the incident visible |
| `monitoring.prometheusRule.alerts.bucketRecreated.enabled` | `false` | The one alert in the set that ships off. Turn it on with the first `Bucket` that sets `spec.allowRecreate`, not before — until then it can never fire |

**The caveat no window fixes:** `..._bucket_recreated_total` is a process counter
and restarts at zero with the operator, so an `increase()` across a restart
under-reports —
[ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
names that the accepted weakest link in the record of an unattended re-creation,
since the Warning event beside it lives only as long as the cluster keeps events.
What to do when the alert fires is in [vanished-buckets.md](vanished-buckets.md).

### `StackitS3UsageMeasurementFailing` — what the threshold implies

`> 3` failures per 30 minutes, with no `for` clause. This is the one number that
is **not** a Helm value — it is written into the rule template, so changing it
means editing the rendered `PrometheusRule`. A failing measurement retries
after `min(interval, 10m)`, and the interval floor is 60m by default, so in
practice a permanently failing bucket produces one failure every 10 minutes —
three per 30-minute window. That sits exactly on the threshold, and `increase()`
extrapolates, so a single permanently failing bucket may or may not cross it;
two failing buckets always do. This is arithmetic from the retry constant and the
default interval floor, not a measured figure.

### The two key-expiry alerts — two lead times on one gauge

```promql
min(stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds - time()) < 1209600   # 14 days
```

Both rules compute the remaining lifetime inside the expression, from the
absolute timestamp the operator exports. That is deliberate: a countdown
exported by the operator would change on every scrape, make the pod's clock a
fault source, and let a hung exporter report a healthy-looking constant
([ADR 0016 D10](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)).

| Value | Default | Constraint |
| --- | --- | --- |
| `monitoring.prometheusRule.alerts.saKeyExpiring.leadTimeDays` | `14` | Has to outlast the slowest rotation path actually in use, issuing the new key included. Integer days; the rule multiplies by 86400 |
| `monitoring.prometheusRule.alerts.saKeyExpiringCritical.leadTimeDays` | `3` | The same measurement at a shorter lead and `critical` severity. Keep it well below the warning lead |

Two things follow. The expressions are identical apart from the number, so once
the critical lead is crossed **both** fire — route them apart by severity rather
than expecting one to replace the other. And both are dead by construction for a
key file carrying no `validUntil`: the gauge is absent, the expression evaluates
to nothing, and no rule in this chart reports that silence.

---

## What each alert means and what to do

| Alert | What you look at | What it usually is |
| --- | --- | --- |
| `StackitS3SkeletonMode` | `kubectl logs` for `running in skeleton mode`; `stackit.serviceAccountKey.secretName` and the referenced Secret | The key Secret is missing, misnamed, or the release was installed without it. See [prerequisites.md](prerequisites.md) |
| `StackitS3SaKeyReloadFailing` | The operator log for `rejected a candidate StackIT service-account key`, then the Secret named by `stackit.serviceAccountKey.secretName` | A rotation the operator is refusing. Nothing is broken yet — it is still working on the key it holds, and a rejected candidate never displaces it — and the log line carries the reason: a file that does not parse, a key naming a different project, or a key the provider will not mint a token with. The danger is the old key expiring while the replacement keeps being refused, which turns this warning into the whole fleet failing at once; `StackitS3SaKeyExpiring` is what bounds that ([ADR 0016 D8](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)) |
| `StackitS3SaKeyExpiring` | `stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds`, and whether a replacement key has been written to the Secret at all | The key in use runs out within the warning lead time. Issue a new one and write it into the Secret named by `stackit.serviceAccountKey.secretName`; the operator picks it up within `stackit.serviceAccountKey.reloadInterval` without a restart, and `StackitS3SaKeyReloadFailing` is what tells you if it will not take it |
| `StackitS3SaKeyExpiringCritical` | The same series, and `stackit_s3_provisioner_sa_key_loaded_timestamp_seconds` to see whether this pod has already picked a new key up | Days from expiry. At expiry every `Bucket` drops to `Failed` in the same minute and the fleet-wide breaker opens on top, and nothing heals until a valid key is in place. The warning alert is still firing alongside this one |
| `StackitS3BucketMissing` | `stackit_s3_provisioner_bucket_provisioned_missing` for **which** `Bucket` — the alert expression drops the labels — then its `BucketPresent` condition and `status.message` | Data loss, not an outage: a bucket the operator provisioned was deleted behind its back, or the operator is authenticated against a project that never held it. It is deliberately **not** re-created. Runbook in [vanished-buckets.md](vanished-buckets.md) |
| `StackitS3BucketRecreated` | `stackit_s3_provisioner_bucket_recreated_total` for which `Bucket`, then the `BucketRecreated` event on it and the workloads mounting its Secret | An authorised rebuild under `spec.allowRecreate` that already happened: the contents are gone, the credentials Secret was replaced, and consumers keep failing with `403` until they re-read it. The previous credentials group is left standing and its cleanup is manual. See [vanished-buckets.md](vanished-buckets.md) |
| `StackitS3BucketFailed` | `kubectl get bkt -A`, then `status.message` on the failed ones | A configuration fault that parks without requeueing: a Secret key collision, a `spec.region` mismatch, a `secretRef` aimed at the admin Secret, an ownership collision. A vanished bucket lands here too — it keeps retrying rather than parking, and `StackitS3BucketMissing` has already fired ten minutes earlier. See [bucket-status.md](bucket-status.md) |
| `StackitS3BucketStuckProvisioning` | `status.message` and `status.clone` | A long clone, provider trouble, or a quota limit |
| `StackitS3BucketStuckDeleting` | `status.message` on the terminating CR | Almost always the emptiness guard refusing to delete a non-empty bucket ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)). Runbook in [deletion.md](deletion.md) |
| `StackitS3CloneFailed` | `status.clone`, then the clone Job's logs in the release namespace | Dead source credentials or a source bucket that no longer exists. See [cloning.md](cloning.md) |
| `StackitS3BucketProviderDegraded` | `status.degradedSince`, the `ProviderReachable` condition, and `stackit_s3_provisioner_provider_circuit_open` | A provider outage that has lasted long enough to matter. See [provider-outages.md](provider-outages.md) |
| `StackitS3ReconcileErrors` | Operator logs and `status.message` on individual buckets | Failures that stayed interleaved with fleet successes — usually one bucket failing among healthy ones. Anything fleet-wide, provider **or** Kubernetes API, trips the breaker and is excluded ([ADR 0013 D11](../adr/0013-a-provider-outage-is-held-fleet-wide.md)) |
| `StackitS3BucketsWipeOnDelete` | `kubectl get bkt -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,WIPE:.spec.wipeOnDelete` | Informational and intentional: deleting such a CR destroys all objects irreversibly |
| `StackitS3WipeRequestedButGateDisabled` | The `wipeOnDelete.enabled` value and the `WipeOnDeleteSkipped` warning events | A manifest promises a wipe the operator will not perform; deletion degrades to the empty-only guard |
| `StackitS3UsageMeasurementFailing` | `status.usage.message` and the `UsageMeasurementFailed` events | The size on the CRs is going stale. `Ready` is unaffected on purpose |
| `StackitS3UsageMeasurementTruncated` | `status.usage.truncated` and `bucketUsage.maxObjects` | A bucket outgrew the object cap; its size and cost are lower bounds. Raise the cap deliberately — the listing pass gets proportionally longer. See [usage-and-cost.md](usage-and-cost.md) |

The event reasons worth watching alongside these, none of which has a shipped
alert: `CredentialsRotated`, `CredentialsGroupAttributed`,
`CredentialsGroupNotAttributable`, `ReadGrantPending`, `WipeOnDeleteSkipped`,
`WipingBucket`, `UsageMeasurementTruncated`, `UsageIntervalClamped`,
`UsageConfigInvalid`. Those names are the event **reason** — the field
`kubectl get events --field-selector reason=<name>` filters on. The event
*action* is a separate field and is coarse: `Reconcile` for everything the
provisioning controller emits, `MeasureUsage` for everything the measurement
controller emits. Filtering on `reason=Reconcile` matches nothing.

---

## What is deliberately not alerted on

**The open circuit itself.** It would fire at the same time as
`StackitS3BucketProviderDegraded` and report the same thing twice. The circuit is
observability — a dashboard signal and an alert suppressor; the hold is the thing
to react to ([ADR 0013 D12](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).

**A failed size measurement, on the `Bucket` itself.** The bucket is fine; only
its size is unknown. Measurement never touches `Ready` and never returns a
reconcile error ([ADR 0014 D3](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)),
which is exactly why the failure counter and its alert exist.

**The operator being absent.** This is a gap, stated plainly: every shipped
expression tests a series the operator itself exports — a fleet gauge, a
per-bucket gauge, a process gauge, or an `increase()` over a counter it exports —
and all sixteen evaluate to nothing when the operator is down, not scraped, or its
`ServiceMonitor` was never discovered because of the label trap above. Nothing in
this chart notices that.
The cover is generic and comes from the monitoring stack, not from here —
`up == 0` on the target, kube-prometheus-stack's `TargetDown`, or the usual
workload alerts on a crash-looping or absent `Deployment`. Not verified, and this
is the gap: whether your stack actually has those enabled. If the operator's
availability matters, add an `absent(stackit_s3_provisioner_skeleton_mode)` rule
of your own — that series is exported unconditionally, so its absence is
unambiguous.

---

## The measurements behind the tuning

These numbers come from read-only investigations of the production management
cluster. They are not reproducible from this repository; what is in the repository
is the tuning they produced.

**2026-08-25 — why `Ready` is held at all.** Between 08:13 and 08:15 UTC the
Object Storage API answered `403` as an nginx HTML error page. The operator
produced 342 reconcile errors in about two minutes, and all 19 buckets on the
cluster went non-`Ready` at once; downstream, roughly 11 tenant Flux
`Kustomization` resources went unhealthy and the downstream readiness alert,
`FluxResourceNotReady`, fired for seven tenant platforms. A second event at
10:37 UTC — one connection dying mid-response — cost a single bucket its
readiness for about ten minutes. Both are in the
context of [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md).

That alert is **not** shipped by this chart. It lives in the consuming cluster's
own monitoring stack — nothing in this repository renders a rule watching Flux
resources, and the sixteen rules in `prometheusrule.yaml` all test series the
operator itself exports. On that cluster it carried severity `warning` and
`for: 5m` over the Flux readiness conditions:

```promql
# example — the rule as it stood on that cluster on 2026-08-25; this chart
# ships nothing like it, so check your own stack before assuming the shape
kube_customresource_status_condition{customresource_kind=~"Kustomization|HelmRelease", type="Ready", status!="True"}
```

A 14-day query of that cluster's Prometheus history, taken on 2026-08-25,
established that the downstream readiness alert had fired almost exclusively
inside that one 08:08–08:24 UTC window, plus a single benign firing on 2026-08-22
when initial provisioning of one bucket took longer than the alert's five-minute
sustain. That is the evidence that the alert was measuring provider blips rather
than anything actionable, and it is what made a *bounded* hold the right answer
instead of a longer `for` clause.

**2026-09-02 — why the reconcile-error alert suppresses circuit windows.** A
provider-side `503` storm began at 14:23 UTC. The first `429`
(`rate limit on IP level exceeded`) appeared eleven minutes later at 14:34, and by
14:42 the operator was collecting 51 rate-limited requests per minute: it had
hammered its own IP rate limit and was extending the outage it was reacting to.
`sum(increase(controller_runtime_reconcile_errors_total{controller="bucket"}[15m]))`
reached 220; the outage resolved on its own at 14:43. One self-healing outage, 242
reconcile errors, two pages, nothing to do — against a threshold of `> 3` with
`for: 0` at the time. The timeline and its reading are in
[ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md).

Two amplifiers were in the operator's own code and both were removed with the
breaker: the bucket controller ran on controller-runtime's default workqueue rate
limiter (5ms base backoff, 10 qps fleet-wide, meant for controllers that only talk
to the local API server), and the retry transport retried `429` — three
*attempts* per read, so two retries — re-asking for a rate limit being the one
response guaranteed to deepen it
([ADR 0013 D9](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).

Note when reconstructing an incident yourself: the operator logs in UTC while
Prometheus range queries answer in the browser's local time. The same incident
then looks like two, two hours apart.

---

## Related

- [provider-outages.md](provider-outages.md) — the hold and the breaker, and the two settings that bound them
- [usage-and-cost.md](usage-and-cost.md) — what the size and cost series are measured from
- [vanished-buckets.md](vanished-buckets.md) — what the two alerts on a missing bucket mean on the `Bucket` itself, and what to do
- [bucket-status.md](bucket-status.md) — phases, conditions and events on a single `Bucket`
- [deployment.md](deployment.md) — replicas, leader election and rolling updates
- [../developer/service-account-key-reload.md](../developer/service-account-key-reload.md) — how the poll, the validation and the swap behind the four `sa_key_*` series are built
- [README reference](../../README.md) — the complete Helm value and `Bucket` field list
- [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md), [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md), [ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md), [ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md), [ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)
