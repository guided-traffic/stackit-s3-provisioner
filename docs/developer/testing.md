# Testing

This page is the map of the test estate: which suite exists, what each one can
prove, what it costs to run, and the conventions that keep a suite from leaving
real cloud resources behind. It names files and functions on purpose, so it goes
stale when the tree moves — whoever moves a test file updates this page in the
same change. If you only want the commands in one block, run `make help` and read
the [Makefile](../../Makefile), which is the authoritative target list; if you
want to know what the code under test actually does, start at
[package-map.md](package-map.md) and the subsystem page for the mechanism you are
changing.

## The layers

Eight distinct suites exist. They are separated by **build tag** and, for two of
them, by an **environment variable**; nothing else gates them.

| # | Layer | Build tag | Extra gate | Command | Needs |
|---|---|---|---|---|---|
| 1 | Pure offline unit | none | — | `go test ./...`, `make test-unit-coverage` | nothing |
| 2 | Offline against the in-memory provider fake | none | — | same command as 1 | nothing |
| 3 | envtest against a real API server | `integration` | — | `make test-integration` | envtest binaries (downloaded by the target) |
| 4 | Real provider API, library level | `integration` | key files present | `go test -tags integration ./stackit/ -run Integration` | a service-account key per project |
| 5 | Real provider API, reconciler level | `integration` | key file present | `go test -tags integration ./internal/controller/ -run IntegrationGroupAttribution` | a service-account key |
| 6 | Kind smoke, skeleton mode | `e2e` | — | `make e2e-local` | Kind, Helm, Docker |
| 7 | Kind against the real provider API | `e2e` | `E2E_STACKIT=1` | `make e2e-stackit` | Kind, Helm, Docker, a service-account key |
| 8 | Chart render assertions | `helm` | — | `make test-helm-render` | `helm` on `PATH` |

Layers 1 and 2 are one `go test` invocation; they are listed apart because they
prove different things and fail for different reasons. Layers 3, 4 and 5 share
the `integration` tag and are otherwise unrelated — see
[One tag, three meanings](#one-tag-three-meanings).

### Layer 1 — pure offline unit

Functions with no I/O: name composition, the Secret data contract, policy
construction and comparison, error classification, the retry transport, the cost
formula, the breaker state machine, the key-reload observer's four series, the
generated DeepCopy code.

<details>
<summary>File-by-file, as of 2026-09-19</summary>

| File | What it pins |
|---|---|
| [api/v1/bucket_types_test.go](../../api/v1/bucket_types_test.go) | Name composition and validation, `EffectiveBucketName`, the Secret data map and its key overrides, the key-collision refusal, the rotation trigger, the `cloneFrom` accessors |
| [api/v1/bucket_usage_test.go](../../api/v1/bucket_usage_test.go) | The three-state `spec.usage` accessors (`Enabled`, `IncludeVersions`, `Interval`) against a cluster default |
| [api/v1/deepcopy_test.go](../../api/v1/deepcopy_test.go) | The generated DeepCopy over a fully populated object, so an added field without `make generate-all` is caught |
| [cmd/main_test.go](../../cmd/main_test.go) | The environment-variable fallbacks for the flags, including that `"0"` survives `envDurationOrDefault` rather than reading as unset — for the key reload it is the documented off switch — and that `setupSAKeyReload` adds nothing to the manager in skeleton mode or at an interval of `0` (it is handed a **nil** manager, which is the cheapest possible proof that neither case reaches one) |
| [stackit/client_test.go](../../stackit/client_test.go) | Service-account key parsing, and that the two key files name two different projects |
| [stackit/newclient_test.go](../../stackit/newclient_test.go) | Client construction against a throwaway generated RSA key — parsing and JWT-signer setup only, never a call |
| [stackit/errors_test.go](../../stackit/errors_test.go) | `ProviderRefused` and `isServiceNotEnabled`, against error bodies captured verbatim from the live API on 2026-08-25, plus the proof that `oapierror.Model` is not a usable discriminator |
| [stackit/retry_test.go](../../stackit/retry_test.go) | The GET/HEAD-only retry transport: recovery, giving up, not retrying definite answers, leaving writes alone, honouring context |
| [stackit/s3_test.go](../../stackit/s3_test.go) | `BuildIsolationPolicy` — statement shape, the admin exemption, reader sanitising, deterministic ordering, and byte-identity without readers — plus `PoliciesEquivalent` |
| [stackit/s3_stats_test.go](../../stackit/s3_stats_test.go) | Size accounting: current objects only, versions, the object cap, the empty bucket, error propagation |
| [internal/controller/breaker_test.go](../../internal/controller/breaker_test.go) | The breaker in isolation on a manually advanced clock: threshold, reset, probe backoff, cooldown clamp, disabled |
| [internal/controller/ownership_test.go](../../internal/controller/ownership_test.go) | The ownership tag values, their stability across a CR UID change, and the collision error |
| [internal/controller/usage_config_test.go](../../internal/controller/usage_config_test.go) | The cost estimate against the real EU01 list price, formatting, the effective-config resolution, and the measurement skew |
| [internal/controller/metrics_test.go](../../internal/controller/metrics_test.go) | The metric collector output, including the two circuit metrics |
| [internal/controller/sakey_reload_test.go](../../internal/controller/sakey_reload_test.go) | `SAKeyReloadObserver` with no I/O at all: that all three `result` label values exist from the first scrape, that the failing gauge goes to `1` on a rejection and back to `0` both on a successful swap and on the file reverting to the key already in use, that the expiry gauge is **absent** rather than zero for a key with no `validUntil` — and goes away again when a key with one is replaced by a key without — that the loaded-at gauge is seeded at startup rather than only at the first rotation, and that a validated swap resets the breaker while a rejection never touches it |
| [internal/controller/bucket_controller_test.go](../../internal/controller/bucket_controller_test.go) | Reconciler helpers without a reconcile: name decision and freezing, group naming, `bucketsForSecret`, the managed/admin Secret predicates |
| [internal/stackitfake/fake_test.go](../../internal/stackitfake/fake_test.go) | The fake's own routing and inspection helpers — the fake is test infrastructure and is itself tested |

</details>

### Layer 2 — offline against the in-memory provider fake

The whole reconciler, driven against [internal/stackitfake](../../internal/stackitfake/)
and the controller-runtime fake client. `newTestEnv` in
[reconciler_fake_test.go](../../internal/controller/reconciler_fake_test.go)
builds a `BucketReconciler` whose `Stackit` client points at the fake's
control-plane URL, so provisioning, teardown, policy drift, rotation, clone,
grants, degradation and the circuit breaker all run end to end with no network
beyond localhost.

<details>
<summary>File-by-file, as of 2026-09-19</summary>

| File | What it pins |
|---|---|
| [reconciler_fake_test.go](../../internal/controller/reconciler_fake_test.go) | Provisioning, idempotence, healing a lost Secret, the four spec guards, the naming policy, ownership collision, policy self-healing, drift requeue, teardown, the wipe path, admin re-bootstrap, rotation |
| [reconciler_errors_test.go](../../internal/controller/reconciler_errors_test.go) | One injected failure per cloud call on both the provisioning and the teardown path, plus the two rollbacks that delete a key whose Secret write failed |
| [reconciler_attribution_test.go](../../internal/controller/reconciler_attribution_test.go) | Group attribution: name collision, migration from the bucket's own policy, restore without status, a deleted group, listing lag, and the teardown cases that must leave a group standing |
| [reconciler_grants_test.go](../../internal/controller/reconciler_grants_test.go) | Read grants: applied, revoked, namespace-scoped, self-reference ignored, admin never granted, stable document, the grantee watch predicate, the clone hold |
| [reconciler_degraded_test.go](../../internal/controller/reconciler_degraded_test.go) | Sticky readiness: held through transient failures, recovery, grace expiry, and every case in the exception list except the vanished bucket, which is the row below |
| [reconciler_missing_bucket_test.go](../../internal/controller/reconciler_missing_bucket_test.go) | The vanished-bucket guard: reported rather than re-created, the refusal holding over five further passes, recovery when the bucket comes back, a never-provisioned Bucket still provisioning, the `spec.allowRecreate` rebuild and its report, a completed clone not re-run by one, the five unreachable-provider shapes that must *not* trip it, the breaker staying shut, a teardown that completes without the bucket, and the three rollout cases — an upgrade touching nothing on healthy Buckets, a Bucket provisioned before `status.resolvedBucketName` existed being adopted rather than re-created, and a naming policy changed between operator versions still being checked under the frozen name |
| [reconciler_circuit_test.go](../../internal/controller/reconciler_circuit_test.go) | The breaker inside a reconcile: trip, hold without churn, grace still expiring, recovery, an isolated broken bucket not tripping it, deferred teardown |
| [reconciler_report_test.go](../../internal/controller/reconciler_report_test.go) | How a successful pass is reported ([ADR 0017](../adr/0017-a-reconcile-that-changes-nothing-is-silent.md)): the three outcomes of `reportPass` against a captured zap log — `Info` plus event on a change, `Info` without event on the first pass since start, verbosity 1 otherwise — and, end to end against the fake, that unchanged resyncs raise no further `Provisioned` event while `status.lastVerifiedTime` advances, that a re-asserted policy and a re-issued Secret are named in the event |
| [reconciler_clone_test.go](../../internal/controller/reconciler_clone_test.go) | The clone Job, the staging Secret, rc progress, failure retry, the guards, teardown of the artifacts, and the Job-name label budget |
| [reconciler_usage_test.go](../../internal/controller/reconciler_usage_test.go) | Measurement: the two switches, the interval floor, the object cap, versions, the failure path, and every skip condition |
| [stackit/client_fake_test.go](../../stackit/client_fake_test.go) | The control-plane wrapper against the fake: service enablement, bucket lifecycle, groups and keys, and `TestBucketExistsOnlyTrustsAStructuredAnswer` — the table-driven proof that `BucketExists` says "no" only for the API's own structured JSON `404` |
| [stackit/s3_fake_test.go](../../stackit/s3_fake_test.go) | The data-plane wrapper: policy and tag round-trips, emptiness, `WipeBucket` |
| [stackit/keyreload_test.go](../../stackit/keyreload_test.go) | The whole service-account key reload contract: the unchanged-hash short-circuit costing no call, a proven candidate becoming live, all eleven ways a candidate can be wrong — empty file, unparsable JSON, no `projectId`, a foreign `projectId`, unusable RSA material, a structured `400` from the token endpoint, a `5xx` from it, a gateway page in front of it, a `403` on the probe itself, an unreachable API, a deleted file — leaving the running key's hash and the project it names exactly as they were, which of those are classified definitive, the two retry schedules and the reset when the file content changes, the `Repeated` flag behind logging a rejection once rather than once per poll, and the poll loop itself starting, applying and stopping on context cancel. See [the offline seam](#the-offline-seam-the-key-reload-suite-uses) below |

</details>

Two test-infrastructure notes that are easy to trip over:

- **A clone Job never runs.** The controller-runtime fake client has no Job
  controller, so `finishCloneJob` writes the `Complete` or `Failed` condition by
  hand, and `TestCloneProgressFromRcStats` replaces `cloneStatsFn` and creates
  the running Pod itself. The offline layer proves the operator's reaction to a
  Job outcome, never that rclone copies anything — that is layer 7.
- **The failure-injection clock is fake.** `withCircuit` in
  [reconciler_circuit_test.go](../../internal/controller/reconciler_circuit_test.go)
  seeds `fakeClock` at wall-clock `now` so cooldowns can be skipped without the
  status timestamps drifting away from real time.

#### The offline seam the key-reload suite uses

Everything else offline stops at the edge of authentication — the fake answers any caller and
proves nothing about a credential ([What it deliberately does not model](#what-it-deliberately-does-not-model)).
The reload suite has to go one step further, because the property under test is precisely *can this
key mint a token*, and it does it with two seams that are worth knowing before you add a case.

**The key flow runs for real, against a local token endpoint.** `buildSAKey` in
[keyreload_test.go](../../stackit/keyreload_test.go) generates a throwaway RSA key and renders a
structurally valid service-account key document, and `withTokenEndpoint` writes the URL of an
`httptest` server into `credentials.tokenEndpoint`. The SDK reads that field whenever no explicit
token URL is configured, so the whole flow — signing the assertion with the generated key,
exchanging it, parsing the access token — really executes, offline. That is what lets one server
method stand for each provider answer that matters: `grant` for a key the provider accepts,
`refuse` for the structured `400 invalid_grant` of a revoked one, and `gatewayPage` for an
intermediary's HTML, which must *not* condemn the key
([provider-errors.md](provider-errors.md#a-revoked-service-account-key-is-a-token-endpoint-400)).
The token itself is a JWT the SDK only ever parses unverified for its expiry, so `fakeAccessToken`
does not sign it — signing would prove nothing and pull a JWT library into the package.

**The control plane is still the ordinary in-memory fake, and a candidate reaches it because the
endpoint now lives on the `Client`.** `newReloadEnv` builds with `NewClientWithEndpoint`, which
remembers the endpoint; `newKeyFlowAPIClient` applies it to every client it builds, including the
candidate of a reload, so a validated swap ends up talking to the same `stackitfake` server the
initial client did. That field exists for this and nothing else. One asymmetry follows from it and
is easy to trip over: the client `NewClientWithEndpoint` returns installs **no** retrying transport
(deliberately, so a `FailNext` injection is not consumed by a retry), while a candidate built by a
reload does get one — `newKeyFlowAPIClient` always makes a fresh `retryTransport`. So a `FailNext`
arming a `5xx` on a read, armed *after* a swap, would be retried away before it reached the caller,
where the same injection before the swap reaches it. Inferred from reading both constructors and
`retryableResult`; no test exercises the combination, and none needs to today.

Tests drive `KeyReloader.tick` directly rather than waiting on the ticker, so the backoff is
asserted in polls rather than slept through; the two that do exercise `Start`
(`TestKeyReloaderStartAppliesAChangedKey`, `TestKeyReloaderStartStopsOnContextCancel`) use a short
interval and a channel.

### Layer 3 — envtest against a real API server

[test/integration/](../../test/integration/) starts one `envtest` control plane
and one manager for the whole package
([suite_test.go](../../test/integration/suite_test.go)), installs the generated
CRD from `config/crd/bases`, and registers the `BucketReconciler` **without a
`Stackit` client** — so everything here runs in skeleton mode and touches no
cloud. `ENVTEST_K8S_VERSION` is `1.33.0` (`# default`, in the
[Makefile](../../Makefile)).

What only a real API server can prove, and therefore what belongs here:

| Case | Proves |
|---|---|
| [bucket_test.go](../../test/integration/bucket_test.go) `TestBucketReconcile_SkeletonFlow`, `..._DeletionRemovesFinalizer` | The skeleton path adds and releases the finalizer through a real watch and a real deletion timestamp |
| `TestBucketReconcile_CustomSecretKeys` | The generated schema round-trips every `secretRef.keys` field |
| `TestBucket_RejectsInvalidSecretKey` | The generated CRD pattern rejects an illegal Secret data key at admission |
| [grant_read_access_test.go](../../test/integration/grant_read_access_test.go) | The CEL rule compiles and rejects a self-grant, and the `listMapKey` constraint rejects a duplicate entry — a CEL expression that fails to compile makes the whole CRD uninstallable |
| [bucket_usage_test.go](../../test/integration/bucket_usage_test.go) | The `spec.usage` block round-trips, an omitted block stays nil, and an invalid interval is refused by the pattern |
| [allow_recreate_test.go](../../test/integration/allow_recreate_test.go) | `spec.allowRecreate` round-trips through the generated schema, reads back as `false` when omitted, and stays mutable after creation — unlike `spec.bucketName` |
| [sa_key_reload_test.go](../../test/integration/sa_key_reload_test.go) `TestSAKeyReloaderRunsOnAStandbyReplica` | That the key poller runs on a replica that is **not** the leader ([ADR 0016 D13](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)) — only a real API server can hold a real Lease |

The shared client is the manager's **cached** client, so every read after a write
polls with `require.Eventually` rather than racing the informer. The measurement
controller is not registered here; measurement has no envtest coverage.

The standby-replica case is the one test in this package that starts a **second**
manager, with leader election on, against the same control plane —
`suite_test.go` exports `testCfg` for exactly that. What makes it prove
something is the setup rather than the assertion: a Lease of the same name is
created first, held by the identity `another-replica` for an hour, so the
manager under test cannot win it. Without that the manager would become leader
and every runnable would start regardless, and the test would pass while
proving nothing — so it also asserts, after the poll has been observed, that
`mgr.Elected()` has not fired. The key path deliberately points at a file that
does not exist: what is under test is that the poll happens at all, not what it
concludes.

### Layers 4 and 5 — the real provider API

See [Suites that create real cloud resources](#suites-that-create-real-cloud-resources).

### Layer 6 — Kind smoke, skeleton mode

`make e2e-local` creates a Kind cluster, builds and side-loads the operator
image, runs the chart render checks, installs the chart with
[test/e2e/helm-values.yaml](../../test/e2e/helm-values.yaml) and runs
[test/e2e/e2e_test.go](../../test/e2e/e2e_test.go) and
[test/e2e/rbac_test.go](../../test/e2e/rbac_test.go). The values file sets
`stackit.serviceAccountKey.secretName: ""`, which is what puts the operator in
skeleton mode, and deliberately leaves `leaderElection.enabled: true` so the
smoke test exercises lease acquisition, the `coordination.k8s.io/leases` RBAC
gate and the release-on-cancel shutdown.

`TestBucketRBACAggregation` polls every *allow* and checks each *deny* only after
the allow that proves aggregation has landed — before the controller-manager has
merged the labelled rules into `view`/`edit`, everything is denied and a
deny-check would pass vacuously.

### Layer 7 — Kind against the real provider API

See [The cloud end-to-end run](#the-cloud-end-to-end-run).

### Layer 8 — chart render assertions

[test/helm/render_test.go](../../test/helm/render_test.go) shells out to
`helm template` and asserts on the rendered user-facing ClusterRoles and on the
manager container's arguments. It exists because the Kind install only ever
exercises the default values: the non-default combination
(`bucketRoles.create=false`) and the exact rule shape are visible to nothing
else.

`TestServiceAccountKeyArgsRenderTogether` is the same argument applied to the
service-account key branch, which the CI Kind run never renders because it
installs in skeleton mode. It pins that `--stackit-sa-key-path` and
`--stackit-sa-key-reload-interval` appear together once
`stackit.serviceAccountKey.secretName` is set, that the shipped default reaches
the operator as `30s` rather than being dropped, and that skeleton mode renders
**neither** — a reload interval without a key is meaningless. Without it the
whole branch, including the interval a rotation depends on, would ship
untested.

It is the only suite whose *test code* shells out to
a binary it expects to already be on `PATH` — `exec.Command("helm", …)` in
[render_test.go](../../test/helm/render_test.go). The envtest binaries of layer 3
are fetched by the make target, and the Kind, Helm and Docker of layers 6 and 7
are needed by the make target rather than by the Go test code, which talks to the
cluster through `client-go`.

### One tag, three meanings

`-tags integration` selects three unrelated things:

| Path | What it is | Run by |
|---|---|---|
| [test/integration/](../../test/integration/) | envtest, no cloud | `make test-integration`, CI |
| [stackit/*_integration_test.go](../../stackit/) | the real provider API, library level | no make target |
| [internal/controller/attribution_integration_test.go](../../internal/controller/attribution_integration_test.go) | the real provider API, reconciler level | no make target |

`make test-integration` runs `./test/integration/...` only, so the two real-API
sets are never started by a make target and never by CI.

**Nothing compiles the tagged files automatically either.** `go vet -tags integration ./...`
is the only invocation that type-checks all three sets, and it is a manual
convention, not a guard: `make vet` and `make lint` both run the untagged
`go vet ./...` ([Makefile](../../Makefile) lines 44 and 49), `.golangci.yml`
declares no `build-tags` so golangci-lint never parses the tagged files, and the
only `-tags=integration` in CI is inside `make test-integration`, which is scoped
to `./test/integration/...`. A rename in `internal/controller` can therefore leave
[attribution_integration_test.go](../../internal/controller/attribution_integration_test.go)
uncompilable while every CI check stays green. Run it by hand after touching a
signature the real-API suites use.

## The provider fake

[internal/stackitfake](../../internal/stackitfake/fake.go) serves both provider
surfaces from two `httptest` servers over one shared state: the Object Storage
control-plane REST API (`/v2/project/{id}/regions/{region}/...`, consumed through
the SDK) and the S3 data plane (path-style, consumed through `minio-go`).

### What it models on purpose

Each of these is a real behaviour that was verified against the live API and then
reproduced, because a wrapper that does not meet it in a test meets it in
production:

| Behaviour | Status |
|---|---|
| A foreign `projectId` on any control-plane path | `403` |
| Creating a bucket that exists | `409` |
| Deleting a bucket that still holds objects | `409` |
| Deleting a credentials group that still holds access keys | `422` |
| Deleting an access key without the `credentials-group` query parameter | `500` |
| A bucket with no tag set / no policy | `NoSuchTagSet` / `NoSuchBucketPolicy` |
| Object Storage not enabled for the project | `GetServiceStatus` answers the not-enabled shape until `EnableService` |

The full set of provider quirks, and which were verified when, is
[stackit-api.md](stackit-api.md); the fake is one consumer of that list, not its
home.

### The injection API

| Helper | Use |
|---|---|
| `FailNext(op, status)` | One JSON-enveloped API failure for the next call of `op` |
| `FailNextRaw(op, status, contentType, body)` | An intermediary's verbatim answer — the nginx HTML `403` page of the 2026-08-25 incident — which the SDK surfaces as an ordinary API error carrying that page's status code. The body is arbitrary, which is what lets the existence tests serve an HTML page, an **empty** body and a **truncated** JSON body at `404` and prove that none of the three is an answer |
| `Close()` | Shuts both `httptest` servers down mid-test, so the next call fails at the transport instead of answering — the only way offline to produce a failure that carries no HTTP answer. `httptest.Server.Close` is idempotent, so the `t.Cleanup(fake.Close)` each environment registers still runs afterwards |
| `Calls(op)` | How many times the fake served `op`; the counter lives in `failFor`, which every named operation consults exactly once, so it cannot drift from the set `FailNext` knows |
| `OmitFromNextListing(id)` | Leaves a group out of exactly one `ListGroups` answer, modelling a project listing that lags a create while the group's own endpoints already answer |
| `SeedBucket` / `SeedObject` / `SeedObjectVersion` / `SetTags` / `SetPolicy` | State placed directly, bypassing the API: a pre-existing foreign bucket, a bucket provisioned before a tag existed, non-current versions with sizes |

The `FailNextRaw` pair is what makes the error-classification tests honest: both
failures arrive as the same SDK error type, and only the body tells a provider
answer from an intermediary's page. See [provider-errors.md](provider-errors.md).

**Trap:** `minio-go` transparently retries a retryable data-plane failure, which
consumes the injection and then succeeds. A test that needs an `S3*` injection to
actually reach the caller must inject `403`, served as `AccessDenied`, which
minio-go does not retry. The caveat is written on `FailNext` itself.

### What it deliberately does not model

| Not modelled | Consequence |
|---|---|
| Authentication and SigV4 signatures | Nothing offline can prove a credential is accepted or rejected; the fake answers any caller |
| Bucket-policy **enforcement** | The fake stores and returns the policy document but never evaluates it. Isolation is only ever proved against the real backend (layer 4) |
| Object `PUT`/`GET` | The data plane answers `NotImplemented` for anything but location, policy, tagging, versions, multi-delete and list |
| Listing pagination | Every listing answers `IsTruncated=false` ([fake.go](../../internal/stackitfake/fake.go) lines 854 and 887), and `max-keys` truncates the body without setting the flag. `minio-go` therefore never has to follow a continuation token, so nothing offline crosses that boundary |
| Eventual consistency, except the one injectable case | `OmitFromNextListing` is the only lag the fake can produce |

## Suites that create real cloud resources

Five suites in two packages reach the live StackIT Object Storage API. They create and delete
real buckets, credentials groups and access keys, and they are the only place
several rules are actually proved rather than modelled.

| Suite | What only it can prove |
|---|---|
| [stackit/integration_test.go](../../stackit/integration_test.go) | Layer-1 cross-project isolation: an account can neither see nor modify the other project's bucket. Both directions, list, create and delete — the property is server-side and does not depend on operator code ([ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md)) |
| [stackit/credentials_integration_test.go](../../stackit/credentials_integration_test.go) | Layer-2 isolation: a workload reads and writes its own bucket, is denied every access to the other one, and is denied bucket management on its own ([ADR 0003 D1–D4](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)). It builds the document with the production `BuildIsolationPolicy`, so what is enforced is what the operator writes |
| [stackit/grants_integration_test.go](../../stackit/grants_integration_test.go) | That the three-statement document is enforced as designed — a `Deny` whose `Principal` is a *list*, which no offline fake evaluates. Granted reader reads, cannot write; ungranted workload stays locked out entirely ([ADR 0008 D6, D7](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md)) |
| [stackit/tagging_integration_test.go](../../stackit/tagging_integration_test.go) | That the backend supports S3 bucket-level tagging at all, and that the production `SetBucketTags`/`BucketTags` wrappers round-trip against it. If this fails, the ownership marker cannot live in a tag and [ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) needs a different carrier |
| [internal/controller/attribution_integration_test.go](../../internal/controller/attribution_integration_test.go) | That a bucket provisioned the pre-ADR-0002 way survives the upgrade **untouched** — the group is neither renamed, re-created nor re-keyed, only tagged — and that a CR restored without status re-attaches to the surviving group ([ADR 0002 D1, D2, D7](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)) |

The attribution suite is a hybrid worth knowing about: the production reconciler
is wired to the **real** provider client and an **in-memory** Kubernetes client,
so it exercises real cloud semantics without needing a cluster.

### Skipping, keys and region

All five skip themselves when the key files are absent, so
`go test -tags integration ./...` on a machine without keys is green and empty
rather than red.

| Suite | Skip condition | Override |
|---|---|---|
| `stackit` package | `accountPaths` in [client_test.go](../../stackit/client_test.go) stats both key files and calls `t.Skipf` if either is missing | `STACKIT_ACCOUNT_1`, `STACKIT_ACCOUNT_2` |
| `internal/controller` | `newIntegrationEnv` stats one key file and calls `t.Skipf` | `STACKIT_ACCOUNT_1` |

Defaults are `account-1.json` and `account-2.json` at the repository root. They
are real RSA private keys, they are untracked, and a single `.gitignore` entry is
all that keeps them out of a commit. Each names one dedicated e2e project; the
project ids are in the key files and are not repeated here, because nobody
without the files can use them. Both clients are built with `stackit.RegionEU01`
— every real-API suite is single-region, matching
[ADR 0005 D2](../adr/0005-the-operator-serves-one-project-in-one-region.md).

`TestLoadAccount` asserts the two accounts name **different** projects. That
assertion is load-bearing: two keys for one project would make the cross-project
isolation test pass while proving nothing.

### Cleanup conventions

Two forms of registration exist, and the difference matters more than it looks:

| Form | Used by | Why |
|---|---|---|
| One `t.Cleanup` **per resource**, registered the moment it is created | `createTempBucket` in [integration_test.go](../../stackit/integration_test.go) (line 62), [tagging_integration_test.go](../../stackit/tagging_integration_test.go) (line 50), [attribution_integration_test.go](../../internal/controller/attribution_integration_test.go) (lines 129 and 159) | Nothing is ever missed, and a `t.Fatalf` between two creations still unwinds |
| **One** `t.Cleanup` registered up front, draining `buckets`/`keys`/`groups` slices that the test appends to | [credentials_integration_test.go](../../stackit/credentials_integration_test.go) (line 127), [grants_integration_test.go](../../stackit/grants_integration_test.go) (line 42) | It is the only form that can enforce the deletion order below |

Per-resource `t.Cleanup` calls run **LIFO**, so in a suite that interleaves
buckets, groups and keys they unwind in reverse creation order — which is not the
order the provider accepts. A suite that creates all three kinds therefore uses
the single up-front cleanup with explicit slices; only a suite that creates one
kind, or creates them in an order whose reverse is already correct, may use the
per-resource form. Both forms follow the same two rules:

1. **Deletion goes through the control plane**, never through the data plane.
   `createTempBucket` registers `c.DeleteBucket`; keys and groups are removed
   with `DeleteAccessKey` and `DeleteCredentialsGroup`. The control plane is
   authenticated by the service-account token and is not subject to the bucket
   policy, so cleanup cannot be locked out by the very Deny document the test
   just installed.
2. **Emptying needs the data plane, and therefore the admin identity.** A bucket
   holding objects refuses deletion with `409`, and only a principal exempted in
   the policy can remove them. `TestIntegrationWorkloadCredentials` keeps an
   admin `minio` client for exactly this and calls `emptyBucket` before
   `DeleteBucket`.

Cleanup order is **buckets → access keys → groups**, because a group cannot be
deleted while it holds keys (`422`) and a bucket cannot be deleted while it holds
objects (`409`). Cleanup failures are logged with `t.Logf`, not failed on: a
cleanup that fails the test masks the cause that made it necessary.

Each cleanup uses a **fresh** `context.WithTimeout(context.Background(), …)`,
because the test's own context is usually already done by the time `t.Cleanup`
runs.

### Never run two real-API suites against one project at the same time

The admin bootstrap resolves the shared `operator-admin` group and then calls
`DeleteAllAccessKeys` on it before minting its own — a pre-existing admin key has
an unrecoverable secret half, so it is replaced rather than reused
([ADR 0004 D5](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)).
Two concurrent runs against one project therefore invalidate each other's admin
key mid-flight, and the failures look like unrelated `AccessDenied` noise.

## The cloud end-to-end run

`make e2e-stackit` is the only suite that exercises the whole chain: CR →
reconciler → control plane → data plane → workload Secret → real S3 access.

What the target does, in order:

1. Creates the Kind cluster and builds and side-loads the operator image.
2. Reads the rclone image out of the **rendered chart** and preloads it into the
   Kind node. It is deliberately not pinned a third time in the Makefile: the tag
   already lives in the chart values and in `DefaultCloneImage`, and a stale copy
   would preload an image the operator never asks for.
3. Creates the service-account key Secret and installs the chart with
   [test/e2e/helm-values-stackit.yaml](../../test/e2e/helm-values-stackit.yaml).
4. Runs `make test-e2e-stackit` (`E2E_STACKIT=1`, `-timeout=40m` `# default`,
   from the [Makefile](../../Makefile)) — **with a leading `-`**, so a test
   failure does not skip teardown.
5. Deletes every `Bucket` in the cluster and waits, so the finalizer releases the
   cloud resources while the cluster is still up.
6. Deletes the Kind cluster.
7. Sweeps the project for leftovers.

The values file is chosen so a run is identifiable and fully reversible:

| Value | Setting | Why |
|---|---|---|
| `bucketNaming.prefix` | `s3e2e` `# example` | Makes every bucket of the run unambiguous, and is what the sweep matches on |
| `bucketNaming.includeNamespace` | `true` `# example` | Composed name is `s3e2e-<namespace>-<spec.bucketName>` |
| `ownership.name` | `stackit-s3-provisioner-e2e` `# example` | Distinct from the production default, so this throwaway operator can never adopt or delete a bucket a real deployment provisioned in the same project |
| `wipeOnDelete.enabled` | `true` `# example` | The tests write objects; without the gate the finalizer refuses to delete a non-empty bucket and the run leaks real resources including live keys |
| `driftResyncInterval` | `1m` `# example` | A late-resolving read grant is picked up quickly; the tests must not depend on it, but it keeps a failure diagnosable |
| `bucketUsage.interval` / `minInterval` | `30s` `# example` | The test has to observe a *second* measurement after growing a bucket; the floor is lowered with the interval or it would clamp the CRs back up. Do not copy these into a real deployment |
| `bucketUsage.pricing.perGBHour` | the real EU01 list price | So the cost the test asserts is the cost a real deployment computes |

`E2E_BUCKET_PREFIX` in the [Makefile](../../Makefile) must equal
`bucketNaming.prefix` in that values file. It is the only thing separating this
run's cloud resources from real ones.

### What the cloud suite covers

[test/e2e/cloud_test.go](../../test/e2e/cloud_test.go), all gated on
`requireCloud` (`E2E_STACKIT=1`):

| Test | Covers |
|---|---|
| `TestCloudProvisioning` | The full provisioning chain and real S3 access with the published Secret |
| `TestCloudReadGrant` | A granted reader reads, an ungranted namesake in another namespace does not — the same CR name exists in `s3e2e-alpha` and `s3e2e-beta` on purpose |
| `TestCloudReadGrantLifecycle` | A grant that resolves only once the grantee appears, and revocation |
| `TestCloudBucketUsage` | Measurement, the cost estimate, and that switching measurement off clears the numbers |
| `TestCloudBucketUsageWithVersions` | The version listing against a non-versioned bucket — that the backend answers it at all and sets `IsLatest` correctly |
| `TestCloudClone` | The real rclone image, a second `Bucket` CR as source, the hold invariant, byte-exact content, and clone-once |

`TestBucketSkeletonReconcile` skips itself under `E2E_STACKIT=1`: its assertions
describe the no-cloud path, and running them against a live key would silently
provision a real bucket under the guise of a skeleton test.

**The wipe path is exercised only where objects are written.** The suite sets
`wipeOnDelete: true` on `solo`, `artifacts`, `reports`, `sized`, `versioned`,
`clonesrc` and `clonedst`, and deliberately not on the grantee buckets `backups`
and `late`, which nothing writes to and which are therefore deleted under the
plain emptiness guard of
[ADR 0006 D2](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md). Not
verified in this pass, and this is the gap: the claim that a run exercises the
wipe path rests on reading the suite, not on having run it.

### Names, and the sweep that finds leftovers

| Artefact | Pattern | Produced by |
|---|---|---|
| Real-API test bucket (layers 4/5) | `s3op-test-<first 8 of project id>-<random 0–999999>` | `bucketName` in [integration_test.go](../../stackit/integration_test.go) |
| Cloud e2e bucket (layer 7) | `<prefix>-<namespace>-<spec.bucketName>`, e.g. `s3e2e-s3e2e-alpha-solo` | `BucketNaming.ComposeBucketName` |
| Cloud e2e namespace | `s3e2e-alpha`, `s3e2e-beta` | constants in [cloud_test.go](../../test/e2e/cloud_test.go) |
| Workload credentials group | `s3op-<namespace>-<name>` truncated, plus `-<8 hex>` | `workloadGroupName` |
| Bootstrap group | `operator-admin`, one per project, shared | `adminGroupName` |

[hack/e2ecleanup](../../hack/e2ecleanup/main.go) is the backstop for a crashed or
interrupted run. It matches **buckets by name prefix** and **groups by the
display-name prefix `s3op-<prefix>`** — which works only because the e2e
namespaces start with the same prefix as the buckets. `-prefix` may not be empty;
the tool refuses, because the prefix is the only thing separating e2e resources
from real ones.

```bash
make e2e-stackit-sweep-dry   # report only
make e2e-stackit-sweep       # go run ./hack/e2ecleanup -key $(SA_KEY) -prefix $(E2E_BUCKET_PREFIX) -delete -admin
```

Three things about the sweep that are not obvious:

- **A non-empty sweep is a test failure, even when every test passed.** On
  2026-09-03 the first cloud run of the attribution work passed everything and
  the sweep found one orphaned keyed credentials group — the only evidence of a
  duplicate-group defect.
- **It mints a fresh admin key to do its work.** A leftover bucket still carries
  the isolation policy, which denies every principal but the admin group and the
  bucket's own workload group, so a newly created group would be locked out of
  the buckets the tool has to empty. `adminS3` therefore looks up the *existing*
  `operator-admin` group and creates a key inside it — the policy names the
  group's URN, not a key, so the new key is exempt. That key is removed again
  only by `deleteAdminGroup`, which runs **only with `-admin`**. Running
  `-delete` without `-admin` leaves a fully privileged, still-valid S3 key in the
  project.
- **`-admin` is unsafe against a project that also hosts a real deployment.** It
  drains and deletes the shared bootstrap group, invalidating that operator's
  admin key. The operator recovers on its own, but it is a needless outage.
  `adminGroupName` is duplicated by hand in the tool; a mismatch degrades it to
  creating its own group, which cannot empty a policied bucket.

### Capturing operator logs

`make e2e-stackit` deletes the Kind cluster at the end and keeps **no operator
logs** — only the `go test` output and the final sweep survive. When a cloud run
needs diagnosing, capture them in a parallel loop while the target runs:

```bash
until grep -q '^EXIT=' e2e.log; do \
  kubectl --context kind-stackit-s3-provisioner-test \
    -n stackit-s3-provisioner-system logs -f deploy/stackit-s3-provisioner \
    >> oplog.log 2>/dev/null || sleep 5; \
done
```

The Kind cluster is `stackit-s3-provisioner-test` (`# default`, `KIND_CLUSTER` in
the [Makefile](../../Makefile)) and the Deployment name is the Helm release name.
The second cloud run of the 2026-09-03 attribution work was diagnosed only
because the log was captured this way.

The real API also answers a `DELETE` with `connection reset by peer` from time to
time. Controller-level tests against it use a tolerant reconcile loop
(`reconcileTolerant` in
[attribution_integration_test.go](../../internal/controller/attribution_integration_test.go));
the operator itself simply requeues.

## Which suite pins which rule

Not exhaustive — the rules where the choice of layer is itself the point.

| Rule | Pinned by |
|---|---|
| [ADR 0001 D1](../adr/0001-a-bucket-only-affects-its-own-namespace.md) — cluster objects stay in the Bucket's namespace | Offline: `TestBucketsForSecret`, which asserts a namesake Secret in another namespace maps only to *that* namespace's Bucket |
| [ADR 0001 D3](../adr/0001-a-bucket-only-affects-its-own-namespace.md) — a spec reference resolves in the Bucket's namespace only | Offline: `TestReadGrantIsNamespaceScoped`, where a same-named Bucket in another namespace is granted nothing and raises `ReadGrantPending` |
| [ADR 0002 D1, D7](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) — attribution by tag, display name is a label | Offline: `TestGroupAttributionSurvivesNameCollision`. Real API: `TestIntegrationGroupAttributionMigration` |
| [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the document's shape vs. its enforcement | Offline pins the *document* (`stackit/s3_test.go`); only the real API pins that it is *enforced* (`credentials_integration_test.go`, `grants_integration_test.go`) |
| [ADR 0004 D8](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — a Bucket may not name the admin Secret | Offline: `TestReconcileGuards/secretRef targets admin secret` and `TestIsAdminSecret` |
| [ADR 0005 D3, D4](../adr/0005-the-operator-serves-one-project-in-one-region.md) — region declares, and a mismatch is definitive | Offline: `TestReconcileGuards/region mismatch` |
| [ADR 0005 D6](../adr/0005-the-operator-serves-one-project-in-one-region.md) — skeleton mode | envtest (the whole package runs with no provider client) and the Kind smoke run |
| [ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) — emptiness guard and the triple-gated wipe | Offline: `TestTeardown`, `TestTeardownWipeOnDelete`. Real API: the cloud run, whose wipe gate is on in its values file |
| [ADR 0007 D5, D6](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) — clear before create, roll back on a failed Secret write | Offline: `TestSecretWriteFailureRollsBackAccessKey`, `TestAdminSecretWriteFailureRollsBackAdminKey` |
| [ADR 0008 D2](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) — a self-reference is refused by the schema and ignored by the reconciler | envtest: `TestGrantReadAccess_SelfReferenceRejected` (the schema half). Offline: `TestReadGrantSelfReferenceIgnored` (the reconciler half) |
| [ADR 0009 D1, D3, D5](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — composed, frozen, and a fault when invalid | Offline: `TestComposeBucketName`, `TestPersistResolvedName`, `TestReconcileGuards/composed name too long` |
| [ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md) — the exception list | Offline: one test per exception in [reconciler_degraded_test.go](../../internal/controller/reconciler_degraded_test.go), including the nginx `403` page that must still be *held*; the seventh case, the vanished bucket, is in [reconciler_missing_bucket_test.go](../../internal/controller/reconciler_missing_bucket_test.go) instead |
| [ADR 0013 D2, D4](../adr/0013-a-provider-outage-is-held-fleet-wide.md) — trip on absence of success, no provider call while open | Offline: `TestProviderCircuitStopsHammeringTheProvider`, `TestProviderCircuitIgnoresAnIsolatedBrokenBucket` |
| [ADR 0014 D3](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) — a failed measurement never surfaces as a reconcile error | Offline: the `measure` helper fails the test if `Reconcile` returns any error at all |
| [ADR 0015 D3, D4](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) — only the provider's own structured `404` may be read as "the bucket is gone" | Offline, once per level: `TestBucketExistsOnlyTrustsAStructuredAnswer` pins the client wrapper, and `TestUnreachableProviderNeverTripsTheGuard` pins that the reconciler holds instead of reporting when the same kinds of failure arrive. Nothing real-API covers either |
| [ADR 0016 D2, D3](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) — change is a content hash, and nothing is swapped before one live authenticated call succeeds | Offline: `TestReloadUnchangedContentIsANoOp` (an unchanged file costs no API call) and the whole `TestReloadRejectsWithoutDisplacingTheRunningKey` table, where each row corrupts one step and asserts the running key is still in place |
| [ADR 0016 D6, D7](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) — the breaker reset, and the two retry schedules | Offline, split across the two packages: `TestSAKeyReloadResetsTheBreaker` / `TestSAKeyRejectionNeverTouchesTheBreaker` for the breaker half, `TestReloadBacksOffOnARepeatedDefinitiveRejection` and `TestReloadRecoversAfterTheKeyIsFixed` for the schedules. That the validation call ignores `Allow()` is structural — the reload path holds no breaker reference — and is pinned by nothing |
| [ADR 0016 D10](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) — the expiry gauge is absent, not zero, when the key carries no `validUntil` | Offline: `TestSAKeyValidUntilGaugeIsAbsentWithoutAnExpiry`, which is also the only place the *present* case is reached at all — verified on 2026-09-19, neither `account-1.json` nor `account-2.json` carries a `validUntil`, so every other run is on the absent path whether it means to be or not |
| [ADR 0016 D12](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) — skeleton mode does not poll and exports none of the series | Offline: `TestSetupSAKeyReloadStaysOff` for the wiring, and `TestServiceAccountKeyArgsRenderTogether` (layer 8) for the chart half, which asserts skeleton mode renders neither argument |
| [ADR 0016 D13](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) — the reload runs on every replica, not only the leader | envtest: `TestSAKeyReloaderRunsOnAStandbyReplica`, with the Lease held by another identity. `TestKeyReloaderNeedsNoLeaderElection` only pins the method's return value; nothing short of a real manager proves the manager honours it |

## What CI runs

All eleven jobs of
[.github/workflows/release.yml](../../.github/workflows/release.yml), every one
of them `runs-on: self-hosted`:

| Job | Runs |
|---|---|
| Unit Tests | `make test-unit-coverage` — layers 1 and 2 |
| Integration Tests (envtest) | `make test-integration-coverage` — layer 3 |
| E2E Tests | Kind, `make test-helm-render` (layer 8), then `make test-e2e` — layer 6, skeleton mode |
| Code Linting | `make lint` |
| GoSec / Vulnerability Check / Cyclomatic Complexity | `make gosec`, `make vuln`, `make cyclo` (`CYCLO_THRESHOLD ?= 15` `# default`, in the [Makefile](../../Makefile)) |
| Malware Scan | ClamAV over the source tree |
| Container Malware Scan | Builds the image from the [Containerfile](../../Containerfile) without pushing it, runs Trivy twice (a table run that fails on `CRITICAL,HIGH` unfixed findings, then a SARIF run kept as an artefact for 30 days), and removes the image again |
| Combined Coverage Report | Merges `coverage/unit.out` and `coverage/integration.out`, writes `.github/badges/coverage.json` on a push to `main`, and comments the delta against `main` on a pull request |
| Semantic Release | Only on a push to `main`, and only after every other job; releases nothing on a pull request |

[.github/workflows/build.yml](../../.github/workflows/build.yml) additionally runs
`make generate-all` and fails the release if the checked-in CRD, DeepCopy or
chart differ from the types.

Coverage is therefore **layers 1–3 only**, measured with `-coverpkg=./...`. The
Kind, chart-render and real-API suites contribute nothing to the number, so a
mechanism covered only by them reads as uncovered.

## What is wrong today, and what this page could not verify

- **`make test` does not run what its name implies.** It resolves
  `KUBEBUILDER_ASSETS` and then runs `go test ./...` with no `-tags`, so the
  envtest package is excluded by its build tag and the assets are fetched for
  nothing. Use `make test-unit-coverage` and `make test-integration-coverage`,
  which is what CI does.
- **`make test-unit` passes `-short`, and nothing reads it.** No test in the
  repository calls `testing.Short()`, so the flag changes nothing today.
- **The real-API suites never run in CI, have no make target, and are not even
  compiled by one.** Layers 4, 5 and 7 run only when a person runs them with keys
  in place. The strongest claims in the project — that Layer-1 isolation is
  server-side, that the three statement policy is actually enforced, that a legacy
  bucket survives the attribution migration — rest on a manual run. The exposure
  is larger than "they never run": nothing type-checks them either, because
  `make vet` and `make lint` are untagged and `.golangci.yml` sets no
  `build-tags`, so a rename can leave them uncompilable with every CI check
  green. `go vet -tags integration ./...` is the manual check, and it is the only
  one.
- **Nothing enforces that the offline suite stays offline.** It is a convention:
  the fake serves over `httptest` on localhost, the real-API files sit behind a
  build tag, and the one untagged test that touches the key files
  (`TestLoadAccount`) reads them from disk and skips when they are absent. A new
  test that dials out would pass CI on a runner with network.
- **No test ever produces a multi-page listing.** The fake answers
  `IsTruncated=false` unconditionally
  ([fake.go](../../internal/stackitfake/fake.go) lines 854 and 887) and truncates
  on `max-keys` without setting the flag, and the real-API suites never create
  enough objects to page. There is no paging loop of our own to test —
  `WipeBucket`, the size measurement and the emptiness check
  ([stackit/s3.go](../../stackit/s3.go) lines 489, 567 and 618) each consume
  `minio-go`'s `ListObjects` channel, which follows the continuation token
  internally. What is unproved is therefore how the operator behaves *across*
  that boundary: a partial listing, a mid-listing error, an object count above one
  page.
- **Measurement has no envtest coverage.** `BucketUsageReconciler` is not
  registered in [suite_test.go](../../test/integration/suite_test.go); only the
  `spec.usage` schema is checked there.
- **Documented timeouts disagree with each other.** Of the four suite headers in
  [stackit/](../../stackit/), two say `-timeout 12m` (credentials, grants), one
  says `-timeout 5m` (tagging) and one names no timeout at all
  ([integration_test.go](../../stackit/integration_test.go)); the attribution
  suite says `15m`. Meanwhile `make test-integration` passes `-timeout=20m`
  (`# default`, [Makefile](../../Makefile) line 86) and `make test-e2e-stackit`
  passes `-timeout=40m` (`# default`, line 102). The header values are suggestions
  in a comment that a person retypes, not a default the tooling applies — which is
  exactly why they drifted.
- **Nothing in this repository runs the tests with `-race`, and the key-reload
  suite is where that costs most.** Verified on 2026-09-19: `-race` occurs in no
  make target and in no workflow. `TestReloadSwapIsAtomicUnderConcurrentReaders`
  spawns readers against a client while it is swapped, which is a structural
  check and a smoke test in CI and nothing more — the detector that would
  actually prove the swap is never switched on. It was run by hand with
  `go test -race ./stackit/... ./internal/controller/...` on 2026-09-19 and
  passed; nothing repeats that, and the test's own comment says so.
- **The live rotation of a real key has not been done, and the repository holds
  no material for it.** Every case of the reload is exercised offline against
  the fake and a local token endpoint. Rotating a key of the *working* project
  needs a **second** key for project 1, issued by hand: `account-2.json` is
  deliberately a different project — `TestLoadAccount` asserts exactly that, and
  the assertion is load-bearing for the cross-project tests — so it cannot stand
  in for a rotation. Until that run happens, ADR 0016's own "not verified"
  entries stand.
- **No end-to-end suite reads the operator's metrics endpoint.** Verified on
  2026-09-19 by grepping [test/e2e/](../../test/e2e/): nothing there scrapes
  `/metrics` or the metrics port at all. None of the four `sa_key` series is
  therefore covered by the Kind suites, and neither is their *absence* in
  skeleton mode — the chart-render case pins that no argument is rendered, not
  that no series is exported.
- **The vanished-bucket guard has never met the real API.** The structured `404`,
  the gateway page carrying one, the empty and truncated bodies, the transport
  failure and the recovery are all reproduced offline against the fake, whose
  shapes come from the 2026-08-25 capture. Deleting a real bucket in a real
  project and watching the guard, the gauge and the recovery has not been done,
  and no cloud suite covers it.
- **Not verified in this pass, and this is the gap:** no suite was executed while
  writing this page. Everything above is read off the test sources, the Makefile
  and the workflow files; the runtimes of the cloud run and the content of its
  last green result are not restated here for that reason.
