# TICKET: stackit-s3-provisioner — optional cross-bucket READ permissions within a namespace

Repo: `~/repos/stackit-s3-provisioner` (github.com/guided-traffic/stackit-s3-provisioner)

## Problem

Credentials issued per Bucket CR are strictly scoped to their own bucket.
There is no way to grant a credential read access to another bucket, even one
owned by the same Kubernetes namespace.

Concrete case (verified on mgmt-d, 2026-08-23): the GitLab backup job in
namespace `gitlab` uses the `gitlab-backups` bucket credentials and gets
`403 AccessDenied` when reading the sibling data buckets (artifacts, uploads,
lfs, packages, mr-diffs, terraform-state, ci-secure-files, registry, pages).
The nightly backup therefore has to skip all object storage components and
only covers the Gitaly repositories — no second copy of the object storage
data exists.

## Desired feature

A Bucket CR should be able to request additional READ-ONLY permissions on
other buckets, restricted to buckets of the same namespace. The bucket
owner's isolation guarantees must stay intact otherwise: no write, no delete,
no cross-namespace access.

## Use case / consumer

`k8s-flux-mgmt` → `components/gitlab/`: once available, the backup bucket
credential gets read access to the GitLab data buckets, and the explicit
`--skip <component>` list in the toolbox backup cron
(`components/gitlab/release/helm-release.yml`) can be removed so the nightly
backup tar contains the object storage data again.

## Acceptance

- A bucket credential can list and get objects in explicitly named sibling
  buckets of the same namespace.
- Access to buckets in other namespaces remains denied.
- Without the new option, behavior is unchanged (scoped as today).
- Verification: `s3cmd ls` / `get` with the `gitlab-backups` credential on
  `sb-<cluster>-gitlab-gitlab-artifacts` succeeds; `put` / `del` still fail.
