# Reloading the service-account key

How the operator notices that its STACKIT service-account key file has changed, how it proves the
new key before using it, and how the swap happens underneath calls that are already in flight. The
decision and its alternatives are [ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md);
this page is the mechanism, and it names files and functions on purpose, so it goes stale when the
tree moves and whoever moves it updates this page in the same change.

What an operator has to *do* about a rotation is [credentials.md](../operations/credentials.md); the
setting itself is [configuration.md](../operations/configuration.md), and the complete value list is
[README.md](../../README.md).

## Contents

- [Why a whole new client, not a new key](#why-a-whole-new-client-not-a-new-key)
- [The three pieces and where they live](#the-three-pieces-and-where-they-live)
- [One poll, step by step](#one-poll-step-by-step)
- [The swap, and what survives it](#the-swap-and-what-survives-it)
- [The two retry schedules](#the-two-retry-schedules)
- [The four metrics](#the-four-metrics)
- [How this is tested offline](#how-this-is-tested-offline)
- [What is wrong today](#what-is-wrong-today)

## Why a whole new client, not a new key

The SDK gives no way to replace key material in a live client, and the reason is worth knowing
before reading any of the code.

`objectstorage.NewAPIClient` → `auth.SetupAuth` → `auth.KeyAuth` unmarshals the key JSON into a
`clients.ServiceAccountKeyResponse` and hands it to `clients.KeyFlow.Init`, whose `validate()`
parses the RSA PEM into `c.privateKey` *eagerly*, at construction. Every later token refresh signs a
fresh assertion with that in-memory key and never looks at the file again. Verified in
`core v0.26.0` on 2026-09-19; the same finding is recorded from the SDK's side in
[stackit-api.md](stackit-api.md).

Two consequences shape everything below. First, reloading means building a **new** `APIClient` —
there is nothing smaller to replace. Second, that eager parse is free validation: a truncated or
malformed key fails at construction, before any call goes out.

## The three pieces and where they live

| Piece | Where | What it is responsible for |
|---|---|---|
| The swap | [`stackit/client.go`](../../stackit/client.go) | `clientState` and the `atomic.Pointer` on `Client` that replaces it as one value |
| The poll and the validation | [`stackit/keyreload.go`](../../stackit/keyreload.go) | reading and hashing the file, `(*Client).reload`, the `KeyReloader` loop and its backoff |
| Logs, metrics, breaker | [`internal/controller/sakey_reload.go`](../../internal/controller/sakey_reload.go) | `SAKeyReloadObserver`, which is also the `prometheus.Collector` for the four series |
| Wiring | [`cmd/main.go`](../../cmd/main.go) | the flag, and `setupSAKeyReload` adding the poller to the manager |

The split between the second and the third row is the one to preserve. Package `stackit` has no
logger and no Prometheus dependency, and the reload does not introduce one: `KeyReloader` hands each
poll to an `observe func(ReloadResult)` callback, and everything that logs or counts lives in
`internal/controller`. Putting the loop in `internal/controller` instead would have worked too, but
then the validation could not be exercised offline without exporting a seam from `stackit` — see
[how this is tested offline](#how-this-is-tested-offline).

`KeyReloader` satisfies controller-runtime's `manager.Runnable` and `manager.LeaderElectionRunnable`
**structurally**: `Start(context.Context) error` and `NeedLeaderElection() bool` are the whole
contract, so `stackit` imports no controller-runtime. `NeedLeaderElection` returns `false`, which is
[ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) D13 — a
standby has to be holding a fresh client at the moment it takes the lease, not start reloading then.
It is the first and so far only `manager.Runnable` in this repository.

## One poll, step by step

`KeyReloader.tick` is the whole poll. It returns `(ReloadResult, bool)`; the `bool` is false only
when the poll did nothing at all because a rejected candidate is still backing off, which is a
deliberate silence rather than an outcome and must not be counted as one.

1. **Read once.** `readKeyFile` does a single `os.ReadFile` and hashes exactly those bytes with
   SHA-256. Those same bytes are what gets parsed and what the SDK receives, so nothing can be
   re-read between the check and the use. A read failure has no hash to back off on, so it is
   retried every poll.
2. **Compare the hash.** `(*Client).keyHash()` is the hash of the bytes the live credential was
   built from. Equal means no-op, and — this is the point of hashing rather than stamping mtime — a
   file changed back to the content already loaded is a no-op too.
3. **Parse.** `parseAccount` rejects an empty file, JSON that does not parse, and a document with no
   `projectId`. The empty case is load-bearing rather than belt-and-braces: `WithServiceAccountKey("")`
   does **not** fail cleanly in the SDK, it falls through to `auth.DefaultAuth`, which searches
   `STACKIT_SERVICE_ACCOUNT_KEY`, `STACKIT_PRIVATE_KEY` and `$HOME/.stackit/credentials.json`. The
   outcome would then depend on ambient state rather than on the candidate.
4. **Compare the project.** `(*Client).reload` refuses a candidate whose `projectId` differs from
   the one the process started with. One string comparison, no state, no configuration — and a
   guardrail for one process lifetime only, never a boundary
   ([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) D4).
5. **Build the candidate client.** `(*Client).newKeyFlowAPIClient` passes
   `config.WithServiceAccountKey(string(content))` and deliberately **not**
   `WithServiceAccountKeyPath` alongside it: the SDK short-circuits on a non-empty in-memory key,
   but would fall back to re-reading the path if the content were ever empty, reintroducing exactly
   the second read step 1 exists to avoid. This is where the eager PEM parse rejects unusable key
   material.
6. **Probe.** `(*Client).probe` makes one `GetServiceStatus` call with the *candidate* client. It
   deliberately does not go through `EnsureService`, which short-circuits on the cached
   `serviceReady` flag and would return success without making any call — a probe that proves
   nothing. This is the only step that distinguishes a well-formed key from a well-formed key the
   provider has revoked.

   Two details about its bound, both of which cost a bug to learn. The validation has **two** legs:
   the key flow mints a token, then the API call goes out. Only the second is context-bound — the
   SDK builds its token POST with `http.NewRequest` and no context, and bounds it solely with its
   own `clients.DefaultClientTimeout` of one minute. So `reloadProbeTimeout` is deliberately *above*
   that minute: a shorter budget can expire inside the token fetch and then fail the API call that
   follows, rejecting a perfectly good key — non-definitively, so it is retried and rejected again
   forever. And because that first leg cannot be cancelled, the probe runs on its own goroutine and
   `reload` selects on the caller's context: waiting for a hung token endpoint at shutdown would
   hold the manager past its graceful-shutdown period and turn a rolling update into a non-zero
   exit.

Only after step 6 is anything swapped. Steps 3 to 6 each produce a rejection that leaves the running
client completely untouched.

## The swap, and what survives it

```go
type clientState struct {
	api       *objectstorage.APIClient
	account   Account
	transport *retryTransport
	keyHash   string
}
```

`Client` holds an `atomic.Pointer[clientState]`. Every exported method loads it **once** on entry —
or delegates to one that does, as `BucketEndpoint` does to `BucketConnInfo` — and passes the
`*clientState` down to unexported helpers: `listBucketNames`, `hasBucket`, `listCredentialsGroups`,
`findCredentialsGroupByName`, `createCredentialsGroup`, `listAccessKeyIDs`, `deleteAccessKey`.

That threading is not decoration. Four operations make more than one API call — `EnsureService`,
`WaitBucketVisible`, `EnsureCredentialsGroup` and `DeleteAllAccessKeys` — and loading per leaf call
would let a look-up that missed under one key be answered by a create under another, or a drain to
delete only the keys the listing happened to see.

`region` and `endpoint` stay on `Client` rather than in `clientState`: they are install-time
settings, not properties of the key.

**The retired pool is drained through the transport, not the http.Client.** `clientState` keeps the
`*retryTransport` for one reason: after `objectstorage.NewAPIClient` runs, the `http.Client`'s
`Transport` is the key flow's round tripper, and `http.Client.CloseIdleConnections` only forwards to
a transport that implements the method — which neither `clients.KeyFlow` nor `retryTransport` did.
`retryTransport.CloseIdleConnections` in [`stackit/retry.go`](../../stackit/retry.go) now delegates
to its base `*http.Transport`, which reaches both the API pool and the token-endpoint pool, because
the key flow's own auth client is built on the same transport. Without that, the retired pool would
sit for the full `idleConnTimeout` while the code pretended otherwise.

**`BackgroundTokenRefreshContext` must stay unset.** The operator never calls
`config.WithBackgroundTokenRefresh`, so `KeyFlow.Init` starts no goroutine. If that ever changes,
every swap leaks a refresher goroutine that keeps signing with a retired key.

What survives a swap, and why:

| State | Survives | Why |
|---|---|---|
| The SDK's access token | no | it goes with the retired key flow, which is the whole point |
| `Client.serviceReady` | yes | project-scoped, and a reload may not change the project |
| `BucketReconciler.admin` | yes | the bootstrap S3 credential belongs to a credentials group, a project resource, not to the service account that created it. Measured live on 2026-09-20 for the case that matters here — a group and its keys stay visible and manageable after the operator authenticates as a different service account; **not verified** for a revoked creator (see below) |
| `ProviderBreaker` | reset on success | the validation has just made a successful authenticated call, which is the exact evidence `Success()` waits for |

Nothing outside `stackit` holds anything derived from the client across calls, and nothing assumes
`*stackit.Client` pointer identity beyond nil-versus-non-nil, so the controllers needed no change at
all. That is worth re-checking whenever a reconciler starts caching something it got from the
client.

## The two retry schedules

`KeyReloader.rejected` schedules the retry, and the class is decided by `isDefinitive`, which is set
by `(*Client).reload` wrapping local refusals and `ProviderRefused(err)` results in a
`definitiveError`.

| Class | Cases | Schedule |
|---|---|---|
| Definitive | empty or unparsable file, no `projectId`, foreign `projectId`, SDK rejects the key material, structured `400`/`401`/`403` on the probe | `waitIntervals` doubles from 1, capped by `maxWaitIntervals()` = `10m` / interval, reset when the content hash changes |
| Not definitive | `5xx`, a gateway or WAF page, a transport error, an unreadable file | every poll |

The backoff counts **polls**, not wall-clock time, so it needs no clock and the test is
deterministic: `ticksLeft = waitIntervals - 1`, meaning the first retry happens on the very next
poll and the gaps are then 1, 2, 4, 8 … intervals. With the default 30-second interval that is
30s, 60s, 2m, 4m, 8m, 10m.

Two properties of the schedule are easy to get wrong and are worth stating, because both were:

* **The backoff throttles the validation, never the read.** `tick` reads and hashes the file on
  every poll and only then consults `ticksLeft`, so a corrected key is picked up at the next poll
  rather than at the next scheduled attempt. Gating the read too would mean an operator who fixed
  the file waited out the remaining backoff — at the cap, ten minutes after the working key was
  already on disk. The backoff exists to bound provider calls; a `read(2)` is not one.
* **The schedule is keyed to the content that earned it**, in `backoffHash`, not to whatever the
  last poll happened to see. A poll that did not reach the provider learned nothing about the
  candidate, so it retries at once *and leaves the accrued wait alone*. Resetting on any
  non-definitive outcome would let one transient failure restart the 1-2-4-8 schedule, and the
  ten-minute cap would then bound nothing at all.

`ReloadResult.Repeated` is what keeps the log readable: a rejection is logged once per distinct file
content, and a *different* bad candidate is loud again immediately.

## The four metrics

`SAKeyReloadObserver` is a `prometheus.Collector` over its own atomics rather than a set of
`prometheus.Gauge` objects, for two reasons. It matches the house style of
[`internal/controller/metrics.go`](../../internal/controller/metrics.go), where every gauge is a
const metric; and it makes the expiry gauge **absent** rather than zero when the key file carries no
`validUntil`, which a `Gauge` cannot do without an explicit delete.

`RegisterSAKeyReloadMetrics` is deliberately separate from `RegisterBucketMetrics` and is called
only from `setupSAKeyReload`, so an operator in skeleton mode — or one with
`--stackit-sa-key-reload-interval=0` — exports none of the four rather than a reassuring zero for a
mechanism that is not running.

The series themselves, and their alerts, are [monitoring.md](../operations/monitoring.md).

## How this is tested offline

The hard part is that neither existing constructor can run the validation offline: `NewClient` talks
to the production API host, and `NewClientWithEndpoint` uses a static token and never touches the
key flow. [`stackit/keyreload_test.go`](../../stackit/keyreload_test.go) closes that with two seams,
and both are worth knowing before adding a case.

* **The control plane** is the existing in-memory fake. `NewClientWithEndpoint` now remembers the
  endpoint on the `Client`, and `newKeyFlowAPIClient` adds `config.WithEndpoint` when it is set, so
  a candidate built by a reload reaches the same fake. In production the field is always empty.
* **The token endpoint** is a local `httptest` server, reached by writing `credentials.tokenEndpoint`
  into the generated test key — the SDK honours that field whenever no token URL is configured. That
  is what makes the key flow *really* run: an assertion is signed with the test key, exchanged, and
  the resulting token is sent to the fake. The server's answer is what separates an accepted key
  (`200` with a JWT) from a revoked one (`400 {"error":"invalid_grant"}`, the shape measured against
  the live endpoint) from an intermediary (`403` carrying an HTML page).

The access token the test server returns is a structurally valid JWT with an `exp` claim and a
deliberately fake signature: the SDK only ever parses it unverified to read the expiry, so signing
it properly would prove nothing and would pull a JWT library into the package.

Beyond that suite: [`internal/controller/sakey_reload_test.go`](../../internal/controller/sakey_reload_test.go)
pins the four metrics and the breaker reset, and
[`test/integration/sa_key_reload_test.go`](../../test/integration/sa_key_reload_test.go) starts a
second manager with leader election on and the lease already held by another identity, which is the
only way to prove the poller runs on a replica that is *not* the leader. The suite table is
[testing.md](testing.md).

## What is wrong today

**The live rotation has been done at library level, not at fleet level.**
[`stackit/keyrotation_integration_test.go`](../../stackit/keyrotation_integration_test.go) rotated a
real client between two service accounts of one project against the real API on 2026-09-20: the
second key was proven and adopted, the issuer, the content hash and the state pointer all changed,
the project and the cached service-ready answer did not, and the bucket, credentials group and
access key the first account had created stayed visible and manageable. The refusal leg reached the
real token endpoint and measured a shape nothing had recorded before —
`400 {"error":"invalid_grant","error_description":"JWT signature validation failed."}`, structured
JSON and therefore definitive, as the retry policy assumes ([provider-errors.md](provider-errors.md)).

What that run did **not** do is watch a fleet. No `Bucket` went unhealthy on a dead key and
recovered on a new one in a cluster, and no suite has observed the four metrics from outside the
process. Nor did it revoke anything: it *added* a second service account rather than removing the
first, because the first is the credential the other real-API suites depend on. So the half of the
credentials-group assumption that a real rotation actually leans on — that a group survives the
revocation of the key that created it — is still reasoning.

**Nothing in this repository runs with `-race`.** The swap-atomicity test spawns concurrent readers
across twenty swaps, which proves much less in CI than it looks like. It was run manually with
`go test -race ./stackit/... ./internal/controller/...` on 2026-09-19 and passed; nothing enforces
that it is run again.

**The bootstrap admin credential is still cached for the life of the process and never probed.** A
key reload does not touch it, deliberately — but it also means a reload cannot heal an admin
credential that was destroyed at the provider, which is a different failure with a different fix.

**Not verified: whether the token endpoint has a propagation delay for a freshly issued key.** If it
answers a definitive `400` for a while after issue, the definitive backoff is what carries the key
through — activation within ten minutes at worst. Nothing in this tree can settle it.
