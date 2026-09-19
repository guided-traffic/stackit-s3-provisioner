# Architecture Decision Records

Every durable architecture decision of this operator lives here, one file per decision family. A
record states **what was decided, why, what was rejected, and what it costs** — so a later change
argues with the decision instead of rediscovering it.

A record is written in the session the decision is taken, not when the work finishes. What is
decided and what is built are two different statements: `Status` carries the build state,
`Decision` is present tense either way.

## Format

Filename: `NNNN-kebab-case-title.md` — four digits, numbered in the order the records were written,
**never reused and never renumbered**. The title states the decision, not the topic:
`0009-the-physical-bucket-name-is-composed-and-then-frozen.md`, not `0009-bucket-names.md`. Reading
the directory listing alone should give a reader the design of the product.

Sections, in this order:

| Section | Content |
|---|---|
| `# ADR NNNN: Title` | The decision as a sentence, not a topic |
| `## Status` | `Accepted` / `Superseded by ADR NNNN` / `Amended`, plus `Date:` and two or three sentences on what is implemented and what is still open |
| `## Context` | The forces and the concrete failure that made the decision necessary, written so a reader who has never seen the code understands why it was not free |
| `## Decision` | `D1 … Dn`, each a rule that holds going forward, in the present tense |
| `## Consequences` | What this costs, including the parts nobody likes |
| `## Alternatives Considered` | Each option and why it lost — including the one that was cheaper and lost anyway |
| `## Residual risks` | Accepted risks, open items, and **separately** what was not verified |
| `## References` | Relative links to sibling ADRs and to pages under `docs/operations/`, `docs/security/` and `docs/developer/` — never into the source tree |

## Ground rules

* **A decision, not an implementation.** A record carries no file paths, no package, type, function
  or method names, no line numbers, no test names and no links into the source tree. A decision has
  to stay true when the tree moves, and a reader has to be able to act on it without cloning the
  repository. If a sentence cannot be written without such a reference, the missing half belongs in
  [`docs/developer/`](../developer/README.md) and the record links to that page instead.
* **The product's own vocabulary is not a code reference, and is named exactly.** CRD field paths,
  annotation and label keys, bucket tag names, Helm values keys, command-line flags, environment
  variables, metric names, event reasons, condition types, S3 action names and error codes are the
  interface the decision is about — spelled precisely, never paraphrased.
* **A record never cites a ticket.** Outstanding work lives in [`docs/tickets/`](../tickets/README.md)
  and is deleted when it lands; a record that pointed at it would rot. Cite a sibling ADR instead.
* **Decisions are labelled `D1 … Dn` and cited as `ADR 0012 D5`.** A label is a stable address: it
  lets another record, a page in another home or a review comment point at one rule rather than a
  whole document. A label is never renumbered, even when the rule behind it is superseded.
* **What was not verified is marked as such**, in the sentence making the claim. An assumption never
  travels as a fact, and "I could not check this" is a complete statement.
* **Every shown value is marked `# default` or `# example`**, and `# default` only where a default
  truly exists. Dates are absolute. Enumerations of five or more parallel items are tables.
* **English only**, whatever language the work was discussed in.

## Keeping them current

**A record is part of the product, not a historical note.** It is updated in the same change as the
decision it records — not afterwards, not in a follow-up. When a decision changes:

* the `Decision` section states the new rule;
* the superseded rule is **struck through in place** (`~~old rule~~`), with a pointer to the record
  that replaced it, never deleted;
* `Status` records the amendment and its date;
* the index row below is trued up in the same change.

A reader must never find an old rule stated as current.

## Index

The **State** column is a coarse reading aid, last trued up on 2026-09-19. Each record's own
`Status` section is the authority on what is built and what is open.

### Isolation and tenancy

| ADR | Decision (one line) | State |
|---|---|---|
| [0001](0001-a-bucket-only-affects-its-own-namespace.md) | A Bucket only affects its own namespace — its Secret, its grants and its RBAC never reach across one | Implemented |
| [0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) | Workloads are isolated from each other by an explicit deny policy, because the provider's default is open | Implemented |
| [0005](0005-the-operator-serves-one-project-in-one-region.md) | One operator deployment serves one project in one region, bound by the mounted service-account key | Implemented, except that replacing the key needs a restart, nothing checks which project a deployment is bound to, and the minimal role is still unnamed |
| [0008](0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) | A read grant is declared by the Bucket that owns the data, never claimed by the reader | Implemented |

### Credentials and identity

| ADR | Decision (one line) | State |
|---|---|---|
| [0002](0002-a-credentials-group-is-attributed-through-its-bucket.md) | A credentials group is attributed through its bucket's tags and policy, never through its display name | Implemented |
| [0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) | The operator bootstraps and owns one S3 admin credential per project, exempt in every bucket policy | Implemented, except that a credential destroyed at the provider is not detected and recovery is manual |
| [0007](0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) | A workload credential lives in its Secret, never expires, and rotates only when a rotation is requested | Implemented, except that a published key deleted out of band goes unnoticed while the group still holds another key |

### Lifecycle and naming

| ADR | Decision (one line) | State |
|---|---|---|
| [0006](0006-a-bucket-is-deleted-only-when-it-is-empty.md) | A bucket is deleted only when it is empty, and a wipe needs three independent authorizations | Implemented, except that the emptiness test looks at current objects only, so a versioned bucket holding non-current data reads as empty |
| [0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) | The physical bucket name is composed from the install-time naming policy and then frozen for the Bucket's life | Implemented |
| [0010](0010-the-operator-never-writes-to-a-bucket-spec.md) | The operator never writes a Bucket's `spec` or labels, so a syncing applier and the operator cannot fight | Implemented |
| [0011](0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) | A clone runs exactly once, as a Job in the operator namespace, and its completed state is terminal | Implemented |

### Operating under failure

| ADR | Decision (one line) | State |
|---|---|---|
| [0012](0012-ready-describes-the-last-verified-state.md) | `Ready` describes the last verified state of a bucket, not the last verification attempt | Implemented, except that a provisioned bucket which vanished at the provider is re-created rather than reported |
| [0013](0013-a-provider-outage-is-held-fleet-wide.md) | A provider outage is held fleet-wide, and the trip condition is the absence of success rather than a parsed error | Implemented, except that the size-measurement queue is not held by the breaker |

### Measurement

| ADR | Decision (one line) | State |
|---|---|---|
| [0014](0014-bucket-size-is-measured-by-a-separate-controller.md) | Bucket size is measured by a separate controller that can only write the usage part of the status | Implemented, except that incomplete multipart uploads are never counted |

## Related documents

* [README.md](../../README.md) — the front page and the single reference table
* [DEVELOPER.md](../../DEVELOPER.md) — repository layout, build and test matrix, CI and release
* [docs/developer/](../developer/README.md) — how each subsystem works, with the code references a record may not carry
* [docs/operations/](../operations/README.md) — what somebody deploying, configuring or integrating with the operator needs
* [docs/security/](../security/README.md) — the threat model, each mechanism and the gap it leaves
* [docs/tickets/](../tickets/README.md) — work that is still outstanding
