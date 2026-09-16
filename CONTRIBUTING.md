# Beitragen

Ein neues Plugin besteht aus einem Ordner unter `plugins/`, einer `Register()`-Zeile und einem Pull Request. Der Core unter `internal/` wird nicht geändert. CLI, REST und MCP übernehmen neue Funktionen automatisch.

## Geänderte Dateien pro Plugin

- `plugins/<id>/plugin.go` (ggf. weitere Dateien im selben Ordner): gesamte Plattformlogik
- `cmd/schoolconnect/main.go`: eine Zeile `rt.Register(<paket>.New(hc))`
- `README.md`: eine Zeile in der Plugin-Tabelle

Nicht ändern:

- `internal/domain`, `internal/app`, `internal/core`, `internal/adapters`
- keine Plugin-`switches` in Adaptern oder Runtime
- keine neuen Dependencies in `go.mod` (stdlib-only, sonst vorher Issue aufmachen)
- keine Secrets, Tokens, Passwörter oder HAR-Files mit Echtdaten
- keine bestehenden Plugin-IDs oder Funktionsnamen umbenennen

Voraussetzungen: Go >= 1.26. Branch pro Plugin: `plugin/<id>`.

## Plugin anlegen

ID: kleingeschrieben, Wörter mit `-` trennen (z.B. `mein-portal`). Die ID wird CLI-/REST-Präfix und MCP-Präfix (`-` wird zu `_`).

Vorlage:

- ohne Login: `plugins/lernplan-bayern/plugin.go`
- mit Login: `plugins/schuelerportal/plugin.go`

Gerüst:

```go
package meinportal

import (
    "context"
    "net/http"

    coreerrors "schoolconnect/internal/core/errors"
    "schoolconnect/internal/domain"
)

type Plugin struct{ client *http.Client }

func New(client *http.Client) *Plugin { return &Plugin{client: client} }

func (p *Plugin) ID() string          { return "mein-portal" }
func (p *Plugin) Name() string        { return "Mein Portal" }
func (p *Plugin) Description() string { return "Was das Portal kann." }
func (p *Plugin) AuthParams() []domain.Param { return nil }
func (p *Plugin) Authenticate(_ context.Context, _ map[string]string) error { return nil }

func (p *Plugin) Functions() []domain.Function {
    return []domain.Function{
        {
            Name:        "search",
            Description: "Was die Funktion tut.",
            Params: []domain.Param{
                {Name: "query", Description: "Suchbegriff.", Required: true},
            },
            Handler: p.search,
        },
    }
}

func (p *Plugin) search(_ context.Context, args map[string]string) (any, error) {
    if args["query"] == "" {
        return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), "missing required param \"query\"")
    }
    return map[string]any{"results": []any{}}, nil
}
```

Mit Login: Credentials nur in `AuthParams()` deklarieren, nie an Datenfunktionen.

```go
func (p *Plugin) AuthParams() []domain.Param {
    return []domain.Param{
        {Name: "username", Description: "Benutzername.", Required: true, Aliases: []string{"email"}},
        {Name: "password", Description: "Passwort.", Required: true, Secret: true},
    }
}
```

Die Runtime generiert daraus `auth`/`logout`, speichert Logins in `~/.config/schoolconnect/credentials.json` (0600) und injiziert sie bei jedem Call (Defaults < Store < Env < Args). Optional `EnvCredentials()` implementieren für `<ID>_SECRET` (z.B. `MEIN_PORTAL_SECRET`).

Regeln:

- `Params` nur mit `Name`, `Description`, `Required`, `Default`, `Aliases`, `Secret`.
- Handler: `func(ctx context.Context, args map[string]string) (any, error)`, Rückgabe plain `any`, kein Adapter-Code im Plugin.
- Fehler über `coreerrors.New` / `Wrap` (`BadRequest`, `Unauthorized`, `NotFound`, `Upstream`, `Internal`).
- Den übergebenen `*http.Client` nutzen, Sessions/Cookie-Jars leben im Plugin.

## Verifizieren

```bash
go vet ./...
go build -o /tmp/schoolconnect ./cmd/schoolconnect
/tmp/schoolconnect list
/tmp/schoolconnect tool mein-portal search --query test
```

Mit Login zusätzlich:

```bash
/tmp/schoolconnect auth mein-portal --username ...
/tmp/schoolconnect tool mein-portal search --query test
/tmp/schoolconnect logout mein-portal
```

## Pull Request

Ein Plugin pro PR gegen `main`. PR-Beschreibung: Plattform, Funktionen mit Beispiel-Calls, mit/ohne Login, wie getestet.

Checkliste:

- [ ] nur `plugins/<id>/`, 1 Zeile `Register()`, 1 Zeile README
- [ ] `go vet` grün, `list` zeigt das Plugin
- [ ] keine Credential-Params an Datenfunktionen
- [ ] keine Secrets oder Echtdaten im Diff
