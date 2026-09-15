// Package domain definiert die einheitlichen Typen, auf die sich
// Plugins, Core und Adapter einigen (Ports & Adapters).
package domain

import "context"

// Param beschreibt einen einzelnen Funktionsparameter.
// Daraus generieren CLI (Flags), REST (Query/JSON) und MCP (Tool-Schema)
// automatisch ihre Schnittstellen.
type Param struct {
	Name        string
	Description string
	Required    bool
	Default     string
}

// Function ist eine einzelne Plugin-Funktion.
// Beispiel: lernplan-bayern.search() wird automatisch zu:
//   - CLI:  tool lernplan search
//   - REST: GET /api/lernplan/search
//   - MCP:  lernplan_search
type Function struct {
	// PluginID verweist auf das owning Plugin (z.B. "lernplan-bayern").
	PluginID string
	// Name ist der Funktionsname innerhalb des Plugins (z.B. "search").
	Name string
	// Description für Hilfe / OpenAPI / MCP-Tool-Beschreibung.
	Description string
	// Params für automatische CLI-/REST-/MCP-Generierung.
	Params []Param
	// Handler enthält die eigentliche Business-Logik (lebt im Plugin).
	Handler func(ctx context.Context, args map[string]string) (any, error)
}

// FullName liefert den globalen Funktionsnamen: "<plugin>.<funktion>".
func (f Function) FullName() string { return f.PluginID + "." + f.Name }

// MCPName liefert den MCP-Tool-Namen: "<plugin>_<funktion>" mit "-" -> "_".
func (f Function) MCPName() string {
	out := ""
	for _, r := range f.PluginID + "_" + f.Name {
		if r == '-' {
			out += "_"
		} else {
			out += string(r)
		}
	}
	return out
}

// Result ist das einheitliche Rückgabeformat aller Plugin-Funktionen.
type Result struct {
	Plugin   string `json:"plugin"`
	Function string `json:"function"`
	Data     any    `json:"data"`
}

// Plugin kapselt vollständig: Auth, Sessions, API-Zugriff,
// Datenmodelle, Business-Logik und Mapping auf Result.
type Plugin interface {
	// ID ist der technische Name (z.B. "mebis"), zugleich CLI-/REST-/MCP-Präfix.
	ID() string
	// Name ist der Anzeigename.
	Name() string
	// Description beschreibt den Dienst.
	Description() string
	// Functions listet alle exponierten Funktionen des Plugins.
	Functions() []Function
	// Authenticate baut eine Session für das Plugin auf (kann Stub sein).
	Authenticate(ctx context.Context, credentials map[string]string) error
}
