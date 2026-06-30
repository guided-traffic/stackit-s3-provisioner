# StackIT S3 Operator — Machbarkeit & Initial Setup

> Lebendiges Findings-Dokument. Wird bei jedem Arbeitsschritt fortgeschrieben.
> Stand: 2026-06-30 · Phase: Machbarkeitsprüfung (noch kein Code)

## 0. Getroffene Entscheidungen (2026-06-30)

| Thema | Entscheidung |
|---|---|
| Tenant-Modell | **Layer 1 + Layer 2** — auch Workload↔Workload-Isolation im selben Cluster. Bucket-Policies sind Pflicht. |
| Region | **`eu01`** (Single-Region, v1). Code dennoch region-parametrisiert. |
| Lösch-Semantik | **Nur löschen wenn Bucket leer** — sonst Reconcile-Fehler, kein Datenverlust. |
| Key-Rotation | **v1: Keys ohne Ablauf**, keine Auto-Rotation. Rotation später nachrüstbar. |

## 1. Ziel

Kubernetes-Operator, der per Custom Resources in mehreren Clustern **Buckets**,
**Workload-Accounts** (Zugangsschlüssel) und **Policies** über die StackIT
Object-Storage-API anlegt.

Harte Anforderung: Der Provisioner-Service-Account von Cluster A darf die Buckets,
Policies und Workload-Accounts von Cluster B **weder sehen noch verändern** können —
und umgekehrt. Die beiden Provisioner-SAs liegen in **unterschiedlichen
StackIT-Projekten** derselben Organisation.

## 2. Kernergebnis (TL;DR)

**Machbar — ja.** Die Isolations-Anforderung ist durch StackIT **strukturell erfüllt**,
nicht erst durch Operator-Logik:

- Object Storage ist in StackIT **streng projekt-gebunden**. Jeder API-Aufruf trägt
  `projectId` + `region`. Das SA-Token trägt nur Rollen **im eigenen Projekt**.
- StackIT-Doku wörtlich: *"If you need to separate the access to the data on the
  object storage for different users you would need to create multiple projects."*
- Operator A (Projekt-A-SA) kann Projekt-B-Ressourcen nicht auflisten oder ändern →
  ohne Rolle in Projekt B liefert jeder Aufruf **403**. Das ist vom Operator **nicht
  umgehbar**.

→ Ein Operator-Deployment pro Cluster, jeweils mit dem projekt-eigenen SA-Key.
Die Cross-Projekt-Isolation (Layer 1) ist damit gegeben.

**Eine kritische Bedingung** (siehe §5): Die Provisioner-SAs dürfen **ausschließlich
auf Projekt-Ebene** berechtigt werden — niemals über eine Organisations-Rolle, die in
beide Projekte durchschlägt. Sonst bricht die Isolation.

## 3. StackIT Object Storage — Architektur

Backend ist **NetApp StorageGRID** (erkennbar an Policy-URNs `urn:sgws:...`).
Zwei getrennte Ebenen:

| Ebene | Protokoll | Auth | Wer ruft auf | Operationen |
|---|---|---|---|---|
| **Control Plane** | STACKIT API (Go SDK) | SA Key-Flow (Bearer-Token) | **Operator** | Bucket / Credentials-Group / Access-Key anlegen+löschen, Service aktivieren |
| **Data Plane** | S3 API (SigV2/V4) | Access-Key + Secret | Operator **und** Workloads | Objekte put/get, **Bucket-Policy setzen** |

Wichtig: **Bucket-Policies gehören zur Data Plane** (S3 `PutBucketPolicy`), nicht zur
Control Plane. Das SDK kann Buckets/Keys anlegen, aber **keine Policy schreiben**.
Daraus folgt eine Architektur-Konsequenz (siehe §6, offene Frage Q3).

### Datenmodell (bestätigt via Go SDK v1.9.0 + Terraform-Provider-Schema)

```
Projekt (StackIT)  ── Isolationsgrenze ──
  └── Object Storage Service (muss pro Projekt aktiviert sein: EnableService)
        ├── Bucket
        │     name (DNS-konform), region
        │     url_path_style / url_virtual_hosted_style
        │     object_lock (nur bei Erstellung setzbar)
        │     └── Bucket-Policy (S3, Principal = Credentials-Group-URN)
        └── Credentials Group ("Workload-Account")
              credentials_group_id, urn, name
              └── Credential / Access Key
                    access_key (public), secret_access_key (sensitiv, nur 1× bei Create!)
                    expiration_timestamp (optional; unset = läuft nie ab)
```

### Go SDK — verfügbare Control-Plane-Operationen

`github.com/stackitcloud/stackit-sdk-go/services/objectstorage` (v1.9.0):

- Buckets: `CreateBucket` / `DeleteBucket` / `ListBuckets` / `GetBucket`
- Groups: `CreateCredentialsGroup` / `DeleteCredentialsGroup` / `ListCredentialsGroups` / `GetCredentialsGroup`
- Keys: `CreateAccessKey` / `DeleteAccessKey` / `ListAccessKeys`
- Service: `EnableService` / `DisableService` / `GetServiceStatus`
- Lock/Retention: `*ComplianceLock`, `*DefaultRetention`

Alle Aufrufe: `(ctx, projectId, region, ...)`.

### Authentifizierung des Operators (Key-Flow)

- **Token-Flow wurde am 2025-12-17 abgeschaltet** → wir **müssen** Key-Flow nutzen.
- SA-Key-JSON enthält `id`, `publicKey`, `credentials{kid,iss,sub,aud,privateKey}`.
- Operator signiert JWT mit privatem RSA-Key → StackIT gibt kurzlebiges Bearer-Token
  (~600 s), SDK refresht automatisch.
- SDK-Config: `WithServiceAccountKeyPath(...)` (+ ggf. `WithPrivateKeyPath(...)`) oder
  Env `STACKIT_SERVICE_ACCOUNT_KEY_PATH` / `STACKIT_PRIVATE_KEY_PATH`.
- Im Cluster: SA-Key als K8s-Secret, in den Operator-Pod gemountet.

## 4. Isolations-Analyse

### Layer 1 — Cross-Projekt / Cross-Cluster (die harte Anforderung)

**Strukturell garantiert.** Jeder Control-Plane-Call ist projekt-skopiert; das
SA-Token besitzt nur projekt-lokale Rollen. Kein Operator-Bug kann Projekt B erreichen,
solange der SA keine Rolle in Projekt B hat. Workload-Accounts (= Credentials Groups)
und deren Policies liegen ebenfalls im Projekt → automatisch mit-isoliert.

→ **Erfüllt die gestellte Anforderung vollständig.**

### Layer 2 — Intra-Projekt (Workload ↔ Workload im selben Cluster)

Innerhalb **eines** Projekts gilt per Default: *"all project members can access all
data within that project's object storage"*. Trennung zwischen einzelnen Workloads
desselben Clusters entsteht **nicht automatisch**, sondern nur über:

1. pro Workload eine **eigene Credentials Group** + eigenen Access Key, und
2. eine **Bucket-Policy**, die Zugriff auf genau diese Group beschränkt
   (Principal = Group-URN), optional + Source-IP-Bedingung.

Das ist Operator-Verantwortung und der fehleranfälligere Teil. **Layer 2 ist
gefordert** (Entscheidung §0) → Policy-Design in §4.1.

### 4.1 Bucket-Policy-Modell & Bootstrap (Layer 2)

**Korrektur (empirisch, 2026-06-30):** StackIT-Default ist **offen** — *jede*
Credentials-Group eines Projekts darf per Default *alles* in *jedem* Bucket des Projekts.
Ein reines `Allow` für die Workload-Group reicht **nicht** zur Trennung (Allow ist
additiv, sperrt niemanden aus). Restriktion erfordert **explizites `Deny`**. Bestätigt
durch Test (§9). Das frühere Allow-Template war falsch und ist unten ersetzt.

**Henne-Ei (Q3 gelöst):** Bucket-Policies setzt man über die **S3-Data-Plane**
(`PutBucketPolicy`), nicht über das SDK. Der Operator braucht also pro Projekt **einen
S3-Admin-Key**. Lösung — einmaliger **Bootstrap** beim ersten Start je Projekt:

1. Control Plane: `CreateCredentialsGroup` → `operator-admin`, dann `CreateAccessKey`
   darin. Den S3-Key in einem operator-eigenen Secret persistieren.
2. Frisch erstellte Buckets haben noch **keine** Policy → der Account-eigene Admin-Key
   (gleiches Projekt) darf sie verwalten. Damit kann der Operator die erste Policy setzen.

**Validiertes Policy-Template je Bucket** (zwei `Deny`-Statements):

```jsonc
{
  "Statement": [
    { // 1) Outsider raus: alle Principals AUSSER admin+workload komplett denied
      "Sid": "deny-all-except-admin-and-workload",
      "Effect": "Deny",
      "NotPrincipal": { "AWS": ["<urn operator-admin-group>", "<urn workload-group>"] },
      "Action": ["s3:*"],
      "Resource": ["arn:aws:s3:::<bucket>", "arn:aws:s3:::<bucket>/*"]
    },
    { // 2) Workload auf Objekt-Ops begrenzen: alles AUSSER Objekt-Ops denied
      "Sid": "workload-objects-only",
      "Effect": "Deny",
      "Principal": { "AWS": "<urn workload-group>" },
      "NotAction": ["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:ListBucket","s3:GetBucketLocation"],
      "Resource": ["arn:aws:s3:::<bucket>", "arn:aws:s3:::<bucket>/*"]
    }
  ]
}
```

- Stmt 1 (`Deny`+`NotPrincipal`) sperrt jede fremde Group aus → Workload↔Workload-Trennung.
  Admin-Group steht in der Ausnahmeliste → **kein Lockout** (StorageGRID kann sonst sogar
  Account-Root aussperren). Provisioner-SA löscht Buckets ohnehin über die Control Plane,
  unabhängig von der Policy — zweites Sicherheitsnetz.
- Stmt 2 (`Deny`+`NotAction`) begrenzt die Workload-Group auf Objekt-Operationen; explizites
  Deny schlägt das Default-`Allow` → **kein Bucket-Management** (kein PutBucketPolicy/DeleteBucket).
- Principal/`NotPrincipal` = Group-**URN** (`urn:sgws:identity::…:group/…`, aus
  `CreateCredentialsGroup`); Resource = `arn:aws:s3:::…`. Mischung ist korrekt (StorageGRID).

## 5. Kritische Guardrails

1. **Rollen nur auf Projekt-Ebene zuweisen.** Eine Organisations-weite Rolle, die in
   beide Projekte kaskadiert, hebelt Layer 1 aus. → Bei Account-Einrichtung prüfen.
2. **`secret_access_key` nur einmal abrufbar** (bei Create). Operator muss ihn sofort in
   ein K8s-Secret schreiben; sonst Key löschen + neu erzeugen.
3. **Minimal-Rolle für den Provisioner-SA** (nicht Projekt-Owner). Exakter Rollenname
   noch zu klären → Q2.
4. **Default-Credentials-Group hat breiten Zugriff.** Für Layer 2 nie die Default-Group
   an Workloads geben — immer dedizierte Groups + restriktive Policy.

## 6. Offene Fragen (zu klären, bevor Code beginnt)

**Offen:**

| # | Frage | Warum wichtig | Wer |
|---|---|---|---|
| Q2 | Exakter Name der **Minimal-Rolle** für Object-Storage-Verwaltung auf Projekt-Ebene? | Least-Privilege für Provisioner-SA | Du / StackIT |
| Q4 | Bucket-Namen eindeutig **pro Projekt** oder **pro Region** (über Projekte geteilt)? | Bei region-global: Namens-Kollision = Info-Leak + Create-Fehler → Präfix-Schema nötig | Du / StackIT |
| Q6 | Service-Aktivierung (`EnableService`) **manuell** pro Projekt oder durch Operator-Bootstrap? | Bootstrap-Reihenfolge | Du |

**Geklärt:**

| # | Frage | Ergebnis |
|---|---|---|
| Q1 | Layer 2 nötig? | **Ja** — siehe §0, §4.1 |
| Q3 | Welches Credential setzt Policies? | **Operator-Admin-S3-Key** pro Projekt via Bootstrap, §4.1 |
| Q5 | Region | **`eu01`** |
| Q7 | Key-Rotation | **v1: kein Ablauf**, später nachrüstbar |
| Q8 | Lösch-Semantik | **Nur wenn Bucket leer** |

## 7. Was du besorgst (Checkliste)

- [ ] 2 StackIT-**Projekte** (gleiche Org) — je 1 pro Test-Cluster
- [ ] Pro Projekt: Object Storage **aktiviert**
- [ ] Pro Projekt: 1 **Service-Account** `s3-bucket-provisioner`
- [ ] Pro SA: **projekt-skopierte** Object-Storage-Verwaltungsrolle (Q2), **keine** Org-Rolle
- [ ] Pro SA: **SA-Key (Key-Flow / RSA)** als JSON exportiert
- [ ] Klärung Q2 (Rollenname), Q4 (Bucket-Namensraum), Q6 (Service-Aktivierung)

## 8. Geplante Operator-Architektur (Entwurf)

- **Sprache/Framework:** Go + Kubebuilder/controller-runtime (Operator-SDK).
- **Deployment:** 1 Operator-Instanz pro Cluster, SA-Key des jeweiligen Projekts als
  gemountetes Secret. `projectId` + `region=eu01` statisch je Instanz (Config/Env).
- **Bootstrap (1× je Projekt):** `operator-admin`-Credentials-Group + S3-Key anlegen,
  in operator-eigenem Secret persistieren (für `PutBucketPolicy`, §4.1).

**CRD `Bucket` (eine CR = ein isolierter Workload, Layer 2):**

```yaml
spec:
  name: my-bucket            # DNS-konform; Präfix-Schema offen (Q4)
  secretRef:                 # wohin access_key/secret geschrieben werden
    namespace: team-a
    name: my-bucket-s3
status:
  bucketUrl, credentialsGroupId, credentialsGroupUrn, accessKeyId
  conditions: [Ready, ...]
```

Eine CR kapselt: Bucket + dedizierte Credentials-Group + Access-Key + Bucket-Policy.
Das hält Layer-2-Isolation pro CR zusammen und vermeidet verwaiste Groups.

**Reconcile-Flow:**
1. Control Plane: `CreateBucket` (idempotent via `GetBucket`).
2. Control Plane: `CreateCredentialsGroup` (workload) → URN merken.
3. Control Plane: `CreateAccessKey` in der Group → `secret_access_key` **sofort** in
   `secretRef`-Secret schreiben (nur 1× verfügbar).
4. Data Plane (operator-admin-Key): `PutBucketPolicy` mit Template aus §4.1.
5. Status + Conditions setzen.

**Finalizer (Lösch-Semantik §0 = nur wenn leer):**
1. Bucket-Inhalt prüfen → nicht leer ⇒ Reconcile-Fehler, Bucket bleibt, Event/Condition.
2. Leer ⇒ `DeleteAccessKey` → `DeleteCredentialsGroup` → `DeleteBucket` → Secret entfernen.

**Idempotenz/Drift:** Jeder Reconcile gleicht Ist (List/Get) gegen Soll ab; Policy wird
bei Abweichung neu gesetzt (Self-Healing gegen manuelle Änderungen).

## 9. Machbarkeits-Smoke-Test — VERIFIZIERT (2026-06-30)

Minimaler Go-Code gegen die **echte** StackIT-API, mit beiden Service-Account-Keys
(`account-1.json` = Projekt `1f426c6e…`, `account-2.json` = Projekt `eb4205a7…`).

**Layer 1 — Cross-Projekt (Control Plane), beide Tests grün:**

| Test | Ergebnis |
|---|---|
| API-Zugriff + Bucket-Anlage (beide Accounts) | ✅ beide legen Bucket im eigenen Projekt an + sehen ihn |
| Cross-Projekt **LIST** (A→B und B→A) | ✅ **HTTP 403** — explizites Deny, kein Daten-Leak |
| Cross-Projekt **CREATE** (A→B und B→A) | ✅ **HTTP 403** |
| Cross-Projekt **DELETE** des fremden Buckets | ✅ **HTTP 403**, Opfer-Bucket überlebt |

→ Anforderung *"weder sehen noch verändern"* (Cross-Projekt) **empirisch bestätigt** —
real per 403 in beide Richtungen. Q6 für Test erledigt (Object Storage manuell aktiviert).

**Layer 2 — Workload↔Workload im selben Projekt (Data Plane / echtes S3), grün:**

Provisioner legt 2 Buckets + je eine Workload-Credentials-Group/Access-Key an, setzt die
Deny-Policy aus §4.1, dann echter S3-Zugriff via `minio-go`:

| Test | Ergebnis |
|---|---|
| Workload A: PutObject + GetObject im **eigenen** Bucket | ✅ schreibt + liest zurück |
| Workload B: Read/List/Write auf **Bucket A** | ✅ **AccessDenied (403)** |
| Workload A: **Management** von Bucket A (SetBucketPolicy, RemoveBucket) | ✅ **AccessDenied (403)** |
| Workload A: Zugriff auf **Bucket B** | ✅ **AccessDenied (403)** |

→ Per-Workload-Isolation + „nur Objekt-Rechte, kein Management" **empirisch bestätigt**.
Damit ist das vollständige CRD-`Bucket`-Verhalten (Bucket + Group + Key + Policy) praktisch
durchgespielt. Testressourcen werden automatisch aufgeräumt (verifiziert: 0 Reste).

**Code-Layout:**

```
stackit/client.go                        Wrapper: LoadAccount, NewClient (Key-Flow),
                                         EnsureService, Bucket-/CredentialsGroup-/AccessKey-Ops,
                                         BucketEndpointHost, List*, StatusCode
stackit/client_test.go                   Offline: Key-Parsing (kein Netz)
stackit/integration_test.go              //go:build integration — Layer-1 (Cross-Projekt)
stackit/credentials_integration_test.go  //go:build integration — Layer-2 (Workload-Creds + S3)
```

**Ausführen:**

```bash
go test ./stackit/ -run TestLoadAccount -v                                  # offline
go test -tags integration ./stackit/ -run Integration -v -timeout 12m       # alle echten Tests
go test -tags integration ./stackit/ -run IntegrationWorkloadCredentials -v  # nur Layer 2
```

Wichtige technische Bestätigungen / Fallstricke (`objectstorage` v1.9.0, `minio-go` v7):
- Auth: `config.WithServiceAccountKeyPath(file)` — RSA-Key im JSON eingebettet, Key-Flow
  automatisch (kein separater Key-Pfad nötig).
- Alle Control-Plane-Calls region-skopiert: `(ctx, projectId, region, …)`, `region="eu01"`.
- Fehler-Typ: `*oapierror.GenericOpenAPIError` → `.GetStatusCode()` (via `errors.As`).
- Cross-Projekt-Zugriff scheitert **serverseitig mit 403** — SA-Token hat keine Rolle im
  Fremdprojekt. Isolation ist **nicht** vom Operator-Code abhängig.
- **Gotcha:** `CreateAccessKey` braucht einen (leeren) Payload (`NewCreateAccessKeyPayload()`),
  sonst *"createAccessKeyPayload is required"*.
- **Gotcha:** `DeleteAccessKey` braucht den `credentials-group`-Query-Param (Group-ID),
  sonst **500**. Group lässt sich erst löschen, wenn ihre Keys weg sind (sonst 422).
- AccessKey-Response: `accessKey` = S3-AccessKey-ID, `secretAccessKey` = Secret (nur 1× !),
  `keyId` = interne ID zum Löschen.
- S3-Endpoint eu01: `object.storage.eu01.onstackit.cloud`, **Path-Style**, **SigV4**.
  Endpoint-Host aus `Bucket.urlPathStyle` ableitbar (kein Hardcode nötig).
- Methoden im `objectstorage`-Top-Level-Paket sind **deprecated ab 2026-09-30** → später
  auf das versionierte Subpaket migrieren (kein Blocker für PoC).

## 10. Quellen

- [Object Storage Concepts](https://docs.stackit.cloud/products/storage/object-storage/basics/concepts/)
- [Bucket Policies](https://docs.stackit.cloud/products/storage/object-storage/how-tos/bucket-policies/)
- [Create/Delete Credentials](https://docs.stackit.cloud/products/storage/object-storage/how-tos/create-and-delete-object-storage-credentials/)
- [Supported S3 Operations](https://docs.stackit.cloud/products/storage/object-storage/reference/supported-operations-on-buckets-and-objects/)
- [Service Accounts](https://docs.stackit.cloud/platform/access-and-identity/service-accounts/) · [Auth Flows (Key-Flow)](https://docs.stackit.cloud/platform/access-and-identity/service-accounts/authentication-flows/)
- [Go SDK objectstorage](https://pkg.go.dev/github.com/stackitcloud/stackit-sdk-go/services/objectstorage) · [Terraform Provider](https://github.com/stackitcloud/terraform-provider-stackit)
- [NetApp StorageGRID — Bucket/Group Access Policies](https://docs.netapp.com/us-en/storagegrid/s3/bucket-and-group-access-policies.html) (Backend, Policy-Auswertung & Lockout)
</content>
</invoke>
