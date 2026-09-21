// Package rest ist der REST-Adapter: jede Plugin-Funktion wird automatisch
// ein Endpunkt: GET/POST /api/<plugin>/<funktion>?param=... .
package rest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"schoolconnect/internal/app"
	"schoolconnect/internal/core/tenant"
	coreerrors "schoolconnect/internal/core/errors"
)

// Server serviert alle Plugin-Funktionen via net/http (stdlib only).
type Server struct {
	rt             *app.Runtime
	log            *slog.Logger
	requireTenant  bool
	tenantSecret   string
	allowSignature bool
}

// New erzeugt den Server (ohne Tenant-Pflicht — normale Einzelnutzung).
func New(rt *app.Runtime, log *slog.Logger) *Server { return &Server{rt: rt, log: log} }

// NewWithTenantPolicy erzeugt den Server mit Tenant-Regeln:
// requireTenant verlangt für Plugins mit Login einen Tenant-Header
// (öffentliche Plugins, /healthz und /api-Index bleiben immer frei);
// sharedSecret aktiviert zusätzlich die HMAC-Signaturprüfung
// (X-SC-Tenant-Sig = HMAC-SHA256(secret, tenant) als Hex).
func NewWithTenantPolicy(rt *app.Runtime, log *slog.Logger, requireTenant bool, sharedSecret string) *Server {
	return &Server{rt: rt, log: log, requireTenant: requireTenant, tenantSecret: sharedSecret, allowSignature: sharedSecret != ""}
}

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
// Der Tenant kommt generisch aus X-SC-Tenant in den Context — nie als
// Funktions-Param, nie per Plugin-Switch: Plugins ohne AuthParams()
// (öffentliche Inhalte) bleiben immer frei; mit SC_REQUIRE_TENANT=true
// verlangen Login-Plugins einen gültigen Tenant (sonst 401 tenant_required).
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	if len(parts) != 2 {
		writeErr(w, coreerrors.New(coreerrors.CodeNotFound, "", "expected /api/<plugin>/<function>"))
		return
	}
	pluginID, funcName := parts[0], parts[1]
	ctx := r.Context()
	if s.requireTenant && s.rt.NeedsAuth(pluginID) {
		t, err := s.resolveTenant(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		ctx = tenant.WithTenant(ctx, t)
	} else if raw := strings.TrimSpace(r.Header.Get(tenant.Header)); raw != "" {
		t, err := tenant.Normalize(raw)
		if err != nil {
			writeErr(w, coreerrors.New(coreerrors.CodeBadRequest, pluginID, err.Error()))
			return
		}
		if sigErr := s.checkSignature(t, r.Header.Get(tenant.SigHeader)); sigErr != nil {
			writeErr(w, sigErr)
			return
		}
		ctx = tenant.WithTenant(ctx, t)
	} else if s.allowSignature && strings.TrimSpace(r.Header.Get(tenant.SigHeader)) != "" {
		writeErr(w, coreerrors.New(coreerrors.CodeBadRequest, pluginID, "X-SC-Tenant-Sig ohne X-SC-Tenant"))
		return
	}
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
	res, err := s.rt.Call(ctx, pluginID, funcName, args)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// resolveTenant liest + validiert den Tenant-Header (Pflicht-Modus) inkl.
// optionaler HMAC-Signatur. Fehlt der Header → 401 tenant_required.
func (s *Server) resolveTenant(r *http.Request) (string, error) {
	raw := strings.TrimSpace(r.Header.Get(tenant.Header))
	if raw == "" {
		return "", coreerrors.New(coreerrors.CodeUnauthorized, "",
			"tenant_required (X-SC-Tenant header setzen)")
	}
	t, err := tenant.Normalize(raw)
	if err != nil {
		return "", coreerrors.New(coreerrors.CodeBadRequest, "", err.Error())
	}
	if err := s.checkSignature(t, r.Header.Get(tenant.SigHeader)); err != nil {
		return "", err
	}
	return t, nil
}

// checkSignature prüft X-SC-Tenant-Sig, wenn ein Shared Secret konfiguriert
// ist (HMAC-SHA256 über den normalisierten Tenant, Hex). Ohne Secret
// ist jede (auch fehlende) Signatur ok.
func (s *Server) checkSignature(t, sig string) error {
	if !s.allowSignature {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(s.tenantSecret))
	mac.Write([]byte(t))
	want := hex.EncodeToString(mac.Sum(nil))
	got, err := hex.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		return coreerrors.New(coreerrors.CodeUnauthorized, "", "invalid tenant signature")
	}
	wantRaw, _ := hex.DecodeString(want)
	if !hmac.Equal(got, wantRaw) {
		return coreerrors.New(coreerrors.CodeUnauthorized, "", "invalid tenant signature")
	}
	return nil
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
