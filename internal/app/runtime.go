// Package app ist die Application-Schicht: Plugin Runtime.
// CLI, REST und MCP greifen alle auf dieselbe Runtime zu.
package app

import (
	"context"
	"fmt"
	"log/slog"

	"schoolconnect/internal/domain"

	coreerrors "schoolconnect/internal/core/errors"
)

// Runtime registriert Plugins und dispatcht Funktionsaufrufe.
// Neue Plattformen = nur neues Plugin + Register() — ohne Änderung an CLI/REST/MCP.
type Runtime struct {
	log       *slog.Logger
	plugins   map[string]domain.Plugin
	functions map[string]domain.Function // key: "<plugin>.<funktion>"
}

// New erzeugt eine leere Runtime.
func New(log *slog.Logger) *Runtime {
	return &Runtime{
		log:       log,
		plugins:   map[string]domain.Plugin{},
		functions: map[string]domain.Function{},
	}
}

// Register meldet ein Plugin (und all seine Funktionen) an.
func (r *Runtime) Register(p domain.Plugin) {
	r.plugins[p.ID()] = p
	for _, fn := range p.Functions() {
		fn.PluginID = p.ID() // sicherstellen, dass PluginID gesetzt ist
		key := fn.FullName()
		r.functions[key] = fn
		r.log.Info("function registered", "function", key, "mcp", fn.MCPName())
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

// Functions listet alle registrierten Funktionen.
func (r *Runtime) Functions() []domain.Function {
	out := make([]domain.Function, 0, len(r.functions))
	for _, f := range r.functions {
		out = append(out, f)
	}
	return out
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
func (r *Runtime) Call(ctx context.Context, pluginID, funcName string, args map[string]string) (domain.Result, error) {
	key := pluginID + "." + funcName
	fn, ok := r.functions[key]
	if !ok {
		return domain.Result{}, coreerrors.New(coreerrors.CodeNotFound, pluginID,
			fmt.Sprintf("unknown function %q", key))
	}
	// Required-Params prüfen (einheitlich für CLI/REST/MCP).
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
	data, err := fn.Handler(ctx, args)
	if err != nil {
		return domain.Result{}, err
	}
	return domain.Result{Plugin: pluginID, Function: funcName, Data: data}, nil
}
