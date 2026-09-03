# Architecture Decision Records

Every durable architecture decision of this operator lives here, one file per decision
family. An ADR records **what was decided, why, what was rejected, and what it costs** — so a
later change can argue with the decision instead of rediscovering it.

## Format

Filename: `NNNN-kebab-case-title.md`, numbered in the order they were written.

Sections, in this order:

| Section | Content |
|---|---|
| `# ADR NNNN: Title` | The decision as a title, not a topic |
| `## Status` | `Accepted` / `Superseded by ADR NNNN` / `Amended`, plus `Date:` and what is actually implemented versus open |
| `## Context` | The forces and the concrete failure that made the decision necessary |
| `## Decision` | `D1 … Dn`, each a rule that holds going forward, in present tense |
| `## Consequences` | What this costs, including the parts nobody likes |
| `## Alternatives Considered` | Each option and why it lost |
| `## Residual risks` | Accepted risks, open items, and what was **not** verified |
| `## References` | Relative links to the code and to sibling ADRs |

Ground rules: English only; every claim verified against the code, with unverified statements
marked as such; identifiers (`functions`, `annotations`, constants) quoted exactly so the ADR
stays checkable against the tree.

## Keeping them current

**An ADR is part of the code, not a historical note.** When a decision changes, the ADR is
updated in the same change — the `Decision` section states the new rule, the `Status` records
the amendment with its date, and the superseded rule is marked in place rather than deleted.
A reader must never find the old rule stated as current.

## Index

### Security and API surface

| ADR | Decision |
|---|---|
| [0001](0001-a-bucket-only-affects-its-own-namespace.md) | A Bucket only affects its own namespace |

## Related documents

* [README.md](../../README.md) — user-facing reference
* [INIT-SETUP.md](../../INIT-SETUP.md) — feasibility findings, policy templates and the founding decisions (§0) that predate this directory
* [CLAUDE.md](../../CLAUDE.md) — project conventions and the ADR obligation
