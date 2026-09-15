// Package app ist die Application-Schicht: Plugin Runtime.
// CLI, REST und MCP greifen alle auf dieselbe Runtime zu.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"schoolconnect/internal/domain"

	coreerrors "schoolconnect/internal/core/errors"
	"schoolconnect/internal/core/session"
)

// Runtime registriert Plugins und dispatcht Funktionsaufrufe.
// Neue Plattformen = nur neues Plugin + Register() — ohne Änderung an CLI/REST/MCP.
//
// Auth gehört der Runtime, nicht den einzelnen Funktionen: Plugins
// deklarieren via AuthParams(), welche Credentials sie brauchen. Die
// Runtime generiert daraus pro Plugin automatisch "auth"/"logout"
// (in CLI/REST/MCP identisch aufrufbar), speichert erfolgreiche Logins
// im Session-Store und injiziert die Credentials bei jedem Call in die
// Handler-Args. Datenfunktionen deklarieren daher keine
// Credential-Params mehr.
type Runtime struct {
	log       *slog.Logger
	plugins   map[string]domain.Plugin
	functions map[string]domain.Function // key: "<plugin>.<funktion>"
	sessions  *session.Store
}

// New erzeugt eine Runtime mit dem Standard-Credential-Store
// (siehe session.DefaultPath).
func New(log *slog.Logger) *Runtime {
	return NewWithStore(log, session.New())
}

// NewWithStore erzeugt eine Runtime mit explizitem Store (für Tests).
func NewWithStore(log *slog.Logger, store *session.Store) *Runtime {
	if store == nil {
		store = session.New()
	}
	return &Runtime{
		log:       log,
		plugins:   map[string]domain.Plugin{},
		functions: map[string]domain.Function{},
		sessions:  store,
	}
}

// Register meldet ein Plugin (und all seine Funktionen) an.
// Für Plugins mit AuthParams() werden zusätzlich synthetische
// "<plugin>.auth" und "<plugin>.logout" registriert — außer das Plugin
// definiert sie selbst.
func (r *Runtime) Register(p domain.Plugin) {
	r.plugins[p.ID()] = p
	for _, fn := range p.Functions() {
		fn.PluginID = p.ID() // sicherstellen, dass PluginID gesetzt ist
		key := fn.FullName()
		r.functions[key] = fn
		r.log.Info("function registered", "function", key, "mcp", fn.MCPName())
	}
	if len(p.AuthParams()) == 0 {
		return
	}
	if _, exists := r.functions[p.ID()+".auth"]; !exists {
		params := append([]domain.Param(nil), p.AuthParams()...)
		fn := domain.Function{
			PluginID:    p.ID(),
			Name:        "auth",
			Description: fmt.Sprintf("Bei %s anmelden. Credentials werden gespeichert und für alle weiteren Aufrufe wiederverwendet.", p.Name()),
			Params:      params,
		}
		pluginID := p.ID()
		fn.Handler = func(ctx context.Context, args map[string]string) (any, error) {
			// Generischer Auth-Call: gespeicherte Credentials einsammeln
			// (Call hat für "auth" bewusst nicht injiziert — die Roh-Args
			// kommen hierher, merge passiert in Authenticate).
			return r.Authenticate(ctx, pluginID, args)
		}
		r.functions[fn.FullName()] = fn
		r.log.Info("function registered", "function", fn.FullName(), "mcp", fn.MCPName())
	}
	if _, exists := r.functions[p.ID()+".logout"]; !exists {
		fn := domain.Function{
			PluginID:    p.ID(),
			Name:        "logout",
			Description: fmt.Sprintf("Gespeicherte Anmeldedaten für %s verwerfen.", p.Name()),
		}
		pluginID := p.ID()
		fn.Handler = func(ctx context.Context, _ map[string]string) (any, error) {
			return r.Logout(ctx, pluginID)
		}
		r.functions[fn.FullName()] = fn
		r.log.Info("function registered", "function", fn.FullName(), "mcp", fn.MCPName())
	}
}

// Plugins listet alle registrierten Plugins.
func (r *Runtime) Plugins() []domain.Plugin {
	out := make([]domain.Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, p)
	}
	return out
}

// Functions listet alle registrierten Funktionen (inkl. synthetischer
// auth/logout-Funktionen).
func (r *Runtime) Functions() []domain.Function {
	out := make([]domain.Function, 0, len(r.functions))
	for _, f := range r.functions {
		out = append(out, f)
	}
	return out
}

// AuthParamsFor liefert die deklarierten Auth-Parameter eines Plugins.
func (r *Runtime) AuthParamsFor(pluginID string) ([]domain.Param, error) {
	p, ok := r.plugins[pluginID]
	if !ok {
		return nil, coreerrors.New(coreerrors.CodeNotFound, pluginID,
			fmt.Sprintf("unknown plugin %q", pluginID))
	}
	return append([]domain.Param(nil), p.AuthParams()...), nil
}

// NeedsAuth meldet, ob ein Plugin einen Login verlangt.
func (r *Runtime) NeedsAuth(pluginID string) bool {
	p, ok := r.plugins[pluginID]
	return ok && len(p.AuthParams()) > 0
}

// StoredKeys listet (ohne Werte), welche Credential-Keys für ein Plugin
// gespeichert sind. Für Secret-Params werden nie Werte herausgegeben.
func (r *Runtime) StoredKeys(pluginID string) []string {
	m, ok := r.sessions.Get(pluginID)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PreviewMerge führt Defaults < Store < Env < Args für ein Plugin zusammen
// (ohne Secret-Werte preiszugeben — die CLI nutzt das nur für
// Leere/Nicht-Leere, nie zur Anzeige). Damit fragt die CLI nur ab,
// was wirklich noch fehlt.
func (r *Runtime) PreviewMerge(pluginID string, args map[string]string) (map[string]string, error) {
	p, ok := r.plugins[pluginID]
	if !ok {
		return nil, coreerrors.New(coreerrors.CodeNotFound, pluginID,
			fmt.Sprintf("unknown plugin %q", pluginID))
	}
	return r.mergeCreds(pluginID, p.AuthParams(), args), nil
}

// Authenticate meldet ein Plugin an: gespeicherte Credentials werden mit
// args zusammengeführt (args gewinnen, Defaults aus AuthParams greifen),
// das Plugin-Authenticate wird aufgerufen und bei Erfolg der Stand
// gespeichert. Das Ergebnis enthält nie Secret-Werte, nur Key-Namen.
func (r *Runtime) Authenticate(ctx context.Context, pluginID string, args map[string]string) (map[string]any, error) {
	p, ok := r.plugins[pluginID]
	if !ok {
		return nil, coreerrors.New(coreerrors.CodeNotFound, pluginID,
			fmt.Sprintf("unknown plugin %q", pluginID))
	}
	params := p.AuthParams()
	merged := r.mergeCreds(pluginID, params, args)
	for _, param := range params {
		if param.Required && merged[param.Name] == "" {
			return nil, coreerrors.New(coreerrors.CodeBadRequest, pluginID,
				fmt.Sprintf("missing credential %q for %q (auth %s ... aufrufen)", param.Name, pluginID, pluginID))
		}
	}
	if err := p.Authenticate(ctx, merged); err != nil {
		return nil, err
	}
	stored := []string{}
	if len(params) > 0 {
		toStore := make(map[string]string, len(params))
		for _, param := range params {
			if merged[param.Name] != "" {
				toStore[param.Name] = merged[param.Name]
			}
		}
		if err := r.sessions.Set(pluginID, toStore); err != nil {
			return nil, coreerrors.Wrap(coreerrors.CodeInternal, pluginID, "anmeldedaten konnten nicht gespeichert werden", err)
		}
		for k := range toStore {
			stored = append(stored, k)
		}
		sort.Strings(stored)
	}
	return map[string]any{
		"plugin":        pluginID,
		"authenticated": true,
		"stored":        stored,
		"store":         r.sessions.Path(),
	}, nil
}

// Logout verwirft die gespeicherten Credentials eines Plugins.
func (r *Runtime) Logout(_ context.Context, pluginID string) (map[string]any, error) {
	if _, ok := r.plugins[pluginID]; !ok {
		return nil, coreerrors.New(coreerrors.CodeNotFound, pluginID,
			fmt.Sprintf("unknown plugin %q", pluginID))
	}
	if err := r.sessions.Delete(pluginID); err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, pluginID, "anmeldedaten konnten nicht verworfen werden", err)
	}
	return map[string]any{"plugin": pluginID, "logged_out": true}, nil
}

// mergeCreds führt zusammen: Param-Defaults < gespeicherte Credentials
// < Env-Credentials (optionales Plugin-Interface) < Args (gewinnen).
// Param-Aliase (z.B. email → username) werden auf den kanonischen Namen
// normalisiert. Das Ergebnis ist eine Kopie — Handler dürfen es bedenkenlos
// lesen, die Runtime loggt es nie.
func (r *Runtime) mergeCreds(pluginID string, params []domain.Param, args map[string]string) map[string]string {
	merged := map[string]string{}
	for _, param := range params {
		if param.Default != "" {
			merged[param.Name] = param.Default
		}
	}
	if stored, ok := r.sessions.Get(pluginID); ok {
		for k, v := range stored {
			if v != "" {
				merged[k] = v
			}
		}
	}
	if p, ok := r.plugins[pluginID]; ok {
		if ep, ok := p.(domain.EnvCredentialsProvider); ok {
			for k, v := range ep.EnvCredentials() {
				if strings.TrimSpace(v) != "" && strings.TrimSpace(merged[k]) == "" {
					merged[k] = v
				}
			}
		}
	}
	for k, v := range args {
		if v != "" {
			merged[canonicalName(params, k)] = v
		}
	}
	return merged
}

// canonicalName löst einen Param-Alias auf den kanonischen Namen auf
// (unbekannte Keys bleiben unverändert).
func canonicalName(params []domain.Param, key string) string {
	for _, p := range params {
		if key == p.Name {
			return key
		}
		for _, a := range p.Aliases {
			if key == a {
				return p.Name
			}
		}
	}
	return key
}

// CallMCP ruft eine Funktion via MCP-Tool-Name ("<plugin>_<funktion>") auf.
// Damit ist die "-" -> "_"-Mehrdeutigkeit exakt aufgelöst (kein Raten im Adapter).
func (r *Runtime) CallMCP(ctx context.Context, mcpName string, args map[string]string) (domain.Result, error) {
	for _, fn := range r.functions {
		if fn.MCPName() == mcpName {
			return r.Call(ctx, fn.PluginID, fn.Name, args)
		}
	}
	return domain.Result{}, coreerrors.New(coreerrors.CodeNotFound, "",
		fmt.Sprintf("unknown MCP tool %q", mcpName))
}

// Call ruft eine Plugin-Funktion via "<plugin>.<funktion>" auf.
// Bei Plugins mit AuthParams() werden gespeicherte Credentials (plus
// mitgegebene Args, Defaults) automatisch injiziert — die Handler sehen
// sie wie normale Args, ohne eigene Credential-Params zu deklarieren.
func (r *Runtime) Call(ctx context.Context, pluginID, funcName string, args map[string]string) (domain.Result, error) {
	key := pluginID + "." + funcName
	fn, ok := r.functions[key]
	if !ok {
		return domain.Result{}, coreerrors.New(coreerrors.CodeNotFound, pluginID,
			fmt.Sprintf("unknown function %q", key))
	}
	// Required-Params prüfen (einheitlich für CLI/REST/MCP).
	// Ausnahme "auth": Hier zählen auch gespeicherte Credentials, daher
	// prüft erst Authenticate() gegen die zusammengeführten Werte.
	isAuth := false
	if p, ok := r.plugins[pluginID]; ok && len(p.AuthParams()) > 0 && funcName == "auth" {
		isAuth = true
	}
	if !isAuth {
		for _, p := range fn.Params {
			if p.Required && args[p.Name] == "" {
				return domain.Result{}, coreerrors.New(coreerrors.CodeBadRequest, pluginID,
					fmt.Sprintf("missing required param %q for %q", p.Name, key))
			}
			if args[p.Name] == "" && p.Default != "" {
				if args == nil {
					args = map[string]string{}
				}
				args[p.Name] = p.Default
			}
		}
	}
	callArgs := args
	if p, ok := r.plugins[pluginID]; ok && len(p.AuthParams()) > 0 && funcName != "logout" {
		callArgs = r.mergeCreds(pluginID, p.AuthParams(), args)
		if funcName == "auth" {
			// Die generische Required-Prüfung oben lief gegen Roh-Args;
			// für auth zählen auch gespeicherte Credentials + Defaults.
			for _, param := range p.AuthParams() {
				if param.Required && callArgs[param.Name] == "" {
					return domain.Result{}, coreerrors.New(coreerrors.CodeBadRequest, pluginID,
						fmt.Sprintf("missing credential %q for %q", param.Name, key))
				}
			}
		} else {
			for _, param := range p.AuthParams() {
				if param.Required && callArgs[param.Name] == "" {
					return domain.Result{}, coreerrors.New(coreerrors.CodeUnauthorized, pluginID,
						fmt.Sprintf("keine Anmeldedaten für %q: erst %q aufrufen oder Credentials als Parameter mitgeben", pluginID, pluginID+".auth"))
				}
			}
		}
	}
	data, err := fn.Handler(ctx, callArgs)
	if err != nil {
		return domain.Result{}, err
	}
	return domain.Result{Plugin: pluginID, Function: funcName, Data: data}, nil
}
