# Build, CI and release

The entry points a contributor uses, what CI does with them, how a version is cut, and which file
carries which pinned version. Read it before you push, before you cut a release, and whenever a bump
touches more than one file at once.

Two neighbouring pages own the parts deliberately missing here. **Which suite exists, what each one
proves, what it costs and which of the eleven CI jobs runs it** is
[testing.md](testing.md#what-ci-runs) — this page never re-enumerates the jobs. **Which file is
generated from which source, and the drift between `config/rbac/` and the chart** is
[package-map.md](package-map.md#what-is-wrong-today). What a released setting *does* is
[configuration.md](../operations/configuration.md), and the complete value list is
[README.md](../../README.md).

This page names files and targets on purpose, so it goes stale when they move: whoever renames a
target, a workflow job or a pin updates it in the same change.

## The targets you actually need

`make help` prints the authoritative list; the [Makefile](../../Makefile) is the source. These are
the non-test entry points. The test, e2e and sweep targets are
[testing.md](testing.md#the-layers)'s, one per layer, and the two whose names mislead — `make test`
and `make test-unit` — are picked apart in that page's
[what is wrong today](testing.md#what-is-wrong-today-and-what-this-page-could-not-verify).

| Target | What it runs | When you need it |
|---|---|---|
| `make build` | `make fmt`, `make vet`, then builds `bin/manager` | Checking that it compiles |
| `make run` | `make fmt`, `make vet`, then the manager against your current kubeconfig with `--zap-log-level=debug` | Driving the operator from your host |
| `make fmt` / `make vet` | `gofmt -s -w .` / untagged `go vet ./...` | Before pushing |
| `make lint` | `go vet ./...`, `gofmt -l .`, then golangci-lint v2 with the config in [`.golangci.yml`](../../.golangci.yml) | What the CI linter job runs |
| `make lint-fix` | golangci-lint with `--fix` | Mechanical cleanups |
| `make cyclo` | gocyclo, failing above `CYCLO_THRESHOLD ?= 15` `# default` | Before splitting a function you just grew |
| `make gosec` | gosec over the whole tree | Same scan CI runs |
| `make vuln` | govulncheck over the whole tree | Same scan CI runs |
| `make generate-all` | `manifests`, `generate`, `sync-helm-crd` | **After any change under `api/v1/`** — see the release gate below |

`make lint` and `make vet` are **untagged**, and [`.golangci.yml`](../../.golangci.yml) sets no
`build-tags`, so neither ever compiles a suite behind a build tag. `go vet -tags integration ./...`
by hand is the only check that the real-API suites still build.

## What CI is

**[`release.yml`](../../.github/workflows/release.yml)** — "Test and Release" — runs on every push
and pull request to `main`, and on manual dispatch. Eleven jobs, all `runs-on: self-hosted`; the job
list and what each one executes is [testing.md](testing.md#what-ci-runs).

**[`build.yml`](../../.github/workflows/build.yml)** — "Release Docker & Helm" — runs **only** when a
GitHub release is published, in two jobs. `build` builds and pushes the multi-tag image to Docker Hub
with `provenance: true` and `sbom: true`, attaches an SBOM and runs Docker Scout.
`release-helm-gh`, which needs `build`, checks the generated artefacts (below), rewrites the chart's
`version`, `appVersion` and `image.tag` from the release tag with `sed`, packages the chart and
force-pushes it to the `gh-pages` branch that serves the Helm repository. Those three values are
therefore never bumped by hand ([package-map.md](package-map.md#the-tree)).

**[`renovate.yml`](../../.github/workflows/renovate.yml)** runs the dependency bot; its policy is
[`renovate.json`](../../renovate.json) and the pins it reaches are the last section of this page.

## How a version is cut

**Commit messages decide the version**, because the eleventh job of `release.yml` is
semantic-release. Conventional commits are therefore a correctness requirement, not a style
preference: `fix:` cuts a patch, `feat:` a minor, a `BREAKING CHANGE:` footer a major, and a commit
outside the convention cuts nothing at all.

The **Semantic Release** job runs only on a push to `main` and only after every one of the other ten
jobs has passed. It runs `npx semantic-release` with [`.releaserc.json`](../../.releaserc.json):
conventional-commit analysis, release notes, a GitHub release, and a
`chore(release): <version> [skip ci]` commit whose only asset is
[`.github/badges/coverage.json`](../../.github/badges/coverage.json) — the badge the combined-coverage
job produced earlier in the same run. Publishing that release is what triggers `build.yml`.
[`package.json`](../../package.json) exists only to pin those Node dev-dependencies; no JavaScript
ships.

`.github/badges/coverage.json` is therefore a generated file that lives in git: written by CI,
committed by semantic-release, and never hand-edited.

### `make generate-all` is a release gate

The chart-publishing job runs `make generate-all` and then fails if the working tree is dirty:

> `::error::Generated CRDs / DeepCopy / Helm chart CRD are out of sync with api/v1/.`

So **after any change under [`api/v1/`](../../api/v1/), run `make generate-all` and commit its
output**, or the release fails. Two properties of that gate are worth knowing before you rely on it:

- It is the **only** check for generated-code drift. `release.yml` has no generate or diff step at
  all, so a pull request with a stale CRD stays green.
- It runs **after** the image push, in the job that needs `build`. A release that trips it has
  already published an image and a GitHub release, and is missing only the chart.

## Toolchain and dependency pins

The Go version literal lives in **five** files, and Renovate matches every one of them: all five
managers emit `depName: golang` from the `golang-version` datasource, and a package rule groups that
datasource into a single "Go version" pull request so they cannot drift apart. Verified on
2026-09-19 by reading each file and the custom managers in [`renovate.json`](../../renovate.json).

| File | Literal today | Form | Matched by |
|---|---|---|---|
| [`go.mod`](../../go.mod) | `go 1.27.1` | full version | custom regex manager on `^go\.mod$` |
| [`Containerfile`](../../Containerfile) | `FROM golang:1.27.1-alpine` | full version | custom regex manager on `^Containerfile$` / `^Dockerfile$` |
| [`.github/workflows/release.yml`](../../.github/workflows/release.yml) | `GO_VERSION: '1.27.1'` | full version | custom regex manager on `^\.github/workflows/.*\.ya?ml$` |
| [`.github/workflows/build.yml`](../../.github/workflows/build.yml) | `GO_VERSION: '1.27.1'` | full version | the same manager |
| [`.github/release-template.hbs`](../../.github/release-template.hbs) | `go-1.26-blue` | major.minor only, via `extractVersionTemplate` | custom regex manager on that file — it reads `1.26` today, one minor behind `go.mod` |

[README.md](../../README.md) is deliberately not in that table: verified on 2026-09-19 it carries
four badges — build status, coverage, Go Report Card and licence — and no Go-version badge, so the
front page holds no Go literal to bump.

Renovate additionally groups every `golang.org/x/*` module bump into the same pull request, so
govulncheck sees a consistent set.

### The untagged Kubernetes modules move with the release line, not on their own

Three modules in [`go.mod`](../../go.mod) carry no semver tag and appear only as a pseudo-version:
`k8s.io/kube-openapi`, `k8s.io/utils` and `sigs.k8s.io/json`. They have no release contract of their
own. Every `k8s.io/*` release pins one commit of each, and only that combination is compiled
upstream — `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go` and `k8s.io/apiextensions-apiserver`
at `v0.37.0` all name the same three pseudo-versions, and [`go.mod`](../../go.mod) carries exactly
those.

Renovate classifies a pseudo-version update as a digest and would otherwise walk each of them to the
newest commit on master, where they compile against Kubernetes master alone. That is not a
hypothetical: `kube-openapi` master moved `pkg/schemaconv` to `sigs.k8s.io/structured-merge-diff/v7`
while `k8s.io/apimachinery v0.37.0` still constructs its type converter from
`structured-merge-diff/v6`, so `go build` failed inside apimachinery and took lint, unit tests,
envtest, govulncheck, the e2e image build and the container scan with it. The grouping rule matches
these modules too, so one unbumpable module also blocks every other Kubernetes update sharing the
pull request.

A package rule in [`renovate.json`](../../renovate.json) therefore disables updates for those three
module paths. They still move — `gomodTidy` raises them whenever a `k8s.io/*` bump lifts the minimum
version — but only as a consequence of the release line, never ahead of it.

**Security note.** Disabling the rule means a fix published in one of the three no longer arrives on
its own. What catches it instead is the vulnerability job: `make vuln` runs govulncheck on every pull
request and fails the pipeline if our code calls a known vulnerability, in a direct or an indirect
module. The residual gap is a vulnerability that govulncheck does not report as called; raising the
pin by hand is then the only route, and nothing prompts for it.

The other pinned versions, none of which is a Go version:

| Pin | Where | Bumped by |
|---|---|---|
| `ENVTEST_K8S_VERSION = 1.33.0` | [`Makefile`](../../Makefile) | by hand |
| `k8s.io/kube-openapi`, `k8s.io/utils`, `sigs.k8s.io/json`, each a pseudo-version | [`go.mod`](../../go.mod) | `go mod tidy`, only when a `k8s.io/*` release raises the minimum — Renovate is switched off for these three module paths, for the reason above |
| `KUBERNETES_VERSION: '1.33.4'` — the kubectl and Kind node image of the e2e job | [`release.yml`](../../.github/workflows/release.yml) | by hand |
| `KUSTOMIZE_VERSION`, `CONTROLLER_GEN_VERSION`, `GOLANGCI_LINT_VERSION`, `GOCYCLO_VERSION`, `GOSEC_VERSION` | [`Makefile`](../../Makefile), each under a `# renovate:` comment | the Makefile custom regex manager; minor and patch automerge, major needs review |
| `ENVTEST_VERSION = release-0.24` | [`Makefile`](../../Makefile), under a `# renovate:` comment like the five above | by hand — the manager's regex captures `v[\d.]+` only, so the `release-N.NN` form never matches and the comment above it has no effect |
| The rclone image used by clone Jobs | the chart's [`values.yaml`](../../deploy/helm/stackit-s3-provisioner/values.yaml), and `DefaultCloneImage` in [`internal/controller/clone.go`](../../internal/controller/clone.go) as the fallback | Renovate, both of them: the chart value through the `helm-values` manager, `DefaultCloneImage` through a custom regex manager that exists to keep the fallback in step with the chart tag. The Makefile deliberately reads the image out of the rendered chart rather than pinning it a third time |

## What is wrong today

- **A stale generated CRD merges green**, and the gate that catches it runs after the image is
  pushed — the two properties above, in one sentence.
- **The release-notes badge lags a minor behind `go.mod`.** `release-template.hbs` says `1.26` while
  `go.mod` says `1.27.1`. The manager is configured correctly; the badge simply moves only when
  Renovate next opens a Go-version pull request.
- **Nothing type-checks the build-tagged suites**, because every lint and vet target is untagged.
  The consequence for a contributor is a manual `go vet -tags integration ./...`
  ([testing.md](testing.md#what-is-wrong-today-and-what-this-page-could-not-verify)).
- **Not verified in this pass, and this is the gap:** no release was cut while writing this page.
  Everything above is read off the two workflow files, [`.releaserc.json`](../../.releaserc.json),
  the [`Makefile`](../../Makefile) and [`renovate.json`](../../renovate.json) on 2026-09-19; no claim
  here rests on having watched a publish run.
