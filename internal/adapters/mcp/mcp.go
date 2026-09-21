// Package mcp ist der MCP-Adapter: jede Plugin-Funktion wird automatisch
// ein MCP-Tool mit Namen "<plugin>_<funktion>" (Bindestriche -> Unterstriche).
//
// Transport: JSON-RPC 2.0 über stdio (für Claude Desktop / MCP-Clients):
//
//	-> {"jsonrpc":"2.0","id":1,"method":"tools/list"}
//	<- {"jsonrpc":"2.0","id":1,"result":{"tools":[...]}}
//	-> {"jsonrpc":"2.0","id":2,"method":"tools/call",
//	    "params":{"name":"lernplan_search","arguments":{"query":"..."}}}
//	<- {"jsonrpc":"2.0","id":2,"result":{...}}
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"schoolconnect/internal/app"
	"schoolconnect/internal/core/tenant"
	"schoolconnect/internal/domain"
)

// Server liest JSON-RPC von stdin und schreibt Antworten auf stdout.
type Server struct {
	rt  *app.Runtime
	log *slog.Logger
}

// New erzeugt den Server.
func New(rt *app.Runtime, log *slog.Logger) *Server { return &Server{rt: rt, log: log} }

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// ServeSTDIO läuft bis EOF. Der Prozess ist an einen Tenant gebunden
// (Env SC_TENANT, Default "default"): Alle Tool-Calls teilen sich dessen
// gespeicherte Logins + Sessions — normale MCP-Nutzung bleibt unverändert,
// Tool-Schemas enthalten keinen Tenant-Parameter.
func (s *Server) ServeSTDIO(ctx context.Context) {
	ctx = tenant.WithTenant(ctx, tenant.FromEnvOrDefault())
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		line := sc.Bytes()
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			out.Encode(rsp(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "schoolconnect", "version": "0.1.0"},
			}))
		case "tools/list":
			tools := []toolDef{}
			for _, fn := range s.rt.Functions() {
				tools = append(tools, toolDef{
					Name: fn.MCPName(), Description: fn.Description,
					InputSchema: schemaOf(fn),
				})
			}
			out.Encode(rsp(req.ID, map[string]any{"tools": tools}))
		case "tools/call":
			var p struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			// Exakte Auflösung über die Runtime (kein String-Raten mit "-"/"_").
			res, err := s.rt.CallMCP(ctx, p.Name, p.Arguments)
			if err != nil {
				out.Encode(errRsp(req.ID, err.Error()))
				continue
			}
			out.Encode(rsp(req.ID, map[string]any{"content": []any{
				map[string]any{"type": "text", "text": mustJSON(res)},
			}}))
		case "notifications/initialized":
			continue
		default:
			out.Encode(errRsp(req.ID, fmt.Sprintf("unknown method %q", req.Method)))
		}
	}
}

func schemaOf(fn domain.Function) any {
	props := map[string]any{}
	required := []string{}
	for _, p := range fn.Params {
		props[p.Name] = map[string]any{"type": "string", "description": p.Description}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func rsp(id any, result any) any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func errRsp(id any, msg string) any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": msg}}
}
