# Flow

Flow ajaa tekoälypohjaista koodausagenttia GitHub-tehtävien (issue) pohjalta
automaattisesti — monella koneella yhtä aikaa, ilman että kaksi konetta tarttuu
samaan työhön, ja niin että ylläpitäjä näkee yhdestä näkymästä mitä on käynnissä.

Käytännössä: merkitset GitHubissa tehtävän valmiiksi ajettavaksi, ja Flow tekee
siitä koodimuutoksen ja avaa muutosehdotuksen (pull request) automaattisesti. Työ
suoritetaan eristetyssä hiekkalaatikossa, johon ei koskaan päädy salasanoja eikä
pääsytunnuksia raakana.

Tämä dokumentti opastaa järjestelmän **käyttöönotossa ja käytössä**. Tarkka
tekninen suunnitelma on erikseen: [`docs/flow-arkkitehtuuri.md`](docs/flow-arkkitehtuuri.md).

> **Tila:** Flow on toimiva runko (MVP). Sillä saa pystyyn keskuspalvelun,
> suorituskoneen ja hallintanäkymän, ja sillä voi ajaa tehtäviä päästä päähän.
> Osa monikäyttäjäominaisuuksista (tiukka käyttöoikeushallinta, salaisuuksien
> hallinta) on vielä kehityksessä — ks. kehitysvaiheet `CLAUDE.md`:stä.

---

## Sisällys

- [Mitä Flow tekee](#mitä-flow-tekee)
- [Käsitteet](#käsitteet)
- [Järjestelmän osat](#järjestelmän-osat)
- [Vaatimukset](#vaatimukset)
- [Käyttöönotto vaihe vaiheelta](#käyttöönotto-vaihe-vaiheelta)
- [Käyttö](#käyttö)
- [Turvallisuusmalli](#turvallisuusmalli-lyhyesti)
- [Paikallinen kehitys](#paikallinen-kehitys)
- [Vianetsintä](#vianetsintä)

---

## Mitä Flow tekee

Yhden tehtävän elinkaari menee näin:

1. **Merkitset tehtävän.** Lisäät GitHub-tehtävälle `auto-run`-merkinnän (label).
2. **Keskus huomaa työn.** Keskuspalvelu (flowd) tarkistaa GitHubista säännöllisin
   väliajoin, onko uusia `auto-run`-tehtäviä, ja lisää ne työjonoon.
3. **Suorituskone tarttuu työhön.** Vapaa suorituskone (runner) pyytää keskukselta
   yhden työn ja saa siihen yksinoikeuden (työn varaus, lease). Näin kaksi konetta
   ei voi koskaan tehdä samaa tehtävää.
4. **Agentti tekee työn eristetyssä kontissa.** Suorituskone käynnistää kertakäyttöisen,
   kovennetun kontin, jossa koodausagentti:
   - lukee tehtävän ja päättää, onko se valmis toteutettavaksi. Jos tehtävänanto on
     epäselvä, agentti **kysyy tarkennuksen** tehtävän kommenttina sen sijaan että
     arvaisi.
   - asentaa projektin riippuvuudet,
   - kirjoittaa koodimuutoksen,
   - tekee itsearvioinnin ja korjaa havaitut puutteet,
   - työntää muutoksen omaan haaraan ja avaa muutosehdotuksen (pull request).
5. **Seuraat etenemistä.** Näet tilan komentoriviltä (`flowctl status`) tai
   selaimessa hallintanäkymästä (dashboard). GitHubissa näkyy valmis PR.

GitHub pysyy koko ajan tehtävien ja muutosehdotusten "totuuden lähteenä" — Flow ei
korvaa GitHubia, vaan koordinoi työn jakamisen, suorituksen ja seurannan sen päälle.

---

## Käsitteet

Muutama vakiintunut termi, jotka toistuvat tässä dokumentissa:

| Termi | Suomeksi | Selitys |
|---|---|---|
| **issue** | tehtävä | GitHubin tehtävä- tai vikamerkintä, johon työ kohdistuu. |
| **pull request (PR)** | muutosehdotus | Ehdotus yhdistää koodihaaran muutokset päähaaraan; tarkastetaan ja hyväksytään GitHubissa. |
| **runner** | suorituskone | Pitkäikäinen prosessi (yleensä oma kone tai palvelin), joka ajaa varsinaisen työn. |
| **lease** | työn varaus | Keskuksen myöntämä määräaikainen yksinoikeus tehdä tietty työ — estää päällekkäisen työn. |
| **kontti (container)** | — | Eristetty kertakäyttöinen suoritusympäristö (Docker). Jokainen ajo saa omansa. |
| **keskus / keskuspalvelu** | flowd | Yhteinen palvelin, joka jakaa työt, pitää kirjaa suorituskoneista ja kerää lokit. |
| **tenant** | asiakasorganisaatio | Eristetty tila monikäyttäjämallissa. Tässä vaiheessa käytössä on yksi (`default`). |
| **egress-proxy** | ulosmenoliikenteen suodatin | Välityspalvelin, joka rajaa kontista ulos lähtevän verkkoliikenteen sallittuihin osoitteisiin. |
| **device flow** | laitevaltuutus | Kirjautumistapa, jossa syötät lyhyen koodin selaimessa — toimii myös etäpalvelimella ilman työpöytää. |

---

## Järjestelmän osat

Flow koostuu viidestä osasta ja yhdestä tietokannasta. Tyypillisessä asennuksessa
keskus, tietokanta, suorituskone, ulosmenosuodatin ja hallintanäkymä pyörivät
samalla palvelimella (esim. tiimin sisäverkossa), ja kehittäjät käyttävät vain
`flowctl`-komentoa omalta koneeltaan.

| Osa | Mikä se on |
|---|---|
| **`flowd`** | **Keskuspalvelu.** Jakaa työt (työn varaukset), pitää rekisteriä suorituskoneista, kerää telemetrian ja lokit, tarkkailee GitHubin `auto-run`-tehtäviä. Tarjoaa REST-rajapinnan ja reaaliaikaisen lokivirran. |
| **`flow-runner`** | **Suorituskone-taustaprosessi.** Pyytää keskukselta työtä, luo per-ajo-työhakemiston ja ajaa työn kovennetussa kontissa. Lähettää tilan ja lokit takaisin keskukselle. |
| **`flowctl`** | **Kehittäjän/ylläpitäjän komentorivityökalu.** Kirjautuminen, projektin rekisteröinti ja tilan tarkkailu. Puhuu vain keskukselle. |
| **egress-proxy** | **Ulosmenoliikenteen suodatin.** Sivuvaunu (sidecar) suorituskoneen rinnalla: sallii vain ennalta määritetyt verkko-osoitteet ja kirjaa ulosmenoliikenteen. |
| **dashboard** | **Hallintanäkymä selaimessa.** Vain luku: käynnissä olevat ajot, niiden tila ja lokit. |
| **PostgreSQL** | Keskuksen tietokanta: työn varaukset, projektit, ajot ja tapahtumat. |

---

## Vaatimukset

**Palvelin, jolle Flow asennetaan:**

- Docker ja Docker Compose
- Verkkoyhteys GitHubiin
- Suositus: sisäverkko (esim. VPN), jossa kehittäjien koneet ja palvelin näkevät
  toisensa. Flow on suunniteltu itse isännöitäväksi (self-host), ei julkiseen
  internetiin sellaisenaan.

**Kehittäjän koneella (jos haluat ajaa `flowctl`-komentoa paikallisesti):**

- Go 1.26 tai uudempi `flowctl`-binäärin kääntämiseen — tai valmis binääri.

**GitHubista:**

- Repositorio (yksityinen tai julkinen), jossa tehtävät ovat.
- Lukuoikeudellinen henkilökohtainen pääsytunnus (token) keskuksen
  GitHub-tarkkailijalle. *(Vapaaehtoinen — ilman tunnusta tarkkailija toimii
  anonyymisti, mutta GitHubin rajoitukset iskevät nopeasti.)*
- Suositus: **haarasuojaus (branch protection)** päähaaralle — vaadi muutokset
  PR:n kautta ja estä suora työntö. Flow nojaa tähän turvarajana.

---

## Käyttöönotto vaihe vaiheelta

Alla oleva polku pystyttää koko järjestelmän yhdellä palvelimella Docker
Composella. Pelkkä paikallinen kokeilu ilman Dockeria on kuvattu kohdassa
[Paikallinen kehitys](#paikallinen-kehitys).

### 1. Hae koodi

```sh
git clone https://github.com/Silon-Oy/flow.git
cd flow
```

### 2. Luo asetustiedosto

Asetukset annetaan `.env`-tiedostolla, jonka Docker Compose lukee
`deploy/`-kansiosta (compose ajetaan siellä, ks. kohta 5). Kopioi malli ja
täytä arvot:

```sh
cp .env.example deploy/.env
```

Avaa `deploy/.env` ja aseta vähintään nämä:

| Asetus | Mitä se on | Pakollinen |
|---|---|---|
| `POSTGRES_PASSWORD` | Tietokannan salasana. Valitse pitkä satunnainen merkkijono. | **Kyllä** |
| `FLOW_BOOTSTRAP_TENANT` | Asiakasorganisaation nimi. Yhden tiimin asennuksessa jätä arvoon `default`. | Ei (oletus `default`) |
| `FLOW_BOOTSTRAP_ADMIN` | GitHub-käyttäjänimesi. Tämä käyttäjä saa ylläpitäjäoikeudet käynnistyksessä. | Suositus |
| `FLOW_GITHUB_TOKEN` | Lukuoikeudellinen GitHub-pääsytunnus tehtävien tarkkailuun. | Ei (tyhjä = anonyymi) |
| `FLOW_GITHUB_OAUTH_CLIENT_ID` | GitHub OAuth -sovelluksen tunniste `flowctl login`-kirjautumista varten (ks. kohta 3). | Tarvitaan kirjautumiseen |

Loput asetukset ovat järkevillä oletuksilla. Tärkeimmät selitykset ovat
`.env.example`-tiedostossa kommentteina.

> **Tärkeää:** älä koskaan vie täytettyä `deploy/.env`-tiedostoa versionhallintaan.
> Salaisuudet annetaan vain tässä — ne eivät kuulu koodiin eivätkä lokeihin.

### 3. Valmistele GitHub

**(a) Tehtävien tarkkailu (vapaaehtoinen mutta suositeltu).**
Luo lukuoikeudellinen pääsytunnus, jolla keskus näkee repositorion tehtävät, ja
laita se `deploy/.env`-tiedoston `FLOW_GITHUB_TOKEN`-arvoksi. Ilman tunnusta keskus
pollaa GitHubia anonyymisti ja törmää nopeasti kyselyrajoituksiin.

**(b) Kehittäjien kirjautuminen (jos haluat `flowctl login`-toiminnon).**
Flow tunnistaa kehittäjät GitHubin laitevaltuutuksella (device flow): kirjautuessa
saat lyhyen koodin, jonka syötät selaimessa. Tämä toimii myös etäpalvelimella
SSH-yhteyden yli, koska mitään selainta ei tarvita itse palvelimella.

1. Mene osoitteeseen <https://github.com/settings/developers> → **New OAuth App**.
2. *Application name*: vapaa. *Homepage URL* ja *Authorization callback URL*: laita
   keskuspalvelun osoite. (Laitevaltuutuksessa callback-osoitteella ei ole
   merkitystä, mutta lomake vaatii sen.)
3. Laitevaltuutus tarvitsee vain **client ID**:n — *client secret*-arvoa **ei**
   tarvita. Kopioi client ID ja aseta se `deploy/.env`-tiedoston
   `FLOW_GITHUB_OAUTH_CLIENT_ID`-arvoksi.

Jos jätät tämän tyhjäksi, `flowctl login` ei ole käytössä (keskus palauttaa
kirjautumispyyntöihin virheen 503, mutta kaikki muu toimii normaalisti).

**(c) Haarasuojaus (suositus).**
Aseta repositorion päähaaralle suojaus: vaadi muutokset PR:n kautta ja estä
suorat työnnöt ja suojattujen haarojen poisto. Flow luottaa siihen, että agentin
tekemät muutokset käyvät PR-tarkastuksen läpi.

### 4. Rakenna kontti-image agentin ajoa varten

Tuotantotilassa jokainen ajo suoritetaan erillisessä, kovennetussa
**orkestraattorikontissa**. Suorituskone käynnistää nämä kontit tarpeen mukaan,
joten niiden image rakennetaan kerran etukäteen:

```sh
docker build -f deploy/Dockerfile.orchestrator -t flow-orchestrator:latest .
```

Tämä image sisältää koodausagentin (claude-code) ja Flow'n suorituslogiikan. Se
ei sisällä mitään projektin koodia eikä pääsytunnuksia — projektin lähdekoodi
liitetään konttiin vasta ajon aikana (oma per-ajo-työhakemisto), ja tunnukset
tuodaan eristyksen sisälle vasta silloin (ks.
[Turvallisuusmalli](#turvallisuusmalli-lyhyesti)).

**Tämä on kone-, ei projektikohtainen vaihe.** Image on geneerinen ajoympäristö,
joka kelpaa kaikille projekteille ja kaikille ajoille:

- **Rakenna kerran jokaisella suorituskoneella** (kone, jolla `flow-runner` ajaa
  kontti-tilassa). Pelkkä keskus (`flowd`) tai kehittäjän `flowctl`-kone **ei**
  tarvitse imagea. Jos suorituskoneita on monta, jokaisella pitää olla image
  rakennettuna (tai jaettuna konttirekisterin kautta).
- **Uusi projekti ei vaadi uutta buildia** — riittää `flowctl init` (ks.
  [Projektin rekisteröinti](#projektin-rekisteröinti)).
- **Rakenna image uudelleen vain, kun Flow itse päivittyy**: muuttuvat agentin
  kehotteet (`internal/prompts/files/`), orkestraattorin koodi tai claude-code-
  versio. Käytännössä `git pull` → sama `docker build` uudelleen.

> Jos haluat ajaa työt **ilman erillistä konttia** (esim. kehityskoneella, jossa
> ei ole Dockeria käytettävissä), voit jättää tämän väliin ja käyttää
> in-process-tilaa — ks. [Paikallinen kehitys](#paikallinen-kehitys). Eristysmalli
> ei silloin ole voimassa, joten käytä sitä vain luotetussa ympäristössä.

### 5. Käynnistä järjestelmä

Käynnistä keskus, tietokanta, suorituskone, ulosmenosuodatin ja hallintanäkymä:

```sh
cd deploy
docker compose up -d
```

Compose rakentaa ja käynnistää palvelut. Tietokannan skeema (taulut) luodaan
automaattisesti ensimmäisellä käynnistyksellä.

Tarkista, että palvelut nousivat:

```sh
docker compose ps
docker compose logs -f flowd
```

Oletusportit:

- **Keskuspalvelu (flowd):** `http://<palvelin>:8080`
- **Hallintanäkymä (dashboard):** `http://<palvelin>:8090`

### 6. Tarkista että toimii

Avaa hallintanäkymä selaimessa osoitteesta `http://<palvelin>:8090`. Sen pitäisi
latautua ja näyttää (toistaiseksi tyhjä) lista suorituskoneista ja ajoista.

Komentoriviltä:

```sh
# Aseta keskuksen osoite, jos flowctl ei ole samalla koneella:
export FLOW_CENTRAL_URL="http://<palvelin>:8080"

flowctl status
```

Tulosteessa pitäisi näkyä ainakin yksi rekisteröitynyt suorituskone (compose-pinon
`flow-runner`), jonka tila on aktiivinen ja elossapitoviesti (heartbeat) tuore.

### Päivittäminen uuteen versioon

Aja tuotantopalvelimella repon juuresta:

```sh
./deploy/deploy.sh
```

Skripti hakee `origin/main`-haaran (vain fast-forward), rakentaa muuttuneet
imaget ja käynnistää muuttuneet kontit uudelleen (`docker compose up -d
--build`). Tietokantamigraatiot ajetaan automaattisesti `flowd`-palvelun
käynnistyessä, ja Postgres-data säilyy `deploy/pgdata`-kansiossa. Skripti
kieltäytyy ajamasta, jos työpuussa on committoimattomia muutoksia, eikä
koskaan nollaa repoa.

> Huom: skripti ei rakenna orkestraattorin ajokuvaa (`flow-orchestrator`) —
> se ei ole compose-palvelu. Jos päivitys koskee agentin kehotteita tai
> orkestraattorin koodia, aja lisäksi kohdan 4 `docker build` uudelleen.

---

## Käyttö

### Kirjautuminen

Kehittäjä kirjautuu keskukseen GitHub-tunnuksellaan:

```sh
flowctl login
```

Komento näyttää osoitteen ja lyhyen koodin:

```
1. Open this URL in a browser:   https://github.com/login/device
2. Enter this code:              ABCD-1234
Waiting for authorization (Ctrl+C to cancel)...
Signed in as ollisaari.
Session token written to ~/.config/flow/credentials (chmod 600).
```

Avaat osoitteen selaimessa, syötät koodin ja hyväksyt — myös toiselta koneelta,
jos kirjaudut etäpalvelimelle. Istuntotunnus tallentuu tiedostoon
`~/.config/flow/credentials` oikeuksin `0600`. Seuraavat `flowctl`-komennot
käyttävät sitä automaattisesti.

> Polun voi ohittaa ympäristömuuttujilla `XDG_CONFIG_HOME` tai
> `FLOW_CREDENTIALS_PATH`. Raaka GitHub-tunnus ei poistu keskukselta — keskus
> vaihtaa sen omaan istuntotunnukseensa.

### Projektin rekisteröinti

Ennen kuin Flow ajaa tehtäviä jollekin repositoriolle, projekti rekisteröidään
keskukselle. Ohjattu toiminto kysyy tarvittavat tiedot:

```sh
flowctl init
```

Se ehdottaa oletuksia nykyisen hakemiston Git-etäosoitteen perusteella, joten
useimmiten riittää painaa Enteriä. Kysyttävät kentät:

| Kenttä | Selitys | Oletus |
|---|---|---|
| `name` | Projektin nimi (uniikki) | — |
| `owner/repo` | GitHub-repositorio muodossa `omistaja/repo` | tunnistetaan Git-etäosoitteesta |
| `base_branch` | Päähaara, johon PR:t kohdistuvat | `main` |
| `labels` | Merkinnät, jotka laukaisevat ajon | `auto-run` |
| `claude_timeout_seconds` | Agentin aikaraja sekunteina (0 = palvelimen oletus) | `0` |
| etäosoitteet | Lisäetäosoitteet (esim. `upstream=Silon-Oy/flow@develop`) | yksi `origin` |
| salaisuusviittaukset | `ALIAS=avain` — **viittauksia, ei arvoja** | — |

> **Salaisuudet ovat viittauksia, ei arvoja.** Et koskaan syötä tähän raakaa
> pääsytunnusta. Ohjattu toiminto kieltäytyy, jos arvo näyttää oikealta
> tunnukselta (esim. alkaa `ghp_`). Tämä on tietoinen turvaperiaate.

Automatisointia varten samat tiedot voi antaa tiedostosta:

```sh
flowctl init --config flow.yaml
```

`flow.yaml` kuvaa kentät yksi yhteen. Esimerkki:

```yaml
name: oma-projekti
owner_repo: Silon-Oy/oma-projekti
base_branch: main
labels:
  - auto-run
remotes:
  - remote: origin
    owner_repo: Silon-Oy/oma-projekti
```

Kelpoisuustarkistus tehdään keskuksessa, ei vain komentorivityökalussa.

### Tehtävän ajaminen

1. Avaa tai valitse GitHub-tehtävä repositoriossasi. Kirjoita selkeä tehtävänanto
   — agentti lukee tehtävän otsikon, kuvauksen ja kommentit.
2. Lisää tehtävälle **`auto-run`-merkintä**.
3. Siinä kaikki. Keskuksen tarkkailija huomaa tehtävän seuraavalla
   tarkistuskierroksella (oletus 60 s), lisää sen jonoon, ja vapaa suorituskone
   ottaa sen työn alle.

Mitä agentti tekee:

- **Päättää, onko tehtävä valmis ajettavaksi.** Jos tehtävänanto on epäselvä tai
  ristiriidassa koodin kanssa, agentti **kirjoittaa tarkentavan kysymyksen tehtävän
  kommenttiin** eikä arvaa. Tila on silloin `awaiting_clarification` — vastaa
  kommenttiin, niin ajo jatkuu.
- Asentaa riippuvuudet, kirjoittaa muutoksen, tekee itsearvioinnin ja korjaa
  puutteet.
- Työntää muutoksen omaan haaraan (esim. `auto-run/issue-42`) ja avaa
  muutosehdotuksen (PR).

> **Vinkki:** älä lisää `auto-run`-merkintää keskeneräiseen tai epäselvään
> tehtävään. Mitä tarkempi tehtävänanto, sitä parempi tulos.

### Etenemisen seuranta

**Komentoriviltä:**

```sh
flowctl status
```

Tuloste listaa suorituskoneet (tila, kapasiteetti, viimeinen elossapitoviesti) ja
ajot (tehtävänumero, etäosoite, tila, vaihe, haara, PR-linkki). Voit rajata tilan
mukaan:

```sh
flowctl status --status awaiting_clarification
```

Ajon tilat: `initialized`, `completed`, `blocked`, `lost_race`, `cancelled`,
`merged`, `pr_conflicted`, `timed_out`, `awaiting_clarification`.

**Selaimessa:** avaa hallintanäkymä (`http://<palvelin>:8090`). Se näyttää
käynnissä olevat ajot, niiden vaiheet ja lokit. Näkymä on tarkoituksella vain
luettava ja näyttää metatiedot ja lokit — ei agentin täysiä kehotteita tai
muutoksia.

---

## Turvallisuusmalli lyhyesti

Flow ajaa koodausagenttia, joka muokkaa koodia automaattisesti. Eristys on
suunniteltu niin, ettei agentti voi vahingoittaa muuta järjestelmää eikä vuotaa
pääsytunnuksia:

- **Raaka pääsytunnus ei koskaan päädy konttiin.** Tehtävän tiedot haetaan
  GitHubista luotetulla isäntäkoneella ennen kontin käynnistystä. Itse kontti ei
  saa GitHub-tunnusta — koodimuutos työnnetään ulosmenosuodattimen kautta, joka
  hoitaa tunnistautumisen. Mitä kontissa ei ole, sitä haittakoodi ei voi varastaa.
- **Kontti on kovennettu.** Jokainen ajo on oma kertakäyttöinen kontti, jossa on
  pudotetut oikeudet (`--cap-drop=ALL`), vain luku -tiedostojärjestelmä,
  ei-root-käyttäjä, muisti- ja prosessirajat, eikä pääsyä Docker-hallintaan. Ainoa
  isäntäkoneen liitos on kyseisen ajon työhakemisto.
- **Ulosmenoliikenne on rajattu.** Kontista pääsee verkkoon vain
  ulosmenosuodattimen (egress-proxy) kautta, joka sallii vain ennalta määritetyt
  osoitteet ja kirjaa liikenteen.
- **Claude-tunnistautuminen on konekohtainen.** Agentin laskutus menee koneen oman
  Claude-tilauksen kautta; keskus ei näe Claude-tunnusta eikä laskuta.

Tarkat säännöt ja perustelut: [`docs/flow-arkkitehtuuri.md`](docs/flow-arkkitehtuuri.md) §11.

---

## Paikallinen kehitys

### Kääntäminen ja testit

```sh
go build ./...
go test ./...
```

Tietokannasta riippuvat testit ohitetaan, ellei `FLOW_TEST_DSN`-muuttujaa ole
asetettu. Aja ne kertakäyttöistä Postgresia vasten:

```sh
docker run -d --name flow-test-pg \
  -e POSTGRES_USER=flow -e POSTGRES_PASSWORD=flow -e POSTGRES_DB=flow \
  -p 55432:5432 postgres:16

FLOW_TEST_DSN="postgres://flow:flow@localhost:55432/flow?sslmode=disable" go test ./...
```

### Ajo ilman konttia (kehityskone)

Kehityskoneella, jossa ei ole Dockeria, suorituskone voi ajaa orkestroinnin
**samassa prosessissa** (in-process). Tämä on kätevä kokeiluun, mutta **eristysmalli
ei ole silloin voimassa** — käytä vain luotetussa ympäristössä.

Aseta suorituskoneen ympäristöön `FLOW_RUNNER_MODE=inproc`. Compose-pinossa tila
on oletuksena `container` (kovennettu kontti per ajo).

### Tietokantamigraatiot

Skeemaa hallitaan golang-migratella upotetulla ajurilla. Uudet muutokset lisätään
numeroituna `up/down`-parina hakemistoon `migrations/` — jo sovellettua
migraatiota ei muokata. Koko skeema lähtee tiedostosta `000001_init.up.sql`.

### Repo-rakenne

```
cmd/{flowctl,flow-runner,flowd}/   binäärien käynnistyspisteet
internal/                          jaettu logiikka
internal/prompts/files/            agentin kehotteet (upotettu binääriin)
prompts/                           samat kehotteet ihmisluettavana viitteenä
migrations/                        Postgres-skeema
deploy/                            docker-compose + Dockerfilet
dashboard/                         hallintanäkymän web-toteutus
docs/                              arkkitehtuuri ja kaaviot
```

---

## Vianetsintä

| Oire | Todennäköinen syy / korjaus |
|---|---|
| `flowctl status` ei näytä suorituskoneita | Tarkista, että `flow-runner` on käynnissä (`docker compose ps`) ja että se on rekisteröitynyt (`docker compose logs flow-runner`). |
| `flowctl login` palauttaa 503 | `FLOW_GITHUB_OAUTH_CLIENT_ID` puuttuu keskuksen asetuksista. Ks. käyttöönotto kohta 3(b). |
| Tehtävä ei lähde käyntiin merkinnän lisäämisen jälkeen | Onko `auto-run`-merkintä juuri kyseinen, jonka projekti odottaa? Onko keskuksella lukuoikeus (`FLOW_GITHUB_TOKEN`)? Tarkistus tehdään tarkkailuvälein (oletus 60 s). |
| Ajo jää tilaan `awaiting_clarification` | Agentti tarvitsee lisätietoa. Vastaa tehtävän kommenttiin esitettyyn kysymykseen. |
| `flow-runner: FLOW_RUNNER_MODE=container but docker not in PATH` | Kontti-tila vaatii Dockerin. Kehityskoneella käytä `FLOW_RUNNER_MODE=inproc` tai asenna Docker. |
| Keskuksen lokit valittavat tietokantayhteydestä | Onko `POSTGRES_PASSWORD` asetettu `deploy/.env`:iin? Onko `postgres`-palvelu terve (`docker compose ps`)? |

Tarkkojen asetusten merkitykset löytyvät `.env.example`-tiedoston kommenteista, ja
arkkitehtuurin yksityiskohdat dokumentista
[`docs/flow-arkkitehtuuri.md`](docs/flow-arkkitehtuuri.md).
