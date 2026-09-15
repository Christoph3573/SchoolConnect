// Package schuelerportal kapselt das Schülerportal:
// Auth, Sessions, API-Zugriff, Datenmodelle und Mapping auf domain.Result.
package schuelerportal

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

// New erzeugt das Plugin (Core stellt den HTTP-Client).
func New(client *http.Client) *Plugin { return &Plugin{client: client} }

func (p *Plugin) ID() string          { return "schuelerportal" }
func (p *Plugin) Name() string        { return "Schülerportal" }
func (p *Plugin) Description() string { return "Noten, Stammdaten und Stundenplan aus dem Schülerportal." }

func (p *Plugin) Authenticate(_ context.Context, credentials map[string]string) error {
	// TODO: echter Login gegen Schülerportal, Token in p.token + Session-Store.
	if t, ok := credentials["token"]; ok {
		p.token = t
	}
	return nil
}

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name: "profile", Description: "Eigenes Schülerprofil laden.",
			Handler: func(_ context.Context, _ map[string]string) (any, error) {
				// TODO: GET <schuelerportal>/api/profile, Mapping auf Einheitsformat.
				return map[string]any{"vorname": "Max", "nachname": "Mustermann", "klasse": "10a"}, nil
			},
		},
		{
			Name: "grades", Description: "Notenübersicht laden.",
			Params: []domain.Param{{Name: "fach", Description: "Fach filtern (optional)."}},
			Handler: func(_ context.Context, args map[string]string) (any, error) {
				// TODO: GET <schuelerportal>/api/grades?fach=..., Mapping.
				return []any{
					map[string]any{"fach": "Mathematik", "note": "2", "halbjahr": "1"},
				}, nil
			},
		},
	}
}
