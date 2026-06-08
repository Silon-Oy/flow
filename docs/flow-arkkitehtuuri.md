# Flow — run-issues irrotettuna itsenäiseksi tiimituotteeksi

> Arkkitehtuurisuunnitelma. Laadittu GoodReason-syklillä (Strategist → Architect → review-gate → R4-koe) 2026-06-01.
> Lähde: dotfiles-repon `claude/scripts/run-issues/` -orkestraattori.
> **Status:** suunnitelma + estävä R4-portti todennettu vihreäksi. Ei vielä toteutuskoodia.

Tämä dokumentti kuvaa, miten nykyinen yhden käyttäjän `run-issues`-bash-orkestraattori irrotetaan itsenäiseksi **Go-pohjaiseksi monikäyttäjä-tuotteeksi** (työnimi **Flow**), jonka devaustiimi voi ottaa käyttöön nopeasti ja jonka toimintaan admineilla on näkyvyys. "Irrottaminen" = arkkitehtuuri kopioidaan pohjaksi uudelle repolle; nykyistä dotfiles-toteutusta ei poisteta.

---

## 1. Tiivistelmä

Nykyinen run-issues toimii vain yhdelle käyttäjälle (Olli) hänen kahdella koneellaan, hänen `gh`-tunnuksellaan. Koordinaatio nojaa paikalliseen tiedostolukkoon + GitHub-`@me`-assignaatioon, mikä **hajoaa heti kun devaajia on monta eri tunnuksilla**. Tila on hajallaan koneiden levyillä eikä dashboard ole mahdollinen ilman SSH:ta. Admin/dev-rooleja ei ole.

Flow ratkaisee tämän **hybridimallilla**: GitHub pysyy issue/PR-totuuden lähteenä, mutta kevyt keskuspalvelu (`flowd`) hoitaa työn omistajuuden (lease), runner-rekisterin, telemetrian ja per-tenant GitHub App -tokenit. Devaajat ja jaetut auto-run-koneet ajavat orkestrointia eristetyissä Docker-konteissa, joiden ulospäin lähtevä liikenne on rajattu ja joiden sisään ei koskaan päädy raakaa GitHub-credentiaalia.

**Kriittisin tekninen epävarmuus (R4) on todennettu vihreäksi:** claude-code plan-billing -autentikointi toimii kovennetussa Docker-kontissa (ks. §12).

---

## 2. Tavoite ja lukitut päätökset

### 2.1 Vaatimukset (annettu)
- Komentorivisovellus (CLI).
- Tuki useammalle devaajalle JA devaajan auto-run-koneelle (vastaava kuin nykyinen Studio).
- Käyttöoikeustasot: admin ja normaali devaaja.
- Wizard-tyyppinen per-projekti-käyttöönotto (haara johon PR kohdistuu, remotet, labelit, jne.).
- Dashboard: kehityksessä olevat projektit, issuet, PR:t, artefaktien siivous, lokit.
- **Päätavoite:** nopea ja sujuva käyttöönotto tiimille + admin-näkyvyys.

### 2.2 Päätösloki (Ollin lukitsemat reunaehdot)

| # | Päätös | Valittu | Perustelu |
|---|---|---|---|
| 1 | Koordinaatiomalli | **Hybridi** | GitHub = issue/PR-totuus (toimii jo); kevyt keskus aggregoi ajotilan/lokit/telemetrian. Koneet pysyvät autonomisina. |
| 2 | Teknologia | **Go** | Single-binary CLI helppoon jakeluun; vahva concurrency keskukselle/runnerille. |
| 3 | Hosting | **Self-host Docker** | Studio/VPS, tiimillä jo Tailscale-verkko. Ei pilvilukkoa. |
| 4 | Auto-run-koneet | **Molemmat** | Jaetut keskitetyt runnerit JA devaajan oma kone; runnerit rekisteröityvät + raportoivat kapasiteetin. |
| 5 | Multi-tenancy | **Aito multi-tenant** | Asiakasorgit eristettyinä (tenant-ID läpi tietomallin, RBAC + secrets per tenant). |
| 6 | Claude-auth | **Per-kone tilaus** | Runner ajaa koneen omalla plan-tilauksella; keskus kirjaa vain telemetrian, ei laskuta. |
| 7 | Admin-näkyvyys | **Metadata + lokit** | Status/kesto/vaihe/retry/linkit + ajolokit. EI agentin täysiä promptteja/diffejä dashboardissa. |
| 8 | Roolit | **admin + developer** | Developer: rekisteröi projekteja, näkee omat ajot. Admin: koko tenant + runner/secret/merge-policy-hallinta. |
| 9 | Runner-eristys | **Taso 1 MVP:hen, Taso 2 roadmapille** | Taso 1 = kovennettu Docker + egress-allow-list + credentials-proxy + scoped git-write. Taso 2 = gVisor (opt-in). |
| 10 | Kaupallisuus | **Sisäinen, commercial-ready** | Silonin sisäinen tiimityökalu nyt. Multi-tenant-skeema (päätös 5) säilyy, mutta ei billingiä/SLA:ta/Malli A:ta — kaupallistaminen mahdollista myöhemmin ilman tietomallin uudelleenkirjoitusta. Luottamusmalli: runnerit ajavat *Silonin omaa* koodia → ei untrusted-tenantteja jaetulla runnerilla MVP:ssä. (Avoin kysymys §17 / #15 ratkaistu.) |
| 11 | R4(2) Claude-auth kontissa | **Malli B; Malli A lykätty roadmap-optioksi** | Credentiaali tiedostona kontissa (§11.5, todennettu §12). Per-kone plan-billing rajaa vahingon koneen kiintiöön, ei tenant-dataan. Malli A (TLS-MITM, credentiaali pois kontista) revisioidaan **vain jos** jaettu untrusted-tenant-runner asiakkaille toteutuu — silloin se kytkeytyy päätöksen 10 uudelleenarviointiin. (§17 / #15 ratkaistu.) |
| 12 | R3c scoped git-write | **GitHub App -scope + branch protection ensisijaisesti** | Branch protection (vaadi PR mainiin, estä force-push + haaran poisto suojatuilla haaroilla) + App-tokenin permission-scope = robusti GitHub-natiivi vahvistus. Proxy tekee vain karkean write/read-tarkistuksen (§11.3 MITM:ää github.comin jo credentials-injektiota varten). Hauras pkt-line-ref-parsinta (§11.4) **lykätty roadmapille** (Taso 2 / defense-in-depth) — toteutetaan vain jos jaettu untrusted-runner sitä vaatii. (§17 / #15 ratkaistu.) |
| 13 | Observability & backup | **Kevyt Prometheus + audit Vaihe 2; pg_dump-backup #13** | (a) Prometheus-`/metrics`-endpoint `flowd`:lle (counterit: ajot statuksittain, lease-myönnöt, skanneri-pollit, egress-denied). OTel-distributed-tracing lykätty roadmapille. Eksplisiittinen append-only audit-loki (kuka muutti merge-policyä/rekisteröi projektin/myönsi runner-tokenin) **Vaihe 2:een** (vaatii roolit, #21). Metrics-endpoint = #22. (b) Postgres-backup = ajastettu `pg_dump` + retention + offsite-kopio, dokumentoitu Vaihe 4 deploy-paketointiin (#13). Egress- (§11.6) + RUN_EVENT-lokit kattavat loput. (§17 / #15 ratkaistu.) |
| 14 | base_branch-skooppi | **Projektitaso default + per-remote override** | `PROJECT.base_branch` = oletus; `remotes`-jsonb saa valinnaisen per-remote `base_branch`-overriden (eri orgit voivat käyttää eri integraatiohaaraa). Ei migraatiota (remotes on jo jsonb), override valinnainen → yhden-haaran tapaus pysyy yksinkertaisena. Resolvointi: remote-override ?? project.base_branch. (§17 / #15 ratkaistu.) |
| 15 | Dashboard auth + roolihaaroitus | **`GET /v1/me` + selain-device-flow + capability-driven UI** | Dashboard renderöi GitHub-device-flow:n suoraan selaimessa (POST `/v1/auth/device/start` → näytä `user_code` + `verification_uri` → polll `/v1/auth/device/poll`); session-token sessionStorageen. UI-näkyvyys haaroittuu **capability-listan** (`/v1/me`:n `capabilities[]`) mukaan, ei rooli-stringin — `§7`-taulu on ainoa totuuden lähde. SSE-logitailille `fetch` + `ReadableStream` (EventSource ei tue Authorization-headeria). (#11) |
| 16 | `merge_policy`-skeema | **`{label, conflict_resolution}` — kaksi tunnettua kenttää** | §8:n "PR_WATCH_MERGE_LABEL + conflict-flag" kiteytyy kahteen jsonb-kenttään: `label` (string, oletus prwatch-tasolla `"auto-merge"`) ja `conflict_resolution` (bool, ohjaa `prwatch.Decide`:n `enableConflictResolution`-argumenttia). `PUT /v1/projects/{id}/merge-policy` (admin-only, `CapMergePolicyManage`) hylkää tuntemattomat kentät (`DisallowUnknownFields`) jotta kirjoitusvirheet eivät katoa jsonb-arvoihin. (#11) |

---

## 3. Nykytodellisuus ja esteet (Strategist-analyysi)

Nämä mekanismit ovat single-user/single-machine-sidonnaisia ja siksi esteitä monikäyttäjä-tavoitteelle.

| Nykyrakenne (tiedosto) | Miksi este | Ratkaisu |
|---|---|---|
| **mkdir-FS-lukko** (`lib/locking.sh`) | Toimii vain yhden koneen sisällä. | Keskitetty lease (§5). |
| **`@me`-assignaatio arbiterina** (`lib/issue.sh:verify_claim`) | "Ainoa assignee" hajoaa monella eri gh-loginilla → tuplatyö. **Ydineste.** | Lease keskuksesta; GitHub-assignaatio vain näkyvyyssignaaliksi. |
| **Per-org single GitHub App** (`lib/github-app-auth.sh`) | Multi-org = monta installaatiota; ei-origin-remotet bypassaavat henkilökohtaiseen tunnukseen. | Per-tenant App-installaatiorekisteri + token broker (§7.3). |
| **hostname-hardkoodattu Studio-portti** (`poller.sh`, `pr-watch-poller.sh`) | Runner-rooli koodissa, ei konfiguraatiossa. | Runner-rekisteröinti + kapasiteettiraportointi (§4). |
| **LaunchAgent + macOS-polut** (`~/Library/...`, `stat -f`) | Self-host = Linux. | Go-daemon kontissa; XDG/konfiguroitavat polut. |
| **Käsin täytetty per-kone `watchlist.json`** | Ei skaalaudu tiimille, ei tue wizardia. | Projektirekisteri keskuksessa + wizard-CLI (§8). |
| **Levylle hajautettu `run.json`/`state.jsonl`** (`lib/state.sh`) | Dashboard ei näe mitään ilman SSH:ta. | Telemetrian push keskukseen (§6). |
| **plan-billing yhdellä tilillä** (`lib/claude-call.sh`) | Monta runneria = monta Claude-tiliä. | Per-kone-auth (päätös 6); todennettu R4-kokeella (§12). |
| **Henkilökohtainen `op`/env-tiedosto** (`source_machine_env`) | Ollin 1Password-tili. | Tenant-/runner-tason secrets-broker (§9). |
| **admin/dev-roolit** | Eivät ole olemassa lainkaan. | Rakennetaan tyhjästä (§7.4). |

**Keskeinen havainto:** bash-koodi EI ole kopioitava arkkitehtuuripohja — se on erinomainen *spesifikaatio*. Kopioitavaa on **logiikka + puhtaat funktiot testeineen** ja **prompt-tiedostot**. Koordinaatio, identiteetti, tila, ajastus ja secrets rakennetaan uusiksi keskuspalvelu-keskeisiksi (ks. portattavuus-kartta §13).

---

## 4. Komponenttiarkkitehtuuri

Viisi komponenttia + Postgres. Työnimet: **`flowctl`** (CLI), **`flowd`** (keskuspalvelu), **`flow-runner`** (runner-daemon), egress-proxy (sidecar), dashboard (web).

```
                         ┌─────────────────────────────────────────────┐
                         │              GitHub (per tenant/org)          │
                         │   issues · PRs · labels · App installations   │
                         └───────▲───────────────────────────▲──────────┘
                                 │ issue/PR truth              │ App token
        ┌────────────────────────┼─────────────────────────────┼───────────┐
        │                  KESKUSPALVELU (flowd)                 │           │
        │   RBAC/authz · Lease manager · GitHub App token broker             │
        │   Project registry · Runner registry · Telemetry sink             │
        │                  HTTP/JSON API (REST + SSE)                        │
        └──────────┬───────────────────┬───────────────────────┬───────────┘
                   │                    │                       │
       ┌───────────┴──────┐  ┌──────────┴─────────┐   ┌─────────┴──────────┐
       │   CLI (flowctl)  │  │  RUNNER HOST        │   │  Dashboard (web)   │
       │  init/status/    │  │  flow-runner +      │   │  read-mostly +     │
       │  login/runner    │  │  egress-proxy +     │   │  SSE live tail +   │
       │  register        │  │  per-ajo kontit     │   │  admin controls    │
       └──────────────────┘  └──────────┬─────────┘   └────────────────────┘
                                        │
                              ┌─────────┴─────────┐
                              │   PostgreSQL      │  ← keskuksen tila
                              └───────────────────┘
```

**Vastuut:**
- **`flowctl`** — devaajan/adminin käsityökalu, puhuu vain keskukseen. Komennot: `login` (GitHub OAuth device flow), `init` (projektin wizard), `status`, `runner register`, `runs <id> logs`, `project list/show`, `secret set` (admin).
- **`flow-runner`** — korvaa `poller.sh` + `pr-watch-poller.sh` + `orchestrate.sh` + `pr-watch.sh`. Pitkäikäinen prosessi. Saa työtä **pull-mallilla** (`POST /v1/leases/acquire`) — ei pollaa GitHubia itse. Ajaa S1–S12 per-ajo kontissa, lähettää heartbeatin + telemetrian. Sisältää egress-proxy-sidecarin (§11).
- **`flowd`** — lease-manager (§5), projektirekisteri, runner-rekisteri, telemetria-sink, RBAC, GitHub App token broker (§7.3) + taustaskanneri joka pollaa GitHubin `auto-run`-issuet (ainoa GitHubia pollaava komponentti → keskittää rate-limitin).
- **egress-proxy** — sidecar per runner-host. Allow-list + credentials-injektio + scoped git-write + egress-loki (§11).
- **dashboard** — read-mostly. REST + SSE. Metadata + lokit (päätös 7). Admin-näkymässä runner/secret/merge-policy-hallinta.
- **PostgreSQL** — atominen lease (`SELECT … FOR UPDATE SKIP LOCKED`), multi-tenant-relaatiot, `JSONB` event-datalle. `postgres:16` -kontti + nimetty volume.

---

## 5. Lease/koordinaatio-protokolla

Korvaa `lib/locking.sh` (mkdir) + `claim_issue`/`verify_claim` (`@me`). **Yksi atominen operaatio keskuksessa.**

Lease-avain (uniikki): `(tenant_id, project_id, remote, issue_number, kind)`. `kind ∈ {develop, pr_watch, clean}`.

**Claim** (`POST /v1/leases/acquire`):
```sql
BEGIN;
  SELECT w.* FROM claimable_work w
   WHERE w.tenant_id = $tenant AND w.kind = ANY($kinds)
     AND NOT EXISTS (SELECT 1 FROM lease l
        WHERE l.work_key = w.work_key AND l.expires_at > now() AND l.status='active')
   ORDER BY w.created_at ASC
   FOR UPDATE OF w SKIP LOCKED
   LIMIT 1;
  INSERT INTO lease (work_key, runner_id, tenant_id, status, acquired_at, expires_at)
  VALUES (..., 'active', now(), now() + interval '<TTL>') RETURNING lease_id;
COMMIT;
```
`SKIP LOCKED` = tietokanta on arbiter. Kaksi runneria ei voi koskaan saada samaa työtä; ei mkdir-kilpajuoksua, ei `@me`-verifiointia, ei `sleep`-paikkausta.

| Parametri | Arvo | Vastine ennen |
|---|---|---|
| Lease-TTL | 15 min | mkdir-lukon 24h stale |
| Heartbeat-väli | 60 s | — |
| Heartbeat-uusinta | `POST /v1/leases/{id}/heartbeat` → expires_at += TTL | — |
| Reaping | tausta: `UPDATE lease SET status='reaped' WHERE expires_at<now()` | `scan_stalled` |

Heartbeat korvaa erillisen liveness-skannerin: jos runner-prosessi kaatuu, heartbeat lakkaa → lease vanhenee → työ palaa jonoon. Pitkä claude-kutsu lähettää heartbeatin taustasäikeestä.

### Degradaatio — keskus alhaalla vs. "koneet autonomisia"
| Tilanne | Käytös | Perustelu |
|---|---|---|
| Uuden työn claim, keskus alhaalla | **ESTYY (fail-closed)** | GitHub-`@me`-fallback toisi takaisin kaksi-arbiteria-ongelman. |
| Käynnissä oleva ajo, keskus putoaa kesken | **JATKUU loppuun**, telemetria puskuroituu | "Autonomia" = aloitettu työ ei katkea keskuksen häiriöön. |
| Keskus palaa | Runner re-syncaa puskuroidut eventit, uusii leasen | |

---

## 6. Tietomalli & API

### Entiteetit (yleistetty `run.json`/`state.jsonl`:stä)

```mermaid
erDiagram
    TENANT ||--o{ USER : has
    TENANT ||--o{ PROJECT : owns
    TENANT ||--o{ RUNNER : registers
    TENANT ||--o{ GITHUB_APP_INSTALL : has
    TENANT ||--o{ SECRET_REF : scopes
    PROJECT ||--o{ RUN : produces
    PROJECT ||--o{ CLAIMABLE_WORK : queues
    RUNNER ||--o{ LEASE : holds
    CLAIMABLE_WORK ||--o| LEASE : claimed_by
    LEASE ||--o| RUN : authorizes
    RUN ||--o{ RUN_EVENT : logs
    USER }o--|| ROLE : assigned

    PROJECT { uuid id PK; uuid tenant_id FK; text name; text owner_repo; jsonb remotes; jsonb labels; text base_branch; uuid runner_pool FK; jsonb secret_refs; jsonb merge_policy; jsonb claude_config }
    %% remotes jsonb: [{remote, base_branch?}] — per-remote base_branch override, fallback PROJECT.base_branch (päätös 14)
    RUNNER { uuid id PK; uuid tenant_id FK; text hostname; int capacity; int active_leases; timestamptz last_heartbeat; text status; jsonb capabilities }
    CLAIMABLE_WORK { uuid id PK; uuid project_id FK; text work_key UK; text remote; int issue_number; text kind; timestamptz created_at }
    LEASE { uuid id PK; text work_key FK; uuid runner_id FK; text status; timestamptz acquired_at; timestamptz expires_at }
    RUN { uuid id PK; uuid tenant_id FK; uuid project_id FK; uuid runner_id FK; text remote; int issue_number; text status; text current_state; text branch; text pr_url; text blocked_reason; int retry_count; text timeout_phase; int clarification_round; timestamptz started_at; timestamptz finished_at }
    RUN_EVENT { uuid id PK; uuid run_id FK; text event; jsonb data; timestamptz ts }
    GITHUB_APP_INSTALL { uuid id PK; uuid tenant_id FK; text org; bigint app_id; bigint installation_id; text private_key_ref }
    SECRET_REF { uuid id PK; uuid tenant_id FK; text key; text store; text path; text delivery }
```

Mappaus: `RUN` = `run.json`-skeema + tenant/project/runner-FK:t (host→runner_id). `RUN_EVENT` = `state.jsonl` rivi-per-rivi (event + JSONB data + ts). `status`-enum säilyy (`initialized/completed/blocked/lost_race/cancelled/merged/pr_conflicted/timed_out/awaiting_clarification`). `SECRET_REF.delivery ∈ {proxy, env}` (§9).

### API: REST + SSE (ei gRPC)
gRPC hylätty: dashboard on web (REST natiivi, gRPC vaatii proxyn), kuorma matala, `gh`-ekosysteemi on REST. SSE live-tailiin (Go `http.Flusher`, ei kirjastoa).

```
# Runner ↔ keskus (kone-identiteetti)
POST /v1/runners/register      → {runner_id, runner_token}
POST /v1/runners/{id}/heartbeat
POST /v1/leases/acquire        → {lease|null, work, project_config}
POST /v1/leases/{id}/heartbeat · /release
POST /v1/runs · PATCH /v1/runs/{id} · POST /v1/runs/{id}/events
GET  /v1/github-app/token?tenant&org → {token, expires_at}
# CLI/Dashboard ↔ keskus (ihmis-identiteetti)
POST /v1/auth/github/device
POST /v1/projects · GET /v1/projects[/{id}]
GET  /v1/runs?project&mine&status · GET /v1/runs/{id} · /logs (SSE)
GET  /v1/runners · POST /v1/secrets · PUT /v1/projects/{id}/merge-policy
```

**Telemetria = push, ei poll.** Runner työntää: Run-tila (PATCH), RunEventit (batch, 5 s / 20 eventtiä, puskuroitu levylle keskuksen häiriön varalta), ajolokit. EI agentin promptteja/diffejä (päätös 7) — vain päätösrivit (`CYCLE_REVIEW_DECISION:` / `IMPLEMENTER_RESULT:`).

---

## 7. Identiteetti, auth & roolit

Kolme tasoa erillään:

**(a) Ihmiskäyttäjä → keskus: GitHub OAuth Device Flow.** `flowctl login` näyttää koodin, käyttäjä syöttää sen selaimessa (toimii headless-Studiolla SSH:n yli). Keskus vaihtaa session-tokeniin (`~/.config/flow/credentials`, chmod 600). RBAC: `USER.role ∈ {admin, developer}` per tenant.

**(b) Runner → keskus: rekisteröintitoken → runner-token.** `flowctl runner register` → keskus luo RUNNER-rivin + palauttaa pitkäikäisen runner-tokenin (Docker-secret). Scope vain runner-endpointteihin.

**(c) GitHub App -installaatiot per tenant/org: multi-installaatio-rekisteri + token broker.** Yleistää `github-app-auth.sh`:n yhden tripletin `GITHUB_APP_INSTALL`-tauluksi. Runner pyytää `GET /v1/github-app/token?tenant&org`, broker mintaa oikealle orgille (JWT→installation-token). Cache keskitetysti. Poistaa ei-origin-bypassin.

### Roolirajat
| Toiminto | Developer | Admin |
|---|---|---|
| Rekisteröi projekti (wizard) | ✅ | ✅ |
| Näkee omat ajot | ✅ | ✅ |
| Näkee koko tenantin ajot | ❌ | ✅ |
| Rekisteröi oman koneen runneriksi | ✅ (henkilökohtainen pool) | ✅ |
| Hallitsee jaettuja runnereita | ❌ | ✅ |
| Asettaa/muokkaa secretsejä | ❌ | ✅ |
| Muokkaa merge-policya | ❌ | ✅ |
| Asettaa GitHub App -installaation | ❌ | ✅ |
| Lukee toisen tenantin dataa | ❌ | ❌ (tenant-raja absoluuttinen) |

Tenant-eristys middlewaressa (ei sovelluskoodin varassa). Cross-tenant super-admin = scope-out.

---

## 8. Wizard-vuon datamalli

`flowctl init` (interaktiivinen tai `--config flow.yaml` CI:lle). Yhdistää nykyiset `run-issues.json` + `watchlist.json` + env-tiedoston yhdeksi PROJECT-entiteetiksi.

| Kenttä | Lähde ennen | Validointi |
|---|---|---|
| `name` | (uusi) | uniikki per tenant |
| `owner_repo` | gh-cwd | regex + App-installaation olemassaolo |
| `remotes[]` | watchlist | resolvoituu owner/repoksi; objekti `{remote, base_branch?}` (päätös 14) |
| `labels[]` | watchlist (oletus `auto-run`) | — |
| `base_branch` | run-issues.json | branch olemassa (App-token); **projektitason default** — per-remote override resolvoituu `remotes[].base_branch ?? base_branch` (päätös 14) |
| `runner_pool` | hostname-hardkoodaus (poistuu) | pool olemassa + oikeus |
| `claude_timeout_seconds` | run-issues.json | positiivinen int |
| `merge_policy` | PR_WATCH_MERGE_LABEL + conflict-flag | enum/bool |
| `secret_refs{}` | `~/.config/run-issues/env` | **viittaukset, EI arvot** |

Validointi keskuksessa (`POST /v1/projects`), ei vain CLI:ssä.

---

## 9. Secrets-arkkitehtuuri

Korvaa `source_machine_env` + henkilökohtaisen `op`:n. **Keskus broker-roolissa, lyhytikäiset tokenit per lease.**

- **MVP-store:** Postgres + `pgcrypto` (avain keskuksen Docker-secretissä). Riittää Tailscale-self-hostiin. `SECRET_REF.store` abstrahoi → myöhempi vaihto Vault/Infisical mahdollinen.
- **Toimituspiste (v2-muutos):** korkea-arvoiset verkkocredentiaalit (GitHub-token) toimitetaan **egress-proxyn kautta header-injektiona** (§11.3) — EIVÄT kontin env:iin. Ajonaikaiset conn-stringit (esim. `DATABASE_URL_TEST`) voidaan injektoida kontin env:iin lease-scoped. Luokittelu: `SECRET_REF.delivery ∈ {proxy, env}`.
- **Claude-auth on poikkeus (päätös 6):** per-kone plan-billing, ei keskuksen kautta. Runner provisioi sen tiedostona konttiin (§12). Keskus ei näe Claude-credentiaalia.

---

## 10. Lease vs. autonomia — yhteenveto invarianteista

- Aloitettu ajo selviää keskuksen häiriöstä loppuun asti (vain telemetria puskuroituu).
- Uutta työtä ei jaeta ilman keskitettyä leasea (fail-closed).
- Lease varmistetaan ennen sivuvaikutuksellisia operaatioita (PR-luonti) → split-brain-suoja (R5).

---

## 11. Runner-eristysmalli (R3-toteutus)

> **Ydinperiaate:** credentiaali ei elä kontissa lainkaan → malware ei voi varastaa sitä mitä siellä ei ole. Capability-pohjainen eristys, ei luottamuspohjainen.

### 11.1 Kontin elinkaari per ajo (Taso 1, MVP)
Jokainen ajo saa oman ephemeraalin kontin:
```
docker run --rm \
  --user 65532:65532          # non-root
  --read-only                 # rootfs read-only
  --tmpfs /tmp                 # kirjoitettava scratch
  --cap-drop=ALL               # pudota kaikki capabilityt
  --security-opt no-new-privileges
  --network flow-egress-<runner>  # vain egress-proxy-verkko
  --memory <m> --cpus <n> --pids-limit <p>
  -v <worktree>:/work          # AINOA host-mount
  -e HTTPS_PROXY=http://egress-proxy:3128
  flow-orchestrator:<ver> orchestrate <run-id>
```
Invariantti: ei host-mountteja paitsi per-ajo worktree, EI Docker-socketia (`/var/run/docker.sock`).

### 11.2 Egress-proxy = sidecar per runner-host (EI per kontti)
Per-kontti hylätty: proxy olisi samassa luottamusrajassa kuin agentti → credentiaali vuotaisi. Per-runner = proxy on kontin **ulkopuolella**. Proxy ajetaan erillisenä konttina; ajo-kontit liitetään vain proxy-verkkoon → ainoa reitti ulos on proxy.

Proxyn kolme vastuuta:
1. **Egress allow-list (default-deny):** ipset/iptables sallii vain `github.com`, `api.github.com`, `registry.npmjs.org`, `repo.packagist.org`, `api.anthropic.com`. Pohja: **Anthropicin Claude Code -devcontainerin `init-firewall.sh`** (tuotteistettu referenssi).
2. **Credentials-injektio (§11.3).**
3. **Scoped git-write:** push vain `auto-run/*`-haaroihin, estää force-pushin mainiin + haaran poiston.

### 11.3 Credentials-injektio (kytkös §7.3 + §9)
```
flowd (token broker) ──(2) tenant-scoped token──► flow-runner
                                                       │ (3) konfiguroi proxy: lease→token
                                                       │ (4) docker run (EI tokenia env:ssä)
   ┌───────────────────────────────────────────────────┐
   │ EGRESS-PROXY (luottamusraja: runner)              │
   │  (6) inject Authorization: Bearer ghs_… (vain GH) │
   └───────────────────▲───────────────────────────────┘
   ┌───────────────────┴───────────────────────────────┐
   │ AJO-KONTTI (luottamusraja: untrusted tenant-koodi)│
   │  (5) git push auto-run/… ILMAN tokenia ───────────┼──► (7) proxy → GitHub
   │  EI näe ghs_…, EI muiden tenanttien credentiaaleja│
   └────────────────────────────────────────────────────┘
```
Raaka credentiaali ei koskaan ylitä luottamusrajaa konttiin. Prompt-injektio voi ajaa mitä tahansa kontissa, mutta `cat $TOKEN` ei tuota mitään.

### 11.4 Scoped git-write (R3c — päätös 12)
**Ensisijainen vahvistus (MVP): GitHub App -permission-scope + branch protection.** Branch protection suojatuilla haaroilla (main): vaadi PR, estä force-push + haaran poisto. App-token rajaa write-oikeudet konfiguroituihin repoihin. Proxy tekee vain **karkean** tarkistuksen — se MITM:ää `github.com`:n jo credentials-injektiota varten (§11.3), joten se erottaa halvalla `git-receive-pack` (write) vs. `git-upload-pack` (read) ja kohde-repon.

**Lykätty roadmapille (Taso 2 / defense-in-depth):** proxy parsii git-smart-HTTP `git-receive-pack`-operaatiot pkt-line-tasolla: sallii vain `auto-run/*`, estää non-fast-forwardin suojattuihin haaroihin. EI MVP:ssä, koska pkt-line-parsinta on hauras (git protocol v2 voi rikkoa) eikä sisäinen luottamusmalli (päätös 10) sitä vaadi. Toteutetaan vain jos jaettu untrusted-tenant-runner sitä edellyttää.

### 11.5 Claude-auth kontissa (kytkös R4)
- **Malli A:** proxy injektoi Claude-credentiaalin (vaatii TLS-MITM api.anthropic.com:lle). Ideaali, credentiaali pois kontista. **Lykätty roadmap-optioksi (päätös 11)** — revisioidaan vain jos jaettu untrusted-tenant-runner asiakkaille toteutuu.
- **Malli B (MVP, valittu — päätös 11):** Claude-auth elää kontissa tiedostona. Per-kone plan-billing rajaa vahingon koneen kiintiöön (ei tenant-dataan). **Todennettu R4-kokeella (§12).**

### 11.6 Egress-lokit → dashboard (päätös 7)
Proxy lokittaa `{lease_id, tenant_id, run_id, host, allowed|denied, ts}` (EI sisältöä/credentiaalia) → admin näkee mihin runnerit ottivat yhteyttä. Denied-yhteydet = kiertoyritys-signaali.

### Roadmap — Taso 2 (gVisor, opt-in)
`--runtime=runsc` — user-space-kernel, VM-luokan eristys ilman VM-painoa. Suojaa kernel-escape-luokalta. Ei Firecracker/microVM/Lima (ylituotantoa; Lima macOS-spesifi).

---

## 12. R4-koe — claude-code plan-auth Docker-kontissa (TODENNETTU)

**Kysymys:** toimiiko claude-code plan-billing -autentikointi headless-Docker-kontissa? (Koko runner-Docker-mallin estävä portti.)

**Koeasetelma:** baked image (`node:22-slim` + `@anthropic-ai/claude-code`), credentiaali macOS Keychainistä poimittuna tiedostoksi (`claudeAiOauth.{accessToken,refreshToken,...}` = Linuxin `~/.claude/.credentials.json`-muoto), mountattuna read-only ja kopioituna `$HOME/.claude/.credentials.json`:iin. Koeharness: `~/.config/flow-experiment/` (Dockerfile + run-test-a.sh + README; credentiaali poistettu ajon jälkeen).

**Tulos (2026-06-01):**

| Variantti | Tila | Huom |
|---|---|---|
| open-default-user | ✅ PASS | `subscriptionType=max`, plan-billing |
| hardened (§11.1) | ✅ PASS | `--cap-drop=ALL` + `no-new-privileges` + `--read-only` + tmpfs HOME/tmp + non-root + resurssirajat **eivät riko** claude-codea |

**Johtopäätös:** R4(1) ratkaistu. Runner provisioi Claude-credentiaalin **tiedostona** konttiin (Linuxin natiivi reitti; Studio/VPS:llä credentiaali on jo tiedostona, macOS-runnerilla poimitaan Keychainistä). Malli B (§11.5). R4(2) (credentiaali pois kontista) lykätty.

**Reprodusointi:**
```sh
security find-generic-password -s 'Claude Code-credentials' -w > ~/.config/flow-experiment/credentials.json
chmod 644 ~/.config/flow-experiment/credentials.json
bash ~/.config/flow-experiment/run-test-a.sh
rm -f ~/.config/flow-experiment/credentials.json   # siivoa credentiaali jälkeen
```

---

## 13. Portattavuus-kartta

| Nykyinen bash | Uusi Go-paketti | Toimenpide |
|---|---|---|
| `git-remote.sh: parse_owner_repo_from_remote_url, remote_label, session_suffix` | `internal/gitremote` | **PORT testeineen** |
| `issue.sh: truncate_for_github` · `issue-images.sh: extract_image_urls` | `internal/issue` | **PORT testeineen** |
| `issue.sh: detect_answer, parse_marker, build_marker` | `internal/clarify` | **PORT testeineen** |
| `pr-watch-lib.sh: pr_decide` | `internal/prwatch` | **PORT testeineen** |
| `env-bootstrap.sh: detect_package_manager, detect_composer` | `internal/envbootstrap` | **PORT testeineen** |
| `prompts/01..04*.md` | `internal/prompts` (`//go:embed`) | **KOPIO sellaisenaan** |
| `locking.sh` (mkdir) | `internal/lease` (keskus) | REWRITE → Postgres `FOR UPDATE SKIP LOCKED` |
| `issue.sh: claim/verify/unclaim, pick_oldest_unassigned` | keskuksen lease + skanneri | REWRITE |
| `issue.sh: comment_issue, fetch_issue_json` | `internal/ghclient` | REWRITE (go-github / gh-shell-out, R6) |
| `state.sh` | `internal/runstate` + keskuksen RUN/RUN_EVENT | REWRITE (tiedosto→API-push, skeema säilyy) |
| `github-app-auth.sh` | `internal/githubapp` (broker) | REWRITE (yhdestä→monesta installaatiosta; `crypto/rsa`+`golang-jwt`) |
| `claude-call.sh` | `internal/claude` (runner) | REWRITE (`os/exec`+`context.WithTimeout`) |
| `pr-watch.sh` (P1–P9) | `internal/prwatch` | REWRITE (blueprint säilyy) |
| `worktree.sh` | `internal/worktree` | REWRITE |
| `poller.sh: scan_*, finalize_stalled` | keskuksen skanneri + lease-reaping | REWRITE (liveness → heartbeat-TTL) |
| `orchestrate.sh` (S1–S12) | `internal/orchestrator` (runner) | REWRITE (lukko+claim → lease-acquire) |

Testit: PORT-rivien `tests/test-*.sh`-fixturet → Go table-driven -testit samoilla fixtureilla.

---

## 14. Repo-rakenne

Yksi repo, kolme binääriä (`cmd/`-alihakemistot), jaettu `internal/`. Dashboard erillisenä.

```
flow/
├── cmd/{flowctl,flow-runner,flowd}/main.go
├── internal/{gitremote,issue,clarify,prwatch,envbootstrap,        # PORT-kohteet
│             lease,runstate,orchestrator,worktree,claude,
│             githubapp,ghclient,api,store,auth,secrets,prompts}
├── prompts/                  # KOPIO dotfilesista: 01..04
├── migrations/               # Postgres-skeema (golang-migrate)
├── dashboard/                # erillinen web-frontend
├── deploy/                   # docker-compose.yml + Dockerfile.{flowd,runner,dashboard,orchestrator}
├── docs/diagrams/            # .mmd-kaaviot
├── go.mod
└── README.md
```
"Single-binary" (päätös 2) viittaa jakeluun: `flowctl` on yksi tiedosto devaajalle. Runner/keskus ovat eri rooleja eri koneilla → erilliset binäärit samasta `go build ./cmd/...`.

**Kopiointi pohjaksi:** (1) `prompts/01..04` sellaisenaan; (2) PORT-funktiot + testifixturet bashista Goksi; (3) S1–S12-vaihelista + status-enum + `docs/diagrams/`-mmd:t blueprintinä. EI kopioida bash-mekaniikkaa (mkdir-lukko, tmux, LaunchAgent, source_machine_env).

---

## 15. Vaiheistus (vaiheet, ei aikatauluja)

> Periaate: nykyinen single-user toimii jo. Jokainen vaihe tuottaa käyttökelpoista; ei rikota olemassa olevaa ennen korvaavan todistamista.

- **Vaihe 0 — Diagnostinen pohja.** PORT-funktiot Goon testeineen + Postgres-skeema + `flowd` käynnistyy tyhjänä. Riskitön.
- **Vaihe 0.5 — R4-koe.** ✅ **TEHTY** (§12). Estävä portti läpäisty.
- **Vaihe 1 — MVP tiimikäyttöön.** Keskitetty lease (§5) + Run/RunEvent-API + skanneri + `flow-runner` (S1–S12 REWRITE) per-ajo **kovennettu kontti + egress/credentials-proxy** (§11) + `flowctl status` + read-only dashboard (sis. egress-lokit) + kevyt Prometheus-`/metrics`-endpoint `flowd`:lle (päätös 13, #22). Single-tenant datassa, `tenant_id` skeemassa valmiina. **Arvo:** kaksi devaajaa eri gh-loginilla ajavat ilman tuplatyötä (lease) JA untrusted-koodi ajetaan eristyksessä (credentiaalit eivät vuoda).
- **Vaihe 2 — Multi-tenancy + RBAC + auth.** Tenant-eristysmiddleware, OAuth device flow, runner-token, roolit, per-tenant App-rekisteri (ei-origin-bypass poistuu) + **append-only audit-loki** (päätös 13, #21: kuka muutti merge-policyä/rekisteröi projektin/myönsi runner-tokenin — vaatii roolit).
- **Vaihe 3 — Wizard + secrets-broker + per-projekti-konfiguraatio.** `flowctl init` (§8), secret broker (§9, delivery proxy/env), admin-dashboard (runner/secret/merge-policy).
- **Vaihe 4 — PR-watch + viimeistely + Taso 2.** `internal/prwatch` P1–P9 + AI-konfliktinratkaisu, Docker-deploy-paketointi (sis. **`pg_dump`-backup + retention + offsite**, päätös 13), Tailscale-dokumentaatio, SSE live-tail, **gVisor opt-in** (Taso 2), pkt-line-proxy-parsinta (päätös 12, jos tarpeen), OTel-tracing (päätös 13, jos tarpeen).

---

## 16. Riskit

| # | Riski | Mitigaatio |
|---|---|---|
| R1 | Keskus = uuden työn pullonkaula (fail-closed) | Kevyt Go+Postgres, Docker restart=always, Tailscale-sisäinen. Käynnissä oleva työ ei nojaa keskukseen silmukassa (testattava invariantti). |
| R2 | GitHub rate-limit multi-tenantissa | Vain keskus pollaa GitHubia; per-org App = oma 15k/h-kiintiö; skanneri-väli konfiguroitava. Webhook-pickup = myöhempi optio. |
| R3 | Untrusted-tenant-koodi jaetulla runnerilla | **Ratkaistu (§11):** credentiaali pois kontista + egress default-deny + scoped write. Jäännös: R3a egress-ohitus (testattava: `curl example.com` ei läpäise), R3b DNS-rebinding (DNS proxyn kautta), R3c git-write (päätös 12: App-scope + branch protection; pkt-line-parsinta lykätty), R3e resurssi-DoS (`--memory/--pids-limit`). |
| R4 | Claude-auth Dockerissa | **Ratkaistu (§12).** Malli B hyväksytty (päätös 11). R4(2)/Malli A lykätty roadmap-optioksi. |
| R5 | Lease-reaping split-brain | TTL 15min ≫ heartbeat 60s; lease varmistetaan ennen PR-luontia. |
| R6 | ghclient: go-github vs. gh-shell-out | go-github keskuksen skannerille, gh-shell-out runnerin git-operaatioille. |

---

## 17. Avoimet kysymykset (myöhempiin päätöksiin)

> **Status:** kaikki viisi ratkaistu 2026-06-03 (issue #15). Päätökset kirjattu päätöslokiin §2.2 (päätökset 10–14). Säilytetään tässä jäljitettävyyden vuoksi.

- ~~**Kaupallisuus:** onko Flow myös asiakkaille myytävä tuote (billing/SLA/eristystakuut) vai Silonin sisäinen?~~ → **RATKAISTU** (päätös 10): sisäinen, commercial-ready. Luottamusmalli MVP:ssä = Silonin oma koodi, ei untrusted-tenantteja jaetulla runnerilla.
- ~~**R4(2):** halutaanko Claude-credentiaali pois kontista (malli A, TLS-MITM) vai riittääkö malli B?~~ → **RATKAISTU** (päätös 11): Malli B; Malli A lykätty roadmap-optioksi (vain jaettu untrusted-tenant-runner laukaisisi sen).
- ~~**R3c:** scoped git-write proxy-parsinnalla vai GitHub App -scopella?~~ → **RATKAISTU** (päätös 12): App-scope + branch protection ensisijaisesti; pkt-line-proxy-parsinta lykätty roadmapille (Taso 2).
- ~~**Audit-loki & observability:** metrics/tracing keskukseen, Postgres-backup-strategia.~~ → **RATKAISTU** (päätös 13): kevyt Prometheus-`/metrics`; audit-loki Vaihe 2:een; OTel-tracing roadmapille; `pg_dump`-backup dokumentoituna #13:ssa.
- ~~**base_branch per remote:** jos eri orgeissa tarvitaan eri integraatiohaara.~~ → **RATKAISTU** (päätös 14): projektitaso default + valinnainen per-remote override `remotes`-jsonb:ssä. Ei migraatiota.

---

## Liite A — Lähdeviitteet
- Nykytoteutus: `dotfiles/claude/scripts/run-issues/` (orchestrate.sh, poller.sh, pr-watch.sh, lib/*.sh, prompts/, tests/) + dotfiles `CLAUDE.md` (yksityiskohtainen kuvaus).
- Egress-firewall-pohja: Anthropic Claude Code devcontainer `init-firewall.sh` (ipset + iptables default-deny).
- R4-koeharness: `~/.config/flow-experiment/`.
- Tilakaaviot: `dotfiles/docs/diagrams/run-issues-*.mmd`, `pr-watch-state-machine.mmd`.
