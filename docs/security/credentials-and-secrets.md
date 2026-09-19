# Credentials and secrets

This page follows every credential this operator holds, mints or hands out: where it lives, who
can read it, what it is worth to somebody who takes it, and what its loss or destruction costs.
It covers the service-account key the operator authenticates to the cloud with, the S3 admin
credential it mints for itself, the workload credential it publishes per `Bucket`, and the two
credentials a clone brings together. It does **not** explain what those credentials are allowed
to *do* once used — the bucket policy, the project boundary and what separates one tenant's data
from another's are in [tenancy-and-isolation.md](tenancy-and-isolation.md); who inside the
cluster may reach these Secrets through Kubernetes RBAC is in
[rbac-and-privilege.md](rbac-and-privilege.md).

## The four credentials of this system

| Credential | Where it lives | Minted by | Scope of what it can do | Rotatable by this operator |
|---|---|---|---|---|
| STACKIT service-account key | a Secret in the operator namespace, mounted read-only into the pod | a human, in the cloud console | the whole project's **control plane**: create and delete buckets, credentials groups and access keys | no — replaced out of band, takes effect on restart |
| Operator S3 admin credential | the operator-owned Secret in the operator namespace | the operator itself, on first need | the whole project's **data plane**: exempt from the deny statement of every bucket policy the operator writes | only by deleting the Secret and restarting ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9) |
| Workload credential | the Secret named by `spec.secretRef.name`, in the `Bucket`'s own namespace | the operator, per `Bucket` | object operations inside that one bucket, plus read on buckets that grant it | yes, on request ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D8) |
| Clone source credential | a Secret the CR author supplies in the `Bucket`'s namespace, copied into a staging Secret in the operator namespace for the duration of the copy | whoever owns the source endpoint | whatever the foreign endpoint grants it | no — it is not this operator's credential |

The two project-wide credentials are the ones worth protecting hardest, and they belong to
different planes. The service-account key is a control-plane bearer identity: it cannot touch a
single object or write a single bucket policy, because there is no control-plane route to
`PutBucketPolicy` at all. That is the whole reason the admin credential has to exist — the reasoning
is in [ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md), under
*Context*. The converse is weaker than it looks, and stating it precisely matters: the operator
never uses the admin credential to create or delete a bucket, because those are control-plane calls
it makes with the service-account key. Whether the provider would *refuse* the admin credential such
a call is not verified here, and this is the gap — the provider's default inside one project is
open, which is the premise the whole bootstrap rests on, and `BuildIsolationPolicy` in
[stackit/s3.go](../../stackit/s3.go) confines only the workload principal while exempting the admin
URN from the blanket deny. Nothing in this repository measures an upper bound on what the admin
credential may do on a bucket of the project, so treat it as the broader of the two on the data
plane, not as a narrower counterpart to the service-account key.

## The service-account key is the project's master credential

The key is a JSON document carrying an RSA **private key** inline, under
`credentials.privateKey`. The operator parses only two fields out of it —
`projectId` and `credentials.iss` (`LoadAccount` in
[stackit/client.go](../../stackit/client.go)) — and hands the file path to the vendor SDK, which
reads the whole file and signs a JWT with the private key to obtain short-lived bearer tokens.
Possession of the file is possession of the project.

**How it reaches the process.** The chart does not create the Secret; it references one that must
already exist. `stackit.serviceAccountKey.secretName` (# default `""`) names it and
`stackit.serviceAccountKey.secretKey` (# default `sa-key.json`) names the data key; the Secret is
mounted read-only at `/etc/stackit/` and the path is passed as `--stackit-sa-key-path`
(see [deploy/helm/stackit-s3-provisioner/templates/deployment.yaml](../../deploy/helm/stackit-s3-provisioner/templates/deployment.yaml)).
Leaving `secretName` empty is a supported mode, not a misconfiguration: the operator then runs in
skeleton mode and makes no cloud call at all. Because the key arrives as a volume, the operator
never reads it through the API server, and nothing in the cluster has to grant it access to that
particular Secret.

**It is read once.** `LoadAccount` reads the file at process start, and the SDK's own
`auth.SetupAuth` does a single `os.ReadFile` of the path while the API client is being constructed
(verified in `core@v0.26.0/auth/auth.go`). The key material then lives in process memory for the
life of the process. Replacing the mounted Secret changes nothing until the pod restarts — which
is the same shape as the admin credential's cache and is recorded as an open item in
[ADR 0005](../adr/0005-the-operator-serves-one-project-in-one-region.md).

**Revocation is loud, and deliberately so.** A revoked or deleted key fails at the token endpoint,
not at the storage API: HTTP 400 with `{"error":"invalid_grant"}`, measured against the live
endpoint on 2026-08-25. The SDK stamps that status and body into the same error type it uses for
API errors, so the operator recognises it as a structured refusal (`ProviderRefused` in
[stackit/errors.go](../../stackit/errors.go)) and every `Bucket` drops `Ready` immediately instead
of being held through the degraded grace window
([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D6). That exception exists for
exactly this case: a fleet reporting `Ready` while every credential it holds is dead is the one
degradation that must never be masked.

**In this repository the key files are real.** `account-1.json` and `account-2.json` in the working
tree are live service-account keys used by the integration and cloud end-to-end suites
(`SA_KEY` in the [Makefile](../../Makefile), `stackit/client_test.go`,
`internal/controller/attribution_integration_test.go`). Two independent one-line patterns keep them
out of published artefacts: `account-*.json` and `*.sa-key.json` in
[.gitignore](../../.gitignore), and the same two patterns in [.dockerignore](../../.dockerignore) —
the latter matters because the container build stage copies the whole working directory before
compiling. The published image is distroless and contains only the compiled binary, so a key that
slipped past `.dockerignore` would sit in a build-stage layer rather than the shipped one; that is
a smaller leak, not a safe one.

**A valid key for the wrong project is not a refusal, and it is now reported per `Bucket`.**
Pointing the operator at a key for a different project authenticates cleanly: every call succeeds,
and the operator's own buckets are simply not in the project it now serves. Each
already-provisioned `Bucket` then reports its bucket as gone — `Ready=False`, reason
`BucketMissing` — instead of being re-provisioned as an empty bucket with a fresh credentials group
and a fresh key, which is what the operator used to do to the whole fleet at once
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D1; what the
operator accepts as the provider's own answer, rather than as a failure to reach it, is in
[ownership-and-attribution.md](ownership-and-attribution.md)). No reconcile destroys anything while
the wrong key is in use, and the fleet recovers by itself once the right one is back in the process
— which takes a restart, see [H-12](#h-12--replacing-the-service-account-key-requires-a-restart).
That covers reconciles only: a `Bucket` *deleted* meanwhile still tears down, and its teardown reads
the bucket as absent through the same project listing, so it skips the empty check and the bucket
delete, removes the workload Secret and drops the finalizer, leaving the real bucket and its
credentials group standing in the project the operator is no longer authenticated against. The
other exception is a `Bucket` carrying `spec.allowRecreate`, which authorises exactly that
unattended rebuild: it creates a bucket in whichever project the current key names and overwrites
the workload Secret with credentials for it (D9, D10). Derived from reading the guard, not
exercised against a second project.

**In a cluster the key must be encrypted at rest in Git.** The README's GitOps path ships it as a
SOPS-encrypted Secret manifest alongside the `HelmRelease`; SealedSecrets or ExternalSecrets are
equally fine. What is not fine is a plain-text Secret manifest in a repository, because the RSA
private key is the whole project.

## The operator's S3 admin credential is a fleet-wide dependency

The operator mints one credentials group named `operator-admin` with exactly one access key in it,
and uses that key for every data-plane operation on the buckets it provisions: reading and writing
ownership tags, writing and healing the isolation policy, the emptiness check before a delete, the
opt-in wipe, the size listing, and the destination side of a clone
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D1/D2). It exists
because setting a bucket policy is an S3 call and the service-account identity has no route to one.

**Where it is stored.** One Secret in the operator's own namespace, named by
`--admin-credentials-secret-name` (# default `stackit-s3-provisioner-admin`), in the namespace from
`POD_NAMESPACE` or `--operator-namespace`. It carries four data keys and the label
`app.kubernetes.io/managed-by: stackit-s3-provisioner`, and **no owner reference**, so nothing
garbage-collects it:

| Data key | Content |
|---|---|
| `accessKeyID` | the S3 access key id |
| `secretAccessKey` | the S3 secret — returned by the provider only at creation time |
| `urn` | the URN of the `operator-admin` group, which every bucket policy exempts |
| `credentialsGroupID` | the group id, needed to delete a key in that group |

Only the first three decide whether the credential counts as complete; `credentialsGroupID` is read
but not required (`adminFromSecret` in
[internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go)). An
incomplete Secret triggers a full re-bootstrap, which clears **every** key in the `operator-admin`
group before minting a new one — sound only because
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D1 makes the group
the operator's exclusively, and destructive the moment a human puts a second key there by hand.

**Why a tenant cannot reach it.** A workload credentials Secret always lives in its `Bucket`'s
namespace and a `Bucket` cannot direct it elsewhere
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1/D4), so only a `Bucket`
*inside the operator namespace* could ever name the admin Secret. That case is refused twice:
`specGuardError` parks such a CR as a configuration fault without a requeue, and `teardown` refuses
to delete the admin Secret even if provisioning were somehow bypassed
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D8). Both halves
are needed — provisioning would overwrite the credential's data keys, and deletion would destroy
the credential for the whole fleet.

**What its loss costs.** Every path in the table under
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) *Consequences* stops
at once, and the one that hurts most is teardown: the emptiness guard is an S3 listing, so a dead
admin credential means **no `Bucket` can be deleted**. Measurement stops with it as well — `measure`
in [internal/controller/bucket_usage_controller.go](../../internal/controller/bucket_usage_controller.go)
builds its S3 client from the same credential. What a failed measurement does not do is touch
readiness: it is informational, returns no reconcile error and leaves the previous values in place
([ADR 0014](../adr/0014-bucket-size-is-measured-by-a-separate-controller.md) D3/D4).

**What its leak costs.** The admin group's URN sits in the `NotPrincipal` of the blanket-deny
statement of every bucket policy in the project, so the key is exempt from the isolation of every
bucket the operator manages. A copy of that Secret is read and write access to all of them,
including the ability to rewrite their policies. It is also not revocable by policy: a bucket
policy that denied the admin group would be an unrepairable lockout, because there is no
control-plane route to `PutBucketPolicy`
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D7). Revoking a
leaked admin key therefore means minting a new one in the same group — deleting the Secret and
restarting the process (D9) — never re-pointing the policies.

**Uninstall leaves it behind.** The chart does not create the Secret and the Secret has no owner
reference, so `helm uninstall` leaves both the Kubernetes Secret and the live cloud credential in
place. That is deliberate — a reinstall picks the same identity back up and every existing bucket
policy stays valid — but deleting the namespace without first deleting the cloud credential orphans
a key that still has project-wide data-plane standing.

## The workload credential lives in its Secret, and the Secret is the only copy

The provider returns the secret half of an access key exactly once, in the response that creates
it. That single fact decides the whole design: there is no read-back, no repair and no recovery,
so the Kubernetes Secret — not the provider — is the source of truth for the live credential
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D1).

**Where it lands.** Always in the `Bucket`'s own namespace, always with the `Bucket` as controller
owner, always with `type: Opaque` and the managed-by label. `spec.secretRef.name` picks only the
name; `spec.secretRef.namespace` does not exist and was removed on 2026-09-03 because it was a
cross-namespace Secret write-and-delete primitive for anyone who could create a `Bucket`. What
holds the absence today is structural rather than a code check: the CRD schema has no such property
and does not preserve unknown fields, so the API server prunes it, and every call site derives the
Secret's namespace from the CR's own
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1/D4).

**What the Secret contains.** Six logical fields, each with a default data key and each overridable
per `Bucket` under `spec.secretRef.keys`. The credential fields are the first two; the rest are
connection parameters so the Secret can be consumed directly via `envFrom`.

<details>
<summary>The six fields and their default data keys</summary>

| Logical field | Default data key | Value | Written when |
|---|---|---|---|
| `accessKeyID` | `AWS_ACCESS_KEY_ID` (# default) | S3 access key id | always |
| `secretAccessKey` | `AWS_SECRET_ACCESS_KEY` (# default) | S3 secret | always |
| `bucketName` | `S3_BUCKET` (# default) | the bucket name | always |
| `region` | `S3_REGION` (# default) | the operator's region | always |
| `endpoint` | `S3_ENDPOINT` (# default) | endpoint host, no scheme | only when non-empty |
| `bucketURL` | `S3_BUCKET_URL` (# default) | full path-style bucket URL | only when non-empty |

The resolution and the collision check are in `SecretKeys`, `Bucket.SecretData` and
`Bucket.ValidateSecretKeys` in [api/v1/bucket_types.go](../../api/v1/bucket_types.go). Two logical
fields resolving to the same data key parks the `Bucket` as a configuration fault **before**
anything is written, because the alternative is one value silently overwriting another
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D10).
The complete `Bucket` reference, including these fields, lives in the [README](../../README.md) and
is not restated here.

</details>

**Replacement clears before it creates.** When a pass finds no usable credential, it deletes every
access key in the `Bucket`'s credentials group *first* and creates the replacement *second*
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D5).
The intuitive order — create, then delete — is the wrong one, and the reason is a security property
rather than a tidiness one: a crash between create and delete leaves a live access key in the
project whose secret half nobody ever persisted, and which no later pass can tell apart from the
legitimate key. Such a key can only be cleaned up by deleting every key in the group. The chosen
order trades an unrecoverable leak for a bounded, visible outage. For the same reason a key whose
Secret write fails is deleted immediately (D6): its only copy was in the write that failed.

Clearing a group blind is only safe because a credentials group is attributed through the bucket
that owns it — its tag first, its own isolation policy second — and never by its display name
([ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1/D2). Until
2026-09-03 it was found by a derived display name with no ownership check, two namespace/name pairs
could derive the same name, and the colliding CR did not merely share the group — it deleted the
victim's live key and published a new one into its own Secret. That is why the attribution rule in
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D1/D2 and the
clear-before-create rule are read together: neither is safe without the other.

**Rotation is requested, hard, and level-triggered.** Setting the annotation
`stackit-bucket.gtrfc.com/rotate-credentials-at` to a value that differs from
`status.lastRotationTrigger` rotates the key exactly once; the operator records the handled value
and never mutates the annotation, so a GitOps controller re-applying the same manifest forever is a
no-op ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D8).
The comparison is against that single recorded value, not a history, so re-applying an older,
superseded value rotates again. There is no overlap window: the previous key is dead the moment it
is deleted, and nothing in Kubernetes pushes a changed Secret into a process that read it at start
(D9). Keys are issued without an expiry and the operator never rotates on its own (D3) — an
operator-chosen expiry is an operator-scheduled outage for every consumer that does not re-read.

**Anyone who can patch a `Bucket` can kill its live credential.** The trigger is an annotation, so
no spec change and no Secret access is needed. Inside a namespace that is a denial-of-service
primitive against that namespace's own workload, which is why `Bucket` write is only ever handed
out together with the Secret access that already implies it — see
[rbac-and-privilege.md](rbac-and-privilege.md).

**The Secret is merged into, and adopted.** The write merges the data keys into whatever Secret
carries that name, leaving unrelated keys untouched, and sets the `Bucket` as controller owner
(`upsertSecret`). If a Secret of that name already exists without a controller owner, it is adopted
silently — and removed with the CR, whole, including the keys the operator never wrote. If it
already has a *different* controller owner — two `Bucket` CRs in one namespace naming the same
Secret — the write fails and the pass burns a key on every retry, because the clear has already
happened by then. The blast radius of that churn is confined to the second CR's own group. Not
verified: what happens when the existing Secret is of a type other than `Opaque`; the write forces
`Opaque`, and Secret `type` is immutable, so the update is expected to fail permanently, but no
test covers it.

**Deleting the Secret costs a credential, not just a Secret.** A missing or emptied Secret is
indistinguishable from a never-provisioned one, so the next pass mints a replacement and the
previous credential dies. Restoring a backup does not undo that: the restored values are already
invalid if a pass ran in between. A backup of this Secret is a backup of a live credential and has
to be protected like one.

**Losing the bucket costs no credential — unless a rebuild is authorised.** When the provider
reports a provisioned bucket as gone, the pass stops before anything is provisioned, so the Secret
and the live key in it are left exactly as they are
([ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md) D1): the credential
outlives the bucket and addresses a name that no longer resolves. `spec.allowRecreate` buys
unattended recovery and pays for it here — the rebuilt bucket gets a fresh credentials group and a
fresh key, the Secret is overwritten, and until the workload re-reads it, it presents a key the new
bucket's policy does not name and fails with `403` (D10). That is the one credential replacement
this operator makes without being asked for it; it is event-driven and not a schedule, so the
no-time-based-rotation rule of
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D3
is untouched. The previous group is left standing with a live key that reaches nothing, its removal
is manual, and it accumulates — see
[H-23](ownership-and-attribution.md#h-23-a-bucket-deleted-out-of-band-orphans-a-keyed-credentials-group).

## A clone brings two credentials together, and neither may meet the other's namespace

A `Bucket` declaring `spec.cloneFrom` is seeded once by an rclone Job in the **operator** namespace
([ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D2). Two
credentials meet in that pod, and where they meet is the whole point:

| Side | Credential | Read from | Reaches the pod as |
|---|---|---|---|
| destination | the operator's S3 admin credential | the admin Secret in the operator namespace | environment variables `RCLONE_CONFIG_DST_ACCESS_KEY_ID` / `..._SECRET_ACCESS_KEY`, sourced straight from that Secret |
| source | the CR author's credential for the foreign endpoint | the Secret named by `spec.cloneFrom.secretRef`, in the `Bucket`'s namespace | environment variables `RCLONE_CONFIG_SRC_*`, sourced from the staging Secret |

The destination side must be the admin identity, because the bucket's own isolation policy is
already written by then and — under the default `holdSecretUntilCloned: true` — the workload
credential does not exist yet. Running the Job in the workload namespace would therefore mount the
project-wide admin credential where a namespace user can read it, which is why the Job runs where
it does and the copy's logs are invisible to the team that asked for it.

The source side is the one place in the whole operator where a credential that is not the admin
credential performs an S3 call
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D2). It is read
only from the `Bucket`'s own namespace, and `spec.cloneFrom.secretRef` carries no `namespace` field
on purpose: the operator holds cluster-wide Secret access, so a reference it followed on a CR
author's behalf would be a read the author could not perform themselves — a confused deputy
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D3/D4). The data-key names
default to `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (# default) and are overridable, which
is what makes a Secret this operator wrote for a sibling `Bucket` usable as a clone source
unchanged.

**The staging Secret** is a copy of the source credentials in the operator namespace, created
because a Job cannot mount a Secret across namespaces. It is named `<clone job name>-src`, carries
the managed-by and `app.kubernetes.io/component: clone` labels, has no owner reference, and also
holds the generated password for the copier's control port. It is deleted when the clone reaches
its terminal state and again during teardown (`deleteCloneArtifacts` in
[internal/controller/clone.go](../../internal/controller/clone.go)).

**The control port.** rclone is started with `--rc --rc-addr=:5572` and a basic-auth user
`operator` whose password is 32 hex characters from `crypto/rand`, generated once per clone and
kept stable across pod restarts. Two things guard it, and both have limits worth knowing. The
chart's NetworkPolicy (`clone.networkPolicy.enabled`, # default `true`) admits ingress to port 5572
only from the operator pod, and selecting the clone pods at all denies every other ingress to
them — but it is inert on a cluster whose CNI does not enforce NetworkPolicies. And the interface
behind that port is rclone's full remote-control API, which
[ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) records as able to
execute commands; the operator itself calls only `POST /core/stats`. That call is plain HTTP, so
the password crosses the pod network in the clear.

**A third-party image runs with the project-wide credential in its environment.** This is the
largest single cost in the design and is recorded as such. The image tag is pinned in the chart and
updated through dependency review, the pod runs as uid 65534 with a read-only root filesystem, no
privilege escalation and all capabilities dropped — none of which changes the fact that a
compromised release of that image is a compromise of the credential exempt in every bucket policy
in the project. An operator unwilling to accept that should not use `spec.cloneFrom`; nothing else
in this operator runs foreign code.

## What the cluster API exposes, and what it never does

The secret half of an access key is written to exactly one place and nowhere else. It is never put
in `status`, never in an event, never in a log line and never in a metric
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D12) —
verified by reading every use of the value in the tree: it leaves the process through exactly two
sinks, the workload Secret write built by `Bucket.SecretData` and the admin Secret write. Every
other use stays inside the process or stays a reference — the admin half is handed to
`stackit.NewS3Admin` by the provisioning reconciler and by the measuring controller, and the clone
Job receives it as a `secretKeyRef` into the admin Secret rather than as a literal
([internal/controller/clone.go](../../internal/controller/clone.go)). What a reader with
`get`/`list` on `buckets` sees instead:

| Surface | Value | Sensitivity |
|---|---|---|
| `status.accessKeyID` | the access key id of the live workload credential | public half only; useless without the secret |
| `status.credentialsGroupID` / `status.credentialsGroupURN` | the identifiers of the `Bucket`'s credentials group | identifiers, not capabilities |
| `status.lastRotationTrigger` / `status.lastRotationTime` | the annotation value last acted upon, and when | says that a rotation happened, not who asked |
| Event `CredentialsRotated` | one Normal event per performed rotation | — |
| Metric `stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds` | Unix time of the last rotation | absent for a `Bucket` that never rotated |

`status.credentialsGroupURN` being readable — and writable by anyone holding `buckets/status`
update — is why a read grant never takes a reader's URN from the referenced object's status. The
grantor resolves the reader through the grantee's *bucket* instead, and the policy builder filters
the admin and workload URNs out regardless of where the list came from
([ADR 0008](../adr/0008-a-read-grant-is-declared-by-the-bucket-that-owns-the-data.md) D3, applying
the attribution rule of
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D5; the attack
it closes is spelled out in the comment above `resolveReadGrants` in
[internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go)).

The operator's own ClusterRole holds cluster-wide `create`/`delete`/`get`/`list`/`patch`/`update`/`watch`
on `secrets`, because it must write a credentials Secret into every tenant namespace
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D6). What that means for
anybody who can reach the operator's ServiceAccount is the subject of
[rbac-and-privilege.md](rbac-and-privilege.md).

## What this does not cover

This page does not describe the bucket policy that decides what a workload credential may do with
its bucket, nor the project boundary beneath it — both are in
[tenancy-and-isolation.md](tenancy-and-isolation.md). It does not describe how the operator decides
a cloud object is its own, which is [ownership-and-attribution.md](ownership-and-attribution.md).
It does not enumerate Kubernetes permissions; that is
[rbac-and-privilege.md](rbac-and-privilege.md). Operational procedure — rotating a workload key and
what to restart afterwards, living with the admin Secret — is
[docs/operations/credentials.md](../operations/credentials.md). Reporting a vulnerability is
[SECURITY.md](../../SECURITY.md).

The following gaps belong to the mechanisms above and are open today.

### H-7 — A published credential is verified by count, not by identity

The operator decides that a workload credential is still backed by asking whether its credentials
group holds **at least one** access key, not whether it holds **that** key
(`ensureAccessKeyAndSecret` in
[internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go); the
weakness is named in
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
*Residual risks*). If the workload's key is deleted out of band while another key exists in the
same group, the operator sees a usable credential and changes nothing, while the Secret carries a
dead credential. The `Bucket` stays `Ready` and the workload fails with provider-side
authentication errors that no status field explains. Live today. The adversary is anyone with
console or API access to the project — this is an insider or a mis-click, not a tenant, since the
one-key invariant means a second key can only get there from outside the operator. An operator who
suspects it can force the issue by requesting a rotation, which re-provisions unconditionally.
Comparing the stored key id against the listing would close it and is not implemented.

### H-8 — An existing Secret is adopted silently and deleted with the CR

`spec.secretRef.name` names a Secret that need not exist yet, and if one does exist without a
controller owner the operator merges into it and takes ownership. Deleting the `Bucket` then
deletes that Secret entirely, including data keys the operator never wrote. Live today, inside one
namespace only ([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1 bounds it
there). The adversary is any subject that can create a `Bucket` in a namespace and wants to destroy
or shadow a Secret in it — which is why this operator's shipped write role is an aggregation
fragment that only ever reaches `edit` and `admin`, subjects that already hold Secret access in the
namespace, and why there is no standalone form of it. An operator handing `Bucket` write to a
narrower subject has to close this first; the mechanism for doing so — a consent annotation on the
referenced Secret — is described and rejected-for-now in
[ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) *Alternatives Considered*.
Meanwhile the practical defence is to give each `Bucket` a Secret name nothing else uses.

### H-9 — A destroyed admin credential is not detected, and the repair is a two-step manual act

Nothing probes the admin credential. In the provisioning process nothing watches its Secret either,
and the first successful load is cached for the life of that process and never invalidated
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9); the measuring
controller is the one carve-out, re-reading the Secret for every measurement and never bootstrapping
from it (D10), so deleting the Secret stops measurement at once while provisioning carries on from
its cached copy. A credential destroyed in the cloud surfaces as an opaque data-plane failure
repeating forever, and **every** `Bucket` deletion blocks on the emptiness guard while it lasts. A
successful bootstrap or re-mint is just as quiet: no event, no metric and **no log entry** —
`ensureAdmin` in
[internal/controller/bucket_controller.go](../../internal/controller/bucket_controller.go) logs only
when it rolls back an orphaned key after a failed Secret write. ADR 0004 *Residual risks* records
the missing event and metric; the code is one step quieter than the record says, and the code wins.
The only trace of a re-mint is the side effect itself — a new access key in the `operator-admin`
group, and changed data in the admin Secret. Live today. This is an availability gap, not a
confidentiality one; the adversary is most plausibly an accident in the cloud console. The repair
is a pair and neither half works alone: delete the admin Secret **and then** restart the pod.
Restarting alone reloads the same dead credential; deleting alone changes nothing while the process
runs. Not verified, and this is the deeper gap: whether the fleet is recoverable at all when the
admin *group* is deleted rather than just its key — every existing policy would then exempt a
principal that no longer exists, and there is no control-plane route to rewrite a bucket policy.

### H-10 — The clone control port is guarded by a policy that may not be enforced, over plain HTTP

rclone's remote-control interface on port 5572 is protected by a generated 32-character password
and by a NetworkPolicy admitting only the operator pod. On a cluster whose CNI does not enforce
NetworkPolicies the resource is silently inert and the port is reachable from anywhere in the
cluster, leaving only the password in front of an interface that
[ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) records as able to
execute commands. The operator's own poll is plain HTTP, so the password also crosses the pod
network in the clear. Live today, and only while a clone is running. The adversary is any workload
that can open a connection inside the cluster. An operator can verify their CNI enforces
NetworkPolicies, set `clone.networkPolicy.enabled: false` knowingly if it does not — the value
exists so the absence is a decision rather than a belief — and in either case avoid
`spec.cloneFrom` on a cluster without enforcement. Not verified in this repository: the exact
surface of rclone's rc API, which is the upstream project's, not this one's.

### H-11 — A staged copy of a tenant's source credentials can outlive its clone

The staging Secret in the operator namespace holds the CR author's source credentials and has no
owner reference. It is deleted when the clone completes, but that cleanup is best-effort: a failed
delete is logged and not retried, and once the clone phase is terminal the clone routine is never
entered again, so nothing tries a second time until the `Bucket` is deleted
(`completeClone` and `ensureClone` in [internal/controller/clone.go](../../internal/controller/clone.go)).
A permanently failing clone keeps the Secret alive by design, for the retry. Live today. The
exposure is confined to readers of the operator namespace, who can already read the far more
valuable admin Secret — so the real cost is a tenant's foreign-endpoint credential sitting
somewhere its owner cannot see and would not think to rotate. An operator can list Secrets carrying
`app.kubernetes.io/component: clone` in the operator namespace and match them against `Bucket`
objects still cloning.

### H-12 — Replacing the service-account key requires a restart

The key is read once at process start, by this operator and by the SDK underneath it, and the
material then lives in process memory. Replacing the mounted Secret — after a suspected compromise,
or as routine hygiene — has no effect until the pod restarts, and nothing in the operator reports
that the file on disk and the key in use have diverged. Live today. The adversary is whoever
obtained the old key; revoking it in the cloud console does take effect immediately and is visible
(`invalid_grant`, see *The service-account key is the project's master credential*), so the safe
order is revoke first, then replace and restart — not replace and assume. There is no expiry on
this key and nothing in this operator manages its lifetime.

### H-13 — Nothing records who requested a rotation

`status.lastRotationTrigger`, `status.lastRotationTime` and the `CredentialsRotated` event say that
a rotation happened and which value caused it. None of them says who set the annotation, and the
operator never mutates it, so the object itself does not carry the actor either. Since patching a
`Bucket` is enough to destroy its live credential, an unexplained outage has no attribution inside
the cluster's own state. Live today. The only record is the API server's audit log, if one is
enabled — which is the one thing an operator can do about it in advance.
