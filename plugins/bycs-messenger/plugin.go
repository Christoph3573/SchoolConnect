// Package bycsmessenger kapselt den ByCS Messenger (Chats, Nachrichten).
package bycsmessenger

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

func (p *Plugin) ID() string          { return "bycs-messenger" }
func (p *Plugin) Name() string        { return "ByCS Messenger" }
func (p *Plugin) Description() string { return "Chats und Nachrichten aus dem ByCS Messenger." }

// AuthParams: derzeit kein Login nötig (Stub).
func (p *Plugin) AuthParams() []domain.Param { return nil }

func (p *Plugin) Authenticate(_ context.Context, credentials map[string]string) error {
	// TODO: Matrix-/Messenger-Login, Token cachen.
	if t, ok := credentials["token"]; ok {
		p.token = t
	}
	return nil
}

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name: "chats", Description: "Chats auflisten.",
			Handler: func(_ context.Context, _ map[string]string) (any, error) {
				return []any{map[string]any{"id": "klasse-10a", "titel": "Klasse 10a"}}, nil
			},
		},
		{
			Name: "send", Description: "Nachricht in einen Chat senden.",
			Params: []domain.Param{
				{Name: "chat", Description: "Chat-ID.", Required: true},
				{Name: "text", Description: "Nachrichtentext.", Required: true},
			},
			Handler: func(_ context.Context, args map[string]string) (any, error) {
				// TODO: POST Nachricht, Mapping.
				return map[string]any{"chat": args["chat"], "sent": true}, nil
			},
		},
	}
}
