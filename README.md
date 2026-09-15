# SchoolConnect
A unified interface for accessing and interacting with digital school services.

Einheitliche Go-Schnittstelle zu bayerischen Schulplattformen. Jede Plattform ist ein Plugin. CLI, REST und MCP greifen auf dieselbe Plugin Runtime zu.

## Architektur (Ports & Adapters, stdlib-only)

```text
CLI ─┐
REST ├──→ Runtime (internal/app) ──→ plugins/* (Handler)
MCP ─┘
```

- `internal/domain`: Ports — `Plugin`, `Function`, `Param`, `Result`. Einheitliche Typen, keine Logik.
- `internal/app`: `Runtime` — `Register()`, `Call(plugin, fn, args)`, `CallMCP(toolName, args)`. Einziger Dispatch-Punkt.
- `internal/core`: geteilte Infra — `config`, `logging` (slog), `errors` (Codes), `session`, `httpclient`. Keine Plattformlogik.
- `internal/adapters`: `cli`, `rest`, `mcp`. Generisch: rendern alle registrierten Funktionen, enthalten keine Plugin-Details.
- `plugins/<id>`: kapselt vollständig Auth, Sessions, API-Zugriff, Datenmodelle, Business-Logik, Mapping auf `domain.Result`.

**Kernregel: Plattformlogik → Plugin. Infra → Core. Adapter nie pro Plugin anfassen.**

Eine `domain.Function` wird automatisch zu allen drei Schnittstellen:

| Definition | CLI | REST | MCP |
|---|---|---|---|
| `<plugin>.<fn>` + `Params` | `tool <plugin> <fn> [--p v]` | `GET\|POST /api/<plugin>/<fn>` | Tool `<plugin>_<fn>` (`-`→`_`), `required` aus `Param.Required` |

MCP-Namen immer exakt über `Runtime.CallMCP()` auflösen (String-Split ist wegen `-`/`_` mehrdeutig).

## Schnellstart

Voraussetzung: Go ≥ 1.26, keine weiteren Dependencies (stdlib-only).

```bash
go vet ./...
go build -o /tmp/schoolconnect ./cmd/schoolconnect
/tmp/schoolconnect list
```

## Schnittstellen

Alle drei greifen auf dieselbe Runtime zu — eine `domain.Function` ist automatisch überall verfügbar.

```bash
# CLI: alle Funktionen auflisten
/tmp/schoolconnect list

# CLI: Funktion aufrufen
/tmp/schoolconnect tool lernplan-bayern search --schulart Gymnasium --lehrplankapitel kap4 --fach Deutsch --jahrgangsstufe 9

# REST-API starten (:8080, via REST_ADDR änderbar)
#   GET|POST /api/<plugin>/<funktion>?param=...
/tmp/schoolconnect serve

# MCP-Server via stdio (JSON-RPC, für Claude Desktop / MCP-Clients)
/tmp/schoolconnect mcp
```

REST-Beispiel:

```bash
curl 'http://localhost:8080/api/lernplan-bayern/search?schulart=Gymnasium&lehrplankapitel=kap4&fach=Deutsch&jahrgangsstufe=9'
```

MCP-Beispiel (`tools/call`):

```json
{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
 "params": {"name": "lernplan_bayern_search",
  "arguments": {"schulart": "Gymnasium", "lehrplankapitel": "kap4", "fach": "Deutsch", "jahrgangsstufe": "9"}}}
```

## Plugins

| Plugin | ID | Status | Funktionen |
|---|---|---|---|
| Lernplan Bayern (LehrplanPLUS) | `lernplan-bayern` | ✅ live (echter Web-Zugriff) | `search`, `details` |
| Schülerportal | `schuelerportal` | ✅ live (Login: `auth`) | `auth`, `logout`, `profil`, `stundenplan`, `hausaufgaben`, `vertretungsplan` |
| mebis | `mebis` | Stub | `courses`, `tasks` |
| ByCS Drive | `bycs-drive` | Stub | `list`, `search` |
| ByCS Messenger | `bycs-messenger` | Stub | `chats`, `send` |

## Auth (einmal anmelden, überall angemeldet)

Plugins mit Login deklarieren `AuthParams()` (z.B. `schule`/`username`/`password`);
die Runtime generiert daraus automatisch `<plugin>.auth` + `<plugin>.logout` —
identisch in CLI, REST und MCP. Erfolgreiche Logins landen in
`~/.config/schoolconnect/credentials.json` (0600, via `SCHOOLCONNECT_CONFIG_DIR`
umleitbar) und werden bei jedem Call automatisch injiziert. Datenfunktionen
brauchen daher keine Credential-Params. Quelle der Credentials, aufsteigend:
Param-Defaults < Store < Env (`SCHUELERPORTAL_SECRET`, Format parst das Plugin)
< explizite Parameter.

```bash
# Interaktiv (fehlende Pflichtwerte werden abgefragt, Passwort ohne Echo):
/tmp/schoolconnect auth schuelerportal
# oder direkt:
/tmp/schoolconnect auth schuelerportal --schule indomagy --username ich@mail --password geheim
# prüfen (Antwort enthält nur Key-Namen, nie Secret-Werte):
/tmp/schoolconnect tool schuelerportal profil
# abmelden:
/tmp/schoolconnect logout schuelerportal        # = tool schuelerportal logout
```

REST und MCP nutzen denselben Flow ohne Adapter-Sondercode:

```bash
curl -X POST localhost:8080/api/schuelerportal/auth \
  -H 'Content-Type: application/json' \
  -d '{"schule":"indomagy","username":"ich@mail","password":"geheim"}'
curl 'localhost:8080/api/schuelerportal/stundenplan?tag=mo'
```

```json
{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
 "params": {"name": "schuelerportal_auth",
  "arguments": {"schule": "indomagy", "username": "ich@mail", "password": "geheim"}}}
```

### schuelerportal: Schülerportal-Zugriff

Echter Zugriff auf `https://api.schueler.schule-infoportal.de/<schule>/…`
(Laravel-Sanctum-XSRF-Flow, aus HAR-Mitschnitt rekonstruiert). `schule` ist das
Schulkürzel im API-Pfad. Nach `auth` genügen:

```bash
/tmp/schoolconnect tool schuelerportal stundenplan --tag mo
/tmp/schoolconnect tool schuelerportal hausaufgaben
/tmp/schoolconnect tool schuelerportal vertretungsplan --datum 2026-09-15
```

### lernplan-bayern: LehrplanPLUS-Suche

Echter Zugriff auf `https://www.lehrplanplus.bayern.de` (öffentlich, kein Login). `search` löst Klarnamen oder IDs auf, ruft `GET /suche/kapitel` auf und parst die Trefferliste.

Alle 4 Params sind Pflicht:

```bash
/tmp/schoolconnect tool lernplan-bayern search \
  --schulart Gymnasium --lehrplankapitel kap4 --fach Deutsch --jahrgangsstufe 9
# → {"count": 1, "kap": "kap4", "url": "/fachlehrplan/gymnasium/9/deutsch", ...}
```

| Param | Werte (Name oder ID) |
|---|---|
| `schulart` | `Gymnasium` (24431), `Realschule` (24430), `Grundschule` (24427), `Mittelschule` (24428), `Förderschule` (24429), `Wirtschaftsschule` (24432), `Fachoberschule` (43644), `Berufsoberschule` (43645) |
| `lehrplankapitel` | `kap4` Fachlehrpläne, `kap2` Fachprofile, `kap3` Jahrgangsstufenprofile, `kap0`/`kap1`/`kap2fuez` |
| `fach` | `Deutsch` (23885), `Mathematik` (23880), … (139 Fächer, Name oder ID) |
| `jahrgangsstufe` | `1`–`13` (Name oder ID, z.B. `9`) |

Kapitel-Filterlogik (aus `data-usable` in `/wizard`): `kap4` nutzt alle Filter, `kap2` ignoriert die Jahrgangsstufe, `kap3` ignoriert das Fach.

`details` lädt eine Ergebnis-URL und gibt die Seite strukturiert zurück — Inhaltsverzeichnis plus Abschnitte mit Kompetenzerwartungen:

```bash
/tmp/schoolconnect tool lernplan-bayern details \
  --url /fachlehrplan/gymnasium/9/deutsch --abschnitt "1.1"
# → {title, inhaltsverzeichnis[], abschnitte[{code, titel, uebergeordnet, text, kompetenzen[]}], ...}
```

`--abschnitt` filtert optional nach Code (`1.1`) oder Stichwort (`Sprechen`).

## Neues Plugin / neue Funktion

1. Vorlage kopieren (Referenz: `plugins/lernplan-bayern/plugin.go`; mit Login: `plugins/schuelerportal/plugin.go`).
2. `ID()`, `Name()`, `Description()`, `Functions()`, `Authenticate()` implementieren — mit Login zusätzlich `AuthParams()` + optional `EnvCredentials()` (Format parst das Plugin; Core bleibt generisch).
3. Nur `Params` mit `Name/Description/Required/Default/Aliases/Secret` deklarieren — Adapter generieren sich daraus. Credential-Params gehören nur in `AuthParams()`, nie an Datenfunktionen.
4. Handler gibt plain `any` zurück, Fehler als `error` (Core mappt auf Codes/HTTP-Status).
5. Eine Zeile in `cmd/schoolconnect/main.go`: `rt.Register(<neu>.New(hc))`. Sonst nichts ändern.

Weitere Details für Agenten: siehe `AGENTS.md`.
