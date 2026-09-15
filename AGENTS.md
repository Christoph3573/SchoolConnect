# AGENTS.md — SchoolConnect

Einheitliche Go-Schnittstelle zu bayerischen Schulplattformen. Jede Plattform ist ein Plugin. CLI, REST und MCP greifen auf dieselbe Plugin Runtime zu.

## Architektur (Ports & Adapters, stdlib-only)

```text
CLI ─┐
REST ├──→ Runtime (internal/app) ──→ plugins/* (Handler)
MCP ─┘
```

- `internal/domain`: Ports — `Plugin`, `Function`, `Param`, `Result`, `EnvCredentialsProvider`. Einheitliche Typen, keine Logik.
- `internal/app`: `Runtime` — `Register()`, `Call(plugin, fn, args)`, `CallMCP(toolName, args)`, `Authenticate(plugin, args)`, `Logout(plugin)`. Einziger Dispatch-Punkt, besitzt den Credential-Merge und die synthetischen `auth`/`logout`-Funktionen.
- `internal/core`: geteilte Infra — `config`, `logging` (slog), `errors` (Codes), `session` (file-backed Credential-Store, 0600), `httpclient`. Keine Plattformlogik.
- `internal/adapters`: `cli`, `rest`, `mcp`. Generisch: rendern alle registrierten Funktionen, enthalten keine Plugin-Details.
- `plugins/<id>`: kapselt vollständig Auth, Sessions, API-Zugriff, Datenmodelle, Business-Logik, Mapping auf `domain.Result`.

## Kernregel

**Plattformlogik → Plugin. Infra → Core. Adapter nie pro Plugin anfassen.**

Eine `domain.Function` wird automatisch zu allen drei Schnittstellen:

| Definition | CLI | REST | MCP |
|---|---|---|---|
| `<plugin>.<fn>` + `Params` | `tool <plugin> <fn> [--p v]` | `GET\|POST /api/<plugin>/<fn>` | Tool `<plugin>_<fn>` (`-`→`_`), `required` aus `Param.Required` |

MCP-Namen immer exakt über `Runtime.CallMCP()` auflösen (String-Split ist wegen `-`/`_` mehrdeutig).

## Auth (gehört der Runtime, nicht den Funktionen)

- Plugin deklariert nur `AuthParams()` (+ optional `EnvCredentials()`); die Runtime generiert `auth`/`logout`, speichert Logins im Session-Store und injiziert Credentials bei jedem `Call` — identisch für CLI, REST, MCP.
- **Datenfunktionen deklarieren nie Credential-Params.** Neue Login-Plugins an `plugins/schuelerportal/plugin.go` orientieren.
- Merge-Reihenfolge: Defaults < Store < Env < Args. `Param.Secret` (kein Echo/Logging), `Param.Aliases` (z.B. email→username).
- CLI fragt fehlende Pflichtwerte interaktiv ab (nur mit TTY; Prompts auf stderr).

## Neues Plugin / neue Funktion

1. Vorlage kopieren (Referenz: `plugins/lernplan-bayern/plugin.go`; mit Login: `plugins/schuelerportal/plugin.go`).
2. `ID()`, `Name()`, `Description()`, `Functions()`, `Authenticate()` implementieren (+ mit Login `AuthParams()`).
3. Nur `Params` mit `Name/Description/Required/Default` deklarieren — Adapter generieren sich daraus.
4. Handler gibt plain `any` zurück, Fehler als `error` (Core mappt auf Codes/HTTP-Status).
5. Eine Zeile in `cmd/schoolconnect/main.go`: `rt.Register(<neu>.New(hc))`. Sonst nichts ändern.

## Verifizieren

```bash
go vet ./...
go build -o /tmp/schoolconnect ./cmd/schoolconnect
/tmp/schoolconnect list
/tmp/schoolconnect tool lernplan-bayern search --query Funktionen
```

## Don'ts

- Keine Plattform-Imports in `internal/`; keine Adapter-`switch`es pro Plugin.
- Keine neuen Dependencies ohne Absprache (aktuell stdlib-only).
- Keine Secrets im Code — `config.SecretFor(pluginID)` (`<ID>_SECRET`, `-`→`_`).
