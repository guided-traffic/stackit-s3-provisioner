# Security

This file is about **reporting a suspected vulnerability** in the StackIT S3 Provisioner: where to
send it, what to put in it, and what happens afterwards. It is deliberately not the security design —
what the operator defends against and what it leaves open lives in
[`docs/security/`](docs/security/README.md), and the last section here says so plainly.

This repository builds two artefacts: the operator image and the Helm chart that installs it. A flaw
in StackIT Object Storage itself, rather than in this operator, belongs to StackIT and not here.

## How to report

**Use GitHub private vulnerability reporting on this repository**: open the
[Security tab](https://github.com/guided-traffic/stackit-s3-provisioner/security) and choose
*Report a vulnerability*. The report is visible only to the repository maintainers until an advisory
is published.

**Never report a vulnerability in a public issue, a pull request, a commit message or a discussion.**
This repository is public (verified 2026-09-19), its issue tracker is open to everyone, and a
description filed there is disclosure — it reaches an attacker at the same moment it reaches a
maintainer, on a codebase that mints and holds S3 credentials.

Private vulnerability reporting is a repository setting that has to be switched on by an
administrator under *Settings → Security → Private vulnerability reporting*, and it was **not
enabled** when this file was last checked (verified against the GitHub API on 2026-09-19, which
answered `{"enabled":false}`). If the Security tab offers you no *Report a vulnerability* button, the
setting is still off: open an issue that asks for a private channel and **contains no detail about
the finding** — not the affected component, not the symptom — and wait for a maintainer to reply.

## What to include

A report is triaged by whoever can reproduce it fastest. These are the things that decide that.

| Include | Why it matters, and how to get it |
| --- | --- |
| **Operator version** | `kubectl get bucket <name> -n <ns> -o jsonpath='{.status.operatorVersion}'` — the operator stamps it into every `Bucket` it reconciles; it is also in the deployment image tag and in the `starting stackit-s3-provisioner` log line at startup. Example value: `1.15.1` &nbsp;`# example` |
| **Chart version** | `helm list -n <ns>` — the `CHART` column. Report it alongside the operator version, and say whether `image.tag` was overridden: the chart lets a deployment run an image tag that differs from its chart version, so the two numbers are not always the same build. |
| **Which credential is involved** | Name the credential the finding concerns rather than describing it as "a key" — the operator holds and hands out several, with different blast radii, and they are listed and compared in [credentials-and-secrets.md](docs/security/credentials-and-secrets.md). **Send no credential material** — no private key, no access key, no secret half of a key pair; redact it and say what it was. |
| **The `Bucket` spec that triggers it** | The full CR as applied, with `metadata.name` and the namespace, since several behaviours depend on both. The spec itself carries no secret, but it names Secrets by reference — do not attach the contents of those Secrets. |
| **Reproduction steps** | What you ran, what you expected, what happened, and whether the cluster had a service-account key configured (without one the operator makes no cloud call at all, which changes what a finding means). Logs and events help; redact bucket names, project ids and endpoints if you consider them sensitive. |

If a proof of concept touches real object storage, please say so explicitly and describe the blast
radius you observed, rather than attaching data you were able to read.

## What to expect

This project is maintained by Guided Traffic (verified 2026-09-19: the chart's `maintainers` entry
and the image's `org.opencontainers.image.vendor` label both name it). There is **no published
response time, no security on-call rotation and no bug bounty** — nothing in this repository commits
to one, and this file does not invent one (verified 2026-09-19: no service-level statement exists in
the repository, the workflows or the chart).

What follows is the **intent**, stated as intent and not as a promise: a report is acknowledged once
a maintainer has read it; it is then reproduced or questioned in the same private thread; a confirmed
finding is fixed on `main` and shipped as a release, and the reporter is credited in the advisory
unless they ask not to be. Where a fix needs a workaround in the meantime — a chart value, a
temporary revocation — that goes into the same thread before the fix ships.

If a report goes unanswered for longer than you find reasonable, say so in the thread. Silence here
means a maintainer has not seen it, not that the finding was judged unimportant.

## Supported versions

What can be verified from the release configuration (`.releaserc.json` and
`.github/workflows/build.yml`, read on 2026-09-19):

* **There is one release line.** semantic-release is configured for the branch `main` only, and the
  repository carries no maintenance or back-port branches. Every fix therefore lands on `main` and
  ships as the next release from it.
* **A release writes one number into three places.** The release workflow writes the release tag
  into the chart version, the chart app version and the chart's default image tag, so "operator
  `X.Y.Z`" and "chart `X.Y.Z`" describe the same build unless a deployment overrides `image.tag`,
  which the chart supports.
* **Older charts stay downloadable.** The published Helm repository keeps every previously released
  package, so an old version remaining installable is not a statement that it is maintained.
* **The most recent release as of 2026-09-19 is `v1.15.1`** (published 2026-09-04) &nbsp;`# example` —
  this line ages; the
  [releases page](https://github.com/guided-traffic/stackit-s3-provisioner/releases) is authoritative.

**What could not be determined:** nothing in this repository defines how far back a release is fixed.
There is no support window, no long-term-support line and no policy for patching a previous minor.
Rather than inventing one: assume a fix reaches you only by upgrading to the newest release, and if
you need something else, say so in the report and it becomes part of the conversation.

## The security design is a different document

This file tells you how to report a flaw. [`docs/security/`](docs/security/README.md) tells you what
the operator is built to defend against and, on every page, what it does not — one page per
perspective: tenancy and isolation, credentials and Secrets, RBAC and privilege, ownership and
attribution. That directory keeps no index on purpose, so start at its README and read the file
names.

The decisions behind all of it are recorded as [ADRs](docs/adr/README.md); a security page states a
rule and cites the record that decided it.
