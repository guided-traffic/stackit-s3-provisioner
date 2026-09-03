# CLAUDE.md — StackIT S3 Operator

Kubernetes-Operator (Go), der über die **StackIT Object Storage API** Buckets,
Workload-Zugangsdaten und Bucket-Policies provisioniert. Ein Operator-Deployment
pro Cluster, jeweils gebunden an **ein StackIT-Projekt** via Service-Account-Key.

**Phase:** Machbarkeit verifiziert (echte API-Tests grün). **Operator-Skelett + CI stehen**
(kubebuilder-Layout, `Bucket`-CRD, controller-runtime Manager, Helm-Chart, GitHub-Pages-Release,
Renovate, semantic-release — alle Checks grün). **Reconciler produktiv implementiert** (§8-Flow:
Admin-Bootstrap → Bucket → Credentials-Group → AccessKey+Secret → Deny-Policy; Finalizer-Teardown
nur wenn Bucket leer). Idempotent via Find-or-Create-by-Name (kein Leak über Crashes), Secret ist
Source-of-Truth fürs Live-Credential, Policy self-heilend bei Drift. **Ohne SA-Key = Skeleton-Mode**
(`Ready=NotImplemented`, kein Cloud-Call — envtest deckt das ab). Detaillierte Findings:
**`INIT-SETUP.md`** (Quelle der Wahrheit). Go-Modul: `github.com/guided-traffic/stackit-s3-provisioner`.

## Architecture Decision Records — das Grundgesetz dieses Repos

[`docs/adr/`](docs/adr/README.md) ist das Grundgesetz, mit dem in diesem Repo gebaut wird. Jede
dauerhafte Architekturentscheidung steht dort als ADR; Format und Index in
[`docs/adr/README.md`](docs/adr/README.md). Die ADRs sind bindend, nicht historisch: Code,
Helm-Chart und Doku sind Ausdruck der ADRs, nicht umgekehrt. CLAUDE.md beschreibt die ADRs
**nicht einzeln** — was gilt, steht im Index und in den ADRs selbst.

Verfahren fuer jede Arbeit in diesem Repo:

1. **Vor der Umsetzung gegen die ADRs pruefen.** Eine Funktion, ein Fix oder ein Umbau muss mit
   allen ADRs im Einklang stehen. Steht eine Anforderung im Widerspruch zu einem ADR, wird
   **nicht implementiert**, sondern der Konflikt benannt (welcher ADR, welche Regel `Dn`, was
   genau kollidiert) und eine **explizite Freigabe des Nutzers** zum Umschreiben des ADRs
   eingeholt. Erst mit der Freigabe wird der ADR geaendert — Amendment mit Datum in `Status`,
   neue Regel in `Decision`, alte Regel markiert statt geloescht — und dann der Code, im selben
   Change.
2. **Neue Architekturentscheidungen fuehren zu neuen ADRs, immer mit dem Nutzer abgestimmt.**
   Eine Entscheidung ist architektonisch, wenn sie kuenftige Aenderungen bindet: Trust-Boundary,
   Form der API/CRD, Loesch-, Fehler- oder Rotationssemantik, Betriebs- und Privilegienmodell,
   Migrationspfade. Wer beim Bauen auf so eine Entscheidung stoesst, entscheidet nicht still im
   Code, sondern stimmt Entscheidung und ADR-Text mit dem Nutzer ab und legt den ADR mit
   derselben Aenderung an.
3. **Vor einer Verhaltensaenderung den passenden ADR lesen**; das ist Teil der Aufgabe, nicht
   optional. Aeltere Entscheidungen (vor 2026-09-03) stehen in `INIT-SETUP.md` §0 und sind nicht
   als ADR nachgetragen.

## Repo-Layout

```
stackit/client.go                        API-Wrapper (Auth, Bucket-/Group-/AccessKey-Ops, S3-Endpoint, Find/EnsureGroup)
stackit/errors.go                        Fehler-Klassifikation: ProviderRefused / isServiceNotEnabled via json.Valid(Body)
stackit/retry.go                         Retry-RoundTripper (nur GET/HEAD) unter der SDK-Auth
stackit/s3.go                            Data-Plane: S3Admin (minio) Put/Get-Policy + BucketEmpty, BuildIsolationPolicy §4.1
stackit/client_test.go                   Offline-Unit-Tests (Key-Parsing)
stackit/s3_test.go                       Offline-Unit-Tests (Policy-Builder + Drift-Vergleich)
stackit/integration_test.go              //go:build integration — Layer-1 (Cross-Projekt-Isolation)
stackit/credentials_integration_test.go  //go:build integration — Layer-2 (Workload-Creds + echtes S3)
stackit/client_fake_test.go              Offline-Tests Control-Plane-Wrapper (gegen stackitfake)
stackit/s3_fake_test.go                  Offline-Tests Data-Plane inkl. WipeBucket (gegen stackitfake)
api/v1/bucket_types.go                    CRD `Bucket` (stackit-bucket.gtrfc.com/v1) + Helper, +kubebuilder-Marker
cmd/main.go                              controller-runtime Manager (stackit.Client + Admin-Secret-Name/-Namespace)
internal/controller/bucket_controller.go Reconciler (VOLL: §8-Provisioning + Admin-Bootstrap + Finalizer-Teardown)
internal/controller/breaker.go           Fleetweiter Provider-Circuit-Breaker (§8.4) + Workqueue-RateLimiter-Gegenstueck
internal/controller/clone.go             Bucket-Clone (spec.cloneFrom): rclone-Job, Staging-Secret, rc-Progress-Polling
internal/controller/bucket_usage_controller.go Groessen-Messung (spec.usage): eigener Controller, Merge-Patch auf status.usage
internal/controller/usage_config.go      Mess-Policy (Gate/Default/Intervall-Floor/Cap) + Kostenformel (720h, angefangene GB)
internal/controller/reconciler_grants_test.go Offline-Tests Read-Grants (spec.grantReadAccess) + Watch-Mapping
internal/controller/reconciler_degraded_test.go Offline-Tests Sticky-Ready (Halten, Grace-Ablauf, Auth-Ausnahme, Teardown)
internal/controller/reconciler_circuit_test.go Offline-Tests Circuit-Breaker (Trip, Hold ohne Churn, Grace, Recovery, Teardown)
internal/controller/breaker_test.go      Unit-Tests Breaker (Threshold, Reset, Probe-Backoff, Disabled)
internal/controller/reconciler_usage_test.go   Offline-Tests Groessen-Messung (Gate, Clamp, Cap, Versionen, Fehlerpfad)
internal/controller/reconciler_*_test.go Offline-Reconciler-Tests (fake k8s-Client + stackitfake, inkl. Fehlerpfade)
internal/stackitfake/                    In-Memory-Fake der StackIT-API (Control-Plane REST + S3-XML) für Offline-Tests
config/                                  kustomize: generierte CRD (crd/bases) + RBAC + Manager
deploy/helm/stackit-s3-provisioner/      Helm-Chart (CRD via `make sync-helm-crd` synchronisiert)
test/integration/                        //go:build integration — envtest gegen echten API-Server
test/e2e/e2e_test.go                     //go:build e2e — Kind-Smoke Skeleton-Mode (Operator healthy + CR reconciled)
test/e2e/cloud_test.go                   //go:build e2e — Kind gegen ECHTE API (E2E_STACKIT=1): Provisioning, Read-Grants, Groessen-Messung, Clone (echtes rclone)
hack/e2ecleanup/                         Sweep fuer Cloud-Reste eines abgebrochenen e2e-Laufs (inkl. verwaister Admin-Key)
Makefile / Containerfile / renovate.json CI-Gerüst (an Valkey-Operator orientiert)
.github/workflows/                       release.yml (Test+Release), build.yml (Docker+Helm), renovate.yml
account-1.json / account-2.json          SA-Keys (ECHTE RSA-Private-Keys, .gitignore'd, NIE committen)
INIT-SETUP.md                            Vollständige Findings, Policy-Templates, offene Fragen
docs/adr/                                Architecture Decision Records (README.md = Format + Index)
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
make e2e-local                  # Kind hochziehen, via Helm installieren, e2e-Smoke (Skeleton, kein Cloud-Call)
make e2e-stackit                # Kind + ECHTER SA-Key: legt reale Buckets/Groups/Keys an, raeumt garantiert ab
make e2e-stackit-sweep-dry      # zeigt Cloud-Reste eines abgebrochenen Laufs (nur Report)
make e2e-stackit-sweep          # loescht diese Reste inkl. verwaister operator-admin-Group
```

Integration-Tests treffen die **echte** StackIT-API (Projekte `ebc9d379…` und `5ad5e488…`,
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
6. **Ein `Bucket` wirkt nur auf seinen eigenen Namespace** — Regeln, Konsequenzen und die
   offene Verletzung stehen in [ADR 0001](docs/adr/0001-a-bucket-only-affects-its-own-namespace.md).

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
- `config.WithMaxRetries` ist seit core v0.26.0 ein **No-Op** (`func WithMaxRetries(_ int)`) — SDK retryt nicht.
  `config.WithHTTPClient` wird von `auth.SetupAuth` als *innerer* Transport uebernommen (auth.go:222),
  ein RoundTripper dort deckt API-Requests **und** Token-Fetches ab und laeuft unter der Auth.
- **Entzogener SA-Key = Token-Endpoint 400 `{"error":"invalid_grant"}`**, NICHT 401/403 der API — der
  Key-Flow erreicht die API gar nicht. Der SDK stempelt Token-Endpoint-Status/Body in denselben
  `GenericOpenAPIError`. Live verifiziert 2026-08-25.
- **Fehler-Diskriminator ist `json.Valid(apiErr.Body)`, nicht der Statuscode.** Ein Gateway/WAF erzeugt
  denselben `*oapierror.GenericOpenAPIError` mit HTML-Body. `oapierror.Model` taugt NICHT: der SDK
  dekodiert jeden Body in `objectstorage.ErrorMessage` (nur `Detail`), fremdgeformtes JSON landet
  fehlerfrei in einer leeren Struct. Live verifiziert 2026-08-25 (INIT-SETUP.md §8.2).
- `GetServiceStatus`: 200 = aktiviert, strukturiertes **404** = nicht aktiviert, strukturiertes **403** =
  nicht berechtigt. Nur beim 404 darf `EnableService` folgen.
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
- Secret liegt **immer** im Namespace des Bucket-CR, mit Controller-OwnerRef (GC mit dem CR).
  `spec.secretRef.namespace` wurde 2026-09-03 entfernt ([ADR 0001](docs/adr/0001-a-bucket-only-affects-its-own-namespace.md) D1/D4) — ein
  Cross-Namespace-Ziel war ein Secret-Write/Delete-Primitiv (Sicherheits-Befund 2, behoben).

## Konventionen

- Integration-Tests hinter `//go:build integration`; Offline-Suite (`go test ./...`) bleibt netzfrei.
- Tests räumen erzeugte Cloud-Ressourcen via `t.Cleanup` ab (Control-Plane, unabhängig von Bucket-Policy).
- Test-Bucket-Namen: `s3op-test-<proj8>-<rand>` (DNS-konform, lowercase).
- Caveman-Mode im Chat aktiv; **Code/Commits/Docs normal** schreiben.

## Offene Fragen (vor Operator-Bau klären)

- **Q2:** Exakter Name der Minimal-Rolle (Object-Storage-Verwaltung, Projekt-Scope, nicht Owner).
- **Q4:** Bucket-Namensraum pro Projekt oder pro Region geteilt? (→ Präfix-Schema nötig?)
- Entschieden: Region `eu01`, Layer 1+2, Delete nur wenn leer, Keys ohne Ablauf (Details `INIT-SETUP.md` §0).

## Reconciler-Design (implementiert)

- **Admin-Bootstrap (`ensureAdmin`):** einmalige `operator-admin`-Credentials-Group + S3-Key,
  persistiert im operator-eigenen Secret (`--admin-credentials-secret-name`, Default
  `stackit-s3-provisioner-admin`, in `POD_NAMESPACE`). Deren URN steht in **jeder** Bucket-Policy
  (`NotPrincipal`, Lockout-Schutz). Fehlt/unvollständig → Find-or-Create-Group + Keys-clear + neuer Key.
- **Provisioning (`reconcileNormal`):** `ValidateSecretKeys` → Admin-Secret-Guard → Region-Guard →
  `ensureAdmin` → `EnsureService` → Bucket (idempotent by name) → `BucketConnInfo` → Workload-Group
  (Find-or-Create by deterministischem Namen `s3op-<ns>-<name>-<uid8>`) → AccessKey+Secret → Policy.
- **AccessKey/Secret:** Secret ist Source-of-Truth. Hat Secret Creds **und** Group ≥1 Key → skip.
  Sonst: **erst alle Group-Keys löschen, dann neuen Key + Secret schreiben** (leak-frei, da Clear
  vor Create); scheitert Secret-Write → neuen Key sofort löschen (Secret unrecoverable).
- **Key-Rotation (Annotation):** `stackit-bucket.gtrfc.com/rotate-credentials-at: "<RFC3339>"` —
  Wert ≠ `status.lastRotationTrigger` → harte Rotation (Skip-Pfad übersteuert, alter Key sofort tot,
  Workloads müssen Secret neu lesen). Handled-Wert + Zeit in Status (level-triggered, GitOps-safe:
  Operator mutiert Annotation nie), Event `CredentialsRotated`.
- **Policy (`ensureBucketPolicy`):** `BuildIsolationPolicy` (§4.1), nur bei Drift neu setzen
  (`PoliciesEquivalent`). Self-healing gegen manuelle Änderungen.
- **Read-Grants (`spec.grantReadAccess`, INIT-SETUP.md §4.1.1):** Producer-Seite — der
  Daten-Bucket listet `Bucket`-CRs **seines Namespace**, deren Workload-Group nur-lesend
  darf. Drittes Policy-Statement + Reader in Stmt-1-`NotPrincipal`; ohne Grant Dokument
  byte-identisch zu vorher (kein Rewrite beim Upgrade). Reader-URN kommt **nie** aus
  `status.credentialsGroupURN` (fälschbar via `buckets/status`), sondern aus
  `workloadGroupName(grantee)` → Control-Plane-Lookup; `BuildIsolationPolicy` filtert
  Admin- + Workload-URN zusätzlich raus (Lockout- bzw. Owner-Verengungs-Schutz).
  Unauflösbarer Grant = Skip + Event `ReadGrantPending`, blockiert `Ready` nicht.
  Mehrdeutiger Group-Displayname (mehrfach im Projekt) = Grant **verweigert**, nicht geraten.
  **Während eines laufenden Clones bleiben Reader aus der Policy** (`holdSecretUntilCloned`
  schützt nur den eigenen Workload; ein Reader hat schon Credentials) — nach Clone-Erfolg
  wird die Policy im selben Pass mit Readern neu geschrieben.
  Grantee publiziert `status.credentialsGroupURN` **sofort** nach Group-Create, nicht erst
  bei Ready — sonst weckt ein selbst noch klonender Grantee seine Grantoren nie.
  Zweiter Bucket-Watch (`granteeCredentialsPredicate`, nur Create/Delete/URN-Wechsel/
  Deletion-Start) weckt Grantoren — sonst Hot-Loop über Status-Writes.
  Status: `status.grantedReadTo`. Self-Grant per Root-CEL abgelehnt (envtest-verifiziert).
- **Finalizer-Teardown:** Empty-Check **zuerst** (Admin-S3, Data-Loss-Guard) → dann Keys → Group →
  Bucket → Secret. Shared Admin-Group wird **nie** angefasst. Opt-in-Wipe: `spec.wipeOnDelete`
  löscht vorher alle Objekte (inkl. Versions/Delete-Markers, `S3Admin.WipeBucket`) — nur wenn
  Feature-Gate an (`--enable-wipe-on-delete` / Helm `wipeOnDelete.enabled`, Default aus) **und**
  Ownership-Tags passen; sonst Degradierung auf Empty-Only + Warning-Event `WipeOnDeleteSkipped`.
- **Provider-Circuit-Breaker (§8.4, implementiert 2026-09-02):** ein Provider-Ausfall ist
  Eigenschaft des Providers, nicht eines Buckets. Nach `--provider-circuit-threshold` (3)
  Reconciles, die **ohne dazwischenliegenden Erfolg** scheitern, ruft der Operator die API
  gar nicht mehr, haelt alle Buckets und probet mit verdoppelndem Cooldown (60s → 5m Cap,
  `--provider-circuit-max-cooldown`). Diskriminator ist bewusst **kein** Fehler-Parsing,
  sondern das Ausbleiben eines Erfolgs — ein einzelner kaputter Bucket ist mit den Erfolgen
  der Flotte verschraenkt und haelt niemanden auf. Offen: kein Provider-Call (auch nicht im
  Teardown, Finalizer bleibt), Reconcile liefert `RequeueAfter` **ohne Fehler** (Log/Event/
  `status.message` unveraendert), `degradedSince` einmal geschrieben statt pro Probe, Grace
  laeuft weiter. `threshold: 0` = aus (Values-only-Rollback). Metriken
  `..._provider_circuit_open` / `..._provider_circuit_opened_timestamp_seconds`.
  Begleitend: Workqueue-RateLimiter (1s → 15min, fleetweit 1 qps/Burst 5 statt 5ms/10 qps),
  `retryTransport` retryt **429 nicht mehr** (Rate-Limit erneut anfragen vertieft ihn),
  `IdleConnTimeout` 30s gegen `connection reset by peer` auf gepoolten Verbindungen.
- **Transiente Provider-Fehler (§8.2, implementiert 2026-08-25):** `Ready` beschreibt den zuletzt
  **verifizierten** Zustand, nicht das Ergebnis des letzten Verifikationsversuchs. `fail` haelt `Ready`
  eines provisionierten Buckets, `degrade` schreibt stattdessen `status.degradedSince` +
  `ProviderReachable=False`; nach `--provider-degraded-grace` (Helm `providerDegradedGrace`, Default 30m,
  `0` = aus) faellt der Bucket wie vorher auf `Failed`. Klassifikation **nach Herkunft**: `failNoRequeue`
  = definitiv (Spec-Guards, Ownership-Collision, `validateCloneSource`), `fail` = nicht-definitiv.
  Unbekannte Fehler sind per Konstruktion nicht-definitiv. **Ausnahmen** (fallen sofort):
  `stackit.ProviderRefused` = strukturiertes **400/401/403** — 400 ist das Fehlerbild eines entzogenen
  SA-Keys (Token-Endpoint `invalid_grant`, der Key-Flow erreicht die API nie; live verifiziert
  2026-08-25); `errCredentialDestroyed` = Key geloescht, Ersatz nicht publiziert (lokale Gewissheit,
  ueber Rotations-Annotation ohne Generations-Bump erreichbar); Teardown;
  `ObservedGeneration != Generation`; nie-Ready. Fehler wird weiterhin zurueckgegeben, also feuert
  `StackitS3ReconcileErrors` unveraendert; zusaetzlich `StackitS3BucketProviderDegraded`.
- **Bucket-Groesse + Kosten (`spec.usage`, INIT-SETUP.md §8.3):** eigener
  `BucketUsageReconciler` (eigene Workqueue, `bucketUsage.concurrency`), misst per
  vollstaendigem S3-Listing mit dem Admin-Key und schreibt **nur** `status.usage`
  per Merge-Patch. Faelligkeit kommt aus `status.usage.lastMeasurementTime`
  (ueberlebt Restarts), naechster Lauf per `RequeueAfter` + deterministischem
  Skew; Watch ist generation/annotation-gefiltert (sonst Hot-Loop durch eigene
  Status-Writes). Zwei Helm-Schalter: `bucketUsage.enabled` = harter Gate,
  `bucketUsage.defaultEnabled` = Default fuer CRs ohne eigene Angabe (aus).
  Guards sind **Zeit**-Guards, nicht Geld: Messen kostet bei StackIT nichts
  (Abrechnung nur per angefangenem GB/h, keine Request-Position — verifiziert
  gegen Preisliste v1.0.43 + Leistungsschein v1.2), aber ~1 Request je 1000 Keys.
  Daher `minInterval` (60m, CR-Wunsch wird hochgeklemmt) und `maxObjects` (2 Mio,
  danach `truncated` → alle Werte sind untere Schranken, `>=`-Praefix).
  Kosten = `ceil(bytes/1e9)` × `pricing.perGBHour` × 720h, auf Cent gerundet.
  **Messfehler beruehren `Ready` nie** und geben keinen Reconcile-Error zurueck.
- **Guards (produktionssicher):** CR darf `secretRef` **nicht** aufs Admin-Secret zeigen (sonst
  Pollution + Admin-Lockout beim Delete); `spec.region` muss = Operator-Region sein (Single-Region v1).
  Beides → `Ready=Failed` ohne Requeue-Hammer.
- **Bucket-Clone (`spec.cloneFrom`, INIT-SETUP.md §8.1):** einmaliger Copy eines fremden S3-Buckets
  via rclone-**Job** im Operator-NS (Image Helm `clone.image`). Quell-Creds aus User-Secret (nur
  CR-Namespace, Keys via `secretRef.keys` konfigurierbar), Ziel = Admin-Key. Quelle default
  path-style, `addressingStyle: virtual-hosted` für AWS-Stil (Ziel bleibt path-style). Default
  `holdSecretUntilCloned: true`: Workload-Secret erst nach Clone-Erfolg (Flow: Bucket → Policy →
  Clone → Key+Secret); `Ready` wartet immer auf den Clone. Fortschritt via rclone-rc (`--rc`,
  Basic-Auth 32-Zeichen-Passwort im Staging-Secret `…-src`, Helm-NetworkPolicy auf Port 5572) →
  `status.clone.progress` („2.0 GiB / 18.0 GiB (11%)“), Poll alle 15s. Clone-once (`Completed`
  terminal), Failed-Job → Delete + Backoff-Retry (rclone resumed). **Bucket-Watch filtert auf
  Generation/Annotation** (sonst Hot-Loop durch Progress-Writes) — Finalizer-Add requeued explizit.

## Sicherheits-Befunde (verifiziert 2026-08-24, PRÄEXISTENT — nicht vom Grant-Feature eingeführt)

1. **`workloadGroupName` kollidiert über Namespaces** (`internal/controller/bucket_controller.go`).
   `("s3op-"+ns+"-"+name)[:23]` + 8-Hex-FNV-1a-32. Empirisch reproduziert:
   `("gitlab","gitlab-artifacts")` == `("gitlab-gitlab","artifacts787ngo")` ==
   `s3op-gitlab-gitlab-arti-70dbcfc2`. `EnsureCredentialsGroup` ist Find-or-Create
   **ohne** Ownership-Check (der Tag-Guard schützt nur Buckets) → fremdes CR adoptiert
   die Gruppe, `ensureAccessKeyAndSecret` löscht den Live-Key des Opfers und schreibt
   einen neuen ins eigene Secret. Fix = längerer/kryptographischer Suffix ⇒ **Migration**
   (alle bestehenden Gruppen würden umbenannt, alte Gruppen + Keys verwaisen). Offen.
2. **`spec.secretRef.namespace` ungeprüft** — **BEHOBEN 2026-09-03** ([ADR 0001](docs/adr/0001-a-bucket-only-affects-its-own-namespace.md)):
   Feld aus der CRD entfernt, `SecretNamespace()` gelöscht, Secret liegt strukturell in
   `b.Namespace` (immer mit Controller-OwnerRef), `bucketsForSecret` listet nur noch den
   Namespace des Secrets. Vorher: `upsertSecret` merged in ein beliebiges Secret jedes
   Namespace, `deleteSecret` löschte es beim CR-Delete → Cross-Namespace-Write/Delete-Primitiv
   für jeden, der ein Bucket-CR anlegen darf. Migration: ein Bestands-CR mit gesetztem Feld
   wird vom API-Server auf den eigenen Namespace gepruned → Operator rotiert den Key in ein
   neues Secret im CR-Namespace, das alte Fremd-Secret verwaist mit totem Key (manuell löschen).

Befund 1 untergräbt weiterhin die Prämisse „Namespace = Trust-Boundary". Doc-Kommentare in
`bucket_types.go`, `resolveReadGrants`, README und INIT-SETUP §4.1.1 sind entsprechend
entschärft — sie behaupten keine Garantie mehr, die der Mechanismus nicht hergibt.

## Nächster Schritt

Reconciler steht, alle Offline/lint/gosec/envtest-Checks grün. **Erledigt:** End-to-End-Provisioning
über den Reconciler gegen die echte StackIT-API (`make e2e-stackit`, Kind + echter SA-Key) inkl.
Read-Grants; Layer-2-Policy-Enforcement mit 3 Statements direkt gegen StorageGRID
(`go test -tags integration ./stackit/ -run IntegrationReadGrant`).
**Erledigt (2026-08-25):** Transiente Provider-Fehler (INIT-SETUP.md §8.2) — EnsureService eskaliert
keinen fehlgeschlagenen Read mehr zu einem Write + prozessweiter Cache, Retry-RoundTripper fuer GET/HEAD
unter der SDK-Auth, Sticky-`Ready` mit begrenztem Grace. Ticket `local_s3-provisioner-transient-errors.md`.
**Erledigt (2026-09-01):** Bucket-Groesse + Monatskosten-Schaetzung an der CR (`spec.usage`,
INIT-SETUP.md §8.3) — eigener Mess-Controller, Helm-Gate + Cluster-Default, Intervall-Floor,
Objekt-Cap, alle Werte als Metriken, zwei neue Alerts.
**Erledigt (2026-09-02):** Provider-Circuit-Breaker + Alarm-Retune (INIT-SETUP.md §8.4).
Befund read-only auf mgmt-p: 503-Storm des StackIT-Edge um 14:23 UTC, ab 14:34 zusaetzlich
`429 rate limit on IP level exceeded` — der Operator hat sein eigenes IP-Limit vollgehaemmert
(kein Workqueue-RateLimiter, `retryTransport` retryte 429 dreifach pro GET). 242 Reconcile-Fehler
fuer **einen** selbstheilenden Ausfall, Alarmschwelle war `>3 / 15m` mit `for: 0`.
`StackitS3ReconcileErrors` unterdrueckt jetzt Fenster mit offenem Circuit (`unless on()`,
fail-open bei fehlender Metrik); `StackitS3BucketProviderDegraded` alarmiert auf das **Alter**
des Haltens (`> holdForSeconds`, Default 1200s < Grace 30m) statt auf dessen Existenz.
**Erledigt (2026-09-01):** e2e gegen die echte API nachgezogen (`make e2e-stackit`, neue SA-Keys,
kompletter Lauf gruen in 658s, Sweep danach leer): `TestCloudBucketUsage` (Messung + Kosten +
Clear beim Abschalten), `TestCloudBucketUsageWithVersions` (Versions-Listing auf einem
nicht-versionierten Bucket — live verifiziert, dass StorageGRID das beantwortet und `IsLatest`
korrekt setzt), `TestCloudClone` (echtes rclone-Image, Quelle ist ein zweites Bucket-CR:
Hold-Invariante, byte-genauer Inhalt, Clone-once). rclone-Image wird im Make-Target aus dem
gerenderten Chart gelesen und in Kind vorgeladen (kein dritter Pin, kein Registry-Pull im Test).
**Offen:** (1) Sicherheits-Befund 1 oben (Group-Namenskollision; Befund 2 behoben 2026-09-03);
(2) Q2 (Minimal-Rolle),
Q4 (Bucket-Namensraum).
RBAC/Helm: Operator braucht Secret-CRUD im eigenen NS (Admin-Secret) — bereits von den cluster-weiten
Secret-RBAC-Markern abgedeckt.
</content>
