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

## Status

Vaihe 0 + 1 (MVP-runko) on toteutettu. Vaiheet 2–4 ovat roadmapilla
GitHub-milestoneina (ks. README "Vaiheistus & roadmap" + issuet #1–#14).
`internal/{auth,githubapp,secrets}` ovat tyhjiä seamejä Vaiheille 2–3.

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
