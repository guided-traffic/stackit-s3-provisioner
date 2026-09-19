# Ticket: decide bucket existence with `GetBucket`, not a project-wide listing

**Status:** Draft — scope decided in review on 2026-09-19 (split out of the vanished-bucket guard),
design proposal, open questions listed. Not approved for implementation.
**Scope:** this repo (`stackit-s3-provisioner`).
**Relates to:** [ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md)
— the vanished-bucket guard, which landed on 2026-09-19. It introduced `Client.BucketExists` and uses
it for that guard only; this ticket converts the remaining callers. The two are independent: the guard
did not wait for this, and this improves paths that exist today regardless of the guard. That record's
residual risks name the teardown case below as still open.
**Date:** 2026-09-19

## Problem

Every existence question in the operator is answered by listing **all** buckets of the project and
scanning the result:

```go
func (c *Client) HasBucket(ctx context.Context, projectID, name string) (bool, error) {
	names, err := c.ListBucketNames(ctx, projectID)   // ListBuckets(projectId, region)
	...
}
```
([client.go:205-218](../../stackit/client.go#L205-L218))

Three problems, in rising order of severity:

1. **Cost.** One project-wide listing per existence question. `ensureBucket` asks once per Bucket per
   reconcile ([bucket_controller.go:570](../../internal/controller/bucket_controller.go#L570)), read
   grants ask once per grant ([bucket_controller.go:1144](../../internal/controller/bucket_controller.go#L1144)),
   teardown asks once ([bucket_controller.go:1201](../../internal/controller/bucket_controller.go#L1201)),
   and `WaitBucketVisible` polls it every 2s after a create
   ([client.go:222-239](../../stackit/client.go#L222-L239)). On a fleet of *n* Buckets that is *n*
   full listings per resync, against an API that has already rate-limited this operator once
   ([ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md), the 2026-09-02 incident).
2. **Pagination is unverified.** `ListBuckets(projectId, region)` takes no paging parameters at all in
   `objectstorage@v1.9.1`, and `ListBucketNames` reads a single response with no continuation loop.
   Whether a project with many buckets returns all of them is **not verified**. If the API caps the
   page, a bucket that exists reads as absent — silently, and more likely the larger the installation.
3. **A scan cannot answer definitively.** "Not in the list I received" is not "does not exist". The
   whole error classification of this operator rests on the opposite: `apiAnswer` in
   [stackit/errors.go](../../stackit/errors.go) only accepts a *structured* answer from the provider as
   a decision. A listing that came back short, or came back at all through a gateway, produces a
   `false` that is indistinguishable from a real absence.

### Where problem 3 already bites

`teardown` decides via `HasBucket` whether the bucket exists, and gates **both** the empty-check data
loss guard and the bucket deletion on the answer
([bucket_controller.go:1201-1231](../../internal/controller/bucket_controller.go#L1201-L1231)). A
listing that wrongly reports the bucket as absent therefore makes teardown skip the empty check *and*
skip the delete, drop the finalizer, and leave a live bucket — with data in it — orphaned in the
project with no CR pointing at it.

**Not observed, derived from the code.** It requires the listing to be wrong, which is exactly what
problem 2 leaves open.

## Goal and success criteria

| ID | Criterion |
|---|---|
| SC1 | Every existence question in the operator is answered by a per-bucket read whose "no" is the API's own structured 404 |
| SC2 | A failure to reach the provider is never returned as "the bucket does not exist" — it stays an error and takes the degraded path |
| SC3 | Provisioning, read grants and teardown behave as before in the happy path; no bucket is created, adopted or deleted differently |
| SC4 | Request volume per reconcile drops from one project listing per question to one per-bucket read |
| SC5 | Whether `ListBuckets` paginates is answered, and the remaining listing users are correct either way |

## Design

### D1 — One helper, already specified

`Client.BucketExists(ctx, name) (bool, error)` from the guard ticket: `GetBucket`, with **only** a
structured JSON 404 mapped to `false` and every other failure returned as an error. The classification
stays inside the client so no caller can conflate "unreachable" with "absent". If the guard ticket
lands first, this ticket only changes callers.

### D2 — Callers to convert

| Caller | Today | After |
|---|---|---|
| `ensureBucket` ([:570](../../internal/controller/bucket_controller.go#L570)) | `HasBucket` | `BucketExists` |
| read-grant resolution ([:1144](../../internal/controller/bucket_controller.go#L1144)) | `HasBucket` | `BucketExists` |
| `teardown` ([:1201](../../internal/controller/bucket_controller.go#L1201)) | `HasBucket` | `BucketExists` |
| `WaitBucketVisible` ([client.go:222](../../stackit/client.go#L222)) | polls `HasBucket` | polls `BucketExists` — **only after the timing question below is answered** |

### D3 — The one risky conversion: `WaitBucketVisible`

`WaitBucketVisible` runs immediately after `CreateBucket` and exists because the project listing is
eventually consistent. Whether `GetBucket` becomes consistent *earlier*, *later* or *at the same time*
as the listing is **unverified**. Converting it blind could either shorten the wait (fine) or make it
time out on a bucket that was created successfully (a failed provisioning of a bucket that exists —
which `ensureBucket` then re-enters, finds present, and adopts, so it self-heals, but noisily).

Proposal: convert it last, behind a live measurement — create a bucket against the real API and poll
both `GetBucket` and the listing, recording which answers first. Until then it keeps polling the
listing, which is correct if slow.

### D4 — What stays on the listing

* `ListCredentialsGroups` / the group index are a different API and untouched.
* `hack/e2ecleanup` ([main.go:211](../../hack/e2ecleanup/main.go#L211)) legitimately wants *all*
  buckets — it sweeps leftovers. It keeps `ListBucketNames`, and it is the one place where the
  pagination question (SC5) actually changes behavior: a capped listing means the sweeper silently
  misses buckets.

## Security considerations

* No change to what the operator may do, only to how it learns whether a bucket exists. No new
  permission, no new credential, no new data leaving the cluster.
* The teardown path above is the security-relevant one: making the existence answer definitive removes
  a way for a wrong answer to leave a data-bearing bucket orphaned and unmanaged. That is the argument
  for doing this at all.
* The change must not accidentally make a *reachability* failure look like absence — the entire point
  of keeping the discriminator in `BucketExists` (D1). A regression here would be worse than today's
  behavior, because absence now has consequences (the guard).

## Test plan

* Offline (`stackitfake`): each converted caller behaves identically for present/absent buckets; a
  transport error, a 5xx, an empty body and a 404 with an HTML body all surface as errors, never as
  absence.
* Offline: teardown of an existing bucket still runs the empty-check; teardown of an absent one still
  completes.
* Offline: a read grant to a Bucket without a bucket still degrades to `ReadGrantPending`.
* Live (`-tags integration`): the D3 measurement — create a bucket, poll `GetBucket` and the listing
  in parallel, record which becomes consistent first.
* Live: the SC5 answer — create enough buckets in a scratch project to exceed a plausible page size and
  check whether `ListBuckets` returns them all. This is the only way to settle problem 2.

## Open questions

### Q1 — ANSWERED: measure first, convert only if `GetBucket` is not slower
**Decision (2026-09-19): the conversion of `WaitBucketVisible` is gated on a live measurement.** Create
a bucket against the real API and poll `GetBucket` and the project listing in parallel, recording which
becomes consistent first. Convert only if `GetBucket` is not the slower of the two; otherwise it stays
on the listing and the ticket says so explicitly.

Why it is the one conversion that cannot be done on reasoning alone: `WaitBucketVisible` exists
*because* the listing is eventually consistent, and nobody has measured whether the per-bucket read
converges earlier, later or at the same moment. Converting it blind risks a 60s timeout on a bucket
that was created successfully — which `ensureBucket` does heal on the next pass via `adoptOrCollide`,
but loudly, with failure events on a provisioning that actually worked.

The measurement is cheap and belongs in the live test plan above either way.

### Q2 — ANSWERED: `HasBucket` is removed, `ListBucketNames` stays
**Decision (2026-09-19): delete `HasBucket` once its callers are converted.** `ListBucketNames` stays
for `hack/e2ecleanup`, which legitimately wants every bucket in the project.

Two existence notions in one client is how a later caller picks the wrong one — and the wrong one here
is the one whose "no" is not definitive. Keeping it with a warning comment would put the distinction
back where this whole change just took it out of: the caller's discipline.

If the [Q1](#q1--answered-measure-first-convert-only-if-getbucket-is-not-slower) measurement says
`WaitBucketVisible` should keep polling the listing, it polls `ListBucketNames` and does the name
comparison itself — one small local loop, not a shared helper inviting reuse.

### Q3 — ANSWERED: yes, its own ADR with the implementation
**Decision (2026-09-19): every ticket carries a separate ADR, written when that ticket is
implemented.** This one's is narrow: bucket existence is decided by a per-bucket read whose only "no"
is the provider's own structured 404, never by scanning a project listing — with the teardown
consequence of the Problem section as the reason it is a rule and not a refactor.

## References

* [stackit/client.go](../../stackit/client.go) — `HasBucket`, `ListBucketNames`, `WaitBucketVisible`, `BucketConnInfo`
* [stackit/errors.go](../../stackit/errors.go) — `apiAnswer`, the structured-answer discriminator
* [internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) — the three callers
* [hack/e2ecleanup/main.go](../../hack/e2ecleanup/main.go) — the legitimate listing user
* [ADR 0013](../adr/0013-a-provider-outage-is-held-fleet-wide.md) — the rate-limit incident that makes request volume a real concern
