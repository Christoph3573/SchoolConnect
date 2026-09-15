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
| Schülerportal | `schuelerportal` | Stub | `profile`, `grades` |
| mebis | `mebis` | Stub | `courses`, `tasks` |
| ByCS Drive | `bycs-drive` | Stub | `list`, `search` |
| ByCS Messenger | `bycs-messenger` | Stub | `chats`, `send` |

Secrets per Plugin via Env: `<ID>_SECRET` (`-`→`_`), siehe `config.SecretFor(pluginID)`. Keine Secrets im Code.

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

1. Vorlage kopieren (Referenz: `plugins/lernplan-bayern/plugin.go`).
2. `ID()`, `Name()`, `Description()`, `Functions()`, `Authenticate()` implementieren.
3. Nur `Params` mit `Name/Description/Required/Default` deklarieren — Adapter generieren sich daraus.
4. Handler gibt plain `any` zurück, Fehler als `error` (Core mappt auf Codes/HTTP-Status).
5. Eine Zeile in `cmd/schoolconnect/main.go`: `rt.Register(<neu>.New(hc))`. Sonst nichts ändern.

Weitere Details für Agenten: siehe `AGENTS.md`.
