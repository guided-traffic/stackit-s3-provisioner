# Provider errors

This page is about one question: a call to the StackIT API came back with an error — what *is* that
error, and what may the operator conclude from it? It covers the recognition helpers in
[`stackit/errors.go`](../../stackit/errors.go), the retrying transport in
[`stackit/retry.go`](../../stackit/retry.go), the classification path in
[`internal/controller/bucket_controller.go`](../../internal/controller/bucket_controller.go) that
turns a recognised error into a readiness decision, and the two questions about *existence* that are
answered from an error shape in [`stackit/client.go`](../../stackit/client.go). The rule it implements is
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md); the fleet-wide behaviour that
sits *after* classification is [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) and
its mechanism is [circuit-breaker.md](circuit-breaker.md). If you wanted which call site routes
which error into which terminal state, that is
[reconcile-pipeline.md](reconcile-pipeline.md#guards-and-their-outcome-class); if you wanted the API
surface and the SDK pitfalls themselves, that is [stackit-api.md](stackit-api.md); if you wanted
what an on-call engineer sees during an outage, that is
[../operations/provider-outages.md](../operations/provider-outages.md).

This page names files and functions on purpose. It goes stale when the tree moves, and whoever moves
the tree updates it in the same change.

## Where a status code comes from

Every non-2xx answer from the StackIT **control plane** — the Object Storage API or the token
endpoint behind the key flow — arrives as exactly one Go type, `*oapierror.GenericOpenAPIError`, and
it is always reached with `errors.As` because the reconciler wraps errors several layers deep
(`fmt.Errorf("enable object storage: %w", …)`). Two different producers fill that type, and this is
the fact the whole page rests on:

| Producer | Where | What it puts in the error |
| --- | --- | --- |
| The generated Object Storage client | `api_default.go` in the SDK's `objectstorage` package (`v1.9.1`, pinned in [`go.mod`](../../go.mod)) | `StatusCode` and the raw response `Body`; for a handful of enumerated statuses also a decoded `Model` |
| The key flow's token fetch | `parseTokenResponse` in the SDK's `core/clients/auth_flow.go` (`core v0.26.0`) | `StatusCode` and the raw `Body` of the **token endpoint's** answer, nothing else |

So an error carrying `403` may have come from the Object Storage API, from an intermediary in front
of it, or — carrying `400` — from `https://accounts.stackit.cloud/oauth/v2/token`, which the
operator never calls directly. The type does not say which.

**The S3 data plane is a different client and a different error type.** `minio-go` returns
`minio.ErrorResponse`, read through `minio.ToErrorResponse` — for example in `S3Admin.BucketTags` in
[`stackit/s3.go`](../../stackit/s3.go), where `NoSuchTagSet` or a `404` means "this bucket has no
tags". The clone controller is a third shape again: it reads a raw `resp.StatusCode` off rclone's
`rc` endpoint in [`clone.go`](../../internal/controller/clone.go). None of the helpers below apply
to either. Both still reach the readiness decision, because `degrade` takes failures of the control
plane, the data plane and the Kubernetes API alike — see
[How classification maps onto reconcile outcomes](#how-classification-maps-onto-reconcile-outcomes)
and [The retry round-tripper](#the-retry-round-tripper).

Five helpers read the control-plane type, and they are not interchangeable:

| Helper | File | Answers | Body-aware |
| --- | --- | --- | --- |
| `apiAnswer(err) (status, ok)` | [`stackit/errors.go`](../../stackit/errors.go) | Did *the provider* decide this, and with which status? | Yes — `json.Valid` over the raw body |
| `ProviderRefused(err) bool` | [`stackit/errors.go`](../../stackit/errors.go) | Is this a structured refusal the operator must treat as definitive? | Yes, via `apiAnswer` |
| `isStructuredNotFound(err) bool` | [`stackit/errors.go`](../../stackit/errors.go) | Is this the API's own definitive "this does not exist"? | Yes, via `apiAnswer` |
| `isServiceNotEnabled(err) bool` | [`stackit/errors.go`](../../stackit/errors.go) | Is this the API's definitive "Object Storage is not enabled here"? | Yes — it is `isStructuredNotFound` under a name that says which resource |
| `StatusCode(err) int` | [`stackit/client.go`](../../stackit/client.go) | What status does this error carry, whichever of the two producers filled it? (`0` for anything that is not the SDK type) | **No** |

`apiAnswer` and `isStructuredNotFound` are unexported on purpose: the questions spelled out above
are the only ones the operator is entitled to ask of a status code, and the two that read a `404` do
it through a helper rather than by comparing a number. `StatusCode` is the deliberate exception and
is discussed in
[Where a bare status code is still used](#where-a-bare-status-code-is-still-used).

## Why the discriminator is the body shape, not the status code

`apiAnswer` returns `ok` only when the error is a `*oapierror.GenericOpenAPIError` **and**
`json.Valid(apiErr.GetBody())` holds. An empty body counts as not structured, because an
intermediary that drops the body would otherwise look authoritative.

The reason is measured, not theoretical. On 2026-08-25 between 08:13 and 08:15 UTC the Object
Storage API answered `403` as an **nginx HTML error page**. The SDK wrapped that page in the same
`*oapierror.GenericOpenAPIError` it uses for a real refusal, carrying the same status code. Reading
`403` alone, the operator concluded "the provider refuses us" for 19 buckets at once; the incident
record and the readiness rule that came out of it are
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md).

`oapierror.Model` is **not** a usable stand-in for the body test, and the reason is worth spelling
out because it looks like the obvious solution:

- The generated client decodes an error body into `objectstorage.ErrorMessage`, a struct with a
  single field, `Detail`. A genuine `403` from the authorization layer — whose JSON carries
  `timestamp`, `path`, `status`, `error`, `message` and no `detail` — unmarshals *successfully* into
  an empty struct. The decoded value therefore says nothing.
- The decode only happens at all for the statuses each generated operation enumerates — `401`, `403`,
  `404`, `422` and `500` for `GetServiceStatus`, and only the first three plus `500` into
  `ErrorMessage` — so anything else leaves `Model` nil regardless of the body. A token-endpoint
  error never passes through the generated client and so never has a `Model` at all.
- For an HTML body the decode fails on the content type, and the SDK replaces the error message with
  the literal `undefined response type` — which is exactly the string in the 2026-08-25 08:13 log
  lines. That failure mode is a side effect of content-type handling by an intermediary the operator
  does not control, not a property it may build on.

Only the raw body separates the two. `TestModelIsNotADiscriminator` in
[`stackit/errors_test.go`](../../stackit/errors_test.go) pins this.

## The measured service-status shapes

Live measurement on 2026-08-25 against region `eu01`, calling `GetServiceStatus`. The bodies are
kept verbatim as constants in [`stackit/errors_test.go`](../../stackit/errors_test.go) so the tests
exercise the real shapes rather than paraphrases.

| Case | Status | Body (`# example`, captured 2026-08-25) | `json.Valid` |
| --- | --- | --- | --- |
| Object Storage enabled | `200` | `{"project":"<uuid>","scope":"PUBLIC"}` | — |
| Object Storage **not** enabled | `404` | `{"detail":[{"key":"project.not_found","msg":"The project could not be found"}]}` | `true` |
| Not authorized | `403` | `{"timestamp":"…","path":"…","status":403,"error":"Forbidden","message":"Unauthorized"}` | `true` |
| Intermediary error page | `403` | `<html>…<center>nginx</center>…</html>` | `false` |

**The caveat travels with the measurement.** Both `403` observations were taken with a *valid* key
against project ids that do not exist, and a counter-check against two real projects outside the
key's reach answered `200`. "This key may not use this project" is therefore a reading of those
numbers, not the case that was isolated — see the closing "Not verified" of
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md). What the measurement *does*
establish, and all that the code depends on, is that each provider answer carries a structured JSON
body while the intermediary's page does not. Neither measurement has been repeated since
2026-08-25.

## A revoked service-account key is a token-endpoint 400

A deleted or rotated service-account key does not produce an API `401` or `403`. It never reaches
the Object Storage API at all: the key flow fails earlier, at the token endpoint. Measured live on
2026-08-25 against `https://accounts.stackit.cloud/oauth/v2/token`:

| Case | Status | Body (`# example`, captured 2026-08-25) |
| --- | --- | --- |
| Key revoked or absent | `400` | `{"error":"invalid_grant"}`, content type `application/json` (RFC 6749 §5.2) |

`parseTokenResponse` stamps that status and body into the same `*oapierror.GenericOpenAPIError` used
for API errors, so the failure surfaces at the operator wearing an API error's clothes. That is why
`ProviderRefused` matches `400` alongside `401` and `403`, and why the match is *not* an auth check:

```go
switch status {
case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
    return true
}
```

A first draft of the definitive set matched only `401` and `403`. With it, a revoked key would have
left the entire fleet advertising `Ready` for the full degradation grace while every credential the
operator holds was dead — the one degradation that must never be masked
([ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md)). The trap is that the
missing case is invisible in a green cluster; the regression tests
`TestProviderRefused/revoked_key:_token_endpoint_400_invalid_grant` and
`.../revoked_key_wrapped_by_the_SDK_and_the_reconciler` exist to keep it that way.

The flip side: `400` also covers a genuinely malformed request the API rejects. Such a bucket drops
`Ready` immediately instead of being held, which is the intended direction — a request the provider
will never accept does not fix itself — but it means a provider-side tightening of request
validation surfaces as a hard failure, not as a degradation.

**The same classification is what the key reload's retry policy keys off.** When a rotated key is
put on trial, `(*Client).reload` in [`stackit/keyreload.go`](../../stackit/keyreload.go) makes one
live `GetServiceStatus` call with the candidate client and asks `ProviderRefused` about the answer.
A match is the provider itself deciding: either the token endpoint refusing to mint a token for the
candidate (`400 invalid_grant`, the case above) or the API refusing the probe made with it
(a structured `401`/`403`). Either way the rejection is marked *definitive* and the same bytes are
retried on a doubling schedule from one poll interval up to ten minutes. Anything else is the provider not answering at all — a `5xx`, a
gateway page, a transport failure — and says nothing about the key, so the candidate is retried
every interval with no backoff
([ADR 0016 D7](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)). It is
the same `apiAnswer` body test doing the same job one layer over: an nginx page carrying `403` must
not be allowed to condemn a key that is perfectly good.

**Not verified: whether a freshly issued key answers with a definitive `400` while it propagates.**
Nothing here has measured how long STACKIT's token endpoint takes to recognise a key it has just
issued, and if there is such a window the answer during it is indistinguishable from a revoked key —
a structured `400`, classified definitive, backed off. The backoff is written so the answer does not
change the outcome: the retry is capped at ten minutes and is never given up on while the file
content differs from the loaded key, so a key that needed time activates by itself within ten
minutes at worst. Holding a definitively rejected hash until the file changes again was rejected for
exactly this case.

## Service status: only a structured 404 may lead to a write

`EnsureService` in [`stackit/client.go`](../../stackit/client.go) is the one place where reading an
error wrong turns into *writing* to the provider. Its shape is therefore deliberate:

1. Return immediately if the process-wide cache already holds a verified "enabled". Otherwise
   call `GetServiceStatus`; on success, cache the answer and return.
2. On an error that is **not** `isServiceNotEnabled` — i.e. not a structured JSON `404` — return it
   unchanged. The service status is now *unknown*, and unknown must never be read as "disabled".
3. Only on the structured `404`: call `EnableService`, then poll `GetServiceStatus` until it answers
   or a 30-second deadline expires.

Before this, every `GetServiceStatus` failure was read as "not enabled" and produced an attempted
`EnableService` — once per bucket, per reconcile. Nineteen buckets retrying against an edge that
answered HTML is how a two-minute blip on 2026-08-25 became 342 reconcile errors.

Note the deliberate asymmetry in step 3: inside the post-enable poll *every* error is treated as
"not ready yet", the opposite of step 2. The service was just enabled, propagation errors are
expected, and the wait is bounded by the deadline rather than by error classification.

**The verified-enabled cache.** `Client.serviceReady` is an `atomic.Bool` that short-circuits the
whole function once the API has confirmed the service is enabled, for the remaining lifetime of the
process. It is not an optimisation bolted on afterwards: without it the operator re-asks a question
whose answer cannot change under a running operator (a project cannot be un-enabled without every
other call failing too) once per bucket per reconcile, which is pure exposure to provider blips. A
restart re-verifies. Nothing invalidates the cache — a project that genuinely lost Object Storage
underneath a running operator would surface as failures on the subsequent calls instead, which are
non-definitive and therefore held.

## Absence is the second question a structured 404 answers

`isServiceNotEnabled` is one line — `return isStructuredNotFound(err)` — because the shape it tests
was never specific to the service status. A structured JSON `404` is the API saying "this does not
exist" about whatever was asked for, and two callers act on that same shape for two different
resources:

| Caller | Asks about | What the structured 404 means | What it does with it |
| --- | --- | --- | --- |
| `EnsureService`, via `isServiceNotEnabled` | the project's Object Storage service | not enabled for this project | Calls `EnableService` — turns a read into a **write** |
| `Client.BucketExists` | one named bucket | that bucket is not there | Returns `(false, nil)` — the caller may conclude a provisioned bucket is **gone** |

Both consequences are damaging when the provider never actually answered, which is why neither
caller is allowed to see a raw status code.

`BucketExists` in [`stackit/client.go`](../../stackit/client.go) asks `GetBucket`, a per-bucket
control-plane read, and it is the only place in the tree where "absent" is permitted to mean *a
bucket the operator provisioned is gone*. The full truth table:

| What comes back from `GetBucket` | `BucketExists` | Why |
| --- | --- | --- |
| `200` | `(true, nil)` | The bucket is there |
| `404` with a structured JSON body | `(false, nil)` | The provider's own answer about this one bucket |
| `404` carrying an intermediary's HTML page | error | A failure in front of the provider, not a decision by it — the nastiest case, because the status code is right and the provenance is wrong |
| `404` with an empty body | error | An intermediary that drops the body must not look authoritative |
| `404` with a truncated or otherwise invalid JSON body | error | `json.Valid` fails, so nothing answered |
| A structured `401`/`403` | error, and `ProviderRefused` matches it | A refusal is not an absence; it is definitive for readiness and never reaches this decision as "gone" |
| Any `5xx`, after the transport's retries | error | Nothing on the provider side decided |
| A transport failure or a dropped connection | error | Same, one layer lower |

**The classification lives in the client, not at the call site**
([ADR 0015 D4](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)). A caller could
have asked `GetBucket` itself and compared `StatusCode(err) == 404`; the point of not doing that is
that conflating "absent" with "could not find out" is then *structurally impossible* rather than a
rule somebody has to remember, and the cost of forgetting it once is a fleet-wide false report of
data loss. There is no path from a gateway page to `false`.

It is also deliberately not `HasBucket`. That one scans the project-wide listing and can only answer
"not in the list I got", which is weaker: the listing is known to lag a create (which is why
`WaitBucketVisible` exists), and whether it is complete for a large project is **not verified**,
because `ListBuckets` takes no pagination parameters in the SDK. Good enough to decide whether to
create a bucket, not good enough to declare one gone — the two existence questions and who asks
which are in
[reconcile-pipeline.md](reconcile-pipeline.md#two-existence-questions-and-only-one-of-them-is-a-listing).
The S3 data plane takes no part in this decision at all: it has its own error shapes, and the
question is asked on the control plane only.

## The retry round-tripper

The SDK does not retry: `config.WithMaxRetries` has been `func WithMaxRetries(_ int)` — a literal
no-op — since `core v0.26.0`, verified in the pinned module. Without a transport of its own, a
single dropped keep-alive connection is a failed reconcile.
[`stackit/retry.go`](../../stackit/retry.go) supplies one, installed by `newKeyFlowAPIClient` in
[`stackit/client.go`](../../stackit/client.go) through
`config.WithHTTPClient(&http.Client{Transport: newRetryTransport(…)})` — the one constructor both
`NewClient` and a key reload build through, each getting its own transport.

**It sits beneath the SDK's auth.** `auth.KeyAuth` in the SDK's `core/auth/auth.go` adopts
`cfg.HTTPClient.Transport` as the key flow's *inner* transport (verified in `core v0.26.0`, the
branch that builds `clients.KeyFlowConfig`). Every retry therefore carries the `Authorization`
header the flow already attached, and the transport sees the token fetches as well as the API
requests.

The three values below are compiled-in constants in [`stackit/retry.go`](../../stackit/retry.go),
not configuration: there is no flag, environment variable or Helm value that changes them, so the
`Configurable` column is `No` throughout rather than a list of knobs.

| Constant | Value | Configurable |
| --- | --- | --- |
| `retryAttempts` | `3` (one initial try plus two retries) | No |
| `retryBackoff` | `200ms`, tripled per retry | No |
| `idleConnTimeout` | `30s` on a clone of `http.DefaultTransport` | No |

Two predicates decide a retry, and both are narrow on purpose:

- `retryableRequest` — `GET` and `HEAD` only, and only with a nil body. A failed write is ambiguous:
  the server may have applied it before the connection broke, and a repeated `CreateAccessKey` would
  mint a second access key whose secret is returned exactly once and then lost, leaking a credential
  nothing tracks. The body check is belt-and-braces: a body is consumed by the first attempt, so
  replaying one would need `GetBody` bookkeeping for a case the SDK never produces. Reads dominate
  the hot path anyway (`GetServiceStatus`, `ListBuckets`, group and key listings), so the safe subset
  is also the useful one.
- `retryableResult` — any transport error, or a status `>= 500`. Every 4xx is an answer, and
  repeating it produces the same answer. `429` is excluded even though it is transient, because it
  is the one answer where re-asking is guaranteed to make things worse; the full reasoning and the
  2026-09-02 measurement behind it belong to
  [circuit-breaker.md](circuit-breaker.md#transport-changes) and
  [ADR 0013 D9](../adr/0013-a-provider-outage-is-held-fleet-wide.md).

**The token fetch is a POST and is therefore not retried.** It traverses the same transport
unretried, so a token fetch failing during a blip fails the whole reconcile. That is deliberate:
the requeue and the readiness hold already cover it, and it is preferable to carving a per-endpoint
exception into the rule that no write is ever repeated. The reasoning is recorded in
`newKeyFlowAPIClient`'s doc comment in [`stackit/client.go`](../../stackit/client.go).

**The S3 data plane is not covered by this transport and does not need to be.** `S3Admin` in
[`stackit/s3.go`](../../stackit/s3.go) is a `minio-go` client that overrides only credentials, TLS,
region and bucket-lookup style (`newS3Client`), leaving the library's own retry behaviour and
transport untouched — and `minio-go` retries on its own. Verified against the pinned
`minio-go/v7 v7.3.0`: `MaxRetry = 10` `# default`, base `200ms` `# default`, cap `1s` `# default`,
with jitter. Its retryable set is *not* the same as the control plane's — it
includes `429`, `503` and the `SlowDown` family. Worth knowing before reasoning about data-plane
traffic during an outage: the control-plane rule "never retry a rate limit" does not hold on the
data plane, and the data plane is also outside the breaker's gate
([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md), residual risks;
[usage-measurement.md](usage-measurement.md)).

## Where a bare status code is still used

`StatusCode` ignores the body. It exists for a different question — "does this answer mean the thing
is absent / already there?" — where the operator is deciding tolerance, not readiness. Every current
call site:

| Call site | Check | What it decides |
| --- | --- | --- |
| `ensureBucket` in [`bucket_controller.go`](../../internal/controller/bucket_controller.go) | `!= 409` | A create that lost a race falls through to the ownership check instead of failing |
| `groupExists` in [`bucket_controller.go`](../../internal/controller/bucket_controller.go) | `== 404` | A credentials group deleted out of band reads as gone; every other error is returned |
| `deleteBucketIfOwned` (teardown) in [`bucket_controller.go`](../../internal/controller/bucket_controller.go) | `!= 404` | An already-absent bucket does not block the finalizer |
| `releaseWorkloadGroup` (teardown) in [`bucket_controller.go`](../../internal/controller/bucket_controller.go) | `!= 404` | An already-absent group does not block the finalizer |
| `DeleteAllAccessKeys` in [`stackit/client.go`](../../stackit/client.go) | `== 404` | A missing group counts as drained; a key that vanished mid-loop is skipped |

None of these apply the `json.Valid` guard, so an intermediary page carrying `404` would read as an
authoritative absence. **Not verified, and this is the gap:** no test covers that combination, and
the only intermediary page ever observed (2026-08-25) carried `403`, which none of these sites look
at. The reachable consequences, by reasoning rather than measurement, are a spurious group re-create
in `groupExists` and a finalizer dropped over a bucket or group that still exists in the teardown
sites — the latter after the emptiness guard of
[ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) has already had to succeed on
the data plane in the same pass.

## How classification maps onto reconcile outcomes

Inside the reconciler, recognition feeds exactly one decision: may this bucket keep claiming the
state the operator last verified? That decision lives in `degrade` and `holdsReadyThrough` in
[`bucket_controller.go`](../../internal/controller/bucket_controller.go).

The classification is **by origin**, never by parsing the error text
([ADR 0012 D2](../adr/0012-ready-describes-the-last-verified-state.md)). `degrade`'s own doc comment
states its input set: *everything routed here failed while talking to the StackIT API, the S3 data
plane or the Kubernetes API*. An unrecognised failure of any of those is "could not verify", never
"verified bad", and that default is reached by not being enumerated
([D3](../adr/0012-ready-describes-the-last-verified-state.md)) — a new provider failure mode cannot
land on the wrong side of the line by being unknown.

`holdsReadyThrough` is the part of the
[ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md) exception list that `degrade`
has to decide, in executable form. Each of those rules maps to one check, in this order:

| Check in `holdsReadyThrough` | Implements | Note |
| --- | --- | --- |
| `r.ProviderDegradedGrace <= 0` | [D5](../adr/0012-ready-describes-the-last-verified-state.md) | `--provider-degraded-grace` / `PROVIDER_DEGRADED_GRACE` / Helm `providerDegradedGrace`, `30m` `# default`; `0` disables the hold entirely |
| `!b.DeletionTimestamp.IsZero()` | D6, teardown | A hold would hide a delete blocked by [ADR 0006](../adr/0006-a-bucket-is-deleted-only-when-it-is-empty.md) |
| `stackit.ProviderRefused(err)` | D6, structured `400`/`401`/`403` | The only provider-shaped exception `degrade` ever sees, and the reason this page exists |
| `errors.Is(err, errCredentialDestroyed)` | D6, destroyed credential | See below |
| `ObservedGeneration == Generation` | D6, unobserved spec | A spec that was never achieved has no verified state to defend |
| `Phase == Ready && ConditionReady` | D6, never-`Ready` | Initial provisioning failures surface at once |

The other two D6 cases never reach `degrade` at all. Configuration faults are routed to
`failNoRequeue` up front — the four call sites are enumerated in
[reconcile-pipeline.md](reconcile-pipeline.md#guards-and-their-outcome-class) — and a provisioned
bucket the provider reports as absent is marked by `guardBucketPresent` itself, on the third outcome
class described there.

**`errCredentialDestroyed` is what makes the local-certainty exception effective.**
`ensureAccessKeyAndSecret` deletes every key of the workload group *before* creating the replacement,
which is what keeps the path leak-free. From that moment the credential published in the workload
Secret is known dead, so both failures after the clear — the `CreateAccessKey` and the Secret write —
are wrapped:

```go
return "", fmt.Errorf("%w: create replacement access key: %w", errCredentialDestroyed, err)
```

Without that wrapping the error would be an ordinary provider failure and the hold would paper over
a bucket whose workloads can no longer authenticate. It is reachable without a spec change, because
the rotation trigger is an annotation and `metadata.generation` does not move for it, so the
unobserved-spec check would not catch it either
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md),
[credentials.md](credentials.md)).

**What a hold records.** `status.degradedSince` (written once, at the start of a run of failures)
and the condition `ProviderReachable=False` with reason `ProviderUnreachable` and the error as its
message. On top of that, `status.message` is set to the error and a `Warning` event with reason
`Failed` is recorded — both emitted exactly as a hard failure would emit them, so the reason a
bucket is held is always on the object
([ADR 0012 D4](../adr/0012-ready-describes-the-last-verified-state.md)). What is unchanged is the
*behaviour* of those two signals, not the value of `status.message`: a held bucket carries the
current provider error there, not the message it had while it was last verified. `clearDegraded`
removes the timestamp and the condition on the next successful pass — the condition is removed
rather than set to `True`, so a bucket that never degraded and one that recovered look identical.
The fleet view is `stackit_s3_provisioner_buckets_provider_degraded` and
`stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds`
([../operations/monitoring.md](../operations/monitoring.md)).

**What classification feeds into the breaker.** Only `fail` calls `Breaker.Failure()`;
`failNoRequeue` touches the breaker in neither direction, which is
[ADR 0013 D3](../adr/0013-a-provider-outage-is-held-fleet-wide.md) in code. Note the consequence of
`degrade`'s origin set: a failure while talking to the *Kubernetes* API — `persistResolvedName`,
`ensureAdmin` reading or writing the admin Secret, a clone job's status read — is non-definitive
too, and therefore counts toward the trip exactly like a provider failure. The trip condition is the
absence of success, not the origin of the error
([ADR 0013 D2](../adr/0013-a-provider-outage-is-held-fleet-wide.md)), so this is consistent rather
than accidental — but it does mean "provider circuit open" can be reached without the provider being
at fault.

**Measurement is outside all of this.** `BucketUsageReconciler` in
[`bucket_usage_controller.go`](../../internal/controller/bucket_usage_controller.go) never touches
`Ready` — `bucketIsReady` is the only place it reads that condition, and it only reads. It returns a
reconcile error only when it cannot read the `Bucket` object itself: the initial `Get` returns
`client.IgnoreNotFound(err)`, which is non-nil for an apiserver outage or a withdrawn RBAC grant.
Every measurement failure after that point is recorded in `status.usage` and retried on a shortened
interval instead. The controller's own doc comment states the contract in its absolute form ("It
never returns an error"), and that holds for every path past the first `Get`. It also never consults
the breaker
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md),
[usage-measurement.md](usage-measurement.md)).

## Tests that pin this

<details>
<summary>Test coverage for recognition, retry and the hold</summary>

| Test | File | What it pins |
| --- | --- | --- |
| `TestProviderRefused` | [`stackit/errors_test.go`](../../stackit/errors_test.go) | The full truth table incl. the gateway page at `403` and `400`, empty bodies, the revoked key, and wrapped errors |
| `TestIsServiceNotEnabled` | [`stackit/errors_test.go`](../../stackit/errors_test.go) | Only a structured `404` may lead to `EnableService` |
| `TestModelIsNotADiscriminator` | [`stackit/errors_test.go`](../../stackit/errors_test.go) | Why the raw body and not `oapierror.Model` |
| `TestRetryTransportRecoversFromBlip` / `…GivesUp` | [`stackit/retry_test.go`](../../stackit/retry_test.go) | Retry on transport errors and 5xx, bounded by `retryAttempts` |
| `TestRetryTransportDoesNotRetryDefiniteAnswers` | [`stackit/retry_test.go`](../../stackit/retry_test.go) | Exactly one request for `400`, `401`, `403`, `404`, `409`, `422` and `429` |
| `TestRetryTransportLeavesWritesAlone` | [`stackit/retry_test.go`](../../stackit/retry_test.go) | `POST`/`PUT`/`PATCH`/`DELETE` get one try even on `503` |
| `TestRetryTransportHonoursContext` | [`stackit/retry_test.go`](../../stackit/retry_test.go) | A cancelled reconcile aborts the backoff |
| `TestNewRetryTransportDefaults` | [`stackit/retry_test.go`](../../stackit/retry_test.go) | The transport clone and the idle timeout |
| `TestBucketExistsOnlyTrustsAStructuredAnswer` | [`stackit/client_fake_test.go`](../../stackit/client_fake_test.go) | Every row of the `BucketExists` truth table above: only the structured `404` yields `false`, while the HTML page at `404`, the empty body, the truncated body, a `503`, a structured `403` and a closed endpoint all come back as errors |
| `TestUnreachableProviderNeverTripsTheGuard` | [`internal/controller/reconciler_missing_bucket_test.go`](../../internal/controller/reconciler_missing_bucket_test.go) | The same distinction as the reconciler sees it: `BucketPresent` is never written and the Bucket stays held |
| `TestProviderRefusalIsNeverHeld` / `TestGatewayPageIsHeldEvenWith403` | [`internal/controller/reconciler_degraded_test.go`](../../internal/controller/reconciler_degraded_test.go) | The body-shape discriminator as the reconciler sees it |
| `TestDegradedHoldsReadyThroughTransientFailures` / `…RecoversOnNextSuccess` / `…GraceExpires` / `…DisabledByZeroGrace` | [`internal/controller/reconciler_degraded_test.go`](../../internal/controller/reconciler_degraded_test.go) | Hold, recovery, grace expiry and the `0` rollback |
| `TestInitialProvisioningFailureIsNotHeld` / `TestSpecChangeFailureIsNotHeld` / `TestTeardownFailureIsNotHeld` / `TestConfigFaultIsNotHeld` / `TestDestroyedCredentialIsNeverHeld` | [`internal/controller/reconciler_degraded_test.go`](../../internal/controller/reconciler_degraded_test.go) | Five more of the D6 exceptions. The seventh, a provisioned bucket the provider reports as absent, never reaches `holdsReadyThrough` and is pinned in [reconciler_missing_bucket_test.go](../../internal/controller/reconciler_missing_bucket_test.go) |

All of these are offline; they run in the default `go test ./...` suite
([testing.md](testing.md)).

</details>

## What is wrong today, and what could not be verified

**The whole classification is exercised against a simulated provider only.** The hold, the grace
expiry, the immediate drop on a structured refusal and every exception have offline coverage, but no
test runs this path against a real provider outage. The last real data points are the 2026-08-25 and
2026-09-02 incidents, both reconstructed from logs and metrics after the fact.

**The measured API semantics are a single sample from 2026-08-25.** Both `403` observations were
taken against project ids that do not exist, with a valid key; the entitlement reading is an
interpretation and should not be quoted as measured fact
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md), "Not verified"). The
revoked-key `400` was measured separately at the token endpoint on the same day. Neither has been
repeated.

**The tolerance checks bypass the discriminator.** The five `StatusCode` call sites listed
[above](#where-a-bare-status-code-is-still-used) read a status code without the `json.Valid` guard.
The failure mode is unobserved and untested. `BucketExists` is the one absence check that does not
bypass it, and it is the one where being wrong reports data loss.

**`ProviderRefused` cannot tell a revoked key from a malformed request.** Both are a structured
`400`, and both drop `Ready` at once. The operator log carries the difference (`invalid_grant`
versus an API validation message); the object does not, beyond the error text in `status.message`.

**Not verified: the `minio-go` asymmetry has never been exercised during a rate limit.** The
data-plane client retries `429` up to ten times by its own defaults while the control plane refuses
to retry it even once, and the data plane is not gated by the breaker. The reasoning that the
measurement interval floor keeps that volume small is
[ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md)'s, not a measurement.

**`degrade` swallows its own status-update failure.** If the `Status().Update` inside `degrade`
fails, it is logged at V(1) and the function still reports that it took ownership of the failure —
so a bucket can be held without the hold being recorded on the object. Verified by reading the code;
no test covers it.
