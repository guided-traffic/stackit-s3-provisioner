# Extension checklists

The ordered steps for the changes people actually make here. Each list exists because one of its
steps is regularly forgotten and the omission is not caught by any test: a generated artefact that
was not committed, an alert that disappears when it is the only one enabled, a flag that renders
without a unit and crash-loops the pod.

A checklist says *what* to do and in which order. *Why* the mechanism is shaped that way is the
subsystem page linked at the step — [reconcile-pipeline.md](reconcile-pipeline.md),
[stackit-api.md](stackit-api.md), [testing.md](testing.md) and the rest — and the rule behind it is
an [ADR](../adr/README.md). Read the subsystem page before you start; it is what makes the step you
are about to take obvious rather than mechanical.

**A requirement that collides with an ADR is not implemented quietly.** Name the record and the rule
(`ADR 0003 D6`), get the record amended first, and change the code in the same commit as the record.

## Add a field to the `Bucket` CRD

1. Add the field and its kubebuilder markers in [`api/v1/bucket_types.go`](../../api/v1/bucket_types.go).
   If a value is *derived* from it, the derivation belongs here too, as a method — not in the
   reconciler.
2. Run `make generate-all`. It regenerates [`config/crd/bases/`](../../config/crd/bases/), the
   DeepCopy code, and syncs the CRD into
   [`deploy/helm/stackit-s3-provisioner/templates/crd.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/crd.yaml).
3. **Commit the generated output.** Nothing on a pull request checks this; the release does, and by
   then the image is already pushed — see
   [build-and-release.md](build-and-release.md#make-generate-all-is-a-release-gate).
4. Remember that the field's doc comment is published to every cluster that installs the chart:
   `controller-gen` copies it into the CRD and it surfaces in `kubectl explain`
   ([package-map.md](package-map.md#what-is-wrong-today)). Write it for a cluster operator.
5. Add a unit test for the helper in [`api/v1/`](../../api/v1/), and a reconciler test against the
   provider fake in [`internal/controller/`](../../internal/controller/).
6. If the field changes behaviour a schema cannot express, add an envtest case under
   [`test/integration/`](../../test/integration/) — that is the layer where CEL rules are asserted
   deliberately. The Kind suites install the same CRD and therefore enforce CEL too, but nothing
   there is aimed at it, so a broken rule shows up as an unrelated failure
   ([testing.md](testing.md#layer-3--envtest-against-a-real-api-server)).
7. Add the field to the CRD reference in [README.md](../../README.md), marked `# default` or
   `# example`. That table is the only home of the field list.
8. Update the [docs/developer/](README.md) page of the subsystem it belongs to, and the
   [docs/operations/](../operations/README.md) page a user would read to use it.
9. If the field changes a rule — deletion, rotation, trust boundary, error semantics — it needs an
   ADR agreed with the maintainer *before* the code, not after.

## Add a control-plane call

1. Add the wrapper method to [`stackit/client.go`](../../stackit/client.go), bound to the client's
   own project and region. Control-plane calls are region-scoped: `(ctx, projectID, region, …)`.
2. Read [stackit-api.md](stackit-api.md#pitfalls) first. Several SDK calls have non-obvious
   requirements, and the list there was paid for.
3. Decide whether the call is safe under the retrying transport: it repeats **GET and HEAD only**,
   never a write, never a 429 ([`stackit/retry.go`](../../stackit/retry.go), and the reasoning is
   [package-map.md](package-map.md#the-boundaries-worth-knowing)).
4. Classify its failures in [`stackit/errors.go`](../../stackit/errors.go) if it can fail in a new
   way. The discriminator is the body shape, never the status code alone
   ([provider-errors.md](provider-errors.md)).
5. Model it in [`internal/stackitfake/fake.go`](../../internal/stackitfake/fake.go), including the
   failure you want a test to inject — otherwise no offline test can reach the new path
   ([testing.md](testing.md#the-injection-api)).
6. Add an offline test against the fake, and an entry to the real-API suite in
   [`stackit/`](../../stackit/) behind `//go:build integration` if the provider's actual behaviour is
   the thing in question.
7. Whatever the real-API test creates, delete it through the **control plane** in `t.Cleanup`, in
   the order buckets → keys → groups ([testing.md](testing.md#cleanup-conventions)).
8. Run `go vet -tags integration ./...` by hand. Nothing in CI compiles the real-API suites
   ([testing.md](testing.md#one-tag-three-meanings)).

## Add a metric and its alert

1. Decide which collector owns it — see [the note below](#where-a-metric-is-registered-decides-what-it-can-say).
   A series derived from `Bucket` objects belongs in
   [`internal/controller/metrics.go`](../../internal/controller/metrics.go); declare its
   `prometheus.Desc` there, with the `stackit_s3_provisioner_` prefix.
2. Emit it in `Describe` **and** in `Collect`. A series only in `Describe` never appears; one only in
   `Collect` breaks registration.
3. The collector in `metrics.go` reads live `Bucket` objects at scrape time, so a per-bucket series
   carries the bucket's identifying labels and is simply absent when the object is gone. Absent is
   not zero — write the alert expression accordingly
   ([monitoring.md](../operations/monitoring.md)).
4. Add the alert to
   [`deploy/helm/stackit-s3-provisioner/templates/prometheusrule.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/prometheusrule.yaml),
   wrapped in its own `{{- if $alerts.<name>.enabled }}` guard.
5. **Extend the `or` on the template's guard line.** The whole resource is skipped when every alert
   is disabled, because prometheus-operator rejects an empty rule group — a new alert that is not in
   that `or` disappears when it is the only one enabled.
6. Add the toggle to `monitoring.prometheusRule.alerts` in
   [`values.yaml`](../../deploy/helm/stackit-s3-provisioner/values.yaml), together with any
   threshold the expression reads. A threshold belongs in the values and is rendered into the
   expression — `saKeyExpiring.leadTimeDays` becomes `{{ mul $alerts.saKeyExpiring.leadTimeDays 86400 }}`
   in the rule — so the operator tunes it without a fork of the template.
7. Add the toggle to the chart-value reference in [README.md](../../README.md) and the alert's
   meaning to [monitoring.md](../operations/monitoring.md) — the key list lives in the README, its
   meaning in the operations page. Both pages state how many alerts there are and how many ship on;
   keep the counts right in the same change.
8. Run `make test-helm-render` to confirm the chart still renders.

### Where a metric is registered decides what it can say

`RegisterBucketMetrics` is not the only registration point, and using it for everything is a
mistake with one specific shape: a series exported as `0` while the mechanism it measures is not
running reads as *healthy*, which is the one answer it must never give.

So a metric goes into its own collector, registered separately, when it describes process state
rather than object state **and** the process may be running without that state at all.
[`sakey_reload.go`](../../internal/controller/sakey_reload.go) is the precedent:
`SAKeyReloadObserver` is a `prometheus.Collector` over its own counters, and
`RegisterSAKeyReloadMetrics` is called from [`cmd/main.go`](../../cmd/main.go) only when the
service-account key reload is actually wired up — in skeleton mode, or at
`--stackit-sa-key-reload-interval` `0`, none of its four series is exported at all.

The same collector shows the per-series form of the rule. The expiry gauge is emitted only when the
loaded key carries a `validUntil`, so an operator whose key has no expiry exports nothing there and
the two expiry alerts, which evaluate `min(<gauge> - time())`, match no series and stay silent —
instead of firing on a `0` that would read as "expired in 1970". Absent is a supported answer, and
writing the expression against a gauge that may be missing is part of adding the metric, not a
follow-up ([ADR 0016 D10](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)).

## Add a Helm value that reaches the operator

1. Add the flag in [`cmd/main.go`](../../cmd/main.go) with a default and, where it is useful, an
   environment-variable fallback. Twenty-three of the twenty-six flags have one
   ([package-map.md](package-map.md#cmd--the-manager-binary)).
2. Validate it at startup if a bad value would only surface later as a confusing reconcile failure.
   The naming policy and the usage price both exit the process instead.
3. Add the key to [`values.yaml`](../../deploy/helm/stackit-s3-provisioner/values.yaml) with one line
   of comment and a link to the page that explains it.
4. Render the flag in
   [`templates/deployment.yaml`](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml).
   A Go duration needs a unit — a bare number is rejected by the flag parser and the pod crash-loops.
5. Add the key to the chart-value reference in [README.md](../../README.md), marked `# default` or
   `# example`.
6. Explain what it does in [configuration.md](../operations/configuration.md), and say that changing
   it needs a restart — because it does. **Exactly one input of this operator is re-read while it
   runs, the service-account key, and a new setting is not going to be the second**
   ([ADR 0016 D1](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)).
   The line is not squeamishness about reload code: the key is the only input that changes without
   anybody touching the deployment, because an external rotation mechanism writes it, and every
   other setting arrives through a rollout that restarts the process anyway. `--ownership-name` is
   the case that settles it — it is part of the bucket ownership key, so re-reading it at runtime
   would make the operator treat its own buckets as foreign. If a value really does need to take
   effect without a restart, that is an amendment to D1 agreed with the maintainer first, not a
   ticker added quietly beside the flag.
7. If the setting adds a Kubernetes permission, edit the chart's ClusterRole by hand. `make
   manifests` regenerates only `config/rbac/role.yaml`, and nothing copies it into the chart
   ([package-map.md](package-map.md#what-is-wrong-today)).

## Add a developer or operations page

1. Check whether an existing page's *title* already covers the subject. If it does, it belongs there.
   If the title would have to be widened, it is a new page.
2. Name it after the noun somebody would search for: `docs/developer/<subsystem>.md`,
   `docs/operations/<subject>.md`.
3. Write it against the code, not against another page. Where you could not verify something, say so
   **in the sentence making the claim**.
4. Add the row to the directory README's *read it when* table in the same commit, and strike the
   subject from the gap list in [docs/developer/README.md](README.md) if it was named there.
5. Cite rules as `ADR NNNN Dn` rather than restating them. Never cite a ticket from a durable page.
6. End a [docs/security/](../security/README.md) page with what it does not cover; every page there
   has that section.
7. Do not restate the CRD or chart-value list. It lives in [README.md](../../README.md), and a second
   copy drifts invisibly.

## What is wrong today

**Nothing enforces any of this.** There is no lint rule, no CI check and no hook that notices a
missing README row, an alert outside the `or`, or a subsystem page that was not updated with the code
it describes. These lists hold by review, or not at all — the one exception is the generated output
of step 2 above, and that is caught at release time rather than at review time
([build-and-release.md](build-and-release.md#make-generate-all-is-a-release-gate)).
