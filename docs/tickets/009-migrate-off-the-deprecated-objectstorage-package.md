# 009 — The operator builds against a supported provider package

**Status:** Raised — survey complete, the migration target is identified and two of its behaviour
changes are measured. Not approved for implementation.
**Scope:** this repo (`stackit-s3-provisioner`); one Go file, one test fake, one linter exclusion.
**Date:** 2026-09-19

Raised on 2026-09-19 out of the documentation restructure. The provider marked the top-level
`objectstorage` package of its Go SDK deprecated and named a removal date, and until today that fact
lived in exactly two places, neither of them a home: a trailing bullet in the feasibility notes the
documentation restructure has dissolved, and a comment on the linter exclusion that suppresses the
resulting warning. A deferral with an expiry date and no file is not a decision anybody is tracking,
so it gets one. It is the only forward-dated commitment in this repository that somebody else made.

## What it is

Every exported symbol of `github.com/stackitcloud/stackit-sdk-go/services/objectstorage` carries the
notice

> `Deprecated: Will be removed after 2026-09-30. Move to the packages generated for each available API version instead`

— 1173 occurrences in the package's Go files at the pinned version, including the one directly above
the `package objectstorage` clause. The module's own changelog puts it more fully at **v1.5.0**:
*"The contents in the root of this SDK module including the `wait` package are marked as deprecated
and will be removed after 2026-09-30. Switch to the new packages for the available API versions
instead."* Verified 2026-09-19 by reading the module cache at v1.9.1 (the version
[go.mod](../../go.mod) pins) and at v1.9.2 (the newest published release), where the notice and its
date are unchanged.

**The date is not a build break.** Go module versions are immutable, so a pin on v1.9.1 keeps
compiling after 2026-09-30 whatever the provider does to later releases. What expires is the ability
to move: the moment the root package is dropped from a release, every SDK update — including one
carrying a security fix, a new region, or a changed API contract — becomes a migration rather than a
version bump, and Renovate's pull requests start failing to compile instead of merging. The cost of
waiting is therefore not an outage on a date; it is the certainty that the migration will one day be
urgent instead of routine.

The deferral is already written into the build. [`.golangci.yml`](../../.golangci.yml) excludes
`SA1019` for `stackit/client.go` alone, deliberately narrow so that any *other* deprecation still
surfaces, with a comment saying the rule is to be removed once the migration lands. That exclusion is
part of this work list, and so is its comment, which cites a section of `CLAUDE.md` that the
documentation restructure has dissolved.

## What the tree looks like today

Surveyed 2026-09-19 by grep over every Go file in the tree, tests and `hack/` included.

**One file imports the deprecated package: [`stackit/client.go`](../../stackit/client.go).** There is
no second import anywhere — not in `internal/controller`, not in `hack/e2ecleanup`, not in any test.
`stackit/errors.go` and `stackit/errors_test.go` import `core/oapierror`, which is a different module
and is **not** deprecated; the versioned packages use it too, so error classification is untouched by
the move.

| What is referenced | Where | Count |
| --- | --- | --- |
| The import itself | `client.go:19` | 1 |
| `objectstorage.APIClient` as the wrapper's field type | `client.go:65` | 1 |
| `objectstorage.NewAPIClient` | `client.go:93` (production), `client.go:114` (fake-backed tests) | 2 |
| `objectstorage.NewCreateCredentialsGroupPayload` / `NewCreateAccessKeyPayload` | `client.go:252`, `client.go:276` | 2 |
| Method calls on the client | see below | 13 |

The thirteen call sites cover twelve distinct operations: `GetServiceStatus` (`client.go:144` and
`:160`), `EnableService` (`:152`), `CreateBucket` (`:175`), `DeleteBucket` (`:184`), `ListBuckets`
(`:194`), `CreateCredentialsGroup` (`:253`), `DeleteCredentialsGroup` (`:265`), `CreateAccessKey`
(`:274`), `DeleteAccessKey` (`:291`), `ListCredentialsGroups` (`:362`), `ListAccessKeys` (`:376`),
`GetBucket` (`:395`).

**No SDK type crosses the package boundary.** The `APIClient` is an unexported field, and every
exported wrapper method returns a local type (`AccessKey`, `CredentialsGroupInfo`) or plain strings.
The blast radius of the migration is therefore one file: no caller in `internal/controller`, in
`hack/e2ecleanup` or in any test mentions an SDK type.

**`go.mod` does not change.** The replacement packages ship inside the same module — there is no
`go.mod` under them — so the migration is an import-path edit, not a dependency change. The
deprecated `wait` package named in the same notice is not imported anywhere in the tree.

### The migration target is `v2api`, and `v1api` is not an option

Both replacement packages are already present at the pinned v1.9.1: `objectstorage/v1api`
(API version 1.1.0) and `objectstorage/v2api` (API version 2.0.1). The deprecated root package is
itself generated from API version 2.0.1, and its request paths are already `/v2/project/{projectId}/regions/{region}/…`
— byte-identical to `v2api`'s, checked path by path.

`v1api` drops the region from every signature (`CreateBucket(ctx, projectId, bucketName)` against
`CreateBucket(ctx, projectId, region, bucketName)`). Adopting it would silently retire the per-call
region argument that [ADR 0005 D2](../adr/0005-the-operator-serves-one-project-in-one-region.md)
rests on, so it is out of scope for this ticket and would need its own decision.

### What `v2api` differs in

Four differences, all verified against the module cache on 2026-09-19; the second and third were also
reproduced with a throwaway program against the real SDK.

| # | Difference | Cost |
| --- | --- | --- |
| 1 | Methods hang off a `DefaultAPI` **interface field** (`client.DefaultAPI.CreateBucket(…)`) instead of on `*APIClient` directly | Mechanical, 13 call sites. It also brings a dependency-free generated mock of that interface |
| 2 | `NewAPIClient` calls `config.ConfigureRegion`, which **rejects** a region set in the configuration | `config.WithRegion` must be dropped from the production constructor. See below |
| 3 | Every response model validates its required properties while decoding; the deprecated layer performs no such check at all (0 occurrences of `requiredProperties` in its Go files) | A response missing a field the spec calls required becomes an error. Affects the offline fake and, possibly, the real API |
| 4 | Models carry plain fields plus `AdditionalProperties` instead of pointer fields behind generated type aliases; getters return `string` rather than `BucketGetNameRetType` | None at the source level — the aliases are true aliases (`= string`), so every `b.GetName()` in the wrapper compiles unchanged. Both payload constructors the wrapper uses exist with the same shapes, as do all three `CredentialsGroup(…)` request builders |

**Difference 2, measured.** `v2api.NewAPIClient(config.WithToken(…), config.WithRegion("eu01"))`
returns `configuring region: this API does not support setting a region in the the client
configuration, please check if the region can be specified as a function parameter`. The OpenAPI
server declares its `region` variable with default `global`, and `config.ConfigureRegion` treats a
configured region on a global URL as a caller error. The deprecated package skips that call outright
— its constructor carries the comment *"using new regional api, so no region is set here"*.
`NewClient` ([`client.go:93`](../../stackit/client.go)) passes `config.WithRegion(region)` today, so
it fails at construction the moment the import changes.

**The trap is that this failure hides from the offline suite.** `ConfigureRegion` returns
immediately when a custom endpoint is configured, and `NewClientWithEndpoint`
([`client.go:114`](../../stackit/client.go)) — the constructor every fake-backed test uses — sets one.
Measured: with `config.WithEndpoint` present, the same option combination constructs without error.
A migration that changes only the import path therefore passes `make test-unit` and fails on the
first real key.

**Difference 3, measured.** A `200` response body of `{"project":"p1"}` to `GetServiceStatus` decodes
cleanly under the deprecated package and, under `v2api`, produces
`no value given for required property scope, status code 200, Body: {"project":"p1"}` as an
`*oapierror.GenericOpenAPIError`. Adding `"scope":"PUBLIC"` makes it decode.

### The offline fake is affected, but not the way it looks

[`internal/stackitfake`](../../internal/stackitfake/fake.go) is a **wire-level** fake: it serves HTTP,
so the import path means nothing to it and the control-plane paths are identical between the two
packages. It is affected only through difference 3 — four handlers answer with bodies that omit a
property `v2api` now requires. Every other control-plane response it writes already carries its
required fields; checked model by model.

| Handler | Body today | Model | Missing property |
| --- | --- | --- | --- |
| `GET` service status ([fake.go:495](../../internal/stackitfake/fake.go)) | `{project}` | `ProjectStatus` | `scope` |
| `POST` enable service ([fake.go:501](../../internal/stackitfake/fake.go)) | `{project}` | `ProjectStatus` | `scope` |
| `DELETE` credentials group ([fake.go:612](../../internal/stackitfake/fake.go)) | `{project}` | `DeleteCredentialsGroupResponse` | `credentialsGroupId` |
| `DELETE` access key ([fake.go:686](../../internal/stackitfake/fake.go)) | `{project}` | `DeleteAccessKeyResponse` | `keyId` |

The service-status row is the expensive one: `EnsureService` runs on the first provisioning reconcile
of every process, so an unfixed fake breaks essentially every reconciler test at once rather than a
named few. Five offline test files build on the fake — `stackit/client_fake_test.go` (8 top-level
tests), `stackit/s3_fake_test.go` (9), `internal/controller/reconciler_fake_test.go` (13),
`internal/controller/reconciler_errors_test.go` (4) and `internal/stackitfake/fake_test.go` (4).

One more fake detail, harmless: `apiError` writes `{"message": …}` while `ErrorMessage` requires
`detail`, so under `v2api` the SDK fails to decode an error body and falls back to the decode error as
the message text. Status code and raw body are set before that point and survive, so `apiAnswer`,
`ProviderRefused` and `isServiceNotEnabled` in [`stackit/errors.go`](../../stackit/errors.go) are
unaffected — only human-readable text changes. Read from the generated error path, **not** measured.

## The decisions it collides with

| Decision | How this ticket touches it |
| --- | --- |
| [ADR 0005 D2](../adr/0005-the-operator-serves-one-project-in-one-region.md) | The region is fixed at install time and travels as a per-call argument. Difference 2 removes the *configuration* half of that and leaves the argument half; `v1api` would remove the argument half too, which is why it is excluded |
| [ADR 0005 D7](../adr/0005-the-operator-serves-one-project-in-one-region.md) | A configured key that cannot be used is a startup failure. Difference 2 turns a mistake in this migration into exactly that — loud, at startup, on every pod — which is the good failure mode, and the reason the offline blind spot matters rather than the failure itself |
| [ADR 0012 D2](../adr/0012-ready-describes-the-last-verified-state.md) | Errors are classified by origin, never by text or status code. A strict-decoding failure is a new origin: a `200` from the provider that the client refuses. It has no classification today and falls through to non-definitive |
| [ADR 0012 D3](../adr/0012-ready-describes-the-last-verified-state.md) | An unrecognised error is non-definitive by construction, so a decode failure holds `Ready` for the grace instead of failing the bucket. That is the correct default and it is also how the problem would stay invisible for 30 minutes |
| [ADR 0013 D3](../adr/0013-a-provider-outage-is-held-fleet-wide.md) | Only non-definitive failures count toward the trip. A field the provider stops sending would therefore fail every reconcile of the fleet and open the circuit — a client-side decoding change presenting as a provider outage |
| [ADR 0013 D9](../adr/0013-a-provider-outage-is-held-fleet-wide.md) | The transport-level rules — no retry on `429`, a 30 s idle timeout — live in an HTTP client handed to the SDK through `config.WithHTTPClient`. `v2api`'s constructor defaults and then passes that same client through `auth.SetupAuth` in the same order as the deprecated one, so the transport is carried over unchanged. Verified by reading both constructors, not measured |

## Work list

1. **Switch the import to `objectstorage/v2api`** in [`stackit/client.go`](../../stackit/client.go)
   and route the thirteen call sites through the `DefaultAPI` field. Nothing else in the tree changes:
   no `go.mod` edit, no caller, no test.
2. **Drop `config.WithRegion` from `NewClient`.** The per-call `region` argument already carries it
   on every one of the twelve operations. Leave `NewClientWithEndpoint` alone if convenient — the
   option is inert behind a custom endpoint — but say so in a comment, because "it is set in one
   constructor and not the other" reads as a bug otherwise.
3. **Add an offline test that constructs the production client.** Difference 2 is invisible to every
   existing test. The cheapest honest check is a unit test that calls `NewClient` with a throwaway
   key file on disk and asserts it returns without error. **Not verified:** whether `auth.SetupAuth`
   fetches a token eagerly at construction — if it does, this test needs a fake token endpoint or has
   to assert on `v2api.NewAPIClient` with the same option set instead. Establish which before writing
   it; a test that needs the network is worse than no test.
4. **Fill the four fake responses** in the table above with the properties `v2api` requires. Values
   should mirror what the real API sends: `scope` as observed in
   [`stackit/errors.go`](../../stackit/errors.go) (`"PUBLIC"`, measured live 2026-08-25), and the two
   ids echoed back from the request.
5. **Re-run every layer, in this order:** `make test-unit`, `make test-integration`,
   `go test -tags integration ./stackit/ -run Integration`,
   `go test -tags integration ./internal/controller/ -run IntegrationGroupAttribution`, then
   `make e2e-stackit` followed by `make e2e-stackit-sweep-dry` reporting nothing left behind. Layers
   1 to 3 cannot see difference 2 and layers 4 upward cannot see the fake, so neither half alone
   proves the migration.
6. **Remove the `SA1019` exclusion** for `stackit/client.go` from
   [`.golangci.yml`](../../.golangci.yml), comment included, and confirm `make lint` is clean without
   it. An exclusion that outlives its reason is how the next deprecation goes unnoticed.
7. **Update the documentation in the same change:** the *The deprecated top-level package* section of
   [docs/developer/stackit-api.md](../developer/stackit-api.md#the-deprecated-top-level-package)
   becomes a statement of which package the operator uses and why `v2api` and not `v1api`; the fake's
   entry in [docs/developer/testing.md](../developer/testing.md#the-provider-fake) gains the strict
   decoding constraint, which is a new rule for anyone adding a handler; and the pages that name the
   package by version — [package-map.md](../developer/package-map.md),
   [provider-errors.md](../developer/provider-errors.md),
   [credentials.md](../developer/credentials.md),
   [usage-measurement.md](../developer/usage-measurement.md) — are checked for the import path.
8. **Keep the SDK version bump out of this change** unless Q3 decides otherwise, so that a regression
   has one possible cause.

## Open questions

### Q1 — Does a strict-decoding failure deserve its own classification?

Under [ADR 0012 D3](../adr/0012-ready-describes-the-last-verified-state.md) it is non-definitive, so
buckets hold `Ready` for the grace; under
[ADR 0013 D3](../adr/0013-a-provider-outage-is-held-fleet-wide.md) it counts toward the trip, so a
field the provider quietly stops sending opens the fleet-wide circuit and reads on every dashboard as
a provider outage. Both behaviours are defensible — the operator genuinely cannot use the answer —
but the cause is in the client, and nothing in the signal says so. The alternative is a distinct
classification that is still non-definitive but named, so the message and the metric point at the
client. That would amend ADR 0012 and needs approval before it is written.

### Q2 — Do the real API's delete responses carry the properties `v2api` now requires?

`DeleteCredentialsGroupResponse` requires `credentialsGroupId` and `DeleteAccessKeyResponse` requires
`keyId`. **Not verified** — the only recorded live response shapes in this repository are the three
`GetServiceStatus` answers in [`stackit/errors.go`](../../stackit/errors.go) (measured 2026-08-25),
and the two delete calls are exercised only by the integration and e2e suites. If the real API omits
either field, teardown and rotation fail against a decode error while the deprecated package tolerated
it, and the migration needs a decision rather than a fix. Step 5 answers this and nothing else can.

### Q3 — Is the SDK bumped to v1.9.2 in the same change?

v1.9.2 is the newest release and changes only its `core` dependency, from v0.26.0 to v0.27.0. The one
function this ticket depends on, `config.ConfigureRegion`, is identical in both core versions
(diffed 2026-09-19: two `strings.Replace` calls became `strings.ReplaceAll`). Bundling is therefore
cheap but makes a regression ambiguous.

### Q4 — Does this need an ADR?

By CLAUDE.md's definition it is not architectural: no trust boundary, API shape, deletion, rotation
or privilege rule changes, and the wire protocol is the same `/v2` API. It becomes architectural only
through Q1. Decide with the user rather than assuming, and decide before the work starts, not after.

### Q5 — Does Renovate move an import path?

[`renovate.json`](../../renovate.json) enables `gomodTidy` and `gomodUpdateImportPaths`. Whether the
latter rewrites an import from a module's root package to a sub-package of the same module is **not
verified**; the working assumption is that it does not, because it exists for major-version module
path changes. Worth ten minutes before somebody waits for a bot that is not coming.

## What it must not break

| Invariant | Why it is at risk here |
| --- | --- |
| Region stays a per-call argument | `v1api` removes it from every signature and is the obvious-looking "version 1, the stable one" choice. The API version the operator speaks is 2.0.1 and always has been ([ADR 0005 D2](../adr/0005-the-operator-serves-one-project-in-one-region.md)) |
| A green offline suite must not be mistaken for a proven migration | Difference 2 is structurally invisible to every fake-backed test, because the custom endpoint short-circuits the check that fails. Step 3 exists solely to close that gap |
| Error classification stays keyed on the body, not the status | [ADR 0012 D2](../adr/0012-ready-describes-the-last-verified-state.md). `core/oapierror` is unchanged and the versioned packages populate `StatusCode` and `Body` the same way, but difference 3 introduces the first error carrying status `200`. Nothing may start switching on the status to handle it |
| The fake stays a wire-level fake | `v2api` ships a Go-level mock of `DefaultAPI`, and replacing the fake with it looks like a simplification. It would silently delete the error-classification tests' whole premise: a gateway's HTML page and a provider's JSON refusal arrive as the same Go error and are told apart only by the body ([docs/developer/provider-errors.md](../developer/provider-errors.md)) |
| The retrying transport stays under authentication | `config.WithHTTPClient` is adopted by `auth.SetupAuth` as the inner transport, which is what makes a retry carry the `Authorization` header the flow already attached, and what keeps writes and token fetches un-retried ([docs/developer/stackit-api.md](../developer/stackit-api.md)). The rules that transport enforces are [ADR 0013 D9](../adr/0013-a-provider-outage-is-held-fleet-wide.md) |
| Skeleton mode still makes no cloud call | Without a key the operator constructs no client at all ([ADR 0005 D6](../adr/0005-the-operator-serves-one-project-in-one-region.md)). A constructor that moves, or a client built eagerly to test the new one, can break that without any test noticing |
| No second change rides along | Narrowing existence checks is [008](008-decide-bucket-existence-with-a-per-bucket-read.md) and key hot-reload is [007](007-hot-reload-the-stackit-service-account-key.md); both touch the same file and both want the same constructor. This change is a package move and nothing else, so that a failure against the real API has one candidate cause |

## Done when

| # | Criterion |
| --- | --- |
| 1 | No file in the tree imports `github.com/stackitcloud/stackit-sdk-go/services/objectstorage` at its root path; `stackit/client.go` imports `…/objectstorage/v2api` and `go.mod` is unchanged |
| 2 | `NewClient` constructs successfully with a real key file, proved by a test that runs in the offline suite without network access — or, if that proves impossible, by a recorded reason and a named suite that covers it instead |
| 3 | The four fake responses carry the properties `v2api` requires, and `make test-unit` and `make test-integration` are green |
| 4 | The control-plane integration suites and `make e2e-stackit` are green against a real key, with `make e2e-stackit-sweep-dry` afterwards reporting nothing left behind — this is what answers Q2 |
| 5 | The `SA1019` exclusion is gone from `.golangci.yml` and `make lint` is clean without it |
| 6 | Q1 is answered: either a decode failure keeps the default non-definitive treatment, recorded as a deliberate choice, or ADR 0012 carries a new rule agreed in advance |
| 7 | [docs/developer/stackit-api.md](../developer/stackit-api.md#the-deprecated-top-level-package) names the package the operator actually uses, and the strict-decoding rule is stated where somebody adding a fake handler will read it |
| 8 | Nothing in the repository still describes the migration as deferred |

## References

* [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) — the region binding that
  decides `v2api` over `v1api`, and the startup-failure rule this migration would trip
* [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) — D2 and D3, the classification
  a strict-decoding failure lands in today
* [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) — D3, why a client-side decoding
  change could present as a provider outage, and D9, the transport that must survive the move
* [docs/developer/stackit-api.md](../developer/stackit-api.md#the-deprecated-top-level-package) —
  the survey this ticket extends, and the page the outcome is written into
* [docs/developer/testing.md](../developer/testing.md#the-provider-fake) — what the fake models on
  purpose, and what it must keep being
* [docs/developer/provider-errors.md](../developer/provider-errors.md) — why the body and not the
  status code is the discriminator
* [`stackit/client.go`](../../stackit/client.go) — the one file that imports the deprecated package
* [`internal/stackitfake/fake.go`](../../internal/stackitfake/fake.go) — the four handlers to fill
* [`.golangci.yml`](../../.golangci.yml) — the exclusion that expires with this work
