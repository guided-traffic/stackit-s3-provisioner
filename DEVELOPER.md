# Developer guide

Everything a contributor needs that is not specific to one subsystem: where things live, how to
build and test them, what CI does, and the ordered checklists for the changes people actually make.
Per-subsystem depth is not here — it is eleven pages under [docs/developer/](docs/developer/README.md),
and this guide links the right one at each step instead of repeating it.

| If you want | Go to |
|---|---|
| Why the operator does something the way it does | [docs/adr/](docs/adr/README.md) — fourteen binding decision records |
| How one subsystem works, before you change it | [docs/developer/](docs/developer/README.md) |
| How to run, configure or integrate with the operator | [docs/operations/](docs/operations/README.md) |
| The threat model and the gaps each mechanism leaves | [docs/security/](docs/security/README.md) |
| The complete `Bucket` CRD and chart-value reference | [README.md](README.md) — it lives there and nowhere else |

**Read the ADRs before you build.** They are binding, not historical. A requirement that collides
with one is not implemented quietly: name the record and the rule (`ADR 0003 D6`), and get the
record amended first. The format and the index are in [docs/adr/README.md](docs/adr/README.md).

---

## Repository layout

One line per directory — enough to know where to look. The file-by-file tree, what each file owns,
and the boundaries that were drawn on purpose are in
[docs/developer/package-map.md](docs/developer/package-map.md).

```
api/v1/                 the Bucket CRD types, and every helper that derives a value from a Bucket
cmd/                    the manager binary: flags, environment, client construction, controller wiring
internal/controller/    the two controllers, the circuit breaker, the clone machinery, the metrics
internal/stackitfake/   an in-memory fake of both provider surfaces, for offline tests
stackit/                the provider client: control plane, data plane, retry transport, error classes
config/                 kubebuilder/kustomize manifests; config/crd/bases is generated and is the CRD source
deploy/helm/            the chart that is the supported install path
hack/e2ecleanup/        a sweep for cloud resources a crashed cloud e2e run left behind
test/                   envtest, Kind e2e and chart-render suites, all behind build tags
docs/                   adr/, developer/, operations/, security/, and the ticket backlog
.github/workflows/      release.yml, build.yml, renovate.yml
```

`account-1.json` and `account-2.json` sit at the root on developer machines only. They are **real
service-account keys with embedded RSA private keys**, and one line in [`.gitignore`](.gitignore) is
the whole of what keeps them out of git — see [Conventions](#conventions).

## What each package is responsible for

One line per package. The file-by-file tables, the helper inventories and the boundaries that were
drawn deliberately are in [docs/developer/package-map.md](docs/developer/package-map.md).

| Package | Responsibility |
|---|---|
| [`api/v1/`](api/v1/) | The `Bucket` type and every value derived from it. A value computed from a `Bucket` has exactly one implementation, and it is here; the inventory of those helpers is in [package-map.md](docs/developer/package-map.md#apiv1--the-bucket-api) |
| [`cmd/`](cmd/main.go) | Flags and environment, the validation that must fail at startup rather than at reconcile time (naming policy, usage price), construction of the provider client or the decision to run without one, and the wiring of both controllers into one manager |
| [`internal/controller/`](internal/controller/) | The provisioning and teardown passes, the measurement pass, the circuit breaker, the clone machinery, and the metric collector that reads live `Bucket` objects at scrape time |
| [`internal/stackitfake/`](internal/stackitfake/fake.go) | Two `httptest` servers over one shared state — the control-plane REST API and the S3 data plane — so the reconciler's real code paths run offline |
| [`stackit/`](stackit/) | The provider, split along the line the provider itself draws: a bucket policy is an S3 operation and the control-plane SDK has no call for it ([docs/developer/stackit-api.md](docs/developer/stackit-api.md)) |
| [`config/`](config/) | Generated kubebuilder manifests. `config/crd/bases/` is the CRD source of truth; the kustomize overlays are not the supported install path |
| [`deploy/helm/`](deploy/helm/stackit-s3-provisioner/) | The chart. Its CRD is synced from `config/crd/bases/`; its ClusterRoles are not |
| [`test/`](test/) | Everything behind a build tag, so that a bare `go test ./...` stays offline |

## The core flows

One fact per step. Each list is the shape of the flow, not its detail — the linked page carries the
conditions, the sentinel errors and the orderings that are load-bearing.

### Startup — [docs/developer/package-map.md](docs/developer/package-map.md#cmd--the-manager-binary)

1. Flags are parsed; 22 of the 25 also accept an environment-variable fallback — the metrics
   address, the probe address and leader election do not.
2. `BucketNaming.Validate` runs; an invalid bucket-name prefix exits the process rather than
   producing unusable bucket names later
   ([ADR 0009](docs/adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md)). The prefix is
   the only thing it validates — whether the namespace is appended is a boolean, so there is no
   namespace policy that can be invalid.
3. The usage price is parsed from its string form; a typo exits instead of silently costing zero.
4. The manager is built with leader election optional and `LeaderElectionReleaseOnCancel` set, so a
   rolling update hands the lease over in seconds.
5. With `--stackit-sa-key-path` set, the key is loaded and the provider client built; without it the
   operator runs in **skeleton mode** and makes no cloud call at all
   ([ADR 0005 D6](docs/adr/0005-the-operator-serves-one-project-in-one-region.md)).
6. With a key, an empty operator namespace is fatal — the admin credential Secret has nowhere to go
   ([ADR 0004](docs/adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)).
7. One `ProviderBreaker` is constructed and shared by the reconciler and the metric collector
   ([docs/developer/circuit-breaker.md](docs/developer/circuit-breaker.md)).
8. `BucketReconciler` and `BucketUsageReconciler` are registered with the same manager but have
   separate queues ([ADR 0014](docs/adr/0014-bucket-size-is-measured-by-a-separate-controller.md)).
9. The bucket metric collector is registered against the controller-runtime registry; health and
   readiness probes are plain pings and say nothing about the provider.

### The provisioning pass — [docs/developer/reconcile-pipeline.md](docs/developer/reconcile-pipeline.md#the-provisioning-pass)

1. `Reconcile` routes before it works: deleted object, teardown, missing finalizer, skeleton mode,
   open circuit, otherwise the provisioning pass.
2. Spec guards run first and park the object without requeue-hammering: Secret key collision, a
   `secretRef` aimed at the admin Secret, a foreign region.
3. The physical bucket name is resolved and **frozen into an annotation before any cloud resource
   exists** ([docs/developer/bucket-identity.md](docs/developer/bucket-identity.md)).
4. The admin credential is ensured once per project and cached in-process
   ([docs/developer/credentials.md](docs/developer/credentials.md#the-admin-credential)).
5. Object Storage is ensured; only a structured 404 may lead to an enable call
   ([docs/developer/provider-errors.md](docs/developer/provider-errors.md)).
6. The bucket is created or adopted, and adoption happens only on matching ownership tags.
7. The workload credentials group is resolved by bucket tag, then by migration from the bucket's own
   policy, then by creation — never by display name
   ([ADR 0002](docs/adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)).
8. Read grants are resolved through each grantee's own bucket, never from anything a namespace user
   can write ([docs/developer/bucket-policy.md](docs/developer/bucket-policy.md#where-the-reader-principals-come-from)).
9. The isolation policy is written **before** any workload credential exists and before a clone
   starts, and only when it differs from the live document
   ([ADR 0003](docs/adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)).
10. A requested clone runs as a Job in the operator namespace; the pass ends and polls while it runs
    ([docs/developer/clone.md](docs/developer/clone.md)).
11. The access key is minted and the Secret written, clear-before-create, with the new key destroyed
    immediately if the Secret write fails
    ([ADR 0007](docs/adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)).
12. The terminal write sets `Ready=True`, clears any degradation and reports success to the breaker;
    the pass requeues after the drift-resync interval.

### The teardown pass — [docs/developer/reconcile-pipeline.md](docs/developer/reconcile-pipeline.md#the-teardown-pass)

1. In skeleton mode the finalizer is dropped at once and no provider call is made.
2. With the circuit open the finalizer is kept, no provider call is made, and the pass requeues on
   the cooldown ([ADR 0013 D4](docs/adr/0013-a-provider-outage-is-held-fleet-wide.md)).
3. A running clone is stopped first, because it would otherwise keep writing into the bucket.
4. **The emptiness decision comes before anything destructive**, so a deletion refused by a non-empty
   bucket leaves the workload fully functional — live key, unchanged Secret
   ([ADR 0006](docs/adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md)).
5. Only with `spec.wipeOnDelete`, the operator-wide gate on **and** matching ownership tags is the
   bucket emptied instead ([docs/security/ownership-and-attribution.md](docs/security/ownership-and-attribution.md)).
6. Access keys, then the credentials group — and only the group this bucket itself attributes.
7. The bucket, only if its tags prove this operator provisioned it.
8. The workload Secret, never the admin Secret.
9. Success is reported to the breaker, then the finalizer is dropped.

### The measurement pass — [docs/developer/usage-measurement.md](docs/developer/usage-measurement.md#one-pass-in-order)

1. A separate controller with its own queue and its own concurrency limit; it writes only
   `status.usage`, by merge patch.
2. Every decision *not* to measure is taken before any cloud call, which is what makes the short
   retry delays affordable.
3. Due time is read from `status.usage.lastMeasurementTime`, so the schedule survives a restart.
4. The pass reads the admin credential and **never bootstraps it** — measuring must not mint a cloud
   credential as a side effect ([ADR 0014](docs/adr/0014-bucket-size-is-measured-by-a-separate-controller.md)).
5. The provider has no usage endpoint, so a size is a full listing; the object cap turns an oversized
   bucket's numbers into lower bounds rather than into a long-running pass.
6. A measurement failure never reaches `Ready` and never returns a reconcile error.

---

## Extension checklists

### Add a field to the `Bucket` CRD

1. Add the field and its kubebuilder markers in [`api/v1/bucket_types.go`](api/v1/bucket_types.go).
   If a value is *derived* from it, the derivation belongs here too, as a method — not in the
   reconciler.
2. Run `make generate-all`. It regenerates [`config/crd/bases/`](config/crd/bases/), the DeepCopy
   code, and syncs the CRD into [`deploy/helm/stackit-s3-provisioner/templates/crd.yaml`](deploy/helm/stackit-s3-provisioner/templates/crd.yaml).
3. **Commit the generated output.** Nothing on a pull request checks this; the release does, and by
   then the image is already pushed — see [CI and release](#ci-and-release).
4. Add a unit test for the helper in [`api/v1/`](api/v1/), and a reconciler test against the provider
   fake in [`internal/controller/`](internal/controller/).
5. If the field changes behaviour a schema cannot express, add an envtest case under
   [`test/integration/`](test/integration/) — that is the layer where CEL rules are asserted
   deliberately. The Kind suites install the same CRD and therefore enforce CEL too, but nothing
   there is aimed at it, so a broken rule shows up as an unrelated failure.
6. Add the field to the CRD reference in [README.md](README.md), marked `# default` or `# example`.
7. Update the [docs/developer/](docs/developer/README.md) page of the subsystem it belongs to, and
   the [docs/operations/](docs/operations/README.md) page a user would read to use it.
8. If the field changes a rule — deletion, rotation, trust boundary, error semantics — it needs an
   ADR agreed with the maintainer *before* the code, not after.

### Add a control-plane call

1. Add the wrapper method to [`stackit/client.go`](stackit/client.go), bound to the client's own
   project and region. Control-plane calls are region-scoped: `(ctx, projectID, region, …)`.
2. Read [docs/developer/stackit-api.md](docs/developer/stackit-api.md#pitfalls) first. Several SDK
   calls have non-obvious requirements, and the list there was paid for.
3. Decide whether the call is safe under the retrying transport: it repeats **GET and HEAD only**,
   never a write, never a 429 ([`stackit/retry.go`](stackit/retry.go)).
4. Classify its failures in [`stackit/errors.go`](stackit/errors.go) if it can fail in a new way. The
   discriminator is the body shape, never the status code alone
   ([docs/developer/provider-errors.md](docs/developer/provider-errors.md)).
5. Model it in [`internal/stackitfake/fake.go`](internal/stackitfake/fake.go), including the failure
   you want a test to inject — otherwise no offline test can reach the new path.
6. Add an offline test against the fake, and an entry to the real-API suite in
   [`stackit/`](stackit/) behind `//go:build integration` if the provider's actual behaviour is the
   thing in question.
7. Whatever the real-API test creates, delete it through the **control plane** in `t.Cleanup` — see
   [Conventions](#conventions).
8. Run `go vet -tags integration ./...` by hand. Nothing in CI compiles the real-API suites.

### Add a metric and its alert

1. Declare the `prometheus.Desc` in [`internal/controller/metrics.go`](internal/controller/metrics.go)
   with the `stackit_s3_provisioner_` prefix.
2. Emit it in `Describe` **and** in `Collect`. A series only in `Describe` never appears; one only in
   `Collect` breaks registration.
3. The collector reads live `Bucket` objects at scrape time, so a per-bucket series carries the
   bucket's identifying labels and is simply absent when the object is gone. Absent is not zero —
   write the alert expression accordingly.
4. Add the alert to [`deploy/helm/stackit-s3-provisioner/templates/prometheusrule.yaml`](deploy/helm/stackit-s3-provisioner/templates/prometheusrule.yaml),
   wrapped in its own `{{- if $alerts.<name>.enabled }}` guard.
5. **Extend the `or` on the template's guard line.** The whole resource is skipped when every alert
   is disabled, because prometheus-operator rejects an empty rule group — a new alert that is not in
   that `or` disappears when it is the only one enabled.
6. Add the toggle to `monitoring.prometheusRule.alerts` in
   [`values.yaml`](deploy/helm/stackit-s3-provisioner/values.yaml).
7. Add the toggle to the chart-value reference in [README.md](README.md) and the alert's meaning to
   [docs/operations/monitoring.md](docs/operations/monitoring.md) — the key list lives in the README,
   its meaning in the operations page.
8. Run `make test-helm-render` to confirm the chart still renders.

### Add a Helm value that reaches the operator

1. Add the flag in [`cmd/main.go`](cmd/main.go) with a default and, where it is useful, an
   environment-variable fallback.
2. Validate it at startup if a bad value would only surface later as a confusing reconcile failure.
   The naming policy and the usage price both exit the process instead.
3. Add the key to [`values.yaml`](deploy/helm/stackit-s3-provisioner/values.yaml) with one line of
   comment and a link to the page that explains it.
4. Render the flag in [`templates/deployment.yaml`](deploy/helm/stackit-s3-provisioner/templates/deployment.yaml).
   A Go duration needs a unit — a bare number is rejected by the flag parser and the pod crash-loops.
5. Add the key to the chart-value reference in [README.md](README.md), marked `# default` or
   `# example`.
6. Explain what it does in [docs/operations/configuration.md](docs/operations/configuration.md), and
   say whether changing it needs a restart. Nothing is hot-reloaded today.
7. If the setting adds a Kubernetes permission, edit the chart's ClusterRole by hand — see the note
   under [Generated files](#generated-files).

### Add a developer or operations page

1. Check whether an existing page's *title* already covers the subject. If it does, it belongs there.
   If the title would have to be widened, it is a new page.
2. Name it after the noun somebody would search for: `docs/developer/<subsystem>.md`,
   `docs/operations/<subject>.md`.
3. Write it against the code, not against another page. Where you could not verify something, say so
   **in the sentence making the claim**.
4. Add the row to the directory README's *read it when* table in the same commit, and strike the
   subject from the gap list in [docs/developer/README.md](docs/developer/README.md) if it was named
   there.
5. Cite rules as `ADR NNNN Dn` rather than restating them. Never cite a ticket from a durable page.
6. End a [docs/security/](docs/security/README.md) page with what it does not cover; every page there
   has that section.
7. Do not restate the CRD or chart-value list. It lives in [README.md](README.md), and a second copy
   drifts invisibly.

---

## Build, test and lint

`make help` prints the full list. These are the targets you actually need.

| Target | What it runs | When you need it |
|---|---|---|
| `make build` | `gofmt -s -w .`, `go vet ./...`, then builds `bin/manager` | Checking that it compiles |
| `make run` | The manager against your current kubeconfig, debug logging | Driving the operator from your host |
| `make fmt` / `make vet` | `gofmt -s -w .` / untagged `go vet ./...` | Before pushing |
| `make lint` | `go vet`, `gofmt -l`, then golangci-lint v2 with the config in [`.golangci.yml`](.golangci.yml) | What the CI linter job runs |
| `make lint-fix` | golangci-lint with `--fix` | Mechanical cleanups |
| `make cyclo` | gocyclo, failing above `CYCLO_THRESHOLD ?= 15` `# default` | Before splitting a function you just grew |
| `make gosec` | gosec over the whole tree | Same scan CI runs |
| `make vuln` | govulncheck over the whole tree | Same scan CI runs |
| `make test-unit-coverage` | Offline tests plus the fake-backed tests, with coverage into `coverage/unit.out` | The default loop; needs no network |
| `make test-integration` | envtest, build tag `integration`, `./test/integration/...` only, 20m timeout; downloads the envtest binaries for Kubernetes `1.33.0` | Changing CRD schema, CEL rules or anything that needs a real API server |
| `make test-integration-coverage` | The same, with coverage into `coverage/integration.out` | What CI runs |
| `make test-helm-render` | `helm template` plus assertions on the rendered ClusterRoles, build tag `helm` | Any chart template change; needs `helm` on `PATH` |
| `make e2e-local` | Kind up, image built and loaded, chart render-checked, chart installed, `make test-e2e`, Kind down — **skeleton mode, no cloud call** | Before a change to the chart or the deployment path |
| `make e2e-stackit` | Kind up, image built and loaded, the rclone image pulled and preloaded, a `stackit-sa-key` Secret created from `SA_KEY ?= account-1.json` `# default`, chart installed against the **real** provider, `make test-e2e-stackit`, Kind down, then a sweep: it creates and deletes real buckets, groups and keys. Unlike `e2e-local` it does **not** render-check the chart and does not pass `--create-namespace` — a `kubectl` step creates the namespace first | Before releasing a change to provisioning, cloning, grants or measurement |
| `make e2e-stackit-sweep-dry` | Reports the cloud resources a crashed run left behind | After any interrupted cloud run |
| `make e2e-stackit-sweep` | Deletes them, including an orphaned admin group | When the dry run is not empty |
| `make generate-all` | `manifests`, `generate`, `sync-helm-crd` | **After any change under `api/v1/`** |

Two further targets, `make test` and `make test-unit`, are deliberately not in that table: neither
does what its name implies. Which part of each is misleading, and what to run instead, is in
[docs/developer/testing.md](docs/developer/testing.md#what-is-wrong-today-and-what-this-page-could-not-verify)
— that page owns the statement, so it is not repeated here.

**`-tags integration` does not select one suite.** Only one of the three it selects has a make
target and runs in CI, which is why [Add a control-plane call](#add-a-control-plane-call) ends in a
manual `go vet`. Which three they are, what compiles each and what each proves is
[docs/developer/testing.md](docs/developer/testing.md#one-tag-three-meanings) — that page owns the
statement, so it is not repeated here.

### Generated files

| File | Generated by | Never |
|---|---|---|
| [`api/v1/zz_generated.deepcopy.go`](api/v1/zz_generated.deepcopy.go) | `make generate` (controller-gen) | hand-edit |
| [`config/crd/bases/stackit-bucket.gtrfc.com_buckets.yaml`](config/crd/bases/) | `make manifests` | hand-edit |
| [`config/rbac/role.yaml`](config/rbac/role.yaml) | `make manifests`, from the `+kubebuilder:rbac` markers | hand-edit |
| [`deploy/helm/.../templates/crd.yaml`](deploy/helm/stackit-s3-provisioner/templates/crd.yaml) | `make sync-helm-crd`, from `config/crd/bases/` | hand-edit |
| [`.github/badges/coverage.json`](.github/badges/coverage.json) | the CI coverage job, committed by semantic-release | hand-edit |

The chart's own ClusterRole, [`templates/clusterrole.yaml`](deploy/helm/stackit-s3-provisioner/templates/clusterrole.yaml),
is **hand-maintained and is not derived from `config/rbac/role.yaml`**, and nothing compares the two
— verified on 2026-09-19 against the `sync-helm-crd` target, which copies only CRDs, and against
[`test/helm/render_test.go`](test/helm/render_test.go), which asserts on the operator role's shape
but never against the generated one. Since the chart is the supported install path, a new
`+kubebuilder:rbac` marker only takes effect in a real cluster once the chart template is edited too,
in the same change. The same asymmetry is why the two user-facing ClusterRoles,
[`clusterrole-view.yaml`](deploy/helm/stackit-s3-provisioner/templates/clusterrole-view.yaml) and
[`clusterrole-edit.yaml`](deploy/helm/stackit-s3-provisioner/templates/clusterrole-edit.yaml), exist
as chart templates only and were **deliberately not mirrored into `config/rbac/`**: `make manifests`
generates nothing there but `role.yaml`, from the `+kubebuilder:rbac` markers, so a viewer and an
editor role beside it would be hand-maintained copies on an install path nothing tests — both e2e
suites install through the chart, and the chart is the supported install path. Adding the mirror
therefore adds a second set of role definitions that no render check and no cluster run ever reads,
and the copy that silently drifts is the one in `config/rbac/`.

`deploy/helm/stackit-s3-provisioner/Chart.yaml` carries `version: 0.1.0` and `appVersion: "0.1.0"` in
git and is meant to: [`build.yml`](.github/workflows/build.yml) rewrites both from the release tag
before packaging, so bumping them by hand achieves nothing.

---

## CI and release

**[`release.yml`](.github/workflows/release.yml)** — "Test and Release" — runs on every push and pull
request to `main`, and on manual dispatch. Ten jobs, all `runs-on: self-hosted`: unit coverage,
envtest coverage, a Kind e2e run in skeleton mode (which also render-checks the chart), lint, gosec,
govulncheck, cyclo, a ClamAV scan of the source tree, a container scan that builds the image and runs
Trivy without pushing, and a combined-coverage job that merges the two coverage profiles, writes
`.github/badges/coverage.json` on a push to `main` and comments the delta against `main` on a pull
request. Coverage therefore measures the offline and envtest layers only; a mechanism covered solely
by the Kind, chart-render or real-API suites reads as uncovered.

The eleventh job, **Semantic Release**, runs only on a push to `main` and only after every other job
has passed. It runs `npx semantic-release` with the configuration in
[`.releaserc.json`](.releaserc.json): conventional-commit analysis, release notes, a GitHub release,
and a `chore(release): <version> [skip ci]` commit carrying the coverage badge. Commit messages
therefore decide the version — `fix:` a patch, `feat:` a minor, a `BREAKING CHANGE:` footer a major.
[`package.json`](package.json) exists only to pin those Node dev-dependencies; no JavaScript ships.

**[`build.yml`](.github/workflows/build.yml)** — "Release Docker & Helm" — runs only when a GitHub
release is published. It builds and pushes the multi-tag image to Docker Hub with provenance and an
SBOM, runs Docker Scout, then packages the chart, rewrites `version`, `appVersion` and `image.tag`
from the release tag, and publishes the chart to the `gh-pages` branch that serves the Helm
repository.

### `make generate-all` is a release gate

The chart-publishing job runs `make generate-all` and then fails if the working tree is dirty:

> `::error::Generated CRDs / DeepCopy / Helm chart CRD are out of sync with api/v1/.`

So **after any change under [`api/v1/`](api/v1/), run `make generate-all` and commit its output**, or
the release fails. Two properties of that gate are worth knowing before you rely on it: it is the
*only* check for generated-code drift — `release.yml` never runs `make generate-all`, so a pull
request stays green with a stale CRD — and it runs *after* the image push, in a job that needs the
build job. A release that trips it has already published an image and a GitHub release, and is
missing only the chart.

---

## Toolchain versions

The Go version literal lives in six files. Five are kept in step by Renovate, which groups them into
one "Go version" pull request; the sixth, the README badge, is not matched by any manager. Verified
on 2026-09-19 by reading each file and the custom managers in [`renovate.json`](renovate.json).

| File | Literal today | Form | Bumped by |
|---|---|---|---|
| [`go.mod`](go.mod) | `go 1.27.1` | full version | Renovate custom regex manager on `^go\.mod$` |
| [`Containerfile`](Containerfile) | `FROM golang:1.27.1-alpine` | full version | Renovate custom regex manager on `^Containerfile$` / `^Dockerfile$` |
| [`.github/workflows/release.yml`](.github/workflows/release.yml) | `GO_VERSION: '1.27.1'` | full version | Renovate custom regex manager on `^\.github/workflows/.*\.ya?ml$` |
| [`.github/workflows/build.yml`](.github/workflows/build.yml) | `GO_VERSION: '1.27.1'` | full version | the same manager |
| [`.github/release-template.hbs`](.github/release-template.hbs) | `go-1.26-blue` | major.minor only | Renovate, with `extractVersionTemplate` — it reads `1.26` today, one minor behind `go.mod` |
| [`README.md`](README.md) | `go-1.27.1-blue` badge | full version | **nothing** — no `managerFilePatterns` entry matches `README.md`; bump it by hand |

Renovate additionally groups every `golang.org/x/*` module bump into the same pull request, so
govulncheck sees a consistent set.

The other pinned versions, none of which is a Go version:

| Pin | Where | Bumped by |
|---|---|---|
| `ENVTEST_K8S_VERSION = 1.33.0` | [`Makefile`](Makefile) | by hand |
| `KUBERNETES_VERSION: '1.33.4'` — the kubectl and Kind node image of the e2e job | [`release.yml`](.github/workflows/release.yml) | by hand |
| `KUSTOMIZE_VERSION`, `CONTROLLER_GEN_VERSION`, `GOLANGCI_LINT_VERSION`, `GOCYCLO_VERSION`, `GOSEC_VERSION` | [`Makefile`](Makefile), each above a `# renovate:` comment | Renovate's custom regex manager; minor and patch automerge, major needs review |
| `ENVTEST_VERSION = release-0.24` | [`Makefile`](Makefile), above a `# renovate:` comment like the five above | by hand — the manager's regex only captures a `v`-prefixed value, so the `release-N.NN` form never matches and the comment above it has no effect |
| The rclone image used by clone Jobs | the chart's [`values.yaml`](deploy/helm/stackit-s3-provisioner/values.yaml), and `DefaultCloneImage` in [`internal/controller/clone.go`](internal/controller/clone.go) as the fallback | Renovate, both of them: the chart value through the `helm-values` manager, `DefaultCloneImage` through a custom regex manager that exists to keep the fallback in step with the chart tag. The Makefile deliberately reads the image out of the rendered chart rather than pinning it a third time |

---

## Conventions

**English everywhere.** Code, comments, commit messages, documentation. Regardless of the language
the work is discussed in.

**Commits are conventional commits**, because semantic-release derives the version from them.

**Integration tests live behind their build tag**, so `go test ./...` stays offline and needs no
credentials. That is the property that makes the default loop fast and safe to run on any machine,
and it is why an offline test that quietly reaches the network is a defect rather than an
inconvenience. Offline reconciler tests run against [`internal/stackitfake/`](internal/stackitfake/fake.go),
not against mocks of our own call sites.

**Tests clean up through the control plane**, in `t.Cleanup`, in the order buckets → keys → groups.
Deliberately not through the data plane: a data-plane cleanup would depend on the very bucket policy
under test, so a test that broke the policy would also lose the ability to remove what it created.
The real-API suites in [`stackit/`](stackit/) name their buckets
`s3op-test-<first 8 of the project id>-<random 0-999999>` `# example`; the real-API reconciler test in
[`internal/controller/`](internal/controller/) names its CR, and therefore its bucket,
`s3op-itest-<random 0-999999>` `# example`. Sweeping leftovers by one prefix alone misses the other
half. The details, including the sweep that finds them, are in
[docs/developer/testing.md](docs/developer/testing.md#cleanup-conventions).

**Never run two real-API suites against one project at the same time.** They share a project and an
admin credential.

**`account-*.json` are never committed.** They are real service-account keys with embedded RSA
private keys, and the only thing keeping them out of git is one line in [`.gitignore`](.gitignore).
Their custody is [docs/security/credentials-and-secrets.md](docs/security/credentials-and-secrets.md).

**Read the [docs/developer/](docs/developer/README.md) page for a subsystem before you change it, and
update it in the same change.** Those pages name files and functions on purpose, which is what makes
them worth reading and also what makes them rot.

**Documentation ships with the change it describes** — not afterwards, not in a follow-up. A decision
goes into an ADR in the session it is taken, a user-visible consequence into
[docs/operations/](docs/operations/README.md) or [README.md](README.md), a mechanism into
[docs/developer/](docs/developer/README.md). **Nothing enforces this but review.** There is no lint
rule, no CI check and no hook: if a reviewer does not ask for the page, it does not get written.

---

## Licence

Apache 2.0 — see [LICENSE](LICENSE).
