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

## Vaiheistus

- **Vaihe 0** — diagnostinen pohja: PORT-funktiot Goon testeineen, Postgres-
  skeema, `flowd` käynnistyy tyhjänä. (Valmis.)
- **Vaihe 1** — MVP: keskitetty lease, Run/RunEvent-API, skanneri,
  `flow-runner` (S1–S12), kovennettu kontti + egress/credentials-proxy,
  `flowctl status`, read-only dashboard. Single-tenant datassa, `tenant_id`
  skeemassa valmiina.

Vaiheet 2–4 (multi-tenancy/RBAC/OAuth, wizard/secrets-broker, pr-watch/gVisor)
ovat roadmapilla, ks. suunnitelma §15.
