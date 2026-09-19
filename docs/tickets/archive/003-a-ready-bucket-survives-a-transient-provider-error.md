# Ticket: stackit-s3-provisioner — do not drop bucket Ready state on transient provider errors

**Target project:** stackit-s3-provisioner (not this repo; CRD group `stackit-bucket.gtrfc.com/v1`, deployed on mgmt clusters in namespace `storage-provisioner` via HelmRepository `stackit-s3-provisioner`, chart version 1.10.19 on mgmt-p as of 2026-08-25)

## Problem

When the STACKIT provider API is temporarily unreachable or misbehaving, the
provisioner flips already-provisioned Buckets from `Ready` to a non-ready
phase (`InProgress`) on the very first failed reconcile. The bucket itself is
fine — only the control-plane call failed.

Observed on mgmt-p, 2026-08-25 (provisioner pod logs, namespace
`storage-provisioner`):

1. **08:13–08:15 UTC:** STACKIT API returned `403 Forbidden` as an nginx HTML
   page (SDK error: `undefined response type, status code 403`) on
   `enable object storage in project 30093b22-862a-4c07-a49b-753bbea5f6a9`.
   342 reconcile errors in ~2 minutes; **all 19 Buckets** on the cluster went
   non-ready simultaneously.
2. **10:37 UTC:** single `ensure bucket: unexpected EOF` on `gitlab/git-lfs`;
   one reconcile failure, bucket non-ready until the next successful
   reconcile (~10 min later).

Downstream effect: Flux Kustomizations health-check the Bucket resources
(kstatus, 5m timeout). A 2-minute provider blip therefore marked ~11
tenant-platform Kustomizations `Ready=False` and fired the
`FluxResourceNotReady` alert for gitlab, harbor, keycloak, vaultwarden,
guided-ssh, dependency-track, coder and their `*-config` Kustomizations.
The provisioner is the amplifier: transient provider error → all buckets
non-ready → cluster-wide alert storm.

## Desired behavior

A Bucket that has been successfully provisioned must keep its `Ready` state
(phase and Ready condition) when a reconcile fails for a *transient* reason.

1. **Classify errors.** Transient at minimum: network errors (`unexpected
   EOF`, connection refused/reset, timeouts), HTTP 5xx, and non-API gateway
   responses (the SDK `undefined response type` case — nginx/WAF HTML instead
   of JSON, regardless of status code). Definitive (may change state):
   structured API responses saying the bucket does not exist, credentials are
   invalid, or the request is semantically rejected (proper JSON 4xx from the
   STACKIT API).
2. **On transient error for an already-Ready Bucket:** keep phase `Ready` and
   the Ready condition `True`; emit the Warning event as today; requeue with
   backoff. Optionally surface the degradation in a *separate* condition
   (e.g. `ProviderReachable=False` with the error message) so it stays
   observable without breaking kstatus health checks.
3. **On transient error during initial provisioning** (never been Ready):
   current behavior is correct — stay `InProgress`.
4. Optional hardening, same root incident: the reconcile loop calls
   `enable object storage` on the project for **every Bucket on every
   reconcile**. Cache/skip this call once the project is known to be enabled
   (it cannot be un-enabled underneath us without everything else breaking
   too). This alone would have reduced the 08:13 incident from 342 errors to
   near zero, since `ensure bucket` against the S3 endpoint itself was not
   failing.

Tradeoff to state in the implementation: with (2), a genuinely deleted-behind
-our-back bucket stays `Ready` until a reconcile gets a definitive answer.
Acceptable — the provider answering "bucket gone" is a structured response
and falls under "definitive".

## Files

Provisioner repo not available from here — locate the bucket controller
(`controller: "bucket"`, kind `Bucket`, group `stackit-bucket.gtrfc.com`):
the reconcile error paths for `ensure bucket`, `ensure bucket policy`, and
`enable object storage`, plus the status writer that sets phase/conditions.

## Verification

- Unit tests: reconcile of a Ready Bucket with a mocked transport returning
  (a) `unexpected EOF`, (b) 403 with HTML body, (c) 503 → phase stays
  `Ready`, Ready condition stays `True`, requeue scheduled.
- Unit test: reconcile of a Ready Bucket with structured API "bucket not
  found" → state may change (existing behavior).
- On a dev cluster: block egress to the STACKIT API (network policy) for
  5+ minutes → no Bucket leaves `Ready`, no Flux Kustomization goes
  `Ready=False`, `FluxResourceNotReady` stays silent.

## Context references (mgmt-p, read-only findings 2026-08-25)

- Alert expr: `kube_customresource_status_condition{customresource_kind=~"Kustomization|HelmRelease", type="Ready", status!="True"}`, `for: 5m`, severity warning.
- 14d Prometheus history: alert fired almost exclusively during the 08:08–08:24 UTC burst; plus one benign firing on 2026-08-22 (initial provisioning of `coder-postgres-backup` took >5m).
