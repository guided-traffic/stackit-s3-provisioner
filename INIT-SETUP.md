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
| Secret-Namespace (ADR 2026-09-03) | **Workload-Secret liegt immer im Namespace des Bucket-CR.** `spec.secretRef.namespace` wurde entfernt — es war ungeprüft und damit ein Cross-Namespace-Secret-Write/Delete-Primitiv für jeden, der Bucket-CRs anlegen darf. Das Secret trägt jetzt immer eine Controller-OwnerRef. Bestands-CRs mit gesetztem Feld: der API-Server pruned das Feld, der Operator rotiert den Key in ein neues Secret im CR-Namespace, das alte Fremd-Secret verwaist mit totem Key (manuell löschen). |

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
      "NotAction": [
        "s3:GetObject","s3:PutObject","s3:PutOverwriteObject","s3:DeleteObject",
        "s3:ListBucket","s3:ListBucketVersions","s3:GetBucketLocation",
        "s3:ListBucketMultipartUploads","s3:ListMultipartUploadParts","s3:AbortMultipartUpload",
        "s3:GetObjectTagging","s3:PutObjectTagging","s3:DeleteObjectTagging",
        "s3:GetObjectVersion","s3:GetObjectVersionTagging",
        "s3:GetBucketVersioning","s3:GetBucketObjectLockConfiguration"
      ],
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
- **`NotAction` ist eine invertierte Whitelist**: jede nicht gelistete Action ist für den
  Workload denied. Aufnahmekriterium: die Action arbeitet auf Objekt-Daten, Objekt-Metadaten
  oder Listings. Denied bleibt alles, was *Zugriff* ändert (Bucket-Policy), Inhalte
  *weiterleitet* (Replication/Notification → Exfiltration), Objekte *festnagelt*
  (Retention/Legal-Hold/Compliance → Bucket wird unlöschbar, Teardown bricht), *Historie*
  zerstört (`DeleteObjectVersion`) oder den Bucket *umkonfiguriert*. Vollständige Begründung
  je Gruppe im Kommentar an `workloadAllowedActions` in [`stackit/s3.go`](stackit/s3.go).
- `s3:PutOverwriteObject` ist eine **StorageGRID-eigene** Action und greift bei jedem Write
  auf einen *bereits existierenden* Key (Daten, User-Metadaten, Tags). Fehlt sie, schlägt ein
  simples `PutObject` auf einen vorhandenen Key mit `AccessDenied` fehl — das bricht jeden
  Client, der Keys neu schreibt (barman/CNPG, restic, Terraform-State, Registry-Treiber).
  Sie ist **keine** Sicherheitsgrenze: `s3:DeleteObject` ist ohnehin erlaubt, ein Angreifer
  erreicht dasselbe per Delete+Put. WORM entsteht laut NetApp nur aus dem *Paar*
  `Deny PutOverwriteObject` **und** `Deny DeleteObject`.
- **Invariante:** Solange der Workload `s3:PutObjectTagging` hat, darf diese Policy **keine**
  tag-basierten Condition-Keys (`s3:ExistingObjectTag/*`, `s3:RequestObjectTag/*`) bekommen —
  sonst könnte der Workload über Tags seine eigenen Rechte umschreiben.
- Principal/`NotPrincipal` = Group-**URN** (`urn:sgws:identity::…:group/…`, aus
  `CreateCredentialsGroup`); Resource = `arn:aws:s3:::…`. Mischung ist korrekt (StorageGRID).

#### 4.1.1 Optionales drittes Statement: Read-Grants (`spec.grantReadAccess`)

Ein Bucket kann anderen `Bucket`-CRs **seines eigenen Namespace** lesenden Zugriff
gewähren (Producer-Seite: der Daten-Eigentümer deklariert, wer lesen darf). Ohne
Grant bleibt das Dokument **byte-identisch** zum Zwei-Statement-Template oben — ein
Operator-Upgrade schreibt bestehende Policies also nicht um.

Mit ≥1 Reader kommt ein drittes Statement dazu, und Stmt 1 nimmt die Reader in
`NotPrincipal` auf (sonst würde der Blanket-Deny sie trotz Stmt 3 aussperren):

```jsonc
{ // 3) Reader auf reine Lese-Ops begrenzen
  "Sid": "granted-readers-read-only",
  "Effect": "Deny",
  "Principal": { "AWS": ["<urn reader-group-a>", "<urn reader-group-b>"] },
  "NotAction": [
    "s3:GetObject","s3:GetObjectVersion",
    "s3:ListBucket","s3:ListBucketVersions","s3:GetBucketLocation",
    "s3:GetObjectTagging","s3:GetObjectVersionTagging",
    "s3:GetBucketVersioning","s3:GetBucketObjectLockConfiguration"
  ],
  "Resource": ["arn:aws:s3:::<bucket>", "arn:aws:s3:::<bucket>/*"]
}
```

- **Echte Teilmenge von Stmt 2.** Beide Listen sind über dieselben Konstanten
  gebaut (`stackit/s3.go`), was Tippfehler und abweichende Schreibweisen ausschließt.
  Die Teilmengen- und Read-only-Eigenschaft selbst ist **nicht** vom Compiler
  erzwungen (jede Konstante ließe sich in beide Listen schreiben) — sie hängt an
  `TestReaderAllowedActions_ReadOnly`. Kein `Put*`/`Delete*`/`Create*`.
- **Kein Multipart-Listing für Reader.** `ListBucketMultipartUploads` /
  `ListMultipartUploadParts` legen die *noch nicht committeten* Uploads des Eigentümers
  offen, `AbortMultipartUpload` zerstört sie. Für `ls`/`get` nicht nötig.
- **Herkunft der Reader-URN ist sicherheitskritisch.** Sie wird **nie** aus
  `status.credentialsGroupURN` des referenzierten CR gelesen (`buckets/status`-Schreibrecht
  ist schwächer als Secret-Leserecht — eine gefälschte URN käme sonst wörtlich in die
  Policy). Stattdessen: deterministischer Group-Name aus `namespace/name`
  (`workloadGroupName`) → Lookup gegen die Control Plane. `BuildIsolationPolicy` filtert
  zusätzlich Admin- und Workload-URN aus der Reader-Liste heraus.
- **Grenze dieser Auflösung (verifiziert 2026-08-24).** `workloadGroupName` ist
  `"s3op-<ns>-<name>"` auf 23 Zeichen gekürzt + 8-Hex-FNV-1a-32 über `"<ns>/<name>"`.
  Das kollidiert über Namespaces hinweg, empirisch reproduziert:
  `("gitlab","gitlab-artifacts")` und `("gitlab-gitlab","artifacts787ngo")` ergeben beide
  `s3op-gitlab-gitlab-arti-70dbcfc2`. Die Namespace-Bindung des Grants ist also eine
  Eigenschaft des **CR-Lookups**, keine kryptographische Zusicherung über das Principal.
  Das ist **nicht** grant-spezifisch: derselbe Name entscheidet über
  `EnsureCredentialsGroup` (Find-or-Create, **ohne** Ownership-Check) schon heute, welche
  Credentials-Group ein Bucket besitzt — siehe offene Punkte in §5.
- **Anzeigenamen sind nicht eindeutig.** Erscheint der abgeleitete Name mehrfach im
  Projekt, wird der Grant **verweigert** (Event `ReadGrantPending`) statt geraten;
  bei einem einzelnen Treffer gilt First-Match wie in `FindCredentialsGroupByName`.
- **Warum das Filtern nötig ist:** Admin-URN als Reader ⇒ der Admin-Key verliert
  `PutBucketPolicy` auf diesem Bucket ⇒ **nicht reparierbarer Lockout** (Deny schlägt
  alles). Workload-URN als Reader ⇒ zweites, engeres Deny auf den Eigentümer; Denies
  **schneiden sich**, der Eigentümer verlöre also still seinen Schreibzugriff.
- Reader-Liste wird dedupliziert und sortiert → Dokument deterministisch, `PoliciesEquivalent`
  meldet keinen Scheindrift.

## 5. Kritische Guardrails

1. **Rollen nur auf Projekt-Ebene zuweisen.** Eine Organisations-weite Rolle, die in
   beide Projekte kaskadiert, hebelt Layer 1 aus. → Bei Account-Einrichtung prüfen.
2. **`secret_access_key` nur einmal abrufbar** (bei Create). Operator muss ihn sofort in
   ein K8s-Secret schreiben; sonst Key löschen + neu erzeugen.
3. **Minimal-Rolle für den Provisioner-SA** (nicht Projekt-Owner). Exakter Rollenname
   noch zu klären → Q2.
4. **Default-Credentials-Group hat breiten Zugriff.** Für Layer 2 nie die Default-Group
   an Workloads geben — immer dedizierte Groups + restriktive Policy.
5. **Workload-Secret nur im Namespace des Bucket-CR** (§0, ADR 2026-09-03). Kein
   `namespace`-Feld an `secretRef` wieder einführen — es wäre ein Secret-Write/Delete-
   Primitiv über Namespace-Grenzen für jeden, der Bucket-CRs anlegen darf.

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

### 8.1 Bucket-Clone via rclone-Job (`spec.cloneFrom`) — implementiert (2026-07-17)

Einmaliger Copy eines existierenden S3-Buckets (beliebiger S3-kompatibler Endpoint)
in den frisch provisionierten Bucket. Kern-Entscheidungen:

- **Ausführung als Kubernetes Job** (Image `rclone/rclone`, Helm `clone.image`,
  Renovate-getrackt) im **Operator-Namespace** — überlebt Operator-Restarts,
  resource-limitiert (`clone.resources` → `CLONE_JOB_RESOURCES`), Credentials
  bleiben aus Workload-Namespaces raus.
- **Credentials-Handhabung:** Ziel-Seite authentifiziert mit dem **Admin-S3-Key**
  (direkt per `secretKeyRef` aufs Admin-Secret — kein zusätzliches Staging, kein
  Escalation-Gewinn, da Job im Operator-NS läuft). Quell-Credentials liest der
  Operator aus dem User-Secret (**nur CR-Namespace**, bewusst kein
  `namespace`-Feld — sonst könnte ein CR-Autor fremde Secrets exfiltrieren) und
  staged sie in ein Secret `s3op-clone-…-src` im Operator-NS.
- **Secret-Gating:** Default `holdSecretUntilCloned: true` — das Workload-Secret
  wird erst nach erfolgreichem Clone geschrieben (reine Flow-Reihenfolge:
  Bucket → Policy → Clone → Key+Secret). Bei `false` kommt das Secret sofort,
  `Ready` wartet trotzdem auf den Clone.
- **Fortschritt via rclone Remote-Control:** Job läuft mit
  `--rc --rc-addr=:5572`, Basic-Auth (User `operator`, 32-Zeichen-Zufallspasswort
  im Staging-Secret). Operator pollt alle 15s `POST /core/stats` auf der Pod-IP
  und schreibt `status.clone` (`bytesCopied`, `totalBytes`, `progress` à la
  „2.0 GiB / 18.0 GiB (11%)“, `rate`, `eta`; Printer-Column `Clone`).
  Gesamtgröße wird **einmal vor Job-Start** vom Operator per List gemessen
  (stabiler Prozent-Nenner). Helm-`NetworkPolicy` (`clone.networkPolicy.enabled`,
  Default an) beschränkt Ingress auf Port 5572 auf den Operator-Pod — die
  rc-API kann auch Kommandos ausführen, daher Passwort + Policy.
- **Addressing-Style:** Quelle default **path-style** (`FORCE_PATH_STYLE=true`,
  S3-kompatibler Standard); `cloneFrom.addressingStyle: virtual-hosted` schaltet
  Quelle auf `bucket.endpoint` (AWS-Stil, minio `BucketLookupDNS` fürs Messen,
  rclone `FORCE_PATH_STYLE=false`). Ziel (StackIT) bleibt immer path-style.
- **Semantik:** Clone-once — `status.clone.phase=Completed` ist terminal, spätere
  `cloneFrom`-Änderungen wirkungslos. Fehlgeschlagene Jobs werden gelöscht und mit
  Reconcile-Backoff neu erzeugt (rclone `copy` resumed, überspringt vorhandene
  Objekte; merged, löscht nie). Self-Clone (gleicher Endpoint + Bucket) wird als
  Config-Fehler abgewiesen. Completed-Status wird **vor** dem Aufräumen von
  Job/Staging-Secret persistiert (crash-safe, kein Doppel-Clone); Job-TTL 1h als
  Backstop, Teardown räumt laufende Clones mit ab.
- **Watch-Hygiene:** Bucket-Watch filtert auf Generation-/Annotation-Änderungen
  (`predicate.Or(GenerationChanged, AnnotationChanged)`) — sonst würde jedes
  Progress-Status-Update sofort re-reconcilen (Hot-Loop). Finalizer-Add requeued
  deshalb explizit. Job-Events mappen über Annotations (`bucket-namespace`/`-name`)
  zurück auf die CR (Cross-Namespace-OwnerRef nicht erlaubt).
- **RBAC neu:** `batch/jobs` CRUD + `pods` get/list/watch (Pod-IP für rc-Polling).

**Live verifiziert (2026-09-01, `make e2e-stackit`, `TestCloudClone`):** Kopie eines echten
Buckets mit dem echten rclone-Image (`rclone/rclone:1.75.0`) in Kind. Quelle war ein zweites
Bucket-CR, dessen Workload-Secret unveraendert als `cloneFrom.secretRef` taugt (die Default-Keys
passen) — der Workload-Key darf sein eigenes Bucket listen und lesen (`s3:ListBucket`,
`s3:GetObject` in `workloadAllowedActions`), was die Quell-Messung und den Copy traegt.
Bestaetigt: `totalBytes` = die geseedete Groesse, drei Objekte inkl. verschachtelter Praefixe
byte-identisch angekommen, `CloneCompleted=True`, und die **Hold-Invariante** — das
Workload-Secret des Ziels taucht nie auf, solange die Clone-Phase nicht `Completed` ist (der
Operator persistiert den Terminalzustand vor dem Secret-Write, der Test prueft das bei jedem
Poll). `completedAt` bleibt ueber nachfolgende Reconciles stabil, der Clone laeuft also
wirklich nur einmal. Das rclone-Image wird vom Make-Target aus dem gerenderten Chart gelesen
(`--clone-image=`) und in den Kind-Node vorgeladen — kein dritter Versions-Pin neben
`DefaultCloneImage` und `clone.image`, und kein Registry-Pull mitten im Test.

### 8.2 Transiente Provider-Fehler — implementiert (2026-08-25)

Anlass: Incident mgmt-p 2026-08-25. Zwei Ereignisse, dieselbe Verstaerkerkette
(Provider-Blip → alle Buckets non-Ready → alle Flux-Kustomizations non-Ready →
clusterweiter Alarmsturm):

1. **08:13–08:15 UTC** — StackIT-API antwortete `403` als **nginx-HTML-Seite**
   (SDK: `undefined response type, status code 403`). 342 Reconcile-Fehler in
   ~2 Minuten, alle 19 Buckets gleichzeitig non-Ready.
2. **10:37 UTC** — ein einzelnes `ensure bucket: unexpected EOF`.

**Verifizierte API-Semantik** (Live-Messung 2026-08-25, `GetServiceStatus`, eu01):

| Fall | Status | Body | `json.Valid(Body)` |
| ---- | ------ | ---- | ------------------ |
| Object Storage aktiviert | 200 | `{"project":"<uuid>","scope":"PUBLIC"}` | — |
| Object Storage **nicht** aktiviert | **404** | `{"detail":[{"key":"project.not_found","msg":"The project could not be found"}]}` | `true` |
| **Keine Berechtigung** | **403** | `{"timestamp":…,"path":…,"status":403,"error":"Forbidden","message":"Unauthorized"}` | `true` |

Die 403-Faelle wurden gegen nicht existierende Projekt-IDs gemessen, das 404
gegen ein reales Projekt ohne Object Storage; Gegenprobe mit zwei realen
Fremdprojekten ergab 200, also trennt die API sauber zwischen "nicht berechtigt"
(403) und "berechtigt, Service nicht aktiviert" (404).

**Wichtige Einschraenkung dieser Messung:** sie lief mit einem GUELTIGEN Key.
Das 403 bedeutet "dieser Key darf nicht auf dieses Projekt" — es ist NICHT das
Fehlerbild eines entzogenen Keys. Ein geloeschter oder rotierter SA-Key erreicht
die Object-Storage-API ueberhaupt nicht: der Key-Flow scheitert vorher am
Token-Endpoint. Ebenfalls live gemessen (2026-08-25,
`https://accounts.stackit.cloud/oauth/v2/token`):

| Fall | Status | Body |
| ---- | ------ | ---- |
| Key entzogen / nicht vorhanden | **400** | `{"error":"invalid_grant"}` (Content-Type `application/json`, RFC 6749 §5.2) |

Der SDK stempelt Status und Body des Token-Endpoints in denselben
`*oapierror.GenericOpenAPIError` wie API-Fehler (`core/clients/auth_flow.go`
`parseTokenResponse`), beide Fehlerbilder kommen also in einer Form an — deshalb
enthaelt die Definitiv-Menge unten **400**. Eine erste Fassung matchte nur
401/403 und haette nach einer Key-Sperrung die ganze Flotte fuer das volle
Grace-Fenster auf `Ready=True` gehalten.

**Entscheidender Diskriminator: `json.Valid(apiErr.Body)`, nicht der Statuscode.**
Ein Gateway/WAF vor der API erzeugt denselben `*oapierror.GenericOpenAPIError`
mit dessen Statuscode, aber einem HTML-Body — genau der 08:13-Fall. `oapierror.Model`
taugt NICHT als Diskriminator: der SDK dekodiert jeden Fehlerbody in
`objectstorage.ErrorMessage` (nur Feld `Detail`), sodass der andersgeformte
403-JSON-Body fehlerfrei in eine leere Struct dekodiert. Nur der Rohbody trennt.

**Drei Massnahmen (`stackit/errors.go`, `stackit/retry.go`, Reconciler):**

1. **`EnsureService` eskaliert einen fehlgeschlagenen Read nicht mehr zu einem
   Write.** Vorher wurde JEDER `GetServiceStatus`-Fehler als "nicht aktiviert"
   gelesen und `EnableService` versucht — pro Bucket, pro Reconcile. Jetzt nur
   noch bei strukturiertem 404 (`isServiceNotEnabled`); alles andere wird
   durchgereicht. Ausserdem wird ein verifiziertes "aktiviert" prozessweit
   gecacht (`Client.serviceReady`), da das Projekt unter einem laufenden Operator
   nicht deaktiviert werden kann, ohne dass alles andere ebenfalls bricht.
2. **Retry-`RoundTripper` unter der Authentifizierung.** `config.WithMaxRetries`
   ist seit core v0.26.0 ein No-Op, der SDK retryt also gar nicht.
   `config.WithHTTPClient` wird von `auth.SetupAuth` als *innerer* Transport des
   Key-Flows uebernommen (`core@v0.26.0/auth/auth.go:222`), der Retry deckt damit
   API-Requests **und** Token-Fetches ab, und jeder Versuch traegt den bereits
   gesetzten Authorization-Header. **Nur GET/HEAD** werden wiederholt: ein
   abgebrochener Write ist mehrdeutig, und ein wiederholtes `CreateAccessKey`
   wuerde einen zweiten Key erzeugen, dessen Secret nur einmal zurueckkommt und
   dann verloren waere. Die S3-Data-Plane braucht das nicht — minio-go retryt selbst.
3. **`Ready` ueberlebt einen nicht-definitiven Fehler** (`BucketReconciler.degrade`).
   `Ready` beschreibt den zuletzt **verifizierten** Zustand des Buckets, nicht das
   Ergebnis des letzten Verifikationsversuchs.

**Klassifikation nach Herkunft, nicht nach Fehlertext.** Die vorhandene Trennung
`fail` vs. `failNoRequeue` kodierte die Unterscheidung bereits — sie war fuer die
Requeue-Hammer-Vermeidung gebaut und deckt sich exakt:

- `failNoRequeue` (definitiv, `Ready` faellt immer): Spec-Guards, ungueltiger
  komponierter Bucketname, `validateCloneSource`, Ownership-Collision. Alles
  Aussagen ueber *diesen* Bucket, die der Operator lokal festgestellt hat.
- `fail` (nicht-definitiv, `Ready` wird gehalten): jeder Fehler beim Reden mit der
  StackIT-API, der S3-Data-Plane oder der Kubernetes-API.

Der Default fuer unbekannte Fehler faellt damit *per Konstruktion* auf "konnte
nicht verifizieren". Das ist Absicht: ein falsch gehaltenes `Ready` kostet ein
verzoegertes Signal, begrenzt durch das Grace-Fenster; ein falsch fallengelassenes
`Ready` markiert die ganze Flotte beim ersten Blip als krank.

**Ausnahmen vom Halten** (fallen sofort auf `Failed`):

- **Strukturierte Ablehnung durch den Provider** (`stackit.ProviderRefused`:
  `errors.As` auf `*oapierror.GenericOpenAPIError` **und** `json.Valid(Body)`
  **und** Status ∈ {400,401,403}). 401/403 = die Object-Storage-API lehnt einen
  authentifizierten Request ab; **400 = der Token-Endpoint lehnt den SA-Key ab**,
  das Fehlerbild eines entzogenen Keys. Muss sofort sichtbar bleiben — sonst
  sieht die Flotte nach einer Key-Sperrung 30 Minuten lang gruen aus, waehrend
  sie tot ist. Die `json.Valid`-Bedingung ist nicht kosmetisch: ohne sie waere
  der 08:13-Gateway-403 wieder "definitiv" und der Incident reproduziert.
- **Vom Operator zerstoertes Workload-Credential** (`errCredentialDestroyed`).
  `ensureAccessKeyAndSecret` loescht erst alle Group-Keys, dann legt es den neuen
  an (leak-frei). Scheitert der Create oder der Secret-Write danach, ist das
  publizierte Credential nachweislich tot — lokale Gewissheit, kein
  unverifizierbarer Provider-Zustand. Erreichbar ohne Spec-Aenderung: der
  Rotations-Trigger ist eine reine Annotation, `metadata.generation` bewegt sich
  nicht, der Generations-Guard greift also nicht.
- Bucket im Teardown (`DeletionTimestamp` gesetzt) — sonst waere ein durch den
  Data-Loss-Guard blockiertes Delete unsichtbar.
- `ObservedGeneration != Generation` — geaenderte Spec wurde nie verifiziert.
- Bucket war nie `Ready`.

**Begrenzung:** `--provider-degraded-grace` (Helm `providerDegradedGrace`,
Default `30m`, `0` = aus). Nach Ablauf faellt der Bucket wie vorher auf `Failed`.
Waehrend des Haltens: `status.degradedSince`, Condition `ProviderReachable=False`,
unveraenderte Warning-Events, `stackit_s3_provisioner_buckets_provider_degraded`
und `stackit_s3_provisioner_bucket_degraded_since_timestamp_seconds`.

> Nachtrag 2026-09-02: der urspruengliche Satz an dieser Stelle — "der Reconcile
> gibt den Fehler weiter zurueck, `StackitS3ReconcileErrors` feuert sofort wie
> bisher" — war als bewusste Nicht-Aenderung gedacht und hat sich als der
> eigentliche Alarm-Treiber herausgestellt. Siehe §8.4.

**Bewusst NICHT gemacht:** eine Taxonomie transienter Fehlermuster (Stringmatch
auf `unexpected EOF`, `undefined response type`, 5xx-Listen). Diese Liste ist
provider-kontrolliert und offen — sie kann nie fertig sein, und jeder nicht
gelistete Fehler faellt auf den falschen Default. Die hier gepflegte Liste
definitiver Faelle ist geschlossen und gehoert uns.

### 8.4 Provider-Circuit-Breaker — implementiert (2026-09-02)

**Befund (read-only in Prometheus + Operator-Logs auf mgmt-p, 2026-09-02).**
§8.2 hat `Ready` stabilisiert, aber nicht die Alarme. Beide Alarme
(`StackitS3ReconcileErrors`, `StackitS3BucketProviderDegraded`) feuerten weiter
bei Provider-Ausfaellen, die sich von allein aufloesen.

Fehler-Zeitleiste des letzten Vorfalls (Operator-Log, UTC; 22 Bucket-CRs):

```
14:23  11x 503   (nginx-HTML-Seite, StackIT-Edge)
14:29  17x 503
14:34  19x 503 + 14x 429  <- erste IP-Rate-Limits
14:40  19x 503 + 21x 429
14:41  20x 503 + 11x 429
14:42  20x 503 + 51x 429
14:43  vorbei, self-healed
```

429-Body: `{"status":429,"error":"Too Many Requests","message":"rate limit on IP
level exceeded; please try again later"}`.

**Kausalitaet ist in der Reihenfolge sichtbar:** 503 kommt zuerst (14:23), 429
erst elf Minuten spaeter (14:34). Der Provider-Edge faellt aus, der Operator
haemmert anschliessend sein eigenes IP-Rate-Limit voll und verlaengert damit den
Ausfall, auf den er reagiert. `sum(increase(controller_runtime_reconcile_errors_total{controller="bucket"}[15m]))`
erreichte **220**; Alarmschwelle war `> 3` mit `for: 0`. Ein Ausfall, 242
Reconcile-Fehler, zwei Pages, nichts zu tun.

**Zwei Verstaerker, beide im eigenen Code:**

1. **Kein Workqueue-RateLimiter am `bucket`-Controller.** `Complete(r)` ohne
   `WithOptions` ⇒ controller-runtime-Default: Exponential-Backoff ab **5 ms**,
   fleetweit 10 qps / Burst 100. Gedacht fuer Controller, deren Reconcile ein
   paar Calls gegen den lokalen API-Server macht.
2. **`retryTransport` retryte 429** (fix 200ms/600ms, ohne `Retry-After`), und
   das pro GET dreifach. Ein Rate-Limit erneut anzufragen ist die eine Antwort,
   die ihn garantiert vertieft.

Dazu ein Grundrauschen von ~1 Fehler/15m: `read: connection reset by peer` auf
gepoolten Keep-Alive-Verbindungen (`http.DefaultTransport`, `IdleConnTimeout`
90s). Trippt die Schwelle nie allein, macht sie aber billig zu ueberschreiten.

**Loesung — vier Schichten:**

| # | Aenderung | Ort |
| - | --------- | --- |
| 1 | Workqueue-RateLimiter: exponentiell ab 1s bis 15min, fleetweit 1 qps / Burst 5 | `bucketRateLimiter`, `SetupWithManager` |
| 2 | 429 nicht mehr retryen; `IdleConnTimeout` 30s (Client schliesst vor dem Edge) | `stackit/retry.go` |
| 3 | Fleetweiter Circuit-Breaker | `internal/controller/breaker.go` |
| 4 | Alarme auf Handlungsbedarf statt auf Burst | `prometheusrule.yaml` |

**Der Diskriminator des Breakers ist bewusst keine Fehler-Klassifikation**,
sondern das **Ausbleiben eines erfolgreichen Reconciles**: ein einzelner kaputter
Bucket unter gesunden hat seine Fehler mit den Erfolgen der uebrigen Flotte
verschraenkt, was den Lauf zuruecksetzt; ein fleetweiter Ausfall erzeugt eine
ununterbrochene Fehlerkette. Das haelt den Breaker aus dem Geschaeft des
Fehler-Parsens heraus, das der Operator in §8.2 schon abgelehnt hat.

Verhalten bei offenem Circuit:

- **kein** Provider-Call, auch nicht im Teardown (Finalizer bleibt, Delete setzt
  beim naechsten Probe fort);
- Reconcile liefert `RequeueAfter` und **keinen** Fehler — der Fehler wird
  weiterhin geloggt, erzeugt Warning-Events und steht in `status.message`;
- Probe-Kadenz 60s, verdoppelnd bis `--provider-circuit-max-cooldown` (5m).
  60s als Basis, weil der offene Zustand *beobachtbar* sein muss: der Chart
  scrapet alle 30s, und der `StackitS3ReconcileErrors`-Alarm unterdrueckt sich
  fuer Fenster mit offenem Circuit;
- `status.degradedSince` wird **einmal** geschrieben, nicht pro Probe;
- die Grace laeuft weiter — ein zu lange gehaltener Bucket faellt weiterhin auf
  `Failed`. Der Breaker verzoegert Reconciles, er verbreitert nicht das Fenster,
  in dem ein Bucket einen unverifizierten Zustand behaupten darf.

`--provider-circuit-threshold=0` (Helm `providerCircuit.threshold`) schaltet ihn
ab — Values-only-Rollback ohne neues Image.

**Alarme:**

- `StackitS3ReconcileErrors`: `> 6` Fehler/15m, `for: 15m`, **`unless on()`
  Fenster mit offenem Circuit**. Ein StackIT-Ausfall erreicht diesen Alarm damit
  gar nicht mehr; uebrig bleiben Fehler, die der Breaker nie als Provider-Ausfall
  erkannt hat (Kubernetes-API, einzelne Buckets bei gesunder Flotte). Die
  `unless`-Form ist fail-open: fehlt die Circuit-Metrik (aelterer Operator),
  entfernt sie nichts und der Alarm verhaelt sich wie vorher.
- `StackitS3BucketProviderDegraded`: `max(time() - ..._bucket_degraded_since_timestamp_seconds) > 1200`
  statt `max(..._buckets_provider_degraded) > 0` mit `for: 10m`. Alarmiert also
  auf das **Alter** des Haltens, nicht auf dessen Existenz. `holdForSeconds` muss
  unter `providerDegradedGrace` bleiben: nach Ablauf der Grace faellt der Bucket
  auf `Failed`, die Serie verschwindet (der Collector emittiert sie nur bei
  `phase == Ready`) und `StackitS3BucketFailed` uebernimmt.

**Neue Metriken:** `stackit_s3_provisioner_provider_circuit_open` (immer
vorhanden, damit kein Alarm gegen eine fehlende Serie rennt) und
`..._provider_circuit_opened_timestamp_seconds` (nur bei offenem Circuit; bewegt
sich beim fehlgeschlagenen Probe **nicht**, misst also den Ausfall statt des
letzten Probe-Intervalls).

**Bewusst NICHT gemacht:** ein eigener Alarm auf den offenen Circuit. Er wuerde
zeitgleich mit `StackitS3BucketProviderDegraded` feuern und dieselbe Sache zweimal
melden. Der Circuit ist Observability (Dashboard, Alarm-Unterdrueckung), das
Halten ist das, worauf man reagiert.

### 8.3 Bucket-Groesse + Kostenschaetzung (`spec.usage`) — implementiert (2026-09-01)

Anforderung: aktuelle Bucket-Groesse an der CR sichtbar (Lens), konfigurierbares
Intervall, clusterweiter Default via Helm (default aus), pro CR ueberschreib- und
abschaltbar, Ermittlung darf den Operator nicht blockieren; zusaetzlich eine
Kostenschaetzung pro Monat aus einem konfigurierbaren GB/h-Preis.

**Verifizierte Preis-/Abrechnungslage** (recherchiert 2026-09-01):

| Frage | Befund | Quelle |
| ----- | ------ | ------ |
| Abrechnungsmetrik | "je angefangener Stunde je angefangenem Gigabyte" — sonst nichts | Leistungsschein STACKIT Object Storage v1.2, gueltig ab 2026-03-26, Abschnitt "Servicepläne - Metriken" |
| Preis Premium-EU01 | **0.00003697772 EUR** per GB/hour (Monatsspalte 0.03 EUR) | STACKIT price list v1.0.43 (08/04/2026), Zeile "Object Storage Premium-EU01" |
| Preis Premium-EU02 | 0.00003883000 EUR per GB/hour | dieselbe Preisliste |
| Preis Archiving-EU01 | 0.00003697772 EUR per GB/hour (identisch zum Standard-Tier) | dieselbe Preisliste |
| Request-/Operations-Kosten | **keine** — die Preisliste enthaelt null Treffer fuer request/operation/traffic/egress ueber alle 31 Seiten | dieselbe Preisliste |
| Monats-Projektion | 720 Stunden ("a hypothetical subscription period of 720 hours (30-day month)") | Fussnote jeder Preislisten-Seite |

**Konsequenz fuer das Feature:** Die Groessenermittlung kostet **kein Geld**, aber
**Zeit**. Es gibt keinen Usage-Endpoint in der Control Plane (geprueft gegen
`objectstorage` SDK v1.9.1: nur Bucket/CredentialsGroup/AccessKey/Service/
Retention/ComplianceLock), die Groesse ist also nur ueber ein vollstaendiges
Listing zu bekommen — ~1 Request je 1000 Keys. Die Guards sind entsprechend auf
Zeit ausgelegt, nicht auf Geld: `minInterval` (Default 60m) und `maxObjects`
(Default 2 Mio, danach `truncated` = untere Schranke).

**Design:**

- **Eigener Controller** (`BucketUsageReconciler`, eigene Workqueue,
  `MaxConcurrentReconciles` = `bucketUsage.concurrency`). Ein Listing ueber einen
  grossen Bucket dauert so lange wie der Bucket gross ist; im Provisioning-Pass
  wuerde das eine unbegrenzte, rein informative Operation vor Credential- und
  Policy-Management haengen.
- **Status-Trennung:** der Mess-Controller schreibt **nur** `status.usage`, und
  zwar per Merge-Patch — er kann die Felder des Provisioning-Reconcilers nicht
  ueberschreiben. Die Gegenrichtung deckt Optimistic Concurrency ab: der
  Provisioning-Pfad schreibt Status per `Update` mit ResourceVersion, eine
  dazwischen gelandete Messung laesst dieses Update also konfliktieren und neu
  laufen statt still verloren zu gehen.
- **Zeitplan aus dem Status, nicht aus einem Prozess-Timer:** faellig ist eine
  Messung wenn `status.usage.lastMeasurementTime + interval` erreicht ist, der
  naechste Lauf kommt per `RequeueAfter`. Ueberlebt Operator-Neustarts; der
  Bucket-Watch ist generation/annotation-gefiltert, sonst wuerde der eigene
  Status-Write die naechste Messung sofort ausloesen (Hot-Loop).
- **Admin-Key liest, legt nichts an:** der Mess-Pfad liest das Admin-Secret und
  bootstrappt es nie — eine Messung darf keine Cloud-Credentials als Seiteneffekt
  erzeugen. Fehlt es, wird kurz requeued.
- **Messfehler beruehren `Ready` nicht** und geben keinen Reconcile-Error zurueck
  (`StackitS3ReconcileErrors` filtert ohnehin auf `controller="bucket"`). Sie
  landen in `status.usage.message`, einem Warning-Event und
  `stackit_s3_provisioner_usage_measurement_failures_total`. Der vorherige Wert
  bleibt stehen — eine veraltete Groesse ist nuetzlicher als keine.
- **Versionen optional** (`includeVersions`): das Versions-Listing liefert
  aktuelle Versionen, alte Versionen und Delete-Marker in einem Pass; ohne diese
  Option unterschaetzt die Messung einen versionierten Bucket gegenueber der
  Rechnung. Unvollstaendige Multipart-Uploads werden **nicht** gezaehlt (bewusst
  offen gelassen — sie belegen Speicher, brauchen aber ein drittes Listing).
- **Kostenformel:** `ceil(billableBytes / 1e9)` angefangene GB × Preis × 720 h,
  auf ganze Cent gerundet. Die Cent-Zahl ist der kanonische Wert
  (`estimatedMonthlyCostCents`), der Anzeigestring ist ihre Darstellung.

**Live verifiziert (2026-09-01, `make e2e-stackit`, Projekt `ebc9d379…`, eu01):**

| Befund | Ergebnis |
| ------ | -------- |
| Messung mit dem Admin-Key gegen StorageGRID | funktioniert; leerer Bucket in 64 ms gemessen |
| 5 MiB / 1 Objekt geschrieben, danach neu gemessen | `bytes=5242880`, `objects=1`, `0.03 EUR` (1 angefangenes GB) — Formel und Preis stimmen gegen die echte Listung |
| `includeVersions: true` auf einem **nicht-versionierten** Bucket | StorageGRID beantwortet das Versions-Listing; das lebende Objekt kommt mit `IsLatest=true` und landet korrekt in den Current-Zaehlern, `versionBytes`/`versionObjects` bleiben 0. Der Pfad laeuft also nicht ins Leere und zaehlt nichts doppelt. |
| `spec.usage.enabled: false` nachtraeglich | `status.usage` verschwindet vollstaendig |

## 9. Machbarkeits-Smoke-Test — VERIFIZIERT (2026-06-30)

Minimaler Go-Code gegen die **echte** StackIT-API, mit beiden Service-Account-Keys
(`account-1.json` = Projekt `ebc9d379…`, `account-2.json` = Projekt `5ad5e488…`;
SA-Keys am 2026-08-24 gegen dedizierte e2e-Accounts getauscht — die alten Projekte antworten
auf `EnsureService` mit 403).

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
- [STACKIT Preisliste (PDF)](https://stackit.com/en/asset/download/37788/file/STACKIT_price_list.pdf) · [Leistungsschein Object Storage (PDF)](https://stackit.com/de/asset/download/34186/file/Leistungsschein_STACKIT_Object_Storage.pdf) · [Object-Storage-Produktseite mit Preisen](https://stackit.com/en/products/storage/stackit-object-storage) (Preis- und Abrechnungsbasis fuer §8.3)
</content>
</invoke>
