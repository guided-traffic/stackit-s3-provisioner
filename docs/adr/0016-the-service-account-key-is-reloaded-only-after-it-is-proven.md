# ADR 0016: The Service-Account Key Is Re-Read at Runtime and Swapped Only After It Is Proven

## Status

Accepted. Date: 2026-09-19. Amends [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) D8
and [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) D4.

Implemented: the operator polls its service-account key file on
`--stackit-sa-key-reload-interval` / `STACKIT_SERVICE_ACCOUNT_KEY_RELOAD_INTERVAL` (`30s` `# default`, `0` disables), compares a
content hash against the key it is running on, validates a differing candidate up to and including
one authenticated call, and only then swaps the authenticated client atomically. A rejected
candidate never displaces the running key, is counted, alarmed and retried. ADR 0005 D8 said the
credential was fixed for the lifetime of the process; that half of it is struck through in place and
replaced by this record. The project and the region remain fixed at start.

Open: the live rotation of a real key against the real API is the last step of the work and is
**not yet done** — every case in this record is exercised offline against the in-memory fake. Whether
STACKIT's token endpoint has a propagation delay for a freshly issued key is **not verified**; D7 is
written so that the answer does not change the outcome.

## Context

The operator authenticates to STACKIT with a service-account key: a JSON file carrying an RSA private
key, mounted from a Kubernetes Secret. The key flow signs an assertion with that private key and
exchanges it for an access token, over and over, for as long as the process runs.

The key material is read exactly once, at process start. Everything after that — every token refresh,
every control-plane call for days — signs with the RSA key that was parsed into memory at second
zero. The file on disk is never looked at again. That is not an oversight in this operator; it is how
the SDK's key flow is built, and it is the sane default for a credential that nobody rotates.

Somebody does rotate this one. The provider stamps `validUntil` into the key itself, ninety days
after it was created, so rotation is not a possibility to be planned for but a scheduled event with a
deadline printed on the credential. And the rotation happens *outside* the deployment: an external
secret manager, a `kubectl apply`, an operator in a console pushes a new value into the Secret. The
kubelet propagates it into the container — the mount is a whole-Secret volume with no `subPath`, so
the file inside the pod really does change. Nothing else does.

What happens next is the reason this record exists. A revoked key does not fail softly. The token
endpoint answers `400 invalid_grant`, which is a *structured* refusal, which the operator classifies
as definitive — correctly, because repeating the request cannot change it. Definitive faults are not
held through the degraded grace of
[ADR 0012](0012-ready-describes-the-last-verified-state.md), so every `Bucket` in the cluster drops
to `Failed` in the same minute, and after three consecutive failures the fleet-wide breaker of
[ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) opens on top. The fix is already sitting on
the container's filesystem. The operator cannot see it. Somebody has to notice the alert and run
`kubectl rollout restart`.

So the feature is not "reload configuration". It is closing the gap between the moment the
replacement credential arrives and the moment the process can use it.

The danger is symmetrical and worth stating before the decision, because it is what shapes the
decision. A hot reload that accepts a candidate without proving it is an outage primitive: a
truncated write, an empty file, a key for the wrong project or a key the provider has already revoked
would take down a *healthy* operator within the poll window — instantly, unattended, at three in the
morning, instead of at the next restart when somebody is watching. The restart requirement is a bad
availability property and an accidental safety net, and removing it without replacing the net is a
net loss.

## Decision

**D1 — The service-account key is the only input of this operator that is re-read at runtime.**
Every other input — the region, the ownership name, the bucket name prefix and the
namespace-inclusion setting, the feature gates, every interval, the degraded grace and the circuit
settings — is resolved at process start, and changing one is a deployment rollout. The line is not
arbitrary: the key is the only input that changes *without anybody touching the deployment
configuration*, because an external rotation mechanism writes it. `--ownership-name` is the example
that shows why the contract must stay narrow rather than grow: it is part of the bucket ownership
key, so re-reading it at runtime would make the operator treat its own buckets as foreign.

**D2 — Change is detected by polling the file and comparing a content hash.** Not by watching the
inode, and not by comparing modification times.

The interval is `--stackit-sa-key-reload-interval` / `STACKIT_SERVICE_ACCOUNT_KEY_RELOAD_INTERVAL`, `30s` `# default`. Setting it
to `0` disables the feature entirely and restores the behaviour of D8 as it stood before this record
— a values-only rollback that needs no new image, the same escape hatch
`--provider-circuit-threshold` offers.

Polling wins on both halves of the trade. It costs nothing against the latency that actually
dominates: the kubelet itself needs up to about ninety seconds to refresh a Secret volume inside a
container, so the poll adds a small fraction to a wait that is somebody else's. And it avoids a
failure that is invisible in testing. A Kubernetes Secret volume is updated by writing a new
timestamped directory and atomically re-pointing the `..data` symlink at it, so a file-path watch
follows the old inode and stops firing after the first rotation — while a test that rewrites the file
in place passes. A content hash rather than an mtime for the same reason, plus one benefit for free:
a file changed back to the content already loaded is a no-op rather than a reload.

**D3 — Nothing is swapped before the candidate is proven, and the proof ends with one authenticated
call.** In order, all of it before anything is replaced: the file is read **once** and those bytes are
what gets hashed and what the SDK receives, so nothing is re-read between the check and the use; the
JSON must parse and carry a `projectId`; the `projectId` must match D4; the SDK must accept the key
material, which parses the RSA PEM eagerly and therefore rejects a truncated or malformed key for
free; and finally one cheap authenticated read is made **with the candidate client**.

The last step is the only one that cannot be skipped. Every step before it passes for a perfectly
well-formed key that the provider has revoked, which is precisely the key an operator in a hurry is
most likely to mount. A candidate that fails any step is a no-op: the running key stays exactly where
it is.

**D4 — The `projectId` check is a guardrail for one process lifetime, not a boundary, and this record
says so rather than implying otherwise.** A candidate whose `projectId` differs from the one the
running process started with is rejected, because re-binding a live operator to a different project
orphans every bucket ownership tag and every credentials group it manages, and does it silently.

But the reference point is the project the *running process* loaded at start, so a pod restart erases
it: the new process has no memory of the previous project and adopts whatever the file says — which
is exactly the path somebody takes after a botched Secret update. The check is kept because it is one
string comparison with no state and no configuration, not because it protects anything. What protects
the data is [ADR 0015](0015-a-provisioned-bucket-is-never-re-created-implicitly.md), which never asks
which project the operator is pointed at, only whether the bucket it provisioned is still there — a
question that needs no remembered identity and therefore survives every restart. Persisting the
project id, in the admin Secret or as a Helm value, was rejected: see the alternatives.

**D5 — The swap is atomic, and no call in flight ever observes a half-swapped client.** The
authenticated state — the API client, the account it was built from and the HTTP client underneath —
is replaced as one value. A call that started against the old key finishes against the old key; if
that fails because the old key has just been revoked, it requeues and the next attempt uses the new
one. The retired HTTP client's idle keep-alive connections are closed rather than left to time out.

The region is not part of the swapped state. It is an install-time setting
([ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) D2), not a property of the key.

**D6 — A successful reload resets the fleet-wide circuit breaker, and the validation call of D3
ignores it.** Both halves amend [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) D4, which
otherwise holds: while the breaker is open the operator makes no provider call at all, *except* the
single validation call of a candidate service-account key.

The exception is not a convenience. The scenario that matters is the old key being revoked before the
new one arrives: the breaker is then open, tripped by the `400 invalid_grant` the dead key produces,
and obeying it would delay the repair by up to `--provider-circuit-max-cooldown` while reconciles
keep probing with the dead key and doubling the cooldown. One call per distinct file hash, on the
schedule of D7, cannot drive a provider blip into the rate limit the breaker exists to prevent.

The reset is the same argument read forwards. The validation has just made a successful authenticated
call, which is the exact evidence the breaker waits for, so resetting it assumes nothing. A failed
validation never counts as a breaker failure: the candidate is on trial, the provider is not.

**D7 — A rejected candidate is retried, and is never given up on while its hash differs from the
loaded key.** Two classes, two schedules:

| Rejection | Cases | Retry |
|---|---|---|
| Definitive | empty or unparsable file, foreign `projectId`, the SDK rejecting the key material, a structured `400`/`401`/`403` on the validation call | doubling from one interval (`30s`, `60s`, `2m`, `4m`, …), capped at `10m`, reset when the file hash changes |
| Not definitive | a `5xx`, a gateway or WAF page, a transport error — anything that is not the provider's own answer | every interval, no backoff: the provider is not answering and the candidate is innocent |

Two properties of the schedule are part of the rule, not of its implementation, because getting
either wrong quietly breaks the promise above. **The backoff throttles the validation call and never
the read**: the file is examined on every poll, so a corrected key is picked up at the next one
rather than at the next scheduled attempt, which at the cap would be ten minutes after somebody
already fixed it. And **the schedule belongs to the content that earned it**: a poll that did not
reach the provider learned nothing about the candidate, so it retries at once and leaves the accrued
wait alone — otherwise a single transient failure restarts the doubling and the cap bounds nothing.

The case that decides the policy is a freshly issued key the token endpoint does not know yet, which
answers with a *definitive* `400` until it propagates. Whether STACKIT has such a delay is **not
verified**; the policy has to survive it either way, and it does — the key activates by itself within
ten minutes at worst. Holding a definitively rejected hash until the file changes was rejected for
exactly that case: it costs zero calls and leaves a key that would have worked sitting rejected until
somebody touches the file.

The worst case of the chosen policy, a foreign key left in place forever, is one token request every
ten minutes in a state that is already alarming.

**D8 — A rejected reload is as visible as a failure, because silent rejection is the dangerous
mode.** The operator keeps working on the old key, the admin believes the rotation landed, and the
outage arrives whenever the old key is finally deleted. Four series carry it:

| Metric | Type | What it answers |
|---|---|---|
| `stackit_s3_provisioner_sa_key_reload_total{result="applied"\|"rejected"\|"unchanged"}` | counter | did anything happen, and what |
| `stackit_s3_provisioner_sa_key_loaded_timestamp_seconds` | gauge | is this pod running a key from before the rotation |
| `stackit_s3_provisioner_sa_key_reload_failing` | gauge `0`/`1` | is a candidate being rejected right now |
| `stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds` | gauge | when does the loaded key expire (D10) |

A successful reload logs the `projectId` and the service-account issuer. A rejection logs once per
distinct file hash, not once per poll. **No key material appears in a log, an event, a `status` or a
metric label** — including inside an error string propagated from the SDK.

**D9 — Nothing is re-enqueued after a swap.** The existing machinery is the recovery path: failed
`Bucket`s retry on the workqueue rate limiter, and the drift resync ticks every
`--drift-resync-interval` (`10m` `# default`) regardless, so the worst case is bounded by the resync.
A fleet-wide re-enqueue on reload was rejected — it buys minutes at the price of a thundering herd
against an API that has just come back, which is the pattern
[ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) exists to prevent.

**D10 — The key's own expiry is exported as an absolute timestamp, and the remaining lifetime is
computed in the alerting rule.** `stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds` carries
the `validUntil` the provider stamps into the key, present only when the file carries the field and
re-set on every swap. The chart ships two rules that compute `<gauge> - time()`: a warning at 14 days
and a critical at 3 days.

The reload answers "does the new key arrive". This answers "does it arrive in time", which is the
question a ninety-day rotation actually asks. A countdown exported by the operator was rejected: it
changes on every scrape, it makes the pod's clock a fault source, and a hung exporter reports a
healthy-looking constant. The absent-gauge path is a supported path, not a fallback — a key file
without `validUntil` is a real shape and is what the offline and end-to-end suites run on.

**D11 — A reload drops exactly the caches that belong to the key, and keeps the rest.** The access
token goes with the retired key flow, which is the whole point. The verified "Object Storage is
enabled" answer is kept: it is project-scoped and D4 forbids the project from changing. The bootstrap
S3 admin credential is kept: a credentials group and its access keys are *project* resources, and the
service account is only the caller that created them — no IAM model cascade-deletes an
administrator's creations when that administrator's own credential is rotated, because that would
make rotation the most destructive operation available. That last one is reasoning rather than a live
observation and is **not verified** until the live rotation confirms it; a fleet that survives the
rotation is the confirmation.

**D12 — Skeleton mode is unchanged and stays fixed at start.** An operator with no key configured
does not poll, does not export any of the series of D8, and cannot acquire a key without a restart.
[ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) D6 and D7 are untouched: no key
means skeleton mode, and a key that cannot be read *at start* is still a startup failure rather than
a degraded run.

**D13 — The reload runs on every replica, not only on the leader.** A standby must already hold a
fresh client at the moment it takes the lease, not start reloading then. The poll costs one file read
per interval on a replica that is doing nothing else, and a validation call only when the file has
actually changed.

## Consequences

**A write to the operator's key Secret is now a live operation.** Before this record it needed a
restart to take effect; now it takes effect by itself within the poll window. That is a real change
in blast radius and it is stated plainly rather than argued away — although the restart requirement
was never a security control: anybody who can write that Secret can already make the operator
authenticate as whatever they put there, and can trigger the restart by deleting the pod. What
changes is the time to effect, from "next restart" to "within the poll window", and that the change
no longer coincides with a restart somebody would have noticed.

**The operator now makes a control-plane call for a reason unrelated to any `Bucket`.** One per
distinct candidate, at most one per interval, and — by D6 — one that the breaker does not gate. It is
a new kind of traffic in a system that otherwise only talks to the provider on behalf of a CR.

**The fleet heals without human action, and that is the point.** A revoked key that today produces a
cluster of `Failed` Buckets until somebody runs `kubectl rollout restart` now recovers within one
drift resync of the replacement landing on disk.

**A fleet that recovers by itself is a fleet nobody looks at.** The metrics of D8 and the expiry
alerts of D10 are what keeps an unattended rotation from becoming an unnoticed one, and they are not
optional extras: without them the difference between "the rotation landed" and "the rotation is being
rejected and the old key still works" is invisible until the old key is deleted.

## Alternatives Considered

**Watch the file with fsnotify instead of polling.** The obvious implementation, and it loses on the
Kubernetes-specific detail in D2: a watch on the file path follows the old inode through the `..data`
symlink swap and silently stops firing, and the correct form — watching the *directory* for
`Create`/`Rename` of `..data` — is a thing a reviewer has to know to ask for. The bug survives every
test that rewrites the file in place. Polling one small file every thirty seconds costs nothing
measurable and cannot be got wrong in a way that is invisible.

**Validate the candidate and then restart the process deliberately.** Strictly simpler: cancel the
manager context and exit `0`, let the kubelet restart the container, and let every process-scoped
cache be rebuilt by construction — the admin credential, the service-ready flag, the breaker state,
the token. The leader lease is already released on cancel, so failover is fast. It loses because it
spends seconds of downtime on every rotation of a fleet that is otherwise perfectly healthy, and
because it makes a correct rotation indistinguishable from a crash in every dashboard that counts
restarts. It stays the honest fallback if the in-process swap ever proves unstable.

**A `checksum/secret` pod annotation in the chart, so `helm upgrade` rolls the Deployment.** Two
lines, and dropped. It is not hot reload — it is a rolling restart on a chart upgrade — and it only
ever fires for a Secret the chart itself manages, which is precisely not where most installations keep
their key: an external secret operator, SOPS or a manual `kubectl apply` writes it, and none of those
runs `helm upgrade`. Shipping it alongside the poll would mean two mechanisms for one purpose, one of
which works sometimes.

**Obey the circuit breaker on the validation call.** Consistent with
[ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) as it stood, and rejected in D6: it adds up
to five minutes of delay in the one scenario the whole feature exists to heal, and it needs a third
reload outcome — "held" — that says nothing about the key.

**Persist the project binding, so the `projectId` check survives a restart.** Offered as a data key
in the operator's admin Secret and as an explicit Helm value, and rejected in D4 on the same ground
in both shapes: no admin is going to copy the `projectId` out of the key file into a second field so
that the operator can check it against the first, and both were only accident guards. The
restart-proof protection is [ADR 0015](0015-a-provisioned-bucket-is-never-re-created-implicitly.md),
which asks a different question.

**Re-enqueue every `Bucket` on a successful reload.** Rejected in D9.

**Export the key's remaining lifetime as a countdown.** Rejected in D10.

**Reload more than the key.** Every other setting was considered for the same treatment and declined
in D1. The `--ownership-name` case is the one that settles it: reloading it would make the operator
treat its own buckets as foreign, so the narrow contract is not a limitation to be lifted later but
the decision itself.

## Residual risks

**The `projectId` check evaporates at every restart**, as D4 states. A pod restarted with a foreign
key adopts it. The consequence is bounded — not prevented — by
[ADR 0015](0015-a-provisioned-bucket-is-never-re-created-implicitly.md): every provisioned `Bucket`
reports its bucket as missing and nothing is re-created or overwritten. A **new** `Bucket` created
while a foreign key is active has no history to contradict and is provisioned in whatever project the
key names; it still fails before a workload Secret is written, because the bootstrap admin credential
belongs to the old project, and leaves an empty bucket behind.

**The reload widens what a write to the key Secret does**, as the Consequences say. Nothing in this
record narrows who may write it; that is RBAC's job and it is unchanged.

**A flapping key file costs one validation call per distinct content.** The unchanged-hash
short-circuit and the backoff of D7 bound it, but a mechanism that rewrites the file with genuinely
new content in a loop would be matched call for call. Nothing observed does that.

**Not verified: whether the token endpoint has a propagation delay for a freshly issued key.** D7 is
written so the answer does not change the outcome, but the ten-minute worst case is a policy choice
made against an unmeasured behaviour.

**Not verified: that a credentials group and its access keys outlive the service-account key that
created them.** D11 rests on a design argument, not an observation. The live rotation confirms it for
free — a fleet that keeps working after the rotation *is* the confirmation — and until that run has
happened it is an assumption.

**Not verified: the whole feature against the real API.** Every case here is reproduced offline
against the in-memory fake. Rotating a real key of a real project and watching the fleet recover
without a restart is the last step of the work and has not been done.

**The reload cannot help a credential the operator minted itself.** A bootstrap S3 admin key deleted
at the provider is not in a file and no file watch can see it; that failure and its manual recovery
are [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md)'s, unchanged by this
record.

## References

* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — the binding this record amends:
  the project and the region stay fixed at start, the credential no longer does
* [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) — the breaker, whose D4 this record amends
  for the validation call, and whose reset a proven key earns
* [ADR 0015](0015-a-provisioned-bucket-is-never-re-created-implicitly.md) — the guard that makes
  running with a foreign key safe, and therefore what lets D4 be honest about its own weakness
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — why a revoked key drops the whole
  fleet at once instead of being held
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the admin credential a
  reload deliberately keeps
* [docs/operations/configuration.md](../operations/configuration.md) — the setting and what it does
* [docs/operations/credentials.md](../operations/credentials.md) — the rotation procedure an operator
  follows
* [docs/operations/monitoring.md](../operations/monitoring.md) — the four series and the expiry alerts
* [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md) — what a write to
  the key Secret now does
* [docs/developer/service-account-key-reload.md](../developer/service-account-key-reload.md) — how the
  poll, the validation and the swap are built
* [docs/developer/stackit-api.md](../developer/stackit-api.md) — the SDK behaviour this record works
  around
