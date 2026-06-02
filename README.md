# Flow

Go-pohjainen monikäyttäjä-orkestraattori, joka irrottaa dotfiles-repon
`run-issues`-bash-orkestraattorin itsenäiseksi tiimituotteeksi. GitHub pysyy
issue/PR-totuuden lähteenä; kevyt keskuspalvelu (`flowd`) hoitaa työn
omistajuuden (lease), runner-rekisterin ja telemetrian. Devaajat ja jaetut
auto-run-koneet ajavat orkestrointia eristetyissä Docker-konteissa.

Arkkitehtuurisuunnitelma (totuuden lähde): [`docs/flow-arkkitehtuuri.md`](docs/flow-arkkitehtuuri.md).

## Komponentit

- **`flowctl`** (`cmd/flowctl`) — devaajan/adminin CLI; puhuu vain keskukseen.
- **`flowd`** (`cmd/flowd`) — keskuspalvelu: lease-manager, runner-rekisteri,
  telemetria-sink, GitHub-skanneri, REST + SSE -API.
- **`flow-runner`** (`cmd/flow-runner`) — runner-daemon; pull-malli, ajaa
  S1–S12-orkestroinnin per-ajo kovennetussa kontissa.
- **egress-proxy** — sidecar per runner-host; egress allow-list +
  credentials-injektio + egress-loki.
- **dashboard** (`dashboard/`) — read-only; metadata + lokit.

## Repo-rakenne

```
cmd/{flowctl,flow-runner,flowd}/   binäärien entrypointit
internal/                          jaettu logiikka (PORT + REWRITE -paketit)
internal/prompts/files/            embedatut agentti-promptit (//go:embed)
prompts/                           promptien ihmisviite (identtinen kopio)
migrations/                        Postgres-skeema (golang-migrate)
deploy/                            docker-compose + Dockerfilet
dashboard/                         web-frontend
docs/                              arkkitehtuuri + kaaviot
```

## Kehitys

Vaatimukset: Go 1.26+, Docker, Postgres 16.

```sh
go build ./...
go test ./...
```

Migraatiot ajetaan golang-migratella (CLI tai upotettu ajuri); ks.
`migrations/` ja `deploy/docker-compose.yml`.

## Vaiheistus & roadmap

Kehitystyö on jäsennetty GitHub-milestoneilla suunnitelman §15-vaiheistuksen
mukaan. Avoimet työt: [issue-lista](https://github.com/Silon-Oy/flow/issues).

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
  [#8](https://github.com/Silon-Oy/flow/issues/8).
- **Vaihe 3 — Wizard + secrets-broker** →
  [milestone 3](https://github.com/Silon-Oy/flow/milestone/3): `flowctl init`
  [#9](https://github.com/Silon-Oy/flow/issues/9), secrets-broker
  [#10](https://github.com/Silon-Oy/flow/issues/10), admin-dashboard
  [#11](https://github.com/Silon-Oy/flow/issues/11).
- **Vaihe 4 — PR-watch + viimeistely + Taso 2** →
  [milestone 4](https://github.com/Silon-Oy/flow/milestone/4): `internal/prwatch`
  [#12](https://github.com/Silon-Oy/flow/issues/12), deploy + Tailscale
  [#13](https://github.com/Silon-Oy/flow/issues/13), gVisor opt-in
  [#14](https://github.com/Silon-Oy/flow/issues/14).

Avoimet päätökset (suunnitelma §17, ei toteutustehtäviä):
[#15](https://github.com/Silon-Oy/flow/issues/15).

## Postgres-integraatiotestit

DB-riippuvaiset testit (`internal/{store,lease,runstate,api}`) ohitetaan, ellei
`FLOW_TEST_DSN` ole asetettu. Aja throwaway-Postgresia vasten:

```sh
docker run -d --name flow-test-pg -e POSTGRES_USER=flow -e POSTGRES_PASSWORD=flow \
  -e POSTGRES_DB=flow -p 55432:5432 postgres:16
FLOW_TEST_DSN="postgres://flow:flow@localhost:55432/flow?sslmode=disable" go test ./...
```

## Käyttöön liittyvät oletukset (Vaihe 1)

- `flow-runner` ajaa orkestroinnin **in-process** oletuksena (kehityskoneella ei
  vaadita Dockeria). Tuotannon eristys = kovennettu per-ajo-kontti
  (`internal/runnerexec`, `deploy/Dockerfile.orchestrator`); dispatch on kytketty
  ajoon: kun `FLOW_RUNNER_MODE=container`, host luo per-ajo-worktreen ja
  dispatchaa orkestroinnin ephemeraaliin `flow-orchestrator orchestrate <run-id>`
  -konttiin runnerexec.Spec:n §11.1-flagein. Kontti hakee run-configin
  `GET /v1/runs/{id}`:llä; GitHub-tokenia ei koskaan välitetä env:iin (§11.3).
- Skanneri pollaa GitHubia anonyymisti, jos `FLOW_GITHUB_TOKEN` puuttuu
  (rate-limitattu). Per-tenant App-broker tulee Vaiheessa 2.
