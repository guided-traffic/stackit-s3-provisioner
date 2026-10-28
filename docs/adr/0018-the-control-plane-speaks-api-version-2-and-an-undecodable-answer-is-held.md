# ADR 0018: The Control Plane Speaks API Version 2, and an Answer the Client Cannot Decode Is Held, Not Judged

## Status

Accepted. Date: 2026-10-28.

Implemented: the operator builds its control-plane client on the provider SDK's package for Object
Storage API version 2, configures no region on that client, and treats a response it cannot decode as
a failure to verify. Verified against the real provider on 2026-10-28 for every control-plane
operation the operator calls except enabling the service, which the test project never needed (see
*Residual risks*).

## Context

The operator talks to the provider's Object Storage control plane through the provider's Go SDK. Until
2026-10-28 it used the SDK's top-level package, which the provider had marked deprecated with a
removal date of 2026-09-30 and replaced with one package per API version. SDK release v1.12.0 removed
the top-level package, and the dependency update that would have brought it in stopped compiling. A
pinned old release keeps compiling forever, so the date was never an outage; what it ended was the
ability to take any later SDK release — a security fix, a new region, a changed contract — as a
version bump instead of a migration.

Two replacement packages exist, and they are not two editions of the same thing. The one generated
for API version 2 speaks exactly what the removed package spoke: the same base URL and the same
request paths, `/v2/project/{projectId}/regions/{region}/…`, compared path by path. The one generated
for API version 1 speaks `/v1/project/{projectId}/…`, carries no region in any operation and puts it
into the host name instead. Its name reads like "the stable one"; it is the older API.

The version-2 package is also stricter than the package it replaces, in two ways that matter here.
It refuses a client configured with a region, because its server definition is region-agnostic and
the region travels with each request. And it validates every response against the properties the API
declares required: a success response missing one — a service-status answer without `scope`, a
key-deletion answer without `keyId` — comes back as an error carrying the success status. The removed
package accepted both. Neither failure is visible to an offline suite: the simulated provider is
reached through a custom endpoint, which skips the region check, and it answers with whatever shapes
it was written to answer.

## Decision

**D1 — The control plane is spoken in the provider's Object Storage API version 2, through the SDK
package generated for that version.** Every operation carries the region as a request parameter,
which is the per-call region [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) D2
rests on; the region is never configured on the client itself. The package for API version 1 is not
used. Moving the control plane to another API version amends this record; it is not a dependency
update.

**D2 — A response the client cannot decode is a failure to verify, never a verdict.** A success
status whose body lacks a property the API declares required is treated like every other unrecognised
failure under [ADR 0012](0012-ready-describes-the-last-verified-state.md) D3: it is non-definitive, a
`Ready` bucket is held through the grace, and it counts toward the fleet-wide trip of
[ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) D3. It has no classification of its own, and
it never leads to a write that only a definitive answer may authorise — enabling the service stays
reserved for the provider's structured "not enabled" answer.

**D3 — A change of the API version or of the SDK package the control plane is built on is verified
against the real provider before it lands.** The offline suite cannot see either of the two ways the
version-2 package differs from its predecessor, and no simulation can prove what shapes the real
provider sends. A version bump within the same package is not such a change.

## Consequences

The wire protocol did not change. Every request the operator sends is byte-for-byte the request it
sent before, so the provider sees nothing of this decision.

The client now enforces the API's contract on the provider's answers. If the provider stops sending a
field it declares required, every reconcile that reads that answer fails; under D2 the fleet is held,
the breaker opens, and dashboards show what looks like a provider outage. Nothing in the condition
reason or the metrics says the cause is in the client — only the error text, which names the missing
property. That is accepted as the price of a client that refuses to act on an answer it cannot read.

The same validation runs on error bodies. The authorization layer's `401`/`403` carries no `detail`
property, so its text in `status.message` and in the `Failed` event now begins with
`no value given for required property detail` before the status code and the body. The classification
of that refusal as definitive is unchanged, because it reads the status and the raw body, not the
text. Measured against the real provider on 2026-10-28.

The simulated provider has to answer every control-plane call with a complete response, or the
offline suite fails at the first service-status read. That makes the simulation a little more work to
extend and much harder to let drift from the real shapes.

## Alternatives Considered

**Stay on the last release that still had the top-level package.** No code change at all. Rejected:
it freezes the SDK, so every later fix arrives as this same migration, under whatever time pressure
the fix brings.

**Move to the API version 1 package.** It exists in the same module and is equally supported.
Rejected: it removes the region from every operation and binds it to the host, which retires the
per-call region of [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) D2 and changes
which API the operator speaks — a product decision presented as a dependency choice.

**Give an undecodable response a named classification of its own** — still non-definitive, but with a
distinct condition reason or metric so the signal points at the client. Rejected for now: it amends
[ADR 0012](0012-ready-describes-the-last-verified-state.md) for a failure that has never been observed
against the real provider, and the error text already names the missing property. It is the obvious
amendment if the failure is ever observed live.

**Use the mock of the API interface the new package generates instead of the wire-level simulation.**
Less code to maintain. Rejected: error classification tells a provider's refusal from an
intermediary's page only by the raw body, and a Go-level mock has no body to tell apart.

## Residual risks

- A field the provider stops sending presents as a provider outage, as described under
  *Consequences*.
- A human reading a `401`/`403` in `status.message` sees a misleading prefix before the status code.

**Not verified:**

- The provider's answer to enabling the service. The test project has Object Storage enabled, so the
  call was never made against the real provider after the move; whether that answer carries `scope`
  is unknown. An installation in a project without Object Storage enabled is where it would surface,
  on the first reconcile, as a held failure to verify under D2.
- Every other control-plane operation the operator calls was exercised against the real provider on
  2026-10-28 and decoded cleanly. That is one sample, and it says nothing about responses the provider
  sends only in states the test runs did not reach.

## References

- [ADR 0005](0005-the-operator-serves-one-project-in-one-region.md) — the per-call region D1 keeps
- [ADR 0012](0012-ready-describes-the-last-verified-state.md) — D3, where an undecodable answer lands
- [ADR 0013](0013-a-provider-outage-is-held-fleet-wide.md) — D3, why such a failure trips the breaker
- [docs/developer/stackit-api.md](../developer/stackit-api.md#the-control-plane-package) — which
  package, how it differs, and the two pitfalls it brings
- [docs/developer/provider-errors.md](../developer/provider-errors.md#a-response-the-client-cannot-decode) —
  how an undecodable answer is classified, and the error-text effect
- [docs/developer/testing.md](../developer/testing.md#the-provider-fake) — the simulation's
  complete-response rule
