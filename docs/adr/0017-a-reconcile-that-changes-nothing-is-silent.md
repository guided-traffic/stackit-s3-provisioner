# ADR 0017: A Reconcile That Changes Nothing Is Silent, and Says So Once After a Start

## Status

Accepted. Date: 2026-09-20.

Implemented: a successful pass over a `Bucket` that changed nothing at the provider or in the
cluster logs one line at verbosity 1 and raises no event; a pass that did change something logs at
`Info`, naming what it did, and raises the `Provisioned` event carrying the same list; the first
successful pass a process completes for a `Bucket` is logged at `Info` even when it changed nothing;
and `status.lastVerifiedTime` advances on every successful pass. Not verified against a live
cluster: the behaviour was verified offline against the simulated provider only.

## Context

Every provisioned `Bucket` is re-reconciled on the drift resync interval
([ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8, `10m` `# default`). Until
this record, every one of those passes ended the same way regardless of what it had found: an
`Info` line `bucket provisioned` and a `Normal` event with reason `Provisioned`, the event echoed
into the log again by the framework at verbosity 1. On a fleet of *n* buckets that is 2*n* log
lines and *n* event writes every ten minutes, all of them saying that nothing happened.

The noise had a cost beyond volume. The log could not answer the one question a resync exists to
answer — *did this pass repair anything?* — because a pass that re-asserted a manually edited
policy and a pass that found everything in order produced the same line. The policy write on the
drift path was in fact not logged at all, so the very repair ADR 0003 D8 promises was invisible.
And the `Provisioned` event, aggregated by the API server into one record with a rising count, had
quietly become the only per-object trace that the operator had looked at a bucket recently: the
`Ready` condition's transition time does not move on a verification, and nothing else in status did
either. Removing the event without a replacement would have removed that trace.

At the same time a process start is different from a resync. After a restart or a leader change the
operator knows nothing until its first pass over each `Bucket`, and an operator log that shows the
fleet coming into a known-good state once, at `Info`, is what somebody watching a rollout reads.

## Decision

**D1 — A successful pass that changed nothing is silent at the default verbosity.** It logs one line
at verbosity 1 (`bucket verified`) and raises no event. Its status write still happens.

**D2 — A successful pass that changed something is reported at `Info`, and the report names the
changes.** The line `bucket provisioned` carries a `changes` list, and the `Provisioned` event's
message carries the same list. A change is any of the following, and the list is amended when a
new kind of write is added to the pass:

| Change | What was written |
|---|---|
| `spec generation N observed` | A new `metadata.generation` was reconciled for the first time |
| `bucket created` | The bucket did not exist and was created — including the authorised re-creation of [ADR 0015](0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D9 |
| `credentials group attributed` | The bucket tags naming the workload credentials group were written: a fresh group, the policy migration of [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md), or a URN backfill |
| `isolation policy written` | The live policy differed from the desired document and was rewritten ([ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8) |
| `workload credentials issued` | An access key was minted and the Secret written — first provisioning, a lost Secret, or a requested rotation ([ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)) |
| `clone completed` | The copy finished in this pass ([ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md)) |

A status write on its own is not a change: every successful pass writes status.

**D3 — The first successful pass a process completes for a `Bucket` is reported at `Info` even when
it changed nothing.** The line is `bucket verified after operator start`; it raises no event, because
the object's history has not changed — the process's knowledge has. Every process starts with that
knowledge empty, so a restart and a leader change both show the state of the fleet exactly once. A
`Bucket` that is deleted and re-created counts as new.

**D4 — `status.lastVerifiedTime` advances on every successful pass, changed or not.** It is the
per-object proof that the resync is running, and it replaces what the per-pass event used to show.
It is also a wide column (`VERIFIED`) of `kubectl get`.

**D5 — The fleet-wide proof that the resync is running is the framework's own counter,
`controller_runtime_reconcile_total{controller="bucket",result=~"success|requeue_after"}`.** A
successful pass that schedules the next resync is counted under `requeue_after`, not `success`;
only with the resync switched off (`driftResyncInterval: "0"`) does a successful pass count as
`success`. Nothing in the operator's own metric set is added for it.

## Consequences

The log at the default level now reads as a change log: a line per bucket at start, then a line
only when the operator did something, with the something named. An unchanged fleet is quiet.

Every successful pass now writes a status that differs from the previous one, because the timestamp
moves. Before this record an unchanged pass wrote an identical status, which the API server does
not persist. The write is one per bucket per resync interval; the `Bucket` watches of both
controllers filter on generation and annotations, so the write wakes nothing — but any external
watcher of `Bucket` objects sees an update per bucket per interval.

`kubectl describe` of a healthy, untouched `Bucket` shows no events once the last real one has aged
out. That is the Kubernetes convention — an event is something that happened — and it is a change
for anyone who read the rising count of the old `Provisioned` event as a heartbeat. The heartbeat is
`status.lastVerifiedTime` now.

The operator's own verbosity-1 lines are one line richer, and `logging.level: debug` shows the
unchanged passes again, one line each.

## Alternatives Considered

**Demote the log line and keep the per-pass event.** Halves the log noise and changes no event
semantics. Rejected: the event was the larger of the two costs — an API write per bucket per
interval — and it would have left the operator's events inconsistent with themselves, every other
`Normal` event being raised once per occurrence ([ADR 0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
even states that for rotations).

**Infer "nothing changed" from status alone** — the generation already observed, the phase already
`Ready`. Cheaper, no signal to thread through the pass. Rejected because it is wrong for exactly the
passes that matter: a policy re-asserted after an upgrade and a Secret re-issued after a loss change
nothing in status, and those are the passes that must be reported.

**Report the first pass after a start as a change, with the event.** Rejected: the bucket did not
change, and an event on every `Bucket` at every restart is the noise this record removes, moved
to a different moment.

**Replace the event's heartbeat with a log line only.** Rejected: a log line is visible only to
whoever is watching the right pod at the right level; `status.lastVerifiedTime` is on the object,
readable by anyone who can read the `Bucket`, and survives a pod restart.

## Residual risks

- A `Bucket` whose every pass fails never advances `status.lastVerifiedTime`, by design; the
  failure is on the object as before ([ADR 0012](0012-ready-describes-the-last-verified-state.md)).
  What the timestamp cannot distinguish is a pass that never ran from a pass that ran and failed —
  the `Ready` condition and the events carry that.
- A `Bucket` provisioned before this record shows no `lastVerifiedTime` until its first pass under
  the new version, which the drift resync delivers within one interval of the rollout.
- The change list is maintained by hand at each write site. A new kind of write added to the pass
  without a note is reported as unchanged. Nothing enforces the list; it holds by review.
- **Not verified:** the behaviour against a live cluster and the real provider. The offline suite
  covers the three reporting outcomes, an unchanged resync, a policy drift and a lost Secret.

## References

- [ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D8 — the drift resync and the
  policy re-assertion this record makes visible
- [ADR 0012](0012-ready-describes-the-last-verified-state.md) — what a successful pass verifies
- [docs/operations/configuration.md](../operations/configuration.md) — verifying that the resync runs
- [docs/operations/bucket-status.md](../operations/bucket-status.md) — the field, the column and the event
- [docs/developer/reconcile-pipeline.md](../developer/reconcile-pipeline.md) — where the changes are noted
