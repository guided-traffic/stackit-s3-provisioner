# Build, CI and release

The entry points a contributor uses, what CI does with them, how a version is cut, and which file
carries which pinned version. Read it before you push, before you cut a release, and whenever a bump
touches more than one file at once.

Two neighbouring pages own the parts deliberately missing here. **Which suite exists, what each one
proves, what it costs and which of the twelve CI jobs runs it** is
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
| `make gosec` | builds gosec from [`hack/tools/`](../../hack/tools/go.mod), then runs it over the whole tree | Same scan CI runs |
| `make vuln` | govulncheck over the whole tree | Same scan CI runs |
| `make generate-all` | `manifests`, `generate`, `sync-helm-crd` | **After any change under `api/v1/`** — see the release gate below |

`make lint` and `make vet` are **untagged**, and [`.golangci.yml`](../../.golangci.yml) sets no
`build-tags`, so neither ever compiles a suite behind a build tag. `go vet -tags integration ./...`
by hand is the only check that the real-API suites still build.

## What CI is

**[`release.yml`](../../.github/workflows/release.yml)** — "Test and Release" — runs on every push
and pull request to `main`, and on manual dispatch. Twelve jobs, all `runs-on: self-hosted`; the job
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

**Commit messages decide the version**, because the last job of `release.yml` is
semantic-release. Conventional commits are therefore a correctness requirement, not a style
preference: `fix:` cuts a patch, `feat:` a minor, a `BREAKING CHANGE:` footer a major, and a commit
outside the convention cuts nothing at all.

The **Semantic Release** job runs only on a push to `main` and only after every one of the other
eleven jobs has passed. It runs `npx semantic-release` with [`.releaserc.json`](../../.releaserc.json):
conventional-commit analysis, release notes, a GitHub release, and a
`chore(release): <version> [skip ci]` commit whose only asset is
[`.github/badges/coverage.json`](../../.github/badges/coverage.json) — the badge the combined-coverage
job produced earlier in the same run. Publishing that release is what triggers `build.yml`.
[`package.json`](../../package.json) exists only to pin those Node dev-dependencies; no JavaScript
ships.

### The release tooling is checked before the merge

Because the Semantic Release job runs only after the merge, a broken set of release plugins used to
surface on `main` alone — and it did, in both possible ways. From 2026-07-10 to 2026-10-12,
`conventional-changelog-conventionalcommits` 10.2.1 and then 10.3.0 rendered release notes without a
single entry: every GitHub release from v1.5.3 to v1.19.15 carries a version header and nothing
else, and the job stayed green throughout. On 2026-10-12, 10.4.1 failed the job outright with
`Missing helper: "conventional-changelog-conventionalcommits requires conventional-changelog-writer@9
or newer"`. Both have one cause: from 10.0.0 the preset needs `conventional-changelog-writer` 9,
while `@semantic-release/release-notes-generator` 14.1.1, the newest on 2026-10-12, still depends on
writer `^8`.

So [`package.json`](../../package.json) holds the preset at 9.3.1, the version every release up to
v1.5.2 used, and a package rule in [`renovate.json`](../../renovate.json) caps it below 10. That cap
does not lift itself: it comes off by hand once release-notes-generator depends on writer 9.

The **Release Tooling Check** job runs `make release-notes-check` on every pull request: an
`npm install` and `npm audit signatures` exactly like the Semantic Release job, then
[`hack/release-notes-check.mjs`](../../hack/release-notes-check.mjs), which loads the installed
commit-analyzer and release-notes-generator with their configuration from
[`.releaserc.json`](../../.releaserc.json), feeds them three synthetic commits and fails unless the
release type is `minor` and both the feature and the fix appear in the notes. It asserts on the
rendered entries rather than on exit status, because the empty-notes failure exits zero. It is a
required status check on `main`, so Renovate's automerge of an npm bump waits for it. Verified on
2026-10-12: the target fails with the preset at 10.3.0 (no entries) and at 10.4.1 (`Missing
helper`) and passes at 9.3.1.

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

| File | Literal | Form | Matched by |
|---|---|---|---|
| [`go.mod`](../../go.mod) | `go <version>` | full version | custom regex manager on `^go\.mod$` |
| [`Containerfile`](../../Containerfile) | `FROM golang:<version>-alpine` | full version | custom regex manager on `^Containerfile$` / `^Dockerfile$` |
| [`.github/workflows/release.yml`](../../.github/workflows/release.yml) | `GO_VERSION: '<version>'` | full version | custom regex manager on `^\.github/workflows/.*\.ya?ml$` |
| [`.github/workflows/build.yml`](../../.github/workflows/build.yml) | `GO_VERSION: '<version>'` | full version | the same manager |
| [`.github/release-template.hbs`](../../.github/release-template.hbs) | `go-<major>.<minor>-blue` | major.minor only, via `extractVersionTemplate` | custom regex manager on that file, with `loose` versioning |

The table shows patterns, not values, because Renovate automerges Go patch releases and a literal
here would be stale after the next one. The badge's manager needs `loose` versioning: `semver`
rejects a two-part value such as `1.26` with `skipReason: invalid-value`, and until 2026-10-11 that
left the badge at `1.26` while `go.mod` moved through `1.27.x`, without any pull request touching it.
A local Renovate dry run with `loose` proposes `1.27` inside `renovate/go-version`.

[README.md](../../README.md) is deliberately not in that table: verified on 2026-09-19 it carries
four badges — build status, coverage, Go Report Card and licence — and no Go-version badge, so the
front page holds no Go literal to bump.

Renovate additionally groups every `golang.org/x/*` module bump into the same pull request, so
govulncheck sees a consistent set — in [`go.mod`](../../go.mod) and in the gosec tool module alike,
which is what the next section depends on.

### gosec is built from a tool module

gosec loads the packages it scans through `golang.org/x/tools`, and that importer has to read the
export data the Go toolchain in use writes. A gosec release pins one `golang.org/x/tools`, and a
gosec release can lag a Go release: gosec v2.29.0 carries `golang.org/x/tools v0.49.0`, which cannot
decode what Go 1.27.2 writes — every package fails with `export data version 5 is greater than
maximum supported version 4`. Reproduced on 2026-10-11 with `go install …/gosec@v2.29.0` under Go
1.27.2. **gosec then prints `Issues : 0`**; only its non-zero exit code marks the scan as failed, so
the summary line alone reads like a clean run.

So `make gosec` does not `go install` a tag. It builds the scanner from
[`hack/tools/go.mod`](../../hack/tools/go.mod), a module holding nothing but a `tool` directive for
gosec, with `golang.org/x/tools` raised above what gosec itself requires, and it rebuilds on every
run, so a gosec that happens to be on `PATH` is never used. Renovate treats that module as gosec's
dependency graph, not ours: two package rules in [`renovate.json`](../../renovate.json) switch every
entry in it off except gosec itself and `golang.org/x/*`, and the existing grouping rule puts the
latter into the Go version pull request. A Go bump therefore arrives together with the newest
`golang.org/x/tools`. Go records a tool's module as `// indirect`, so gosec is an indirect dependency
in that file — the re-enabling rule names it explicitly, and carries no `matchUpdateTypes`, because
Renovate decides `enabled` per dependency before it knows any update type. Both rules were checked on
2026-10-11 with a local `renovate --platform=local` dry run of the version the workflow uses (44):
`golang.org/x/tools` landed in `renovate/go-version`, gosec in a branch of its own, and every other
entry of the file stayed disabled.

The coupling cannot help when no `golang.org/x/tools` that reads the new export data exists yet; the
Go version pull request then fails the GoSec job and waits until one is released.

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
`structured-merge-diff/v6`, so `go build` failed inside apimachinery itself. Six of the then eleven CI
jobs ([testing.md](testing.md#what-ci-runs)) went red on that single error and two more were skipped
behind them. The grouping rule matches these modules too, so one unbumpable module also blocks every
other Kubernetes update sharing the pull request.

A package rule in [`renovate.json`](../../renovate.json) therefore disables updates for those three
module paths. They still move — `gomodTidy` raises them whenever a `k8s.io/*` bump lifts the minimum
version — but only as a consequence of the release line, never ahead of it.

**Security note.** Switching those updates off means a fix published in one of the three no longer
arrives on its own. What catches it instead is the vulnerability job: `make vuln` runs govulncheck on every pull
request and fails the pipeline if our code calls a known vulnerability, in a direct or an indirect
module. The residual gap is a vulnerability that govulncheck does not report as called; raising the
pin by hand is then the only route, and nothing prompts for it.

The other pinned versions, none of which is a Go version:

| Pin | Where | Bumped by |
|---|---|---|
| `ENVTEST_K8S_VERSION = 1.33.0` | [`Makefile`](../../Makefile) | by hand |
| `k8s.io/kube-openapi`, `k8s.io/utils`, `sigs.k8s.io/json`, each a pseudo-version | [`go.mod`](../../go.mod) | `go mod tidy`, only when a `k8s.io/*` release raises the minimum — Renovate is switched off for these three module paths, for the reason above |
| `KUBERNETES_VERSION: '1.33.4'` — the kubectl and Kind node image of the e2e job | [`release.yml`](../../.github/workflows/release.yml) | by hand |
| `KUSTOMIZE_VERSION`, `CONTROLLER_GEN_VERSION`, `GOLANGCI_LINT_VERSION`, `GOCYCLO_VERSION` | [`Makefile`](../../Makefile), each under a `# renovate:` comment | the Makefile custom regex manager; minor and patch automerge, major needs review |
| gosec, and the `golang.org/x/tools` it is built with | [`hack/tools/go.mod`](../../hack/tools/go.mod) | Renovate's gomod manager, restricted to those two by package rules: gosec in a pull request of its own, `golang.org/x/*` inside the Go version pull request; minor and patch automerge |
| `conventional-changelog-conventionalcommits`, held below 10 | [`package.json`](../../package.json) | Renovate's npm manager, capped by `allowedVersions: "<10"` in [`renovate.json`](../../renovate.json); the cap comes off by hand, for the reason in [the release tooling section](#the-release-tooling-is-checked-before-the-merge) |
| `ENVTEST_VERSION = release-0.24` | [`Makefile`](../../Makefile), under a `# renovate:` comment like the four above | by hand — the manager's regex captures `v[\d.]+` only, so the `release-N.NN` form never matches and the comment above it has no effect |
| The rclone image used by clone Jobs | the chart's [`values.yaml`](../../deploy/helm/stackit-s3-provisioner/values.yaml), and `DefaultCloneImage` in [`internal/controller/clone.go`](../../internal/controller/clone.go) as the fallback | Renovate, both of them: the chart value through the `helm-values` manager, `DefaultCloneImage` through a custom regex manager that exists to keep the fallback in step with the chart tag. The Makefile deliberately reads the image out of the rendered chart rather than pinning it a third time |

## What is wrong today

- **A stale generated CRD merges green**, and the gate that catches it runs after the image is
  pushed — the two properties above, in one sentence.
- **The Release Tooling Check covers analysis and notes, not publishing.** It never calls
  `@semantic-release/github` or `@semantic-release/git`, because both need a token with write access
  and a real tag. A bump that breaks authentication, the GitHub release or the badge commit still
  shows up only as a red Semantic Release job on `main`, after the merge.
- **Releases v1.5.3 to v1.19.15 keep their empty notes.** The fix applies to the next release on;
  nothing rewrites the notes of the 71 releases already published.
- **Nothing type-checks the build-tagged suites**, because every lint and vet target is untagged.
  The consequence for a contributor is a manual `go vet -tags integration ./...`
  ([testing.md](testing.md#what-is-wrong-today-and-what-this-page-could-not-verify)).
- **Not verified in this pass, and this is the gap:** no release was cut while writing this page.
  Everything above is read off the two workflow files, [`.releaserc.json`](../../.releaserc.json),
  the [`Makefile`](../../Makefile) and [`renovate.json`](../../renovate.json) on 2026-09-19; no claim
  here rests on having watched a publish run.
