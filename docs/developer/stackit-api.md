# StackIT API

This page is the operator's model of the provider: the two planes it talks to, the resources they
expose, how the process authenticates, and the pitfalls that cost time when you touch
[`stackit/client.go`](../../stackit/client.go), [`stackit/retry.go`](../../stackit/retry.go) or any
caller of them. It stops where the answer stops being an API fact: how a provider error is
recognised and classified is [provider-errors.md](provider-errors.md), what goes into the isolation
document is [bucket-policy.md](bucket-policy.md), and which cloud object belongs to which `Bucket`
is [bucket-identity.md](bucket-identity.md). Everything here is verified against
`services/objectstorage v1.9.1` and `core v0.26.0` as pinned in [go.mod](../../go.mod) on
2026-09-19; where a statement rests on a live measurement instead of on the code, the measurement
and its date are named.

## The two planes

The provider exposes Object Storage through two entirely separate surfaces, with separate
protocols, separate credentials and separate capabilities. Knowing which plane an operation lives
on is the single most load-bearing fact on this page, because the split is not documented by the
SDK: the SDK is one of the two planes and is silent about the other.

| | Control plane | Data plane |
|---|---|---|
| Protocol | STACKIT Object Storage REST API | S3 |
| Go client | [`stackit-sdk-go/services/objectstorage`](../../stackit/client.go) | [`minio-go/v7`](../../stackit/s3.go) |
| Auth | Service-account key flow, bearer token | S3 access key + secret, SigV4 |
| Base URL | `https://object-storage.api.stackit.cloud` &nbsp;`# default` (SDK server config) | Per bucket, from the bucket's `urlPathStyle` |
| Who calls it | The operator only | The operator (admin credential) **and** every workload |
| Can do | Enable the service; create/delete buckets, credentials groups, access keys | Object I/O, bucket policy, bucket tagging, listings |
| Cannot do | **Write a bucket policy. Read or write a bucket tag.** | Create or delete a credentials group or an access key — neither has an S3 surface at all |

The data plane's "cannot" row says nothing about buckets on purpose. S3 *does* carry bucket-level
operations on this endpoint — `s3:DeleteBucket` and `s3:PutBucketVersioning` among them — and what
keeps a workload away from them is the isolation policy, not the API: both are in the deny list of
[ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D4. Whether the
endpoint would accept a `CreateBucket` or `DeleteBucket` from the *admin* credential is not
measured anywhere in this tree; the operator creates and deletes buckets over the control plane and
has never needed the answer.

The consequence is structural and shapes the whole operator: the two mechanisms the operator
depends on most — the isolation policy ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D2)
and the bucket tags, both the ownership markers `managed-by` / `owner` and the credentials-group
attribution tags `credentials-group-id` / `credentials-group-urn`
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1)
— are only reachable over S3. The operator therefore cannot work with its control-plane credential
alone; it has to hold an S3 credential of its own, which is what
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D1 decides and
[credentials.md](credentials.md) implements.

The backend behind the S3 plane is NetApp StorageGRID. It is visible in the principal URNs a
credentials group carries (`urn:sgws:identity::<account>:group/<id>` &nbsp;`# example`) and it
matters twice. First, the backend vendor documents that StorageGRID evaluates a `Deny` against the
account root as well, which is why the admin identity is exempted in every document
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D7) — that lockout
property is **not verified in this repository**, it is taken from the vendor and has never been
measured against the provider used here (ADR 0004, Residual risks). The exemption is therefore a
precaution whose necessity is assumed, not demonstrated. Second, the backend accepts S3 bucket
tagging, which the control plane does not offer at all; that one is verified, because every
provisioned bucket carries its tags.

## The resource model

```
Project  ─── the isolation boundary; one per operator deployment (ADR 0005 D1)
  └── Object Storage service      must be enabled per project before anything else works
        ├── Bucket
        │     name, region, objectLockEnabled, urlPathStyle, urlVirtualHostedStyle
        │     └── bucket policy + bucket tags   (S3 only, not in this model's API)
        └── Credentials group ("workload account")
              credentialsGroupId, urn, displayName
              └── Access key
                    accessKey, secretAccessKey, keyId, displayName, expires
```

| Resource | Identified by | Creation-only attributes | Notes |
|---|---|---|---|
| Bucket | `name`, unique per project and region | `objectLockEnabled` | Name is 3–63 characters, DNS-conform; the SDK rejects anything outside that range before sending. The operator composes the name and freezes it ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D1/D3) |
| Credentials group | `credentialsGroupId`; `urn` is the policy principal | none | `displayName` is not unique and carries no meaning to the operator ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D3) |
| Access key | `keyId` for deletion, `accessKey` for S3 | `expires` | `secretAccessKey` exists in exactly one response, at creation |

`objectLockEnabled` can be set only when the bucket is created and, per the SDK's own parameter
documentation, requires an active project-level compliance lock. The operator never sets it:
[`Client.CreateBucket`](../../stackit/client.go) calls `CreateBucket(...).Execute()` without the
`ObjectLockEnabled` option, so every provisioned bucket has it off. Because the attribute is
creation-only, turning it on later would mean recreating the bucket, which the operator will not
do.

The bucket's two URL fields are the reason no S3 endpoint is hardcoded anywhere in this repo.
[`Client.BucketConnInfo`](../../stackit/client.go) reads `urlPathStyle` with `GetBucket` and parses
host and full bucket URL out of it; [`Client.BucketEndpoint`](../../stackit/client.go) builds
`scheme://host` from the same value. For eu01 that resolves to
`object.storage.eu01.onstackit.cloud` &nbsp;`# example`, path-style, SigV4 — the operator gets
there by parsing, not by knowing.

## Authentication

The token flow was switched off by the provider on 2025-12-17, so the key flow is the only way in.
Its shape matters because two of the operator's more surprising behaviours follow from it.

The service-account key is a JSON document that **embeds the RSA private key**, so no separate key
file is needed. The fields the operator and the SDK read:

<details>
<summary>Service-account key file fields</summary>

| Field | Read by | Purpose |
|---|---|---|
| `projectId` | [`stackit.LoadAccount`](../../stackit/client.go) | The project this deployment serves; there is no project flag ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D1) |
| `credentials.iss` | `LoadAccount`, and separately the SDK | `Account.Issuer`, whose only non-test use is one error message — the client-construction failure in [`stackit.NewClient`](../../stackit/client.go). No log line carries it; the startup line in [`cmd/main.go`](../../cmd/main.go) prints project and region. The SDK parses the same field into its own key struct, where it backs `KeyFlow.GetServiceAccountEmail` |
| `credentials.privateKey` | SDK (`core/auth`) | The RSA key the self-signed JWT is signed with |
| `credentials.kid` / `sub` / `aud` | SDK | JWT header and claims |
| `credentials.tokenEndpoint` | SDK | Token endpoint; when absent the SDK falls back to `https://service-account.api.stackit.cloud/token` &nbsp;`# default` |
| `id`, `publicKey`, `active`, `createdAt`, `validUntil`, `keyAlgorithm`, `keyOrigin`, `keyType` | parsed by the SDK, acted on by nothing | `clients.ServiceAccountKeyResponse` in `core v0.26.0` declares a typed field for each, so they are unmarshalled; no code path in the SDK or in this repo reads them. Notably `validUntil` is parsed and then ignored, so an expired key file is not detected before the token exchange fails |

A structurally valid key file for offline tests is generated by `writeTestSAKey` in
[`stackit/newclient_test.go`](../../stackit/newclient_test.go); it is the cheapest reference for
the exact shape.

</details>

Flow: the SDK signs a JWT with the private key, exchanges it at the token endpoint for a bearer
token that lives roughly 600 seconds — about ten minutes — and refreshes on its own. The operator
passes only the path — `config.WithServiceAccountKeyPath(acc.KeyPath)` in
[`stackit.NewClient`](../../stackit/client.go) — never handles the token and keeps no token cache
of its own; at that lifetime there would be nothing worth caching.

The ten minutes are a **recorded measurement**, taken against the provider on 2026-06-30 and not
re-measured since; no code here and none in the SDK asserts a lifetime. Verified in `core v0.26.0`:
the SDK reads the expiry from the token's own `exp` claim (`tokenExpired` in
`clients/auth_flow.go`) and treats the token as expired a fixed 5 s early
(`defaultTokenExpirationLeeway`, installed by `KeyFlow.Init` and never overridden here). The
refresh is lazy rather than scheduled — `KeyFlow.RoundTrip` asks `GetAccessToken` on every request
and exchanges a new token only when that check says the current one is gone; the SDK's
background-refresh goroutine stays off because nothing sets `BackgroundTokenRefreshContext`. The
order of magnitude is what carries the operational consequence: a service-account key revoked at
the provider stops working inside a running process in minutes, not hours. Not verified, and the
gap in exactly that sentence: `recreateAccessToken` prefers the refresh-token grant while the
refresh token is still valid, and the refresh token's lifetime was never measured here.

**The key is snapshotted at client construction, and a replaced key file has no effect until the
process restarts.** Verified in `core v0.26.0`: `auth.SetupAuth` → `auth.KeyAuth` reads the file
into the configuration, unmarshals it, and hands the struct plus the PEM string to
`clients.KeyFlow.Init`, whose `validate()` parses the PEM into an in-memory `*rsa.PrivateKey`. Every
later refresh signs with that in-memory key (`GetAccessToken` → `recreateAccessToken` →
`createAccessToken` → `generateSelfSignedJWT`); nothing re-reads the path. On the operator's side
the path is consumed once, in [`cmd/main.go`](../../cmd/main.go), outside any loop, and nothing in
the tree watches the file (`fsnotify` is an indirect dependency only). This is the mechanism behind
[ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D8: project, credential and
region are fixed for the life of the process. A key rotation is a pod restart.

Two further consequences worth carrying:

- **A revoked key never reaches the Object Storage API.** The failure comes from the token endpoint,
  and the SDK stamps that endpoint's status and body into the same
  `*oapierror.GenericOpenAPIError` it uses for API errors (`clients/auth_flow.go`,
  `parseTokenResponse`). Measured live on 2026-08-25: `400 {"error":"invalid_grant"}`. That is why
  the definitive-refusal set in [`stackit/errors.go`](../../stackit/errors.go) contains `400` at all
  — see [provider-errors.md](provider-errors.md).
- **Without a key path there is no client.** `stackitClient` stays `nil` and the operator runs in
  skeleton mode ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) D6); a key
  path that cannot be read aborts startup instead (D7).

In the cluster the key is a mounted Secret. The chart mounts
`stackit.serviceAccountKey.secretName` at `/etc/stackit` and passes
`--stackit-sa-key-path=/etc/stackit/<secretKey>`, with `secretKey: sa-key.json` &nbsp;`# default`
— see [deployment.md](../operations/deployment.md).

## The control-plane operations

Every call is region-scoped: the signature is `(ctx, projectId, region, …)` and both land in the
path. There is no client-side default region — [`cmd/main.go`](../../cmd/main.go) supplies it from
`--stackit-region` / `STACKIT_REGION`, defaulting to `eu01` &nbsp;`# default` via
`stackit.RegionEU01`.

These are the operations the wrapper in [`stackit/client.go`](../../stackit/client.go) exposes:

| Wrapper method | SDK call | HTTP | Path under `/v2/project/{projectId}/regions/{region}` |
|---|---|---|---|
| `EnsureService` | `GetServiceStatus`, then `EnableService` only on a structured 404 | `GET`, `POST` | *(the region root)* |
| `CreateBucket` | `CreateBucket` | `POST` | `/bucket/{bucketName}` |
| `DeleteBucket` | `DeleteBucket` | `DELETE` | `/bucket/{bucketName}` |
| `ListBucketNames`, `HasBucket`, `WaitBucketVisible` | `ListBuckets` | `GET` | `/buckets` |
| `BucketConnInfo`, `BucketEndpoint` | `GetBucket` | `GET` | `/bucket/{bucketName}` |
| `CreateCredentialsGroup` | `CreateCredentialsGroup` | `POST` | `/credentials-group` |
| `DeleteCredentialsGroup` | `DeleteCredentialsGroup` | `DELETE` | `/credentials-group/{groupId}` |
| `ListCredentialsGroups`, `FindCredentialsGroupByName`, `EnsureCredentialsGroup` | `ListCredentialsGroups` | `GET` | `/credentials-groups` |
| `CreateAccessKey` | `CreateAccessKey` | `POST` | `/access-key` &nbsp;(+ `?credentials-group=`) |
| `DeleteAccessKey`, `DeleteAllAccessKeys` | `DeleteAccessKey` | `DELETE` | `/access-key/{keyId}` &nbsp;(+ `?credentials-group=`) |
| `ListAccessKeyIDs` | `ListAccessKeys` | `GET` | `/access-keys` &nbsp;(+ `?credentials-group=`) |

`FindCredentialsGroupByName` and `EnsureCredentialsGroup` are the display-name path, and they have
exactly one legitimate caller left: `ensureAdmin` in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go),
resolving the shared `operator-admin` group. That carve-out is
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D6; using either
method for a workload group is forbidden by
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D3. Verified on
2026-09-19: `ensureAdmin` is the only non-test caller.

<details>
<summary>Control-plane operations the SDK offers and the operator does not use</summary>

| SDK call | HTTP | Path suffix | Why unused |
|---|---|---|---|
| `DisableService` | `DELETE` | *(region root)* | The operator enables the service and never turns it off; disabling is a project-wide act it has no mandate for |
| `GetCredentialsGroup` | `GET` | `/credentials-group/{groupId}` | A per-group read that would answer the existence question directly — see [What is wrong today](#what-is-wrong-today) |
| `CreateComplianceLock`, `GetComplianceLock`, `DeleteComplianceLock` | `POST`/`GET`/`DELETE` | `/compliance-lock` | Object Lock is never enabled on a provisioned bucket |
| `GetDefaultRetention`, `SetDefaultRetention`, `DeleteDefaultRetention` | `GET`/`PUT`/`DELETE` | `/bucket/{bucketName}/default-retention` | No retention model in the `Bucket` CRD |

</details>

### Service enablement is a read that must not become a write

`EnsureService` enables the service **only** on the API's definitive "not enabled" answer, a
structured JSON `404`. Any other failure leaves the status unknown and is returned unchanged, so a
failed read is never escalated into an attempted write. A verified answer is then cached in an
`atomic.Bool` for the life of the process, because a project cannot be un-enabled underneath a
running operator without every other call failing too, and a restart re-verifies. The reasoning
behind the discriminator lives in [provider-errors.md](provider-errors.md); the shape of the
mistake it prevents is on record: on 2026-08-25 a two-minute provider blip produced 342 reconcile
errors because an unreadable status read as "disabled" on every `Bucket` of the fleet.

After `EnableService` the call polls `GetServiceStatus` every 2 seconds for up to 30 seconds. In
*that* loop every error reads as "not ready yet" — the opposite of the rule above, and deliberately
so: the service was just enabled, propagation errors are expected, and the deadline bounds the
wait.

## Pitfalls

Each of these cost a debugging session once. They are properties of the provider or of the SDK, not
of this code, so they survive refactoring.

| # | Pitfall | Symptom if you get it wrong | Where it is handled |
|---|---|---|---|
| 1 | `CreateAccessKey` needs a payload object although the payload has no fields | `createAccessKeyPayload is required and must be specified` — a client-side error, no request is sent | `NewCreateAccessKeyPayload()` in [`Client.CreateAccessKey`](../../stackit/client.go) |
| 2 | All three access-key calls — `CreateAccessKey`, `ListAccessKeys`, `DeleteAccessKey` — carry the group as the `credentials-group` query parameter | Measured for `DeleteAccessKey` only: `500` from the API, not a `400`. What the other two answer without the parameter is **not measured**; the offline fake models both as `404` | `.CredentialsGroup(groupID)` on all three calls in [`stackit/client.go`](../../stackit/client.go) |
| 3 | A credentials group cannot be deleted while it still holds access keys | `422` | `DeleteAllAccessKeys` before `DeleteCredentialsGroup`; the teardown order is buckets → keys → groups |
| 4 | Deleting a non-empty bucket is refused | `409` — recorded from a measurement on 2026-06-30 and never re-measured since, see [What is wrong today](#what-is-wrong-today) | The operator never gets there: emptiness is checked first over S3 ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D2/D3) |
| 5 | Creating a bucket whose name exists is refused | `409` | `ensureBucket` treats only `409` as a create race and falls through to the ownership check |
| 6 | The three fields of the access-key response mean different things | Writing the wrong one into the Secret yields credentials that never authenticate, or a key that cannot be deleted | `accessKey` = S3 access-key id, `secretAccessKey` = S3 secret, `keyId` = the deletion handle; mapped once in `AccessKey` in [`stackit/client.go`](../../stackit/client.go) |
| 7 | `secretAccessKey` is returned exactly once, at creation | A key whose secret was lost is worthless and can only be replaced | The Secret is the source of truth for the live credential ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)); see [credentials.md](credentials.md) |
| 8 | `ListAccessKeys` returns `displayName`, `expires` and `keyId` only — never the access key id and never the secret | Listing cannot tell you which key a Secret holds | `ListAccessKeyIDs` returns key ids, and is used to *count* and to *delete*, never to identify |
| 9 | No S3 endpoint is a constant | A hardcoded host breaks in a second region and in every test fake | Derived from the bucket's `urlPathStyle`: `BucketConnInfo` / `BucketEndpoint` |
| 10 | The S3 plane is path-style and SigV4 | Virtual-hosted addressing against STACKIT fails to resolve | `minio.BucketLookupPath` + `credentials.NewStaticV4` in `NewS3Admin`; `NewS3VirtualHosted` exists only for clone *sources* that need AWS-style addressing ([clone.md](clone.md)) |
| 11 | `config.WithMaxRetries` has been a no-op since `core v0.26.0` (`func WithMaxRetries(_ int)`), as have `WithWaitBetweenCalls` and `WithRetryTimeout` | The SDK retries nothing, silently; a single dropped connection is a failed reconcile | Own transport, see below |
| 12 | `config.WithHTTPClient`'s transport becomes the **inner** transport of the auth flow | A round tripper installed there sees API requests *with* the `Authorization` header already attached, and sees token fetches too | `auth.KeyAuth` copies `cfg.HTTPClient.Transport` into `clients.KeyFlowConfig.HTTPTransport` (`core v0.26.0`, `auth/auth.go`); relied on by `retryingHTTPClient()` |
| 13 | Bucket names are validated client-side to 3–63 characters | The error arrives from the SDK, not from the API, and looks nothing like a provider refusal | Name composition validates first ([ADR 0009](../adr/0009-the-physical-bucket-name-is-composed-and-then-frozen.md) D5) |

Pitfalls 2–5 are encoded as fidelity rules in the offline fake
([`internal/stackitfake/fake.go`](../../internal/stackitfake/fake.go)), which is where they are
actually enforced against regressions — see [testing.md](testing.md). Pitfall 1 cannot be: the
missing-payload check runs inside the SDK's `CreateAccessKeyExecute` and returns before a request
is built, so no server, fake or real, ever sees it. Of pitfall 2 the fake mirrors only the measured
half, the `500` on a key deletion without the group parameter. Beyond this table the fake also
mirrors the Layer-1 refusal — a foreign `projectId` answers `403` — which is a property of the
isolation boundary rather than of the SDK. The fake's header comment is the canonical list of
behaviours it mirrors.

## The HTTP layer beneath the SDK

Because the SDK does not retry (pitfall 11) and adopts a caller's transport underneath
authentication (pitfall 12), [`stackit/retry.go`](../../stackit/retry.go) installs a
`retryTransport` there. Its rules are narrow on purpose:

| Property | Value | Reason |
|---|---|---|
| Attempts | 3 (1 try + 2 retries) | Worst-case added latency stays well under a second, so a retrying request never outlives a reconcile |
| Backoff | 200 ms, then tripled | Same bound |
| Methods retried | `GET` and `HEAD` only, and only with no request body | Repeating a write is ambiguous: a repeated `CreateAccessKey` mints a second key whose secret is returned once and then lost, leaking a credential nothing tracks |
| Retried outcomes | Any transport error, and `5xx` | Neither says anything about the request |
| **Not** retried | `429`, and every other `4xx` | A `4xx` is an answer; repeating a rate limit is the one reply guaranteed to make it worse. Backing off from a `429` belongs to the workqueue and the circuit breaker ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) D9) |
| `IdleConnTimeout` | 30 s, on a *clone* of `http.DefaultTransport` | The operator talks in bursts and is idle in between, so a pooled connection is likely already dropped at the edge; closing first turns `connection reset by peer` into a fresh dial. The 30 s is not matched to a measured edge timeout — see [What is wrong today](#what-is-wrong-today) |
| `http.Client.Timeout` | unset, matching the SDK's own default | Deadlines stay the caller's context's business |

None of these values is configurable, which is why none of them is marked as a default: `retryAttempts`,
`retryBackoff` and `idleConnTimeout` are compile-time constants at the top of
[`stackit/retry.go`](../../stackit/retry.go), with no flag, environment variable or chart key behind
them. Changing one is a code change and a release.

Two follow-on facts that are easy to miss:

- **The key flow's token fetch is a `POST` and is therefore not retried.** It traverses the same
  transport. A token fetch failing during a provider blip fails the whole reconcile, which the
  requeue and the degraded hold already cover ([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D1)
  — preferable to carving a per-endpoint exception into the rule that no write is ever repeated.
- **The S3 data plane is not covered by this transport and does not need to be**: `minio-go` retries
  retryable failures itself.

`NewClientWithEndpoint` — the constructor used by offline tests against
[`internal/stackitfake`](../../internal/stackitfake/fake.go) — deliberately omits the retrying
transport. The fake arms a single injected failure, and retrying would consume the injection and
quietly turn an error-path test into a happy-path test. `retryTransport` is covered directly in
[`stackit/retry_test.go`](../../stackit/retry_test.go) instead.

## The deprecated top-level package

Every symbol the operator imports from `services/objectstorage` carries
`Deprecated: Will be removed after 2026-09-30. Move to the packages generated for each available
API version instead`. The replacement packages ship in the same module and are already present at
v1.9.1: `objectstorage/v1api` and `objectstorage/v2api`. The top-level package the operator uses
today is generated from API version 2.0.1 and its request paths are already `/v2/...`, so the
migration target is `v2api`.

Verified on 2026-09-19: [go.mod](../../go.mod) pins `services/objectstorage v1.9.1` and
[`stackit/client.go`](../../stackit/client.go) imports the top-level package. Nothing in the tree
imports `v1api` or `v2api`. The removal date is the provider's, stated in the package itself; what
happens to the top-level package after it is the provider's call, not this repo's.

## What is wrong today

- **Every existence question is answered by a project-wide listing.** `HasBucket` calls
  `ListBuckets` and scans the result, even though `GetBucket` — already used by `BucketConnInfo` —
  answers for one bucket. `WaitBucketVisible` polls that same listing every 2 seconds. Group
  resolution lists all credentials groups (`listGroups` in
  [`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go))
  even when a single id is wanted, although the SDK offers `GetCredentialsGroup`. The cost grows
  with the size of the project rather than with the work being done, and the reconcile of one
  `Bucket` reads the state of every other. The one place that already does the narrow thing is
  `groupExists`, which probes a group through its keys endpoint because that call answers the
  moment the group is created.
- **The listing lags its own writes.** A group created a moment ago may be missing from the next
  `ListCredentialsGroups`. The attribution path absorbs this by waiting and retrying rather than
  creating a second group ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D8);
  the mechanism is in [bucket-identity.md](bucket-identity.md). Treat any listing answer as
  possibly older than your last write.
- **The 30 s `IdleConnTimeout` is a guess, and the source says so.** The provider edge's own idle
  timeout was never measured; 30 s was chosen to sit below any plausible value.
- **`ProviderRefused` maps a structured `400` to "definitive", which is broader than the case it was
  written for.** The case is a revoked service-account key (`invalid_grant` from the token
  endpoint). A structured `400` from the Object Storage API itself — a malformed request — lands in
  the same bucket. That is intentional (neither is retryable) but it means a `400` in the logs does
  not by itself tell you which of the two happened; the body does. See
  [provider-errors.md](provider-errors.md).
- **Not verified: whether a non-empty bucket delete is refused by the current API.** `409` is
  recorded in the fake's fidelity notes as mirroring measured real-API behaviour from 2026-06-30,
  and no test in this tree re-measures it against the live API. The operator does not depend on it
  either way — it checks emptiness itself first
  ([ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) D3).
- **This page names files and functions on purpose and goes stale when the tree moves.** Whoever
  moves `stackit/client.go`, `stackit/retry.go` or the wrapper's exported surface updates this page
  in the same change.
