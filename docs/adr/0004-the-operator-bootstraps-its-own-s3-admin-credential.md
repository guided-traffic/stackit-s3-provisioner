# ADR 0004: The Operator Bootstraps Its Own S3 Admin Credential, One Per Project

## Status

Accepted. Date: 2026-09-19. The decision itself dates from the feasibility study of 2026-06-30 and
has been in the tree since the reconciler was first implemented; this record was written later and
carries no new rule.

Implemented: the bootstrap, the operator-owned Secret and its four data keys, the exemption of the
admin group in every bucket policy, the guard that refuses a Bucket pointing at the admin Secret,
and the in-process cache. Open: nothing detects that the admin credential has been destroyed in the
cloud, so recovery is a manual two-step act (D9), and no event or metric marks a bootstrap.

## Context

Two planes carry this product. The control plane — buckets, credentials groups, access keys,
enabling the object-storage service — is reached with the project's service-account key, which the
operator is given at startup. The data plane is plain S3, reached with an access key and a secret.

The isolation the product exists to provide is a bucket policy (ADR 0003), and setting a bucket
policy is a data-plane call. It is not merely absent from the vendor's control-plane SDK; it is not
a control-plane operation at all. The service-account key the operator is configured with therefore
cannot write a single policy. The same is true of everything else the operator needs to know about a
bucket's contents: its tags, whether it holds any objects, how large it is, and whether its objects
may be removed.

That produced the chicken-and-egg problem recorded during the feasibility study on 2026-06-30. To
write a bucket's first policy the operator must already hold an S3 identity; to hold an S3 identity
someone must create a credentials group and an access key, which is a control-plane act the operator
*can* perform. The way out was measured on the same day: the provider's default inside one project
is open — every credentials group of the project may do everything in every bucket of the project —
so a freshly created bucket carries no policy and any credentials group of the same project can
manage it. The operator can therefore mint one credentials group of its own, take a key in it, and
use that key to write the first policy of every bucket it creates. After that first write the
bucket is closed to everyone the policy does not exempt, which is why the admin identity must appear
in the exemption of every policy the operator ever writes.

The alternative that looks obvious — let each bucket's own workload credential write that bucket's
policy — is closed by the policy itself: the second statement denies the workload everything outside
object operations, so the first successful policy write would also be the last. A credential that
can rewrite the policy that constrains it is not a boundary.

Two properties follow and are the reason this needs a record rather than a code comment. The
credential is *shared*: one identity serves every Bucket in the project, so its health is a
fleet-wide property, not a per-bucket one. And it is *privileged*: it is exempt from the blanket
deny of every bucket in the project, which is precisely the capability that must never be handed to
a workload, never be named as a read-grant reader, and never be reachable through a Bucket's own
`spec.secretRef.name`.

## Decision

**D1 — The operator mints one S3 admin credential per project and owns it exclusively.** It is a
credentials group with the display name `operator-admin` plus exactly one access key in it, created
on first need and shared by every Bucket the operator serves. It is not created per Bucket and it is
never torn down with one. No other party may place a key in that group.

**D2 — Every data-plane operation the operator performs on a bucket it provisions uses that
credential, and only that credential.** A workload credential never performs operator work, and on a
bucket of this operator's own fleet the operator never borrows one. There is exactly one operation
outside that fleet, and it is the only exception:

| Data-plane operation | Credential used |
|---|---|
| writing and reading a bucket's ownership and attribution tags (ADR 0002) | the admin credential |
| writing and healing the isolation policy (ADR 0003) | the admin credential |
| the emptiness check that guards deletion (ADR 0006) | the admin credential |
| the opt-in wipe under `spec.wipeOnDelete` | the admin credential |
| the size listing behind `spec.usage` (ADR 0014) | the admin credential |
| the destination side of a clone (ADR 0011) | the admin credential |
| **reading the clone source** (ADR 0011) — the exception | the credential the Bucket names in `spec.cloneFrom.secretRef` |

The exception is not a borrowed workload credential of this fleet: a clone source is a foreign
bucket, in general at a foreign endpoint, on which the admin credential has no standing at all. The
Bucket supplies that credential itself, it is read only from the Bucket's own namespace (ADR 0001),
and it is used only against the source bucket — never against a bucket this operator provisions.

**D3 — The credential is persisted in one Secret in the operator's own namespace, and that Secret is
its source of truth.** The name comes from `--admin-credentials-secret-name` (or the environment
variable `ADMIN_CREDENTIALS_SECRET_NAME`), default `stackit-s3-provisioner-admin`; the namespace is
the operator's own, taken from `POD_NAMESPACE` or `--operator-namespace`. The Secret carries the
four data keys below and the label `app.kubernetes.io/managed-by: stackit-s3-provisioner`, and it
carries no owner reference, so nothing garbage-collects it.

| Data key | Content |
|---|---|
| `accessKeyID` | the S3 access key id |
| `secretAccessKey` | the S3 secret, obtainable from the provider only at creation time |
| `urn` | the URN of the `operator-admin` credentials group, which every bucket policy exempts |
| `credentialsGroupID` | the group id, needed to delete a key in that group |

**D4 — A service-account key without a known operator namespace is a startup fault.** The process
refuses to start rather than run with nowhere to persist the credential it is about to mint.

**D5 — Bootstrap runs only when the Secret is absent or incomplete, and it replaces the key rather
than repairing it.** Complete means `accessKeyID`, `secretAccessKey` and `urn` are all present. When
bootstrap runs it finds or creates the `operator-admin` group, deletes every access key in that
group, mints one fresh key and writes the Secret; if that write fails, the fresh key is deleted
again so the provider is not left holding a credential nothing can use. Clearing the group is sound
only because of D1's exclusivity, and the old key cannot be kept in any case: the provider returns a
key's secret exactly once.

**D6 — `operator-admin` is the single credentials group that is resolved by display name.** ADR 0002
D3 carves this group out of its prohibition on looking a credentials group up by display name; the
reason is recorded here. A workload group belongs to a Bucket and is attributed through it, so a
name — which the provider does not keep unique — is never needed and never trusted. This group
belongs to no Bucket, so there is no bucket to attribute it through and the name is the only handle.
The exception exists so it stays deliberate rather than becoming a precedent, and it does not extend
to deletion: this group is never released by a Bucket teardown at all (D1), which is a stricter rule
than the one ADR 0002 D4 applies to workload groups.

**D7 — The admin group's URN is exempt in every bucket policy, and can never be a reader.** It is
listed in the `NotPrincipal` of the blanket-deny statement `deny-all-except-admin-and-workload`, and
a read grant naming it is dropped when the policy is built, unconditionally and wherever the reader
list came from — assembling a reader list is not what filters it. An admin identity that is denied on
a bucket cannot rewrite the policy denying it, because there is no
control-plane route to a bucket policy — that is an unrepairable lockout, not an inconvenience.

**D8 — A Bucket may not name the operator's admin Secret.** A Bucket in the operator's namespace
whose `spec.secretRef.name` equals the configured admin Secret name is a definitive configuration
fault: it is refused without a retry, and teardown of such a Bucket never deletes that Secret. Both
halves matter — provisioning would overwrite the admin credential's data keys, and deletion would
destroy the credential for the whole fleet.

**D9 — The credential is cached in the provisioning process, never probed, and replaced only on
human request.** The first successful load is kept for the life of the process and nothing
invalidates it: the process neither re-reads the admin Secret nor watches it. The operator does not
verify the credential at startup and does not mint a replacement in response to a data-plane
refusal; minting an admin-scope credential is a privileged act and is not taken on the strength of
an error signal. The one action that causes a fresh credential to be minted is a **pair**, and
neither half works alone: delete the admin Secret **and then restart the provisioning process**.
Deleting the Secret under a running process changes nothing, because the running process is still
serving every Bucket from its cached copy; restarting without deleting it reloads the same credential
(D5). The measuring path is not a way around this — it re-reads the Secret but never bootstraps
(D10).

**D10 — The measuring path reads the credential and never bootstraps it.** Size measurement re-reads
the Secret for each measurement and, when it is missing or incomplete, waits instead of creating
anything. A measurement is informational and must never have cloud credentials as a side effect; and
until provisioning has run there is nothing provisioned to measure.

## Consequences

The credential is a single fleet-wide dependency, and it is deliberately on the critical path of
almost everything. When it stops working, this is what stops with it:

| Path | Effect |
|---|---|
| resolving a bucket's credentials group from its tags | every reconcile fails; no Bucket converges |
| writing or healing the isolation policy | isolation cannot be established or repaired |
| the emptiness check before teardown | **every Bucket deletion blocks** on the data-loss guard |
| the opt-in wipe | `spec.wipeOnDelete` cannot run |
| the destination side of a clone | a clone cannot start or finish |
| size measurement | listings fail — non-fatal by design, `Ready` is untouched |

Neither half of the repair works alone. A restart alone does not fix it: the Secret is still present
and still complete, so D5's bootstrap does not run and the same dead credential is loaded and cached
again. Deleting the Secret alone does not fix it either: the running process holds the credential in
memory and never looks at the Secret again. That is the price of D3 and D9 together, and it is why
D9 names both halves of the manual act explicitly instead of leaving it folklore.

D5 destroys every key in the `operator-admin` group whenever it runs. That is correct under D1 and
dangerous the moment anyone puts a second key there by hand — an operator sharing that group with a
human user will have that user's key deleted without warning.

The Secret has no owner reference and the chart does not create it, so uninstalling the operator
leaves a live, privileged S3 credential behind in the namespace. That is deliberate — a reinstall
picks the same credential back up and every existing bucket policy stays valid — but it is a
credential nobody is watching, and removing the namespace without deleting the cloud credential
first orphans a key that still has admin scope inside the project.

D7 couples policy correctness to the *group's* lifetime, not just the key's. Replacing the key is
invisible to every policy, because policies name the group URN. Replacing the group is not: every
policy in the project then exempts a principal that no longer exists.

## Alternatives Considered

**Use the service-account key for the data plane.** Rejected, and not actually available: bucket
policies, tags, listings and object deletion are S3 operations, and the service-account identity is
a control-plane bearer identity. There is no route from it to `PutBucketPolicy`.

**Let each Bucket's own workload credential write its bucket's policy.** This was the cheaper
option — no bootstrap, no shared Secret, no fleet-wide dependency, no privileged identity to protect
— and it lost for two independent reasons. The policy the workload would write denies the workload
everything outside object operations, so the first write would be the last and no drift could ever
be healed. And before the policy exists, the workload key would need bucket-management rights, which
is exactly the capability the design exists to withhold.

**Use the project's default credentials group.** Rejected. The default group has broad access across
the project and is precisely the identity that must never reach a workload; building the operator on
it would make the guardrail against handing it out harder to hold, and it is not an identity this
operator owns or can rotate.

**One admin credential per bucket.** Rejected. It multiplies an admin-scope identity by the size of
the fleet for no gain: each one would still have to be exempt in its bucket's policy, still have to
be stored, and a per-bucket credential is no more recoverable than a shared one. The blast radius of
a leak would grow, not shrink.

**An admin key supplied by the installer through Helm values.** Rejected. It makes a human mint,
store and rotate a second long-lived credential, and the operator already holds the control-plane
rights needed to mint it itself — a manual step that buys nothing but adds a way to get it wrong.

**Probe the credential at startup.** Rejected on 2026-09-19. A probe spends a call on every healthy
start to surface a condition the first Bucket reconcile surfaces seconds later anyway, and on an
operator with no Buckets there is nothing to protect and nothing to probe against, since the
data-plane client is addressed through a bucket's endpoint. The readiness probe stays as it is: a
dead admin credential does not make the operator unready, it makes its Buckets fail, which is where
the signal belongs.

**Re-mint automatically when the data plane refuses the credential.** Not taken. It is the obvious
repair for the outage described in Consequences, and it is held back because it reacts to an error
signal with a privileged action. The discriminator would have to separate a refusal *of the
credential* — the S3 error codes `InvalidAccessKeyId` and `SignatureDoesNotMatch` — from
`AccessDenied`, which is a refusal *of the request* and is exactly what a correct isolation policy
produces for a workload. Getting that distinction wrong would let any correctly denied request cause
the operator to mint admin credentials, and each attempt creates a real cloud credential. Should it
be built, it needs a rate limit and it needs to report itself, because an admin key that disappeared
may be an incident rather than an accident and a silent repair would erase the signal.

## Residual risks

**Accepted.** The fleet-wide blast radius of D1 is accepted in exchange for one identity to protect
rather than one per bucket. Uninstall leaving the Secret and the cloud credential behind is accepted
in exchange for a reinstall that keeps working. D5's key-clearing is accepted on the strength of
D1's exclusivity, which nothing enforces technically.

**Open.** A credential destroyed out of band is not detected, not reported and not repaired; it
surfaces as an opaque data-plane failure repeating forever, and the manual repair of D9 has to be
known in advance. No event and no metric marks a bootstrap, so a re-mint is visible only in the log.

**Not verified: that the fleet is recoverable at all when the admin *group* is lost.** If the group
is deleted rather than just its key, every existing bucket policy exempts a principal that no longer
exists, the operator's new admin identity is denied by the blanket-deny statement, and there is no
control-plane route to rewrite a bucket policy — so nothing the operator holds could repair it. That
chain is derived from the policy shape and D7, not measured. It decides whether an automatic
re-mint would be a repair or only a very loud alarm, and it should be tested before anything is
built on top of it.

**Not verified: the lockout property of the storage backend itself.** The claim that this backend can
deny even the account root, which is why D7 exempts the admin group in the first place, is taken from
the backend vendor's documentation and has never been measured against the provider used here.
Whether a stabler second principal could serve as an escape hatch is therefore open.

**Not verified: behaviour during a leader handover.** Where leader election is enabled — the chart
turns it on by default, while the operator's own `--leader-elect` flag defaults to off, so a
deployment that bypasses the chart may run several reconciling replicas at once — only the leader
reconciles and two replicas do not race in the normal case. But a rolling update releases leadership
without an atomic handover, and a bootstrap that clears the group's keys while the outgoing process
still holds one has not been analysed.

**Derived, not reproduced:** the failure table in Consequences was established by reading the code,
not by deleting a live admin key and observing the result.

## References

* [ADR 0001](0001-a-bucket-only-affects-its-own-namespace.md) — why the clone-source credential of
  D2 can only be read from the Bucket's own namespace
* [ADR 0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) — credentials groups are
  attributed through their bucket; its D3 carves out the display-name look-up that D6 above records
* [ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the policy this credential
  exists to write, and the exemption D7 depends on
* [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — one project per deployment,
  which is what makes "one admin credential" well defined
* [ADR 0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) — the emptiness guard that this
  credential performs, and that blocks when it is dead
* [ADR 0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) — the clone's destination
  side uses this credential, and its source side is D2's one exception
* [ADR 0014](0014-bucket-size-is-measured-by-a-separate-controller.md) — the measuring controller,
  whose read-only relationship to this credential is D10
* [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md) — where every
  credential lives and what its loss costs
* [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md) — the isolation
  claim and the lockout gap
* [docs/operations/credentials.md](../operations/credentials.md) — living with the admin Secret, and
  what to do when its credential is gone
* [docs/developer/credentials.md](../developer/credentials.md) — the bootstrap, the Secret contract
  and the key-replacement path as implemented
* [docs/developer/bucket-policy.md](../developer/bucket-policy.md) — how the exemption is built and
  compared
* [docs/developer/clone.md](../developer/clone.md) — how the clone source credential is read and
  where it is used
