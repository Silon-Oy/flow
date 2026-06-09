# CLAUDE.md — Flow

Go-pohjainen monikäyttäjä-orkestraattori (`run-issues` irrotettuna tiimituotteeksi).
Tämä tiedosto ohjaa kehitystyötä tässä repossa. Globaalit ohjeet (~/.claude/CLAUDE.md)
pätevät normaalisti — älä toista niitä tässä.

## Totuuden lähde

- **Arkkitehtuuri:** [`docs/flow-arkkitehtuuri.md`](docs/flow-arkkitehtuuri.md) on
  kanoninen suunnitelma. Lue relevantti pykälä ennen kuin toteutat — kaikki
  invariantit, päätökset ja portattavuus-kartta ovat siellä.
- **Päätösloki:** §2.2. Kun teet arkkitehtuurivalinnan, kirjaa se sinne.
- **§17:n avoimet päätökset (10–14) on ratkaistu** (issue #15 suljettu) ja
  kirjattu päätöslokiin §2.2. Uudet arkkitehtuurivalinnat → §2.2.

## Status & roadmap

Vaihe 0 + 1 (MVP-runko) on toteutettu. `internal/{auth,githubapp,secrets}` ovat
tyhjiä seamejä Vaiheille 2–3. Kehitystyö on jäsennetty GitHub-milestoneilla
suunnitelman §15-vaiheistuksen mukaan (avoimet työt:
[issue-lista](https://github.com/Silon-Oy/flow/issues)). README pidetään
puhtaana käyttöönotto-/käyttöoppaana — roadmap asuu **tässä**.

- **Vaihe 0** — diagnostinen pohja: PORT-funktiot Goon testeineen, Postgres-
  skeema, `flowd` käynnistyy tyhjänä. **Valmis.**
- **Vaihe 1 — MVP** (runko valmis): keskitetty lease (§5, `internal/lease`),
  Run/RunEvent-API + SSE (`internal/api`), GitHub-skanneri (`internal/ghclient`),
  `flow-runner` pull-loop + S1–S12-orkestraattori (`internal/orchestrator`),
  kovennetun kontin `docker run`-flagit (`internal/runnerexec`, §11.1),
  egress-proxy (`deploy/egress-proxy`, §11.2/6), `flowctl status`, read-only
  dashboard. Single-tenant datassa, `tenant_id` skeemassa valmiina.
  Viimeistelyn langanpäät → [milestone 1](https://github.com/Silon-Oy/flow/milestone/1):
  kontti-dispatch [#1](https://github.com/Silon-Oy/flow/issues/1),
  ghclient issue-fetch [#2](https://github.com/Silon-Oy/flow/issues/2),
  egress-loki [#3](https://github.com/Silon-Oy/flow/issues/3).
- **Vaihe 2 — Multi-tenancy + RBAC + auth** →
  [milestone 2](https://github.com/Silon-Oy/flow/milestone/2): tenant-middleware
  [#4](https://github.com/Silon-Oy/flow/issues/4), OAuth device flow
  [#5](https://github.com/Silon-Oy/flow/issues/5), runner-token
  [#6](https://github.com/Silon-Oy/flow/issues/6), roolit
  [#7](https://github.com/Silon-Oy/flow/issues/7), App-broker
  [#8](https://github.com/Silon-Oy/flow/issues/8), audit-loki
  [#21](https://github.com/Silon-Oy/flow/issues/21).
- **Vaihe 3 — Wizard + secrets-broker** →
  [milestone 3](https://github.com/Silon-Oy/flow/milestone/3): `flowctl init`
  [#9](https://github.com/Silon-Oy/flow/issues/9), secrets-broker
  [#10](https://github.com/Silon-Oy/flow/issues/10), admin-dashboard
  [#11](https://github.com/Silon-Oy/flow/issues/11).
- **Vaihe 4 — PR-watch + viimeistely + Taso 2** →
  [milestone 4](https://github.com/Silon-Oy/flow/milestone/4): `internal/prwatch`
  [#12](https://github.com/Silon-Oy/flow/issues/12), deploy + Tailscale
  [#13](https://github.com/Silon-Oy/flow/issues/13), gVisor opt-in
  [#14](https://github.com/Silon-Oy/flow/issues/14), Prometheus /metrics
  [#22](https://github.com/Silon-Oy/flow/issues/22).

Suunnitelman §17 avoimet päätökset (10–14) on ratkaistu ja kirjattu
päätöslokiin §2.2 ([#15](https://github.com/Silon-Oy/flow/issues/15) suljettu).
Seuranta-issuet: audit-loki [#21](https://github.com/Silon-Oy/flow/issues/21),
metrics-endpoint [#22](https://github.com/Silon-Oy/flow/issues/22).

## Build & test

```sh
go build ./...
go test ./...                      # DB-riippuvaiset testit ohitetaan ilman DSN:ää
```

DB-testit (`internal/{store,lease,runstate,api}`) vaativat `FLOW_TEST_DSN`:n
throwaway-Postgresia (16) vasten — ks. README. **Aja `go test ./...` vihreäksi
ennen committia.** Go 1.26+.

## Arkkitehtuuriset invariantit — ÄLÄ riko ilman §2.2-päätöstä

1. **Lease fail-closed (§5/§10):** uutta työtä ei jaeta ilman keskitettyä
   leasea. Ei GitHub-`@me`-fallbackia keskuksen ollessa alhaalla — se toisi
   takaisin kaksi-arbiteria-ongelman. `ErrNoWork` ≠ DB-virhe.
2. **Aloitettu ajo selviää keskuksen häiriöstä loppuun** — vain telemetria
   puskuroituu levylle. Käynnissä oleva työ ei saa nojata keskukseen silmukassa.
3. **Raaka credentiaali EI koskaan ylitä luottamusrajaa konttiin (§11.3).**
   `internal/runnerexec`-invariantit pakotetaan testeillä: ei `docker.sock`-
   mounttia, ei raakaa tokenia kontin env:iin, `--cap-drop=ALL` +
   `no-new-privileges` + `--read-only` + non-root + resurssirajat, worktree
   ainoa host-mount. Jos kosket runnerexeciin, näiden testien on pysyttävä
   vihreinä.
4. **`tenant_id` läpäisee tietomallin.** Single-tenant datassa Vaiheessa 1,
   mutta skeema on valmis. Vaiheessa 2 tenant-raja pakotetaan **middlewaressa**,
   ei sovelluskoodissa.
5. **`run_status`-enum on kanoninen** (`initialized/completed/blocked/lost_race/
   cancelled/merged/pr_conflicted/timed_out/awaiting_clarification`). Älä lisää
   arvoja kevyesti — ne mäppäävät bashin tila-koneeseen.
6. **RUN_EVENT-järjestys = `ORDER BY (ts, seq)`.** `ts` ei ole yksilöivä
   (batch-eventit). Älä järjestä pelkän `ts`:n tai uuid-PK:n mukaan
   (regressiotesti `TestRunLifecycleAndEvents` suojaa tätä).
7. **Dashboard = metadata + lokit (päätös 7).** EI agentin promptteja/diffejä.
   Dynaaminen data DOM:lla/`textContent`illa, ei `innerHTML` (XSS).

## PORT vs. REWRITE (§13)

Kaksi eri luonteista pakettiryhmää — kohtele eri tavoin:

- **PORT-paketit** (`gitremote, issue, clarify, prwatch, envbootstrap`): bashin
  puhtaiden funktioiden tarkkoja portteja. Niillä on **table-driven-testit
  samoilla fixtureilla** kuin alkuperäisellä bashilla. Käytös on lukittu —
  muuta vain jos bash-spesifikaatio muuttuu, ja päivitä testit vastaavasti.
- **REWRITE-paketit** (`lease, runstate, orchestrator, worktree, claude,
  ghclient, api, githubapp` …): keskuskeskeisiä uudelleenkirjoituksia. EI
  bash-mekaniikan (mkdir-lukko, tmux, LaunchAgent, `source_machine_env`,
  `@me`) kopiointia — vain logiikka/blueprint säilyy.

Bash-lähde on **spesifikaatio**, ei kopioitava pohja: `dotfiles/claude/scripts/
run-issues/`.

## Promptit

Kanoniset, embedatut kopiot: `internal/prompts/files/` (`//go:embed` ei yllä
parent-hakemistoon). Repo-juuren `prompts/` on identtinen **ihmisviite** —
**pidä molemmat synkassa** jos muokkaat promptia.

## Migraatiot

golang-migrate, upotettu ajuri (`migrations/embed.go`). Lisää uusi numeroitu
`up/down`-pari — **älä muokkaa jo sovellettua migraatiota**. Skeema kaikkine
entiteetteineen on `000001_init.up.sql`.

## Tämä repo on JULKINEN (Silon-Oy/flow)

- **Ei salaisuuksia, sisäisiä hostnameja, Tailscale-IP:itä eikä asiakaskohtaista
  dataa** committeihin, prompteihin, lokeihin tai issueihin. Jokainen commit on
  pysyvästi nähtävillä.
- `secret_refs` ovat **viittauksia, ei arvoja** (§8/§9). `.env.example` vain
  placeholdereita.

## Työnkulku

- Työ jäsennetty issueina + milestoneina (§15-vaiheet). Etene järjestyksessä —
  vaiheet rakentuvat toistensa päälle.
- **`auto-run`-label laukaisee orkestraattorin** (kun se on käyttökunnossa). Älä
  lisää sitä keskeneräiseen tai epäselvään issueen.
- Älä committaa suoraan `main`-haaraan — tee feature-haara ja PR.
