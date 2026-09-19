# Ticket: hot-reload the StackIT service-account key

**Status:** Draft — design decided, waiting on its precondition. Not approved for implementation.
Q1, Q2, Q3 and Q12 answered in review on 2026-09-19. Q12 replaced the project-binding idea with a
vanished-bucket guard that is proposed as its own ticket and precondition. Q13–Q17 answered in a
second review on 2026-09-19; no question is open. Sequencing confirmed there: the vanished-bucket
guard lands first ([Q14](#q14--answered-the-vanished-bucket-guard-lands-first)).
**Scope:** this repo (`stackit-s3-provisioner`).
**ADR:** its own, written as part of implementing this ticket (see
[Q11](#q11--answered-its-own-adr-written-with-the-implementation)) — not signed off in advance.
**Date:** 2026-09-19

## Problem

The operator reads its StackIT service-account (SA) key exactly once, at process start, and holds
the parsed RSA key in memory for the lifetime of the process. Changing the key on disk — a Secret
update, a rotation, an external secret manager pushing a new value — has no effect until the pod
restarts.

Verified in this tree on 2026-09-19:

| # | Finding | Evidence |
|---|---|---|
| 1 | Key path is consumed once in `main()` | [cmd/main.go:263-268](../../cmd/main.go#L263-L268) — `stackit.LoadAccount` then `stackit.NewClient`, both outside any loop |
| 2 | `LoadAccount` reads the file only for `projectId` | [stackit/client.go:36-61](../../stackit/client.go#L36-L61) |
| 3 | The SDK snapshots the key material at client construction | core v0.26.0 `auth/auth.go:377` (`os.ReadFile` into `cfg.ServiceAccountKey`), `auth/auth.go:176-231` (`KeyAuth` unmarshals, then `KeyFlow.Init`), `clients/key_flow.go:134-170` + `validate()` (PEM parsed into `c.privateKey`) |
| 4 | Token refresh signs with the in-memory key; the file is never re-read | `clients/key_flow.go` `GetAccessToken` → `recreateAccessToken` → `createAccessToken` → `generateSelfSignedJWT` |
| 5 | No file watching anywhere in the operator | `grep -rn fsnotify --include='*.go' .` → 0 hits; `fsnotify` is only an indirect dependency ([go.mod](../../go.mod)) |
| 6 | The Helm mount itself is fine | [deployment.yaml:113-127](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml#L113-L127) — whole-Secret volume, no `subPath`, so kubelet **does** refresh the file inside the container. The staleness is purely in the process. |
| 7 | `helm upgrade` with a changed key Secret does not roll the pods | no `checksum/...` pod annotation in the Deployment template |
| 8 | The admin S3 credentials are cached in-process too | [bucket_controller.go:1435-1441](../../internal/controller/bucket_controller.go#L1435-L1441) — `r.admin` is memoized after the first read of the operator Secret |

### What this costs today

A revoked key surfaces as `400 invalid_grant` from the token endpoint, which
[`stackit.ProviderRefused`](../../stackit/errors.go#L60) classifies as a **definitive** refusal.
`holdsReadyThrough` therefore refuses to hold Ready ([bucket_controller.go:1646](../../internal/controller/bucket_controller.go#L1646)):
every Bucket in the cluster drops to `Failed` at once, with no degraded grace. After
`--provider-circuit-threshold` (default 3) consecutive failures the circuit opens and the operator
probes with up to a 5-minute cooldown.

None of that heals, whatever the file on disk says, until somebody runs `kubectl rollout restart`.

One piece of good news for the design: once a *valid* key is loaded, the fleet recovers on its own.
Failed reconciles requeue through [`bucketRateLimiter`](../../internal/controller/bucket_controller.go#L1798)
(1s → 15min cap) and the drift resync runs every 10 minutes, so no extra re-enqueue machinery is
strictly required — see [Q5](#q5--answered-the-existing-requeue-mechanics-are-the-recovery-path).

## Goal and success criteria

| ID | Criterion |
|---|---|
| SC1 | A changed key file takes effect without a restart, within a bounded and documented window |
| SC2 | An invalid, empty, truncated or foreign key never displaces a working one; a rejected reload is a no-op |
| SC3 | No in-flight reconcile ever observes a half-swapped client |
| SC4 | After a successful reload the fleet recovers without operator action, within one `--drift-resync-interval` (default 10m) |
| SC5 | Reload attempts, their outcome, and the age of the loaded key are visible in metrics and logs — and no key material appears in logs, events, status or metric labels |
| SC6 | Skeleton mode (no key configured) behaves exactly as today, and stays fixed at startup |
| SC7 | Offline tests cover accept / reject / swap atomicity; one live rotation is verified against the real API |
| SC8 | A Bucket the operator has already provisioned is never re-created implicitly — if it is gone, the CR goes stale, says so, and alarms ([Q12](#q12--answered-nothing-is-remembered-a-vanished-bucket-is-reported-not-re-created)) |

## Options

| Option | What it is | Cost | Availability impact |
|---|---|---|---|
| **A — in-process swap** (recommended) | Watch the file, validate a candidate key, atomically replace the client inside `stackit.Client` | ~200 LOC + tests, new concurrency in a hot path | none; reconciles keep running |
| **B — validated self-restart** | Watch the file, validate a candidate key, then cancel the manager context and exit 0 so the kubelet restarts the container | ~50 LOC | seconds of downtime per rotation; leader lease is released on cancel already (`LeaderElectionReleaseOnCancel: true`, [cmd/main.go](../../cmd/main.go)) |
| ~~**C — Helm `checksum/secret` annotation**~~ (dropped, [Q10](#q10--answered-no-checksum-annotation)) | Pod annotation over the key Secret so `helm upgrade` rolls the Deployment | 2 lines | one rolling update; **not** hot reload, and does nothing when the Secret is managed outside the chart (ESO, SOPS, manual) |

B is strictly simpler and is provably consistent — every process-scoped cache (`r.admin`,
`serviceReady`, breaker state, the current token) is rebuilt by construction. A is what the request
asks for and is the better answer for a fleet that is otherwise healthy. C was dropped in review
([Q10](#q10--answered-no-checksum-annotation)).

Recommendation: implement **A**, with the vanished-bucket guard of [Q12](#q12--answered-nothing-is-remembered-a-vanished-bucket-is-reported-not-re-created) as its precondition.

## Design (Option A)

### A1 — Detection: content poll every 30s

**Decided in review on 2026-09-19: a content poll, not fsnotify.** A ticker reads the file and compares
a SHA-256 against the hash of the currently loaded key; a differing hash starts the validation of A2.
Interval behind `--sa-key-reload-interval` / `SA_KEY_RELOAD_INTERVAL`, default `30s`, `0` disables the
feature and restores today's behavior exactly (the same values-only rollback `--provider-circuit-threshold`
offers).

Why polling wins here: kubelet itself needs up to ~60-90s to refresh a Secret volume inside the
container, so a 30s poll adds a small fraction to a latency that is dominated by something else
entirely — and reading one small file every 30s costs nothing.

What it avoids is a real footgun. Kubernetes Secret volumes are updated by writing a new timestamped
directory and atomically renaming the `..data` symlink, so an fsnotify watch registered on the *file*
path follows the old inode and stops firing after the first swap; the correct form watches the
*directory* for `Create`/`Rename` of `..data`. That mistake is invisible in any test that simply
rewrites the file in place, which is how it survives into production.

Compare the file *hash*, never mtime: an atomic directory swap can leave mtime unhelpful, and a hash
also makes "changed back to the same content" a no-op for free.

### A2 — Validate before swapping

This is the crux of the feature. A hot reload that accepts an unvalidated key is a self-inflicted
outage primitive: it lets a truncated write or a wrong-project Secret take down a healthy operator
in seconds instead of at the next restart, when somebody is watching.

Order, all of it before anything is swapped:

1. Read the file **once** and keep the bytes. Empty or unreadable → reject. The bytes are what gets
   hashed (A1) and what the SDK receives: core v0.26.0 offers `config.WithServiceAccountKey(content)`
   next to `WithServiceAccountKeyPath`, so the candidate client is built from the same bytes that
   were validated, and no second read of a file that may be mid-rewrite happens between the hash and
   the client construction. Verified in the module cache on 2026-09-19.
2. `stackit.LoadAccount` → reject if it does not parse or carries no `projectId`.
3. **Project sanity check:** reject if `projectId` differs from the one the running process started
   with ([Q1](#q1--answered-project-binding)). One string comparison, no state, no configuration — it
   catches an accidental in-place swap for free. It is explicitly **not** the protection against a
   foreign key: that is the vanished-bucket guard of
   [Q12](#q12--answered-nothing-is-remembered-a-vanished-bucket-is-reported-not-re-created), which
   needs no identity plumbing and survives restarts because it asks a different question.
4. `stackit.NewClient` → this already fails fast on malformed key material: `KeyAuth` unmarshals the
   JSON and `KeyFlow.Init` → `validate()` parses the RSA PEM eagerly (verified above). Free validation.
5. **Live probe** with the candidate client: one cheap authenticated read (`GetServiceStatus` via a
   small probe helper). This is the only step that proves the key can actually mint a token — steps
   1-4 all pass for a perfectly well-formed key that the provider has revoked.

Only after (5) does the swap happen. Every rejection increments a counter and logs once per distinct
file hash (not once per tick). The probe in (5) does **not** consult the circuit breaker's `Allow()`
([Q15](#q15--answered-the-validation-probe-bypasses-the-circuit-breaker)), and a rejected candidate
is retried on the schedule of [Q16](#q16--answered-a-rejected-candidate-is-retried-with-a-backoff).

### A3 — The swap

`stackit.Client` gains a single `atomic.Pointer[clientState]` where
`clientState{api *objectstorage.APIClient; account Account; http *http.Client}`. Every method loads
the pointer once on entry, so a call that started against the old key finishes against the old key
and a failed one simply requeues. `region` stays immutable (it is a flag, not part of the key).

Two details worth writing down:

* The retired `*http.Client` keeps idle keep-alive connections for `idleConnTimeout` (30s,
  [stackit/retry.go](../../stackit/retry.go)). Calling `CloseIdleConnections()` on the retired state
  is tidy and costs one line — hence `http` in `clientState`.
* `config.BackgroundTokenRefreshContext` is **not** set by this operator, so `KeyFlow.Init` starts no
  background goroutine (`clients/key_flow.go:160`). If that ever changes, every swap would leak a
  refresher goroutine. Keep it unset, or cancel it on retire.

`NewClientWithEndpoint` (the stackitfake path, [stackit/client.go:109](../../stackit/client.go#L109))
must keep working unchanged.

### A4 — Caches on reload

| Cache | Action | Reason |
|---|---|---|
| SDK token | dropped with the old `KeyFlow` | nothing to do |
| `Client.serviceReady` | keep | project-scoped, and the project may not change ([Q1](#q1--answered-project-binding)) |
| `BucketReconciler.admin` | keep | the admin S3 key belongs to a credentials group, which is a project resource, not a derivative of the SA — see [Q3](#q3--answered-credentials-groups-and-their-access-keys-are-independent-of-the-sa-that-created-them) |
| `ProviderBreaker` | `Success()` on a successful swap ([Q4](#q4--answered-a-successful-reload-resets-the-breaker)) | the validation already made a successful authenticated call — the exact evidence `Success()` waits for |

### A5 — Wiring

A `manager.Runnable` added with `mgr.Add(...)` that reports `NeedLeaderElection() false`: a standby
replica must already hold a fresh client at the moment it takes the lease, not start reloading then.

### A6 — Observability

* `stackit_s3_provisioner_sa_key_reload_total{result="applied|rejected|unchanged"}` (counter)
* `stackit_s3_provisioner_sa_key_loaded_timestamp_seconds` (gauge) — when the currently active key
  was loaded; makes "this pod is running a key from before the rotation" a query
* `stackit_s3_provisioner_sa_key_reload_failing` (gauge 0/1) — a rejected reload must be loud, or it
  looks exactly like a healthy operator right up to the moment the old key expires
* `stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds` (gauge, absolute Unix time) — the
  `validUntil` of the loaded key, exported only when the file carries the field; re-set on every
  swap ([Q17](#q17--answered-the-keys-validuntil-is-exported-as-a-timestamp-gauge))
* `PrometheusRule` entries: a rejection that persists; the key expiring within 14 days (warning)
  and within 3 days (critical), both computed as `<gauge> - time()` in the rule, never as a
  countdown exported by the operator

Log the `projectId` and the SA issuer on a successful reload. Never the file content.

### A7 — Documentation touched in the same change

[README.md](../../README.md) (the new flag and the chart value, in the reference table that is their
only home), [docs/operations/configuration.md](../operations/configuration.md) (what the setting does
and that it is the first thing here that is hot-reloaded),
[docs/operations/credentials.md](../operations/credentials.md) (the rotation procedure an operator
follows), [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md) (what a
write to the key Secret now does, per the security considerations below),
[docs/developer/stackit-api.md](../developer/stackit-api.md) (the SDK finding that the key is
snapshotted at client construction) and the Helm values. The decision itself goes into this ticket's
own ADR, written with the implementation.

## Security considerations

* **This widens what a write to the operator's key Secret does.** Today such a write needs a restart
  to take effect; afterwards it takes effect by itself within the poll window. That is a real change
  in blast radius, but the restart requirement was never a security control — anyone who can write
  that Secret can already make the operator authenticate as whatever they put there, and can trigger
  the restart by deleting the pod.
* **The in-process project check (A2.3) is a guardrail, not a boundary, and the ADR must say so.** It
  compares against the projectId the *running process* loaded at startup, so it holds for one process
  lifetime and a pod restart wipes the reference point. It is kept because it is free, not because it
  protects anything. What actually protects the data is the vanished-bucket guard
  ([Q12](#q12--answered-nothing-is-remembered-a-vanished-bucket-is-reported-not-re-created)): it never
  asks which project the operator is pointed at, only whether the bucket it provisioned is still
  there — a question that needs no remembered identity and therefore survives every restart.
* **A rejected reload must be as visible as a failure.** Silent rejection is the dangerous mode: the
  operator keeps working on the old key, the admin believes the rotation landed, and the outage
  arrives whenever the old key is finally deleted.
* **No key material in logs, events, `status`, or metric labels** — including in error strings
  propagated from the SDK.
* **Probe cost.** Each reload attempt spends one API call. A flapping file must not become a request
  loop: skip unchanged hashes, and back off on repeated rejection of the same content.
* The key file's permissions inside the pod are unchanged by this work (Secret volume default). Noted
  only so that no implementation copies the key somewhere else to make watching easier.

## Test plan

* **Offline unit** (`stackit/`): swap atomicity under concurrent readers; reject empty / malformed /
  foreign-project candidates; unchanged hash is a no-op; probe failure leaves the old client in place.
  The probe runs against `stackitfake`.
* **Offline controller**: reconciles continue across a swap; skeleton mode untouched.
* **envtest**: the runnable starts without leader election and does not disturb the manager.
* **Live** (`make e2e-stackit` or a dedicated `-tags integration` test): rotate a key of the working
  project and watch the fleet recover without a restart. Scheduled as the **last** implementation step
  ([Q2](#q2--answered-test-material)); the same run confirms [Q3](#q3--answered-credentials-groups-and-their-access-keys-are-independent-of-the-sa-that-created-them)
  and settles SC7.

## Open questions

These need answers before implementation starts; several change the design, not just the code.

### Q1 — ANSWERED: project binding
**Decision (2026-09-19): reject a candidate key whose `projectId` differs from the expected one.**
Re-binding orphans every bucket ownership tag and every credentials group the operator manages, and
does it silently.

**But the check as designed is weak, and the reason is worth writing down.** It compares against the
projectId the running process loaded at startup. A pod restart erases that reference point: the new
process has no memory of the previous project and simply adopts whatever the file says. So the rule
holds *within* a process lifetime and evaporates at every restart — which is precisely the path an
operator is most likely to take after a botched Secret update.

What the restart path actually does today, with a foreign key in place:

1. `ensureAdmin` finds the existing admin Secret and reuses the old admin S3 credentials — which
   belong to a credentials group in the **old** project.
2. `ensureBucket` asks `HasBucket(ctx, ProjectID(), name)` against the **new** project → `false` →
   `CreateBucket`. Whether that succeeds depends on whether bucket names are per-project or
   per-region, which is open question Q4 in [CLAUDE.md](../../CLAUDE.md) and still unanswered.
3. If it succeeds, the operator has created an empty bucket in a foreign project, tagged it as its
   own, and will then fail on the data plane, because the old admin key has no rights there.

Reachable **today**, without any of this work. Hot reload does not create the gap; it makes the
in-process check look like a guarantee it is not. The answer is not a better identity check but a
different question entirely — see [Q12](#q12--answered-nothing-is-remembered-a-vanished-bucket-is-reported-not-re-created),
which also shows that step 2 is worse than "stuck": it re-creates the bucket and overwrites the
workload Secret.

### Q2 — ANSWERED: test material
**Decision (2026-09-19): rotate a key of the working project towards the end of the ticket.** The
live rotation is therefore the last implementation step, not a prerequisite, and it settles two things
in one run: SC7, and the assumption in
[Q3](#q3--answered-credentials-groups-and-their-access-keys-are-independent-of-the-sa-that-created-them)
— a fleet that survives the rotation *is* the confirmation.
`account-1.json` / `account-2.json` stay what they are: two *different* projects, exercising
cross-project isolation, not rotation.

### Q3 — ANSWERED: credentials groups and their access keys are independent of the SA that created them
**Resolved (2026-09-19) by design argument.** Consider the opposite: deleting or rotating an
administrator's credential would cascade-delete everything that administrator ever created. No cloud
IAM model works that way — it would make credential rotation, the one operation every security
baseline mandates, the most destructive operation available. Credentials groups and access keys are
project resources; the service account is only the caller that created them.

Residual: this is reasoning, not a live observation, so the ticket keeps it marked as an assumption
until the [Q2](#q2--answered-test-material) rotation confirms it — which that run does for free, since
a surviving fleet after the rotation *is* the confirmation. It does not block the design.

### Q4 — ANSWERED: a successful reload resets the breaker
**Decision (2026-09-19): call `Breaker.Success()` after a validated swap.** The validation step already
includes a successful authenticated API call (A2.5), so the reload does not assume the provider is back
— it has just observed it. Without the reset, a fleet whose circuit was tripped by a revoked key would
sit out up to `--provider-circuit-max-cooldown` (5 min) after the fix, despite the operator holding
fresh proof that the API answers.

Accepted cost: the breaker gains a second writer outside the reconcile loop. `Success()` is already the
idempotent "a call worked" signal ([breaker.go:172](../../internal/controller/breaker.go#L172)), so the
reload uses the existing contract rather than reaching into breaker state.

### Q5 — ANSWERED: the existing requeue mechanics are the recovery path
**Decision (2026-09-19): no fleet-wide re-enqueue.** After a successful swap the operator does nothing
beyond resetting the breaker ([Q4](#q4--answered-a-successful-reload-resets-the-breaker)). Failed
Buckets keep retrying through `bucketRateLimiter` (1s → 15min cap) and the drift resync ticks
independently every `--drift-resync-interval` (default 10m), so the worst-case lag is bounded by the
resync, not by the rate-limiter cap.

Rejected: a `source.Channel` that enqueues every Bucket on reload. It buys minutes at the price of new
wiring in `SetupWithManager` and a thundering herd against an API that has just come back — the exact
pattern the circuit breaker and the workqueue rate limiter were built to prevent ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md)).

SC4 is therefore satisfied by existing machinery, and the documented worst case is one drift-resync
interval.

### Q8 — ANSWERED: own ticket, different problem
**Decision (2026-09-19): out of scope here.**
[recover from an admin S3 key that was deleted out of band](006-recover-from-a-deleted-admin-s3-key.md)
carries it. The two look alike and are not: the SA key is rotated from outside and lives in a file, so
watching the file is the fix; the admin S3 key is minted by the operator and only breaks when somebody
deletes it in the cloud, which no file watch can see.

Verified while splitting it out, and worse than assumed: a **restart does not recover** from a deleted
admin key. `ensureAdmin` re-reads the Secret, finds it complete, and caches the same dead credential —
the re-bootstrap path only runs for a *missing or incomplete* Secret. Recovery today requires a human
to delete the admin Secret.

### Q9 — ANSWERED: the service-account key and nothing else
**Decision (2026-09-19): exactly one runtime-mutable input.** The ADR states it positively — the
service-account key is re-read at runtime; every other input (region, ownership name, bucket prefix,
feature gates, intervals, grace and circuit settings) takes effect at process start and changing it is
a deployment rollout.

The line is not arbitrary: the key is the only input that changes **without anyone touching the
deployment configuration**, because an external rotation mechanism writes it. Everything else is a flag
or a Helm value, and changing one already rolls the pod.

Worth restating in the ADR even though it follows: `--ownership-name` is part of the bucket ownership
key ([cmd/main.go](../../cmd/main.go) documents the warning), so reloading it at runtime would make the
operator treat its own buckets as foreign. It is not merely "not reloadable" — it is the example of why
the contract is narrow.

### Q10 — ANSWERED: no checksum annotation
**Decision (2026-09-19): option C is dropped.** With the operator polling the file itself
([A1](#a1--detection-content-poll-every-30s)) the annotation solves a problem that no longer exists —
and it only ever worked for chart-managed Secrets, which is precisely not where most installations keep
their key (ESO, SOPS, manual). Two mechanisms for one purpose, one of which fires only sometimes.

Consequence: the option table above keeps C for the record, but it is not part of the implementation.

### Q11 — ANSWERED: its own ADR, written with the implementation
**Decision (2026-09-19): each ticket carries a separate ADR, produced when that ticket is
implemented.** For this one: the reload contract, the project sanity check from
[Q1](#q1--answered-project-binding) stated explicitly as a guardrail rather than a boundary, the
validate-before-swap rule, and the rejection semantics. The vanished-bucket rule belongs to its own
ticket and its own ADR.

### Q12 — ANSWERED: nothing is remembered; a vanished bucket is reported, not re-created
**Decision (2026-09-19): no persisted project binding.** The operator is stateless with respect to its
own configuration, and no admin is going to be asked to copy the `projectId` into a second field just
so the operator can check it against the first. Both options sketched here earlier — a data key in the
admin Secret, an explicit Helm value — are rejected on that ground, and they were only accident guards
anyway.

**What replaces it is a different question, asked per CR.** The operator does not need to know which
project it is pointed at. It needs to notice that a bucket it once provisioned is not there any more,
and then stop rather than repair:

> A Bucket whose `status.resolvedBucketName` is set has been fully provisioned at least once. If the
> provider then answers that this bucket does not exist, the operator must **not** create it. The CR
> goes stale — `Ready=False` with a distinct reason, an event, and a dedicated metric to alarm on —
> and keeps re-checking on the normal rate-limited requeue, so it recovers by itself the moment the
> bucket is reachable again.

This needs no remembered identity, no new configuration, and no new state: the evidence is a status
field the operator already writes. It also survives restarts for free, because the CR does.

`status.resolvedBucketName` is the right predicate and the annotation is not:
[bucket_controller.go:375](../../internal/controller/bucket_controller.go#L375) writes the status field
only on the success path, together with `credentialsGroupID` and `accessKeyID`, while
`persistResolvedName` ([bucket_controller.go:1951-1960](../../internal/controller/bucket_controller.go#L1951-L1960))
stamps the `stackit-bucket.gtrfc.com/resolved-bucket-name` annotation *before* any cloud resource
exists. Only the status field proves a completed provisioning round.

#### What this also fixes, and it is not small

The same code path is reachable today without any key rotation: delete a bucket out of band, in the
StackIT console. Reading the flow (**derived from the code, not yet reproduced** — an offline
reproduction against `stackitfake` is the first acceptance test):

1. `ensureBucket` → `HasBucket` false → `CreateBucket` ([bucket_controller.go:569-585](../../internal/controller/bucket_controller.go#L569-L585)),
   and the new bucket is stamped with this operator's ownership tags. `freshBucket` is now `true`.
2. `resolveWorkloadGroup` finds no group tag and no policy on the fresh bucket, so it falls through to
   creating one — and `guardGroupCreate` returns `nil` immediately when `freshBucket` is set
   ([bucket_controller.go:914-918](../../internal/controller/bucket_controller.go#L914-L918)), so the
   ADR 0002 D8 guard ("do not create a second group while the group in status still exists") does not
   apply here.
3. The new group has no keys, so `ensureAccessKeyAndSecret` mints one and **overwrites the workload
   Secret**.
4. Result: an empty bucket under the old name, fresh credentials, the previous credentials group
   orphaned, `Ready=True`, and no signal anywhere that the data is gone.

Silent data-loss invisibility, in the one place the operator is supposed to be conservative. The guard
above turns it into a stale CR and an alarm. That is the stronger argument for the guard — the foreign
key after a restart is then just a second failure mode that the same single check already covers.

#### Residual, to be written into the ADR rather than fixed

* A **new** Bucket CR created while a foreign key is active has no history to contradict, so it is
  provisioned in whatever project the key names. Nothing can distinguish that from a deliberate move.
  It will still fail before writing a workload Secret, because `ensureAdmin` reuses the admin
  credentials of the old project and the data-plane call is refused — leaving an empty untagged bucket
  behind. Loud, cheap to clean up, no data at risk.
* A CR restored from backup with its status wiped looks like a first provisioning and will create.
  That is the same case `decideBucketName` already documents, and it doubles as the escape hatch when
  a re-create really is wanted — see the sub-question below.

#### Resolution

Confirmed in review on 2026-09-19: a bucket deleted by a third party puts the CR into a failure state
so the incident is visible, and an explicit annotation overrides that to authorize a re-creation.

Carried out of this ticket into its own work, which **landed on 2026-09-19** as
[ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md):
the guard, the standing `spec.allowRecreate` opt-in and delete-and-re-apply as the way to re-create
once. It was a **precondition** of hot reload, not a dependent of it: it stood on the
out-of-band-deletion bug alone, and it is what lets this ticket's ADR describe the project check
honestly as a guardrail rather than as the thing that makes a foreign key safe.

### Q13 — ANSWERED: ADR 0005 D8 is struck through, the reload contract gets its own record
**Decision (2026-09-19): approved.** ADR 0005 D8 ("the binding is fixed for the lifetime of the
process ... replacing the key ... takes effect on the next restart") contradicts this ticket for the
credential and stays true for project and region. D8 is struck through in place with a pointer to the
new record; the Residual-risks paragraph "replacing the key requires restarting the operator" and the
index row are trued up in the same change. D1 (the project comes from the key) and D2 (region at
install time) are untouched. The new record carries the whole reload contract: the key as the single
runtime-mutable input ([Q9](#q9--answered-the-service-account-key-and-nothing-else)), validate-before-
swap, the rejection and retry semantics, the project check as a guardrail, the metrics, and under
Consequences the widened effect of a write to the key Secret (Security considerations above).
Numbering is assigned when written; the vanished-bucket record precedes it
([Q14](#q14--answered-the-vanished-bucket-guard-lands-first)).

Rejected: amending D8 in place inside ADR 0005 with no separate record. The reload contract is more
than one rule and has alternatives of its own (fsnotify, self-restart, checksum annotation), and
[Q11](#q11--answered-its-own-adr-written-with-the-implementation) had already decided on a separate
record.

### Q14 — ANSWERED: the vanished-bucket guard lands first
**Decision (2026-09-19): confirmed — the vanished-bucket guard is implemented before this ticket.**
It has since landed, as [ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md).
Put to the user again because the dependency is one of
documentation honesty, not of code: the restart-with-a-foreign-key path exists today and hot reload
does not widen it (the in-process project check rejects a foreign key where today's restart adopts
it). The alternative — this ticket first, with the gap named under Residual risks and closed when the
guard lands — was offered and declined. The rotation feature therefore waits for the guard.

### Q15 — ANSWERED: the validation probe bypasses the circuit breaker
**Decision (2026-09-19): the probe of A2.5 does not consult `Allow()`, and ADR 0013 D4 is amended
to say so.** D4 reads "while the breaker is open the operator makes no provider call at all"; it gains
the exception "except the single validation call of a candidate service-account key", approved by
the user in this review. Reason: the one scenario where it matters is an old key revoked before the
new one arrived — the circuit is then open, tripped by `400 invalid_grant` (verified: a structured
refusal runs through `fail()` and therefore `Breaker.Failure()`,
[bucket_controller.go:1559](../../internal/controller/bucket_controller.go#L1559)). Obeying `Allow()`
would delay the reload by up to `--provider-circuit-max-cooldown` while reconcile probes with the dead
key keep doubling the cooldown. The probe is one call per distinct file hash on the schedule of
[Q16](#q16--answered-a-rejected-candidate-is-retried-with-a-backoff), which cannot drive a provider
blip into a rate limit — the thing the breaker exists to prevent. Its success calls `Success()`
([Q4](#q4--answered-a-successful-reload-resets-the-breaker)); its failure never calls `Failure()`.

Rejected: obeying `Allow()`. No record change, but up to five minutes of delay in the scenario hot
reload is meant to heal, plus a third reload result ("held") that says nothing about the key.

### Q16 — ANSWERED: a rejected candidate is retried with a backoff
**Decision (2026-09-19): two rejection classes, two schedules; a candidate is never given up on
while its hash differs from the loaded key.**

| Rejection | Cases | Retry |
|---|---|---|
| Definitive | empty or unparsable file, foreign `projectId`, SDK rejects the PEM, structured `400`/`401`/`403` on the probe (`stackit.ProviderRefused`) | doubling interval starting at one tick (30s, 60s, 2m, 4m, ...), capped at 10min, reset when the file hash changes |
| Non-definitive | 5xx, HTML gateway page, transport error — anything `apiAnswer` does not accept as an answer | every tick, no backoff: the provider is not answering and the candidate is innocent |

The case that decides the policy: a freshly issued key the token endpoint does not know yet answers
with a *definitive* `400` for a while. **Not verified** that STACKIT has such a propagation delay; the
policy has to survive it either way. Worst-case cost of the chosen policy with a foreign key left in
place permanently: one token request every 10 minutes, in a state that already alarms via
`stackit_s3_provisioner_sa_key_reload_failing`.

Rejected: holding a definitively rejected hash until it changes (zero calls, but a delayed-activation
key is rejected until somebody touches the file, and the alert then fires for a case the operator
could have healed); retrying every tick regardless (the rate-limit exposure of ADR 0013 for no gain).

### Q17 — ANSWERED: the key's `validUntil` is exported as a timestamp gauge
**Decision (2026-09-19): in scope.** `LoadAccount` reads `validUntil` (parsed by the SDK today,
read by nothing — [docs/developer/stackit-api.md](../developer/stackit-api.md)) and the operator
exports it as `stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds`, an absolute Unix time,
present only when the file carries the field, re-set on every swap. Two `PrometheusRule` entries
compute the remaining lifetime in the rule — `<gauge> - time() < 14 * 86400` (warning),
`< 3 * 86400` (critical). Reason: the reload answers "does the new key arrive"; the gauge answers
"does it arrive in time", which is the question a 90-day rotation actually asks. The
`_timestamp_seconds` shape follows the three existing timestamp gauges in the operator; a countdown
exported by the operator was rejected because it changes on every scrape, makes the pod's clock a
fault source, and looks healthy from a hung exporter.

**Verified on 2026-09-19:** the production key in the `awe-d` cluster (Secret
`storage-provisioner/stackit-sa-key`) carries `validUntil`, set by the provider to exactly 90 days
after `createdAt` — the rotation period is encoded in the key itself, so the gauge measures the
process rule directly. The e2e key (`account-1.json`) carries **no** `validUntil`; the absent-gauge
path is therefore what the offline and e2e tests exercise, and it must be a tested path, not a
fallback. A `createdAt`-based gauge was considered as a substitute and dropped once the production
key was checked.

## References

* [cmd/main.go](../../cmd/main.go) — startup wiring, skeleton mode, metrics registration
* [stackit/client.go](../../stackit/client.go) — `LoadAccount`, `NewClient`, `serviceReady`
* [stackit/retry.go](../../stackit/retry.go) — the transport that must be carried across a swap
* [stackit/errors.go](../../stackit/errors.go) — `ProviderRefused`, the classification that makes a revoked key fatal
* [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) — `ensureAdmin`, `holdsReadyThrough`, `bucketRateLimiter`, `SetupWithManager`
* [internal/controller/breaker.go](../../internal/controller/breaker.go) — `Allow` / `Failure` / `Success`
* [deploy/helm/stackit-s3-provisioner/templates/deployment.yaml](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml) — the key mount
* [docs/adr/README.md](../adr/README.md) — ADR format and the obligation
