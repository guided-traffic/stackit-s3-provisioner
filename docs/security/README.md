# Security

This directory is the security **design** of the operator: what each mechanism defends against, how
it is built to defend it, and — in the same breath — what it leaves open. It is written for somebody
evaluating whether this operator is safe to put in front of their data, and for whoever has to keep
it that way.

There is deliberately no index here. The set of pages changes, and a list in this file would be a
second place to keep current; the first time it fell behind, a reader would conclude a perspective
does not exist. **The file names are the index.** `ls docs/security/` is the table of contents, which
is why a page is named for the perspective it takes rather than for the mechanism it happens to
describe.

This file describes the **form** of a page here — what belongs on one, how it is shaped, how a gap
is recorded and how the directory grows.

## What belongs here, and what does not

A page here answers *what does this defend against, and what does it not*. Everything else about the
same mechanism has another home, and the test is whether each page still reads correctly with the
other deleted.

| The sentence you have | Where it goes |
|---|---|
| A threat, a boundary, a mechanism's guarantee, or the gap it leaves | A page here |
| The rule itself — what the product does, why, what was rejected, what it costs | An [ADR](../adr/README.md), cited from here as `ADR 0003 D5` |
| How the mechanism is implemented — the call order, the function that builds the document, the predicate that filters a watch | [`docs/developer/`](../developer/README.md) |
| What somebody running the operator has to configure, watch or do about it | [`docs/operations/`](../operations/README.md) |
| Work that has not happened yet | A [ticket](../tickets/README.md) — never a page here |
| How to report a vulnerability in this operator | [`SECURITY.md`](../../SECURITY.md) at the repository root |

A page here may name files, functions and flags where that makes a claim checkable — that is the
difference between this home and an ADR, which carries no references into the tree at all. The cost
is that such a page goes stale when the tree moves, so whoever moves the tree updates the page in
the same change.

## The shape of a page

1. **A title that names the perspective**, not the mechanism. The reader is choosing from a
   directory listing, and a title naming what is at stake ("credentials and secrets") tells them
   something a title naming an implementation ("the policy builder") does not.
2. **One opening paragraph** that says what the page covers and then points at the neighbouring
   perspective the reader may actually have wanted. Somebody who opened the wrong page should find
   that out in the first five lines, with a link, not on page three.
3. **The body in `##` sections, never numbered.** Numbers invite citation by number, and citation by
   number breaks — see *Citing a page* below.
4. **A closing `## What this does not cover`.** Every perspective page ends with one, no exceptions.
   A page that cannot name its own limits has not been thought through, and a reader who finishes a
   page without knowing where its authority stops will assume it covers everything.

This README is not a perspective page and has no closing section of its own; the rule applies to the
pages, not to the description of them.

## Gaps

An open gap lives in **the page whose mechanism has it**, at the end of that page — never in a
central list of open security issues. A gap recorded beside the thing it affects is read by exactly
the person evaluating that thing; a central list is read by nobody, and the person evaluating one
mechanism never finds the gap that mechanism has.

**A gap carries a fixed identifier in its heading, `H-<n>`, and one continuous sequence runs across
the whole directory.** The numbering does not restart per page: a new gap takes the next free number
whichever page it lands on, so an identifier is unique across this home and never changes once
written.

**Home-wide uniqueness is the point, not a tidiness rule.** Reports, reviews and conversations say
"H-4" and stop there — the file name does not travel with the number, and a per-page sequence would
put an `H-4` on several pages at once and make the bare citation ambiguous. The pages here already
depend on it: a gap on one page is referred to from another by bare number alone. It also survives
the one event that reshuffles this directory — splitting a page moves a gap to a new file without
touching its identifier, so every citation made before the split still resolves. A written citation
still links the file and the heading anchor (see *Citing a page* below), because a link needs a
target; the number alone is what identifies the gap.

**A number is never reused.** When a gap closes it loses its number and its entry disappears; the
number does not pass to the next gap written, on that page or on any other. The next free number is
the one above the highest ever issued, not the lowest one currently unoccupied — otherwise a closed
`H-4` would be re-issued to an unrelated gap and every report citing the old one would now read as a
claim about the new one. What survives a closed gap is the rule it left behind, written as prose in
the mechanism's own section — the reader needs to know that a check exists and why it reads the way
it does, not that it was once missing.

This file does not record which page currently holds which range; that would be the index it refuses
to keep, and it would be wrong the first time a page is split. `grep -rn '^### H-' docs/security/`
answers both questions a writer has — which numbers are in use, and what the next one is.

A gap entry is only useful if it can be acted on, so it names: **which mechanism** it belongs to,
**which adversary** it requires (and "none — this is a misconfiguration" is a real answer), whether
it is **live today** or dormant, and **what an operator can do meanwhile**. A vague warning is worse
than no warning, because it consumes attention and yields no action.

Both placements in use today are correct: the `H-<n>` entries appear either as `###` subsections of
the closing section, or under an `## Open gaps` heading immediately before it. What matters is that
the gap sits on the page whose mechanism has it and at the end of it, not which of the two headings
carries it.

## Citing a page

**Cite by file name and heading, never by section number.** Link the file, and where a specific gap
or section is meant, link its heading anchor: `[H-3](<perspective>.md#h-3-the-gap-stated-as-a-sentence)`.

Numbers are stable only until something is inserted above them; headings are stable because they are
what the page is about. This repository has already paid for the other way round: the historical
single-source document was cited by section number from code comments, CRD field descriptions and
tickets, and its own sections were not even in numeric order — following such a citation by scrolling
landed in the wrong section.

A rule belonging to a decision is cited as `ADR NNNN Dn` with a relative link to the record, for
instance [ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md) D5. The page
explains and instructs; the record decides. Where a page and an ADR disagree, the ADR wins — unless
the **code** disagrees with both, in which case the page says so explicitly rather than quietly
choosing a side.

## Ground rules

* **Every claim is verified against the code before it is written.** Where something could not be
  verified, the page says so in the sentence making the claim. "Not verified, and this is the gap:
  the backend's lockout behaviour is quoted from the vendor and has never been measured here" is a
  complete, publishable statement; the same sentence with the caveat dropped is a fabrication.
* **Where the code and a page disagree, the code wins**, and the disagreement is named rather than
  quietly corrected — somebody made that claim for a reason, and the reason is usually the
  interesting part.
* **A gap is documented, not hidden.** Nothing here is softened because it is unflattering. A
  product whose security pages contain no gaps is a product whose security pages were not written
  honestly.
* **No tickets.** No checkboxes, no owners, no "planned for", no work item with a date. A page here
  states what *is*; outstanding work is a [ticket](../tickets/README.md), and nothing outside
  `docs/tickets/` may cite one — a citation outlives the ticket it points at. Where a page must
  explain why something is the way it is, it cites the ADR.
* **History only where it carries a rule.** A closed incident or a fixed defect earns a few
  sentences if and only if it explains why a current rule reads the way it does, written as prose in
  the past tense with its date. Not as a changelog.
* **Security notes only where a real security relation exists.** A note that a value is
  "security-relevant" without saying to whom, against what, and with what consequence is noise, and
  it teaches readers to skip the notes that matter.
* **Dates are absolute**, enumerations of five or more parallel items are tables, long repetitive
  reference material goes inside a collapsible `<details>` block, links are relative, and the
  language is English regardless of the language the work was discussed in.

## How this directory grows

**Add a perspective by adding a page.** If a new subject fits an existing page's title it belongs on
that page; if it would need the title widened to fit, it is a new page. A page whose name stops
predicting its content stops being findable, and that is the whole cost of getting this wrong.

**A page that has grown past one sitting holds two perspectives — split it.** The point of one page
per perspective is that somebody finishes it; a document nobody finishes is a document nobody
checks, and an unchecked security page is worse than none because it looks like assurance.

Adding, splitting or removing a page changes nothing in this file. That is the property the missing
index buys.

## The other homes

* [`docs/adr/`](../adr/README.md) — the decisions themselves: what the product does, why, what was
  rejected and what it costs. Every rule stated on a page here is cited back to a record there.
* [`docs/developer/`](../developer/README.md) — how the mechanisms are implemented, for somebody
  about to change one.
* [`docs/operations/`](../operations/README.md) — what somebody running or integrating the operator
  needs: configuration, deployment, deletion, outages, monitoring.
* [`docs/tickets/`](../tickets/README.md) — work still outstanding, and the only place a ticket may
  be named.
* [`README.md`](../../README.md) — the front page and the single complete reference for the CRD and
  the chart values.

* [`SECURITY.md`](../../SECURITY.md) — **vulnerability reporting**: how to report a suspected
  vulnerability in this operator, what to include, and what response to expect. That is a different
  thing from this directory, which is the security **design**. If you have found a flaw, go there;
  if you want to know what the product defends against, stay here.
