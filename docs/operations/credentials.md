# Credentials at runtime

Three credentials matter to whoever runs this operator. The **workload credential**
is the S3 access key a `Bucket` hands to its application through a Kubernetes
Secret; it is per-`Bucket`, lives in that `Bucket`'s namespace and is rotated on
request. The **admin credential** is the operator's own S3 identity, one per
StackIT project, kept in a Secret in the operator's namespace and used for every
data-plane call the operator makes on the fleet. The **service-account key** is
what the operator authenticates to STACKIT's control plane with; it is issued in
the STACKIT console, replaced from outside the cluster, and it is the only one of
the three the operator does not create itself.

This page is about handling all three at runtime: consuming a workload Secret,
rotating a key, what a rotation costs, living with the admin Secret, and rotating
the service-account key without a restart. The rules behind the behaviour are
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md)
(workload credential),
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)
(admin credential) and
[ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)
(the service-account key at runtime). The complete `Bucket` and chart reference — every field, every
value, every default — is in the [README](../../README.md) and only there; this
page names the handful of keys it is about and explains them.

| Not on this page | Where it is |
|---|---|
| The complete list of Secret data keys and `spec.secretRef.keys` fields | [README](../../README.md) |
| How the key-replacement path, the Secret write and the rotation bookkeeping are implemented | [docs/developer/credentials.md](../developer/credentials.md) |
| Who can read a credential, what holding it is worth, and the open gaps | [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md) |
| Reading the rotation fields on a `Bucket` alongside the rest of its status | [bucket-status.md](bucket-status.md) |
| The rotation metric and the alerts around it | [monitoring.md](monitoring.md) |
| Issuing a service-account key in the first place, and the role it needs | [prerequisites.md](prerequisites.md) |
| How the key poll, the validation and the atomic swap are built | [docs/developer/service-account-key-reload.md](../developer/service-account-key-reload.md) |

---

## The workload Secret

The operator writes the provisioned access key **and** the S3 connection
parameters into the Secret named by `spec.secretRef.name`. The Secret is created
in the `Bucket`'s own namespace, carries the `Bucket` as controller owner and is
garbage-collected with it; there is no field that can aim it at another namespace
([ADR 0001](../adr/0001-a-bucket-only-affects-its-own-namespace.md) D1/D4).

Six logical fields are written. The two that carry the credential default to
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (# default), the other four carry
the bucket name, region, endpoint host and the full path-style bucket URL. Every
data-key name is overridable per `Bucket` under `spec.secretRef.keys`
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D10);
the complete field list with its defaults is in the [README](../../README.md).

**The bucket-name key carries the *resolved physical* bucket name, not
`spec.bucketName`.** `SecretData` writes what `EffectiveBucketName()` returns —
the frozen `status.resolvedBucketName`, else the resolved-name annotation, else
`spec.bucketName`. The two are equal only on a deployment that runs without a
name prefix and without the namespace component; configure either and the key
holds `<prefix>-<namespace>-<spec.bucketName>` instead. An application must use
the value out of the Secret and never reconstruct it from the CR — see
[bucket-naming.md](bucket-naming.md).

The defaults are env-var style so the Secret can be consumed with a single
`envFrom`. A working minimal pair:

```yaml
# every name and value below is "# example"; nothing here has a default
apiVersion: stackit-bucket.gtrfc.com/v1
kind: Bucket
metadata:
  name: reports
  namespace: analytics        # the Secret is created here, always
spec:
  bucketName: reports         # the CR-side name; the physical name may carry a prefix
  secretRef:
    name: reports-s3          # only the NAME is yours to pick; the namespace is the CR's
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: reporting
  namespace: analytics
spec:
  template:
    spec:
      containers:
        - name: app
          image: example/reporting:1.0
          envFrom:
            - secretRef:
                name: reports-s3          # AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY
                                          # (# default) and the connection keys;
                                          # see the README table for the full list
```

Two properties of this Secret decide how you should treat it:

- **It is the source of truth for the live credential.** The provider returns the
  secret half of an access key exactly once, in the response that creates it, and
  never again. Nobody can read it back — not the operator, not the provider's
  console ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D1).
- **Deleting it costs a credential, not just a Secret.** The next reconcile cannot
  tell a deleted Secret from a never-provisioned one, so it deletes every access
  key in the bucket's credentials group and publishes a fresh one
  ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D4/D5).
  Restoring a backup of the Secret does not undo that: the restored values are
  already dead if a pass ran in between. A backup of this Secret is a backup of a
  live credential and has to be protected like one.

`envFrom` reads the Secret once, at pod start. *Not verified:* whether a consumer
that mounts the Secret as a projected volume picks up a rotated credential without
a restart. The kubelet does refresh mounted Secret volumes, but this operator has
not tested it and
[ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D9
does not promise it.

---

## Rotating a workload credential

Rotation is requested with an annotation on the `Bucket`; no spec change and no
Secret access is needed. The value is opaque — an RFC3339 timestamp by convention,
mirroring what `kubectl rollout restart` writes — and the operator acts whenever
it differs from the value recorded in `status.lastRotationTrigger`
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D8).

```yaml
metadata:
  annotations:
    stackit-bucket.gtrfc.com/rotate-credentials-at: "2026-09-19T10:00:00Z"   # example
```

### Procedure

1. **Decide what has to restart afterwards.** The old key dies the moment the
   rotation runs; see [What a rotation costs](#what-a-rotation-costs) below.
   Rotating a selector rotates every matched `Bucket` at once.
2. **Set the annotation.** One `Bucket`:

   ```bash
   kubectl annotate bkt reports -n analytics \
     stackit-bucket.gtrfc.com/rotate-credentials-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
     --overwrite
   ```

   Every `Bucket` matching a label selector:

   ```bash
   kubectl annotate bkt -l team=payments \
     stackit-bucket.gtrfc.com/rotate-credentials-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
     --overwrite
   ```

   `--overwrite` is required on every rotation after the first, because the
   annotation already exists then. Both commands act on the current namespace; add
   `-n <namespace>`, or `--all-namespaces` together with a selector.
3. **Verify the operator acted.** The recorded trigger must equal what you set:

   ```bash
   kubectl get bkt reports -n analytics \
     -o jsonpath='{.status.lastRotationTrigger}{"\n"}{.status.lastRotationTime}{"\n"}{.status.accessKeyID}{"\n"}'
   ```

   and there is one `Normal` event per performed rotation:

   ```bash
   kubectl get events -n analytics --field-selector reason=CredentialsRotated
   ```
4. **Restart the consumers** so they re-read the Secret — see *Nothing in
   Kubernetes propagates a changed Secret into a running process* under
   [What a rotation costs](#what-a-rotation-costs).

Under GitOps the same applies with the annotation written in Git: the operator
never mutates the annotation, so a continuously syncing controller re-applies the
same value indefinitely and nothing happens after the first rotation
([gitops.md](gitops.md)).

### What a rotation leaves behind

| Surface | Value |
|---|---|
| `status.accessKeyID` | the access key id of the live credential — the public half only |
| `status.lastRotationTrigger` | the annotation value the operator last acted upon |
| `status.lastRotationTime` | when that rotation completed |
| Event `CredentialsRotated` (Normal) | one per performed rotation |
| Metric `stackit_s3_provisioner_credentials_last_rotation_timestamp_seconds` | Unix time of the last rotation; absent for a `Bucket` that never rotated |

The secret half is never written to status, an event, a log line or a metric
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D12).

### Edges worth knowing before you use it

| Situation | What happens |
|---|---|
| Re-applying the same value | Nothing. The trigger is level-based and an unchanged value is a no-op. |
| Removing the annotation | Nothing. Removal is not a trigger. |
| Re-applying an **older** value that has since been superseded | **It rotates again.** The comparison is against the single value currently in `status.lastRotationTrigger`, not against a history ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D8). |
| Annotating a `Bucket` whose clone is still running with the default `holdSecretUntilCloned: true` | The rotation is deferred. That pass returns before the credential step, so the trigger stays pending and is acted upon in the pass after the copy finishes ([cloning.md](cloning.md), [ADR 0011](../adr/0011-a-clone-runs-once-as-a-job-in-the-operator-namespace.md) D7). Verified in the reconciler's pass ordering. |
| Renaming `spec.secretRef.name` on a live `Bucket` | It rotates. The new name looks unprovisioned, so a fresh credential is published under it and the old Secret is left holding a dead one. *Not verified:* no test covers a rename; this is reasoned from the write rules, and it is recorded as a residual risk in [ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md). |
| The operator crashes between publishing the key and writing status | The next pass rotates again. Correct, but it costs a second disruption — including for a workload that had just picked up the first new credential. |
| Rotation and `metadata.generation` | A rotation moves no generation, so nothing that keys off `observedGeneration` marks it. |

---

## What a rotation costs

**There is no overlap window.** Every existing key in the bucket's credentials
group is deleted *before* the replacement is created, so the previous credential
stops working immediately and there is never a second valid one
([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D5/D9).
The ordering is deliberate: creating first and deleting second would, on a crash
between the two steps, leave a live access key in the project whose secret half
nobody holds and which no later pass can recognise — an unrecoverable leak traded
for a bounded, visible outage.

**Nothing in Kubernetes propagates a changed Secret into a running process.** The
operator can publish a new credential; it cannot make anybody use it. Consumers
reading the Secret via `envFrom` keep the old values until their pods restart:

```bash
kubectl rollout restart deployment/reporting -n analytics
```

Rotating a whole label selector therefore means restarting all of those workloads.
Plan the restart as part of the rotation, not after somebody reports S3 errors.

**The window between the two halves is real.** If the operator fails after
deleting the old key and before publishing the new one, the workload has no
working credential at all. That state is short, it is visible — `Ready` drops at
once, see below — and the next successful pass closes it.

**Anyone who can patch the `Bucket` can kill its live credential.** The trigger is
an annotation in `metadata`, so patch permission on the CR is enough; no Secret
access is required. Within a namespace this is a denial-of-service primitive
against that namespace's own workload, which is why `Bucket` write is only handed
out together with the permissions that already imply it — see
[docs/security/rbac-and-privilege.md](../security/rbac-and-privilege.md).

---

## Failure modes: workload credentials

| What you see | What it means | What to do |
|---|---|---|
| `Ready=False`, reason `Failed`, message containing `workload credential destroyed and not replaced` | The operator deleted the live key and could not publish a replacement — the Secret in the cluster is dead. This drops `Ready` immediately instead of being held through the degraded grace window ([ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md) D6). | Fix whatever the wrapped error names (provider refusal, API-server write failure) and let the next pass republish. No manual cloud cleanup is needed: a key whose Secret write failed is deleted again by the operator ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D6). |
| `Ready=False`, reason `Failed`, message `secretRef.keys: "<a>" and "<b>" both map to data key "<k>"` | Two logical fields resolve to the same Secret data key; one value would silently overwrite the other. The `Bucket` is parked without a retry and re-reconciles on the next spec change ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md) D10). | Give the colliding fields distinct key names under `spec.secretRef.keys`. |
| `Ready=False`, reason `Failed`, message `secretRef … targets the operator's admin credentials Secret; refusing to provision` | A `Bucket` in the operator's namespace named the admin Secret. Refused without retry, and teardown of such a `Bucket` never deletes that Secret ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D8). | Point `spec.secretRef.name` at any other name. |
| Workload gets provider-side authentication errors while the `Bucket` is `Ready` and status shows nothing | The backing check is count-based: a pass is satisfied when the credentials group holds **at least one** access key, not that it holds *this* key. A key deleted out of band while another key exists in the same group is not noticed ([ADR 0007](../adr/0007-a-workload-credential-lives-in-its-secret-and-rotates-only-on-request.md), *Residual risks*). | Force a replacement with the rotation annotation, then restart the consumers. |
| Workload gets authentication errors and the group holds no key at all | The next pass sees no key, mints a replacement and republishes the Secret. | Restart the consumers to pick up the new credential. There is no path that restores the previous one. |

There is no audit trail of *who* rotated. The recorded trigger value and the
`CredentialsRotated` event say that a rotation happened and which value caused it;
the identity that set the annotation is only in the API server's own audit log, if
one is enabled.

---

## The operator's admin Secret

The operator mints one S3 credential for itself per project — a credentials group
with the display name `operator-admin` holding exactly one access key — and
persists it in a Secret in its own namespace. It exists because setting a bucket
policy is an S3 call, and the service-account key the operator is configured with
is a control-plane identity with no route to `PutBucketPolicy`
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md), Context).

| Property | Value |
|---|---|
| Secret name | `stackit-s3-provisioner-admin` (# default), from `--admin-credentials-secret-name` or the environment variable `ADMIN_CREDENTIALS_SECRET_NAME` |
| Namespace | the operator's own, from `POD_NAMESPACE` (the chart sets it from `metadata.namespace`) or `--operator-namespace` |
| Data keys | `accessKeyID`, `secretAccessKey`, `urn`, `credentialsGroupID` |
| Label | `app.kubernetes.io/managed-by: stackit-s3-provisioner` |
| Owner reference | none — nothing garbage-collects it |

Verified 2026-09-19: the chart renders **no** value for the Secret name and passes
no such argument, so under a chart install the default name always applies. There
is no `extraArgs` escape hatch in the chart either.

The group's URN is listed in the exemption (`NotPrincipal`) of the blanket-deny
statement of **every** bucket policy the operator writes, and a read grant naming
it is dropped when the policy is built
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D7,
[ADR 0003](../adr/0003-workloads-are-isolated-by-an-explicit-deny-policy.md)). An
admin identity that a policy denies cannot rewrite that policy, because there is
no control-plane route to a bucket policy — that is an unrepairable lockout, not an
inconvenience.

### Rules for handling it

- **Do not delete it while the operator is running.** See the repair procedure
  below for why that alone changes nothing, and what it does once the process
  restarts.
- **Do not put a second access key into the `operator-admin` group by hand.** Any
  bootstrap deletes every key in that group before minting a fresh one
  ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D5),
  so a hand-placed key is destroyed without warning.
- **Uninstalling the chart leaves the Secret and the cloud credential behind.** It
  has no owner reference and the chart does not create it. That is deliberate — a
  reinstall picks the same credential back up and every existing bucket policy
  stays valid — but deleting the namespace without deleting the cloud credential
  first orphans a key that still has admin scope inside the project.
- **A service-account key without a known namespace is a startup fault.** With a
  key configured and `POD_NAMESPACE` unset (and no `--operator-namespace`), the
  process logs `operator namespace unknown; set POD_NAMESPACE (or
  --operator-namespace) when a StackIT key is configured` and exits rather than
  run with nowhere to persist the credential
  ([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D4).

### When the admin credential stops working

Nothing detects this. The operator does not probe the credential at startup and
does not mint a replacement in response to a data-plane refusal — minting an
admin-scope credential is a privileged act and is not taken on the strength of an
error signal
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9).
It surfaces as repeated reconcile failures with opaque data-plane errors. What
stops with it:

| Path | Effect |
|---|---|
| resolving a bucket's credentials group from its tags | every reconcile fails; no `Bucket` converges |
| writing or healing the isolation policy | isolation cannot be established or repaired |
| the emptiness check before teardown | **every `Bucket` deletion blocks** on the data-loss guard ([deletion.md](deletion.md)) |
| the opt-in wipe | `spec.wipeOnDelete` cannot run |
| the destination side of a clone | a clone cannot start or finish |
| size measurement | listings fail — non-fatal by design, `Ready` is untouched |

That table was established by reading the code, not by destroying a live admin key
and observing the result
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md),
*Residual risks*).

### Repair procedure: replacing the admin credential

The repair is a **pair of acts, and neither half works alone**
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9).
Deleting the Secret under a running process does not repair provisioning,
because the **provisioning** path caches the credential on first success and
holds it in memory for the life of the process, never re-reading the Secret
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D9).
Restarting without deleting the Secret reloads the same dead credential, because
bootstrap only runs when the Secret is absent or incomplete — complete meaning
`accessKeyID`, `secretAccessKey` and `urn` are all present.

The cache is scoped to provisioning, and the deletion is not invisible
everywhere. With `bucketUsage.enabled`, size measurement runs in a second
controller in the same process that re-reads the admin Secret for **every**
measurement and caches nothing
([ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D10).
So between step 1 and step 2 below, provisioning keeps running on the cached
credential while measurement starts failing immediately — the measuring
reconcile logs `bucket size measurement waiting for admin credentials` at
verbosity 1 (`logging.level: debug`) and leaves `status.usage` untouched; it never touches `Ready`
([usage-and-cost.md](usage-and-cost.md)). Keep the gap short.

1. Delete the Secret in the operator namespace:

   ```bash
   kubectl delete secret stackit-s3-provisioner-admin -n <operator-namespace>
   ```
2. Restart the operator:

   ```bash
   kubectl rollout restart deployment/<release>-stackit-s3-provisioner -n <operator-namespace>
   ```
3. Verify a new Secret was written and that it is complete:

   ```bash
   kubectl get secret stackit-s3-provisioner-admin -n <operator-namespace> \
     -o jsonpath='{.data.accessKeyID}{"\n"}{.data.secretAccessKey}{"\n"}{.data.urn}{"\n"}'
   ```

   The values are base64-encoded; that all three are non-empty is what matters
   here, because those three are exactly what "complete" means.
4. Verify the fleet converges again — a `Bucket` that was failing should return to
   `Ready=True`:

   ```bash
   kubectl get bkt -A
   ```

On bootstrap the operator finds or creates the `operator-admin` group **by display
name** — the single credentials group resolved that way, carved out of the
prohibition in
[ADR 0002](../adr/0002-a-credentials-group-is-attributed-through-its-bucket.md) D3
and recorded in
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md) D6 —
deletes every key in it, mints one fresh key and writes the Secret. Because the
group is kept and only its key is replaced, every existing bucket policy stays
valid: policies name the group's URN, not the key.

**A successful bootstrap leaves no trace in the cluster at all** — no event, no
metric, and, verified against the code on 2026-09-19, not even a log line:
`ensureAdmin` logs nothing on the success path and `writeAdminSecret` logs
nothing either. The only evidence a re-mint happened is the newly written admin
Secret and the new key in the project, which is why step 3 above — checking the
Secret — is the verification and the only one available.
[ADR 0004](../adr/0004-the-operator-bootstraps-its-own-s3-admin-credential.md)
(*Residual risks*) says a re-mint is visible in the log; here the code disagrees
with the ADR, and the code is what runs.

A **failed** bootstrap is visible on the `Bucket` that triggered it:
`status.message` begins with `bootstrap admin credentials:` — wrapping whichever
step failed (group, key clear, key create, Secret write) — and a Warning event
with reason `Failed` carries the same text. A `Bucket` that has never been
`Ready` goes to `Ready=False`; one that was already provisioned keeps `Ready`
and gets `ProviderReachable=False` for the degraded grace window, because a
bootstrap failure is a non-definitive provider failure like any other
([provider-outages.md](provider-outages.md),
[ADR 0012](../adr/0012-ready-describes-the-last-verified-state.md)).

*Not verified, and this is the gap:* whether the fleet is recoverable at all when
the `operator-admin` **group** is lost rather than just its key. Every existing
bucket policy would then exempt a principal that no longer exists, the operator's
new admin identity would be denied by the blanket-deny statement, and there is no
control-plane route to rewrite a bucket policy. That chain is derived from the
policy shape, not measured.

*Not verified, and this is the gap:* the behaviour of a bootstrap during a leader
handover. The chart enables leader election by default while the operator's own
`--leader-elect` flag defaults to off, so a deployment that bypasses the chart may
run several reconciling replicas at once; a rolling update releases leadership
without an atomic handover, and a bootstrap that clears the group's keys while the
outgoing process still holds one has not been analysed.

---

## Rotating the StackIT service-account key

The service-account key is the operator's credential for STACKIT's control plane: every bucket
create, every credentials group, every policy read goes through a token minted from it. STACKIT
stamps `validUntil` into the key when it issues one — 90 days after `createdAt` on the keys checked
on 2026-09-19 — so this rotation is a scheduled event with a deadline printed on the credential,
not a possibility to plan for.

Unlike the other two credentials on this page, the operator does not create this one and cannot
rotate it for you. What it does is notice that you have.

### Procedure

The operator re-reads the mounted key file every
`stackit.serviceAccountKey.reloadInterval` (`"30s"` # default) and, when the content has changed,
proves the new key before using it
([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md)). So the
rotation is: write the new key into the Secret, then watch.

1. **Issue a new key** for the *same* STACKIT project, in the STACKIT console or through whatever
   issues them for you. A key for a different project is refused by the running process — see
   [the project guard](#the-project-guard-is-not-a-boundary).
2. **Write it into the Secret** the chart's `stackit.serviceAccountKey.secretName` names, under the
   data key `stackit.serviceAccountKey.secretKey` names (`sa-key.json` # default). Whatever writes
   it is yours: an external secret operator, SOPS, or by hand.

   ```bash
   NS=stackit-s3-provisioner-system      # example; the release namespace
   kubectl -n $NS create secret generic stackit-sa-key \
     --from-file=sa-key.json=./new-key.json \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

3. **Wait out the window.** The kubelet needs up to about 90 seconds to refresh the file inside the
   container — that part is Kubernetes', not this operator's — and then up to one
   `reloadInterval` for the operator to notice.
4. **Confirm it landed.** One log line per successful swap, naming the project and the service
   account and never the key:

   ```bash
   kubectl -n $NS logs deploy/stackit-s3-provisioner \
     | grep 'loaded a new StackIT service-account key'
   # expected: project=<the project id>  issuer=<the new service account>  validUntil=<the new expiry>
   ```

   The same fact is a metric: `stackit_s3_provisioner_sa_key_loaded_timestamp_seconds` moves to the
   moment of the swap, and `stackit_s3_provisioner_sa_key_valid_until_timestamp_seconds` to the new
   key's expiry — absent if the new key carries none.
5. **Only then revoke the old key.** Nothing forces this order, and getting it wrong is the one way
   to turn a routine rotation into an outage: between revoking the old key and the new one landing,
   every `Bucket` in the cluster drops to `Failed`.

### If the operator refuses the new key

A candidate that does not parse, names a different project, is not usable key material, or that the
provider will not mint a token with, is **discarded** — the operator carries on with the key it
already holds. That is deliberate: it means a truncated write or an already-revoked key cannot take
down a healthy operator. It also means a failed rotation is invisible from the outside until the old
key expires, so it has its own signals:

| Signal | What it says |
|---|---|
| `stackit_s3_provisioner_sa_key_reload_failing` is `1` | the operator is refusing the key currently on disk |
| `StackitS3SaKeyReloadFailing` (warning, after 30m) | the same, as an alert |
| A log line `rejected a candidate StackIT service-account key; carrying on with the key in use` | the reason, logged once per distinct file content, with `definitive` telling you whether retrying can help |

A rejected candidate is never given up on. A definitive rejection — the file does not parse, it names
a foreign project, the key material is unusable, or the provider answered a structured `400`/`401`/
`403` — is retried on a doubling schedule from one interval up to ten minutes, and the schedule
resets the moment the file content changes. Anything else, meaning the provider did not actually
answer, is retried every interval. So a key that is simply slow to propagate activates by itself.

### The project guard is not a boundary

The running process refuses a candidate whose `projectId` differs from the one it started with. That
comparison lives in memory, so a **pod restart erases it**: the new process has no memory of the
previous project and adopts whatever the mounted key names. The guard is worth having because it is
free, not because it protects anything
([ADR 0016](../adr/0016-the-service-account-key-is-reloaded-only-after-it-is-proven.md) D4).

What actually protects the data if the wrong key is ever mounted is
[ADR 0015](../adr/0015-a-provisioned-bucket-is-never-re-created-implicitly.md): every provisioned
`Bucket` reports its bucket as missing and nothing is re-created or overwritten. Reading that state
is [vanished-buckets.md](vanished-buckets.md).

### Rotating with the reload switched off

`stackit.serviceAccountKey.reloadInterval: "0"` restores the behaviour that needed a restart. The
rotation is then steps 1 and 2 above, followed by:

```bash
kubectl -n $NS rollout restart deploy/stackit-s3-provisioner
kubectl -n $NS logs deploy/stackit-s3-provisioner | grep 'StackIT client configured'
# expected: the project id from the NEW key, and the configured region
```

With the reload off, none of the `sa_key` metrics is exported, so neither the expiry warning nor the
rejection alert can fire. A key path that is unreadable or unusable **at startup** is still a startup
failure and crash-loops the pod, whatever the interval is.

### Not verified

The procedure above was exercised against the real API on 2026-09-20, between two service accounts
of one project: the replacement key was proven and adopted without a restart, and the bucket,
credentials group and access key the previous account had created stayed exactly where they were.

Two things that run did not settle, and a rotation leans on both:

* **It added a service account rather than revoking one.** That a credentials group and its access
  keys outlive the *revocation* of the key that created them is still a design argument — no cloud
  IAM model cascade-deletes an administrator's creations when that administrator's credential is
  rotated — and not an observation. It is the reason step 5 of the procedure says to revoke the old
  key only after the new one has demonstrably landed.
* **No fleet was watched.** The recovery of already-failed `Bucket` resources after a swap follows
  from the ordinary requeue machinery rather than from a measurement, so the "within one
  `driftResyncInterval`" figure is derived, not timed.

---

## Related

| Page | Why |
|---|---|
| [README](../../README.md) | the complete `Bucket` and chart reference, including every Secret data key |
| [bucket-status.md](bucket-status.md) | the full status surface these fields sit in |
| [monitoring.md](monitoring.md) | the rotation metric, the reconcile-error alert and the degraded alert |
| [provider-outages.md](provider-outages.md) | why most provider failures hold `Ready` while a destroyed credential does not |
| [cloning.md](cloning.md) | why publishing the workload Secret can be delayed |
| [deletion.md](deletion.md) | the emptiness guard the admin credential performs |
| [gitops.md](gitops.md) | why a repeated sync of the same rotation trigger does nothing |
| [docs/developer/credentials.md](../developer/credentials.md) | the mechanics behind everything on this page |
| [docs/security/credentials-and-secrets.md](../security/credentials-and-secrets.md) | where every credential lives and what its loss or leak costs |
| [prerequisites.md](prerequisites.md) | issuing the service-account key and the role it needs |
| [configuration.md](configuration.md#the-service-account-key-reload-interval) | the reload interval, the full window, and switching it off |
| [docs/developer/service-account-key-reload.md](../developer/service-account-key-reload.md) | how the poll, the validation and the atomic swap are built |
