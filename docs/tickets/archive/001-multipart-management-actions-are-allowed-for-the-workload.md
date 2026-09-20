# TICKET: stackit-s3-provisioner — bucket policy blocks multipart management (GitLab registry 500)

Repo: `~/repos/stackit-s3-provisioner` (github.com/guided-traffic/stackit-s3-provisioner)

## Problem
GitLab CI image build & cache pushes to `registry.mgmt-p.sutorbank.cloud` fail:
`500 Internal Server Error` on blob PUT (`unknown: unknown error` from buildkit).
Registry pod log (`kubectl logs -n gitlab deploy/gitlab-registry`):

```
"error":"s3aws: AccessDenied: Access Denied\n\tstatus code: 403", "msg":"error resolving upload"
```

## Root cause (confirmed by direct S3 tests with the registry bucket creds)
Permission matrix against `sb-mgmt-p-gitlab-gitlab-registry`
(endpoint `https://object.storage.eu01.onstackit.cloud`, creds from
secret `gitlab-registry` in ns `gitlab`):

| Operation | Result |
|---|---|
| ListBucket / Put / Get / DeleteObject | OK |
| CreateMultipartUpload / UploadPart / CompleteMultipartUpload | OK (map to s3:PutObject) |
| **ListMultipartUploads** | **AccessDenied** |
| **ListParts** | **AccessDenied** |
| **AbortMultipartUpload** | **AccessDenied** |

Same denial with the runner-cache creds on their own bucket → policy-wide, not bucket-specific.
Source: the operator's per-bucket isolation policy `BuildIsolationPolicy()` in
`stackit/s3.go` — statement 2 (`workload-objects-only`) is a `Deny` with a
`NotAction` whitelist that misses the three multipart-management IAM actions.
The Docker/GitLab registry S3 driver calls ListMultipartUploads on every blob
commit ("resolving upload") → 403 → 500. Registry S3 driver docs require
exactly: `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts`,
`s3:AbortMultipartUpload`.

## Change (ALREADY APPLIED locally in the repo, uncommitted)
- `stackit/s3.go` — `BuildIsolationPolicy`, statement 2 `NotAction` extended with
  `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts`,
  `s3:AbortMultipartUpload` (+ explanatory comment).
- `stackit/s3_test.go` — expected-actions list extended accordingly.
- `go build ./...` and `go test ./stackit/...` passed after the edit
  (a later gofmt/linter pass reformatted `s3.go`; re-verify).

## Remaining work
1. `gofmt -l ./stackit` clean; `go test ./...` full suite green.
2. Review + commit + release: new image + Helm chart version (currently
   pinned `1.6.0`).
3. Bump the chart pin in `stutor-k8s-flux-base`:
   `components/storage-provisioner/stackit-s3-provisioner/app/helm-release.yml`
   (`version: "1.6.0"` → new release).
4. Propagation is automatic: `internal/controller/bucket_controller.go:623` —
   reconcile compares desired vs current policy (`PoliciesEquivalent`) and
   rewrites on drift, so ALL existing buckets get the fixed policy after the
   operator upgrade. No Bucket CR changes needed. Do NOT delete Bucket CRs
   (`wipeOnDelete` is enabled!).

## Verification
1. With registry bucket creds:
   `aws s3api list-multipart-uploads --bucket sb-mgmt-p-gitlab-gitlab-registry --endpoint-url https://object.storage.eu01.onstackit.cloud`
   → must succeed (no AccessDenied).
2. Re-run the failing GitLab CI job (buildkit push to
   `.../container-cache`) → blob PUT must return 201.

## Cleanup note
Diagnostic leftovers in `sb-mgmt-p-gitlab-gitlab-registry`, prefix `_claude-probe/`:
one dangling multipart upload for key `_claude-probe/mpu.bin`
(uploadId `d08NeqruP-JY9CfldLD6UHiwVhv4fp5yQgJTpldqBjJ-ESu9b574JhqRjA`) —
abort was denied with workload creds; abort it with admin creds after the
policy fix (or rely on storage lifecycle cleanup). Test objects themselves
were deleted.
