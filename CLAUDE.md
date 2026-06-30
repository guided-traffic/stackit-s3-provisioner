# CLAUDE.md — StackIT S3 Operator

Kubernetes-Operator (Go), der über die **StackIT Object Storage API** Buckets,
Workload-Zugangsdaten und Bucket-Policies provisioniert. Ein Operator-Deployment
pro Cluster, jeweils gebunden an **ein StackIT-Projekt** via Service-Account-Key.

**Phase:** Machbarkeit verifiziert (echte API-Tests grün). Noch **kein** Operator/CRD-Code —
nur ein getesteter API-Wrapper als Fundament. Detaillierte Findings: **`INIT-SETUP.md`** (Quelle der Wahrheit).

## Repo-Layout

```
stackit/client.go                        API-Wrapper (Auth, Bucket-/Group-/AccessKey-Ops, S3-Endpoint)
stackit/client_test.go                   Offline-Unit-Tests (Key-Parsing)
stackit/integration_test.go              //go:build integration — Layer-1 (Cross-Projekt-Isolation)
stackit/credentials_integration_test.go  //go:build integration — Layer-2 (Workload-Creds + echtes S3)
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

Kubebuilder-Scaffold + CRD `Bucket` (= Bucket + Credentials-Group + Access-Key + Deny-Policy in einer CR)
+ Reconciler, der die in `stackit/client.go` verifizierten Calls verdrahtet. Reconcile-/Finalizer-Flow: `INIT-SETUP.md` §8.
</content>
