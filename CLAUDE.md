# CLAUDE.md — agent instructions

**This file is a router, not a home.** It says where a statement goes and what each home's
contract is; it never becomes a sixth home. There is no configuration table here, no threat
model, no subsystem description and no work list. If a paragraph here is the *only* place
something is written down, it is in the wrong file — move it to the home below and leave a link.

`stackit-s3-provisioner` is a Kubernetes operator written in Go, module
`github.com/guided-traffic/stackit-s3-provisioner` (`go.mod` is the authority for that path). What
it does and how to run it is [README.md](README.md); why it is built that way is
[docs/adr/README.md](docs/adr/README.md); the repository layout, the build, the test suites and the
release process are [docs/developer/](docs/developer/README.md).

## Documentation has five homes, and a statement goes to exactly one

| Kind of statement | Home |
|---|---|
| A decision — what the product does, why, what was rejected, what it costs | an [ADR](docs/adr/README.md), carrying no references into the code |
| How a subsystem works, an invariant, a hard-won detail | [docs/developer/](docs/developer/README.md) |
| What somebody running or integrating the operator needs — prerequisites, install, configuration, reading a `Bucket`, deletion, outages, monitoring | [docs/operations/](docs/operations/README.md) |
| The threat model, each mechanism and the gap it leaves | [docs/security/](docs/security/README.md) — one page per perspective, each closing with what it does not cover. Reporting a vulnerability is [SECURITY.md](SECURITY.md) |
| Work still outstanding | a ticket in [docs/tickets/](docs/tickets/), archived when the work lands |

When two homes fit, **split the sentence, do not copy it** — the decision to the ADR, its
operator-visible consequence to `docs/operations/`, which links back. The test: each page still
reads correctly with the other deleted.

## Decisions are ADRs, and the ADR is written in the same session as the decision

[`docs/adr/`](docs/adr/README.md) is binding, not historical — the code, the Helm chart and the
documentation are expressions of the ADRs, not the other way round. **Write the record in the
session the decision is taken, not when the work finishes**; an ADR written afterwards becomes a
description of what was built, and the alternatives — the expensive part — are already gone.
Format, ground rules and the index are in [docs/adr/README.md](docs/adr/README.md); this file never
describes an individual ADR, and of those ground rules restates only the code-reference contract, as
the table below. Three obligations hold for every change here:

1. **Check the work against the ADRs before implementing it.** A feature, a fix or a rebuild must
   be consistent with every record. Where a requirement contradicts one, it is **not implemented**:
   name the conflict (which ADR, which rule `Dn`, what exactly collides) and get **explicit
   approval from the user** before the record is amended. Only then is the ADR changed — and the
   code in the same change.
2. **A new architectural decision is agreed with the user and its ADR is written in the same
   change.** A decision is architectural when it binds future changes: a trust boundary, the shape
   of the API or the CRD, deletion, error or rotation semantics, the operating and privilege
   model, a migration path. Meeting one of these while building is not a reason to decide quietly
   in code.
3. **Read the relevant record before changing behaviour.** That is part of the task, not optional.

## A ticket is a work list, and nothing outside `docs/tickets/` may cite one

A ticket lives in [docs/tickets/](docs/tickets/), holds what has to be done, what was verified and
how, and what was deliberately left out — nothing else, and only while work is outstanding.

**The extraction is the close, not the move.** Before a ticket goes to
[docs/tickets/archive/](docs/tickets/archive/): the decision moves into an ADR, the user-facing
consequence into `docs/operations/`, `README.md` or `docs/security/`, the subsystem knowledge into
`docs/developer/`; then `git grep` the ticket number and clear what is left. An archived file is
history and is never the source of a current rule.

**Nothing outside `docs/tickets/` may reference a ticket** — not `README.md`, not a security or
developer page, not this file, not a code comment, not a commit message, not a pull request. Cite
the ADR instead; ADRs may be cited from anywhere.

## Code references per home

| Home | May it name files, functions, line numbers? | Why |
|---|---|---|
| [docs/adr/](docs/adr/README.md) | **No — never** | A decision must stay true when the tree moves, and be actionable without the repository. The product's own vocabulary — CRD field paths, annotation and bucket-tag keys, Helm values keys, flags, metric names, event reasons — is not a code reference and is named exactly |
| [docs/developer/](docs/developer/README.md) | **Yes, and should** | That is the point of the page; it goes stale on purpose and is updated in the same change |
| [docs/operations/](docs/operations/README.md) | Yes, where it makes an instruction executable | Same staleness contract |
| [docs/security/](docs/security/README.md) | Yes, where it makes a claim checkable | Same staleness contract |
| [docs/tickets/](docs/tickets/) | Yes | A work list points at the work |
| [README.md](README.md) | Links to documents and example manifests, **not into the source tree** | The front page is read by people who have not cloned the repository |

## Read the page for a subsystem before you change it, and update it in the same change

[docs/developer/](docs/developer/README.md) points at files and functions deliberately, so it rots
when the tree moves. Whoever moves the tree updates the page in the same change; whoever changes a
subsystem reads its page first. Nothing enforces either — they hold by review, or not at all.

## The reference table lives in `README.md` and nowhere else

[README.md](README.md) carries the complete public surface — every `Bucket` CRD field and every
Helm chart value, each marked `# default` or `# example`. A page under `docs/operations/`
*explains* a setting and links to that table; it never restates the list. **A key added anywhere
else is a duplicate**, and two lists drift invisibly.

## Writing conventions for this repository

- **The chat is not the artefact.** Caveman mode, where active, compresses the conversation only;
  code, comments, commit messages and every document are written as normal English prose,
  whatever language the work is discussed in.
- **Every claim is verified before it is written**, and what could not be verified says so in the
  sentence that makes the claim. An assumption never travels as a fact.
- **Dates are absolute**, enumerations of five or more parallel items are tables, links are
  relative, and generated files are never hand-edited.
- The remaining project conventions live with the mechanism they belong to: the commit format in
  [docs/developer/build-and-release.md](docs/developer/build-and-release.md), the test-suite
  contract in [docs/developer/testing.md](docs/developer/testing.md), credential custody in
  [docs/security/credentials-and-secrets.md](docs/security/credentials-and-secrets.md).
