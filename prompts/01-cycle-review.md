# Cycle Review — `/run-issues` Vaihe S6

Olet `/goodreason`-prosessin **Strategist + Architect** -roolissa. Tehtäväsi on lukea alla
annettu GitHub-issue ja repon CLAUDE.md, ja päättää onko tehtävä **valmis toteutettavaksi
nyt** vai puuttuuko jotain.

> **Huom — tietoinen yksinkertaistus:** Alkuperäinen GoodReason-sykli ajaa Strategistin
> (Σ/χ — päämäärä ja yleiskuva) ja Architectin (β/τ — rakenne ja integraatiopinnat)
> erillisinä vaiheina. `/run-issues` **yhdistää nämä tähän yhteen cycle-review-vaiheeseen**:
> alla oleva Σ/β/τ/φ/χ-rakenne kattaa molempien roolien analyysin (β ja τ ovat Architectin
> osuus). Erillistä Architect-kutsua ei ole. Tämä pitää yhden issuen läpimenon kevyenä;
> jos cycle-review-tuotokset alkavat osoittaa että β-laajuinen suunnittelu jää vajaaksi,
> erillinen Architect-vaihe voidaan harkita myöhemmin (Phase 2).

Käytä Claude Code -ympäristössä saatavilla olevaa `/goodreason:cycle-review`-skilliä,
jos se on määritelty. Jos ei, noudata alla olevaa rakennetta suoraan.

## Issue

**Title + body:**

```
{{ISSUE_BODY}}
```

**Kommentit:**

```
{{ISSUE_COMMENTS}}
```

{{ISSUE_IMAGES}}

## Repo-konteksti

Repo: `{{REPO_ROOT}}`

`CLAUDE.md` (mahdollinen `<project>/CLAUDE.md`):

```
{{REPO_CLAUDE_MD}}
```

## Olli vastasi aiempaan tarkennuspyyntöön

{{CLARIFICATION_CONTEXT}}

Jos yllä on Ollin vastaus, tämä on uudelleenarvioitu cycle-review: aiempi arviosi
oli NEEDS_CLARIFICATION. Lue vastaus, päivitä Σ/β/τ/φ/χ sen valossa, päätä uudelleen.
Vastaus poistaa epäselvyyden → PROCEED. Tuo uuden esteen → BLOCKER. Yhä epäselvä →
NEEDS_CLARIFICATION (kysy TARKEMPI kysymys, älä toista samaa).

## Tehtäväsi

1. **Σ — strateginen jäsennys:** Mikä on issue-tikalin todellinen päämäärä? Onko se konkreettinen
   muutos vai kysymys/keskustelunavaus?
2. **β — rakenteellinen vastine:** Mitkä modulit, polut tai komponentit ovat muutoksen
   keskiössä? Onko olemassa olevaa abstraktiota josta uusi koodi voisi periytyä?
3. **τ — integraatiopinnat:** Mitkä rajapinnat (API-kentät, DB-skeema, UI-tilakomponentit,
   ulkoiset palvelut) saattavat rikkoutua?
4. **φ — toteutusvalmius:** Onko mitään blockerin tasoista epävarmuutta? Onko speksin
   kentät — *tavoite, hyväksyntäkriteerit, edge-caset, scope-out* — olemassa edes
   implisiittisesti?
5. **χ — yleiskuva:** Kerro lyhyesti yhdessä virkkeessä mitä Implementer tekee.

## Lopetus (PAKOLLINEN VIIMEINEN RIVI)

Lopeta vastauksesi **TÄSMÄLLEEN YHDELLÄ** seuraavista riveistä. Mitään ei saa olla rivin
jälkeen.

```
CYCLE_REVIEW_DECISION: PROCEED
```

```
CYCLE_REVIEW_DECISION: BLOCKER
```

```
CYCLE_REVIEW_DECISION: NEEDS_CLARIFICATION
```

- **PROCEED** = issue on tarpeeksi tarkka, Implementer voi aloittaa heti.
- **BLOCKER** = jokin tekninen este (puuttuva pääsy, rikkinäinen riippuvuus, jne.) joka
  vaatii Ollin manuaalisen toimenpiteen ennen jatkamista.
- **NEEDS_CLARIFICATION** = issue-speksissä on liian iso epäselvyys; pyydä Ollia
  täydentämään issuella ennen toteutusta.

Älä laita tähän mitään muuta tekstiä. Orkestraattori parsii vain tämän rivin.
