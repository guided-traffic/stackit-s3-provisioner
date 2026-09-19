# Tickets

**A ticket is a work list and nothing else.** It holds what has to be done, what was verified and
how, and what was deliberately left out. It exists while the work is outstanding, and it is the one
place where unfinished work **on this repository** may be written down: a durable page states what
*is*, never what is planned. Work this repository raises for a *different* repository is written down
at the repository root instead and is not numbered here — see
[work that belongs to another repository](#work-that-belongs-to-another-repository).

**Nothing outside `docs/tickets/` may reference a ticket** — not [README.md](../../README.md), not a
page under [docs/security/](../security/README.md), [docs/operations/](../operations/README.md) or
[docs/developer/](../developer/README.md), not an ADR, not a code comment, not a commit message, not
a pull request. Cite the [ADR](../adr/README.md) instead; an ADR may be cited from anywhere, and it
outlives the work list that produced it.

Files are named `NNN-<slug>.md`, numbered in the order they are raised and never renumbered. The
slug names the **outcome**, not the topic: `005-a-provisioned-bucket-that-vanished-is-reported.md`,
so the directory listing reads as the list of things that will be true when the work lands.

**Every ticket carries its own ADR, written when that ticket is implemented** — with the change, not
signed off in advance. The decision a ticket proposes is not a decision until it is built, so the
record is produced in the session the work happens and the ticket then has somewhere durable to hand
its reasoning to.

---

## Open work

Six live tickets, none approved for implementation. Five carry **2026-09-19** on the file itself, the
date they were raised or last refined; 010 is older than its file — it was open question Q2 of the
feasibility findings on **2026-06-30** and was given a file on 2026-09-19, because three durable pages
now say "unknown" in the place its answer belongs. States below are judged from each file's own
answered and unanswered questions on 2026-09-19.

| Ticket | State | What it covers |
| --- | --- | --- |
| [005 — a provisioned bucket that vanished is reported](005-a-provisioned-bucket-that-vanished-is-reported.md) | Design complete: all seven questions answered, not approved for implementation | A bucket the operator provisioned can disappear behind its back (console deletion, cleanup script, a key for a different project). Today the operator reads "absent" as "not yet there", re-creates it empty and overwrites the workload Secret, with no event, condition or metric saying the data is gone. The ticket makes that a reported failure state, with a deliberate opt-in for the one case where re-creating is what the user wants. Precondition for 007. |
| [006 — recover from a deleted admin S3 key](006-recover-from-a-deleted-admin-s3-key.md) | **One question still open (Q4)**, not approved for implementation | The operator's single S3 admin credential ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)) drives every data-plane call. Deleted in the console, it breaks the whole fleet — including every CR deletion, which blocks on the emptiness guard — and a restart does not help, because a Secret holding a dead key looks valid. The ticket adds recognition, self-recovery and visibility. Q4 asks whether the isolation policy needs a lockout escape hatch at all; it changes the policy shape and depends on a provider claim that has never been measured here, so it is deliberately not decided inside a credential-recovery ticket. |
| [007 — hot-reload the StackIT service-account key](007-hot-reload-the-stackit-service-account-key.md) | Design complete: every listed question answered, not approved for implementation | The SA key is read once at process start and the SDK signs every token refresh from the in-memory copy, so a rotated key on disk changes nothing until the pod restarts — while a revoked key is a definitive refusal that drops every `Bucket` to `Failed` at once. The ticket picks up a replaced key without a restart, and refuses an invalid or foreign one without displacing the working client. Waits on 005 for the guard that makes a foreign key safe. |
| [008 — decide bucket existence with a per-bucket read](008-decide-bucket-existence-with-a-per-bucket-read.md) | Design complete: all three questions answered, not approved for implementation | Every existence question is answered today by listing all buckets of the project and scanning the result: one project-wide listing per question, unverified pagination, and an answer that cannot be definitive — a listing that came back short is indistinguishable from a real absence, which is exactly what teardown gates the data-loss guard on. Independent of 005; it improves paths that exist regardless. |
| [009 — migrate off the deprecated `objectstorage` package](009-migrate-off-the-deprecated-objectstorage-package.md) | Survey complete and two behaviour changes measured; **five questions open (Q1–Q5)**, not approved for implementation | The provider marked the top-level `objectstorage` package of its Go SDK deprecated for removal after **2026-09-30** — the only forward-dated commitment in this repository that somebody else made. The pinned version keeps compiling past that date; what expires is the ability to *move*, because the first SDK update afterwards becomes a migration instead of a version bump. One file imports it and no SDK type crosses the package boundary, so the move itself is small — the cost sits in the target package's stricter behaviour: it rejects a configured region, and that failure is structurally invisible to the offline suite. |
| [010 — name the minimal StackIT role for the operator](010-name-the-minimal-stackit-role-for-the-operator.md) | Nothing to design; **the answer is not in this repository** and four questions (Q1–Q4) wait on it. Not approved for implementation | The service-account key is the operator's entire authority over the provider, and only its *scope* is specified — project level, never organisation level ([ADR 0005 D5](../adr/0005-the-operator-serves-one-project-in-one-region.md)). Least privilege inside the project is unnamed, so an installation reaches for a role that obviously works, which in practice means project owner, and a leaked key is then authority over the whole project rather than over its object storage. The ticket enumerates the twelve control-plane operations a role has to cover and proves a candidate with a reduced service account; the catalogue it is matched against comes from the provider. |

---

## Work that belongs to another repository

Four work lists sit at the repository root as `local_<slug>.md`. Each describes a change in a
**different** repository — a GitOps repository, a consumer of the operator, a cluster configuration —
raised here because this is where the need was found and where the facts behind it can be verified.
They carry no ticket number, because a number in `docs/tickets/` promises work that will land in this
tree, and none of this will.

**The convention: work in another repository is written up here and never performed from here.** A
change needed outside this tree is described in full — target repository, exact path, the change, how
to verify it, and what was verified from this side and on which date — and then handed over. Nobody
edits the other repository from this one, so a cross-repository file stays until somebody reports back
that it landed.

| Work list | Target | What it asks for |
| --- | --- | --- |
| [local_adopt-interim-bucket-view-clusterrole.md](../../local_adopt-interim-bucket-view-clusterrole.md) | `k8s-flux-base` | A hand-written interim `stackit-s3-provisioner-view` ClusterRole, added on 2026-09-03 while the chart had no user roles, now collides with the one the chart ships under the same name; Helm refuses to adopt an object without its ownership metadata and Flux rolls the release back. Must be handled **before** the chart pin moves |
| [local_bump-provisioner-chart-for-bucket-roles.md](../../local_bump-provisioner-chart-for-bucket-roles.md) | `k8s-flux-base` | Raise the mgmt-p chart pin past the release carrying the aggregated `Bucket` ClusterRoles, so readers holding only built-in roles can see `Bucket` CRs at all. Blocked by the entry above |
| [local_gitlab-backup-read-grants.md](../../local_gitlab-backup-read-grants.md) | `k8s-flux-mgmt` | Declare the backup bucket as a reader on the nine GitLab data buckets, so the nightly backup covers the object-storage data instead of the Gitaly repositories alone. The provisioner side shipped with [ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md); the consumer change was never recorded as done |
| [local_verify-provider-hold-on-a-cluster.md](../../local_verify-provider-hold-on-a-cluster.md) | The GitOps repository of a dev or management cluster | Cut the operator off from the provider API and its token endpoint with a temporary `NetworkPolicy` and watch the hold of [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) behave on a real cluster. That rule is verified offline against a simulated provider only, and this is the one test that exercises the whole chain down to the silent readiness alert |

---

## Closing a ticket: the extraction is the close, not the move

Moving the file is the last step and the least important one. Before it moves, everything durable
inside it has to reach the home that will keep it current:

| Step | What moves | Where it goes |
| --- | --- | --- |
| 1 | The decision — what the product now does, what was rejected, what it costs | An [ADR](../adr/README.md), written with the implementation |
| 2 | The user-facing consequence — configuration, deployment, behaviour, a security property or gap | [docs/operations/](../operations/README.md), [README.md](../../README.md) or [docs/security/](../security/README.md) |
| 3 | The subsystem knowledge — an invariant, a mechanism, a hard-won detail | [docs/developer/](../developer/README.md) |
| 4 | Whatever still points at the ticket | `git grep` the number and clear it |
| 5 | The file itself | [`archive/`](archive/) |

**An archived ticket is history and is never the source of a current rule.** A rule that can only be
found in `archive/` means one of steps 1 to 3 was skipped — it is the same defect as a rule that
lives only in a live ticket, and the fix is to extract it now, not to cite the archive.

---

## Archive

Closed work lists, kept for their origin stories and their measurements. The decision each one
produced lives in the ADR beside it; verified on 2026-09-19 that each named record carries it.

| Archived work list | Closed | The decision now lives in |
| --- | --- | --- |
| [001 — multipart management actions are allowed for the workload](archive/001-multipart-management-actions-are-allowed-for-the-workload.md) | 2026-07-22 | [ADR 0003 D7](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — a container registry's blob commits died with 403 because listing and aborting its own multipart uploads was treated as bucket management; multipart management is object work |
| [002 — a bucket can grant read access to siblings](archive/002-a-bucket-can-grant-read-access-to-siblings.md) | 2026-08-24 | [ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) — a backup job could not read the sibling buckets of its own namespace, so read access became something the bucket that owns the data declares |
| [003 — a ready bucket survives a transient provider error](archive/003-a-ready-bucket-survives-a-transient-provider-error.md) | 2026-08-25 | [ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) — a provider blip flipped every provisioned bucket out of `Ready` on the first failed call; readiness now describes the last verified state, and the ticket's own error taxonomy was rejected in favour of classification by origin |
| [004 — user-facing ClusterRoles are shipped aggregated](archive/004-user-facing-clusterroles-are-shipped-aggregated.md) | 2026-09-03 | [ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) — the chart ships `Bucket` read and write as aggregation fragments for the built-in roles, and write is deliberately never handed out without Secret access; the privilege inventory behind that is [docs/security/rbac-and-privilege.md](../security/rbac-and-privilege.md) |

---

## The other documentation

| Home | What it holds |
| --- | --- |
| [docs/adr/](../adr/README.md) | Every decision: what the product does, why, what was rejected, what it costs |
| [docs/developer/](../developer/README.md) | How a subsystem works, and the invariants it rests on |
| [docs/operations/](../operations/README.md) | What somebody running or integrating the operator needs |
| [docs/security/](../security/README.md) | The threat model and the gap each mechanism leaves |
| [README.md](../../README.md) | The front page and the complete configuration reference |
