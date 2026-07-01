# CLAUDE.md — StackIT S3 Operator

Kubernetes-Operator (Go), der über die **StackIT Object Storage API** Buckets,
Workload-Zugangsdaten und Bucket-Policies provisioniert. Ein Operator-Deployment
pro Cluster, jeweils gebunden an **ein StackIT-Projekt** via Service-Account-Key.

**Phase:** Machbarkeit verifiziert (echte API-Tests grün). **Operator-Skelett + CI stehen**
(kubebuilder-Layout, `Bucket`-CRD, controller-runtime Manager, Helm-Chart, GitHub-Pages-Release,
Renovate, semantic-release — alle Checks grün). Der Reconciler ist noch ein **Stub**
(Finalizer + `Ready=NotImplemented`-Condition, kein Cloud-Call). Detaillierte Findings:
**`INIT-SETUP.md`** (Quelle der Wahrheit). Go-Modul: `github.com/guided-traffic/stackit-s3-provisioner`.

## Repo-Layout

```
stackit/client.go                        API-Wrapper (Auth, Bucket-/Group-/AccessKey-Ops, S3-Endpoint)
stackit/client_test.go                   Offline-Unit-Tests (Key-Parsing)
stackit/integration_test.go              //go:build integration — Layer-1 (Cross-Projekt-Isolation)
stackit/credentials_integration_test.go  //go:build integration — Layer-2 (Workload-Creds + echtes S3)
api/v1/bucket_types.go                    CRD `Bucket` (stackit-bucket.gtrfc.com/v1) + Helper, +kubebuilder-Marker
cmd/main.go                              controller-runtime Manager (baut stackit.Client aus SA-Key-Env)
internal/controller/bucket_controller.go Reconciler (STUB: Finalizer + Condition, TODO Provisioning §8)
config/                                  kustomize: generierte CRD (crd/bases) + RBAC + Manager
deploy/helm/stackit-s3-provisioner/      Helm-Chart (CRD via `make sync-helm-crd` synchronisiert)
test/integration/                        //go:build integration — envtest gegen echten API-Server
test/e2e/                                //go:build e2e — Kind-Smoke (Operator healthy + CR reconciled)
Makefile / Containerfile / renovate.json CI-Gerüst (an Valkey-Operator orientiert)
.github/workflows/                       release.yml (Test+Release), build.yml (Docker+Helm), renovate.yml
account-1.json / account-2.json          SA-Keys (ECHTE RSA-Private-Keys, .gitignore'd, NIE committen)
INIT-SETUP.md                            Vollständige Findings, Policy-Templates, offene Fragen
```

## Build & Test

```bash
go build ./...
go vet -tags integration ./...
go test ./stackit/ -run TestLoadAccount -v                                  # offline (kein Netz)
go test -tags integration ./stackit/ -run Integration -v -timeout 15m       # echte API (legt Ressourcen an, räumt auf)
go test -tags integration ./stackit/ -run IntegrationWorkloadCredentials -v  # nur Layer 2

# Operator/CI (make):
make help                       # alle Targets
make generate-all               # CRD + DeepCopy regenerieren, Helm-Chart-CRD syncen (nach api/v1-Änderung!)
make lint gosec vuln cyclo      # Linter + Security-Scans (wie CI)
make test-unit-coverage         # Unit (offline), make test-integration-coverage = envtest
make e2e-local                  # Kind hochziehen, via Helm installieren, e2e-Smoke
```

Integration-Tests treffen die **echte** StackIT-API (Projekte `1f426c6e…` und `eb4205a7…`,
Region `eu01`), erzeugen + löschen reale Buckets/Groups/Keys. Skippen automatisch ohne SA-Key-Dateien.

## Architektur (Kern)

Zwei Ebenen:
- **Control Plane** (`stackit-sdk-go/services/objectstorage`, Bearer-Token): Bucket / CredentialsGroup /
  AccessKey anlegen+löschen, Service aktivieren. Ruft der **Operator**.
- **Data Plane** (S3 / `minio-go`, Access-Key+Secret): Objekte put/get, **Bucket-Policy setzen**
  (`PutBucketPolicy` — nicht im SDK!). Ruft Operator (admin-Key) **und** Workloads.

**Isolations-Layer:**
- **Layer 1 (Cross-Projekt):** strukturell garantiert — SA-Token hat nur Rollen im eigenen Projekt.
  Fremdprojekt-Zugriff → **403**. Nicht vom Operator-Code abhängig. ✅ verifiziert.
- **Layer 2 (Workload↔Workload im Projekt):** nur via Bucket-Policy. StackIT-Default ist **offen**,
  daher **explizite Deny-Policy** nötig (Template in `INIT-SETUP.md` §4.1). ✅ verifiziert.

## Sicherheits-Invarianten (nicht verletzen)

1. **SA-Rollen nur auf Projekt-Ebene** zuweisen — eine kaskadierende Org-Rolle bricht Layer 1.
2. `account-*.json` **niemals committen** (echte Private-Keys; `.gitignore` schützt).
3. Bucket-Policy braucht **2 Deny-Statements**: `Deny NotPrincipal [admin, workload]` (Outsider raus)
   + `Deny Principal workload NotAction [object-ops]` (kein Bucket-Management). Reines `Allow` isoliert NICHT.
4. **Admin-Group immer in `NotPrincipal`** lassen → sonst Lockout (StorageGRID kann Account-Root aussperren).
5. `secretAccessKey` nur **1× bei Create** verfügbar → sofort sichern.

## SDK-Fallstricke (verifiziert)

- Auth: `config.WithServiceAccountKeyPath(file)` — RSA-Key im JSON eingebettet, Key-Flow automatisch.
  Token-Flow ist tot (seit 2025-12-17).
- Control-Plane-Calls sind region-skopiert: `(ctx, projectId, region, …)`.
- Fehler-Status: `*oapierror.GenericOpenAPIError` → `.GetStatusCode()` (via `errors.As`).
- `CreateAccessKey` braucht leeren Payload (`NewCreateAccessKeyPayload()`), sonst Fehler.
- `DeleteAccessKey` braucht `credentials-group`-Param (Group-ID), sonst **500**. Group erst löschbar,
  wenn ihre Keys weg sind (sonst 422). Cleanup-Reihenfolge: Buckets → Keys → Groups.
- AccessKey-Response: `accessKey`=S3-Key-ID, `secretAccessKey`=Secret, `keyId`=interne Lösch-ID.
- S3-Endpoint eu01: `object.storage.eu01.onstackit.cloud`, **Path-Style**, **SigV4**
  (Host aus `Bucket.urlPathStyle` ableitbar).
- `objectstorage`-Top-Level-Paket ist **deprecated ab 2026-09-30** → später aufs versionierte Subpaket migrieren.

## Credentials-Secret (Vertrag)

Der Operator schreibt **Zugangsdaten + S3-Verbindungsparameter** ins referenzierte
Secret (`spec.secretRef.name`), damit sich anbindende Workloads ohne Zusatzconfig
verbinden können. Default-Keys sind **env-var-Style** (direkt via `envFrom` nutzbar):

| Default-Key             | Wert                                    | Quelle                        |
| ----------------------- | --------------------------------------- | ----------------------------- |
| `AWS_ACCESS_KEY_ID`     | S3 Access-Key-ID                        | `SecretValues.AccessKeyID`    |
| `AWS_SECRET_ACCESS_KEY` | S3 Secret                               | `SecretValues.SecretAccessKey`|
| `S3_BUCKET`             | Bucket-Name                             | `spec.bucketName`             |
| `S3_REGION`             | Region                                  | `GetRegion()` (Default eu01)  |
| `S3_ENDPOINT`           | Endpoint-Host (ohne Scheme)             | `SecretValues.Endpoint` (opt.)|
| `S3_BUCKET_URL`         | voller Path-Style-Bucket-URL            | `SecretValues.BucketURL` (opt.)|

- **Jeder Key-Name** ist pro Bucket via `spec.secretRef.keys.<feld>` überschreibbar
  (leeres Feld → Default). Logische Felder: `accessKeyID`, `secretAccessKey`,
  `bucketName`, `region`, `endpoint`, `bucketURL`.
- Helper in `api/v1/bucket_types.go` (Quelle der Wahrheit, vollständig unit-getestet):
  - `SecretKeys.<X>Key()` — resolved Key-Name (mit Default).
  - `Bucket.SecretData(SecretValues)` — baut die `map[string][]byte`-Secret-Data;
    optionale Felder (`endpoint`, `bucketURL`) nur bei nicht-leerem Wert.
  - `Bucket.ValidateSecretKeys()` — Fehler bei Key-Kollision (zwei Felder → selber Key,
    sonst stiller Datenverlust). Reconciler muss das **vor** dem Secret-Write prüfen.
- Default-Key-Konstanten: `Default*Key` in `api/v1/bucket_types.go`.

## Konventionen

- Integration-Tests hinter `//go:build integration`; Offline-Suite (`go test ./...`) bleibt netzfrei.
- Tests räumen erzeugte Cloud-Ressourcen via `t.Cleanup` ab (Control-Plane, unabhängig von Bucket-Policy).
- Test-Bucket-Namen: `s3op-test-<proj8>-<rand>` (DNS-konform, lowercase).
- Caveman-Mode im Chat aktiv; **Code/Commits/Docs normal** schreiben.

## Offene Fragen (vor Operator-Bau klären)

- **Q2:** Exakter Name der Minimal-Rolle (Object-Storage-Verwaltung, Projekt-Scope, nicht Owner).
- **Q4:** Bucket-Namensraum pro Projekt oder pro Region geteilt? (→ Präfix-Schema nötig?)
- Entschieden: Region `eu01`, Layer 1+2, Delete nur wenn leer, Keys ohne Ablauf (Details `INIT-SETUP.md` §0).

## Nächster Schritt

Skelett steht (CRD `Bucket`, Manager, Helm, CI). **Offen: Provisioning-Logik im Reconciler-Stub**
(`internal/controller/bucket_controller.go`) — die in `stackit/client.go` verifizierten Calls
verdrahten: `CreateBucket` → `CreateCredentialsGroup` → `CreateAccessKey` →
`ValidateSecretKeys()` + `SecretData(...)` ins `secretRef`-Secret schreiben →
`PutBucketPolicy` (Deny-Template §4.1); Finalizer-Teardown (nur wenn Bucket leer). Flow: `INIT-SETUP.md` §8.
Vorher Q2 (Minimal-Rolle) und Q4 (Bucket-Namensraum) klären.
</content>
