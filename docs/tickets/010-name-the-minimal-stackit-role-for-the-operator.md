# 010 — The minimal StackIT role for the operator's service account is named

**Status:** Raised — no design needed, the work is investigation plus one proving run. Not approved
for implementation.
**Scope:** this repo (`stackit-s3-provisioner`) for the documentation and the proving run; the answer
itself comes from the provider.
**Date:** 2026-09-19

Raised on 2026-06-30 as open question Q2 of the feasibility findings — *which project-scoped role
does the provisioner's service account actually need* — and never answered. It is given a file on
2026-09-19 because it now blocks a written statement in three places:
[ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) records under *Residual
risks* that the minimal role is unknown and deployments are therefore granted something broader than
the decision wants; [docs/operations/prerequisites.md](../operations/prerequisites.md#which-role)
tells an installer, in the section where the role name belongs, that the name is not established;
and [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md#h-1-an-organisation-level-role-dissolves-layer-1-and-nothing-here-can-see-it)
carries the same sentence inside gap H-1. Three pages say "we do not know" in the place where the
answer goes.

## What it is

One service-account key is the operator's entire authority over the provider. Every control-plane
call in the process is signed with it, and the role attached to that service account decides what
those calls may do. Today the only requirement anyone can state precisely is the **scope** — project
level, never organisation level
([ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md)) — because an
organisation-level role cascades into the other clusters' projects and dissolves the tenant boundary
with no signal at all. Least privilege *inside* the project is unspecified, so an installation
following the documentation as written reaches for whatever role obviously works, which in practice
means project owner.

The cost is blast radius, and it is not hypothetical: the key is a file on disk in the operator
namespace, readable by anything that can read Secrets there or exec into the pod, and it is read once
at process start and held in memory for the process lifetime
([ADR 0005 D8](../adr/0005-the-operator-serves-one-project-in-one-region.md)). A leaked key with an
owner-level role is authority over the whole project — every service in it, not only object storage.
A leaked key with the minimal role is authority over buckets, credentials groups and access keys of
that project, which is bad, bounded, and exactly the damage the product's own threat model already
assumes.

This ticket does not make the operator safer by changing code. It replaces "we do not know" with a
name, so the privilege an installation grants becomes a decision instead of a guess.

**The name cannot be derived from this repository.** Role catalogues are a provider-side artefact:
no call in the tree enumerates roles, the SDK exposes none, and the key file carries a project id and
a private key and nothing about grants (verified 2026-09-19). The answer comes from StackIT's own
documentation or from a support answer, and the repository's part is to state exactly what the role
has to cover and then prove a candidate.

## What the tree looks like today

Surveyed 2026-09-19.

**The control-plane call set is closed and small.** `stackit/client.go` is the only file in the tree
that imports `github.com/stackitcloud/stackit-sdk-go/services/objectstorage` (verified by grep over
all Go files, tests included), so every request the service-account key ever signs is one of the
twelve operations below. There is no second path, no raw HTTP client, and no dynamic call
construction.

| Operation | Called from | What needs it |
| --- | --- | --- |
| `GetServiceStatus` | `EnsureService` | Deciding whether object storage is enabled; runs on the first provisioning reconcile of the process and is then cached for the process lifetime |
| `EnableService` | `EnsureService` | Enabling the service, and **only** on a structured `404` answer — never otherwise |
| `ListBuckets` | `ListBucketNames`, used by `HasBucket` and `WaitBucketVisible` | Every existence question: provisioning, each read grant, teardown, and the post-create visibility poll |
| `CreateBucket` | `CreateBucket` | Provisioning a `Bucket`'s cloud bucket |
| `GetBucket` | `BucketConnInfo`, `BucketEndpoint` | The path-style URL, which yields the S3 endpoint written into the workload Secret and used by the measuring controller |
| `DeleteBucket` | `DeleteBucket` | Finalizer teardown, after the emptiness guard |
| `ListCredentialsGroups` | `ListCredentialsGroups`, `EnsureCredentialsGroup` | Admin bootstrap, and the project-wide group index built during provisioning and teardown |
| `CreateCredentialsGroup` | `CreateCredentialsGroup`, `EnsureCredentialsGroup` | The operator's own admin identity, and each `Bucket`'s workload identity |
| `DeleteCredentialsGroup` | `DeleteCredentialsGroup` | Teardown of a `Bucket`'s own workload group |
| `ListAccessKeys` | `ListAccessKeyIDs` | Checking that a group still holds a live key |
| `CreateAccessKey` | `CreateAccessKey` | Minting the admin key and each workload key |
| `DeleteAccessKey` | `DeleteAllAccessKeys`, `DeleteAccessKey` | Clear-before-create on rotation, compensating delete on a failed Secret write, and teardown |

`hack/e2ecleanup` uses the same client and therefore the same twelve operations; it needs no rights
beyond the operator's.

**The data plane is outside the question.** Bucket policies, bucket tags, object listing, the
emptiness check and the opt-in wipe are plain S3 calls authenticated with the access key the operator
mints for itself ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)),
not with the service-account token. A narrower role restricts what the operator may *create*; what
the created credential may then *do* on the data plane is a separate mechanism — and whether the two
are actually independent is open question Q2 below.

**A too-narrow role is loud, which makes a candidate cheap to test.** The API answers a structured
`403` (body `{"…","status":403,"error":"Forbidden","message":"Unauthorized"}`, measured live
2026-08-25 and recorded in `stackit/errors.go`). `ProviderRefused` classifies that as a definitive
refusal, so the `Bucket` drops to `Ready=False`, reason `Failed`, immediately and without a readiness
hold, and `status.message` names the operation that was refused
([ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md)). A reduced-role run therefore
reports which call is missing rather than hanging.

## The decisions it collides with

| Decision | How this ticket touches it |
| --- | --- |
| [ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md) | States the scope rule and nothing about least privilege. The answer is a companion rule, and the *Residual risks* paragraph recording the gap is retired by it |
| [ADR 0005 D1](../adr/0005-the-operator-serves-one-project-in-one-region.md) | The project binding comes from the key. A second, reduced key for the proving run means a second binding, and by D9 it must not be pointed at a project an existing deployment already serves |
| [ADR 0004 D1](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) | The bootstrap mints a project-wide credentials group and a key for it. Whatever else a minimal role loses, it keeps `CreateCredentialsGroup` and `CreateAccessKey`, or the operator cannot bootstrap at all |
| [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) | Isolation rests on the admin credential being able to write bucket policies. If a reduced role produces a credential that cannot, isolation fails silently rather than loudly — see Q2 |
| [ADR 0012 D6](../adr/0012-ready-describes-the-last-verified-state.md) | Supplies the failure signal the proving run reads: a structured `403` is definitive and names its operation |

## Work list

1. **Obtain the role catalogue.** Get StackIT's project-scoped role list for Object Storage from the
   provider's documentation or a support answer, and record which document or ticket the answer came
   from, with its date. This step is outside the repository and nothing here can substitute for it.
2. **Map the twelve operations to the candidate role** using the table above, and check the mapping
   against the two groupings that are likely to split: *bucket* operations against *credentials
   group and access key* operations, and service enablement against both (Q1).
3. **Create a reduced service account.** In a test project, a service account holding **only** the
   candidate role, at project scope, and export a key file. Do not reuse `account-1.json` or
   `account-2.json`; those carry the current broad grant and would prove nothing.
4. **Run the full end-to-end suite against it.** `make e2e-stackit SA_KEY=<reduced key>` brings up
   Kind, installs the chart with that key and runs the cloud suite: provisioning, read grants and
   their revocation, size measurement, clone, and a teardown per test through CR deletion. Follow it
   with `make e2e-stackit-sweep-dry`, which must report nothing left behind — a teardown that failed
   on a missing right shows up there and nowhere else.
5. **Run the control-plane integration suites against it** —
   `go test -tags integration ./stackit/ -run Integration` and
   `go test -tags integration ./internal/controller/ -run IntegrationGroupAttribution` — for the
   paths the e2e suite does not reach, in particular group attribution against a pre-existing bucket.
6. **Exercise service enablement deliberately, or say that it was not.** `EnableService` fires only
   on a structured `404` from `GetServiceStatus`, so a run against a project where object storage is
   already enabled never calls it (verified in `EnsureService`, `stackit/client.go`). Either use a
   project where the service is not yet enabled, or record that this one operation of the twelve is
   unproven and why.
7. **Write the answer into its homes**, in the same change: the role name and its source into the
   *Which role* section of [docs/operations/prerequisites.md](../operations/prerequisites.md#which-role),
   replacing the gap statement; a new rule in
   [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) naming the role beside
   the scope rule of D5, with the *Residual risks* paragraph about the unknown role removed and the
   `Status` section amended and dated; and the sentence inside gap H-1 of
   [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md#h-1-an-organisation-level-role-dissolves-layer-1-and-nothing-here-can-see-it)
   trimmed to what stays true — H-1 itself does not close, because a wrong *scope* remains invisible
   whatever the role is called.
8. **Decide the record.** The privilege model is architectural, so the outcome is either an amendment
   to ADR 0005 or its own ADR (Q3), agreed before it is written.

## Open questions

### Q1 — Does enabling object storage need more than the object-storage management role?

Enabling a service on a project may be a project-administration permission rather than an
object-storage one. If it is, the minimal role cannot cover `EnableService`, and the choice is
between granting a second role and taking enablement out of the operator — making it a prerequisite
the installer performs once, which would change
[docs/operations/prerequisites.md](../operations/prerequisites.md) and a rule in ADR 0005. Not
verified; the answer comes with the role catalogue.

### Q2 — Does a narrower role change what the credentials groups it creates may do on the data plane?

The operator's admin key must be able to write bucket policies and bucket tags, or isolation
([ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)) and ownership
attribution ([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md)) both
fail. It is **not verified** whether the rights of a created credentials group derive in any way from
the role of the service account that created it; the assumption in this repository is that they do
not. This is the one failure mode of a reduced role that would not be loud, which is why step 4's
proving run must include a policy write and a tag write rather than only a bucket create.

### Q3 — Amendment to ADR 0005, or its own record?

Naming the role is a rule about the privilege model, which CLAUDE.md counts as architectural. It sits
naturally beside ADR 0005 D5, whose scope rule it completes. It would deserve its own record if the
answer forces a structural change — for example if Q1 moves service enablement out of the operator.
Decide with the user before writing.

### Q4 — Is the role assignment uniform across regions?

Role assignments are made at project scope and control-plane calls are region-scoped. Whether a role
granted for a project covers every region of it was never checked, and it matters only for a second
deployment serving a second region
([ADR 0005 D9](../adr/0005-the-operator-serves-one-project-in-one-region.md)). Not verified.

## What it must not break

| Invariant | Why it is at risk here |
| --- | --- |
| Roles stay at project scope | The temptation while hunting a working role is to widen the grant until calls succeed. Widening *upwards* — to organisation scope — is the one move that silently dissolves the tenant boundary ([ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md)) and is forbidden even as a diagnostic step |
| No self-check is added | A permission probe in the operator asks the same token about itself, so it cannot detect an over-privileged grant. H-1 already records that adding one would not help; this ticket must not produce one as a consolation prize |
| Existing deployments keep working | The outcome is documentation plus, at most, an ADR. No code changes, no chart changes, and no reduction of an existing installation's grant without the operator of that installation doing it deliberately |
| The proving run does not touch a live project | `make e2e-stackit` creates and deletes real buckets, groups and keys, and its admin bootstrap clears the project-wide admin identity's existing keys. Pointed at a project an operator already serves, it destroys that operator's admin credential ([ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md), *Consequences*) |

## Done when

| # | Criterion |
| --- | --- |
| 1 | The minimal project-scoped role is named exactly, with the provider document or support answer it came from and the date it was obtained |
| 2 | A service account holding **only** that role, at project scope, completes `make e2e-stackit` green — provisioning, read grants, measurement, clone and teardown — and `make e2e-stackit-sweep-dry` afterwards reports nothing left behind |
| 3 | The control-plane integration suites pass with the same key, including group attribution against a pre-existing bucket |
| 4 | Each of the twelve control-plane operations is either exercised by that run or explicitly recorded as unproven, with the reason — `EnableService` is the one that needs saying out loud |
| 5 | A policy write and a tag write are among the operations the run exercised, so Q2 is answered by evidence rather than by assumption |
| 6 | The role is stated in the *Which role* section of the prerequisites page as a verified fact, and the three places that currently say the name is unknown no longer say it |
| 7 | ADR 0005 carries the rule and no longer carries the gap, with its `Status` amended and dated; or the new record exists, agreed in advance |
| 8 | Gap H-1 still stands, reduced to what remains true: scope, not least privilege, is the boundary nothing in the product can see |

## References

* [ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md) — the project binding, the
  project-scope rule of D5, and the residual risk this ticket retires
* [ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — why the data plane
  is authenticated by a credential the operator mints, not by the service-account token
* [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the policy writes a
  reduced role must not cost the operator
* [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) — D6, the definitive-refusal
  classification that turns a missing right into a named failure
* [docs/operations/prerequisites.md](../operations/prerequisites.md#which-role) — where the answer is
  written for an installer
* [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md#h-1-an-organisation-level-role-dissolves-layer-1-and-nothing-here-can-see-it) —
  gap H-1, which this ticket narrows and does not close
* [stackit/client.go](../../stackit/client.go) — the only file that speaks to the control plane
* [stackit/errors.go](../../stackit/errors.go) — the measured answer shapes a reduced-role run
  produces
