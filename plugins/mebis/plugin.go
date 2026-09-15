// Package mebis kapselt mebis (Kurse, Aufgaben, Materialien).
package mebis

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

func (p *Plugin) ID() string          { return "mebis" }
func (p *Plugin) Name() string        { return "mebis" }
func (p *Plugin) Description() string { return "Kurse, Aufgaben und Lernmaterialien aus mebis." }

func (p *Plugin) Authenticate(_ context.Context, credentials map[string]string) error {
	// TODO: mebis-Login (SSO), Token cachen.
	if t, ok := credentials["token"]; ok {
		p.token = t
	}
	return nil
}

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name: "courses", Description: "Belegte Kurse auflisten.",
			Handler: func(_ context.Context, _ map[string]string) (any, error) {
				// TODO: mebis-API, Mapping auf Einheitsformat.
				return []any{map[string]any{"id": "m10-1", "titel": "Mathematik 10"}}, nil
			},
		},
		{
			Name: "tasks", Description: "Aufgaben eines Kurses laden.",
			Params: []domain.Param{{Name: "course", Description: "Kurs-ID.", Required: true}},
			Handler: func(_ context.Context, args map[string]string) (any, error) {
				return []any{map[string]any{"course": args["course"], "titel": "Übungsblatt 3"}}, nil
			},
		},
	}
}
