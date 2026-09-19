# Operations

Thirteen pages for whoever runs this operator or integrates with it: what has to exist in the
STACKIT account before the first install, how to install and upgrade it, how to read a `Bucket`,
and what to do when the provider is unreachable at three in the morning.

**These pages explain settings. They never list them.** The complete configuration surface — every
chart value and every `Bucket` field, each with its default — lives in the
[README reference](../../README.md) and nowhere else. See
[Where the settings themselves are written down](#where-the-settings-themselves-are-written-down)
at the bottom for why that boundary is drawn where it is, and why the one place it looks violated
is deliberate.

---

## Read it when

| Page | Read it when |
| --- | --- |
| [prerequisites.md](prerequisites.md) | Before the first install. You need a STACKIT project, a service account and a role, and you need to know which parts of that the operator cannot do for you — and which one mistake silently breaks cross-project isolation. |
| [deployment.md](deployment.md) | You are installing, upgrading, rolling back or uninstalling the operator, running it without a service-account key, or wondering what more than one replica does. |
| [gitops.md](gitops.md) | Your `Bucket` manifests live in Git and a controller re-applies them continuously. Also for writing a health check that does not turn a provider blip into a cluster-wide alert storm, and for planning a replay from Git. |
| [configuration.md](configuration.md) | A value is set and you want to know what it actually does — what needs a restart, what the periodic drift resync re-checks and costs, and which single setting can park every `Bucket` in the cluster at once. |
| [bucket-naming.md](bucket-naming.md) | The cloud bucket is not called what the CR asked for, or you are planning a restore into a fresh cluster and need to know which install-time values decide whether the operator recognises its own buckets. |
| [bucket-status.md](bucket-status.md) | You have a `Bucket` in front of you: which column comes from which field, what each phase, condition and reason means, which faults park the object until somebody edits it, and what it means when the status and the cloud disagree. |
| [deletion.md](deletion.md) | A `kubectl delete bucket` will not finish, or you need to know what a teardown removes, in which order, and what it leaves behind in the cloud. |
| [credentials.md](credentials.md) | An application has to consume a workload Secret, a key must be rotated, or you are dealing with the operator's own admin Secret. |
| [cloning.md](cloning.md) | You want a new bucket seeded once from an existing S3 bucket, or a clone is running, stuck or failing and you need to read it. |
| [read-grants.md](read-grants.md) | A second workload in the same namespace needs read access to a bucket it does not own, or you are revoking such access. |
| [usage-and-cost.md](usage-and-cost.md) | You want size and a monthly cost estimate on the `Bucket`, or you have those numbers and need to know what they are not. |
| [provider-outages.md](provider-outages.md) | You are on call and the STACKIT API is unreachable: readiness held past the point you expected, deletions that appear stuck, an error counter that moved once and then stopped. |
| [monitoring.md](monitoring.md) | You are wiring up scraping, reading one of the exported series, or tuning, suppressing or silencing one of the eleven shipped alerts. |

---

## Supported integrations

One row per repeating integration, with the suite or procedure that proves it still works. Where
nothing proves it, the row says so — an untested integration documented as tested is worse than an
untested one.

| Integration | Page | What proves it still works |
| --- | --- | --- |
| The Helm chart install path | [deployment.md](deployment.md) | `make test-helm-render` renders the chart and asserts on the user-facing and operator `ClusterRole` objects ([test/helm/render_test.go](../../test/helm/render_test.go)); `make e2e-local` then installs the rendered chart into a Kind cluster and runs the `e2e`-tagged suite — `TestOperatorIsHealthy`, `TestBucketSkeletonReconcile`, `TestBucketRBACAggregation`. Both run on every push, in the `E2E Tests` job of [release.yml](../../.github/workflows/release.yml). |
| STACKIT Object Storage itself (control plane and S3 data plane) | all of them | `make e2e-stackit` — the same chart in Kind, but with a real service-account key: provisioning, read grants, size measurement and a real clone against the live API, with a guaranteed sweep afterwards. Manual by construction: it needs a key that is not in the repository, so CI never runs it. Last full green run recorded: 2026-09-01. Below it, `go test -tags integration ./stackit/` exercises the API wrapper and the policy evaluation directly. |
| GitOps with FluxCD | [gitops.md](gitops.md) | No suite drives a syncing controller against a live operator. The property that makes a sync a no-op is verified rule by rule — the operator writes only `status` and two pieces of metadata ([ADR 0010](../adr/0010-the-operator-never-writes-to-a-bucket-spec.md) D1, D3) — and `TestReconcileIsIdempotent` in [reconciler_fake_test.go](../../internal/controller/reconciler_fake_test.go) pins that a repeated reconcile changes nothing. Flux is the only syncing controller this has been exercised against, in practice rather than in a suite. |
| Argo CD and plain `kubectl apply` | [gitops.md](gitops.md) | Nothing in this repository runs Argo CD; the health-check snippet on that page reads only status fields verified against the CRD, and is marked unverified there. For `kubectl`, the claim that a diff leaves the operator's finalizer and frozen-name annotation alone is kubectl's documented merge behaviour, not something this repository tests. |
| kube-prometheus-stack (`ServiceMonitor` + `PrometheusRule`) | [monitoring.md](monitoring.md) | No suite renders or asserts either object — [test/helm/render_test.go](../../test/helm/render_test.go) covers the RBAC objects only. The procedure is manual and is on that page: render with `helm template --show-only templates/prometheusrule.yaml`, then port-forward `8080` and count `stackit_s3_provisioner_` series, then query `stackit_s3_provisioner_skeleton_mode` to prove the scrape actually happens. |
| rclone-compatible clone sources | [cloning.md](cloning.md) | `go test ./internal/controller/ -run Clone` offline (hold invariant, progress parsing, Job-failure retry, guards, teardown); `TestCloudClone` under `make e2e-stackit` for a real copy with the real rclone image, using a sibling `Bucket`'s own workload Secret as the source credential. Only a STACKIT source has been copied from: a source that requires virtual-hosted addressing — AWS S3 in particular — is verified against the offline suite only, and [cloning.md](cloning.md) states that gap where the setting is explained. |

---

## The other documentation

| Where | What lives there |
| --- | --- |
| [README.md](../../README.md) | The front page: the pitch, the naming conventions, the fast start — and the complete reference for every Helm value and every `Bucket` field. |
| [docs/adr/](../adr/README.md) | The decisions. Every rule these pages cite as `ADR 0012 D5` is defined there, with what it costs and what was rejected. An ADR names no file and no function on purpose, so it stays true when the tree moves. |
| [docs/developer/](../developer/README.md) | How the subsystems work, for somebody about to change them: the package map, the reconcile pipeline, the policy builder, the circuit breaker, the clone machinery, the provider-error classification, the test layout. |
| [docs/security/](../security/README.md) | The security design, one page per perspective — tenancy and isolation, credentials and Secrets, RBAC and privilege, ownership and attribution — each ending with what it does not cover and which gaps are open. |
| [DEVELOPER.md](../../DEVELOPER.md) | The contributor guide: repository layout, the build and test matrix, CI and the release process, the extension checklists. |
| [SECURITY.md](../../SECURITY.md) | How to report a vulnerability. The security *design* is `docs/security/`; this file is the reporting convention and nothing else. |

---

## Where the settings themselves are written down

**One decision, recorded here so that nobody later improves the documentation by duplicating it:**

> The complete configuration surface — every chart value and every `Bucket` CRD field, with its
> default — is listed in [README.md](../../README.md) and in no other file. A page in this
> directory explains what a setting *means*, what it costs and what it interacts with. It never
> restates the list.

Two lists of keys drift, and the drift is invisible: nothing fails, nothing is flagged, and the
reader who followed the stale one sets a value that no longer exists or misses one that now
matters. One list can be wrong; two lists are guaranteed to disagree eventually, and there is no
way to tell which one is lying. Which values block is explained on which page is itself a table —
[configuration.md → Where every other setting is explained](configuration.md#where-every-other-setting-is-explained)
— so a new key is added to the README reference and routed there, never copied into a page.

### The seam this makes visible: monitoring

Monitoring is where the split looks like a bug and is not. The README carries the value block —
`monitoring.serviceMonitor.*`, `monitoring.prometheusRule.enabled`, and the eleven
`monitoring.prometheusRule.alerts.<name>.enabled` toggles with a one-line trigger per alert.
[monitoring.md](monitoring.md) carries everything you need to decide what to do about an alert that
fires: what each of the 22 exported series actually counts, that an absent series is not a zero one
and which alert expressions depend on that, why the reconcile-error alert excludes windows in which
the circuit breaker was open, and why the degraded alert fires on the *age* of a hold rather than
its existence.

The boundary is: **the README answers "what is the key and what is its default", this directory
answers "what happens if I change it".** A trigger line in the README tells you which alert you are
looking at; it is not the explanation.

One value is deliberately stated in both places:
`monitoring.prometheusRule.alerts.bucketProviderDegraded.holdForSeconds`. The README lists it with
its default, because it is part of the surface. [monitoring.md](monitoring.md) states the constraint
that it must stay below `providerDegradedGrace` — otherwise the `Bucket` falls to `Failed` and the
series the alert watches disappears before the alert can fire. That is a relation between two keys,
not a second copy of a key: it is the kind of statement a reference table cannot hold, and the kind
this directory exists for.
