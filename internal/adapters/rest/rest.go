// Package rest ist der REST-Adapter: jede Plugin-Funktion wird automatisch
// ein Endpunkt: GET/POST /api/<plugin>/<funktion>?param=... .
package rest

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"schoolconnect/internal/app"
	coreerrors "schoolconnect/internal/core/errors"
)

// Server serviert alle Plugin-Funktionen via net/http (stdlib only).
type Server struct {
	rt  *app.Runtime
	log *slog.Logger
}

// New erzeugt den Server.
func New(rt *app.Runtime, log *slog.Logger) *Server { return &Server{rt: rt, log: log} }

// Handler baut den mux: /api/<plugin>/<funktion> + /healthz + /api (Index).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api", s.handleIndex)
	mux.HandleFunc("/api/", s.handleCall)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Plugin      string `json:"plugin"`
		Function    string `json:"function"`
		Method      string `json:"method"`
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	items := []item{}
	for _, fn := range s.rt.Functions() {
		items = append(items, item{
			Plugin: fn.PluginID, Function: fn.Name,
			Method: "GET|POST", Path: "/api/" + fn.PluginID + "/" + fn.Name,
			Description: fn.Description,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// handleCall: /api/<plugin>/<funktion>, Params aus Query + (bei POST) JSON-Body.
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	if len(parts) != 2 {
		writeErr(w, coreerrors.New(coreerrors.CodeNotFound, "", "expected /api/<plugin>/<function>"))
		return
	}
	pluginID, funcName := parts[0], parts[1]
	args := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			args[k] = v[0]
		}
	}
	if r.Method == http.MethodPost && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			for k, v := range body {
				if _, ok := args[k]; !ok {
					args[k] = toString(v)
				}
			}
		}
	}
	res, err := s.rt.Call(r.Context(), pluginID, funcName, args)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if ce, ok := err.(*coreerrors.Error); ok {
		switch ce.Code {
		case coreerrors.CodeNotFound:
			status = http.StatusNotFound
		case coreerrors.CodeBadRequest:
			status = http.StatusBadRequest
		case coreerrors.CodeUnauthorized:
			status = http.StatusUnauthorized
		case coreerrors.CodeUpstream:
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"error": ce.Message, "code": ce.Code, "plugin": ce.Plugin})
		return
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
