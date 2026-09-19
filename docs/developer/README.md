# Developer documentation

Eleven pages, one per subsystem, written for somebody about to change that subsystem. They cover
what the code cannot state on its own: the order a pass runs in, the invariant a format rests on,
why a boundary sits where it does, and the hard-won detail behind a line that looks arbitrary. They
do not carry decisions — those are the fourteen records in [../adr/](../adr/README.md), and a page
here cites a rule as `ADR 0002 D4` rather than restating it. They do not carry operator instructions
either; those are [../operations/](../operations/README.md).

**The staleness contract.** Unlike an ADR, a page here names files, functions and sometimes line
numbers **on purpose** — that is what makes it worth reading before you touch the code, and it is
also what makes it rot. So: **whoever moves the tree updates the page in the same change**, and
**you read the page for a subsystem before you change that subsystem**. Nothing enforces either;
they hold by review, or not at all. Where a page could not verify something, it says so in the
sentence making the claim, and most pages close with a `What is wrong today` section — read that
first if you are here because something is behaving oddly.

## Read it when

| Page | Read it when |
|---|---|
| [package-map.md](package-map.md) | You are new, or you are hunting for the file that owns a behaviour: the Go packages, the generated manifests, the chart, the test tree, and why the provider client has two halves |
| [reconcile-pipeline.md](reconcile-pipeline.md) | You are changing the `Bucket` controller's control flow: the order of the provisioning and teardown passes, which failure lands in which terminal state, and which events are allowed to wake the controller at all — watches and predicates, the queue, the drift resync, leader election |
| [bucket-identity.md](bucket-identity.md) | You need to know which object in the STACKIT project a `Bucket` CR is, and how the operator proves that object is its own: name composition, the freeze, the four bucket tags, collision detection |
| [bucket-policy.md](bucket-policy.md) | You are touching the one document that separates one workload from another: how it is built, which action belongs in which exemption list and why, how it is compared against the live one, and what must never be added to it |
| [credentials.md](credentials.md) | You are touching anything that mints, publishes or replaces an S3 key: the Secret data contract, the write path, the key-replacement ordering, rotation bookkeeping, the admin bootstrap |
| [clone.md](clone.md) | You are changing the machinery behind `spec.cloneFrom`: the Job, the staging Secret, progress polling, the ordering that makes a crash mid-clone safe, and the locked-down control port |
| [usage-measurement.md](usage-measurement.md) | You are changing the second controller: how a measurement pass is scheduled, how it stays out of the provisioning controller's way, what the listing costs, and how the cost figure is computed |
| [circuit-breaker.md](circuit-breaker.md) | You are changing anything that runs while the provider is down: the fleet-wide breaker, the workqueue rate limiter, the two transport changes, and the two circuit metrics |
| [provider-errors.md](provider-errors.md) | You are changing an error path: where a status code comes from, why the discriminator is the body shape rather than the code, what a revoked service-account key actually looks like, and how a classified error becomes a readiness decision |
| [stackit-api.md](stackit-api.md) | You are calling the provider: the two planes, the resource model, how the process authenticates, and the SDK pitfalls that cost time |
| [testing.md](testing.md) | You need to know which suite proves what, what each costs to run, and the conventions that stop a run from leaving real cloud resources behind |

## What has no page here

Determined on 2026-09-19 by walking `api/`, `cmd/`, `internal/`, `stackit/`, `config/`, `deploy/`,
`hack/` and `test/` and striking out what the eleven pages above already cover. This list is the
section most likely to be written once and never trued up — re-derive it the same way rather than
trusting it.

| Subject | Where its material is today |
|---|---|
| The metric registry as a whole — [`internal/controller/metrics.go`](../../internal/controller/metrics.go), the custom collector and the twenty-odd series it exports | The catalogue, what each series means, "absent is not zero" and the shipped alerts are [../operations/monitoring.md](../operations/monitoring.md). The file gets one row in [package-map.md](package-map.md); the two circuit series are in [circuit-breaker.md](circuit-breaker.md) and the usage series in [usage-measurement.md](usage-measurement.md). No page here owns the collector itself |
| The Kubernetes Events the operator emits, as a vocabulary | [../operations/bucket-status.md](../operations/bucket-status.md), section *Events*. Each individual event is also named in the page of the mechanism that raises it |
| The `Bucket` API types — field semantics, the kubebuilder markers, the two CEL validation rules | The complete field list is the reference in [../../README.md](../../README.md); the status fields are [../operations/bucket-status.md](../operations/bucket-status.md); the self-grant CEL rule is in [bucket-policy.md](bucket-policy.md) and the immutability rule in [reconcile-pipeline.md](reconcile-pipeline.md); the file's responsibility is a row in [package-map.md](package-map.md). Adding a field is an extension checklist in [../../DEVELOPER.md](../../DEVELOPER.md) |
| Skeleton mode as a code path — the `r.Stackit == nil` branch | The rule is [ADR 0005 D6](../adr/0005-the-operator-serves-one-project-in-one-region.md); the branch is stated where it bites, in [reconcile-pipeline.md](reconcile-pipeline.md), [usage-measurement.md](usage-measurement.md) and [testing.md](testing.md); installing that way is [../operations/deployment.md](../operations/deployment.md) |
| The Helm chart's templates beyond a file-per-row table | One table in [package-map.md](package-map.md); what a value does and what needs a restart is [../operations/configuration.md](../operations/configuration.md); the chart's own `values.yaml` is, for several settings, the only place the reason was written down |
| The kustomize tree under `config/` | [package-map.md](package-map.md). It is generated by kubebuilder and is not the supported install path — [../operations/deployment.md](../operations/deployment.md) |
| The operator's privilege footprint — the `+kubebuilder:rbac` markers and the ClusterRole they generate | [../security/rbac-and-privilege.md](../security/rbac-and-privilege.md); the RBAC a clone adds is in [clone.md](clone.md), and the chart's role templates are a row in [package-map.md](package-map.md) |
| The release pipeline and the toolchain pins — semantic-release, the chart and image publish, which file carries the Go version literal | [../../DEVELOPER.md](../../DEVELOPER.md). [testing.md](testing.md) enumerates the CI jobs, but only from the angle of which suite each one runs |

Three subjects that look like gaps and are not: **leader election** and **the drift resync loop**
are both in [reconcile-pipeline.md](reconcile-pipeline.md) (operator-facing tuning for each is in
[../operations/configuration.md](../operations/configuration.md)), and **the cloud cleanup sweep**
in [`hack/e2ecleanup`](../../hack/e2ecleanup/main.go) is in [testing.md](testing.md).

## The other homes

| Where | What it holds |
|---|---|
| [../../DEVELOPER.md](../../DEVELOPER.md) | The contributor guide: repository layout, the build, test and lint matrix, CI and release, and the ordered extension checklists. It is the *whole-repo* view; this directory is per-subsystem depth. Start there if you have not built the project yet |
| [../adr/](../adr/README.md) | The decisions — what the operator does, why, what was rejected. Binding, and free of code references on purpose. Every rule a page here explains is cited back to one |
| [../operations/](../operations/README.md) | What somebody running or integrating the operator needs: prerequisites, deployment, configuration, status, deletion, credentials, cloning, read grants, usage and cost, provider outages, monitoring |
| [../security/](../security/README.md) | The security design, one page per perspective, each closing with what it does not cover and with the gaps it leaves numbered `H-n` |
| [../tickets/](../tickets/README.md) | Work still outstanding. A page in this directory never cites one — it states what is |
| [../../README.md](../../README.md) | The front page, and the single home of the complete CRD and chart-value reference. A page here explains a setting; it never restates the list |
