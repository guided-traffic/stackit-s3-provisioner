# ADR 0012: Ready Describes the Last Verified State of a Bucket, Not the Last Verification Attempt

## Status

Accepted. Date: 2026-08-25. Amended 2026-09-02 by [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md).

Implemented: a provisioned `Bucket` holds `Ready=True` through a non-definitive reconcile failure,
records the degradation in `status.degradedSince` and the `ProviderReachable` condition, and drops
to phase `Failed` once `providerDegradedGrace` elapses. This record deliberately left the reconcile
returning its error, so a provider outage still counted once per retry per Bucket in
`controller_runtime_reconcile_errors_total`; that hole was the actual alert driver and is closed by
ADR 0013, which stops the calls instead of the errors. Still open: a provisioned bucket that has
vanished at the provider is not covered by this record at all - the provider answers the existence
question successfully, so nothing is held, the bucket is re-created empty and the `Bucket` keeps
reporting `Ready`; reporting that absence instead of silently re-provisioning it is outstanding work.

## Context

Two provider events on 2026-08-25 produced the same amplifier chain on the production management
cluster mgmt-p (the operator ran there in namespace `storage-provisioner`, at chart version 1.10.19
on that date): a provider blip made every `Bucket` non-`Ready`, every Flux `Kustomization`
health-checking those Buckets went `Ready=False`, and a cluster-wide alert storm followed. That
version predates this record; the hold described below first shipped in 1.12.0, which is the
before/after boundary for anyone re-reading the incident against a deployed release.

| Time (UTC) | What the provider did | What the operator did |
| --- | --- | --- |
| 08:13-08:15, 2026-08-25 | The Object Storage API answered `403` as an nginx HTML error page, not as an API answer | 342 reconcile errors in about two minutes; all 19 Buckets on the cluster went non-`Ready` simultaneously |
| 10:37, 2026-08-25 | One connection died mid-response (`unexpected EOF`) | One reconcile failure; that Bucket stayed non-`Ready` for roughly 10 minutes, until the next successful reconcile |

Downstream, the 08:13 window marked roughly 11 tenant `Kustomization` resources unhealthy and fired
the Flux readiness alert for seven tenant platforms (gitlab, harbor, keycloak, vaultwarden,
guided-ssh, dependency-track, coder) and their configuration Kustomizations. Nothing was wrong with
a single bucket. The buckets were fine; only the control-plane call had failed, and the operator had
turned a two-minute provider blip into a page.

Three things made this not free to fix.

**The failure count was self-inflicted.** A failed read of the project's Object Storage service
status was read as "the service is not enabled", which triggered an attempted enablement write -
once per Bucket, per reconcile. Nineteen Buckets retrying against an edge that answered HTML is how
two minutes became 342 errors.

**A status code does not tell a provider answer from a gateway page.** Both arrive as the same error
shape carrying the same status code; only the body separates them, and the structured body of a real
refusal decodes into a near-empty shape, so the decoded form is no discriminator either. Measured
live on 2026-08-25 against region `eu01`: an enabled project answers `200`, a project without Object
Storage answers a structured JSON `404`, and a project id the key cannot resolve answers a structured
JSON `403`. Both `403` observations were taken against project ids that do not exist, and the
counter-check against two real projects outside the key's reach answered `200`; the reading that the
API separates "not entitled" from "entitled, service not enabled" is therefore an interpretation of
those numbers and not something the measurement isolated. What the measurement does establish, and
what D2 rests on, is that each of these answers arrives with a structured JSON body. The 08:13 `403`
did not: it was an intermediary in front of the API, with an HTML body.

**A revoked service-account key does not look like an API refusal at all.** Measured live on
2026-08-25 against `https://accounts.stackit.cloud/oauth/v2/token`: a revoked or absent key answers
`400` with `{"error":"invalid_grant"}` (RFC 6749 section 5.2). The key flow never reaches the Object
Storage API, and the SDK stamps the token endpoint's status and body into the same error shape as an
API error. A first draft of the definitive set matched only `401` and `403`; it would have kept the
entire fleet green for the full grace window after a key was revoked - the one degradation that must
never be masked.

The forcing question is therefore not "which errors are transient" but "what does `Ready` claim".
Answering "the last reconcile attempt succeeded" makes every Bucket a live probe of the provider's
edge, which is what the incident exploited. Answering "this is the state the operator last verified"
makes the readiness of a provisioned Bucket a property of the bucket, and makes reachability a
separate, separately observable thing.

## Decision

**D1 - Readiness on a provisioned Bucket describes the last verified state.** `Ready` reports what
the operator last confirmed about the bucket, its credentials and its policy - not the outcome of
the last attempt to confirm it. A failure to reach the provider, the S3 data plane or the Kubernetes
API is a failure to verify and does not by itself change what the Bucket claims.

**D2 - Errors are classified by origin, never by their text or their status code.** A fault the
operator established locally about *this* Bucket is definitive and drops `Ready` at once. A failure
that arose while talking to another system is non-definitive and is held. Where the provider's own
structured answer must be told apart from an intermediary's error page, the discriminator is the
shape of the body - a structured answer against anything else - and never the status code alone.

**D3 - An unrecognised error is non-definitive by construction.** The definitive set is
enumerated in this record and grows only by amending it; everything not enumerated is held. The
default is reached by not being enumerated, so a new provider failure mode cannot land on the wrong
side of the line by being unknown.

**D4 - A held state is reported separately, and the existing signals are unchanged.** While a Bucket
is held it carries `status.degradedSince` (the moment the run of failures began, written once and
not per retry) and the condition `ProviderReachable=False` with reason `ProviderUnreachable` and the
error as its message. `status.message` and the `Warning` event with reason `Failed` are emitted as
they were before the hold existed, so the reason a Bucket is held is always on the object. Fleet
visibility is `stackit_s3_provisioner_buckets_provider_degraded` and the per-Bucket
`stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds`, both of which describe a hold that
is actually in effect and stop when it is given up.

**D5 - The hold is bounded by one operator-wide grace, after which the Bucket fails as before.** The
window is `--provider-degraded-grace` (`PROVIDER_DEGRADED_GRACE`, Helm `providerDegradedGrace`,
`30m` # default). Once it elapses the Bucket drops to phase `Failed` exactly as it did before this
record, keeping `status.degradedSince` so the status still says when the trouble started. `0`
disables the hold entirely and restores the previous behaviour without a different image. The grace
is not per-Bucket: no Bucket can ask for a longer or a shorter hold than the operator's.

**D6 - The exceptions are the cases enumerated below, and each one drops `Ready` immediately
whatever the grace says.**

| Case | Why it is definitive |
| --- | --- |
| A structured `400`, `401` or `403` from the provider | The provider's own refusal, not a failure to reach it. `401`/`403` is the Object Storage API refusing an authenticated request; `400` is how a revoked service-account key surfaces at the token endpoint. An error page carrying any of those codes has a non-structured body and *is* held. |
| A workload credential the operator itself destroyed | The previous access key was deleted and its replacement could not be published. The operator knows the credential in the Secret is dead: local certainty, not an unverifiable provider state. |
| A configuration fault | A Secret key collision, a `spec.secretRef` aimed at the operator's own admin Secret, a `spec.region` other than the operator's, a composed bucket name outside the permitted range, an ownership collision on an existing bucket, an unusable clone source. Each is a statement about this Bucket that the operator made on its own. |
| A Bucket being deleted | Holding `Ready` through a teardown would hide a deletion blocked by the non-empty guard of [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md). |
| A Bucket whose spec has not been observed (`status.observedGeneration` differs from `metadata.generation`) | The user asked for something new and it was not achieved; there is no verified state matching the current spec. |
| A Bucket that has never been `Ready` | There is nothing verified to defend. Failures during initial provisioning surface at once. |

## Consequences

While the operator cannot reach the provider, a Bucket keeps asserting a state nobody is checking:
a policy edited behind the operator's back, a credential revoked at the provider, a bucket filling
up - all of it still reads as `Ready` until something definitive arrives or the grace elapses. This
is the price of D1 and it is paid deliberately: a delayed signal is bounded by D5 and recoverable,
while marking the whole fleet unhealthy on the first blip is neither. It also means a health check
that gates a deployment on a Bucket can pass while the operator is blind to that bucket; consumers
that want "the last attempt succeeded" must read `ProviderReachable` or the degraded metrics, not
`Ready`.

A bucket deleted behind the operator's back is *not* an instance of that cost, and it is worth
saying so because the two are easily confused. There the provider answers, and answers that the
bucket is absent; the reconcile succeeds, the bucket is provisioned again from scratch and nothing is
held, so no rule of this record applies to it. That gap belongs to the provisioning step, not to the
readiness semantics - see Residual risks.

`status.degradedSince` survives into phase `Failed` so the record of when the trouble started is not
lost, but the two degraded metrics deliberately stop at that moment, because the hold has been given
up. Any alert on the age of a hold must therefore fire before the grace expires, or the series it
watches disappears before it ever triggers - which is why the shipped `StackitS3BucketProviderDegraded`
threshold must stay below `providerDegradedGrace`.

This record left the reconcile returning its error, so that retries, `Warning` events and
`controller_runtime_reconcile_errors_total` behaved exactly as before. That was meant as a
conservative non-change and turned out to be the real alert driver: the error rate still counted a
single outage once per retry per Bucket. ADR 0013 revisits it by removing the calls during a
fleet-wide outage rather than by suppressing the errors.

Finally, the definitive set contains `400`, which also covers a genuinely malformed request rejected
by the API. Such a Bucket drops `Ready` at once rather than being held. That is the intended
direction - a request the provider will never accept is not going to fix itself - but it means a
provider-side change in request validation surfaces as a hard failure, not as a degradation.

## Alternatives Considered

**A taxonomy of transient error patterns.** Rejected. This was the original proposal: classify
`unexpected EOF`, connection refused/reset, timeouts, HTTP 5xx and the SDK's "undefined response
type" case as transient, and treat everything else as definitive. It is the cheaper option - it
needs no reasoning about origin and can be written as a list of strings and status codes - and it
lost anyway, because the list is provider-controlled and open-ended. It can never be complete, and
every error not on it lands on the *wrong* default: a new phrasing from the provider's edge would
have reproduced the 2026-08-25 incident exactly. The list of definitive cases in D6 is the same idea
inverted: it is closed, it is about the operator's own certainty, and it belongs to us.

**Keeping the drop and fixing the consumers.** Rejected. Relaxing the Flux health checks, or their
timeouts, would have quietened the alert storm without touching the operator. It treats the symptom
in every consumer instead of the cause in one producer, and it requires every future consumer of a
`Bucket` to know that readiness here means something weaker than readiness elsewhere. The operator
is the amplifier; the fix belongs in the amplifier.

**A per-Bucket grace in the spec.** Rejected. The grace expresses how long the *operator* is willing
to vouch for a state it cannot verify, which is a property of the operator's relationship with the
provider, not of any one bucket. Making it per-Bucket would let a tenant extend its own blind window
and would put a security-relevant timer in a namespace-writable field.

**Reporting the hold by setting `ProviderReachable=True` while healthy.** Rejected. The condition is
removed rather than set to `True` on recovery, so a Bucket that never degraded and one that
recovered look identical, and an operator upgrade writes nothing to Buckets that are simply healthy.

## Residual risks

The asymmetry in D3 is a judgement, not a measurement: holding a state wrongly costs a delayed
signal bounded by the grace, dropping one wrongly marks the entire fleet sick on the first blip. If
a future failure mode makes a wrongly-held `Ready` expensive - a bucket that is gone and being
written to - the judgement has to be revisited rather than patched around.

A vanished provisioned bucket would be the concrete instance of that, and today it is not reached by
this record at all. Provisioning decides existence from the provider's own answer about the project's
buckets, and a successful answer that the name is absent is indistinguishable from a bucket that was
never created: the operator creates it again, empty, stamps its ownership tags, attributes a new
credentials group to it and publishes a fresh access key into the workload Secret. The reconcile
succeeds, so `status.degradedSince` is never written, `ProviderReachable` is never set and `Ready`
never moves - the data is gone and nothing on the object says so. Reporting the absence instead of
re-provisioning over it is outstanding work, and it has to be built in the provisioning step; no
change to the rules above would catch it.

**Not verified.** The behaviour was verified offline against a simulated provider only: the hold, the
expiry of the grace, the immediate drop on a structured refusal and the teardown exception all have
offline reconciler coverage, but no test exercises this path against a real cluster or a real
provider outage. The live blocking test proposed with the original work - cutting egress to the
provider for five minutes and confirming that no Bucket leaves `Ready` and no Flux `Kustomization`
goes unhealthy - has not been run.

**Not verified:** that `30m` is the right window. It was chosen to outlast the observed blips (about
two minutes on 2026-08-25, about twenty minutes on 2026-09-02) while leaving time to act before the
drop, not derived from a measured distribution of provider outage lengths.

**Not verified:** what the structured `403` actually means. It was measured with a *valid* key
against project ids that do not exist, and the counter-check with two real projects outside the key's
reach answered `200`, so "this key may not use this project" is a reading of the result rather than
the case that was observed. It is in any event not the error picture of a revoked key, which was
measured separately at the token endpoint. Nothing in D2 or D6 depends on the reading - both turn on
the body being structured and the status being `400`, `401` or `403` - but the entitlement semantics
should not be quoted from here as measured fact. Both measurements are from 2026-08-25 and neither
has been repeated since.

## References

* [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) - amends this record: a fleet-wide outage
  stops the provider calls entirely, which is what finally made the error rate usable for alerting.
* [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) - the data-loss guard whose blocked
  deletions must stay visible, which is why a teardown is on the D6 list.
* [ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) - the
  credential lifecycle that produces the destroyed-credential exception in D6.
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) - the region guard, one of the
  configuration faults D6 treats as definitive.
* [docs/operations/provider-outages.md](../operations/provider-outages.md) - the on-call view: what a
  held Bucket looks like, what is not held, and the settings that bound it.
* [docs/operations/bucket-status.md](../operations/bucket-status.md) - how to read a `Bucket`'s
  phases, conditions and status fields.
* [docs/operations/monitoring.md](../operations/monitoring.md) - the degraded metrics and the alert
  that fires on the age of a hold.
* [docs/developer/provider-errors.md](../developer/provider-errors.md) - how a provider error is
  recognised, the measured API semantics behind D2, and the retry rules.
* [docs/developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md) - the provisioning
  step that decides a bucket's existence, which is where the re-creation gap above lives.
