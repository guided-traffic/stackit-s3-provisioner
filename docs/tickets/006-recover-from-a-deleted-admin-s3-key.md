# Ticket: recover from an admin S3 key that was deleted out of band

**Status:** Draft — split out of the hot-reload ticket in review on 2026-09-19. Design proposal, open
questions listed. Not approved for implementation.
**Scope:** this repo (`stackit-s3-provisioner`).
**Relates to:** [hot-reload the StackIT service-account key](archive/007-hot-reload-the-stackit-service-account-key.md), which landed as [ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)
— its Q8. Different credential, different cause, different fix: the SA key is rotated *from outside*
and lives in a file; the admin S3 key is minted *by the operator* and only ever breaks when somebody
deletes it in the cloud. A file watch would not help here.
**ADR:** its own, written as part of implementing this ticket.
**Date:** 2026-09-19

## Problem

The operator mints one S3 admin credential at bootstrap — the `operator-admin` credentials group plus
an access key — and uses it for **every data-plane call**: bucket tags, bucket policies, the empty
check before teardown, the wipe, and the usage listings. It is persisted in the operator-owned Secret
and cached in memory for the life of the process
([bucket_controller.go:1434-1483](../../internal/controller/bucket_controller.go#L1434-L1483)).

If that access key is deleted in the StackIT console, every data-plane call starts failing. Verified by
reading the code:

1. `ensureAdmin` returns the cached `r.admin` without ever probing it
   ([bucket_controller.go:1439-1441](../../internal/controller/bucket_controller.go#L1439-L1441)).
2. **A restart does not help.** The admin Secret still exists and is still complete, so `adminFromSecret`
   hands the same dead credential back and it is cached again. The re-bootstrap path only runs when the
   Secret is *missing or incomplete* — a Secret full of a deleted key looks perfectly valid.
3. So the failure persists until a human deletes the admin Secret by hand, which is the one action that
   makes the operator mint a new key.

What breaks while it lasts, all of it fleet-wide because the credential is shared:

| Path | Effect |
|---|---|
| `resolveWorkloadGroup` → `BucketTags` | every reconcile fails; no Bucket converges |
| `ensureBucketPolicy` | isolation policy cannot be written or healed |
| `assertBucketEmpty` in teardown | **every CR deletion blocks** on the data-loss guard |
| `S3Admin.WipeBucket` | `spec.wipeOnDelete` cannot run |
| usage measurement | listings fail (already non-fatal by design) |

The error the operator sees is an S3-level refusal (`InvalidAccessKeyId` /
`SignatureDoesNotMatch`) from minio, not a control-plane error, so none of the existing classification
in [stackit/errors.go](../../stackit/errors.go) applies to it. Today it surfaces as an opaque reconcile
error that repeats forever.

**Derived from reading the code; not reproduced.** The offline reproduction against `stackitfake` is the
first acceptance test.

## Goal and success criteria

| ID | Criterion |
|---|---|
| SC1 | A deleted admin key is recognised as such, not as an anonymous data-plane failure |
| SC2 | The operator recovers on its own by minting a replacement, without a human deleting the admin Secret |
| SC3 | Recovery is visible: an event and a metric, so "the operator silently replaced its admin credential" is never a surprise |
| SC4 | A data-plane failure that is *not* a dead credential (network, 5xx, a bucket-specific 403) does not trigger a re-bootstrap |
| SC5 | Two operator replicas cannot fight over the admin credential |
| SC6 | A changed admin group URN is detected and reported, and a policy resync is attempted across the fleet |

## Design sketch

### D1 — Recognise it

The discriminator is an S3 authentication refusal, which minio surfaces as a typed
`minio.ErrorResponse` with `Code` in `InvalidAccessKeyId` / `SignatureDoesNotMatch`. That is about the
*credential*, unlike `AccessDenied`, which is about the *request* and is exactly what the isolation
policy is supposed to produce for a workload. Conflating the two would let a correctly-denied request
trigger a credential rotation. The classification belongs in `stackit/s3.go` next to the existing
S3 helpers, not in the controller.

### D2 — Re-bootstrap

On that answer: drop `r.admin`, and re-run the bootstrap that `ensureAdmin` already contains — find or
create the `operator-admin` group, clear its keys, mint a fresh one, rewrite the Secret. The code path
exists; what is new is reaching it without a missing Secret.

Note for the ADR: the admin group is looked up **by display name**
([bucket_controller.go:1462](../../internal/controller/bucket_controller.go#L1462), `adminGroupName`),
which is the deliberate exception to [ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)'s
rule that credentials groups are never resolved by name. The exception holds because this group is the
operator's own and belongs to no Bucket — but a re-bootstrap makes that exception load-bearing in a new
place, so it has to be restated rather than assumed.

### D3 — Do not loop

A re-bootstrap that fails, or one whose fresh key is refused again, must not become a key-minting loop
against the provider — every attempt creates a credential. Needs a cooldown, or a single attempt per
reconcile with the normal rate-limited requeue carrying the retry.

### D4 — Concurrency

Only the leader reconciles, so two replicas do not race in the normal case
(`LeaderElection` in [cmd/main.go](../../cmd/main.go)). During a rolling update with
`LeaderElectionReleaseOnCancel` the handover is quick but not atomic; a re-bootstrap that clears the
group's keys while the outgoing pod still holds one is worth thinking through.

## Security considerations

* Re-bootstrapping mints a **new S3 credential with admin scope** in response to an error. That is a
  privileged reaction to an untrusted signal, so the signal must be narrow (D1) and rate-limited (D3).
  Getting D1 wrong — matching `AccessDenied` too — would let any denied data-plane request cause the
  operator to mint admin credentials.
* `DeleteAllAccessKeys` on the admin group is part of the existing bootstrap. It destroys any other key
  of that group, which is correct when the operator owns the group exclusively and dangerous if anyone
  ever shares it. Worth stating as an invariant in the ADR.
* The deletion of an admin key may itself be an incident rather than an accident. Recovering silently
  would erase the signal — hence SC3: recovery is reported, not just performed.

## Test plan

* Offline (`stackitfake`): delete the admin key out of band, reconcile, assert today's behavior (opaque
  repeated failure, restart does not help); then flip the test to assert detection and recovery.
* Offline: `AccessDenied` on a bucket does **not** trigger a re-bootstrap (SC4).
* Offline: teardown of a CR unblocks after recovery.
* Offline: a failing re-bootstrap does not mint a key per reconcile (SC3/D3).
* Live (`-tags integration`): delete the real admin key, observe recovery and the event.

## Open questions

### Q1 — ANSWERED: lazy — react to a real failure, no startup probe
**Decision (2026-09-19): no probe at startup.** The recovery is triggered by an actual data-plane
refusal (D1), nothing else.

A probe would spend one call on **every healthy start** to surface a condition the first Bucket
reconcile surfaces seconds later anyway — and on an operator with no Buckets there is nothing to
protect and no bucket to probe against in the first place (the S3 client is built from a bucket's
endpoint, [bucket_controller.go:648-654](../../internal/controller/bucket_controller.go#L648-L654)).

Consequence for the readiness probe: it stays as it is. A dead admin credential does not make the
operator unready — it makes its Buckets fail, which is where the signal belongs (SC3's event and
metric).

### Q2 — ANSWERED: the Secret's data is replaced, not merged
**Decision (2026-09-19): on a re-bootstrap, `sec.Data` is set fresh instead of merged into.** After a
re-bootstrap every value of the old identity is dead — access key, secret, URN and group id. Merging
overwrites the four fields the operator knows, but silently carries over anything else somebody put
there, and such a field would belong to an identity that no longer exists.

Only the data is replaced; the Secret object itself is kept (labels, ownership, resourceVersion), so
there is no window in which the operator holds a freshly minted cloud key with nowhere to store it —
the failure mode that deleting and re-creating the Secret would introduce.

Note this changes `writeAdminSecret` ([bucket_controller.go:1512-1532](../../internal/controller/bucket_controller.go#L1512-L1532))
only for the re-bootstrap path; the initial bootstrap has nothing to merge with anyway.

### Q3 — ANSWERED: detect the URN change and force a fleet-wide policy resync
**Decision (2026-09-19): compare the freshly bootstrapped group's URN against the one recorded in the
admin Secret; if it differs, force a policy resync across every Bucket.** The admin URN sits in
`NotPrincipal` of statement 1 of every bucket policy and is the lockout guard ([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D5). A
policy naming a principal that no longer exists does not merely point at nothing — it means the
operator's *new* identity is no longer exempt from statement 1, so the bucket denies it.

**The hard part, and it must go into the ADR rather than be discovered later: the resync may not be
executable.** Deriving it from the policy shape and this repo's own invariant 4 ("keep the admin group
in `NotPrincipal`, otherwise lockout — StorageGRID can lock out the account root",
[CLAUDE.md](../../CLAUDE.md)):

* Statement 1 denies every principal not listed. The new admin identity is not listed.
* `PutBucketPolicy` is a data-plane call — it is not in the control-plane SDK at all — so there is no
  second route to rewrite it.
* The workload key is still listed, but statement 2 denies the workload everything outside object
  operations, so it cannot rewrite the policy either.

If that holds, losing the `operator-admin` **group** (as opposed to just its key) leaves every bucket
policy frozen with a dead admin principal and nothing the operator holds can repair it.
**Derived from the documented invariant, not verified live** — and it is the single most important
thing to verify before this ticket is implemented, because it decides whether the recovery is a repair
or only a very loud alarm.

Either way the detection is worth having: an operator that knows its URN changed can say so, instead of
failing with 403 on every bucket for reasons nobody can see.

### Q4 — Should the policy carry a lockout escape hatch at all?
Follows from Q3. If the admin group's URN is the only principal that can rewrite a policy, then losing
that group is unrecoverable by construction. Options worth weighing before the ADR fixes the current
shape:

* Leave it as is and rely on the group never being deleted (today's implicit assumption, now explicit).
* Add a second, stabler principal to `NotPrincipal` — the project's account root, if StorageGRID honours
  it, which invariant 4 suggests it may not.
* Accept it and document bucket re-creation as the recovery.

*No recommendation.* This changes the isolation policy, which is [ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md)
and [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) territory, and it should not be decided as a side effect of a credential-recovery
ticket. Verifying Q3's lockout claim comes first.

## References

* [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) — `ensureAdmin`, `adminFromSecret`, `writeAdminSecret`, `newS3Admin`
* [stackit/s3.go](../../stackit/s3.go) — the data-plane client the admin credential drives
* [stackit/errors.go](../../stackit/errors.go) — the control-plane classification that does *not* cover this
* [docs/adr/0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) — the by-name lookup rule and the admin group's exception
* [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D5 — why the admin URN is in every bucket policy
