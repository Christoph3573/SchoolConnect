// Package bycsdrive kapselt ByCS Drive (Dateien, Freigaben).
package bycsdrive

import (
	"context"
	"net/http"

	"schoolconnect/internal/domain"
)

// Plugin implementiert domain.Plugin.
type Plugin struct {
	client *http.Client
	token  string
}

// New erzeugt das Plugin.
func New(client *http.Client) *Plugin { return &Plugin{client: client} }

func (p *Plugin) ID() string          { return "bycs-drive" }
func (p *Plugin) Name() string        { return "ByCS Drive" }
func (p *Plugin) Description() string { return "Dateien und Freigaben aus ByCS Drive." }

// AuthParams: derzeit kein Login nötig (Stub).
func (p *Plugin) AuthParams() []domain.Param { return nil }

func (p *Plugin) Authenticate(_ context.Context, credentials map[string]string) error {
	// TODO: ByCS-OAuth, Token cachen.
	if t, ok := credentials["token"]; ok {
		p.token = t
	}
	return nil
}

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name: "list", Description: "Dateien in einem Ordner auflisten.",
			Params: []domain.Param{{Name: "path", Description: "Ordnerpfad.", Default: "/"}},
			Handler: func(_ context.Context, args map[string]string) (any, error) {
				return []any{map[string]any{"path": args["path"], "name": "Arbeitsblatt.pdf"}}, nil
			},
		},
		{
			Name: "search", Description: "Dateien suchen.",
			Params: []domain.Param{{Name: "query", Description: "Suchbegriff.", Required: true}},
			Handler: func(_ context.Context, args map[string]string) (any, error) {
				return []any{map[string]any{"name": "Ergebnis zu " + args["query"]}}, nil
			},
		},
	}
}
