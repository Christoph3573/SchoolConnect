# Beitragen — neues Plugin hinzufügen 🧩

Danke, dass du SchoolConnect erweitern willst! Das Prinzip ist absichtlich simpel:

> **Ein neues Plugin = ein neuer Ordner unter `plugins/` + eine `Register()`-Zeile + ein Pull Request. Sonst nichts.**

Der Core (`internal/`) wird **nicht** angefasst. CLI, REST und MCP bekommen deine Funktionen automatisch — du musst keinen Adapter ändern.

## Was du anfassen darfst — und was nicht

**Erlaubt (das ist dein PR):**

| Datei | Was |
|---|---|
| `plugins/<deine-id>/plugin.go` (+ ggf. weitere Dateien im selben Ordner) | Deine komplette Plattformlogik: Auth, API-Zugriff, Modelle, Handler |
| `cmd/schoolconnect/main.go` — **genau 1 Zeile** | `rt.Register(<deinpaket>.New(hc))` |
| `README.md` — **1 Zeile** | Eintrag in der Plugin-Tabelle |
| Diese `CONTRIBUTING.md` | Nur wenn die Anleitung selbst falsch/unvollständig ist |

**Verboten (führt zur Ablehnung des PRs):**

- ❌ `internal/domain`, `internal/app`, `internal/core`, `internal/adapters` ändern
- ❌ `switch`/`if plugin == ...` in Adaptern oder der Runtime einbauen
- ❌ Neue Dependencies in `go.mod` (das Projekt ist **stdlib-only** — bei Bedarf vorher ein Issue aufmachen)
- ❌ Secrets, Tokens, Passwörter oder HAR-Files mit echten Daten committen
- ❌ Bestehende Plugin-IDs oder Funktionsnamen umbenennen (bricht CLI/REST/MCP)

## Voraussetzungen

- Go ≥ 1.26, keine weiteren Tools nötig
- Fork des Repos, Branch pro Plugin: `plugin/<deine-id>` (z.B. `plugin/itslearning`)

## Schritt für Schritt

### 1. Ordner anlegen

```
plugins/<deine-id>/plugin.go
```

Regeln für `<deine-id>`: kleingeschrieben, Wörter mit `-` trennen (z.B. `mein-portal`, nicht `MeinPortal` oder `mein_portal`). Die ID wird automatisch CLI-/REST-Präfix und (mit `-` → `_`) MCP-Tool-Präfix.

### 2. Vorlage wählen

- **Ohne Login** (öffentliche API/Website): kopiere die Struktur aus `plugins/lernplan-bayern/plugin.go`
- **Mit Login**: kopiere die Struktur aus `plugins/schuelerportal/plugin.go`

### 3. Minimales Plugin-Gerüst

```go
// Package meinportal kapselt <Plattform> (<URL>).
package meinportal

import (
    "context"
    "net/http"

    coreerrors "schoolconnect/internal/core/errors"
    "schoolconnect/internal/domain"
)

type Plugin struct{ client *http.Client }

// New bekommt den geteilten Core-HTTP-Client übergeben — keinen eigenen bauen.
func New(client *http.Client) *Plugin { return &Plugin{client: client} }

func (p *Plugin) ID() string          { return "mein-portal" }
func (p *Plugin) Name() string        { return "Mein Portal" }
func (p *Plugin) Description() string { return "Was das Portal kann, in einem Satz." }

// Ohne Login: return nil. Mit Login: Credentials deklarieren (siehe Schritt 4).
func (p *Plugin) AuthParams() []domain.Param { return nil }

func (p *Plugin) Authenticate(_ context.Context, _ map[string]string) error { return nil }

func (p *Plugin) Functions() []domain.Function {
    return []domain.Function{
        {
            Name:        "search",
            Description: "Was die Funktion tut (erscheint in --help, REST-Doku und MCP).",
            Params: []domain.Param{
                {Name: "query", Description: "Wonach gesucht wird.", Required: true},
            },
            Handler: p.search,
        },
    }
}

func (p *Plugin) search(_ context.Context, args map[string]string) (any, error) {
    if args["query"] == "" {
        return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), "missing required param \"query\"")
    }
    // ... API aufrufen, Ergebnis als plain any (map/struct/slice) zurückgeben
    return map[string]any{"results": []any{}}, nil
}
```

### 4. Nur mit Login: `AuthParams()` statt Credential-Params

Datenfunktionen deklarieren **nie** `username`/`password`/Tokens. Stattdessen:

```go
func (p *Plugin) AuthParams() []domain.Param {
    return []domain.Param{
        {Name: "username", Description: "Benutzername.", Required: true, Aliases: []string{"email"}},
        {Name: "password", Description: "Passwort.", Required: true, Secret: true},
    }
}

func (p *Plugin) Authenticate(ctx context.Context, credentials map[string]string) error {
    // credentials enthält bereits Store + Env + Args gemergt.
    // Hier nur: einloggen / Session aufbauen / Fehler werfen.
}
```

Die Runtime erledigt den Rest automatisch:

- generiert `<plugin>.auth` + `<plugin>.logout` für CLI, REST und MCP
- speichert erfolgreiche Logins in `~/.config/schoolconnect/credentials.json` (0600)
- injiziert die Credentials bei jedem Call (Merge: Defaults < Store < Env < Args)
- `Param.Secret` wird nie geloggt/gelistet, `Param.Aliases` akzeptieren alle Adapter

Optional: `EnvCredentials() map[string]string` implementieren, um `<ID>_SECRET` (z.B. `MEIN_PORTAL_SECRET`, `-` → `_`) zu parsen. **Das Format parst dein Plugin selbst**, der Core bleibt generisch.

### 5. Regeln für `Params` und Handler

- Nur `Name`, `Description`, `Required`, `Default`, `Aliases`, `Secret` setzen — daraus generieren sich CLI-Flags, REST-Query/JSON und MCP-Schema von selbst.
- Handler-Signatur: `func(ctx context.Context, args map[string]string) (any, error)`.
- Rückgabe: plain `any` (`map[string]any`, Slice, Struct) — kein HTTP-/CLI-/MCP-Code im Plugin.
- Fehler: immer über `coreerrors.New` / `coreerrors.Wrap` mit passendem Code:
  `CodeBadRequest` (falsche Params), `CodeUnauthorized` (Login nötig/falsch),
  `CodeNotFound`, `CodeUpstream` (Plattform antwortet falsch), `CodeInternal`.
- HTTP: den übergebenen `*http.Client` nutzen (Timeout steckt schon drin). Eigene Cookie-Jars/Sessions leben im Plugin (siehe `schuelerportal`).
- Keine Secrets im Code — Env-Namen via `<ID>_SECRET` ableiten.

### 6. Registrieren (die eine Zeile)

In `cmd/schoolconnect/main.go`:

```go
rt.Register(meinportal.New(hc))
```

Sonst **nichts** in `cmd/` oder `internal/` ändern.

### 7. README ergänzen (eine Zeile)

In der Plugin-Tabelle unter `## Plugins`:

```markdown
| Mein Portal | `mein-portal` | ✅ live (Login: `auth`) | `auth`, `logout`, `search` |
```

### 8. Lokal verifizieren

```bash
go vet ./...
go build -o /tmp/schoolconnect ./cmd/schoolconnect
/tmp/schoolconnect list
/tmp/schoolconnect tool mein-portal search --query test
```

`list` muss deine Funktionen zeigen, der `tool`-Call muss ohne Core-Änderung funktionieren. Wenn dein Plugin einen Login hat, zusätzlich testen:

```bash
/tmp/schoolconnect auth mein-portal --username ...   # Passwort wird abgefragt
/tmp/schoolconnect tool mein-portal search --query test
/tmp/schoolconnect logout mein-portal
```

## Pull Request stellen

1. Pushe deinen Branch `plugin/<deine-id>` in deinen Fork.
2. Öffne einen PR gegen `main` — **ein Plugin pro PR**, damit Review und Revert sauber bleiben.
3. Beschreibe im PR kurz:
   - Welche Plattform + welche Funktionen (mit Beispiel-Calls wie oben)
   - Mit oder ohne Login
   - Wie getestet (`go vet`, `build`, `list`, Beispiel-Output — Secrets schwärzen!)
   - Bestätigung: `internal/` unverändert, stdlib-only, keine Secrets committet

Checkliste vor dem Absenden:

- [ ] Nur `plugins/<id>/` + 1 Zeile `Register()` + 1 Zeile README geändert
- [ ] `go vet ./...` grün, Build läuft, `list` zeigt das Plugin
- [ ] Keine Credential-Params an Datenfunktionen, `Secret: true` bei Passwörtern/Tokens
- [ ] Fehler über `coreerrors` mit passendem Code
- [ ] Keine Secrets, HARs mit Echtdaten oder private URLs im Diff (`git status` + `git diff` prüfen)

## Was im Review passiert

Maintainer prüfen: Plugin-Grenzen eingehalten? Keine Core-Änderung? Params sauber deklariert? Auth-Flow wie in `schuelerportal`? Bei Feedback einfach im selben PR nachbessern — kein neuer PR nötig.

Noch Fragen? Schau dir `plugins/lernplan-bayern/plugin.go` (einfach, ohne Login) und `plugins/schuelerportal/plugin.go` (mit Login) an — wenn du deren Muster folgst, passt dein PR fast sicher. Viel Spaß! 🚀
