# ADR 0005: The Operator Serves One Project in One Region

## Status

Accepted. Date: 2026-09-19. The deployment model itself was decided on 2026-06-30, before any code
existed; this record states it as a rule and is written after the fact, so the alternatives below are
reconstructed from the feasibility findings of that date rather than from a design discussion.

Implemented and in use: the project comes from the service-account key, the region is an install-time
setting defaulting to `eu01`, a `Bucket` whose `spec.region` disagrees with the operator's region is
parked as a configuration fault, and an operator started without a key runs in skeleton mode and
makes no provider call. Four items are open and all four are named under Residual risks: the key is
read once at process start, so replacing it needs a restart; nothing declares or checks which project
a deployment is expected to serve, so the binding is whatever the mounted key names; the exact
minimal project-scoped role the service account should hold is still unknown, so deployments today
are granted a broader role than the decision wants; and whether bucket names are unique per project
or shared across the projects of a region was never answered.

## Context

The product exists to hand several Kubernetes clusters their own object storage without letting any
of them see or touch another's. That requirement was the starting point on 2026-06-30, and the answer
the provider offers is blunt: object storage is strictly project-bound. Every control-plane call
carries a project id and a region, and a service-account token carries roles only inside its own
project. The provider's own documentation says that separating access to data means creating separate
projects:

> "If you need to separate the access to the data on the object storage for different users you would
> need to create multiple projects."

That sentence is the one external, citable statement the whole deployment model rests on. It was
recorded verbatim from the STACKIT object storage documentation on 2026-06-30, when the feasibility
work was done, and has not been re-read since; the wording published today may differ.

This was measured, not assumed. On 2026-06-30 two service accounts in two projects of the same
organisation were pointed at each other against the real API. Listing the other project's buckets
failed with HTTP 403 in both directions; creating a bucket in the other project failed with 403 in
both directions; deleting the other project's bucket failed with 403 and the victim's bucket
survived. The separation is therefore produced by the provider's authorisation, before any operator
logic runs — which makes it the strongest guarantee in the system, and one that no bug in this
codebase can weaken.

That guarantee has a precondition, and the precondition is what turns a deployment topology into an
architectural rule. It holds only while the process is in possession of exactly one project's
credential and no role reaches across projects. An organisation-level role assigned to a
service account cascades into every project underneath it and dissolves the boundary silently:
nothing in the operator would report it, and every cross-project call would simply start succeeding.
The same logic applies to the shape of the operator itself. A process holding credentials for several
projects would replace a boundary enforced by the provider with a boundary enforced by this code — a
downgrade that no amount of testing makes up for, because the failure mode is one bad name resolution
away and produces no error.

Region is a smaller question with the same shape. Every control-plane call is region-scoped, so the
code is region-parameterised throughout and multi-region support looks nearly free. It is not free,
because region is not only a routing input. The operator holds one bootstrap S3 admin credential per
binding ([ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md)), the physical
bucket name is composed and then frozen ([ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md)),
and the region is written into the workload's credentials Secret under `S3_REGION`, where it is
consumed as the SigV4 signing region. A per-`Bucket` region selector therefore adds an admin
credential per region, makes region part of a bucket's identity, and lets an edit to `spec.region`
on a provisioned `Bucket` silently re-point it at a different physical bucket. The decision on
2026-06-30 was `eu01`, single-region for v1, with the code kept region-parameterised so the value is
configuration and not a constant.

Which leaves `spec.region` needing a meaning. The field exists on the CR, is shown in the `REGION`
print column, and lands in the workload Secret. Ignoring it would mean a `Bucket` asking for one
region gets provisioned in another and is handed a Secret whose `S3_REGION` is a lie — a lie that
surfaces later as a signature failure in somebody else's application. So the field cannot be
decoration, and with one region per deployment it cannot be a selector either.

Finally, the operator has to be installable with no cloud credential at all: the chart and the CRD
are installed and exercised in test clusters that have no service account, and a CRD-only install
must not crash-loop. That is what skeleton mode is for, and it is dangerous in exactly one way — a
cluster in skeleton mode looks entirely healthy while provisioning nothing.

## Decision

**D1 — One operator deployment serves exactly one project, and the binding comes from the key.**
The project id is read from the `projectId` field of the service-account key the operator is given.
There is no project setting, and no `Bucket` field selects a project. The credential decides which
project the deployment serves, which is what keeps cross-project separation a property of the
provider's authorisation rather than of this code.

**D2 — One operator deployment provisions in exactly one region, chosen at install time.**
The region is operator-wide configuration, not per resource.

| Binding | Set by | Value |
| --- | --- | --- |
| Project | `--stackit-sa-key-path` / `STACKIT_SERVICE_ACCOUNT_KEY_PATH`, chart `stackit.serviceAccountKey.secretName` + `stackit.serviceAccountKey.secretKey` | the key file's own `projectId`; no default |
| Region | `--stackit-region` / `STACKIT_REGION`, chart `stackit.region` | `eu01` &nbsp;`# default` |
| Bucket's declared region | `spec.region` on the `Bucket` | `eu01` &nbsp;`# default`, applied by the CRD |

**D3 — `spec.region` declares, it does not select.** A `Bucket` whose effective region differs from
the operator's region is a configuration fault of that CR, not a provider problem and not a routing
instruction. The operator refuses to provision it, records `Ready=False` with reason `Failed` and a
message naming both regions, and makes no provider call for that CR. Correcting the CR — or applying
it to the cluster whose operator serves that region — lets the next generation reconcile.

**D4 — A region mismatch is definitive and never retried.** It is a statement about this CR that the
operator established locally, so it drops readiness at once and is not requeued on a backoff, on the
same footing as the other definitive faults in
[ADR 0012](0012-ready-describes-the-last-verified-state.md). Nothing already provisioned is deleted,
renamed or reassigned by the refusal.

**D5 — Cross-project separation is a property of the credential, and the service account is granted
roles only at project level.** An organisation-level role, or any role that cascades into a second
project, breaks the separation invisibly and is forbidden regardless of convenience. The operator
neither detects nor compensates for such a grant, and must never be presented as the thing that
enforces the boundary.

**D6 — Without a service-account key the operator runs, and provisions nothing.** In skeleton mode
every `Bucket` reconciles to phase `Pending` with `Ready=False`, reason `NotImplemented`, and no call
is made to the provider in either direction — neither provisioning nor teardown. The state is
published as `stackit_s3_provisioner_skeleton_mode` so that a cluster which silently provisions
nothing is an alertable condition rather than a discovery.

**D7 — A configured key that cannot be used is a startup failure, not a degraded run.** Skeleton mode
is entered only when no key path is configured at all. A key path that cannot be read, or whose JSON
carries no `projectId`, aborts startup. The operator never falls back from "should provision" to
"provisions nothing".

**D8 — The binding is fixed for the lifetime of the process.** Project, credential and region are
resolved at start. Replacing the key or changing the region takes effect on the next restart, and
until then the running process keeps serving the binding it started with.

**D9 — A second project or a second region means a second deployment, and two deployments never share
a project.** Each deployment carries its own key, its own region, its own operator namespace and its
own bootstrap admin credential. Two operators bound to the same project contend for the same
project-wide admin identity, which is not a supported configuration.

## Consequences

Cross-project isolation costs nothing to maintain and cannot be regressed by a change in this
repository; it is bought entirely at account-setup time, by how the service account is granted. That
also means the most dangerous mistake in the whole system is made in a console this code cannot see,
which is why it is written down as a rule here and repeated in the deployment prerequisites.

Serving three clusters means running three operators and holding three keys. There is no single pane
of glass across projects, no fleet-wide view, and no way to move a `Bucket` from one project to
another by editing it. A team that wants a bucket in a second region files it against the operator
that serves that region.

`spec.region` is a field that can only ever hold one correct value per cluster, which reads as
redundant and occasionally is. It is kept because it makes the CR self-describing, feeds the
credentials Secret honestly, and turns a misrouted manifest into a parked CR with an explicit message
instead of a bucket quietly created in the wrong place.

A misconfigured region at install time is diagnosed one `Bucket` at a time. A wrong `stackit.region`
value does not fail the deployment; it produces a fleet of CRs stuck in `Failed`, each carrying the
same message. That is loud but late.

Skeleton mode means health probes stay green while nothing happens. `Bucket` CRs created in skeleton
mode still receive the operator's finalizer and, if deleted while still in skeleton mode, have it
removed with no teardown — correct, because nothing was ever created, and worth knowing, because a
cluster that acquires a key later has no record of what was requested and dropped while it had none.

The rule against two deployments on one project has teeth. Both would bootstrap the same project-wide
admin identity, and the bootstrap clears that identity's existing keys before minting a fresh one, so
the second install destroys the first operator's admin credential — and with it, for that operator,
policy writes, emptiness checks and therefore deletion. Verified by reading the bootstrap path, not
by an experiment.

## Alternatives Considered

**Make the region a per-`Bucket` selector and serve several regions from one deployment.** Rejected,
and this is the option that looked cheapest and lost anyway. Every control-plane call is already
region-scoped and the client is already region-parameterised, so the change reads as plumbing. It is
not: the bootstrap admin credential is one per binding, so each region adds another admin credential
to hold and to lose — and it was never established whether two regions of the same project would get
separate admin identities at all or contend for a single one, which is the open question Residual
risks records; the physical bucket name is frozen at first provisioning without a region component,
so the same name in two regions is two different buckets under one identity; and
`spec.region` would become part of a `Bucket`'s identity, meaning an edit to it on a provisioned CR
re-points a workload at a different bucket with no rename and no warning. Single-region is a
restriction that can be lifted later with a migration; the identity confusion could not be untangled
afterwards.

**Hold several projects' keys in one deployment and select the project per `Bucket`.** Rejected.
Cross-project separation is currently enforced by the provider — measured as HTTP 403 in both
directions on 2026-06-30 — and a multi-project process would convert it into a separation enforced by
this operator's own resolution logic. That trade turns a guarantee no bug can break into one that a
single mistaken lookup can, in exchange for running one pod instead of three.

**Ignore `spec.region` entirely, or drop the field from the CRD.** Rejected. The region is written
into the workload's credentials Secret under `S3_REGION` and consumed there as the SigV4 signing
region, so silently provisioning an `eu01` bucket for a CR that asked for another region ships a
Secret that is wrong in a way the workload discovers as a signature failure. Dropping the field
instead would make the CR unable to state where its data lives, which a manifest in Git has to be
able to say.

**Treat a region mismatch as a transient failure and retry it.** Rejected. No retry changes a
declared region, so a backoff would only produce a permanent stream of reconcile errors and provider
calls for a CR that can never succeed, and would bury real outages under it. Parking the CR keeps the
error loud in `status` and silent in the error rate.

**Refuse to start without a service-account key.** Rejected. The chart, the CRD and the RBAC are
installed and exercised in clusters that deliberately have no cloud credential, and a crash-looping
operator makes that path unusable and a dry-run install alarming. The real cost of the alternative
chosen — a healthy-looking operator that provisions nothing — is paid down with an explicit metric and
a critical alert on it rather than by removing the mode.

**Validate the operator's region against a list of known regions at startup.** Not done. Any string
is accepted, which keeps a new provider region usable without a release; a typo surfaces as failing
provider calls rather than a startup error.

## Residual risks

Replacing the service-account key requires restarting the operator, because the key is read once at
start. A key revoked out of band leaves the process authenticating with a dead credential until it is
restarted, and that failure arrives as a structured 400 from the token endpoint rather than the
authorisation error one would expect.

Nothing pins the project a deployment is expected to serve. The project is whatever the mounted key
names, so mounting the wrong key moves an entire cluster's provisioning into a different project
without any configuration looking wrong. There is no setting to declare the expected project id and
no check that the key matches it.

The exact minimal project-scoped role the service account needs is still unknown, so deployments are
granted a broader role than this decision wants. The requirement is only that the role stays scoped to
the project; least privilege inside it is outstanding.

Whether physical bucket names are unique per project or shared across projects within a region was
never established with the provider. Under a single-project, single-region binding it does not change
behaviour today, but it decides whether a name collision between two tenants is possible at all, and
it is the open question behind the composed-name policy in
[ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md).

**Not verified.** Whether a region string that is not a real provider region is rejected at startup by
the SDK or only fails at the first call, and what that failure looks like — the operator performs no
validation and the case was not tested. Whether credentials groups are scoped per region or per
project, and therefore whether two deployments serving different regions of the same project would
contend for the same bootstrap admin identity the way two deployments in one region do; the
contention itself was established by reading the bootstrap path, not by running it. The 2026-06-30
cross-project measurements were made against the two feasibility projects of that date, whose service
accounts have since been replaced; they have not been repeated against the current accounts.

## References

* [ADR 0003](0003-workloads-are-isolated-by-an-explicit-deny-policy.md) — the isolation inside the one
  project this record binds the operator to, which is the layer that is not free
* [ADR 0004](0004-the-operator-bootstraps-its-own-s3-admin-credential.md) — the bootstrap admin
  credential that exists once per binding and is why two deployments must not share a project
* [ADR 0009](0009-the-physical-bucket-name-is-composed-and-then-frozen.md) — why region is not part of
  a bucket's identity, and the open name-uniqueness question
* [ADR 0012](0012-ready-describes-the-last-verified-state.md) — the classification a region mismatch
  falls under as a definitive, locally established fault
* [docs/operations/prerequisites.md](../operations/prerequisites.md) — the account setup this decision
  depends on, including the project-level-roles-only rule
* [docs/operations/deployment.md](../operations/deployment.md) — running one deployment per project and
  supplying the key
* [docs/operations/configuration.md](../operations/configuration.md) — what the region and key settings
  do and when a restart is needed
* [docs/operations/bucket-status.md](../operations/bucket-status.md) — reading a `Bucket` parked on a
  region mismatch or held in skeleton mode
* [docs/operations/monitoring.md](../operations/monitoring.md) — the skeleton-mode metric and its alert
* [docs/security/tenancy-and-isolation.md](../security/tenancy-and-isolation.md) — what this binding
  defends against and what it does not
